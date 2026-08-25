//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// Q319/Q643: the RunnerSet worker-capacity gauges, proven against a real apiserver with
// the reconciler writing the conditions the collector reads. The collector is
// registered into a throwaway registry (the scale-set tests' pattern) rather than
// scraped from the global controller-runtime one, so a series here belongs to this
// test's fixture and cannot be another suite's leftover.

const (
	familyQuotaPressure = "actions_gateway_runnerset_worker_quota_pressure"
	familyQuotaExceeded = "actions_gateway_runnerset_worker_quota_exceeded"
	familyUnschedulable = "actions_gateway_runnerset_workers_unschedulable"
	familyNotStarting   = "actions_gateway_runnerset_workers_not_starting"
	familyDeclined      = "actions_gateway_runnerset_worker_capacity_declined"
)

// runnerSetCapacityGauge returns the value reg currently exposes for the named gauge
// family at (ns, name), and whether that series exists at all — so a caller can tell
// an unemitted series from a legitimate 0.
func runnerSetCapacityGauge(reg *prometheus.Registry, family, ns, name string) (float64, bool) {
	fams, err := reg.Gather()
	if err != nil {
		return 0, false
	}
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotNS, gotSet string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "namespace":
					gotNS = l.GetValue()
				case "runner_set":
					gotSet = l.GetValue()
				}
			}
			if gotNS == ns && gotSet == name {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// requireCapacityGauge asserts the series exists and carries want.
func requireCapacityGauge(t *testing.T, reg *prometheus.Registry, family, ns, name string, want float64) {
	t.Helper()
	got, ok := runnerSetCapacityGauge(reg, family, ns, name)
	require.True(t, ok, "%s must be emitted for %s/%s", family, ns, name)
	require.Equal(t, want, got, "%s for %s/%s", family, ns, name)
}

// declinedGauge returns the reason label and value of the single capacity-gate series
// at (ns, name), and whether any such series exists. The family carries one series per
// gated set — the condition's current reason — so more than one match means the
// collector is emitting a reason it should have replaced, which the caller fails on.
func declinedGauge(t *testing.T, reg *prometheus.Registry, ns, name string) (string, float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)

	var reason string
	var value float64
	found := 0
	for _, f := range fams {
		if f.GetName() != familyDeclined {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotNS, gotSet, gotReason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "namespace":
					gotNS = l.GetValue()
				case "runner_set":
					gotSet = l.GetValue()
				case "reason":
					gotReason = l.GetValue()
				}
			}
			if gotNS == ns && gotSet == name {
				reason, value = gotReason, m.GetGauge().GetValue()
				found++
			}
		}
	}
	require.LessOrEqual(t, found, 1,
		"%s must carry exactly one series per gated set; a reason change replaces the series rather than adding one",
		familyDeclined)
	return reason, value, found == 1
}

// newCapacityGaugeRegistry registers a fresh collector reading through the direct
// (uncached) envtest client, so a gather reflects the apiserver rather than an
// informer that may not have caught up.
func newCapacityGaugeRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(controller.NewRunnerSetCapacityCollector(k8sClient)))
	return reg
}

// TestV2_RunnerSet_CapacityGauges_QuotaExceeded proves the error tier reaches the
// gauges: an exhausted namespace ResourceQuota reads 1 on the exceeded family under
// the set's own (namespace, runner_set) labels, while the superseded pressure tier
// reads 0 — the collector maps each condition independently rather than exporting a
// single capacity flag.
func TestV2_RunnerSet_CapacityGauges_QuotaExceeded(t *testing.T) {
	const ns = "v2-rs-cap-gauge-quota"
	const setName = "gauge-quota-set"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplateWithCPURequest("tmpl", ns, "500m")))
	rs := newRunnerSet(setName, ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3))
	require.NoError(t, k8sClient.Create(ctx, rs))

	// Headroom below a single 500m worker trips the error tier, which supersedes the
	// warning tier the 3-worker ceiling would otherwise raise.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tight", Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")}},
	}
	require.NoError(t, k8sClient.Create(ctx, quota))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), quota)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	reg := newCapacityGaugeRegistry(t)
	require.Eventually(t, func() bool {
		v, ok := runnerSetCapacityGauge(reg, familyQuotaExceeded, ns, setName)
		return ok && v == 1
	}, 20*time.Second, 200*time.Millisecond,
		"an exhausted namespace ResourceQuota must read 1 on "+familyQuotaExceeded)

	requireCapacityGauge(t, reg, familyQuotaPressure, ns, setName, 0)
	requireCapacityGauge(t, reg, familyUnschedulable, ns, setName, 0)
}

