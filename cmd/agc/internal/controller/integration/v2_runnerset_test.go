//go:build integration

package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// These tests exercise the v2 RunnerSet reconciler (Q164, M3a) against the real
// apiserver: runtime reference resolution (gatewayRef/templateRef/proxyRef) with
// fail-closed NotFound conditions, and end-to-end worker provisioning once every
// reference resolves — the worker pod owner-referenced to the real RunnerSet and
// its egress wired through the resolved EgressProxy (the provisioner Target seam).

// startRunnerSetReconciler wires and starts a RunnerSetReconciler against the
// shared envtest apiserver with a real Provisioner attached. It returns the
// reconciler so tests can drive its test-only hooks (e.g. SetConditionForTest).
func startRunnerSetReconciler(t *testing.T) *controller.RunnerSetReconciler {
	t.Helper()
	return startRunnerSetReconcilerWithRegistrar(t, nil)
}

// startRunnerSetReconcilerWithRegistrar is startRunnerSetReconciler with an optional
// Registrar override (nil selects the default brokerRegistrar). A failing registrar
// drives agentpool.EnsureAgents to error so the reconciler's Ready=False provisioning
// path can be asserted against the real apiserver (Q308).
func startRunnerSetReconcilerWithRegistrar(t *testing.T, registrar agentpool.Registrar) *controller.RunnerSetReconciler {
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

	var reg agentpool.Registrar = &brokerRegistrar{stub: brokerStub}
	if registrar != nil {
		reg = registrar
	}
	r := &controller.RunnerSetReconciler{
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    reg,
		AgentKeyType: agentpool.KeyTypeEd25519,
		Provisioner:  p,
		// The uncached reader the capacity gate reads pod Events through on a cluster
		// that can grow (Q406, Q470), exactly as main.go wires it.
		EventReader: mgr.GetAPIReader(),
		// The same process-wide counters the v1 suite asserts on, so a v2 classic test
		// can read jobs_admission_rejected_total as a delta against a baseline.
		Metrics: sharedListenerMetrics(),
		// A mutable stub sizing source (Q359 Phase 2): empty by default (no
		// sizing status is written), populated per-test via sizingStub.Set —
		// concurrency-safe, so tests may seed it while the manager runs.
		Sizing: &sizingStub{},
		BrokerConfig: controller.BrokerConfig{
			BrokerURL:        brokerStub.URL,
			RunnerVersion:    "2.335.1",
			RunnerOS:         "linux",
			UseV2Flow:        true,
			HTTPClient:       brokerStub.HTTPClient(),
			IdleThreshold:    500,
			RenewJobInterval: 50 * time.Millisecond,
		},
	}
	require.NoError(t, r.SetupWithManager(mgr))

	mgrDone := make(chan struct{})
	go func() { defer close(mgrDone); _ = mgr.Start(mgrCtx) }()
	t.Cleanup(func() { mgrCancel(); <-mgrDone })
	return r
}

func newRunnerSet(name, ns, gateway string) *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:  v2alpha1.ObjectRef{Name: gateway},
			TemplateRef: &v2alpha1.ObjectRef{Name: "tmpl"},
			// Pin Classic: the default is ScaleSet (Q264 P5), but these reconciler tests
			// exercise the classic acquisition path. The ScaleSet path has its own suite
			// (v2_runnerset_scaleset_test.go).
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolClassic,
			MaxListeners:        1,
			RunnerLabels:        []string{"self-hosted"},
		},
	}
}

func newGatewayForSet(name, ns, proxyRef string) *v2alpha1.ActionsGateway {
	ag := &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.ActionsGatewaySpec{
			Credentials: v2alpha1.GitHubCredentials{
				Type:      v2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &v2alpha1.LocalSecretReference{Name: "github-app"},
			},
			GitHubURL: "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: proxyRef}
	}
	return ag
}

// newFixedSizeGatewayForSet is newGatewayForSet plus the platform operator's assertion
// that nothing in this cluster will add a node (Q470). That fact — not anything on the
// RunnerSet — is what makes the scheduler's Unschedulable verdict a sound intake signal,
// so a capacity-gate test that expects scheduler-verdict gating must use this gateway.
func newFixedSizeGatewayForSet(name, ns string) *v2alpha1.ActionsGateway {
	ag := newGatewayForSet(name, ns, "")
	ag.Spec.ClusterCapacity = &v2alpha1.ClusterCapacity{
		NodeAutoscaling: v2alpha1.NodeAutoscalingAbsent,
	}
	return ag
}

func newRunnerTemplate(name, ns string) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "runner:test",
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
				},
			},
		},
	}
}

func readySetCondition(t *testing.T, ns, name string) *metav1.Condition {
	t.Helper()
	var rs v2alpha1.RunnerSet
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
		return nil
	}
	return meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionReady)
}

func waitForSetReadyReason(t *testing.T, ns, name string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		c := readySetCondition(t, ns, name)
		return c != nil && c.Status == wantStatus && c.Reason == wantReason
	}, 20*time.Second, 100*time.Millisecond, "RunnerSet %s should report Ready=%s/%s", name, wantStatus, wantReason)
}

