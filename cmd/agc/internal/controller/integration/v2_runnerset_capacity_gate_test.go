//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q405 phase 1 — the capacity gate's SchedulerVerdict mode, proven against a real
// apiserver on BOTH acquisition tiers.
//
// The unit tests cover the rung's arithmetic and every fail-open path. What only
// envtest can show is the loop closing: a real Pod carrying a real
// PodScheduled=False/Unschedulable status, aged past the scheduling grace by the
// reconciler's own RequeueAfter, publishing a condition that the admission rung then
// reads back on a live listener's next delivery. A unit test can assert each half; it
// cannot assert that the two halves agree about the same object.
//
// Both tiers are covered because Q443 established that a rung expressed in only one
// form ships to only one tier. The tiers observe the refusal differently by
// construction: classic declines a delivered job and counts it under
// jobs_admission_rejected_total{reason="capacity"}, while the scale-set tier withholds
// slots from its advertisement so the job is never assigned and there is no delivery to
// count — hence capacity_withheld{reason="capacity"} is its counterpart.

// waitForCapacityDeclined blocks until the named RunnerSet's WorkerCapacityDeclined
// condition is True, which is both the operator's signal and the value the admission
// rung reads.
func waitForCapacityDeclined(t *testing.T, ns, name string, within time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		c := setCondition(t, ns, name, v2alpha1.ConditionWorkerCapacityDeclined)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2alpha1.ReasonPodsUnschedulable
	}, within, 100*time.Millisecond,
		"an unschedulable worker pod must close the capacity gate on RunnerSet %s/%s", ns, name)
}

// TestV2_RunnerSet_CapacityGate_SkipsAcquireOnTheSchedulerVerdict is the classic tier:
// with the gate closed, a delivered job must be left queued at GitHub rather than
// claimed.
//
// The acquire assertion is made server-side on the broker stub's call counter, not by
// the absence of a worker pod: "no pod appeared" would also pass if the AGC had claimed
// the job and then failed to place it, which is exactly the claim-and-stall this rung
// exists to prevent.
//
// maxWorkers is deliberately 3 against a single in-flight pod, so the ceiling rung
// cannot be what refused. That isolation is what makes the reason label meaningful:
// a rung misordered behind the ceiling would report reason="ceiling" here.
func TestV2_RunnerSet_CapacityGate_SkipsAcquireOnTheSchedulerVerdict(t *testing.T) {
	const ns = "v2-rs-capgate-classic"
	const setName = "capgate-set"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet(setName, ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3))
	rs.Spec.CapacityGate = &v2alpha1.CapacityGate{Mode: v2alpha1.CapacityGateModeSchedulerVerdict}
	// A 30s deadline gives a 15s scheduling grace and a further 15s before the reaper
	// deletes the pod — enough window to observe the closed gate refuse a delivery.
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 30 * time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	acquiresBefore := brokerStub.AcquireJobCalls()
	capacityBefore := admissionRejections(ns, setName, runnercore.AdmitReasonCapacity)
	ceilingBefore := admissionRejections(ns, setName, runnercore.AdmitReasonCeiling)
	quotaBefore := admissionRejections(ns, setName, runnercore.AdmitReasonQuota)

	// The scheduler cannot place this set's worker shape.
	pod := createV2WorkerPod(t, ns, setName, "worker-unplaceable")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")
	waitForCapacityDeclined(t, ns, setName, 25*time.Second)

	id := enqueueJobOnOwnerSession(15*time.Second, setName, nil, broker.RunnerJobRequestBody{})
	require.NotEmpty(t, id, "a session for %s should be active", setName)

	require.Eventually(t, func() bool {
		return admissionRejections(ns, setName, runnercore.AdmitReasonCapacity) > capacityBefore
	}, 20*time.Second, 25*time.Millisecond,
		"a delivery under a closed capacity gate must be rejected with reason=capacity")

	// The job was never claimed. The counter is monotonic and only this test enqueues
	// while it runs, so a flat count is a real skip rather than a slow claim.
	require.Never(t, func() bool {
		return brokerStub.AcquireJobCalls() > acquiresBefore
	}, 2*time.Second, 100*time.Millisecond,
		"a job the cluster cannot place must be left queued at GitHub, never claimed by acquirejob")

	// Attribution: neither sibling rung may absorb a capacity refusal. The ceiling
	// series is the load-bearing one — a capacity rung ordered after the ceiling, or one
	// that reserved a slot, would show up here.
	require.Equal(t, ceilingBefore, admissionRejections(ns, setName, runnercore.AdmitReasonCeiling),
		"the capacity rung reserves nothing and must never be attributed to the ceiling")
	require.Equal(t, quotaBefore, admissionRejections(ns, setName, runnercore.AdmitReasonQuota),
		"an unplaceable pod is not a quota rejection; the two rungs answer different questions")
}

// TestV2_RunnerSet_ScaleSet_CapacityGateWithholdsAdvertisedCapacity is the same rung's
// integer form on the default acquisition tier (Q443's invariant: a rung expressed in
// only one form ships to only one tier).
//
// The invariant asserted on every poll is
//
//	advertised + withheld == declared ceiling
//
// A declining gate bounds the advertisement at the set's own in-flight worker pods, so
// with one stuck pod against a ceiling of 4 the split is 1 + 3. Counting the same slot
// on both sides — or letting the rung reserve one, as the classic rung deliberately
// does not — breaks that sum while a cruder all-or-nothing assertion would still pass.
func TestV2_RunnerSet_ScaleSet_CapacityGateWithholdsAdvertisedCapacity(t *testing.T) {
	const ns = "v2-rs-capgate-scaleset"
	const label = "linux-capgate"
	const setName = "ss-capgate"
	const ceiling = int32(4)
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, ceiling)
	rs.Spec.CapacityGate = &v2alpha1.CapacityGate{Mode: v2alpha1.CapacityGateModeSchedulerVerdict}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 40 * time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	require.Eventually(t, func() bool {
		_, ok := srv.ScaleSetIDByName(label)
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	// Before the gate closes, the set advertises its whole declared ceiling and the
	// capacity rung publishes an explicit zero — the negative control for everything
	// below, and proof the rung reports "evaluated, not withholding" rather than
	// leaving its series absent.
	require.Eventually(t, func() bool {
		advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
		withheld := testutil.ToFloat64(
			scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonCapacity))
		return advertised == float64(ceiling) && withheld == 0
	}, 20*time.Second, 100*time.Millisecond,
		"an open gate must advertise the full ceiling and publish a zero for the capacity rung")

	pod := createV2WorkerPod(t, ns, setName, "worker-unplaceable")
	markUnschedulable(t, pod, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")
	waitForCapacityDeclined(t, ns, setName, 35*time.Second)

	require.Eventually(t, func() bool {
		advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
		withheld := testutil.ToFloat64(
			scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonCapacity))
		return advertised == 1 && withheld == float64(ceiling)-1
	}, 20*time.Second, 100*time.Millisecond,
		"a closed gate must advertise only the set's one in-flight worker and withhold the rest of the ceiling")

	advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, setName))
	withheld := testutil.ToFloat64(
		scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, setName, runnercore.AdmitReasonCapacity))
	require.Equal(t, float64(ceiling), advertised+withheld,
		"the capacity rung splits the declared ceiling, it does not consume from it")

	// Only the one pod this test created exists: a slot withheld here is a job GitHub
	// never assigns, so unlike the classic tier nothing is claimed and then unwound.
	var pods corev1.PodList
	require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{provisioner.LabelRunnerSet: setName}))
	require.Len(t, pods.Items, 1, "no further worker pod may be built while the set withholds capacity")
}
