//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	agcv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	testScheme *runtime.Scheme
	ctx        context.Context
	cancel     context.CancelFunc
	brokerStub *brokertest.Server
)

func TestMain(m *testing.M) {
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	brokerStub = brokertest.New()
	defer brokerStub.Close()

	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = v1alpha1.AddToScheme(testScheme)
	_ = agcv2alpha1.AddToScheme(testScheme)
	// Register the v2beta1 hub (Q74) so envtest recognizes the five v2 kinds as
	// convertible and redirects their CRD conversion to the local /convert server
	// this suite starts below. The v2 CRDs are now multi-version with v2beta1 as
	// storage, so a v2alpha1 create is only lossless if conversion is wired.
	_ = agcv2beta1.AddToScheme(testScheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd",
			// The five v2 (actions-gateway.com) CRDs live in the neutral api module,
			// reached through testdata/ so the read stays inside this module root and
			// go keeps it as a test-cache input (Q902).
			"testdata/crd",
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic(err)
	}

	// Serve /convert for the v2 hub kinds so v2alpha1<->v2beta1 conversion works
	// against the real apiserver (Q74). Must run before any v2 create in the suite.
	if err := startConversionWebhook(); err != nil {
		panic(err)
	}

	exitCode := m.Run()
	if conversionWebhookCancel != nil {
		conversionWebhookCancel()
	}
	_ = testEnv.Stop()
	cancel()
	os.Exit(exitCode)
}

// stubProvider always returns a hardcoded installation token.
type stubProvider struct{}

