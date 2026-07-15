package controller

import (
	"context"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func egressProxy(name string) *gmcv2alpha1.EgressProxy {
	return &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name},
		Spec: gmcv2alpha1.EgressProxySpec{
			MinReplicas: ptrInt32(1),
			MaxReplicas: ptrInt32(5),
		},
	}
}

// evalEgressProxyQuota must report ProxyQuotaExceeded from the proxy Deployment's
// ReplicaFailure "exceeded quota" condition and suppress the pressure warning while
// exceeded (mutually exclusive).
func TestEvalEgressProxyQuota_ExceededFromReplicaFailure(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &EgressProxyReconciler{Client: fc}

	ep := egressProxy("team-a")
	dep := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Replicas: 2,
			Conditions: []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentReplicaFailure,
				Status:  corev1.ConditionTrue,
				Message: `pods "team-a-proxy-x" is forbidden: exceeded quota: team-a`,
			}},
		},
	}

	qc := r.evalEgressProxyQuota(context.Background(), ep, dep)
	assert.True(t, qc.exceeded, "ReplicaFailure with 'exceeded quota' trips ProxyQuotaExceeded")
	assert.False(t, qc.pressure, "pressure is superseded while exceeded")
	assert.Equal(t, "Superseded", qc.pressureReason)
}

// evalEgressProxyQuota's warning tier must trip when the namespace ResourceQuota
// lacks headroom to grow the pool to maxReplicas.
func TestEvalEgressProxyQuota_PressureFromHeadroom(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: "team-a"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("3")},
			Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2")},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	r := &EgressProxyReconciler{Client: fc}

	ep := egressProxy("team-a") // maxReplicas 5
	dep := &appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: 1}}

	// Growing from 1 to 5 needs 4 more pods, but only 1 pod of headroom remains.
	qc := r.evalEgressProxyQuota(context.Background(), ep, dep)
	assert.True(t, qc.pressure, "insufficient pod headroom trips ProxyQuotaPressure")
	assert.Equal(t, "InsufficientQuotaHeadroom", qc.pressureReason)
	assert.False(t, qc.exceeded)
}

// With no ResourceQuota in the namespace, neither quota condition trips.
func TestEvalEgressProxyQuota_NoQuota(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &EgressProxyReconciler{Client: fc}

	qc := r.evalEgressProxyQuota(context.Background(), egressProxy("team-a"),
		&appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: 1}})
	assert.False(t, qc.pressure)
	assert.False(t, qc.exceeded)
	assert.Equal(t, "NoQuota", qc.pressureReason)
}

// evalEgressProxyEgressRulesStale trips only when the shared IP cache's last refresh
// is older than the threshold, and stays "pending" (not an alarm) when the egress
// mode is not CIDR-refreshed or no refresh has happened yet.
func TestEvalEgressProxyEgressRulesStale(t *testing.T) {
	now := time.Now()

	t.Run("stale when last refresh exceeds threshold", func(t *testing.T) {
		cache := &IPRangeCache{}
		cache.MarkRefreshed(now.Add(-50 * time.Hour))
		r := &EgressProxyReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}
		es := r.evalEgressProxyEgressRulesStale(egressProxy("team-a"), now)
		assert.True(t, es.stale)
		assert.Equal(t, gmcv2alpha1.ReasonRefreshStalled, es.reason)
	})

	t.Run("fresh when within threshold", func(t *testing.T) {
		cache := &IPRangeCache{}
		cache.MarkRefreshed(now.Add(-1 * time.Hour))
		r := &EgressProxyReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}
		es := r.evalEgressProxyEgressRulesStale(egressProxy("team-a"), now)
		assert.False(t, es.stale)
		assert.Equal(t, gmcv2alpha1.ReasonRefreshCurrent, es.reason)
	})

	t.Run("pending before first refresh", func(t *testing.T) {
		r := &EgressProxyReconciler{IPCache: &IPRangeCache{}, EgressStaleThreshold: 49 * time.Hour}
		es := r.evalEgressProxyEgressRulesStale(egressProxy("team-a"), now)
		assert.False(t, es.stale)
		assert.Equal(t, gmcv2alpha1.ReasonRefreshPending, es.reason)
	})

	t.Run("pending for FQDN egress mode", func(t *testing.T) {
		cache := &IPRangeCache{}
		cache.MarkRefreshed(now.Add(-50 * time.Hour))
		r := &EgressProxyReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}
		ep := egressProxy("team-a")
		ep.Spec.EgressPolicyMode = gmcv2alpha1.EgressPolicyModeFQDN
		es := r.evalEgressProxyEgressRulesStale(ep, now)
		assert.False(t, es.stale, "FQDN mode carries no refreshed CIDR rule, so it cannot go stale")
		assert.Equal(t, gmcv2alpha1.ReasonRefreshPending, es.reason)
	})

	t.Run("pending for unmanaged NetworkPolicy", func(t *testing.T) {
		cache := &IPRangeCache{}
		cache.MarkRefreshed(now.Add(-50 * time.Hour))
		r := &EgressProxyReconciler{IPCache: cache, EgressStaleThreshold: 49 * time.Hour}
		ep := egressProxy("team-a")
		unmanaged := false
		ep.Spec.ManagedNetworkPolicy = &unmanaged
		es := r.evalEgressProxyEgressRulesStale(ep, now)
		assert.False(t, es.stale)
	})
}

