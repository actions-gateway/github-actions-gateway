//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q506 #3: the one GitHub Enterprise Server gap the GMC cannot close in code. The
// default CIDR egress mode programs the ranges api.github.com/meta publishes, and an
// appliance on the customer's own address space appears in none of them — so the
// NetworkPolicy denies the proxy's traffic to the one host it exists to reach. The
// appliance's ranges are knowable only to the operator, so the reconciler names the
// obligation on status rather than leaving it to surface as a connect timeout.

// TestV2_EgressProxy_GHESReferrerInCIDRModeFlagsEgressGap drives the full arc: a
// public-GitHub pool is quiet, binding a GHES gateway trips the condition, and
// supplying destinationCIDRs clears it.
func TestV2_EgressProxy_GHESReferrerInCIDRModeFlagsEgressGap(t *testing.T) {
	const ns = "v2-ep-ghes-egress"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	startEgressProxyReconciler(t, nil)

	ep := newV2EgressProxyObject("pool", ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	g := gomega.NewWithT(t)

	// No referrer: nothing to be incomplete about.
	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionGitHubEgressIncomplete)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", "GitHubEgressAllowed"),
	))

	gw := newV2GatewayWired("ghesgw", ns, "github-app", "pool")
	gw.Spec.GitHubURL = "https://ghes.example.com/example-org"
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gw) })

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionGitHubEgressIncomplete)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", "ApplianceRangesRequired"),
		gomega.HaveField("Message", gomega.ContainSubstring("ghes.example.com")),
	), "binding a GHES gateway to a CIDR-mode pool must name the unreachable appliance")

	// The operator's declaration clears it — the GMC cannot verify the ranges cover
	// the appliance, only that the obligation was answered.
	require.Eventually(t, func() bool {
		var got gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pool"}, &got); err != nil {
			return false
		}
		// Inside the suite's platform egress allowlist: destinationCIDRs is
		// platform-gated, so the tenant's remedy needs an admin's allowlist entry.
		got.Spec.DestinationCIDRs = []string{"10.10.0.0/16"}
		return k8sClient.Update(ctx, &got) == nil
	}, 10*time.Second, 100*time.Millisecond, "destinationCIDRs update should land")

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionGitHubEgressIncomplete)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", "GitHubEgressAllowed"),
	), "supplying the appliance's ranges must clear the obligation")
}
