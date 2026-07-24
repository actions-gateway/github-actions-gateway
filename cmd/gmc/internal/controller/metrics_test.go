package controller

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// newV2MetricsScheme returns the v1 IPRange scheme with the v2alpha1 kinds also
// registered, so the fake client can serve the v2 ActionsGateway/EgressProxy lists
// the v2-aware collectors read (Q320).
func newV2MetricsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newIPRangeScheme(t)
	require.NoError(t, gmcv2alpha1.AddToScheme(s))
	return s
}

// v2ManagedGateway builds a v2alpha1 ActionsGateway for the managed-gateways
// collector's v2 count.
func v2ManagedGateway(name string, deleting bool) *gmcv2alpha1.ActionsGateway {
	ag := &gmcv2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name}}
	if deleting {
		now := metav1.Now()
		ag.DeletionTimestamp = &now
		// A deletion timestamp only persists in the fake client with a finalizer.
		ag.Finalizers = []string{"actions-gateway/test"}
	}
	return ag
}

// v2EgressProxyWithCondition builds a v2alpha1 EgressProxy carrying a single status
// condition, for driving the v2 arm of the proxy-quota and egress-stale collectors.
func v2EgressProxyWithCondition(name, condType string, status metav1.ConditionStatus) *gmcv2alpha1.EgressProxy {
	ep := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name}}
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: "Test", Message: "test",
	})
	return ep
}

// v2GatewayWithCondition builds a v2alpha1 ActionsGateway carrying a single status
// condition, for driving the v2 ActionsGateway condition collector (Q321).
func v2GatewayWithCondition(name, condType string, status metav1.ConditionStatus) *gmcv2alpha1.ActionsGateway {
	ag := v2ManagedGateway(name, false)
	meta.SetStatusCondition(&ag.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: "Test", Message: "test",
	})
	return ag
}

// testIPRangeUpdates returns an unregistered Metrics with just the counter, so
// tests do not touch the global controller-runtime registry.
func testIPRangeUpdates() *Metrics {
	return &Metrics{
		IPRangeUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_ip_range_updates_total",
		}, []string{"namespace"}),
	}
}

// A successful NetworkPolicy patch must increment the per-namespace counter.
func TestIPRangeReconciler_IncrementsUpdateCounter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scheme := newIPRangeScheme(t)
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	np := buildProxyNetworkPolicy(ag, nil)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, np).Build()

	m := testIPRangeUpdates()
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{cidrs: []net.IPNet{parseCIDR(t, "140.82.112.0/20")}},
		Interval: time.Hour,
		Metrics:  m,
	}

	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")),
		"one successful patch should record one update")

	// A second refresh patches again.
	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")))
}

// A missing NetworkPolicy is a no-op and must not record an update.
func TestIPRangeReconciler_NoUpdateWhenNetworkPolicyMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scheme := newIPRangeScheme(t)
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	// No NetworkPolicy seeded: patchNetworkPolicy hits NotFound and returns nil.
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()

	m := testIPRangeUpdates()
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{cidrs: []net.IPNet{parseCIDR(t, "140.82.112.0/20")}},
		Interval: time.Hour,
		Metrics:  m,
	}

	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")),
		"a missing NetworkPolicy must not record an update")
}

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

