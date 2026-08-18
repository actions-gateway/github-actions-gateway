package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The capacity gate reads two inputs from two parties (Q470): the tenant's mode on the
// RunnerSet, and the platform operator's cluster fact on the ActionsGateway. These
// helpers keep the two visibly separate in every test, because the point of the split
// is that a set cannot pick a signal — it can only ask to be gated.

// gateOn returns a RunnerSet mutator that opts the set into the capacity gate.
func gateOn(mode string) func(*v2alpha1.RunnerSet) {
	return func(r *v2alpha1.RunnerSet) {
		r.Spec.CapacityGate = &v2alpha1.CapacityGate{Mode: mode}
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	}
}

// gwAutoscaling builds the resolved ActionsGateway the reconciler passes down, carrying
// the platform operator's assertion about whether anything can add a node.
func gwAutoscaling(nodeAutoscaling string) *v2alpha1.ActionsGateway {
	return &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v2alpha1.ActionsGatewaySpec{
			ClusterCapacity: &v2alpha1.ClusterCapacity{NodeAutoscaling: nodeAutoscaling},
		},
	}
}

// gwFixed is a cluster the operator has asserted cannot grow — the only configuration
// in which the scheduler's verdict alone may gate intake.
func gwFixed() *v2alpha1.ActionsGateway { return gwAutoscaling(v2alpha1.NodeAutoscalingAbsent) }

// gwElastic is a cluster that can grow, which is also the default for an unset field.
func gwElastic() *v2alpha1.ActionsGateway { return gwAutoscaling(v2alpha1.NodeAutoscalingPresent) }

// TestCapacityGate_ModeOffCarriesNoCondition is the no-cost-for-the-default assertion.
// The default must not merely report False — it must publish nothing at all, so an
// operator scanning conditions can tell an opted-in set from an untouched one.
func TestCapacityGate_ModeOffCarriesNoCondition(t *testing.T) {
	now := time.Now()
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	for _, tt := range []struct {
		name string
		mut  func(*v2alpha1.RunnerSet)
	}{
		{"capacityGate unset entirely", func(r *v2alpha1.RunnerSet) {
			r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
		}},
		{"capacityGate present with mode Off", gateOn(v2alpha1.CapacityGateModeOff)},
		{"capacityGate present with an empty mode", gateOn("")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rs := rsObj("set", "ns", tt.mut)
			r := capReconciler(t, now, pod)

			r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

			// The sibling condition still fires — the pod really is unschedulable. Only
			// the intake decision is absent, which is what "the gate is off" means.
			unsched := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable)
			require.NotNil(t, unsched)
			assert.Equal(t, metav1.ConditionTrue, unsched.Status)
			assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined),
				"a set with no capacity gate must carry no WorkerCapacityDeclined condition")
		})
	}
}

// TestCapacityGate_FixedClusterDeclinesOnAnUnschedulablePod: where the operator has
// asserted nothing can add a node, the scheduler's own verdict — already published as
// WorkersUnschedulable — becomes the intake decision, because no actor is waiting on
// that pod and it is pure waste.
func TestCapacityGate_FixedClusterDeclinesOnAnUnschedulablePod(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, pod)
	r.Recorder = rec

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonPodsUnschedulable, c.Reason,
		"the reason must name the SIGNAL, so an operator can tell which rung stopped their jobs")
	assert.Contains(t, c.Message, "job intake is gated",
		"the message must state the consequence, not merely restate the stuck pod")
	assert.Contains(t, c.Message, "untolerated taint", "the scheduler's own verdict must survive into the message")

	// Two Events fire on this reconcile and both are wanted: the stuck pod, and the
	// distinct fact that this set has stopped taking jobs.
	var got []string
	for len(rec.Events) > 0 {
		got = append(got, <-rec.Events)
	}
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "WorkersUnschedulable")
	assert.Contains(t, got[1], "WorkerCapacityDeclined")
}

// TestCapacityGate_LatchesWhenThePodIsReaped is Q512 at the condition layer: the
// gate's evidence is the stuck pod itself, and the reaper deletes that pod at
// pendingPodDeadline. Clearing on the reap is what §9e measured as a no-op on the
// scale-set tier — the advertisement snapped back to the full ceiling every deadline
// window — so the decline must latch as AwaitingProbe instead, the reason the two
// rung forms read to admit exactly one probe job.
func TestCapacityGate_LatchesWhenThePodIsReaped(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	// The reaper deletes the stuck pod; nothing has shown that capacity returned.
	require.NoError(t, r.Delete(context.Background(), pod))
	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status,
		"reaping the gate's own evidence must not restore the full advertisement")
	assert.Equal(t, v2alpha1.ReasonAwaitingProbe, c.Reason)
	assert.Contains(t, c.Message, "one probe job",
		"the message must say intake is limited, not closed")
	assert.Contains(t, c.Message, v2alpha1.ReasonPodsUnschedulable,
		"the reaped verdict must survive into the latched message")
	assert.Contains(t, c.Message, "untolerated taint",
		"the scheduler's own text must survive the reap — the pod that carried it is gone")
	assert.Equal(t, autoscalerVerdictRecheck, requeue,
		"a probe that binds but stays Pending never fires the phase-only Pod watch, so the latch must poll")

	// Re-publishing the latch over itself must keep its message, not re-wrap it.
	msg := c.Message
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	c = meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, msg, c.Message)
}

