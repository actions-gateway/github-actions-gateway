//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The capacity gate against a real apiserver, across both cluster facts (Q405, Q406,
// Q470).
//
// The gate takes two inputs from two parties: a tenant's spec.capacityGate.mode on the
// RunnerSet, and the platform operator's spec.clusterCapacity.nodeAutoscaling on the
// ActionsGateway. Which signal may be trusted follows from the second, so these tests
// vary the GATEWAY to reach the two signal paths — a set has no say in it, which is the
// property Q470 introduced and TestCapacityGate_TheDangerousCombinationIsUnrepresentable
// pins at the unit level.
//
// The unit tests cover the rung's arithmetic and every fail-open path. What only
// envtest can show is the loop closing: a real Pod carrying a real
// PodScheduled=False/Unschedulable status, aged past the scheduling grace by the
// reconciler's own RequeueAfter, publishing a condition that the admission rung then
// reads back on a live listener's next poll. A unit test can assert each half; it
// cannot assert that the two halves agree about the same object.
//
// Both tiers are covered because Q443 established that a rung expressed in only one
// form ships to only one tier. The tiers observe the refusal differently by
// construction: classic declines a delivered job and counts it under
// jobs_admission_rejected_total{reason="capacity"}, while the scale-set tier withholds
// slots from its advertisement so the job is never assigned and there is no delivery to
// count — hence capacity_withheld{reason="capacity"} is its counterpart.

// waitForCapacityDeclined blocks until the named RunnerSet's WorkerCapacityDeclined
// condition is True for the given reason, as seen through reader.
//
// The reader is load-bearing (Q559). The condition has two observers that do not
// update together: the apiserver, read directly by the suite's k8sClient, and the
// manager's informer cache, which is what the admission rung reads back in
// runnerSetTarget.capacityGateCondition. The apiserver is the earlier of the two, so
// a caller that waits on k8sClient and then delivers a job races the watch — the rung
// still sees an open gate, admits, and acquires.
//
// A caller asserting an admission outcome therefore passes the reconciler's own
// client. One asserting a per-poll advertisement may pass k8sClient: that tier
// re-reads every poll, so a stale read self-corrects, while the classic tier gets one
// delivery and no second chance. Detail: docs/development/testing.md.
func waitForCapacityDeclined(t *testing.T, reader client.Reader, ns, name, reason string, within time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		var rs v2.RunnerSet
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2.ConditionWorkerCapacityDeclined)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason
	}, within, 100*time.Millisecond,
		"an unplaceable worker pod must close the capacity gate on RunnerSet %s/%s with reason %s",
		ns, name, reason)
}

// recordPodEvent writes a real core/v1 Event against a worker pod, in the legacy
// recorder's shape — Source.Component plus First/LastTimestamp — which is how both
// cluster-autoscaler and Karpenter record. `at` is explicit because Event timestamps
// carry one-second resolution and the matcher orders verdicts by time.
func recordPodEvent(t *testing.T, pod *corev1.Pod, name, eventType, reason, component, message string, at time.Time) {
	t.Helper()
	stamp := metav1.NewTime(at)
	e := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1", Kind: "Pod",
			Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		},
		Reason:         reason,
		Message:        message,
		Source:         corev1.EventSource{Component: component},
		Type:           eventType,
		Count:          1,
		FirstTimestamp: stamp,
		LastTimestamp:  stamp,
	}
	require.NoError(t, k8sClient.Create(ctx, e))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), e) })
}