// TestActionsGatewayV2ConditionsCollector_MirrorsConditions asserts the v2
// ActionsGateway condition collector (Q321) exports the runnersets_degraded,
// agc_available, egress_unattributed, and agc_autoscaling_unavailable (Q390)
// gauges per non-deleting v2 gateway, each mirroring its condition's True/False
// (set/clear) state, and skips a deleting gateway entirely.
func TestActionsGatewayV2ConditionsCollector_MirrorsConditions(t *testing.T) {
	scheme := newV2MetricsScheme(t)

	// "degraded": RunnerSetsDegraded=True, AGCAvailable=False, EgressUnattributed=True,
	// AGCAutoscalingUnavailable=True.
	degraded := v2GatewayWithCondition("degraded", gmcv2alpha1.ConditionRunnerSetsDegraded, metav1.ConditionTrue)
	meta.SetStatusCondition(&degraded.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionAGCAvailable, Status: metav1.ConditionFalse, Reason: "Test", Message: "test",
	})
	meta.SetStatusCondition(&degraded.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionEgressUnattributed, Status: metav1.ConditionTrue, Reason: "Test", Message: "test",
	})
	meta.SetStatusCondition(&degraded.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionAGCAutoscalingUnavailable, Status: metav1.ConditionTrue, Reason: "Test", Message: "test",
	})

	// "healthy": RunnerSetsDegraded=False, AGCAvailable=True, EgressUnattributed=False,
	// AGCAutoscalingUnavailable=False.
	healthy := v2GatewayWithCondition("healthy", gmcv2alpha1.ConditionRunnerSetsDegraded, metav1.ConditionFalse)
	meta.SetStatusCondition(&healthy.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionAGCAvailable, Status: metav1.ConditionTrue, Reason: "Test", Message: "test",
	})
	meta.SetStatusCondition(&healthy.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionEgressUnattributed, Status: metav1.ConditionFalse, Reason: "Test", Message: "test",
	})
	meta.SetStatusCondition(&healthy.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionAGCAutoscalingUnavailable, Status: metav1.ConditionFalse, Reason: "Test", Message: "test",
	})

	deleting := v2ManagedGateway("deleting", true)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(degraded, healthy, deleting).Build()
	c := newActionsGatewayV2ConditionsCollector(fc)

	const expected = `
# HELP actions_gateway_agc_autoscaling_unavailable 1 when the v2 ActionsGateway AGCAutoscalingUnavailable condition is True (the agcAutoscaling opt-in cannot be satisfied, e.g. the VerticalPodAutoscaler CRDs are not installed), else 0. The AGC still runs on its stamped agcResources sizing; this is advisory.
# TYPE actions_gateway_agc_autoscaling_unavailable gauge
actions_gateway_agc_autoscaling_unavailable{name="degraded",namespace="degraded"} 1
actions_gateway_agc_autoscaling_unavailable{name="healthy",namespace="healthy"} 0
# HELP actions_gateway_agc_available 1 when the v2 ActionsGateway AGCAvailable condition is True (the tenant's AGC Deployment has a ready replica), else 0.
# TYPE actions_gateway_agc_available gauge
actions_gateway_agc_available{name="degraded",namespace="degraded"} 0
actions_gateway_agc_available{name="healthy",namespace="healthy"} 1
# HELP actions_gateway_egress_unattributed 1 when the v2 ActionsGateway EgressUnattributed condition is True (the gateway runs in direct egress mode, so its GitHub traffic is not attributed to a per-tenant egress proxy), else 0.
# TYPE actions_gateway_egress_unattributed gauge
actions_gateway_egress_unattributed{name="degraded",namespace="degraded"} 1
actions_gateway_egress_unattributed{name="healthy",namespace="healthy"} 0
# HELP actions_gateway_runnersets_degraded 1 when the v2 ActionsGateway RunnerSetsDegraded condition is True (one or more RunnerSets bound to the gateway report an impairing condition), else 0. The v2 twin of actions_gateway_runnergroups_degraded.
# TYPE actions_gateway_runnersets_degraded gauge
actions_gateway_runnersets_degraded{name="degraded",namespace="degraded"} 1
actions_gateway_runnersets_degraded{name="healthy",namespace="healthy"} 0
`
	// The deleting gateway must not appear as any series — CollectAndCompare fails
	// if the collected exposition contains anything beyond the lines above.
	assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