// TestCapacityGate_LatchClearsWhenAProbeStarts: the probe pod's first container
// running is the evidence that capacity returned, and it must clear the latch
// completely — the whole advertisement comes back, not another probe slot.
//
// The probe is still Pending when it clears, which is the point of reading container
// state rather than the phase: a pod whose init container is running has demonstrated
// the cluster can start a worker while the phase still says Pending.
func TestCapacityGate_LatchClearsWhenAProbeStarts(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	probe := capWorkerPod("ns", "set", "worker-probe", corev1.PodPending, now, corev1.ConditionTrue, "", "")
	probe.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(time.Minute))
	capStarted(probe, now.Add(time.Minute))
	require.NoError(t, r.Create(context.Background(), probe))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "a started probe is capacity returning")
	assert.Equal(t, v2alpha1.ReasonCapacityAvailable, c.Reason)
}

// TestCapacityGate_LatchHoldsWhileTheProbeHasOnlyBound is Q714's negative control on
// Q512's latch, and the reason the release evidence had to change from scheduling to
// starting.
//
// A probe pod binds within a second of creation and only reveals that it cannot start
// seconds later. Releasing the latch on the bind therefore restores the full
// advertisement inside that gap — GitHub assigns the whole ceiling, and the burst of
// wasted claims the latch exists to bound comes back. Nothing about this window is
// visible in the reason or the value; only the timing differs, which is why it needs
// a test rather than an argument.
func TestCapacityGate_LatchHoldsWhileTheProbeHasOnlyBound(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	// Bound after the decline, no container has run yet: the kubelet has not said
	// anything either way, so nothing has shown that capacity returned.
	probe := capWorkerPod("ns", "set", "worker-probe", corev1.PodPending, now, corev1.ConditionTrue, "", "")
	probe.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(time.Minute))
	require.NoError(t, r.Create(context.Background(), probe))
	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status,
		"a probe that has only bound is not yet evidence that a worker can run here")
	assert.Equal(t, v2alpha1.ReasonAwaitingProbe, c.Reason)
	assert.Equal(t, startupVerdictRecheck, requeue,
		"an undecided bound probe fires no phase-change watch event, so the gate must poll for its verdict")
}

// TestCapacityGate_LatchIgnoresPreDeclineSchedulingEvidence: a pod that scheduled
// BEFORE the decline began — a still-running worker from before the cluster filled,
// or a Succeeded pod lingering inside completedPodTTL — says nothing about capacity
// now. Only a binding newer than the condition breaks the latch; anything else would
// defeat it the moment a set had any pre-decline pod at all.
func TestCapacityGate_LatchIgnoresPreDeclineSchedulingEvidence(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	old := capWorkerPod("ns", "set", "worker-old", corev1.PodRunning, now.Add(-time.Hour), corev1.ConditionTrue, "", "")
	old.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-time.Hour))

	r := capReconciler(t, now, pod, old)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonAwaitingProbe, c.Reason)
}

// TestCapacityGate_LatchReturnsToTheLiveVerdictWhenTheProbeSticks closes the cycle:
// the probe's own stuck pod re-earns the live reason with fresh evidence, and the
// next reap latches again — one wasted claim per deadline window, §5's trickle rate.
func TestCapacityGate_LatchReturnsToTheLiveVerdictWhenTheProbeSticks(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.Equal(t, v2alpha1.ReasonAwaitingProbe,
		meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined).Reason)

	// The probe pod sticks past the scheduling grace in its turn.
	probe := capWorkerPod("ns", "set", "worker-probe", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "still no room")
	require.NoError(t, r.Create(context.Background(), probe))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonPodsUnschedulable, c.Reason, "fresh evidence must displace the latch")
	assert.Contains(t, c.Message, "still no room")
}

// TestCapacityGate_LatchDoesNotSurviveAnUnsupportedMode: an unrecognized mode's
// fail-open is not a capacity verdict, and a latch held across it would gate a set
// under semantics the operator never selected.
func TestCapacityGate_LatchDoesNotSurviveAnUnsupportedMode(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn("Probe"))
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
		Reason: v2alpha1.ReasonAwaitingProbe, Message: "latched",
	})
	r := capReconciler(t, now)

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, v2alpha1.ReasonGateModeUnsupported, c.Reason)
}

// TestCapacityGate_OptingOutRetractsTheCondition: a set that turns the gate off while
// it is declining must not leave a True condition behind — the rung reads that
// condition, so a stale True would keep intake gated forever.
func TestCapacityGate_OptingOutRetractsTheCondition(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	r := capReconciler(t, now, pod)

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	rs.Spec.CapacityGate.Mode = v2alpha1.CapacityGateModeOff
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined),
		"opting out must retract the condition, not leave a stale True gating intake")
}

// TestCapacityGate_ClearedWhenReferencesDoNotResolve: with no listeners running and no
// worker pods being provisioned, a lingering True would gate a set that is already
// stopped for a louder reason.
func TestCapacityGate_ClearedWhenReferencesDoNotResolve(t *testing.T) {
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
		Reason: v2alpha1.ReasonPodsUnschedulable, Message: "stale",
	})
	r := capReconciler(t, time.Now())

	r.clearWorkerCapacityConditions(rs)

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))
}

