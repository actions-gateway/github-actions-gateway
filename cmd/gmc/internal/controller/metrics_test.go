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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

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
		c := newManagedGatewaysCollector(fc)
		assert.Equal(t, 0.0, testutil.ToFloat64(c))
	})

	t.Run("counts active, excludes deleting", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(
				managedGateway("a", false),
				managedGateway("b", false),
				managedGateway("c", true),
			).Build()
		c := newManagedGatewaysCollector(fc)
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
	c := newProxyQuotaCollector(fc)

	const expected = `
# HELP actions_gateway_proxy_quota_exceeded 1 when the ActionsGateway ProxyQuotaExceeded condition is True (proxy replica creation is being rejected by the namespace ResourceQuota), else 0.
# TYPE actions_gateway_proxy_quota_exceeded gauge
actions_gateway_proxy_quota_exceeded{name="gw",namespace="gw"} 0
# HELP actions_gateway_proxy_quota_pressure 1 when the ActionsGateway ProxyQuotaPressure condition is True (the proxy pool cannot scale to maxReplicas within the namespace ResourceQuota headroom), else 0.
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
	c := newProxyQuotaCollector(fc)
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
	c := newEgressRulesStaleCollector(fc)

	const expected = `
# HELP actions_gateway_egress_rules_stale 1 when the ActionsGateway EgressRulesStale condition is True (the GitHub egress IP-range allowlist has not been refreshed within the staleness window), else 0.
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

	m := NewMetrics(fc)
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
