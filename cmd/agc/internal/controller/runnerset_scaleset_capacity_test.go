package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The scale-set tier's expression of the admission ladder (Q443). Before this, a
// ScaleSet set advertised its declared ceiling whatever the namespace quota said, so a
// quota-blocked job was assigned, claimed a JIT runner record, and then sat in
// createPodWithQuotaRetry holding the GitHub job lock. These tests pin the two halves:
// runnerSetTarget.QuotaCapacity (the observed bound, as a total) and
// scaleSetCapacityFunc (the composed integer that reaches X-ScaleSetMaxCapacity).

// workerPodObj builds one of a set's worker pods in the given phase, labelled so the
// active-pod count and the reaper both select it.
func workerPodObj(name, ns, set string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{provisioner.LabelRunnerSet: set},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestRunnerSetTarget_QuotaCapacity covers the delta-to-total conversion on live
// objects. tmplObj declares no container resources, so each worker is charged the
// provisioner's 500m gap-fill default.
func TestRunnerSetTarget_QuotaCapacity(t *testing.T) {
	const ns = "team-a"
	ctx := context.Background()

	t.Run("headroom plus the set's own in-flight pods", func(t *testing.T) {
		// 4 cpu hard, 1 used by this set's two running workers ⇒ 6 more fit, 8 total.
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns),
			quotaRS(ns, "4", "1"),
			workerPodObj("w1", ns, "set", corev1.PodRunning),
			workerPodObj("w2", ns, "set", corev1.PodPending))

		limit, bounded := target.QuotaCapacity(ctx, 20)
		require.True(t, bounded)
		assert.Equal(t, int32(8), limit)
	})

	t.Run("no headroom still admits the jobs already running", func(t *testing.T) {
		// A full quota must not retract capacity from assignments already in flight —
		// advertising below them would be a lie GitHub cannot act on anyway, and the
		// number has to converge back up as they finish.
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns),
			quotaRS(ns, "2", "2"),
			workerPodObj("w1", ns, "set", corev1.PodRunning),
			workerPodObj("w2", ns, "set", corev1.PodRunning))

		limit, bounded := target.QuotaCapacity(ctx, 20)
		require.True(t, bounded)
		assert.Equal(t, int32(2), limit)
	})

	t.Run("a full quota and no pods advertises zero", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns),
			quotaRS(ns, "2", "2"))

		limit, bounded := target.QuotaCapacity(ctx, 20)
		require.True(t, bounded)
		assert.Zero(t, limit, "with no headroom and nothing in flight, no job should be assigned at all")
	})

	t.Run("terminal pods do not count as in flight", func(t *testing.T) {
		// A Succeeded pod still sits in the quota's `used` until the reaper collects
		// it, and the headroom read already accounts for that. Counting it as active
		// too would add it back a second time and over-advertise.
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns),
			quotaRS(ns, "4", "1"),
			workerPodObj("done", ns, "set", corev1.PodSucceeded))

		limit, bounded := target.QuotaCapacity(ctx, 20)
		require.True(t, bounded)
		assert.Equal(t, int32(6), limit)
	})

	t.Run("another set's pods do not count as this set's", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns),
			quotaRS(ns, "4", "1"),
			workerPodObj("other", ns, "sibling", corev1.PodRunning))

		limit, bounded := target.QuotaCapacity(ctx, 20)
		require.True(t, bounded)
		assert.Equal(t, int32(6), limit, "a sibling set's worker is inside `used`, never inside this set's total")
	})
}

// TestRunnerSetTarget_QuotaCapacity_FailsOpen mirrors the QuotaExhausted fail-open
// suite. Every one of these must leave the set advertising its declared ceiling: a
// reference that does not resolve fails CLOSED in Resolve (§H.7) with a diagnosable
// condition, and must never show up as silent starvation at the capacity header.
func TestRunnerSetTarget_QuotaCapacity_FailsOpen(t *testing.T) {
	const ns = "team-a"
	ctx := context.Background()

	t.Run("set missing", func(t *testing.T) {
		target := quotaTarget(t, ns, gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))
		_, bounded := target.QuotaCapacity(ctx, 10)
		assert.False(t, bounded)
	})

	t.Run("gateway missing", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))
		_, bounded := target.QuotaCapacity(ctx, 10)
		assert.False(t, bounded)
	})

	t.Run("template chain unresolved", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), quotaRS(ns, "2", "2"))
		_, bounded := target.QuotaCapacity(ctx, 10)
		assert.False(t, bounded)
	})

	t.Run("no namespace quota", func(t *testing.T) {
		target := quotaTarget(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns))
		_, bounded := target.QuotaCapacity(ctx, 10)
		assert.False(t, bounded)
	})
}

// capacityReconciler wires a RunnerSetReconciler over the given objects, far enough to
// exercise scaleSetCapacityFunc end to end (target adapter, provisioner, metrics).
func capacityReconciler(t *testing.T, ns string, objs ...client.Object) (*RunnerSetReconciler, *scalesetlistener.Metrics, *runnerSetTarget) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build()
	sm := scalesetlistener.NewMetrics(prometheus.NewRegistry())
	r := &RunnerSetReconciler{
		Client:          c,
		Provisioner:     provisioner.NewProvisioner(c, nil, nil),
		ScaleSetMetrics: sm,
	}
	target := &runnerSetTarget{client: c, prov: r.Provisioner, key: client.ObjectKey{Namespace: ns, Name: "set"}, uid: "uid-1"}
	return r, sm, target
}