// --- an elastic cluster: the autoscaler's own declination (Q406) --------------

// eventReaderStub stands in for the uncached apiserver reader the gate reads pod Events
// through. The fake client cannot serve an arbitrary field selector, and the selector is
// exactly what these tests need to assert: the gate must read Events for stuck pods
// only, one pod at a time.
type eventReaderStub struct {
	byPod map[string][]corev1.Event
	err   error
	// calls records the pod names the gate asked about, in order, so a test can assert
	// both what was read and — more importantly — what was not.
	calls []string
}

func (s *eventReaderStub) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("eventReaderStub: unexpected Get")
}

func (s *eventReaderStub) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var lo client.ListOptions
	for _, o := range opts {
		o.ApplyToList(&lo)
	}
	if lo.FieldSelector == nil {
		return errors.New("eventReaderStub: the gate must field-select Events to one pod, not list a namespace")
	}
	name, ok := lo.FieldSelector.RequiresExactMatch("involvedObject.name")
	if !ok {
		return errors.New("eventReaderStub: no involvedObject.name in the field selector")
	}
	s.calls = append(s.calls, name)
	if s.err != nil {
		return s.err
	}
	el, ok := list.(*corev1.EventList)
	if !ok {
		return errors.New("eventReaderStub: not an EventList")
	}
	el.Items = s.byPod[name]
	return nil
}

// autoscalerGate runs one reconcile's worth of capacity-condition evaluation for a
// gate-enabled set on an ELASTIC cluster against the given pods and Event stub,
// returning the published condition and the re-check the gate asked for.
func autoscalerGate(t *testing.T, now time.Time, reader client.Reader, pods ...*corev1.Pod) (*v2alpha1.RunnerSet, *metav1.Condition, time.Duration) {
	t.Helper()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	objs := make([]client.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	r := capReconciler(t, now, objs...)
	r.EventReader = reader

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())
	return rs, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined), requeue
}

// stuckPod is a worker pod the scheduler has given up on, aged past the scheduling
// grace (half of the 10m pendingPodDeadline gateOn sets).
func stuckPod(name string, now time.Time) *corev1.Pod {
	return capWorkerPod("ns", "set", name, corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available: 3 Insufficient cpu")
}

// TestCapacityGate_ElasticClusterGatesOnADeclination is the mode's positive path:
// the cluster autoscaler's own "I will not add a node for this" becomes the intake
// decision, and its per-node-group text reaches the operator.
func TestCapacityGate_ElasticClusterGatesOnADeclination(t *testing.T) {
	now := time.Now()
	pod := stuckPod("worker-stuck", now)
	reader := &eventReaderStub{byPod: map[string][]corev1.Event{
		"worker-stuck": {legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, now.Add(-time.Minute))},
	}}

	_, c, requeue := autoscalerGate(t, now, reader, pod)

	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonScaleUpDeclined, c.Reason,
		"the reason must name the autoscaler's declination, not the scheduler's verdict")
	assert.Contains(t, c.Message, "job intake is gated")
	assert.Contains(t, c.Message, "worker-stuck")
	assert.Contains(t, c.Message, "max node group size reached",
		"the autoscaler's own per-node-group text is what makes the condition actionable")
	assert.Equal(t, autoscalerVerdictRecheck, requeue,
		"nothing watches Events, so the gate must schedule its own re-check — otherwise a "+
			"later scale-up would never reopen it")
}

// TestCapacityGate_ElasticClusterLatchesAfterTheReap: the latch is signal-agnostic —
// an autoscaler declination survives its pod's reaping exactly as a scheduler verdict
// does, with the autoscaler's own text carried into the latched message.
func TestCapacityGate_ElasticClusterLatchesAfterTheReap(t *testing.T) {
	now := time.Now()
	pod := stuckPod("worker-stuck", now)
	reader := &eventReaderStub{byPod: map[string][]corev1.Event{
		"worker-stuck": {legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, now.Add(-time.Minute))},
	}}
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	r := capReconciler(t, now, pod)
	r.EventReader = reader

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())
	require.Equal(t, v2alpha1.ReasonScaleUpDeclined,
		meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined).Reason)

	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonAwaitingProbe, c.Reason)
	assert.Contains(t, c.Message, v2alpha1.ReasonScaleUpDeclined)
	assert.Contains(t, c.Message, "max node group size reached",
		"the autoscaler's per-node-group text must survive the reap")
}

// TestCapacityGate_ElasticClusterIgnoresATransientSchedulerFailure is THE test for
// this mode. FailedScheduling is kube-scheduler's own reason as well as Karpenter's,
// so a matcher keyed on the reason string alone would gate every set on an elastic
// cluster the moment a pod waited on an ordinary transient placement failure —
// refusing jobs the cluster was about to run.
func TestCapacityGate_ElasticClusterIgnoresATransientSchedulerFailure(t *testing.T) {
	now := time.Now()
	pod := stuckPod("worker-stuck", now)
	reader := &eventReaderStub{byPod: map[string][]corev1.Event{
		"worker-stuck": {legacyEvent(reasonFailedScheduling, defaultSchedulerName, schedulerFailMsg, now.Add(-time.Minute))},
	}}

	rs, c, _ := autoscalerGate(t, now, reader, pod)

	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status,
		"an ordinary scheduling failure is not an autoscaler declination; gating on it would "+
			"starve a tenant whose cluster was about to grow")
	assert.Equal(t, v2alpha1.ReasonCapacityAvailable, c.Reason)

	// The two conditions genuinely disagree on the same reconcile, and that disagreement
	// IS the point: the pod really is unschedulable — the same set on a cluster reporting
	// nodeAutoscaling: Absent would have gated on exactly this — and here the gate
	// declines to draw the same conclusion because no autoscaler has said no.
	assert.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable),
		"the sibling condition must still report the stuck pod")
}

