package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Unit coverage for the Q303 v2 RunnerSet worker-capacity evaluations. The
// envtest suite (v2_runnerset_capacity_test.go) proves the end-to-end
// condition/RequeueAfter behaviour against a real apiserver; these fake-client
// tests exercise the eval branches directly and fast.

// capTemplate returns a RunnerTemplateSpec whose single container requests the
// given CPU (empty ⇒ no request), the v2 source of the quota footprint.
func capTemplate(cpu string) *v2alpha1.RunnerTemplateSpec {
	req := corev1.ResourceList{}
	if cpu != "" {
		req[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	return &v2alpha1.RunnerTemplateSpec{
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Resources: corev1.ResourceRequirements{Requests: req}}},
			},
		},
	}
}

// capQuota builds a namespace ResourceQuota with the given hard limits.
func capQuota(ns, name string, hard corev1.ResourceList) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}
}

// capWorkerPod builds a worker pod carrying the v2 runner-set label for setName.
func capWorkerPod(ns, setName, name string, phase corev1.PodPhase, created time.Time, schedStatus corev1.ConditionStatus, schedReason, schedMsg string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{provisioner.LabelRunnerSet: setName},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if schedReason != "" || schedStatus != "" {
		p.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  schedStatus,
			Reason:  schedReason,
			Message: schedMsg,
		}}
	}
	return p
}

// capBackingOff puts a bound worker pod into the kubelet's image-pull backoff — the
// shape §9j measured from the 1.4 DinD templates' example.invalid placeholder.
func capBackingOff(pod *corev1.Pod, reason, message string) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  provisioner.WorkerContainerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}}
	return pod
}

// capStarted marks a worker pod's runner container as having begun running at at.
func capStarted(pod *corev1.Pod, at time.Time) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  provisioner.WorkerContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}},
	}}
	return pod
}

func capReconciler(t *testing.T, now time.Time, objs ...client.Object) *RunnerSetReconciler {
	t.Helper()
	return &RunnerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build(),
		Now:    func() time.Time { return now },
	}
}

// --- worker-quota ladder ---------------------------------------------------

// TestRunnerSetWorkerQuota_NoQuota: with no namespace ResourceQuota, both tiers are
// False and the pressure reason is NoQuota.
func TestRunnerSetWorkerQuota_NoQuota(t *testing.T) {
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) { r.Spec.MaxWorkers = ptr.To(int32(3)) })
	r := capReconciler(t, time.Now())
	st := r.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))
	assert.False(t, st.pressure)
	assert.False(t, st.exceeded)
	assert.Equal(t, "NoQuota", st.pressureReason)
}

// TestRunnerSetWorkerQuota_Exceeded: quota headroom below a single worker's request
// trips WorkerQuotaExceeded and supersedes the pressure tier.
func TestRunnerSetWorkerQuota_Exceeded(t *testing.T) {
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) { r.Spec.MaxWorkers = ptr.To(int32(3)) })
	quota := capQuota("ns", "tight", corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")})
	r := capReconciler(t, time.Now(), quota)

	st := r.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))
	assert.True(t, st.exceeded)
	assert.Equal(t, "QuotaExhausted", st.exceededReason)
	assert.False(t, st.pressure, "exceeded supersedes pressure")
	assert.Equal(t, "Superseded", st.pressureReason)
}

// TestRunnerSetWorkerQuota_Pressure: quota admits one worker but not the full ceiling
// (maxWorkers), tripping WorkerQuotaPressure but not Exceeded.
func TestRunnerSetWorkerQuota_Pressure(t *testing.T) {
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) { r.Spec.MaxWorkers = ptr.To(int32(3)) })
	quota := capQuota("ns", "roomy", corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")}) // 1000m
	r := capReconciler(t, time.Now(), quota)

	st := r.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))
	assert.False(t, st.exceeded, "1000m admits one 500m worker")
	assert.True(t, st.pressure, "1000m cannot fit the 1500m ceiling")
	assert.Equal(t, "InsufficientQuotaHeadroom", st.pressureReason)
}