func TestV2_RunnerSet_FailsClosedUntilRefsResolve(t *testing.T) {
	const ns = "v2-rs-resolve"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	// 1. RunnerSet alone: no gateway, no template, no proxy → GatewayNotFound.
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonGatewayNotFound)

	// 2. Add the gateway (defaultProxyRef → "shared") → TemplateNotFound.
	gw := newGatewayForSet("gw", ns, "shared")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonTemplateNotFound)

	// 3. Add the template → ProxyNotFound (proxy required, §H.10).
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonProxyNotFound)

	// 4. Add the EgressProxy → all references resolve; a listener comes up and the
	//    set flips Ready=True/ListenerActive. This proves the watch-driven
	//    re-reconcile (§H.7): each NotFound condition cleared the moment its
	//    referent synced, with no re-apply of the RunnerSet.
	ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// TestV2_RunnerSet_TemplateDeleted_DegradesNotBlocks: deleting a previously-resolved
// RunnerTemplate out from under a Ready RunnerSet degrades it to
// Ready=False/TemplateDeleted — not the generic TemplateNotFound — and restoring the
// template flips it back to Ready with no re-apply of the set (§H.8
// degrade-not-block, Q309).
func TestV2_RunnerSet_TemplateDeleted_DegradesNotBlocks(t *testing.T) {
	const ns = "v2-rs-tmpl-deleted"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	// Direct egress (no proxy) keeps the referent under test to the template alone.
	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Delete the resolved template: the referent watch re-reconciles the set, which
	// degrades with the deletion-specific reason (its status.templateSource is the
	// evidence of the prior resolution).
	require.NoError(t, k8sClient.Delete(ctx, tmpl))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonTemplateDeleted)

	// Restore the template: the set self-heals to Ready the moment it syncs.
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// TestV2_RunnerSet_ProxyDeleted_DegradesNotBlocks: deleting a previously-resolved
// EgressProxy degrades the set to Ready=False/ProxyDeleted (its status.proxyMode
// Proxied is the evidence of the prior resolution), and restoring the proxy heals it
// (§H.8, Q309).
func TestV2_RunnerSet_ProxyDeleted_DegradesNotBlocks(t *testing.T) {
	const ns = "v2-rs-proxy-deleted"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	gw := newGatewayForSet("gw", ns, "shared")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })
	tmpl := newRunnerTemplate("tmpl", ns)
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tmpl) })
	ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, ep))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	require.NoError(t, k8sClient.Delete(ctx, ep))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonProxyDeleted)

	require.NoError(t, k8sClient.Create(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}))
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
}

// TestV2_RunnerSet_DirectEgress_ReadyAndUnattributed: a RunnerSet under a gateway
// with no defaultProxyRef and with no proxyRef of its own resolves to direct egress
// (Q168, §H.10) — it reaches Ready/ListenerActive (not ProxyNotFound) and reports
// proxyMode Direct + the advisory EgressUnattributed condition.
func TestV2_RunnerSet_DirectEgress_ReadyAndUnattributed(t *testing.T) {
	const ns = "v2-rs-direct"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	// Gateway with NO defaultProxyRef, a template, and a RunnerSet with no proxyRef.
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	// No proxy needed: the set goes Ready/ListenerActive directly.
	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// proxyMode Direct + EgressUnattributed=True/DirectEgress.
	require.Eventually(t, func() bool {
		var got v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "set"}, &got); err != nil {
			return false
		}
		unattr := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionEgressUnattributed)
		return got.Status.ProxyMode == v2alpha1.ProxyModeDirect &&
			unattr != nil && unattr.Status == metav1.ConditionTrue && unattr.Reason == v2alpha1.ReasonDirectEgress
	}, 20*time.Second, 100*time.Millisecond, "direct RunnerSet should report proxyMode Direct + EgressUnattributed=True")
}

// TestV2_RunnerSet_DirectEgress_WorkerHasNoProxy: a worker pod provisioned for a
// direct-egress RunnerSet carries no HTTP(S)_PROXY env and no proxy-CA mount, so it
// reaches GitHub directly (Q168). The worker's egress restriction is enforced by the
// GMC's direct-egress workload NetworkPolicy (verified in the GMC suite + kind e2e).
func TestV2_RunnerSet_DirectEgress_WorkerHasNoProxy(t *testing.T) {
	const ns = "v2-rs-direct-worker"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, ""))) // no defaultProxyRef
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("direct-worker-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	id := enqueueJobOnSetSession(15*time.Second, "direct-worker-set", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for direct-worker-set should register")

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "direct-worker-set"}); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "worker Pod should be created for the direct RunnerSet")

	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner)
	envByName := map[string]string{}
	for _, e := range runner.Env {
		envByName[e.Name] = e.Value
	}
	assert.Empty(t, envByName["HTTP_PROXY"], "direct egress: worker has no HTTP_PROXY")
	assert.Empty(t, envByName["HTTPS_PROXY"], "direct egress: worker has no HTTPS_PROXY")
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			assert.NotContains(t, v.Secret.SecretName, "-proxy-tls", "direct egress: worker mounts no proxy-CA secret")
		}
	}
}