func (stubProvider) Token(_ context.Context) (string, error) { return "inst-token", nil }
func (stubProvider) TokenWithExpiry(_ context.Context) (*githubapp.InstallationToken, error) {
	return &githubapp.InstallationToken{
		Token:     "inst-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// runIdentityContext returns the `contextData` block of an AcquireJob response
// carrying a complete run identity, in the shape a real response uses: the
// serialised `github` context, as a type-tagged key/value list.
//
// Specs that need eviction recovery to fire must build the identity this way and
// not as system.github.* variables. Those variables do not exist in a real
// acquirejob body — building them here is what let the classic tier ship unable to
// name the run it had to re-run, with every test passing (Q495).
// testdata/job_payload.json is the capture this mirrors.
func runIdentityContext(repository, runID string) map[string]interface{} {
	return map[string]interface{}{
		"github": map[string]interface{}{
			"t": 2,
			"d": []map[string]interface{}{
				{"k": "repository", "v": repository},
				{"k": "run_id", "v": runID},
			},
		},
	}
}

// sharedListenerMetrics returns the process-wide runnercore.Metrics, constructed
// once. runnercore.NewMetrics registers with the controller-runtime metrics
// registry, so it must be created a single time per test binary; counters
// accumulate across tests, so assertions read before/after deltas.
var sharedListenerMetrics = sync.OnceValue(runnercore.NewMetrics)

// brokerRegistrar returns credentials pointing to the stub server.
type brokerRegistrar struct {
	stub   *brokertest.Server
	nextID atomic.Int64
}

func (r *brokerRegistrar) Register(_ context.Context, _ string, _ agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	id := r.nextID.Add(1)
	return &agentpool.AgentCredentials{
		AgentID:          id,
		ClientID:         fmt.Sprintf("client-%d", id),
		AuthorizationURL: r.stub.URL + "token",
		BrokerURL:        r.stub.URL,
	}, nil
}

func (r *brokerRegistrar) Deregister(_ context.Context, _ string, _ int64) error { return nil }

func (r *brokerRegistrar) ResolveAgentID(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

// provisionerOptions configures the optional Provisioner attached to the reconciler.
type provisionerOptions struct {
	enabled            bool
	maxEvictionRetries int
	githubAPIURL       string
	pollInterval       time.Duration
	// evictionRerunRetryInterval overrides the pacing between re-run attempts that
	// GitHub refuses because the original run has not concluded (Q503). Zero leaves
	// the production default (30s), far past these suites' Eventually timeouts.
	evictionRerunRetryInterval time.Duration
	// registrar overrides the default brokerRegistrar. Nil uses the default.
	registrar agentpool.Registrar
	// baselineRecheckInterval overrides the reconciler's baseline re-check cadence
	// (Q137). Zero leaves the production default.
	baselineRecheckInterval time.Duration
	// metrics, when non-nil, is attached to the reconciler so the listener
	// goroutines record into it (e.g. the Q260 duplicate-delivery counter). Nil
	// leaves metrics unwired, as most suites do not assert on them.
	metrics *runnercore.Metrics
	// fanoutCompletion enables the guarded Q260 Option A so the winner of a
	// fanned-out job fans completejob out to every deduped sibling delivery on
	// completion. Off in every suite but the one that asserts on it.
	fanoutCompletion bool
	// informerWaiter wires the production InformerPodWaiter onto the Provisioner
	// instead of leaving Waiter nil (which falls back to the poll loop). Most
	// suites drive completion by writing a terminal phase, where the two agree.
	// They do NOT agree on a pod that is *deleted* without ever reaching one —
	// the drain path — so the Q421 suite opts in to measure the production
	// mechanism rather than the fallback's approximation of it.
	informerWaiter bool
}

// startAGCReconciler starts a RunnerGroupReconciler for the duration of a test.
// Returns a cancel func that tests can call to simulate SIGTERM before cleanup.
func startAGCReconciler(t *testing.T) (context.CancelFunc, <-chan struct{}) {
	_, cancel, done := startAGCReconcilerOpts(t, provisionerOptions{})
	return cancel, done
}

// startAGCReconcilerWithProvisioner starts the reconciler with a real Provisioner attached.
func startAGCReconcilerWithProvisioner(t *testing.T, opts provisionerOptions) (context.CancelFunc, <-chan struct{}) {
	opts.enabled = true
	_, cancel, done := startAGCReconcilerOpts(t, opts)
	return cancel, done
}

// startAGCReconcilerOpts wires and starts a RunnerGroupReconciler against the
// shared envtest API server, returning the reconciler (so tests can drive its
// test hooks), a cancel func, and a done channel.
func startAGCReconcilerOpts(t *testing.T, opts provisionerOptions) (*controller.RunnerGroupReconciler, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		// Mirror the production AGC manager's Secret cache-isolation
		// (cmd/agc/main.go, W4 / H-2 / decision D-3): r.List and r.Get on
		// Secrets bypass the controller-runtime cache and go straight to the
		// API server, so Secret.Data is never resident in the in-process
		// informer cache. Without this, envtest is both less safe than
		// production for the duration of the test run and exposed to
		// cache-lag races (e.g. agentpool.EnsureAgents seeing a partial
		// post-create Secret list) that production code cannot hit.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	require.NoError(t, err)

	tm := token.NewManager(stubProvider{}, nil)
	go tm.Start(mgrCtx)
	_, _ = tm.Token(mgrCtx)

	var reg agentpool.Registrar
	if opts.registrar != nil {
		reg = opts.registrar
	} else {
		reg = &brokerRegistrar{stub: brokerStub}
	}

	r := &controller.RunnerGroupReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    reg,
		// Use Ed25519 agent keys: these suites exercise the session/reconcile
		// lifecycle, not crypto, and Ed25519 generation is near-instant. The
		// production default is RSA-3072 (Q109), whose per-agent generation cost
		// is high enough to blow these tests' Eventually timeouts on CI.
		AgentKeyType: agentpool.KeyTypeEd25519,
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:     brokerStub.URL,
			RunnerVersion: "2.335.1",
			RunnerOS:      "linux",
			UseV2Flow:     true,
			HTTPClient:    brokerStub.HTTPClient(),
			// Idle threshold for burst goroutines, expressed against the empty-poll
			// floor (broker.MinPollInterval, 100ms — Q788): 20 polls is ~2s of life,
			// long enough for session detection polling at 1ms to catch a new session
			// and short enough for the drain assertions to see it go. The old 500 was
			// calibrated against an unpaced loop, where it bought ~50ms.
			IdleThreshold: 20,
			// Short renew interval so integration tests can verify RenewJob is called.
			RenewJobInterval: 50 * time.Millisecond,
			// Guarded Q260 Option A; off unless a test opts in.
			FanoutCompletion: opts.fanoutCompletion,
		},
		// Q137 baseline re-check cadence; zero leaves the production default.
		BaselineRecheckInterval: opts.baselineRecheckInterval,
		// Q260: wire the listener metrics when a test wants to assert on them.
		Metrics: opts.metrics,
	}

	if opts.enabled {
		pollInterval := opts.pollInterval
		if pollInterval == 0 {
			pollInterval = 50 * time.Millisecond
		}
		maxRetries := opts.maxEvictionRetries
		p := &provisioner.Provisioner{
			Client:                     mgr.GetClient(),
			Log:                        slog.Default(),
			PollInterval:               pollInterval,
			WorkerSA:                   agcnames.WorkerSAName,
			MaxEvictionRetries:         maxRetries,
			EvictionRerunRetryInterval: opts.evictionRerunRetryInterval,
			DefaultWorkerImage:         "runner:test",
			GitHubAPIURL:               opts.githubAPIURL,
			HTTPClient:                 brokerStub.HTTPClient(),
			TokenFunc:                  stubProvider{}.Token,
		}
		if opts.informerWaiter {
			w := provisioner.NewInformerPodWaiter(mgr.GetCache(), nil)
			require.NoError(t, mgr.Add(w))
			p.Waiter = w
		}
		r.Provisioner = p
	}

	err = r.SetupWithManager(mgr)
	require.NoError(t, err)

	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		_ = mgr.Start(mgrCtx)
	}()

	t.Cleanup(func() {
		mgrCancel()
		<-mgrDone
	})

	return r, mgrCancel, mgrDone
}