// TestRunnerSetWorkerQuota_UnboundedCeilingNoPressure: with no maxWorkers/priorityTiers
// the ceiling is unbounded, so the warning tier is skipped even under a quota.
func TestRunnerSetWorkerQuota_UnboundedCeilingNoPressure(t *testing.T) {
	rs := rsObj("set", "ns", nil) // no MaxWorkers, no PriorityTiers
	quota := capQuota("ns", "roomy", corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")})
	r := capReconciler(t, time.Now(), quota)

	st := r.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))
	assert.False(t, st.pressure)
	assert.False(t, st.exceeded)
}

// TestRunnerSetWorkerQuota_ListError: a quota-list failure yields the advisory
// QuotaUnknown pressure reason rather than an alarm.
func TestRunnerSetWorkerQuota_ListError(t *testing.T) {
	rs := rsObj("set", "ns", nil)
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()
	r := &RunnerSetReconciler{Client: c}
	st := r.evalRunnerSetWorkerQuota(context.Background(), rs, capTemplate("500m"))
	assert.Equal(t, "QuotaUnknown", st.pressureReason)
	assert.False(t, st.exceeded)
}

// --- unschedulable ---------------------------------------------------------

// TestRunnerSetUnschedulable_NoPods: no worker pods ⇒ schedulable.
func TestRunnerSetUnschedulable_NoPods(t *testing.T) {
	rs := rsObj("set", "ns", nil)
	r := capReconciler(t, time.Now())
	st := r.evalRunnerSetWorkersUnschedulable(context.Background(), rs)
	assert.False(t, st.unschedulable)
	assert.Equal(t, v2alpha1.ReasonWorkersSchedulable, st.reason)
}

// TestRunnerSetUnschedulable_PastGrace: a Pending+Unschedulable pod older than the
// grace (deadline/2) trips the condition and names the pod + verdict.
func TestRunnerSetUnschedulable_PastGrace(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) {
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute} // grace 5m
	})
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available: untolerated taint")
	r := capReconciler(t, now, pod)

	st := r.evalRunnerSetWorkersUnschedulable(context.Background(), rs)
	assert.True(t, st.unschedulable)
	assert.Equal(t, v2alpha1.ReasonPodsUnschedulable, st.reason)
	assert.Contains(t, st.message, "worker-stuck")
	assert.Contains(t, st.message, "untolerated taint")
}

// TestRunnerSetUnschedulable_WithinGrace: an unschedulable pod younger than the grace
// does not trip yet and schedules a re-check at the crossing.
func TestRunnerSetUnschedulable_WithinGrace(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) {
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute} // grace 5m
	})
	pod := capWorkerPod("ns", "set", "worker-young", corev1.PodPending, now.Add(-2*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "no nodes")
	r := capReconciler(t, now, pod)

	st := r.evalRunnerSetWorkersUnschedulable(context.Background(), rs)
	assert.False(t, st.unschedulable)
	assert.InDelta(t, (3 * time.Minute).Seconds(), st.requeueAfter.Seconds(), 1, "re-check ~3m out (grace 5m − age 2m)")
}

// TestRunnerSetUnschedulable_ListError: a pod-list failure yields a schedulable
// result carrying the error in the message.
func TestRunnerSetUnschedulable_ListError(t *testing.T) {
	rs := rsObj("set", "ns", nil)
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()
	r := &RunnerSetReconciler{Client: c, Now: func() time.Time { return time.Now() }}
	st := r.evalRunnerSetWorkersUnschedulable(context.Background(), rs)
	assert.False(t, st.unschedulable)
	assert.Contains(t, st.message, "could not list worker pods")
}

// TestRunnerSetUnschedulableGrace_Default: an unset pendingPodDeadline uses the
// provisioner default; grace is half of it and always positive.
func TestRunnerSetUnschedulableGrace_Default(t *testing.T) {
	assert.Equal(t, provisioner.PendingPodDeadlineOrDefault(nil)/2, runnerSetUnschedulableGrace(rsObj("set", "ns", nil)))
	// A tiny deadline still yields a positive (≥1s) grace.
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) { r.Spec.PendingPodDeadline = &metav1.Duration{Duration: time.Nanosecond} })
	assert.Equal(t, time.Second, runnerSetUnschedulableGrace(rs))
}

