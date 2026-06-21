package controller

import (
	"testing"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func egressBoolPtr(b bool) *bool { return &b }

// agWithManagedNP returns a minimal ActionsGateway with the proxy
// ManagedNetworkPolicy set (nil = managed default).
func agWithManagedNP(managed *bool) *gmcv1alpha1.ActionsGateway {
	ag := applyTestAG()
	ag.Spec.Proxy.ManagedNetworkPolicy = managed
	return ag
}

// TestEvalEgressRulesStale_Fresh: a recent refresh is not stale.
func TestEvalEgressRulesStale_Fresh(t *testing.T) {
	now := time.Now()
	cache := &IPRangeCache{}
	cache.MarkRefreshed(now.Add(-2 * time.Hour))
	r := &ActionsGatewayReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}

	es := r.evalEgressRulesStale(agWithManagedNP(nil), now)
	assert.False(t, es.stale)
	assert.Equal(t, gmcv1alpha1.ReasonRefreshCurrent, es.reason)
}

// TestEvalEgressRulesStale_Stalled: a refresh older than the window trips it.
func TestEvalEgressRulesStale_Stalled(t *testing.T) {
	now := time.Now()
	cache := &IPRangeCache{}
	cache.MarkRefreshed(now.Add(-72 * time.Hour))
	r := &ActionsGatewayReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}

	es := r.evalEgressRulesStale(agWithManagedNP(nil), now)
	assert.True(t, es.stale)
	assert.Equal(t, gmcv1alpha1.ReasonRefreshStalled, es.reason)
	assert.Contains(t, es.message, "silently dropped")
}

// TestEvalEgressRulesStale_NeverRefreshed: before the first refresh, staleness
// cannot be asserted (startup) — not an alarm.
func TestEvalEgressRulesStale_NeverRefreshed(t *testing.T) {
	r := &ActionsGatewayReconciler{IPCache: &IPRangeCache{}, EgressStaleThreshold: 49 * time.Hour}
	es := r.evalEgressRulesStale(agWithManagedNP(nil), time.Now())
	assert.False(t, es.stale)
	assert.Equal(t, gmcv1alpha1.ReasonRefreshPending, es.reason)
}

// TestEvalEgressRulesStale_NilCache: no cache wired → pending, never stale.
func TestEvalEgressRulesStale_NilCache(t *testing.T) {
	r := &ActionsGatewayReconciler{}
	es := r.evalEgressRulesStale(agWithManagedNP(nil), time.Now())
	assert.False(t, es.stale)
	assert.Equal(t, gmcv1alpha1.ReasonRefreshPending, es.reason)
}

// TestEvalEgressRulesStale_UnmanagedNP: an unmanaged proxy NetworkPolicy is never
// reported stale even when the (irrelevant) refresh is old — its egress rules are
// operator-maintained, not driven by the refresh loop.
func TestEvalEgressRulesStale_UnmanagedNP(t *testing.T) {
	now := time.Now()
	cache := &IPRangeCache{}
	cache.MarkRefreshed(now.Add(-100 * time.Hour))
	r := &ActionsGatewayReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}

	es := r.evalEgressRulesStale(agWithManagedNP(egressBoolPtr(false)), now)
	assert.False(t, es.stale)
	assert.Equal(t, gmcv1alpha1.ReasonRefreshPending, es.reason)
}

// TestEgressStaleThreshold_Default: unset selects the package default.
func TestEgressStaleThreshold_Default(t *testing.T) {
	r := &ActionsGatewayReconciler{}
	assert.Equal(t, DefaultEgressStaleThreshold, r.egressStaleThreshold())
	r.EgressStaleThreshold = 10 * time.Hour
	assert.Equal(t, 10*time.Hour, r.egressStaleThreshold())
}

// TestEgressRecheckRequeue: managed+cache yields a bounded cadence; unmanaged or
// no cache yields none.
func TestEgressRecheckRequeue(t *testing.T) {
	cache := &IPRangeCache{}
	r := &ActionsGatewayReconciler{IPCache: cache, EgressStaleThreshold: 48 * time.Hour}
	assert.Equal(t, 6*time.Hour, r.egressRecheckRequeue(agWithManagedNP(nil)))
	assert.Zero(t, r.egressRecheckRequeue(agWithManagedNP(egressBoolPtr(false))))
	assert.Zero(t, (&ActionsGatewayReconciler{}).egressRecheckRequeue(agWithManagedNP(nil)))
}

// TestIPRangeCache_LastRefresh: the cache reports zero/false before the first
// refresh and the stamped time after.
func TestIPRangeCache_LastRefresh(t *testing.T) {
	cache := &IPRangeCache{}
	if _, ok := cache.LastRefresh(); ok {
		t.Fatal("a fresh cache must report no refresh yet")
	}
	now := time.Now()
	cache.MarkRefreshed(now)
	got, ok := cache.LastRefresh()
	assert.True(t, ok)
	assert.Equal(t, now, got)
}
