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
// shared envtest apiserver with a real Provisioner attached.
func startRunnerSetReconciler(t *testing.T) {
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
		Client:       mgr.GetClient(),
		TokenManager: tm,
		Registrar:    &brokerRegistrar{stub: brokerStub},
		AgentKeyType: agentpool.KeyTypeEd25519,
		Provisioner:  p,
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
}

func newRunnerSet(name, ns, gateway string) *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:   v2alpha1.ObjectRef{Name: gateway},
			TemplateRef:  v2alpha1.ObjectRef{Name: "tmpl"},
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
		},
	}
}

func newGatewayForSet(name, ns, proxyRef string) *v2alpha1.ActionsGateway {
	ag := &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.ActionsGatewaySpec{
			GitHubAppRef: v2alpha1.LocalSecretReference{Name: "github-app"},
			GitHubURL:    "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &v2alpha1.LocalObjectRef{Name: proxyRef}
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
	id := enqueueJobOnOwnerSession(15*time.Second, "worker-set", nil, broker.RunnerJobRequestBody{})
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