// --- apply / clear ---------------------------------------------------------

// TestApplyWorkerCapacityConditions_SetsAllThree: applying under an exhausted quota +
// an unschedulable pod sets all three conditions, emits one Warning Event on the
// False→True unschedulable transition, and returns no re-check (pod already past grace).
func TestApplyWorkerCapacityConditions_SetsAllFour(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) {
		r.Spec.MaxWorkers = ptr.To(int32(3))
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	})
	quota := capQuota("ns", "tight", corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("100m")})
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, quota, pod)
	r.Recorder = rec

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate("500m"), nil)
	assert.Zero(t, requeue, "pod already past grace ⇒ no scheduled re-check")

	exceeded := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerQuotaExceeded)
	require.NotNil(t, exceeded)
	assert.Equal(t, metav1.ConditionTrue, exceeded.Status)
	unsched := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable)
	require.NotNil(t, unsched)
	assert.Equal(t, metav1.ConditionTrue, unsched.Status)
	pressure := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerQuotaPressure)
	require.NotNil(t, pressure)
	assert.Equal(t, metav1.ConditionFalse, pressure.Status, "superseded by exceeded")
	// Q906's condition is published on every apply, and this pod is a scheduling
	// problem rather than a startup one, so it must read False here. The two are
	// disjoint by construction: an unschedulable pod is never bound.
	notStarting := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, notStarting)
	assert.Equal(t, metav1.ConditionFalse, notStarting.Status)
	assert.Equal(t, v2alpha1.ReasonWorkersStarting, notStarting.Reason)

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "WorkersUnschedulable")
	default:
		t.Fatal("expected a Warning Event on the WorkersUnschedulable False→True transition")
	}
}

// TestApplyWorkerCapacityConditions_NoEventWhenStable: a second apply with the
// condition already True emits no further Event (transition-only).
func TestApplyWorkerCapacityConditions_NoEventWhenStable(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) {
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	})
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, pod)
	r.Recorder = rec

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)
	<-rec.Events // drain the first-transition event
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)
	select {
	case ev := <-rec.Events:
		t.Fatalf("no Event expected while the condition stays True, got %q", ev)
	default:
	}
}

// TestClearWorkerCapacityConditions: clearing sets all four to False with benign reasons.
func TestClearWorkerCapacityConditions(t *testing.T) {
	rs := rsObj("set", "ns", nil)
	r := capReconciler(t, time.Now())
	r.clearWorkerCapacityConditions(rs)
	for _, ct := range []string{
		v2alpha1.ConditionWorkerQuotaPressure,
		v2alpha1.ConditionWorkerQuotaExceeded,
		v2alpha1.ConditionWorkersUnschedulable,
		v2alpha1.ConditionWorkersNotStarting,
	} {
		c := meta.FindStatusCondition(rs.Status.Conditions, ct)
		require.NotNil(t, c, ct)
		assert.Equal(t, metav1.ConditionFalse, c.Status, ct)
	}
}

// The Q906 condition: the kubelet's startup verdict, reported on a set that opted into
// no capacity gate at all. Q714 already computed this fact on every reconcile and
// published it only through WorkerCapacityDeclined, which a default set does not carry
// — so an ungated set said nothing between the kubelet's verdict and the reaper's
// WorkerPodStuckPending Event one full pendingPodDeadline later.