// TestCapacityGate_ElasticClusterFailsOpen covers every way the signal can be
// missing or unreadable. Each must leave intake exactly as it is today: under-gating is
// this rung's default behavior, over-gating starves a tenant.
func TestCapacityGate_ElasticClusterFailsOpen(t *testing.T) {
	now := time.Now()

	t.Run("no events recorded for the stuck pod", func(t *testing.T) {
		reader := &eventReaderStub{}
		_, c, _ := autoscalerGate(t, now, reader, stuckPod("worker-stuck", now))

		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Contains(t, c.Message, "no cluster autoscaler has declined")
	})

	t.Run("the Event read fails", func(t *testing.T) {
		reader := &eventReaderStub{err: errors.New("events forbidden")}
		_, c, requeue := autoscalerGate(t, now, reader, stuckPod("worker-stuck", now))

		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Contains(t, c.Message, "events forbidden",
			"the operator must see that the gate could not evaluate, not a bare 'capacity available'")
		assert.Equal(t, autoscalerVerdictRecheck, requeue, "a failed read must still be retried")
	})

	t.Run("no Event reader is wired", func(t *testing.T) {
		_, c, _ := autoscalerGate(t, now, nil, stuckPod("worker-stuck", now))

		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Contains(t, c.Message, "no direct API reader is wired")
	})

	t.Run("an unrecognized autoscaler vocabulary", func(t *testing.T) {
		reader := &eventReaderStub{byPod: map[string][]corev1.Event{
			"worker-stuck": {legacyEvent("OptimizerDeferred", "cast-ai", "waiting for a cheaper node", now)},
		}}
		_, c, _ := autoscalerGate(t, now, reader, stuckPod("worker-stuck", now))

		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionFalse, c.Status,
			"a proprietary autoscaler the matcher does not know must look exactly like no autoscaler")
	})
}

// TestCapacityGate_ElasticClusterReadsStuckPodsOnly is the load bound the row calls
// for. Events are the highest-churn object in a cluster, so the mode must read them
// only for pods that have already earned a verdict — never for healthy or
// still-within-grace pods, and never at all for a set with nothing stuck.
func TestCapacityGate_ElasticClusterReadsStuckPodsOnly(t *testing.T) {
	now := time.Now()

	t.Run("a healthy set costs no Event reads at all", func(t *testing.T) {
		running := capWorkerPod("ns", "set", "worker-running", corev1.PodRunning, now.Add(-time.Hour),
			corev1.ConditionTrue, "", "")
		reader := &eventReaderStub{}

		_, c, requeue := autoscalerGate(t, now, reader, running)

		assert.Empty(t, reader.calls, "the default cost of an enabled gate on a healthy set must be zero reads")
		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Zero(t, requeue, "with nothing stuck there is no Event to wait for")
	})

	t.Run("a pod still inside the scheduling grace is not read", func(t *testing.T) {
		// Unschedulable, but only just: the grace is 5m (half of the 10m deadline).
		fresh := capWorkerPod("ns", "set", "worker-fresh", corev1.PodPending, now.Add(-time.Minute),
			corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available")
		reader := &eventReaderStub{}

		_, _, requeue := autoscalerGate(t, now, reader, fresh)

		assert.Empty(t, reader.calls,
			"an autoscaler has not had time to reach a verdict inside the grace, and reading "+
				"early would spend a read to learn nothing")
		assert.NotZero(t, requeue, "the reconciler must still come back when the pod crosses the grace")
	})

	t.Run("only the stuck pod of a mixed set is read", func(t *testing.T) {
		running := capWorkerPod("ns", "set", "worker-running", corev1.PodRunning, now.Add(-time.Hour),
			corev1.ConditionTrue, "", "")
		reader := &eventReaderStub{}

		autoscalerGate(t, now, reader, running, stuckPod("worker-stuck", now))

		assert.Equal(t, []string{"worker-stuck"}, reader.calls)
	})

	t.Run("the per-reconcile read budget is bounded", func(t *testing.T) {
		var pods []*corev1.Pod
		for i := range maxAutoscalerVerdictPodReads + 5 {
			// Staggered ages so the oldest-first ordering is observable.
			p := capWorkerPod("ns", "set", fmt.Sprintf("worker-%02d", i), corev1.PodPending,
				now.Add(-time.Duration(30-i)*time.Minute),
				corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available")
			pods = append(pods, p)
		}
		reader := &eventReaderStub{}

		_, c, _ := autoscalerGate(t, now, reader, pods...)

		assert.Len(t, reader.calls, maxAutoscalerVerdictPodReads,
			"a badly-stuck set must not turn one reconcile into an unbounded read fan-out")
		assert.Equal(t, "worker-00", reader.calls[0], "the oldest stuck pod is read first")
		require.NotNil(t, c)
		assert.Contains(t, c.Message, "not checked this pass",
			"a bounded scan must say what it skipped rather than imply it saw everything")
	})

	t.Run("one declination is enough — the scan stops there", func(t *testing.T) {
		older := stuckPod("worker-a", now)
		older.CreationTimestamp = metav1.NewTime(now.Add(-20 * time.Minute))
		newer := stuckPod("worker-b", now)
		reader := &eventReaderStub{byPod: map[string][]corev1.Event{
			"worker-a": {legacyEvent(reasonNotTriggerScaleUp, "cluster-autoscaler", caDeclineMsg, now)},
		}}

		autoscalerGate(t, now, reader, newer, older)

		assert.Equal(t, []string{"worker-a"}, reader.calls)
	})
}

// TestCapacityGate_UnrecognizedModeFailsOpen: the CRDs ship as their own chart and can
// be upgraded ahead of the AGC, so a mode a newer CRD accepts (Q407's Probe/Provision)
// can reach an older AGC. It must fail open rather than fall through to an implemented
// mode — an operator who asked to *solicit* capacity did not ask to be gated on an
// observed verdict, and quietly substituting one for the other is the class of silent
// wrong-semantics this gate's design is ordered around.
func TestCapacityGate_UnrecognizedModeFailsOpen(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn("Provision"))
	r := capReconciler(t, now, stuckPod("worker-stuck", now))

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c, "the condition is still published: the set asked for a gate, and its absence "+
		"would read as 'no gate configured'")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, v2alpha1.ReasonGateModeUnsupported, c.Reason)
	assert.Contains(t, c.Message, `"Provision"`)
}

