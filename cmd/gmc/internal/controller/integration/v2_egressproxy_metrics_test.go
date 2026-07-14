//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Q320: the GMC's ProxyQuota (Q82) and EgressRulesStale (Q157) conditions, ported
// from the v1 ActionsGateway onto the v2 EgressProxy, proven against a real
// apiserver — and the v2-aware gauge collectors reflecting a real v2 EgressProxy.

func egressProxyCondition(t *testing.T, ns, name, condType string) *metav1.Condition {
	t.Helper()
	var ep gmcv2alpha1.EgressProxy
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ep); err != nil {
		return nil
	}
	return meta.FindStatusCondition(ep.Status.Conditions, condType)
}

// TestV2_EgressProxy_SetsQuotaAndStaleConditions proves the EgressProxy reconciler
// surfaces ProxyQuotaPressure (a tight namespace ResourceQuota can't admit growth to
// maxReplicas) and EgressRulesStale (the shared IP-range refresh is past the window)
// on the EgressProxy's own status — the signals the gauge collectors count.
func TestV2_EgressProxy_SetsQuotaAndStaleConditions(t *testing.T) {
	const ns = "v2-ep-metrics"
	createNamespace(t, ns)

	// pods=2 hard cap: the default pool (maxReplicas 10) cannot scale into it.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), quota) })

	ep := newV2EgressProxyObject("pool", ns)
	ep.Spec.EgressPolicyMode = gmcv2alpha1.EgressPolicyModeCIDR
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })

	// A shared cache whose last successful refresh is past the default 49h window.
	ipCache := &controller.IPRangeCache{}
	ipCache.MarkRefreshed(time.Now().Add(-72 * time.Hour))
	startEgressProxyReconciler(t, ipCache)

	g := gomega.NewWithT(t)
	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionProxyQuotaPressure)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", "InsufficientQuotaHeadroom"),
	), "a tight ResourceQuota must trip ProxyQuotaPressure on the EgressProxy")

	g.Eventually(func() *metav1.Condition {
		return egressProxyCondition(t, ns, "pool", gmcv2alpha1.ConditionEgressRulesStale)
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", gmcv2alpha1.ReasonRefreshStalled),
	), "a stalled IP-range refresh must trip EgressRulesStale on the EgressProxy")
}

// gaugeValue extracts the value of a gauge series identified by name and an exact
// label set from a gathered registry, or returns (0, false) if absent.
func gaugeValue(families []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := len(got) == len(labels)
			for k, v := range labels {
				if got[k] != v {
					match = false
				}
			}
			if match {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestV2_GaugeCollectors_ReflectV2 proves the v2-aware collectors, reading a real
// apiserver, count a v2 ActionsGateway in managed_gateways and reflect a v2
// EgressProxy's ProxyQuota/EgressRulesStale conditions. NewMetrics registers on the
// process-global controller-runtime registry, so it must be the SOLE caller in the
// integration binary (the unit suite exercises NewMetrics in its own binary).
func TestV2_GaugeCollectors_ReflectV2(t *testing.T) {
	const ns = "v2-gauge-collectors"
	createNamespace(t, ns)

	ag := newV2ActionsGateway(ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	ep := newV2EgressProxyObject("pool", ns)
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ep) })
	// Hand-set the proxy conditions (no reconciler in this test) via the status
	// subresource, then assert the collectors reflect them.
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionProxyQuotaPressure, Status: metav1.ConditionTrue,
		Reason: "InsufficientQuotaHeadroom", Message: "test",
	})
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionEgressRulesStale, Status: metav1.ConditionTrue,
		Reason: gmcv2alpha1.ReasonRefreshStalled, Message: "test",
	})
	require.NoError(t, k8sClient.Status().Update(ctx, ep))

	// v2Enabled=true: the collectors also list the v2 kinds.
	_ = controller.NewMetrics(k8sClient, true)

	g := gomega.NewWithT(t)
	epLabels := map[string]string{"namespace": ns, "name": "pool"}
	g.Eventually(func() bool {
		fams, err := ctrlmetrics.Registry.Gather()
		if err != nil {
			return false
		}
		managed, okM := gaugeValue(fams, "actions_gateway_managed_gateways", map[string]string{})
		pressure, okP := gaugeValue(fams, "actions_gateway_proxy_quota_pressure", epLabels)
		stale, okS := gaugeValue(fams, "actions_gateway_egress_rules_stale", epLabels)
		return okM && managed >= 1 && okP && pressure == 1 && okS && stale == 1
	}, 15*time.Second, 200*time.Millisecond).Should(gomega.BeTrue(),
		"managed_gateways must count the v2 gateway and the proxy gauges must reflect the v2 EgressProxy conditions")

	// The exceeded gauge is a distinct series that reflects False for this proxy.
	fams, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	exceeded, ok := gaugeValue(fams, "actions_gateway_proxy_quota_exceeded", epLabels)
	assert.True(t, ok, "the exceeded gauge is emitted per v2 EgressProxy")
	assert.Equal(t, 0.0, exceeded, "no ReplicaFailure, so ProxyQuotaExceeded is False")
}
