//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// These tests exercise the Q264 P3d wiring: a RunnerSet with
// spec.acquisitionProtocol == ScaleSet is driven by a scale-set listener session per
// set (session + capacity-gated poll + provision-on-JobAssigned) against the real
// apiserver and the scalesettest fake, while a Classic set (the default) keeps using
// the classic multiplexer path untouched — it never registers a scale set.

// scaleSetTestMetrics is the shared scale-set metrics registry for the integration
// tests. It registers into a throwaway prometheus.Registry rather than the global
// controller-runtime one, so it neither collides with the AGC's real collectors nor
// with a second NewMetrics call (Q288). Counters are labelled per
// (namespace, runner_set), so the tests' distinct namespaces keep independent series.
var scaleSetTestMetrics = scalesetlistener.NewMetrics(prometheus.NewRegistry())

// startRunnerSetReconcilerWithScaleSet wires and starts a RunnerSetReconciler like
// startRunnerSetReconciler, but injects a ScaleSetClientFactory that points every
// ScaleSet-protocol set's client at the given scalesettest fake, so the scale-set
// acquisition tier is exercised offline. It wires the scale-set metrics so a test can
// assert the reconciler → listener recorder path increments end-to-end.
func startRunnerSetReconcilerWithScaleSet(t *testing.T, srv *scalesettest.Server) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	require.NoError(t, err)

	tm := token.NewManager(stubProvider{}, nil)
	go tm.Start(mgrCtx)
	_, _ = tm.Token(mgrCtx)

	p := provisioner.NewProvisioner(mgr.GetClient(), nil, slog.Default())
	p.PollInterval = 50 * time.Millisecond
	p.WorkerSA = agcnames.WorkerSAName
	p.DefaultWorkerImage = "runner:test"
	p.HTTPClient = brokerStub.HTTPClient()
	p.TokenFunc = stubProvider{}.Token

	r := &controller.RunnerSetReconciler{
		Client:          mgr.GetClient(),
		TokenManager:    tm,
		Registrar:       &brokerRegistrar{stub: brokerStub},
		AgentKeyType:    agentpool.KeyTypeEd25519,
		Provisioner:     p,
		ScaleSetMetrics: scaleSetTestMetrics,
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:        brokerStub.URL,
			RunnerVersion:    "2.335.1",
			RunnerOS:         "linux",
			UseV2Flow:        true,
			HTTPClient:       brokerStub.HTTPClient(),
			IdleThreshold:    500,
			RenewJobInterval: 50 * time.Millisecond,
		},
		// Point every ScaleSet set's protocol client at the fake.
		ScaleSetClientFactory: func(_ *v2alpha1.RunnerSet, _ *v2alpha1.ActionsGateway) (*scaleset.Client, error) {
			return scaleset.New(scaleset.Config{
				TokenProvider: stubProvider{},
				ConfigURL:     "https://github.com/acme",
				APIBase:       srv.URL,
				HTTPClient:    srv.HTTPClient(),
				PollClient:    srv.HTTPClient(),
			})
		},
	}
	require.NoError(t, r.SetupWithManager(mgr))

	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); _ = mgr.Start(mgrCtx) }()
	t.Cleanup(func() { mgrCancel(); <-mgrDone })
}

// newScaleSetRunnerSet builds a ScaleSet-protocol RunnerSet whose single runnerLabel
// (its scale set's name) is label, with maxWorkers as the advertised capacity ceiling.
func newScaleSetRunnerSet(name, ns, gateway, label string, maxWorkers int32) *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:          v2alpha1.ObjectRef{Name: gateway},
			TemplateRef:         &v2alpha1.ObjectRef{Name: "tmpl"},
			RunnerLabels:        []string{label},
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolScaleSet,
			MaxWorkers:          ptr.To(maxWorkers),
		},
	}
}

