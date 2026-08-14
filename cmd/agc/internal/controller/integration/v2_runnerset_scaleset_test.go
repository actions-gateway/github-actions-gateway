//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
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
	"k8s.io/apimachinery/pkg/api/resource"
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
//
// It returns a stop function that shuts the manager down and blocks until it has
// drained — how a test models an AGC process going away mid-flight (Q435). Callers
// that only need one long-lived manager ignore it; the same shutdown is registered
// as a t.Cleanup either way, and calling stop twice is a no-op.
//
// Optional tweaks mutate the Provisioner before the manager starts (e.g. to point the
// GitHub REST base at a fake for the eviction-recovery rerun call).
func startRunnerSetReconcilerWithScaleSet(t *testing.T, srv *scalesettest.Server, tweaks ...func(*provisioner.Provisioner)) func() {
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
	// The uncached reader the orphaned-worker scan lists pods through (Q844), exactly as
	// main.go wires it: that scan acts on a pod's absence, so a cold cache would read as
	// a whole set's workers having been disrupted.
	p.APIReader = mgr.GetAPIReader()
	p.PollInterval = 50 * time.Millisecond
	p.WorkerSA = agcnames.WorkerSAName
	p.DefaultWorkerImage = "runner:test"
	p.HTTPClient = brokerStub.HTTPClient()
	p.TokenFunc = stubProvider{}.Token
	for _, tweak := range tweaks {
		tweak(p)
	}

	r := &controller.RunnerSetReconciler{
		Client:          mgr.GetClient(),
		TokenManager:    tm,
		Registrar:       &brokerRegistrar{stub: brokerStub},
		AgentKeyType:    agentpool.KeyTypeEd25519,
		Provisioner:     p,
		ScaleSetMetrics: scaleSetTestMetrics,
		// The uncached reader the capacity gate reads pod Events through on a cluster
		// that can grow (Q406, Q470), exactly as main.go wires it.
		EventReader: mgr.GetAPIReader(),
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
	var once sync.Once
	stop := func() { once.Do(func() { mgrCancel(); <-mgrDone }) }
	t.Cleanup(stop)
	return stop
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

// TestV2_RunnerSet_ScaleSet_EvictedWorkerTriggersRerun is the Q417 proof at the tier the
// unit tests cannot reach. The unit tests pin that the provisioner stamps identity when
// asked and re-runs when handed an evicted pod; only this one shows the reconciler
// actually connects the two on a pod that came out of the real provisioning path — which
// is exactly what was missing before, the same gap Q373 had (a wired Provision closure
// and no second half at all).
//
// envtest runs no kubelet, so the test plays that role and drives the worker pod to
// PodFailed/Evicted through the status subresource.
func TestV2_RunnerSet_ScaleSet_EvictedWorkerTriggersRerun(t *testing.T) {
	const ns = "v2-rs-ss-evict"
	const label = "linux-evict"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// Stand-in for the GitHub REST API, counting rerun-failed-jobs calls and recording
	// the run each one addressed.
	var rerunCalls atomic.Int64
	rerunPaths := make(chan string, 8)
	fakeGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			rerunCalls.Add(1)
			select {
			case rerunPaths <- r.URL.Path:
			default:
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(fakeGitHub.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-evict", ns, "gw", label, 3)
	// The CRD floors evictionRetryDelay at 1s, so take the floor: the delay's own
	// behaviour is handleEviction's and already covered by the unit tests. Keep
	// completedPodTTL long so the reaper cannot delete the evicted pod before recovery
	// reads it — the ordering this wiring depends on.
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.MaxEvictionRetries = ptr.To(int32(2))
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-evict", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	srv.EnqueueJob(ssID)

	// The worker pod comes out of the real provisioning path, so its identity annotations
	// and tier label are the ones production would write — not fixtures.
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-evict"}); err != nil {
			return false
		}
		for i := range pods.Items {
			if strings.HasPrefix(pods.Items[i].Name, "runner-") {
				pod = pods.Items[i]
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the assigned job")

	require.Equal(t, provisioner.AcquisitionProtocolScaleSet, pod.Labels[provisioner.LabelAcquisitionProtocol],
		"the provisioned worker must be marked as a scale-set worker, or recovery will not consider it")
	wantRepo := scalesettest.DefaultJobOwner + "/" + scalesettest.DefaultJobRepository
	require.Equal(t, wantRepo, pod.Annotations[provisioner.AnnotationRepository],
		"the assignment's owner/repository must have reached the pod")
	runID := pod.Annotations[provisioner.AnnotationRunID]
	require.NotEmpty(t, runID, "the assignment's workflowRunId must have reached the pod")

	// The kubelet evicts the worker mid-job under node pressure: nothing in the pod runs,
	// so the job would sit failed until a human re-ran it.
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	require.Eventually(t, func() bool { return rerunCalls.Load() >= 1 },
		20*time.Second, 50*time.Millisecond,
		"an evicted scale-set worker must trigger rerun-failed-jobs without human action")

	select {
	case path := <-rerunPaths:
		assert.Equal(t, "/repos/"+wantRepo+"/actions/runs/"+runID+"/rerun-failed-jobs", path,
			"the re-run must address the run the evicted pod recorded")
	default:
		t.Fatal("no rerun path recorded")
	}

	// The pod is claimed, so the reconcile loop — which keeps seeing this terminal pod for
	// the whole completedPodTTL — cannot re-run the same eviction again.
	require.Eventually(t, func() bool {
		var got corev1.Pod
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, &got); err != nil {
			return false
		}
		_, ok := got.Annotations[provisioner.AnnotationEvictionHandledAt]
		return ok
	}, 20*time.Second, 50*time.Millisecond, "the evicted pod must be stamped as handled")

	// Several more reconciles' worth of time: still exactly one re-run.
	time.Sleep(2 * time.Second)
	assert.Equal(t, int64(1), rerunCalls.Load(),
		"repeated reconciles of one evicted pod must not spend more of the run's retry budget")
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

// TestV2_RunnerSet_ScaleSet_ReclaimsJobSecretOnCompletion is the Q373 proof at the tier
// the unit tests cannot reach: it exercises the whole reconciler → listener → provisioner
// wiring against a real apiserver. The unit tests pin that the provisioner reclaims when
// asked and that the listener asks on a terminal completion; only this one shows the
// reconciler actually connects the two, which is exactly what was missing — the Provision
// closure was wired and no cleanup closure existed at all, so every ScaleSet job leaked
// its credential-bearing JIT-config Secret until the RunnerSet was deleted.
func TestV2_RunnerSet_ScaleSet_ReclaimsJobSecretOnCompletion(t *testing.T) {
	const ns = "v2-rs-ss-reclaim"
	const label = "linux-reclaim"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-reclaim", ns, "gw", label, 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-reclaim", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	_, jobID := srv.EnqueueJob(ssID)

	// The JIT-config Secret is staged and — critically — SURVIVES provisioning: the
	// worker pod mounts it, so an eager delete here would strand the pod.
	var secretName string
	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-reclaim"}); err != nil {
			return false
		}
		for i := range secrets.Items {
			if len(secrets.Items[i].Data["jitconfig"]) > 0 {
				secretName = secrets.Items[i].Name
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "the JIT-config Secret must be staged for the assigned job")

	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-reclaim"}); err != nil {
			return false
		}
		return len(pods.Items) > 0
	}, 20*time.Second, 50*time.Millisecond, "the worker pod that mounts the Secret must exist")

	var staged corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &staged),
		"the Secret must still exist once the worker pod is created")

	// The worker finishes its job: the queue delivers the terminal JobCompleted, and the
	// listener's reclaim hook must delete the Secret — while the RunnerSet still exists,
	// so a pass here cannot be cascade-GC in disguise.
	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "succeeded"),
		"the job must be assigned server-side before it can complete")

	require.Eventually(t, func() bool {
		var got corev1.Secret
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &got)
		return apierrors.IsNotFound(err)
	}, 20*time.Second, 50*time.Millisecond,
		"a completed job's JIT-config Secret must be reclaimed, not left for the RunnerSet's cascade-GC")

	var stillThere v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ss-reclaim"}, &stillThere),
		"the RunnerSet must still exist — the reclaim is steady-state, not teardown")
}

