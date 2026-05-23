//go:build integration

package integration_test

import (
	"context"
	"net"
	"testing"
	"time"

	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"}, &np); err != nil {
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

	// Wait for the workload NetworkPolicy to include an egress rule on port 8080 targeting the
	// proxy Service ClusterIP as a /32 CIDR. The controller fetches the ClusterIP after
	// creating the Service, so this check waits for at least one post-Service reconcile.
	g.Eventually(func() bool {
		// Fetch the proxy Service ClusterIP.
		var svc corev1.Service
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"}, &svc); err != nil {
			return false
		}
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
			return false
		}
		expectedCIDR := svc.Spec.ClusterIP + "/32"

		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-workload"}, &np); err != nil {
			return false
		}
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
				if peer.IPBlock != nil && peer.IPBlock.CIDR == expectedCIDR {
					return true
				}
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"egress rule on port 8080 must target the proxy Service ClusterIP as a /32 CIDR")
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
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"}, &np); err != nil {
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
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"}, &np); err != nil {
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"}, &np))

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