// quotaToEgressProxies fans a ResourceQuota event out to every EgressProxy in the
// quota's namespace and none elsewhere (Q326).
func TestQuotaToEgressProxies(t *testing.T) {
	scheme := newV2MetricsScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		egressProxy("team-a"),
		&gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "team-a"}},
		&gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"}},
	).Build()
	r := &EgressProxyReconciler{Client: fc}

	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: "team-a"}}
	reqs := r.quotaToEgressProxies(context.Background(), quota)
	require.Len(t, reqs, 2, "both proxies in the quota's namespace map; the foreign-namespace one does not")
	names := []string{reqs[0].Name, reqs[1].Name}
	assert.ElementsMatch(t, []string{"team-a", "second"}, names)
}

// egressStaleThreshold falls back to the shared default when unset.
func TestEgressProxyEgressStaleThreshold_Default(t *testing.T) {
	r := &EgressProxyReconciler{}
	require.Equal(t, DefaultEgressStaleThreshold, r.egressStaleThreshold())
	r.EgressStaleThreshold = time.Hour
	require.Equal(t, time.Hour, r.egressStaleThreshold())
}

// egressProxyEgressRecheckRequeue returns a bounded cadence whenever the GMC manages
// the egress policy (CIDR staleness recheck, or FQDN CNI-policy drift recheck — Q326)
// and 0 when there is nothing to re-check.
func TestEgressProxyEgressRecheckRequeue(t *testing.T) {
	cidr := &EgressProxyReconciler{IPCache: &IPRangeCache{}, EgressStaleThreshold: 48 * time.Hour}
	assert.Equal(t, 6*time.Hour, cidr.egressProxyEgressRecheckRequeue(egressProxy("team-a")),
		"threshold/8 bounds the recheck cadence")

	noCache := &EgressProxyReconciler{EgressStaleThreshold: 48 * time.Hour}
	assert.Equal(t, time.Duration(0), noCache.egressProxyEgressRecheckRequeue(egressProxy("team-a")),
		"CIDR staleness is not evaluated without the shared IP cache")

	fqdn := egressProxy("team-a")
	fqdn.Spec.EgressPolicyMode = gmcv2alpha1.EgressPolicyModeFQDN
	assert.Equal(t, 6*time.Hour, noCache.egressProxyEgressRecheckRequeue(fqdn),
		"an FQDN-mode proxy re-checks CNI-policy drift on the same cadence, IP cache or not (Q326)")

	unmanaged := egressProxy("team-a")
	unmanaged.Spec.EgressPolicyMode = gmcv2alpha1.EgressPolicyModeFQDN
	f := false
	unmanaged.Spec.ManagedNetworkPolicy = &f
	assert.Equal(t, time.Duration(0), cidr.egressProxyEgressRecheckRequeue(unmanaged),
		"an unmanaged egress policy is operator-maintained; nothing of ours to re-check")
}