// TestV2_RunnerSet_ScaleSet_QuotaHeadroomBoundsAdvertisedCapacity is the Q443 proof at
// the tier that matters: the pre-claim quota rung was wired into Provisioner.Admit,
// which reconcileScaleSetListener never reaches, so a quota-blocked job on the default
// acquisition tier was assigned anyway and left to createPodWithQuotaRetry — holding
// the GitHub job lock across the retry budget and abandoning the pod on exhaustion.
//
// The assertion is deliberately server-side (AssignedJobCount on the fake, which
// assigns strictly under the advertised X-ScaleSetMaxCapacity). "No worker pod was
// created" would also pass if the AGC had claimed the job and merely failed to place
// it, which is exactly the failure being fixed; "GitHub never assigned it" can only
// pass if the capacity header carried the quota bound.
func TestV2_RunnerSet_ScaleSet_QuotaHeadroomBoundsAdvertisedCapacity(t *testing.T) {
	const ns = "v2-rs-scaleset-quota"
	const label = "linux-quota"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newScaleSetRunnerSet("ss-quota", ns, "gw", label, 4)
	require.NoError(t, k8sClient.Create(ctx, rs))

	// 100m of headroom cannot admit a single 500m worker, so the set must advertise
	// zero capacity however high its maxWorkers is.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)
	waitForSetReadyReason(t, ns, "ss-quota", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// The advertised capacity reaches zero, and the withheld gauge names the rung that
	// took it — this tier's answer to "why did assignments stop", since the classic
	// per-job admission counter is structurally unreachable here.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, "ss-quota")) == 0 &&
			testutil.ToFloat64(scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, "ss-quota", runnercore.AdmitReasonQuota)) == 4
	}, 20*time.Second, 100*time.Millisecond,
		"an exhausted namespace ResourceQuota must withhold the whole declared ceiling from X-ScaleSetMaxCapacity")

	// A queued job stays queued at GitHub: never assigned, so no JIT runner record is
	// spent and no job lock is taken out on a pod that cannot be created.
	srv.EnqueueJob(ssID)
	require.Never(t, func() bool {
		return srv.AssignedJobCount(ssID) > 0
	}, 3*time.Second, 100*time.Millisecond,
		"a quota-blocked job must be left queued at GitHub, not assigned and then stalled in createPodWithQuotaRetry")

	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{provisioner.LabelRunnerSet: "ss-quota"}))
	require.Empty(t, pods.Items, "no worker pod should exist for a job that was never assigned")

	// Headroom returns (an admin raises the quota): the gate is self-clearing, and the
	// still-queued job is assigned on a later poll with nothing having been wasted.
	var live corev1.ResourceQuota
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tight"}, &live))
	live.Spec.Hard = corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("4")}
	require.NoError(t, k8sClient.Update(ctx, &live))

	require.Eventually(t, func() bool {
		return srv.AssignedJobCount(ssID) > 0
	}, 30*time.Second, 100*time.Millisecond,
		"restored quota headroom must reopen assignment without an AGC restart")

	require.Eventually(t, func() bool {
		var got corev1.PodList
		if err := k8sClient.List(ctx, &got, client.InNamespace(ns), client.MatchingLabels{provisioner.LabelRunnerSet: "ss-quota"}); err != nil {
			return false
		}
		return len(got.Items) > 0
	}, 20*time.Second, 50*time.Millisecond, "the job that waited for headroom must provision its worker once assigned")
}