// TestScaleSetCapacityFunc is the behaviour this item exists for: the integer the
// listener advertises as X-ScaleSetMaxCapacity is the MINIMUM of the declared ceiling
// and live quota headroom, so a quota-blocked job is never assigned in the first place
// — no JIT runner record spent, no job lock held, nothing for
// createPodWithQuotaRetry to abandon.
func TestScaleSetCapacityFunc(t *testing.T) {
	const ns = "team-a"
	ctx := context.Background()
	key := types.NamespacedName{Namespace: ns, Name: "set"}
	max8 := func(rs *v2alpha1.RunnerSet) { rs.Spec.MaxWorkers = ptr.To(int32(8)) }

	t.Run("quota below the ceiling binds", func(t *testing.T) {
		// 2 cpu free ⇒ 4 workers of 500m, against a declared ceiling of 8.
		r, sm, target := capacityReconciler(t, ns, rsObj("set", ns, max8), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "4", "2"))

		assert.Equal(t, 4, r.scaleSetCapacityFunc(key, target)(ctx))
		assert.Equal(t, float64(4), testutil.ToFloat64(sm.AdvertisedCapacity.WithLabelValues(ns, "set")))
		assert.Equal(t, float64(4), testutil.ToFloat64(sm.CapacityWithheld.WithLabelValues(ns, "set", runnercore.AdmitReasonQuota)))
	})

	t.Run("ample quota leaves the ceiling in charge", func(t *testing.T) {
		r, sm, target := capacityReconciler(t, ns, rsObj("set", ns, max8), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "100", "0"))

		assert.Equal(t, 8, r.scaleSetCapacityFunc(key, target)(ctx))
		assert.Equal(t, float64(0), testutil.ToFloat64(sm.CapacityWithheld.WithLabelValues(ns, "set", runnercore.AdmitReasonQuota)),
			"a rung that did not bind must publish an explicit zero, not go stale")
	})

	t.Run("no quota and no ceiling falls back to the default", func(t *testing.T) {
		r, _, target := capacityReconciler(t, ns, rsObj("set", ns, nil), gwObj("gw", ns, ""), tmplObj("tmpl", ns))

		assert.Equal(t, defaultScaleSetMaxCapacity, r.scaleSetCapacityFunc(key, target)(ctx))
	})

	t.Run("an exhausted quota advertises zero", func(t *testing.T) {
		r, _, target := capacityReconciler(t, ns, rsObj("set", ns, max8), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))

		assert.Zero(t, r.scaleSetCapacityFunc(key, target)(ctx), "GitHub must assign nothing while no worker pod can be created")
	})

	t.Run("the quota opt-out restores ceiling-only behaviour", func(t *testing.T) {
		r, _, target := capacityReconciler(t, ns, rsObj("set", ns, max8), gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "2", "2"))
		r.Provisioner.DisableQuotaAdmission = true

		assert.Equal(t, 8, r.scaleSetCapacityFunc(key, target)(ctx), "AGC_QUOTA_ADMISSION=false must opt out on both tiers")
	})
}

// TestScaleSetCapacityFunc_RecoversWhenHeadroomReturns pins the self-clearing property.
// The capacity function is built once at listener start, so a quota that frees up has
// to reopen assignment on the very next poll with no restart and no reset — and unlike
// the classic rung, nothing had to be claimed and abandoned to discover it.
func TestScaleSetCapacityFunc_RecoversWhenHeadroomReturns(t *testing.T) {
	const ns = "team-a"
	ctx := context.Background()
	key := types.NamespacedName{Namespace: ns, Name: "set"}

	r, _, target := capacityReconciler(t, ns,
		rsObj("set", ns, func(rs *v2alpha1.RunnerSet) { rs.Spec.MaxWorkers = ptr.To(int32(8)) }),
		gwObj("gw", ns, ""), tmplObj("tmpl", ns), quotaRS(ns, "4", "4"))
	capacity := r.scaleSetCapacityFunc(key, target)

	require.Zero(t, capacity(ctx))

	freed := quotaRS(ns, "4", "2")
	require.NoError(t, r.Client.Update(ctx, freed))

	assert.Equal(t, 4, capacity(ctx), "freed headroom must reopen assignment on the next poll")
}

// TestScaleSetMetrics_DeleteRunnerSet verifies a deleted set stops reporting. A gauge
// nobody writes any more is indistinguishable from one frozen at its last reading, so
// cleanupLocalState drops the series rather than leaving a phantom set advertising
// capacity forever.
func TestScaleSetMetrics_DeleteRunnerSet(t *testing.T) {
	sm := scalesetlistener.NewMetrics(prometheus.NewRegistry())
	sm.SetAdvertisedCapacity("ns", "set", 4)
	sm.SetCapacityWithheld("ns", "set", runnercore.AdmitReasonQuota, 4)
	require.Equal(t, 1, testutil.CollectAndCount(sm.AdvertisedCapacity))

	sm.RecorderFor("ns", "set").SetAvailableJobs(3)
	require.Equal(t, 1, testutil.CollectAndCount(sm.AvailableJobs))

	sm.DeleteRunnerSet("ns", "set")

	assert.Zero(t, testutil.CollectAndCount(sm.AdvertisedCapacity))
	assert.Zero(t, testutil.CollectAndCount(sm.CapacityWithheld))
	assert.Zero(t, testutil.CollectAndCount(sm.AvailableJobs), "the demand gauge outlived the set it describes")
}
