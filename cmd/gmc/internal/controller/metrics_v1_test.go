package controller

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// The v1 ActionsGateway half of the scrape-time collector tests, and the v1
// fixtures they share with the dual-version tests in metrics_test.go. Paired with
// metrics_v1.go so the v1 sunset (Q273) deletes both whole (Q403).

func managedGateway(name string, deleting bool) *gmcv1alpha1.ActionsGateway {
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	if deleting {
		now := metav1.Now()
		ag.DeletionTimestamp = &now
		// A deletion timestamp only persists in the fake client with a finalizer.
		ag.Finalizers = []string{"actions-gateway/test"}
	}
	return ag
}

// gatewayWithCondition builds an ActionsGateway carrying a single status
// condition of the given type/status, for driving the condition-mirroring
// collectors (runnerGroupsDegradedCollector, proxyQuotaCollector,
// egressRulesStaleCollector).
func gatewayWithCondition(name, condType string, status metav1.ConditionStatus) *gmcv1alpha1.ActionsGateway {
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: "Test", Message: "test",
	})
	return ag
}

// The managed-gateways collector must report the count of non-deleting CRs.
func TestManagedGatewaysCollector(t *testing.T) {
	scheme := newIPRangeScheme(t)

	t.Run("none", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(scheme).Build()
		c := newManagedGatewaysCollector(fc, false)
		assert.Equal(t, 0.0, testutil.ToFloat64(c))
	})

	t.Run("counts active, excludes deleting", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(
				managedGateway("a", false),
				managedGateway("b", false),
				managedGateway("c", true),
			).Build()
		c := newManagedGatewaysCollector(fc, false)
		assert.Equal(t, 2.0, testutil.ToFloat64(c),
			"two active gateways; the deleting one is excluded")
	})
}

// TestRunnerGroupsDegradedCollector_MirrorsCondition asserts the collector
// exports a 1/0 gauge per ActionsGateway that mirrors the RunnerGroupsDegraded
// condition, and that a deleting gateway is skipped entirely.
func TestRunnerGroupsDegradedCollector_MirrorsCondition(t *testing.T) {
	scheme := newIPRangeScheme(t)

	degraded := gatewayWithCondition("degraded", gmcv1alpha1.ConditionRunnerGroupsDegraded, metav1.ConditionTrue)
	healthy := gatewayWithCondition("healthy", gmcv1alpha1.ConditionRunnerGroupsDegraded, metav1.ConditionFalse)
	deleting := managedGateway("deleting", true)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(degraded, healthy, deleting).Build()
	c := newRunnerGroupsDegradedCollector(fc)

	const expected = `
# HELP actions_gateway_runnergroups_degraded 1 when the ActionsGateway RunnerGroupsDegraded condition is True (one or more owned RunnerGroups report an impairing condition), else 0.
# TYPE actions_gateway_runnergroups_degraded gauge
actions_gateway_runnergroups_degraded{name="degraded",namespace="degraded"} 1
actions_gateway_runnergroups_degraded{name="healthy",namespace="healthy"} 0
`
	// The deleting gateway must not appear as a series at all — CollectAndCompare
	// fails if the collected exposition contains anything beyond the two lines above.
	assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

// TestProxyQuotaCollector_MirrorsBothConditions asserts the collector exports
// both the ProxyQuotaPressure and ProxyQuotaExceeded gauges per gateway,
// independently mirroring each condition's True/False state.
func TestProxyQuotaCollector_MirrorsBothConditions(t *testing.T) {
	scheme := newIPRangeScheme(t)

	ag := gatewayWithCondition("gw", gmcv1alpha1.ConditionProxyQuotaPressure, metav1.ConditionTrue)
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: gmcv1alpha1.ConditionProxyQuotaExceeded, Status: metav1.ConditionFalse, Reason: "Test", Message: "test",
	})

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()
	c := newProxyQuotaCollector(fc, false)

	const expected = `
# HELP actions_gateway_proxy_quota_exceeded 1 when the ProxyQuotaExceeded condition is True (proxy replica creation is being rejected by the namespace ResourceQuota), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.
# TYPE actions_gateway_proxy_quota_exceeded gauge
actions_gateway_proxy_quota_exceeded{name="gw",namespace="gw"} 0
# HELP actions_gateway_proxy_quota_pressure 1 when the ProxyQuotaPressure condition is True (the proxy pool cannot scale to maxReplicas within the namespace ResourceQuota headroom), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.
# TYPE actions_gateway_proxy_quota_pressure gauge
actions_gateway_proxy_quota_pressure{name="gw",namespace="gw"} 1
`
	assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

// TestEgressRulesStaleCollector_MirrorsCondition asserts the collector exports
// a 1/0 gauge mirroring the EgressRulesStale condition, and excludes a
// deleting gateway.
func TestEgressRulesStaleCollector_MirrorsCondition(t *testing.T) {
	scheme := newIPRangeScheme(t)

	stale := gatewayWithCondition("stale", gmcv1alpha1.ConditionEgressRulesStale, metav1.ConditionTrue)
	fresh := gatewayWithCondition("fresh", gmcv1alpha1.ConditionEgressRulesStale, metav1.ConditionFalse)
	deleting := managedGateway("deleting", true)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, fresh, deleting).Build()
	c := newEgressRulesStaleCollector(fc, false)

	const expected = `
# HELP actions_gateway_egress_rules_stale 1 when the EgressRulesStale condition is True (the GitHub egress IP-range allowlist has not been refreshed within the staleness window), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy carrying the condition.
# TYPE actions_gateway_egress_rules_stale gauge
actions_gateway_egress_rules_stale{name="fresh",namespace="fresh"} 0
actions_gateway_egress_rules_stale{name="stale",namespace="stale"} 1
`
	assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}