// TestV2_RunnerSet_CapacityGate_ElasticClusterDiscriminatesByReporter is Q406, and it is
// one test rather than two on purpose: both halves run against the same set, the same
// reconcile loop, and the same PodScheduled=False/Unschedulable verdict, so the ONLY
// difference between the pod that closes the gate and the pod that does not is who
// reported the event.
//
// That is the property the elastic-cluster signal rests on. `FailedScheduling` is
// kube-scheduler's own reason for every ordinary transient placement failure as well as
// Karpenter's declination, so a matcher keyed on the reason string alone would refuse
// jobs the moment a pod waited — the exact tenant-starving outcome this signal exists to
// avoid, and the reason a cluster that can grow is not gated on the scheduler's verdict
// at all.
//
// The gateway here deliberately leaves spec.clusterCapacity UNSET, so this also pins the
// default end to end: an operator who has never heard of the field gets the signal that
// can only under-gate, never the one that can starve a tenant.
//
// What only envtest can show, over the unit table: the field-selected UNCACHED Event
// read works against a real apiserver (the selector is the part a fake client cannot
// exercise), against Events with the real API's timestamp resolution, and the verdict
// it produces reaches the published condition that the admission rung reads back.
func TestV2_RunnerSet_CapacityGate_ElasticClusterDiscriminatesByReporter(t *testing.T) {
	const ns = "v2-rs-capgate-autoscaler"
	const setName = "capgate-as-set"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet(setName, ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3))
	rs.Spec.CapacityGate = &v2.CapacityGate{Mode: v2.CapacityGateModeObserve}
	// 30s deadline ⇒ a 15s scheduling grace, and a further 15s before the reaper
	// deletes the pod. Every assertion below lands inside one such window.
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 30 * time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	rec := startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2.ReasonListenerActive)

	// --- half one: an ordinary scheduler failure must NOT close the gate -------
	//
	// The pod is unschedulable and stays unschedulable; kube-scheduler says so on the
	// pod and again in an Event. On an elastic cluster this is a pod the autoscaler may
	// still be about to rescue.
	stuck := createV2WorkerPod(t, ns, setName, "worker-transient")
	markUnschedulable(t, stuck, "0/3 nodes are available: 3 Insufficient cpu")
	recordPodEvent(t, stuck, "worker-transient.sched", corev1.EventTypeWarning,
		"FailedScheduling", "default-scheduler",
		"0/3 nodes are available: 3 Insufficient cpu.", time.Now())

	// WorkersUnschedulable going True is the proof the pod crossed the grace and the
	// gate had its chance to read that pod's Events — without it, "the gate is open"
	// would be indistinguishable from "the gate has not looked yet".
	require.Eventually(t, func() bool {
		c := setCondition(t, ns, setName, v2.ConditionWorkersUnschedulable)
		return c != nil && c.Status == metav1.ConditionTrue
	}, 25*time.Second, 100*time.Millisecond,
		"the worker pod must age past the scheduling grace before the gate can be judged")

	gate := setCondition(t, ns, setName, v2.ConditionWorkerCapacityDeclined)
	require.NotNil(t, gate, "an opted-in set must publish the condition even when the gate is open")
	require.Equal(t, metav1.ConditionFalse, gate.Status,
		"a transient FailedScheduling from the scheduler is not an autoscaler declination; "+
			"gating on it would starve a tenant whose cluster was about to grow")
	require.Equal(t, v2.ReasonCapacityAvailable, gate.Reason)

	// --- half two: the autoscaler's own declination DOES close it -------------
	//
	// Same set, same shape of stuck pod, one difference: cluster-autoscaler itself
	// records that it will not add a node.
	declined := createV2WorkerPod(t, ns, setName, "worker-declined")
	markUnschedulable(t, declined, "0/3 nodes are available: 3 node(s) had untolerated taint {dedicated: gpu}")
	recordPodEvent(t, declined, "worker-declined.ca", corev1.EventTypeNormal,
		"NotTriggerScaleUp", "cluster-autoscaler",
		"pod didn't trigger scale-up: 1 max node group size reached, 2 node(s) had untolerated taint {dedicated: gpu}",
		time.Now())

	waitForCapacityDeclined(t, rec.Client, ns, setName, v2.ReasonScaleUpDeclined, 25*time.Second)

	gate = setCondition(t, ns, setName, v2.ConditionWorkerCapacityDeclined)
	require.Contains(t, gate.Message, "max node group size reached",
		"the autoscaler's own per-node-group text must reach the operator — that is what makes "+
			"this condition actionable rather than merely true")

	// End to end: the closed gate withholds the set's capacity from GitHub, so no job
	// is assigned that no node is coming for. The reaper may already have deleted the
	// first pod past its deadline, so the set advertises one or two in-flight pods of
	// its ceiling of 3 and withholds the rest.
	waitForCapacityWithheld(t, ns, setName, inFlightWorkerPods(t, ns, setName), 3)
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

	require.NoError(t, k8sClient.Create(ctx, newFixedSizeGatewayForSet("gw", ns)))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet(setName, ns, "gw", label, ceiling)
	rs.Spec.CapacityGate = &v2.CapacityGate{Mode: v2.CapacityGateModeObserve}
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 40 * time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	require.Eventually(t, func() bool {
		_, ok := srv.ScaleSetIDByName(label)
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register one scale set named %q", label)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2.ReasonListenerActive)

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
	waitForCapacityDeclined(t, k8sClient, ns, setName, v2.ReasonPodsUnschedulable, 35*time.Second)

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

// markImagePullBackOff puts a worker pod into the shape a real kubelet reports for an
// unpullable image: bound to a node, phase still Pending, and the runner container
// waiting in ImagePullBackOff. Measured on a kind cluster against the placeholder the
// 1.4 DinD templates ship (capacity-aware-intake.md §9j); envtest runs no kubelet, so
// the status is stamped rather than produced.
func markImagePullBackOff(t *testing.T, pod *corev1.Pod, image string) {
	t.Helper()
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodScheduled,
		Status: corev1.ConditionTrue,
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  provisioner.WorkerContainerName,
		Image: image,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "ImagePullBackOff",
			Message: `Back-off pulling image "` + image + `"`,
		}},
	}}
	require.NoError(t, k8sClient.Status().Update(ctx, pod))
}