// waitForSetTemplateSource waits until the RunnerSet reports the expected
// status.templateSource (Q172): which rung of the template-resolution chain resolved.
func waitForSetTemplateSource(t *testing.T, ns, name, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		return rs.Status.TemplateSource == want
	}, 20*time.Second, 100*time.Millisecond, "RunnerSet %s should report templateSource=%s", name, want)
}

// TestV2_RunnerSet_ResolvesViaGatewayDefaultTemplate: a RunnerSet with no templateRef
// of its own inherits the gateway's defaultTemplateRef (Q172, §H.4, rung 2) — it
// reaches Ready and reports status.templateSource=GatewayDefault.
func TestV2_RunnerSet_ResolvesViaGatewayDefaultTemplate(t *testing.T) {
	const ns = "v2-rs-gw-default-tmpl"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	// Gateway names a defaultTemplateRef; the RunnerSet sets no templateRef.
	gw := newGatewayForSet("gw", ns, "")
	gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "gw-default-tmpl"}
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("gw-default-tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	rs.Spec.TemplateRef = nil
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "gw-default-tmpl", Namespace: ns}})
	})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	waitForSetTemplateSource(t, ns, "set", v2alpha1.TemplateSourceGatewayDefault)
}

// TestV2_RunnerSet_NoTemplateFailsClosed: a RunnerSet with no templateRef whose gateway
// names a *missing* defaultTemplateRef fails closed Ready=False/TemplateNotFound (§H.4) —
// no worker pod is synthesized without a pod shape. (Rung 2 short-circuits before the
// cluster-default rung, so this is independent of any cluster-scoped template state.)
func TestV2_RunnerSet_NoTemplateFailsClosed(t *testing.T) {
	const ns = "v2-rs-no-tmpl"
	createNSForAGC(t, ns)
	startRunnerSetReconciler(t)

	gw := newGatewayForSet("gw", ns, "")
	gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "absent-tmpl"}
	require.NoError(t, k8sClient.Create(ctx, gw))
	rs := newRunnerSet("set", ns, "gw")
	rs.Spec.TemplateRef = nil
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), gw)
	})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonTemplateNotFound)
}

func TestV2_RunnerSet_ProvisionsWorkerPod(t *testing.T) {
	const ns = "v2-rs-worker"
	createNSForAGC(t, ns)

	// Apply the full object set up front (apply order must not matter, §H.7).
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "shared")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	require.NoError(t, k8sClient.Create(ctx, &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}))
	rs := newRunnerSet("worker-set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	startRunnerSetReconciler(t)

	// A session registers; enqueue a job on it, then the provisioner creates a
	// worker pod for the RunnerSet.
	id := enqueueJobOnSetSession(15*time.Second, "worker-set", nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for worker-set should register")

	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: "worker-set"},
		); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "worker Pod should be created for the RunnerSet")

	// Owner reference to the real RunnerSet (a synthesized RunnerGroup would have a
	// dangling owner-ref the apiserver GCs).
	foundOwner := false
	for _, o := range pod.OwnerReferences {
		if o.Kind == "RunnerSet" && o.Name == "worker-set" && o.Controller != nil && *o.Controller {
			foundOwner = true
		}
	}
	assert.True(t, foundOwner, "worker pod must be owner-referenced to the RunnerSet")

	// Pod shape comes from the resolved RunnerTemplate; egress is wired to the
	// resolved EgressProxy (HTTP(S)_PROXY + the proxy CA mount secret name).
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
	assert.Equal(t, "https://shared-proxy."+ns+".svc.cluster.local:8080", envByName["HTTPS_PROXY"])
	foundProxyCA := false
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "shared-proxy-tls" {
			foundProxyCA = true
		}
	}
	assert.True(t, foundProxyCA, "worker pod must project the resolved EgressProxy's CA cert")
}

// failingRegistrar makes agentpool.EnsureAgents fail by rejecting every Register call —
// the AGC can never provision a listener agent's Secret for the set.
type failingRegistrar struct{}

func (failingRegistrar) Register(_ context.Context, _ string, _ agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	return nil, errAgentRegisterRejected
}
func (failingRegistrar) Deregister(_ context.Context, _ string, _ int64) error { return nil }
func (failingRegistrar) ResolveAgentID(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

var errAgentRegisterRejected = errors.New("registration API rejected the agent")

// TestV2_RunnerSet_AgentProvisioningFailure_SetsReadyFalse: when every reference
// resolves but agentpool.EnsureAgents fails (the registration API rejects the agent),
// the RunnerSet must report Ready=False/AgentProvisioningFailed rather than silently
// staying healthy until the next reconcile (Q308). Direct egress keeps the fixture to
// a gateway + template.
func TestV2_RunnerSet_AgentProvisioningFailure_SetsReadyFalse(t *testing.T) {
	const ns = "v2-rs-agentfail"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithRegistrar(t, failingRegistrar{})

	waitForSetReadyReason(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonAgentProvisioningFailed)
}