// TestV2_RunnerSet_CapacityGauges_WorkersUnschedulable proves the scheduler-verdict
// signal reaches its own gauge, and that a set with no capacity problem emits
// explicit zeros on the other two families rather than no series at all — a frozen
// or absent series is what an operator's alert would misread as healthy.
func TestV2_RunnerSet_CapacityGauges_WorkersUnschedulable(t *testing.T) {
	const ns = "v2-rs-cap-gauge-unsched"
	const setName = "gauge-unsched-set"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet(setName, ns, "gw")
	// A 12s pending deadline gives a 6s scheduling grace and a 6s window to observe
	// the gauge before the reaper deletes the pod.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 12 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	reg := newCapacityGaugeRegistry(t)
	require.Eventually(t, func() bool {
		v, ok := runnerSetCapacityGauge(reg, familyUnschedulable, ns, setName)
		return ok && v == 0
	}, 20*time.Second, 200*time.Millisecond,
		"a healthy set must emit an explicit 0 on "+familyUnschedulable+", not an absent series")
	requireCapacityGauge(t, reg, familyQuotaPressure, ns, setName, 0)
	requireCapacityGauge(t, reg, familyQuotaExceeded, ns, setName, 0)

	pod := createV2WorkerPod(t, ns, setName, "worker-gauge-unsched")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")

	require.Eventually(t, func() bool {
		v, ok := runnerSetCapacityGauge(reg, familyUnschedulable, ns, setName)
		return ok && v == 1
	}, 11*time.Second, 100*time.Millisecond,
		"a worker pod the scheduler cannot place must read 1 on "+familyUnschedulable)

	// No ResourceQuota constrains this namespace, so the quota families stay 0 while
	// the scheduler signal is True — the three gauges do not move together.
	requireCapacityGauge(t, reg, familyQuotaExceeded, ns, setName, 0)
}

// TestV2_RunnerSet_CapacityGauges_DeclinedCarriesReason is Q643: the capacity-gate
// condition's own gauge, walked through all three states an operator has to tell apart.
//
// The reason label is the whole point of the fourth family, and the latch is what
// proves it. A bare 1/0 would report the same value for a live decline and for the
// latched AwaitingProbe state, which are different situations with different remedies:
// one has stuck pods to go look at, the other has none — its evidence was reaped — and
// intake is throttled to one probe job per deadline window rather than gated on a
// present verdict. The sibling gauge is the control: WorkersUnschedulable falls back to
// 0 at the reap while this one stays 1, so an operator watching only the scheduler
// signal sees the set as recovered while its intake is still throttled.
//
// A fixed-size gateway selects the scheduler-verdict signal, so the decline follows
// from the pod alone with no autoscaler Event to stage. The 30s deadline gives a 15s
// scheduling grace and a further 15s before the reaper deletes the pod; the live
// decline is asserted inside that second window and the latch after it.
func TestV2_RunnerSet_CapacityGauges_DeclinedCarriesReason(t *testing.T) {
	const ns = "v2-rs-cap-gauge-declined"
	const setName = "gauge-gated-set"
	const ungatedName = "gauge-ungated-set"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newFixedSizeGatewayForSet("gw", ns)))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))

	rs := newRunnerSet(setName, ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3))
	rs.Spec.CapacityGate = &v2alpha1.CapacityGate{Mode: v2alpha1.CapacityGateModeObserve}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 30 * time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))

	// The negative control, and the reason the family is emitted conditionally: a set
	// that never opted in carries no condition at all, so it must produce no series —
	// a 0 here would read as "gate evaluated, capacity available" on every ungated set.
	ungated := newRunnerSet(ungatedName, ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, ungated))

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), ungated)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)
	waitForSetReadyReason(t, ns, ungatedName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	reg := newCapacityGaugeRegistry(t)

	// An open gate publishes an explicit 0 under its own reason, so a dashboard can
	// tell "evaluated and not gating" from "no gate here".
	require.Eventually(t, func() bool {
		reason, v, ok := declinedGauge(t, reg, ns, setName)
		return ok && v == 0 && reason == v2alpha1.ReasonCapacityAvailable
	}, 20*time.Second, 200*time.Millisecond,
		"an opted-in set with no stuck pod must read 0 on %s under reason=%s",
		familyDeclined, v2alpha1.ReasonCapacityAvailable)

	_, _, ok := declinedGauge(t, reg, ns, ungatedName)
	require.False(t, ok, "a set with no spec.capacityGate must emit no %s series at all", familyDeclined)
	requireCapacityGauge(t, reg, familyUnschedulable, ns, ungatedName, 0)

	// --- the live decline -----------------------------------------------------
	pod := createV2WorkerPod(t, ns, setName, "worker-gauge-declined")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")

	require.Eventually(t, func() bool {
		reason, v, ok := declinedGauge(t, reg, ns, setName)
		return ok && v == 1 && reason == v2alpha1.ReasonPodsUnschedulable
	}, 25*time.Second, 100*time.Millisecond,
		"a pod the scheduler cannot place must read 1 on %s under reason=%s",
		familyDeclined, v2alpha1.ReasonPodsUnschedulable)
	requireCapacityGauge(t, reg, familyUnschedulable, ns, setName, 1)

	// --- the latch, once the reaper deletes the gate's own evidence -----------
	require.Eventually(t, func() bool {
		reason, v, ok := declinedGauge(t, reg, ns, setName)
		return ok && v == 1 && reason == v2alpha1.ReasonAwaitingProbe
	}, 30*time.Second, 100*time.Millisecond,
		"reaping the declined worker pod must latch %s at 1 under reason=%s, not clear it",
		familyDeclined, v2alpha1.ReasonAwaitingProbe)

	// The sibling gauge has already recovered — which is exactly what the reason label
	// buys: without it, this state is indistinguishable from a live decline, and the
	// scheduler signal alone reports the set as healthy while its intake is throttled.
	requireCapacityGauge(t, reg, familyUnschedulable, ns, setName, 0)
	_, _, ok = declinedGauge(t, reg, ns, ungatedName)
	require.False(t, ok, "the ungated set must still emit no %s series", familyDeclined)
}

