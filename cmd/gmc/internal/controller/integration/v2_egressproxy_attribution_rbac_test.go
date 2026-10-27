//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// startEgressProxyAsServiceAccount runs the EgressProxy reconciler and the tenant
// Secret access reconciler as the GMC ServiceAccount, with the production Secret
// cache bypass, so every Secret call the pool makes meets the shipped RBAC.
func startEgressProxyAsServiceAccount(t *testing.T) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	syncPeriod := 2 * time.Second
	mgr, err := ctrl.NewManager(gmcServiceAccountConfig(), ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}},
		}},
	})
	require.NoError(t, err)

	require.NoError(t, (&controller.TenantSecretAccessReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		ServiceAccountName:      gmcSAName,
		ServiceAccountNamespace: gmcSANamespace,
		Watches: &controller.TenantSecretWatches{
			Config: mgr.GetConfig(), HTTPClient: mgr.GetHTTPClient(), Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(),
		},
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.EgressProxyReconciler{
		Client:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		Scheme:     mgr.GetScheme(),
		IPCache:    &controller.IPRangeCache{},
		ProxyImage: "proxy:test",
		Recorder:   mgr.GetEventRecorder("egressproxy-controller"),
	}).SetupWithManager(mgr))

	go func() { _ = mgr.Start(mgrCtx) }()
}

// markPoolRolledOut reports the pool Deployment's current template as fully
// rolled out, as the Deployment controller envtest lacks would.
func markPoolRolledOut(t *testing.T, key types.NamespacedName) {
	t.Helper()
	var dep appsv1.Deployment
	if k8sClient.Get(ctx, key, &dep) != nil || dep.Spec.Replicas == nil {
		return
	}
	n := *dep.Spec.Replicas
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: n, UpdatedReplicas: n, AvailableReplicas: n, ReadyReplicas: n}
	_ = k8sClient.Status().Update(ctx, &dep)
}

// The pool provisions, projects and withdraws under the GMC's shipped RBAC, which
// reads Secrets only in tenant namespaces and never lists them cluster-wide.
func TestV2_EgressProxy_Attribution_RunsUnderTheShippedRBAC(t *testing.T) {
	installGMCShippedRBAC(t)
	const providerNS = "v2-attr-rbac-provider"
	const consumerNS = "v2-attr-rbac-consumer"
	marker := map[string]string{v2alpha1.TenantNamespaceMarkerLabel: v2alpha1.TenantNamespaceMarkerValue}
	createNamespaceWithLabels(t, providerNS, marker)
	createNamespaceWithLabels(t, consumerNS, marker)

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "attr-pool", Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			FailureAttribution: "PerConsumerPort",
			Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	ag := newV2GatewayWired("attr-gw", consumerNS, "attr-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "attr-pool", Namespace: providerNS}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startEgressProxyAsServiceAccount(t)
	depKey := types.NamespacedName{Namespace: providerNS, Name: "attr-pool-proxy"}
	projKey := types.NamespacedName{Namespace: consumerNS, Name: "proxy-attribution-" + providerNS + "-attr-pool"}

	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, depKey, &appsv1.Deployment{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "a pool that attributes nothing must still provision under the shipped RBAC")

	setGatewayAttribution(t, types.NamespacedName{Namespace: consumerNS, Name: "attr-gw"}, "PerJob")
	require.Eventually(t, func() bool {
		markPoolRolledOut(t, depKey)
		var s corev1.Secret
		return k8sClient.Get(ctx, projKey, &corev1.ConfigMap{}) == nil &&
			k8sClient.Get(ctx, projKey, &s) == nil && len(s.Data[corev1.TLSPrivateKeyKey]) > 0
	}, 30*time.Second, 250*time.Millisecond, "the consumer never received its port and client certificate")

	setGatewayAttribution(t, types.NamespacedName{Namespace: consumerNS, Name: "attr-gw"}, "Disabled")
	require.Eventually(t, func() bool {
		var s corev1.Secret
		return apierrors.IsNotFound(k8sClient.Get(ctx, projKey, &corev1.ConfigMap{})) &&
			k8sClient.Get(ctx, projKey, &s) == nil && len(s.Data[corev1.TLSPrivateKeyKey]) == 0
	}, 30*time.Second, 250*time.Millisecond, "withdrawal never removed the ConfigMap and blanked the key")

	var pool v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &pool))
	for _, c := range pool.Status.Conditions {
		if c.Type == "Degraded" {
			assert.Equal(t, metav1.ConditionFalse, c.Status, "the pool must not go Degraded: %s", c.Message)
			assert.False(t, strings.Contains(c.Message, "forbidden"), c.Message)
		}
	}
}