// TestCapacityGate_TheDangerousCombinationIsUnrepresentable is the whole reason for the
// two-axis split (Q470).
//
// Before it, a RunnerSet could name its own signal, and naming SchedulerVerdict on an
// elastic cluster meant refusing jobs on pods the autoscaler was about to rescue — the
// one genuinely harmful misconfiguration this feature has, prevented only by
// documentation. Now the signal is chosen from the gateway's cluster fact, so no value a
// tenant can write produces scheduler-verdict gating where a node may still arrive.
//
// The set below does everything it can to be gated: it enables the gate and it has a
// worker pod that has been Pending and Unschedulable for well past the grace. On an
// elastic cluster it still must not gate, because nothing has said a node is not coming.
func TestCapacityGate_TheDangerousCombinationIsUnrepresentable(t *testing.T) {
	now := time.Now()
	pod := stuckPod("worker-stuck", now)

	for _, tt := range []struct {
		name string
		gw   *v2alpha1.ActionsGateway
	}{
		{"the operator declared the cluster elastic", gwElastic()},
		// The safe direction has to be the DEFAULT, not merely available: an operator who
		// has never heard of this field must not get scheduler-verdict gating by omission.
		{"clusterCapacity is unset", &v2alpha1.ActionsGateway{}},
		{"nodeAutoscaling is empty", gwAutoscaling("")},
		// An unresolvable gateway is the same question in its most degraded form.
		{"the gateway could not be resolved at all", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
			r := capReconciler(t, now, pod)
			// A reader that returns no events: no autoscaler has recorded anything.
			r.EventReader = &eventReaderStub{}

			r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), tt.gw)

			require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable),
				"precondition: the pod really is stuck, so scheduler-verdict gating WOULD fire here")
			c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status,
				"a set must not be able to gate on the scheduler's verdict where a node may still arrive")
			assert.Equal(t, v2alpha1.ReasonCapacityAvailable, c.Reason)
		})
	}
}

// TestCapacityGate_FixedClusterReadsNoEvents: the two cluster facts select genuinely
// different code paths, not the same path with different thresholds. Where nothing can
// add a node there is no autoscaler to consult, so the gate must not spend an uncached
// Event read asking — and must not requeue waiting for an answer that will never come.
func TestCapacityGate_FixedClusterReadsNoEvents(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	reader := &eventReaderStub{}
	r := capReconciler(t, now, stuckPod("worker-stuck", now))
	r.EventReader = reader

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed())

	assert.Empty(t, reader.calls, "a cluster that cannot grow has no autoscaler verdict to read")
	assert.Zero(t, requeue, "and no Event to wait for, so no re-check is owed")
	assert.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))
}

// --- the Target adapter -----------------------------------------------------

