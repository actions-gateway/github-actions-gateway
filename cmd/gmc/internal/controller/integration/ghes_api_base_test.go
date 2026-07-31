//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A GitHub Enterprise Server gateway must reach its own appliance's REST API, not
// api.github.com (Q506 #1). The AGC resolves that base from GITHUB_API_BASE_URL and
// falls back to api.github.com when it is unset, and nothing on either provisioning
// path set it — so a GHES tenant POSTed its App JWT to a host that had never issued
// the App and failed at token exchange before acquiring any job.
//
// These assert against the real apiserver rather than the builder alone because the
// defect was an absent variable: a builder test that only checks the value would
// have passed on a Deployment that never carried it. Each requires the variable to
// be present AND to address the appliance, so removing the injection fails both.

// TestGMC_GHESGateway_AGCAddressesTheAppliance covers the v1 path.
func TestGMC_GHESGateway_AGCAddressesTheAppliance(t *testing.T) {
	const nsName = "team-ghes-apibase"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("ghes-gateway", nsName, "github-app")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep); err != nil {
			return ""
		}
		return envValue(&dep, "GITHUB_API_BASE_URL")
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Equal("https://ghes.example.com/api/v3"),
		"the AGC's token exchange must address the GHES appliance, not api.github.com")
}

// TestV2_GHESGateway_AGCAddressesTheAppliance covers the v2 path, where the AGC
// Deployment is named per gateway.
func TestV2_GHESGateway_AGCAddressesTheAppliance(t *testing.T) {
	const ns = "v2-ghes-apibase"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	ag := newV2GatewayWired("ghesgw", ns, "github-app", "")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	g := gomega.NewWithT(t)
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ghesgw-agc"}, &dep); err != nil {
			return ""
		}
		return envValue(&dep, "GITHUB_API_BASE_URL")
	}, 15*time.Second, 100*time.Millisecond).Should(gomega.Equal("https://ghes.example.com/api/v3"),
		"the AGC's token exchange must address the GHES appliance, not api.github.com")
}

// TestV2_PublicGateway_AGCKeepsPublicAPIBase pins the no-change half: a github.com
// gateway carries the value the AGC already defaulted to, so the rollout that adds
// the variable changes no public-SaaS tenant's behaviour.
func TestV2_PublicGateway_AGCKeepsPublicAPIBase(t *testing.T) {
	const ns = "v2-public-apibase"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
		})
	})

	ag := newV2GatewayWired("publicgw", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	g := gomega.NewWithT(t)
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "publicgw-agc"}, &dep); err != nil {
			return ""
		}
		return envValue(&dep, "GITHUB_API_BASE_URL")
	}, 15*time.Second, 100*time.Millisecond).Should(gomega.Equal("https://api.github.com"))
}
