//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Q326: the v2 EgressProxy reconciler's ResourceQuota watch, proven against a real
// apiserver with real watch semantics. The reconciler runs WITHOUT the suite's 2s
// cache resync (startEgressProxyReconcilerNoResync) and the proxy pool is driven to
// Ready first — so once steady, the ONLY thing that can re-trigger a reconcile is
// the quota watch event itself.

// TestV2_EgressProxy_QuotaEditRetriggersReconcile proves an admin's ResourceQuota
// create/edit promptly refreshes the EgressProxy's ProxyQuotaPressure condition:
// creating a tight quota trips it, and raising the quota's .spec.hard clears it —
// with no EgressProxy write or child event in between (Q326).
func TestV2_EgressProxy_QuotaEditRetriggersReconcile(t *testing.T) {
	const ns = "v2-ep-quota-watch"
	createNamespace(t, ns)

	ep := newV2EgressProxyObject("pool", ns)
	ep.Spec.MinReplicas = ptr32(1) // maxReplicas stays the default 10
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	// A freshly-refreshed IP cache keeps EgressRulesStale quiet and pushes the
	// periodic egress recheck out to threshold/8 (~6h) — far beyond this test.
	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now())
	startEgressProxyReconcilerNoResync(t, ipCache)

	// envtest runs no kubelet, so drive the pool Ready by writing the proxy
	// Deployment's status once the reconciler has created it. Ready means the
	// reconciler returns no short not-ready requeue — the pool goes dormant.
	g := gomega.NewWithT(t)
	depKey := types.NamespacedName{Namespace: ns, Name: "pool-proxy"}
	var dep appsv1.Deployment
	g.Eventually(func() error {
		return k8sClient.Get(ctx, depKey, &dep)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	require.NoError(t, k8sClient.Status().Update(ctx, &dep))

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionReady)
	}, 20*time.Second, 100*time.Millisecond).Should(
		gomega.HaveField("Status", metav1.ConditionTrue),
		"the pool must be Ready (dormant) before the quota edit, so the watch is the only trigger")

	// Steady state without a quota: no pressure.
	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionProxyQuotaPressure)
	}, 20*time.Second, 100*time.Millisecond).Should(
		gomega.HaveField("Reason", "NoQuota"))

	// A tight quota created out-of-band must re-reconcile the proxy via the watch:
	// pods=2 cannot admit growth to the default maxReplicas of 10.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), quota) })

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionProxyQuotaPressure)
	}, 10*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", "InsufficientQuotaHeadroom"),
	), "creating a tight ResourceQuota must promptly trip ProxyQuotaPressure via the quota watch (Q326)")

	// Raising .spec.hard must clear the pressure through the hard-changed predicate.
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "rq"}, quota))
	quota.Spec.Hard[corev1.ResourcePods] = resource.MustParse("50")
	require.NoError(t, k8sClient.Update(ctx, quota))

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionProxyQuotaPressure)
	}, 10*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", "QuotaHeadroomSufficient"),
	), "raising the quota's .spec.hard must promptly clear ProxyQuotaPressure via the watch predicate (Q326)")

	// The condition freshness above is the observable contract; also pin that the
	// proxy stayed Ready throughout (the quota conditions are advisory).
	var got gmcv2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pool"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
}