// TestRunnerSetTarget_CapacityDeclined covers the read the admission rung performs on
// every delivered job, including every fail-open path. Over-gating starves a tenant, so
// each of these must resolve to "not declined" rather than to an alarm.
func TestRunnerSetTarget_CapacityDeclined(t *testing.T) {
	declining := func(mode string) func(*v2alpha1.RunnerSet) {
		return func(r *v2alpha1.RunnerSet) {
			gateOn(mode)(r)
			meta.SetStatusCondition(&r.Status.Conditions, metav1.Condition{
				Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
				Reason: v2alpha1.ReasonPodsUnschedulable, Message: "job intake is gated: no node fits",
			})
		}
	}

	tests := []struct {
		name         string
		mut          func(*v2alpha1.RunnerSet)
		wantDeclined bool
	}{
		{"a declining gate", declining(v2alpha1.CapacityGateModeObserve), true},
		{"gate on, condition False", func(r *v2alpha1.RunnerSet) {
			gateOn(v2alpha1.CapacityGateModeObserve)(r)
			meta.SetStatusCondition(&r.Status.Conditions, metav1.Condition{
				Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionFalse,
				Reason: v2alpha1.ReasonCapacityAvailable, Message: "fine",
			})
		}, false},
		{"gate on, condition not computed yet", gateOn(v2alpha1.CapacityGateModeObserve), false},
		// The load-bearing one: a set whose mode flipped to Off must stop gating on the
		// very next delivered job, without waiting for a reconcile to retract a
		// condition it is still carrying.
		{"mode Off with a stale True condition", func(r *v2alpha1.RunnerSet) {
			declining(v2alpha1.CapacityGateModeObserve)(r)
			r.Spec.CapacityGate.Mode = v2alpha1.CapacityGateModeOff
		}, false},
		{"no capacity gate at all", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := rsObj("set", "ns", tt.mut)
			c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
			target := &runnerSetTarget{client: c, key: keyOf(rs)}

			declined, detail := target.CapacityDeclined(context.Background())

			assert.Equal(t, tt.wantDeclined, declined)
			if tt.wantDeclined {
				assert.Contains(t, detail, "no node fits", "the detail must carry the signal into the rejection log")
			} else {
				assert.Empty(t, detail)
			}
		})
	}

	t.Run("an unreadable RunnerSet fails open", func(t *testing.T) {
		// No objects in the client: the Get 404s exactly as a lost cache would.
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rsObj("set", "ns", nil))}

		declined, _ := target.CapacityDeclined(context.Background())

		assert.False(t, declined, "a set the AGC cannot read must never gate intake")
	})
}

// TestRunnerSetTarget_DeclinedCapacity pins the delta-to-total conversion for the
// scale-set tier: a declining gate advertises exactly the set's own in-flight worker
// pods, so GitHub keeps what it has assigned and is offered nothing more.
func TestRunnerSetTarget_DeclinedCapacity(t *testing.T) {
	ctx := context.Background()
	declining := func(r *v2alpha1.RunnerSet) {
		gateOn(v2alpha1.CapacityGateModeObserve)(r)
		meta.SetStatusCondition(&r.Status.Conditions, metav1.Condition{
			Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
			Reason: v2alpha1.ReasonPodsUnschedulable, Message: "gated",
		})
	}

	t.Run("bounds at the set's own in-flight pods", func(t *testing.T) {
		rs := rsObj("set", "ns", declining)
		running := capWorkerPod("ns", "set", "w1", corev1.PodRunning, time.Now(), corev1.ConditionTrue, "", "")
		pending := capWorkerPod("ns", "set", "w2", corev1.PodPending, time.Now(), corev1.ConditionFalse, corev1.PodReasonUnschedulable, "no fit")
		// A sibling set's pod must not inflate this set's bound.
		other := capWorkerPod("ns", "other", "w3", corev1.PodRunning, time.Now(), corev1.ConditionTrue, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, running, pending, other).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Equal(t, int32(2), limit)
	})

	t.Run("nothing in flight advertises zero", func(t *testing.T) {
		rs := rsObj("set", "ns", declining)
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Zero(t, limit)
	})

	t.Run("never advertises above the caller's cap", func(t *testing.T) {
		rs := rsObj("set", "ns", declining)
		objs := []client.Object{rs}
		for _, n := range []string{"w1", "w2", "w3"} {
			objs = append(objs, capWorkerPod("ns", "set", n, corev1.PodRunning, time.Now(), corev1.ConditionTrue, "", ""))
		}
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 2)

		assert.True(t, bounded)
		assert.Equal(t, int32(2), limit, "a rung may only ever LOWER what the earlier rungs left")
	})

	t.Run("a gate that is not declining imposes no bound", func(t *testing.T) {
		rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		_, bounded := target.DeclinedCapacity(ctx, 10)

		assert.False(t, bounded, "fail-open here means the ceiling and quota rungs stand alone")
	})
}

// latchedSet returns a RunnerSet mutator seeding the latched condition (Q512) with a
// controlled transition time, so a test can place pods before or after the decline.
func latchedSet(declinedSince time.Time) func(*v2alpha1.RunnerSet) {
	return func(r *v2alpha1.RunnerSet) {
		gateOn(v2alpha1.CapacityGateModeObserve)(r)
		r.Status.Conditions = append(r.Status.Conditions, metav1.Condition{
			Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
			Reason: v2alpha1.ReasonAwaitingProbe, Message: "job intake is limited to one probe job",
			LastTransitionTime: metav1.NewTime(declinedSince),
		})
	}
}