// TestActionsGatewayV2ConditionsCollector_NoGateways asserts the collector emits
// nothing when there are no v2 ActionsGateways — no phantom zero-value series.
func TestActionsGatewayV2ConditionsCollector_NoGateways(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := newActionsGatewayV2ConditionsCollector(fc)
	assert.Equal(t, 0, testutil.CollectAndCount(c))
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

// TestProxyQuotaCollector_NoGateways asserts the collector emits nothing when
// there are no ActionsGateway CRs — no phantom zero-value series.
func TestProxyQuotaCollector_NoGateways(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := newProxyQuotaCollector(fc, false)
	assert.Equal(t, 0, testutil.CollectAndCount(c))
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

// TestNewMetrics_RegistersCounterAndCollectors asserts NewMetrics returns a
// Metrics with a usable, correctly-labelled IPRangeUpdates counter and
// registers it (plus the scrape-time collectors) with the controller-runtime
// metrics registry without panicking. This is the only test in the package
// that calls NewMetrics — MustRegister panics on a second registration of the
// same fixed metric names, so a second caller would collide on the
// process-global registry.
func TestNewMetrics_RegistersCounterAndCollectors(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	m := NewMetrics(fc, false)
	require.NotNil(t, m)
	require.NotNil(t, m.IPRangeUpdates)

	// The counter is a real, usable CounterVec: incrementing it and reading it
	// back exercises the metric NewMetrics constructed (not just a nil field).
	m.IPRangeUpdates.WithLabelValues("team-a").Inc()
	assert.Equal(t, 1.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")))

	// Gathering the registry must succeed and include the counter this call
	// registered, confirming the MustRegister call inside NewMetrics landed.
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	var found bool
	for _, f := range families {
		if f.GetName() == "actions_gateway_ip_range_updates_total" {
			found = true
		}
	}
	assert.True(t, found, "NewMetrics must register actions_gateway_ip_range_updates_total with the controller-runtime registry")
}

// TestManagedGatewaysCollector_CountsV1AndV2 asserts the managed-gateways gauge sums
// non-deleting v1 and v2 ActionsGateways when v2 is enabled, and counts v1 only when
// v2 is disabled (a v1-only cluster never lists the absent v2 kind).
func TestManagedGatewaysCollector_CountsV1AndV2(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		managedGateway("v1a", false),
		managedGateway("v1b", false),
		managedGateway("v1del", true),
		v2ManagedGateway("v2a", false),
		v2ManagedGateway("v2del", true),
	).Build()

	t.Run("v2 enabled sums both versions", func(t *testing.T) {
		c := newManagedGatewaysCollector(fc, true)
		assert.Equal(t, 3.0, testutil.ToFloat64(c),
			"2 active v1 + 1 active v2; the deleting ones are excluded")
	})
	t.Run("v2 disabled counts v1 only", func(t *testing.T) {
		c := newManagedGatewaysCollector(fc, false)
		assert.Equal(t, 2.0, testutil.ToFloat64(c),
			"v2 gateways are not counted when v2 is disabled")
	})
}

// TestProxyQuotaCollector_ReflectsV2EgressProxy asserts the proxy-quota collector
// exports both gauges per v2 EgressProxy, mirroring each condition, when v2 is
// enabled — and emits nothing for v2 when it is disabled.
func TestProxyQuotaCollector_ReflectsV2EgressProxy(t *testing.T) {
	scheme := newV2MetricsScheme(t)

	ep := v2EgressProxyWithCondition("proxy", gmcv2alpha1.ConditionProxyQuotaExceeded, metav1.ConditionTrue)
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type: gmcv2alpha1.ConditionProxyQuotaPressure, Status: metav1.ConditionFalse, Reason: "Test", Message: "test",
	})
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()

	t.Run("v2 enabled reflects the EgressProxy conditions", func(t *testing.T) {
		c := newProxyQuotaCollector(fc, true)
		const expected = `
# HELP actions_gateway_proxy_quota_exceeded 1 when the ProxyQuotaExceeded condition is True (proxy replica creation is being rejected by the namespace ResourceQuota), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.
# TYPE actions_gateway_proxy_quota_exceeded gauge
actions_gateway_proxy_quota_exceeded{name="proxy",namespace="proxy"} 1
# HELP actions_gateway_proxy_quota_pressure 1 when the ProxyQuotaPressure condition is True (the proxy pool cannot scale to maxReplicas within the namespace ResourceQuota headroom), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy owning the pool.
# TYPE actions_gateway_proxy_quota_pressure gauge
actions_gateway_proxy_quota_pressure{name="proxy",namespace="proxy"} 0
`
		assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
	})
	t.Run("v2 disabled ignores the EgressProxy", func(t *testing.T) {
		c := newProxyQuotaCollector(fc, false)
		assert.Equal(t, 0, testutil.CollectAndCount(c),
			"an EgressProxy contributes no series when v2 is disabled")
	})
}

// TestEgressRulesStaleCollector_ReflectsV2EgressProxy asserts the egress-stale
// collector exports a 1/0 gauge per v2 EgressProxy alongside the v1 ActionsGateway
// series, sharing one metric family across both versions.
func TestEgressRulesStaleCollector_ReflectsV2EgressProxy(t *testing.T) {
	scheme := newV2MetricsScheme(t)

	stale := v2EgressProxyWithCondition("v2stale", gmcv2alpha1.ConditionEgressRulesStale, metav1.ConditionTrue)
	fresh := v2EgressProxyWithCondition("v2fresh", gmcv2alpha1.ConditionEgressRulesStale, metav1.ConditionFalse)
	v1 := gatewayWithCondition("v1stale", gmcv1alpha1.ConditionEgressRulesStale, metav1.ConditionTrue)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, fresh, v1).Build()
	c := newEgressRulesStaleCollector(fc, true)

	const expected = `
# HELP actions_gateway_egress_rules_stale 1 when the EgressRulesStale condition is True (the GitHub egress IP-range allowlist has not been refreshed within the staleness window), else 0. The name label is the v1 ActionsGateway or the v2 EgressProxy carrying the condition.
# TYPE actions_gateway_egress_rules_stale gauge
actions_gateway_egress_rules_stale{name="v1stale",namespace="v1stale"} 1
actions_gateway_egress_rules_stale{name="v2fresh",namespace="v2fresh"} 0
actions_gateway_egress_rules_stale{name="v2stale",namespace="v2stale"} 1
`
	assert.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}