// TestV2_RunnerSet_ScaleSet_ReapDeregistersRunnerRecord is the Q550 fix proven through
// the real reconciler wiring, which is where the defect actually lived: the listener
// minted a runner name and the Provision closure dropped it, so the name never reached
// the pod and no reap path could deregister anything. Only a tier that runs the real
// reconciler against a real apiserver can observe that, which is why the unit tests
// (which construct the listener handle by hand) are not sufficient here.
//
// The full chain: assignment -> minted name -> pod annotation -> reap -> record gone.
func TestV2_RunnerSet_ScaleSet_ReapDeregistersRunnerRecord(t *testing.T) {
	const ns = "v2-rs-scaleset-reap-dereg"
	const label = "linux-reap-dereg"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-set", ns, "gw", label, 3)
	// A short completedPodTTL so the reaper collects the worker within the test.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Second}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "ss-set", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")

	srv.EnqueueJob(ssID)

	// The pod carries the name its runner was pre-registered under, and that record
	// exists at GitHub.
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "ss-set"}); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if p.Annotations[provisioner.AnnotationRunnerName] != "" {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond,
		"the provisioned worker must carry its runner name; without it nothing can deregister the record")

	runnerName := pod.Annotations[provisioner.AnnotationRunnerName]
	assert.Contains(t, srv.RegisteredRunners(), runnerName,
		"minting the JIT config registers the runner, which is the record that leaks")

	// Drive the worker terminal so the reaper collects it past completedPodTTL. envtest
	// runs no kubelet, so the phase is staged.
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))

	require.Eventually(t, func() bool {
		var got corev1.Pod
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod.Name}, &got)
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 100*time.Millisecond, "the terminal worker must be reaped past completedPodTTL")

	require.Eventually(t, func() bool {
		for _, n := range srv.RegisteredRunners() {
			if n == runnerName {
				return false
			}
		}
		return true
	}, 20*time.Second, 100*time.Millisecond,
		"reaping the worker must deregister its runner record, or the name stays taken and the job's own retries 409 against it")
}