// TestRunnerSetTarget_LatchedDeclinedCapacity pins the probe slot (Q512): a latched
// gate advertises the set's in-flight pods plus exactly one, and only while no probe
// pod is outstanding — the integer tier's one-claim-per-window trickle.
func TestRunnerSetTarget_LatchedDeclinedCapacity(t *testing.T) {
	ctx := context.Background()
	declinedSince := time.Now().Add(-time.Hour)

	t.Run("an empty set advertises exactly the probe slot", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Equal(t, int32(1), limit, "the latch never closes intake — its floor is one probe job")
	})

	t.Run("an outstanding probe closes the slot", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		probe := capWorkerPod("ns", "set", "probe", corev1.PodPending, time.Now(), corev1.ConditionFalse, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, probe).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Equal(t, int32(1), limit, "one probe per window: the outstanding probe IS the slot")
	})

	t.Run("a reaped probe reopens the slot", func(t *testing.T) {
		// The probe went terminal without the reconciler clearing the latch — the
		// next window's probe must still be admitted or the trickle stalls.
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		done := capWorkerPod("ns", "set", "probe-done", corev1.PodFailed, time.Now(), corev1.ConditionFalse, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, done).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Equal(t, int32(1), limit)
	})

	t.Run("pre-decline workers keep their slots plus the probe", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		objs := []client.Object{rs}
		for _, n := range []string{"w1", "w2"} {
			objs = append(objs, capWorkerPod("ns", "set", n, corev1.PodRunning, declinedSince.Add(-time.Hour), corev1.ConditionTrue, "", ""))
		}
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(objs...).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 10)

		assert.True(t, bounded)
		assert.Equal(t, int32(3), limit, "GitHub keeps its running jobs; only one probe rides on top")
	})

	t.Run("the probe slot never exceeds the caller's cap", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		w := capWorkerPod("ns", "set", "w1", corev1.PodRunning, declinedSince.Add(-time.Hour), corev1.ConditionTrue, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, w).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		limit, bounded := target.DeclinedCapacity(ctx, 1)

		assert.True(t, bounded)
		assert.Equal(t, int32(1), limit, "a rung may only ever LOWER what the earlier rungs left")
	})
}

// TestRunnerSetTarget_LatchedCapacityDeclined pins the classic tier's half of the
// probe slot (Q512): a latched gate declines every delivery EXCEPT the one that
// becomes the probe — without it, a latched classic set would starve, since no probe
// pod could ever exist to resolve the latch.
func TestRunnerSetTarget_LatchedCapacityDeclined(t *testing.T) {
	ctx := context.Background()
	declinedSince := time.Now().Add(-time.Hour)

	t.Run("no probe outstanding admits the probe", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		declined, _ := target.CapacityDeclined(ctx)

		assert.False(t, declined, "this delivery becomes the probe")
	})

	t.Run("an outstanding probe declines the rest of the window", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		probe := capWorkerPod("ns", "set", "probe", corev1.PodPending, time.Now(), corev1.ConditionFalse, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, probe).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		declined, detail := target.CapacityDeclined(ctx)

		assert.True(t, declined)
		assert.Contains(t, detail, "one probe job")
	})

	t.Run("pre-decline workers are not probes", func(t *testing.T) {
		rs := rsObj("set", "ns", latchedSet(declinedSince))
		w := capWorkerPod("ns", "set", "w1", corev1.PodRunning, declinedSince.Add(-time.Hour), corev1.ConditionTrue, "", "")
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs, w).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		declined, _ := target.CapacityDeclined(ctx)

		assert.False(t, declined, "a running pre-decline worker must not be mistaken for the probe")
	})
}

// keyOf is the object's namespace/name as the Target adapter stores it.
func keyOf(rs *v2alpha1.RunnerSet) client.ObjectKey {
	return client.ObjectKey{Namespace: rs.Namespace, Name: rs.Name}
}

// --- the kubelet's startup verdict (Q714) ----------------------------------

// TestCapacityGate_DeclinesOnAWorkerThatBoundAndCouldNotStart is Q714's headline
// assertion. `podUnschedulable` reads PodScheduled=False, so a worker that binds to a
// node and then cannot pull its image trips no rung at all: it sits Pending until the
// reaper deletes it at pendingPodDeadline, and every job delivered in that window is
// claimed, spending a single-use JIT runner record and holding a GitHub job lock.
//
// It asserts BOTH cluster facts, which is the design point rather than table padding.
// The other two signals are selected by clusterCapacity.nodeAutoscaling because an
// unschedulable pod may be the request that makes a node appear. A bound pod is not a
// request for anything, so no autoscaler is waiting on it and the verdict is sound
// wherever it is read.
func TestCapacityGate_DeclinesOnAWorkerThatBoundAndCouldNotStart(t *testing.T) {
	const image = `Back-off pulling image "example.invalid/build-capable-runner:replace-me"`

	for _, tt := range []struct {
		name string
		gw   *v2alpha1.ActionsGateway
	}{
		{"fixed-size cluster", gwFixed()},
		{"elastic cluster", gwElastic()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
				now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", image)
			rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
			r := capReconciler(t, now, pod)

			r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), tt.gw)

			c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, v2alpha1.ReasonPodsNotStarting, c.Reason)
			assert.Contains(t, c.Message, "worker-backoff")
			assert.Contains(t, c.Message, "example.invalid",
				"the kubelet's own message names the image, which is the operator's whole remedy")

			// The sibling condition must stay silent: this pod is not a scheduling
			// problem, and reporting it there would send an operator after a node, a
			// taint, or a quota when the fix is an image.
			unsched := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable)
			require.NotNil(t, unsched)
			assert.Equal(t, metav1.ConditionFalse, unsched.Status)
			assert.Equal(t, v2alpha1.ReasonWorkersSchedulable, unsched.Reason)
		})
	}
}

