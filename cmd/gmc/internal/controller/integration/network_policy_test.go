//go:build integration

package integration_test

import (
	"context"
	"net"
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	gmcnames "github.com/actions-gateway/github-actions-gateway/gmc/names"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGMC_NetworkPolicy_ProxyEgressContainsGitHubCIDRs(t *testing.T) {
	const nsName = "team-np-cidr"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	_, cidr, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr}}

	ag := newActionsGateway("cidr-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
	})

	startGMCReconciler(t, fetcher)

	g := gomega.NewWithT(t)

	// Wait for the proxy NetworkPolicy to appear with the GitHub CIDR egress rule.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &np); err != nil {
			return false
		}
		for _, rule := range np.Spec.Egress {
			for _, port := range rule.Ports {
				if port.Port != nil && port.Port.IntVal == 443 {
					for _, peer := range rule.To {
						if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
							return true
						}
					}
				}
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"expected egress rule for 140.82.112.0/20 on port 443")
}

func TestGMC_NetworkPolicy_AGCWorkerEgressToProxy(t *testing.T) {
	const nsName = "team-np-agc-egress"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("agc-egress-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// The workload NetworkPolicy must include an egress rule on port 8080 selecting
	// the proxy pods by label. The egress rule must NOT use an ipBlock on the proxy
	// Service ClusterIP — kube-proxy DNATs ClusterIP→PodIP before NetworkPolicy
	// enforcement, so an ipBlock rule on a ClusterIP silently never matches.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workloadName}, &np); err != nil {
			return false
		}
		return workloadNPHasProxyRule(np)
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"egress rule on port 8080 must select proxy pods by label (PodSelector app=actions-gateway-proxy)")
}

func TestGMC_NetworkPolicy_IPRangeReconciler_UpdatesExistingPolicy(t *testing.T) {
	const nsName = "team-np-iprange"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr1}}

	ag := newActionsGateway("iprange-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	ipRangeReconciler := startGMCReconciler(t, fetcher)

	g := gomega.NewWithT(t)

	// Wait for the proxy NetworkPolicy to appear with the first CIDR.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &np); err != nil {
			return false
		}
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
					return true
				}
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "initial proxy NetworkPolicy should contain 140.82.112.0/20")

	// Update the fetcher to return a new CIDR.
	_, cidr2, _ := net.ParseCIDR("1.2.3.0/24")
	fetcher.SetCIDRs([]net.IPNet{*cidr1, *cidr2})

	// Trigger an immediate reconcile via the IPRangeReconciler.
	ipRangeReconciler.ReconcileNow(ctx)

	// Assert the proxy NetworkPolicy now includes the new CIDR.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &np); err != nil {
			return false
		}
		found140, found1 := false, false
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil {
					if peer.IPBlock.CIDR == "140.82.112.0/20" {
						found140 = true
					}
					if peer.IPBlock.CIDR == "1.2.3.0/24" {
						found1 = true
					}
				}
			}
		}
		return found140 && found1
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"proxy NetworkPolicy should include both 140.82.112.0/20 and 1.2.3.0/24 after IP range update")

}

func TestGMC_NetworkPolicy_AGCPolicyExists(t *testing.T) {
	const nsName = "team-np-agc-policy"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("agc-policy-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for the AGC-specific NetworkPolicy to appear, selecting AGC pods by app label.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &np); err != nil {
			return false
		}
		// Selector must target AGC pods by app label (not the broad workload label).
		if np.Spec.PodSelector.MatchLabels["app"] != agcName {
			return false
		}
		// Must include port 443 egress for Kubernetes API server access.
		for _, rule := range np.Spec.Egress {
			for _, port := range rule.Ports {
				if port.Port != nil && port.Port.IntVal == 443 {
					return true
				}
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"actions-gateway-controller NetworkPolicy must exist with AGC pod selector and port-443 egress")
}

func TestGMC_NetworkPolicy_IPRangeRefresh_WorkloadEgressPreserved(t *testing.T) {
	const nsName = "team-np-preserve"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	_, cidr, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr}}

	ag := newActionsGateway("preserve-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	ipRangeReconciler := startGMCReconciler(t, fetcher)

	g := gomega.NewWithT(t)

	// Wait for workload NetworkPolicy with proxy egress rule (PodSelector-based).
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workloadName}, &np); err != nil {
			return false
		}
		return workloadNPHasProxyRule(np)
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"workload NP must have proxy egress rule before IP range refresh")

	// Trigger an IP range refresh — this updates only the proxy NP, not the workload NP.
	ipRangeReconciler.ReconcileNow(ctx)

	// Workload NP must still have its proxy egress rule after the refresh.
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workloadName}, &np))
	require.True(t, workloadNPHasProxyRule(np),
		"workload NP proxy egress rule must be preserved after IP range refresh")
}

// workloadNPHasProxyRule returns true if np contains an egress rule for port 8080
// targeting the proxy pods by label. The peer must be a PodSelector — not an
// ipBlock — because kube-proxy DNATs ClusterIP→PodIP before NetworkPolicy
// enforcement, so an ipBlock rule on the Service ClusterIP never matches.
func workloadNPHasProxyRule(np networkingv1.NetworkPolicy) bool {
	for _, rule := range np.Spec.Egress {
		hasPort8080 := false
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 8080 {
				hasPort8080 = true
				break
			}
		}
		if !hasPort8080 {
			continue
		}
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == gmcnames.ProxyName {
				return true
			}
		}
	}
	return false
}

func TestGMC_NetworkPolicy_ManagedFalse_NoGitHubCIDRs(t *testing.T) {
	const nsName = "team-np-managed-false"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	_, cidr, _ := net.ParseCIDR("140.82.112.0/20")
	fetcher := &stubIPFetcher{cidrs: []net.IPNet{*cidr}}

	managedFalse := false
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-false-gateway", Namespace: nsName},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
			Proxy: gmcv1alpha1.ProxyConfig{
				ManagedNetworkPolicy: &managedFalse,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
	})

	startGMCReconciler(t, fetcher)

	g := gomega.NewWithT(t)

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName}, &np))

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						require.NotEqual(t, "140.82.112.0/20", peer.IPBlock.CIDR,
							"GitHub CIDR must not appear when managedNetworkPolicy=false")
					}
				}
			}
		}
	}
}
