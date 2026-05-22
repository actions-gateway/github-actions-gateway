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

	// Wait for the NetworkPolicy to appear with the GitHub CIDR egress rule.
	g.Eventually(func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway"}, &np); err != nil {
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
	}, 15*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(),
		"expected egress rule for 140.82.112.0/20 on port 443")
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway"},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway"}, &np))

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