// TestCapacityGate_StartupVerdictNeedsNoSchedulingGrace: the scheduling grace is half
// pendingPodDeadline so WorkersUnschedulable cannot be set in the same pass the reaper
// deletes its evidence (Q95). Copying that number here would be wrong in both
// directions — pendingPodDeadline is deliberately generous FOR image pulls, so half of
// it would admit minutes of doomed claims, while the kubelet reaches its verdict in
// about two seconds (§9j).
//
// The backoff state is this signal's grace instead: it is the kubelet's conclusion
// after an attempt, not a snapshot the next moment can overturn. So a pod far inside
// the scheduling grace still gates.
func TestCapacityGate_StartupVerdictNeedsNoSchedulingGrace(t *testing.T) {
	now := time.Now()
	// Created one second ago against a ten-minute deadline: nowhere near the
	// five-minute grace an unschedulable pod would have to sit out.
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-fresh", corev1.PodPending,
		now.Add(-time.Second), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	r := capReconciler(t, now, pod)

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, v2alpha1.ReasonPodsNotStarting, c.Reason)
}

// TestCapacityGate_StartupVerdictIgnoresAPreBackoffFailure: ErrImagePull is one failed
// pull, which a registry blip produces and the next attempt clears. Gating on it would
// be gating on the attempt rather than the kubelet's conclusion — and since excluding
// it is the whole of this signal's grace, an accidental widening here would remove the
// grace silently.
func TestCapacityGate_StartupVerdictIgnoresAPreBackoffFailure(t *testing.T) {
	now := time.Now()
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-firsttry", corev1.PodPending,
		now.Add(-time.Second), corev1.ConditionTrue, "", ""), "ErrImagePull", "failed to pull image")
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	r := capReconciler(t, now, pod)

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "one failed pull is not the kubelet's verdict")
	assert.Equal(t, v2alpha1.ReasonCapacityAvailable, c.Reason)
	assert.Equal(t, startupVerdictRecheck, requeue,
		"the pod is bound and undecided, so the gate must come back for the verdict")
}

// TestCapacityGate_StartupVerdictSchedulesItsOwnRecheck: the Pod watch drops updates
// that change no phase, and a pod entering ImagePullBackOff changes none. Without a
// re-check the gate would first learn of an unpullable image when the reaper deleted
// the pod ten minutes later — the deadline this rung exists to stop jobs waiting out.
func TestCapacityGate_StartupVerdictSchedulesItsOwnRecheck(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))

	// Bound, Pending, no verdict either way: the window the watch cannot see through.
	starting := capWorkerPod("ns", "set", "worker-starting", corev1.PodPending, now, corev1.ConditionTrue, "", "")
	r := capReconciler(t, now, starting)
	assert.Equal(t, startupVerdictRecheck,
		r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwFixed()))

	// Once it has started there is nothing left to come back for: the phase change to
	// Running is an event the watch does deliver.
	started := capStarted(capWorkerPod("ns", "set", "worker-started", corev1.PodPending, now,
		corev1.ConditionTrue, "", ""), now)
	rs2 := rsObj("set2", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	r2 := capReconciler(t, now, started)
	assert.Zero(t, r2.applyWorkerCapacityConditions(context.Background(), rs2, capTemplate(""), gwFixed()))
}

// TestCapacityGate_StartupVerdictIsOffByDefault: the gate is opt-in, so a set that
// never set spec.capacityGate must carry no condition at all even with a worker pod
// wedged in backoff — the same no-cost-for-the-default contract every other signal
// keeps.
func TestCapacityGate_StartupVerdictIsOffByDefault(t *testing.T) {
	now := time.Now()
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	rs := rsObj("set", "ns", func(r *v2alpha1.RunnerSet) {
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	})
	r := capReconciler(t, now, pod)

	requeue := r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))
	assert.Zero(t, requeue, "an ungated set must not pay a re-check for a signal it never asked for")
}

// TestCapacityGate_StartupVerdictLatchesAndTrickles is the end-to-end rate property
// for this signal: decline, reap, latch, one probe, and the live verdict re-earned
// when that probe sticks. It is the §9e no-op check in miniature — a gate that cleared
// on the reap would restore the whole advertisement once per deadline window, and a
// burst of N wasted claims would stay N.
func TestCapacityGate_StartupVerdictLatchesAndTrickles(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeObserve))
	pod := capBackingOff(capWorkerPod("ns", "set", "worker-backoff", corev1.PodPending,
		now.Add(-time.Minute), corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())
	require.Equal(t, v2alpha1.ReasonPodsNotStarting,
		meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined).Reason)

	// The reaper deletes the gate's own evidence at pendingPodDeadline.
	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())
	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, v2alpha1.ReasonAwaitingProbe, c.Reason)
	assert.Contains(t, c.Message, v2alpha1.ReasonPodsNotStarting,
		"the reaped verdict must survive into the latched message, or the operator loses the image")

	// The probe job's pod carries the same unreplaced image and sticks the same way.
	probe := capBackingOff(capWorkerPod("ns", "set", "worker-probe", corev1.PodPending, now,
		corev1.ConditionTrue, "", ""), "ImagePullBackOff", "Back-off pulling image")
	probe.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(time.Minute))
	require.NoError(t, r.Create(context.Background(), probe))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""), gwElastic())
	c = meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, v2alpha1.ReasonPodsNotStarting, c.Reason,
		"a probe that sticks must return the live verdict, not sit latched")
}