// TestV2_RunnerSet_CapacityGauges_WorkersNotStarting is Q906: the kubelet's startup
// verdict reaches its own gauge on a set that opted into NO capacity gate.
//
// The set here is deliberately ungated, which is the default and the case the condition
// exists for. Before Q906 the same fact reached an operator only through the
// capacity-gate family, which an ungated set does not emit at all — so this test's
// assertion on familyDeclined being ABSENT is not incidental. It is the half that says
// the observation is now published independently of the decision.
//
// The sibling family is the control: a bound pod is not a scheduling problem, so
// familyUnschedulable must stay 0 for the whole window. If it moved, the two conditions
// would be reporting the same pod and one of them would be wrong.
func TestV2_RunnerSet_CapacityGauges_WorkersNotStarting(t *testing.T) {
	const ns = "v2-rs-cap-gauge-notstarting"
	const setName = "gauge-notstarting-set"
	const image = "example.invalid/build-capable-runner:replace-me"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet(setName, ns, "gw")
	// No spec.capacityGate at all: the default, and the whole point of this test.
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 30 * time.Second}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	reg := newCapacityGaugeRegistry(t)
	require.Eventually(t, func() bool {
		v, ok := runnerSetCapacityGauge(reg, familyNotStarting, ns, setName)
		return ok && v == 0
	}, 20*time.Second, 200*time.Millisecond,
		"a healthy set must emit an explicit 0 on "+familyNotStarting+", not an absent series")

	pod := createV2WorkerPod(t, ns, setName, "worker-gauge-notstarting")
	markImagePullBackOff(t, pod, image)

	require.Eventually(t, func() bool {
		v, ok := runnerSetCapacityGauge(reg, familyNotStarting, ns, setName)
		return ok && v == 1
	}, 25*time.Second, 100*time.Millisecond,
		"a worker pod the kubelet could not start must read 1 on "+familyNotStarting+
			" even though this set has no capacity gate")

	// The scheduler placed this pod, so its signal must not move.
	requireCapacityGauge(t, reg, familyUnschedulable, ns, setName, 0)

	// And the gate's family must be absent, not 0: its presence is what says a set has
	// a capacity gate, and this one does not.
	_, _, present := declinedGauge(t, reg, ns, setName)
	require.False(t, present,
		"%s must not be emitted for an ungated set; a 0 there reads as \"gate evaluated, capacity available\"",
		familyDeclined)

	// The condition the gauge mirrors carries the kubelet's own text, which names the
	// image — the operator's whole remedy.
	var got v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: setName}, &got))
	c := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Equal(t, v2alpha1.ReasonPodsNotStarting, c.Reason)
	require.Contains(t, c.Message, image)
	require.NotContains(t, c.Message, "job intake is gated",
		"this condition reports and decides nothing; this set's intake is not gated")
}
