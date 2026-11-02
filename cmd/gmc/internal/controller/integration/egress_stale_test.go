//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q157 EgressRulesStale condition, proven against a real apiserver. The shared
// IP-range cache's last-refresh timestamp is driven directly (as the
// IPRangeReconciler would on a successful fetch), and the test asserts the
// ActionsGateway reconciler surfaces — and clears — the staleness on its status.

func egressCondition(t *testing.T, nsName, name string) *metav1.Condition {
	t.Helper()
	var ag gmcv1alpha1.ActionsGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: name}, &ag); err != nil {
		return nil
	}
	return meta.FindStatusCondition(ag.Status.Conditions, gmcv1alpha1.ConditionEgressRulesStale)
}

// TestGMC_EgressRulesStale_TripsAndRecovers proves that a stalled IP-range refresh
// (last success older than the staleness window) trips EgressRulesStale=True on
// the gateway, and that a fresh refresh clears it.
func TestGMC_EgressRulesStale_TripsAndRecovers(t *testing.T) {
	const nsName = "team-egress-stale"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("egress-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	// A shared cache whose last successful refresh is well past the 48h window.
	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now().Add(-72 * time.Hour))
	startGMCReconcilerWithOptions(t, nil, gmcReconcilerOptions{
		ipCache:         ipCache,
		egressThreshold: 48 * time.Hour,
	})

	g := gomega.NewWithT(t)

	g.Eventually(func() *metav1.Condition {
		return egressCondition(t, nsName, "egress-gateway")
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv1alpha1.ReasonRefreshStalled),
	), "a stalled IP-range refresh must trip EgressRulesStale")

	// A fresh refresh must clear it (the 2s SyncPeriod re-reconciles).
	ipCache.MarkRefreshed(time.Now())

	g.Eventually(func() bool {
		c := egressCondition(t, nsName, "egress-gateway")
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == gmcv1alpha1.ReasonRefreshCurrent
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"a fresh refresh must clear EgressRulesStale")
}

// TestGMC_EgressRulesStale_NeverSucceeded proves that while no refresh has ever
// succeeded the condition is True with RefreshNeverSucceeded: the proxy
// NetworkPolicy carries no GitHub rule, which no other condition reports (Q1228).
func TestGMC_EgressRulesStale_NeverSucceeded(t *testing.T) {
	const nsName = "team-egress-pending"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("egress-pending-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	// A cache that has never been refreshed.
	startGMCReconcilerWithOptions(t, nil, gmcReconcilerOptions{
		ipCache:         &controller.IPRangeCache{},
		egressThreshold: 48 * time.Hour,
	})

	g := gomega.NewWithT(t)
	g.Eventually(func() bool {
		c := egressCondition(t, nsName, "egress-pending-gateway")
		return c != nil && c.Status == metav1.ConditionTrue &&
			c.Reason == gmcv1alpha1.ReasonRefreshNeverSucceeded
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"with no refresh ever succeeded, EgressRulesStale reports RefreshNeverSucceeded")
}
