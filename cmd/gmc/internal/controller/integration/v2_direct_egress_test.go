//go:build integration

package integration_test

import (
	"context"
	"net"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// These tests exercise the v2 direct-egress (optional-proxy) behavior (Q168, §H.10)
// against the real apiserver: a v2 ActionsGateway with no defaultProxyRef provisions
// the AGC control plane with direct egress — no proxy env on the AGC, and AGC +
// workload NetworkPolicies carrying the GitHub-CIDR allowlist (DNS + GitHub + kube
// API, never arbitrary internet) — reports proxyMode Direct + EgressUnattributed, and
// the IPRangeReconciler keeps those direct NetworkPolicies current.

// startActionsGatewayV2ReconcilerWithIPRange starts a v2 ActionsGateway reconciler
// wired to a shared IP-range cache, plus an IPRangeReconciler over that cache, for the
// duration of a test. The cache is pre-populated from the fetcher (mirroring the
// steady state after the startup fetch). Returns the IPRangeReconciler so a test can
// trigger a manual refresh, and the shared cache.
func startActionsGatewayV2ReconcilerWithIPRange(t *testing.T, fetcher controller.GitHubIPRangeFetcher) (*controller.IPRangeReconciler, *controller.IPRangeCache) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	syncPeriod := 2 * time.Second
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
	})
	require.NoError(t, err)

	if fetcher == nil {
		fetcher = &stubIPFetcher{cidrs: []net.IPNet{}}
	}
	ipCache := &controller.IPRangeCache{}
	if cidrs, fetchErr := fetcher.FetchIPRanges(ctx); fetchErr == nil {
		ipCache.Set(cidrs)
	}

	err = (&controller.ActionsGatewayV2Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		AGCImage: "agc:test",
		IPCache:  ipCache,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	ipRangeReconciler := &controller.IPRangeReconciler{
		Client:  mgr.GetClient(),
		Fetcher: fetcher,
		Cache:   ipCache,
	}

	go func() { _ = mgr.Start(mgrCtx) }()
	return ipRangeReconciler, ipCache
}

// npHasGitHubCIDR reports whether np has an egress rule with an ipBlock peer for cidr.
func npHasGitHubCIDR(np *networkingv1.NetworkPolicy, cidr string) bool {
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				return true
			}
		}
	}
	return false
}

func TestV2_DirectEgress_ProvisionsRestrictedControlPlane(t *testing.T) {
	const ns = "v2-direct-egress"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	_, cidr, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr}}

	// No defaultProxyRef ⇒ direct egress.
	ag := newV2GatewayWired("gw", ns, "github-app", "")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2ReconcilerWithIPRange(t, fetcher)

	// AGC Deployment comes up with NO proxy env (direct egress) and no proxy-CA mount.
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should be created in direct mode")
	assert.Empty(t, envValue(&dep, "HTTP_PROXY"), "direct egress: AGC has no HTTP_PROXY")
	assert.Empty(t, envValue(&dep, "HTTPS_PROXY"), "direct egress: AGC has no HTTPS_PROXY")
	assert.Empty(t, envValue(&dep, "PROXY_TLS_SECRET_NAME"))
	for _, v := range dep.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, "proxy-ca", v.Name)
		if v.Secret != nil {
			assert.NotContains(t, v.Secret.SecretName, "-proxy-tls", "direct mode mounts no proxy-CA secret")
		}
	}

	// AGC NetworkPolicy: direct egress allows the GitHub CIDR on 443 AND keeps the
	// mandatory kube-API egress (443/6443). Restriction preserved.
	var agcNP networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &agcNP))
	assert.True(t, npHasGitHubCIDR(&agcNP, "140.82.112.0/20"), "direct AGC NP allows GitHub CIDR")
	saw6443 := false
	for _, rule := range agcNP.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 6443 {
				saw6443 = true
			}
		}
	}
	assert.True(t, saw6443, "AGC NP keeps the mandatory kube-API egress in direct mode")

	// Workload NetworkPolicy: direct egress allows the GitHub CIDR; default-deny ingress.
	var workloadNP networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-workload"}, &workloadNP))
	assert.True(t, npHasGitHubCIDR(&workloadNP, "140.82.112.0/20"), "direct workload NP allows GitHub CIDR")
	assert.Empty(t, workloadNP.Spec.Ingress, "workload NP default-denies ingress")

	// Status: proxyMode Direct + EgressUnattributed=True; not Degraded.
	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
			return false
		}
		unattr := findCondition(got.Status.Conditions, v2alpha1.ConditionEgressUnattributed)
		degraded := findCondition(got.Status.Conditions, v2alpha1.ConditionDegraded)
		return got.Status.ProxyMode == v2alpha1.ProxyModeDirect &&
			unattr != nil && unattr.Status == metav1.ConditionTrue && unattr.Reason == v2alpha1.ReasonDirectEgress &&
			degraded != nil && degraded.Status == metav1.ConditionFalse
	}, 15*time.Second, 100*time.Millisecond, "status should report proxyMode Direct + EgressUnattributed=True, not Degraded")
}

func TestV2_DirectEgress_IPRangeRefreshUpdatesDirectPolicies(t *testing.T) {
	const ns = "v2-direct-iprange"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr1}}

	ag := newV2GatewayWired("gw", ns, "github-app", "") // direct
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	ipRangeReconciler, _ := startActionsGatewayV2ReconcilerWithIPRange(t, fetcher)

	// Wait for the direct AGC + workload NetworkPolicies to carry the first CIDR.
	require.Eventually(t, func() bool {
		var agcNP, workloadNP networkingv1.NetworkPolicy
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &agcNP) != nil {
			return false
		}
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-workload"}, &workloadNP) != nil {
			return false
		}
		return npHasGitHubCIDR(&agcNP, "140.82.112.0/20") && npHasGitHubCIDR(&workloadNP, "140.82.112.0/20")
	}, 15*time.Second, 100*time.Millisecond, "direct NetworkPolicies should carry the initial CIDR")

	// GitHub rotates ranges: the fetcher now returns a second CIDR.
	_, cidr2, _ := net.ParseCIDR("1.2.3.0/24")
	fetcher.SetCIDRs([]net.IPNet{*cidr1, *cidr2})
	ipRangeReconciler.ReconcileNow(ctx)

	// The relocated refresh loop (Q168) patches the direct AGC + workload NPs with the
	// new range — the direct-egress allowlist stays current.
	require.Eventually(t, func() bool {
		var agcNP, workloadNP networkingv1.NetworkPolicy
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &agcNP) != nil {
			return false
		}
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-workload"}, &workloadNP) != nil {
			return false
		}
		return npHasGitHubCIDR(&agcNP, "1.2.3.0/24") && npHasGitHubCIDR(&workloadNP, "1.2.3.0/24") &&
			npHasGitHubCIDR(&agcNP, "140.82.112.0/20") && npHasGitHubCIDR(&workloadNP, "140.82.112.0/20")
	}, 15*time.Second, 100*time.Millisecond, "IP refresh must patch the direct-egress NetworkPolicies with the new CIDR")
}