// TestV2_RunnerSet_ScaleSet_ProvisionsWorkerOnJobAssigned is the P3d deliverable's
// proof: a ScaleSet RunnerSet starts its listener (registering exactly one scale set),
// a JobAssigned from the fake provisions one worker pod with the JIT-config Secret
// staged, and deleting the RunnerSet stops the listener and deletes its session — no
// leaked session.
func TestV2_RunnerSet_ScaleSet_ProvisionsWorkerOnJobAssigned(t *testing.T) {
	const ns = "v2-rs-scaleset"
	const label = "linux"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Apply the full object set up front (apply order must not matter, §H.7).
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-set", ns, "gw", label, 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	// 1. The listener starts and registers exactly one scale set named after the
	//    single runnerLabel (§2.1: one scale-set object per group).
	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)

	// The set reports Ready=True/ListenerActive with one session (one per scale set).
	waitForSetReadyReason(t, ns, "ss-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	require.True(t, srv.HasActiveSession(ssID), "the listener must hold one active session")

	// 2. A JobAssigned from the fake provisions exactly one worker pod carrying the
	//    JIT-config Secret (the run.sh --jitconfig blob, no acquired payload — P3c).
	srv.EnqueueJob(ssID)

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-set"}); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	// Owner-referenced to the real RunnerSet (worker pods GC with the set).
	foundOwner := false
	for _, o := range pod.OwnerReferences {
		if o.Kind == "RunnerSet" && o.Name == "ss-set" && o.Controller != nil && *o.Controller {
			foundOwner = true
		}
	}
	assert.True(t, foundOwner, "worker pod must be owner-referenced to the RunnerSet")

	// The reconciler wired a per-RunnerSet Prometheus recorder into the listener: the
	// assigned job counts as assigned and its successful provision counts as
	// provisioned, under this set's (namespace, runner_set) labels (Q264 P4).
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(scaleSetTestMetrics.JobsProvisionedTotal.WithLabelValues(ns, "ss-set")) >= 1
	}, 10*time.Second, 50*time.Millisecond, "the scale-set tier must count the provisioned worker")
	assert.GreaterOrEqual(t, testutil.ToFloat64(scaleSetTestMetrics.JobsAssignedTotal.WithLabelValues(ns, "ss-set")), float64(1),
		"the assigned job must be counted")

	// The worker runs in scale-set mode (run.sh --jitconfig): WORKER_MODE=scaleset.
	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner, "worker pod must have a runner container")
	envByName := map[string]string{}
	for _, e := range runner.Env {
		envByName[e.Name] = e.Value
	}
	assert.Equal(t, "scaleset", envByName["WORKER_MODE"], "scale-set worker runs run.sh --jitconfig")

	// The JIT-config Secret is staged (the blob, no acquired payload — §2.4). The
	// secret name carries a hash suffix, so select it by the owner-identity label.
	var secret corev1.Secret
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-set"}); err != nil {
			return false
		}
		for i := range secrets.Items {
			if len(secrets.Items[i].Data["jitconfig"]) > 0 {
				secret = secrets.Items[i]
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the JIT-config Secret must be staged for the assigned job")
	assert.NotEmpty(t, secret.Data["jitconfig"], "the staged Secret carries the JIT config blob")
	assert.NotContains(t, secret.Data, "payload", "the scale-set worker Secret carries no acquired payload")

	// 3. Delete the RunnerSet: the listener stops and its session is deleted (no leak).
	require.NoError(t, k8sClient.Delete(ctx, rs))
	require.Eventually(t, func() bool {
		return !srv.HasActiveSession(ssID)
	}, 20*time.Second, 100*time.Millisecond, "deleting the RunnerSet must stop the listener and delete its session")

	// The RunnerSet object is fully gone (finalizer removed).
	require.Eventually(t, func() bool {
		var got v2alpha1.RunnerSet
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ss-set"}, &got)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 100*time.Millisecond, "the RunnerSet finalizer must be removed after teardown")
}

// TestV2_RunnerSet_ScaleSet_SessionFailureConditionsReachStatus proves the Q325
// wiring end-to-end against the real apiserver: a session failure the scale-set
// listener detects (an unauthorized queue-token refresh — revoked credentials)
// lands on the RunnerSet's .status.conditions as Degraded=True/Unauthorized via the
// reconciler's condition channel, and recovery clears it back to
// Degraded=False/SessionAuthorized. Listener-pushed conditions drain on the next
// reconcile, so the polling loops poke the set to trigger one.
func TestV2_RunnerSet_ScaleSet_SessionFailureConditionsReachStatus(t *testing.T) {
	const ns = "v2-rs-scaleset-conds"
	const label = "linux-conds"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("cond-set", ns, "gw", label, 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register the scale set")
	waitForSetReadyReason(t, ns, "cond-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	key := types.NamespacedName{Namespace: ns, Name: "cond-set"}
	// pokeSet touches an annotation so the reconciler runs and drains the condition
	// channel (conditions are recorded on the next reconcile).
	pokeSet := func() {
		var got v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations["test.poke"] = time.Now().Format(time.RFC3339Nano)
		_ = k8sClient.Update(ctx, &got)
	}
	setCondition := func(condType string) *metav1.Condition {
		var got v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return nil
		}
		return apimeta.FindStatusCondition(got.Status.Conditions, condType)
	}

	// 1. Healthy baseline: the started listener publishes Degraded=False/SessionAuthorized.
	require.Eventually(t, func() bool {
		pokeSet()
		c := setCondition(v2alpha1.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == v2alpha1.ReasonSessionAuthorized
	}, 20*time.Second, 200*time.Millisecond, "a healthy ScaleSet set must report Degraded=False/SessionAuthorized")

	// 2. Revoke the credentials: the cached queue token 401s the poll and the refresh
	//    that should recover it is itself rejected — Degraded=True/Unauthorized must
	//    reach status.
	srv.FailSessionRefresh(true)
	srv.ExpireQueueToken(ssID)
	require.Eventually(t, func() bool {
		pokeSet()
		c := setCondition(v2alpha1.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2alpha1.ReasonSessionUnauthorized
	}, 20*time.Second, 200*time.Millisecond, "an unauthorized session refresh must surface Degraded=True on status")

	// 3. Restore the credentials: the next refresh succeeds and the listener clears
	//    the condition.
	srv.FailSessionRefresh(false)
	require.Eventually(t, func() bool {
		pokeSet()
		c := setCondition(v2alpha1.ConditionDegraded)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == v2alpha1.ReasonSessionAuthorized
	}, 20*time.Second, 200*time.Millisecond, "recovery must clear Degraded back to False/SessionAuthorized")
}

// TestV2_RunnerSet_Classic_DoesNotRegisterScaleSet proves the default path is
// unaffected: a Classic RunnerSet (the default protocol) reaches Ready via the classic
// multiplexer and never registers a scale set on the fake — the scale-set client is
// built only for ScaleSet sets.
func TestV2_RunnerSet_Classic_DoesNotRegisterScaleSet(t *testing.T) {
	const ns = "v2-rs-classic-untouched"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("classic-set", ns, "gw") // default acquisitionProtocol = Classic
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	// The classic set comes up via the classic multiplexer (ListenerActive).
	waitForSetReadyReason(t, ns, "classic-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// It defaulted to Classic and never touched the scale-set backend.
	var got v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "classic-set"}, &got))
	assert.Equal(t, v2alpha1.AcquisitionProtocolClassic, got.Spec.AcquisitionProtocol, "a set with no protocol defaults to Classic")

	// Give the reconciler ample time; the fake must have served zero calls (no scale
	// set registered, no session opened) — the classic path is byte-for-byte unchanged.
	time.Sleep(500 * time.Millisecond)
	assert.Empty(t, srv.Calls(), "a Classic RunnerSet must never reach the scale-set backend")
	_, ok := srv.ScaleSetIDByName("self-hosted")
	assert.False(t, ok, "no scale set is registered for a Classic set")
}