// TestV2_RunnerSet_CapacityGate_WorkerThatCannotStartWithholdsCapacity is Q714 through the
// whole loop: a real Pod that BOUND to a node and then could not start must close the
// gate and withhold the set's capacity from GitHub.
//
// The gateway is the ELASTIC one, which is the load-bearing choice. Under
// nodeAutoscaling: Present the only other signal is the autoscaler's own declination,
// read from Events — and no Event is recorded here, so that path fails open. A decline
// observed in this configuration can therefore have come from nothing but the kubelet's
// startup verdict, which is what makes this the assertion that the verdict is not
// selected by the cluster fact the other two are.
//
// The skip is asserted on the advertised capacity rather than on the absence of a pod:
// "no pod appeared" would also pass if GitHub had assigned the job and the AGC had then
// failed to place it, which is the claim-and-stall this rung exists to prevent.
func TestV2_RunnerSet_CapacityGate_WorkerThatCannotStartWithholdsCapacity(t *testing.T) {
	const ns = "v2-rs-capgate-nostart"
	const setName = "capgate-nostart-set"
	const image = "example.invalid/build-capable-runner:replace-me"
	createNSForAGC(t, ns)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newRunnerSet(setName, ns, "gw")
	rs.Spec.MaxWorkers = ptr.To(int32(3))
	rs.Spec.CapacityGate = &v2.CapacityGate{Mode: v2.CapacityGateModeObserve}
	// Long enough that the reaper cannot delete the gate's evidence mid-assertion.
	rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: 5 * time.Minute}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), newGatewayForSet("gw", ns, ""))
		_ = k8sClient.Delete(context.Background(), &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})

	rec := startRunnerSetReconciler(t)
	waitForSetReadyReason(t, ns, setName, metav1.ConditionTrue, v2.ReasonListenerActive)

	pod := createV2WorkerPod(t, ns, setName, "worker-unpullable")
	markImagePullBackOff(t, pod, image)
	waitForCapacityDeclined(t, rec.Client, ns, setName, v2.ReasonPodsNotStarting, 25*time.Second)

	// The scheduler signal must stay quiet on the same object: this pod was placed, so
	// reporting it as unschedulable would send an operator after a node it already has.
	var seen v2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: setName}, &seen))
	unsched := meta.FindStatusCondition(seen.Status.Conditions, v2.ConditionWorkersUnschedulable)
	require.NotNil(t, unsched)
	require.Equal(t, metav1.ConditionFalse, unsched.Status,
		"a bound pod is not a scheduling failure, whatever the kubelet then makes of it")
	declined := meta.FindStatusCondition(seen.Status.Conditions, v2.ConditionWorkerCapacityDeclined)
	require.NotNil(t, declined)
	require.Contains(t, declined.Message, image,
		"the condition must name the image, which is the operator's whole remedy")

	// The closed gate withholds the set's capacity from GitHub: one in-flight pod
	// against a ceiling of 3 advertises that one and withholds the other two.
	waitForCapacityWithheld(t, ns, setName, inFlightWorkerPods(t, ns, setName), 3)
}

// waitForCapacityWithheld waits until set advertises only its inflight worker pods,
// with the capacity rung withholding the rest of ceiling. inflight is read on every
// poll, so a pod the reaper deletes mid-wait moves the target with it.
func waitForCapacityWithheld(t *testing.T, ns, set string, inflight func() int32, ceiling int32) {
	t.Helper()
	require.Eventually(t, func() bool {
		n := inflight()
		advertised := testutil.ToFloat64(scaleSetTestMetrics.AdvertisedCapacity.WithLabelValues(ns, set))
		withheld := testutil.ToFloat64(
			scaleSetTestMetrics.CapacityWithheld.WithLabelValues(ns, set, runnercore.AdmitReasonCapacity))
		return n > 0 && advertised == float64(n) && withheld == float64(ceiling-n)
	}, 20*time.Second, 100*time.Millisecond,
		"a closed gate must advertise only the set's %d in-flight worker(s) and withhold the rest of %d", inflight, ceiling)
}

// inFlightWorkerPods counts set's worker pods that are not being deleted, which is
// the in-flight figure the scale-set tier advertises.
func inFlightWorkerPods(t *testing.T, ns, set string) func() int32 {
	return func() int32 {
		var pods corev1.PodList
		require.NoError(t, k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: set}))
		var n int32
		for i := range pods.Items {
			if pods.Items[i].DeletionTimestamp == nil {
				n++
			}
		}
		return n
	}
}