// TestWorkersNotStarting_UngatedSetReportsIt is that case end to end.
func TestWorkersNotStarting_UngatedSetReportsIt(t *testing.T) {
	now := time.Now()
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""),
		"ImagePullBackOff", `Back-off pulling image "example.invalid/build-capable-runner:replace-me"`)
	// No gateOn(...): spec.capacityGate is unset, which is the default.
	rs := rsObj("set", "ns", nil)
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, pod)
	r.Recorder = rec

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonPodsNotStarting, c.Reason)
	assert.Contains(t, c.Message, "worker-backoff")
	assert.Contains(t, c.Message, "example.invalid",
		"the kubelet's own message names the image, which is the operator's whole remedy")
	assert.NotContains(t, c.Message, "job intake is gated",
		"this condition reports and decides nothing; on an ungated set intake is not gated")

	// The gate's condition must be absent, not False: its presence is what says a set
	// has a gate at all, and this set has none.
	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	// The sibling stays silent — a bound pod is not a scheduling problem.
	unsched := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable)
	require.NotNil(t, unsched)
	assert.Equal(t, metav1.ConditionFalse, unsched.Status)

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "WorkersNotStarting")
	default:
		t.Fatal("expected a Warning Event on the WorkersNotStarting False→True transition")
	}
}

// TestWorkersNotStarting_UngatedSetSchedulesTheStartupRecheck is the half that makes the
// condition arrive at all rather than ten minutes late.
//
// A pod that has BOUND but not yet declared itself either way changes no phase when it
// enters backoff, and the Pod watch drops updates that change no phase — so nothing
// wakes the reconciler between the pod's creation and its pendingPodDeadline reap. The
// gate schedules that re-check on its own path; before Q906 that was the only place it
// was scheduled, so an ungated set never got one.
func TestWorkersNotStarting_UngatedSetSchedulesTheStartupRecheck(t *testing.T) {
	now := time.Now()
	// Bound (PodScheduled=True), Pending, and no container status at all: the kubelet
	// has not reached a verdict yet. This is the only state that needs the re-check.
	pod := capWorkerPod("ns", "set", "worker-undecided", corev1.PodPending,
		now.Add(-time.Second), corev1.ConditionTrue, "", "")
	rs := rsObj("set", "ns", nil)
	r := capReconciler(t, now, pod)

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)

	assert.Equal(t, startupVerdictRecheck, requeue,
		"an ungated set must still come back for a bound worker pod that has not declared itself")

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "no verdict yet is not a verdict of not-starting")
}

// TestWorkersNotStarting_ClearsWhenTheWorkerStarts: a pod that ran clears the condition,
// so a recovered set does not sit on a stale alarm.
func TestWorkersNotStarting_ClearsWhenTheWorkerStarts(t *testing.T) {
	now := time.Now()
	backoff := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	rs := rsObj("set", "ns", nil)
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, backoff)
	r.Recorder = rec
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting))
	<-rec.Events // drain the first-transition Event

	// Same set, and the pod is now running.
	running := capStarted(capWorkerPod("ns", "set", "worker-backoff", corev1.PodRunning,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), now)
	r2 := capReconciler(t, now, running)
	r2.Recorder = rec
	r2.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, v2alpha1.ReasonWorkersStarting, c.Reason)
}

// TestWorkersNotStarting_NoEventWhenStable: transition-only, like its sibling.
func TestWorkersNotStarting_NoEventWhenStable(t *testing.T) {
	now := time.Now()
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	rs := rsObj("set", "ns", nil)
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, pod)
	r.Recorder = rec

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)
	<-rec.Events
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), nil)
	select {
	case ev := <-rec.Events:
		t.Fatalf("no Event expected while the condition stays True, got %q", ev)
	default:
	}
}

// TestWorkersNotStarting_GatedSetReportsBothWithoutContradiction: on a set that DID opt
// in, the observation and the intake decision both publish, and only the gate's message
// says intake is gated.
func TestWorkersNotStarting_GatedSetReportsBothWithoutContradiction(t *testing.T) {
	now := time.Now()
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	r := capReconciler(t, now, pod)

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())

	obs := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	require.NotNil(t, obs)
	assert.Equal(t, metav1.ConditionTrue, obs.Status)
	assert.NotContains(t, obs.Message, "job intake is gated")

	gate := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, gate)
	assert.Equal(t, metav1.ConditionTrue, gate.Status)
	assert.Equal(t, v2alpha1.ReasonPodsNotStarting, gate.Reason)
	assert.Contains(t, gate.Message, "job intake is gated")
	assert.Contains(t, gate.Message, obs.Message,
		"the gated message is the observation with the consequence prefixed, so the two cannot drift")
}
