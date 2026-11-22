//go:build integration

package integration_test

import (
	"context"
	"net"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// startCountingV2Reconcilers runs the v2 ActionsGateway and EgressProxy reconcilers in
// one manager resyncing every syncPeriod, and returns their reconcile counters.
func startCountingV2Reconcilers(t *testing.T, ipCache *controller.IPRangeCache, syncPeriod time.Duration) (ag, ep *reconcileCounter) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
	})
	require.NoError(t, err)

	ag = newReconcileCounterFor(mgr.GetClient(), func(o client.Object) bool { _, ok := o.(*v2.ActionsGateway); return ok })
	ep = newReconcileCounter(mgr.GetClient())
	require.NoError(t, (&controller.ActionsGatewayV2Reconciler{
		Client:   ag,
		Scheme:   mgr.GetScheme(),
		AGCImage: "agc:test",
		IPCache:  ipCache,
		Recorder: mgr.GetEventRecorder("actionsgateway-v2-controller"),
		Reader:   mgr.GetAPIReader(),
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.EgressProxyReconciler{
		Client:          ep,
		APIReader:       mgr.GetAPIReader(),
		Scheme:          mgr.GetScheme(),
		IPCache:         ipCache,
		ProxyImage:      "proxy:test",
		EgressAllowlist: egressTestAllowlist,
		Recorder:        mgr.GetEventRecorder("egressproxy-controller"),
	}).SetupWithManager(mgr))

	go func() { _ = mgr.Start(mgrCtx) }()
	return ag, ep
}

// TestV2_SteadyState_NoChildChurn pins the apply path's no-op on a real apiserver: the
// apply helpers replace whole Specs with builder objects that omit server-defaulted
// fields, so each reconcile sends a non-empty patch and relies on the apiserver
// re-defaulting it into an unchanged object. A fake client neither defaults nor skips
// no-op writes, so only this tier sees a child's resourceVersion stay put.
// TestV2_GMCRestart_LeavesTheAGCDeploymentUntouched covers one object across a
// restart; this covers every child of a gateway and its EgressProxy across resyncs.
func TestV2_SteadyState_NoChildChurn(t *testing.T) {
	const ns = "v2-steady-state"
	createNamespaceWithLabels(t, ns, map[string]string{v2.SecurityProfileLabel: v2.SecurityProfileRestricted})
	createGitHubAppSecret(t, ns, "github-app")

	_, cidr, _ := net.ParseCIDR("140.82.112.0/20")
	ipCache := &controller.IPRangeCache{}
	ipCache.Set([]net.IPNet{*cidr})
	ipCache.MarkRefreshed(time.Now())

	ep := newV2EgressProxyObject(egressProxyName, ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	ag := newV2GatewayWired("gw", ns, "github-app", egressProxyName)
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
		_ = k8sClient.Delete(context.Background(), ep)
		_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + ns + ".gw"}})
	})

	const syncPeriod = 2 * time.Second
	agCount, epCount := startCountingV2Reconcilers(t, ipCache, syncPeriod)

	proxy := proxyChildName(egressProxyName)
	targets := []struct {
		label string
		name  string
		obj   client.Object
	}{
		{"AGC Deployment", "gw-agc", &appsv1.Deployment{}},
		{"AGC Service", "gw-agc", &corev1.Service{}},
		{"AGC ServiceAccount", "gw-agc", &corev1.ServiceAccount{}},
		{"worker ServiceAccount", "gw-worker", &corev1.ServiceAccount{}},
		{"AGC RoleBinding", "gw-agc", &rbacv1.RoleBinding{}},
		{"AGC NetworkPolicy", "gw-agc", &networkingv1.NetworkPolicy{}},
		{"workload NetworkPolicy", "gw-workload", &networkingv1.NetworkPolicy{}},
		{"AGC metrics server Secret", "gw-agc-metrics-tls", &corev1.Secret{}},
		{"AGC metrics client Secret", "gw-agc-metrics-client", &corev1.Secret{}},
		{"proxy Deployment", proxy, &appsv1.Deployment{}},
		{"proxy Service", proxy, &corev1.Service{}},
		{"proxy HPA", proxy, &autoscalingv2.HorizontalPodAutoscaler{}},
		{"proxy PDB", proxy, &policyv1.PodDisruptionBudget{}},
		{"proxy NetworkPolicy", proxy, &networkingv1.NetworkPolicy{}},
		{"proxy TLS Secret", proxyTLSName(egressProxyName), &corev1.Secret{}},
	}
	for _, tt := range targets {
		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: tt.name}, tt.obj) == nil
		}, 20*time.Second, 100*time.Millisecond, "%s should be created", tt.label)
	}

	// Two resyncs past provisioning let the first pass's follow-on writes (finalizer,
	// status, the EgressProxy readiness the gateway reads) land before the baseline.
	readRVs := func() map[string]string {
		rvs := make(map[string]string, len(targets))
		for _, tt := range targets {
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: tt.name}, tt.obj))
			rvs[tt.label] = tt.obj.GetResourceVersion()
		}
		return rvs
	}
	time.Sleep(2 * syncPeriod)
	baseline := readRVs()
	agBefore, epBefore := agCount.startedIn(ns), epCount.startedIn(ns)

	// Three resync periods: each reconciler must have run at least twice in the window,
	// or a stable resourceVersion would say nothing about the apply path.
	time.Sleep(3 * syncPeriod)
	assert.Equal(t, baseline, readRVs(), "steady-state reconciles must not rewrite any child")
	assert.GreaterOrEqual(t, agCount.startedIn(ns)-agBefore, 2, "ActionsGateway reconciles inside the window")
	assert.GreaterOrEqual(t, epCount.startedIn(ns)-epBefore, 2, "EgressProxy reconciles inside the window")
}

// TestV2_ActionsGateway_LeavesTheResourceQuotaAlone pins the least-privilege property
// docs/design/05-security.md states: the namespace ResourceQuota is platform-owned, so
// the GMC neither creates one nor adopts or rewrites the one that exists.
func TestV2_ActionsGateway_LeavesTheResourceQuotaAlone(t *testing.T) {
	const ns = "v2-no-quota"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	platform := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-quota", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("50")}},
	}
	require.NoError(t, k8sClient.Create(ctx, platform))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), platform) })
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(platform), platform))
	platformRV := platform.ResourceVersion

	ep := newV2EgressProxyObject(egressProxyName, ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	ag := newV2GatewayWired("gw", ns, "github-app", egressProxyName)
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
		_ = k8sClient.Delete(context.Background(), ep)
		_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + ns + ".gw"}})
	})
	startCountingV2Reconcilers(t, nil, 2*time.Second)

	// The AGC Deployment and the proxy PDB are the last children each reconciler
	// applies, so both existing means a full pass of each has run.
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &appsv1.Deployment{}) == nil &&
			k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &policyv1.PodDisruptionBudget{}) == nil
	}, 20*time.Second, 100*time.Millisecond, "both reconcilers should complete a pass")

	var quotas corev1.ResourceQuotaList
	require.NoError(t, k8sClient.List(ctx, &quotas, client.InNamespace(ns)))
	assert.Len(t, quotas.Items, 1, "the GMC must not author a ResourceQuota")
	var got corev1.ResourceQuota
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(platform), &got))
	assert.Empty(t, got.OwnerReferences, "the platform quota must not be adopted")
	assert.Equal(t, platformRV, got.ResourceVersion, "the platform quota must not be rewritten")
}