// A namespace that opts in after another is already projected changes the
// attributed set, so the pool reissues a bundle that already exists, which is a
// patch on two owned Secrets (Q1200, Q1214).
func TestV2_EgressProxy_Attribution_AddsASecondConsumerUnderTheShippedRBAC(t *testing.T) {
	installGMCShippedRBAC(t)
	const providerNS = "v2-attr-rbac-second-provider"
	const alphaNS = "v2-attr-rbac-second-alpha"
	const betaNS = "v2-attr-rbac-second-beta"
	marker := map[string]string{v2alpha1.TenantNamespaceMarkerLabel: v2alpha1.TenantNamespaceMarkerValue}
	for _, ns := range []string{providerNS, alphaNS, betaNS} {
		createNamespaceWithLabels(t, ns, marker)
	}

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "attr-pool", Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			FailureAttribution: "PerConsumerPort",
			Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: []string{alphaNS, betaNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	optIn := func(ns string) {
		ag := newV2GatewayWired("attr-gw", ns, "attr-creds", "")
		ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "attr-pool", Namespace: providerNS}
		ag.Spec.ProxyFailureAttribution = "PerJob"
		require.NoError(t, k8sClient.Create(ctx, ag))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
	}
	optIn(alphaNS)

	startEgressProxyAsServiceAccount(t)
	depKey := types.NamespacedName{Namespace: providerNS, Name: "attr-pool-proxy"}
	projName := "proxy-attribution-" + providerNS + "-attr-pool"
	projected := func(ns string) bool {
		markPoolRolledOut(t, depKey)
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: projName}, &corev1.ConfigMap{}) == nil
	}
	require.Eventually(t, func() bool { return projected(alphaNS) }, 30*time.Second, 250*time.Millisecond,
		"the first consumer never received its port")

	optIn(betaNS)
	require.Eventually(t, func() bool { return projected(betaNS) }, 30*time.Second, 250*time.Millisecond,
		"the second consumer never received its port")

	var pool v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &pool))
	for _, c := range pool.Status.Conditions {
		if c.Type == "Degraded" {
			assert.Equal(t, metav1.ConditionFalse, c.Status, "the pool must not go Degraded adding a consumer: %s", c.Message)
		}
	}
}

// A departing namespace with a RunnerSet is held, not withdrawn outright, under
// the same shipped RBAC: the hold reads RunnerSets and records itself on the pool
// Deployment, and neither may need a grant the GMC lacks (Q1208).
func TestV2_EgressProxy_Attribution_HoldsUnderTheShippedRBAC(t *testing.T) {
	installGMCShippedRBAC(t)
	const providerNS = "v2-attr-rbac-hold-provider"
	const consumerNS = "v2-attr-rbac-hold-consumer"
	marker := map[string]string{v2alpha1.TenantNamespaceMarkerLabel: v2alpha1.TenantNamespaceMarkerValue}
	createNamespaceWithLabels(t, providerNS, marker)
	createNamespaceWithLabels(t, consumerNS, marker)

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-pool", Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			FailureAttribution: "PerConsumerPort",
			Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	rs := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-rs", Namespace: consumerNS},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:        v2alpha1.ObjectRef{Name: "hold-gw"},
			RunnerLabels:      []string{"self-hosted"},
			MaxWorkerLifetime: &metav1.Duration{Duration: time.Hour},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	ag := newV2GatewayWired("hold-gw", consumerNS, "hold-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "hold-pool", Namespace: providerNS}
	ag.Spec.ProxyFailureAttribution = "PerJob"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startEgressProxyAsServiceAccount(t)
	depKey := types.NamespacedName{Namespace: providerNS, Name: "hold-pool-proxy"}
	projKey := types.NamespacedName{Namespace: consumerNS, Name: "proxy-attribution-" + providerNS + "-hold-pool"}
	require.Eventually(t, func() bool {
		markPoolRolledOut(t, depKey)
		return k8sClient.Get(ctx, projKey, &corev1.ConfigMap{}) == nil
	}, 30*time.Second, 250*time.Millisecond, "the consumer never received its port")

	setGatewayAttribution(t, types.NamespacedName{Namespace: consumerNS, Name: "hold-gw"}, "Disabled")
	require.Eventually(t, func() bool {
		var dep appsv1.Deployment
		return apierrors.IsNotFound(k8sClient.Get(ctx, projKey, &corev1.ConfigMap{})) &&
			k8sClient.Get(ctx, depKey, &dep) == nil && dep.Annotations["actions-gateway.com/attribution-held-until"] != ""
	}, 30*time.Second, 250*time.Millisecond, "the pool never withdrew the projection and recorded a hold")

	var pool v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &pool))
	for _, c := range pool.Status.Conditions {
		if c.Type == "Degraded" {
			assert.Equal(t, metav1.ConditionFalse, c.Status, "the pool must not go Degraded holding a port: %s", c.Message)
		}
	}
}
