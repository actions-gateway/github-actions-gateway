package controller

import (
	"context"
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

// gateOn returns a RunnerSet mutator that opts the set into a capacity-gate mode.
func gateOn(mode string) func(*v2alpha1.RunnerSet) {
	return func(r *v2alpha1.RunnerSet) {
		r.Spec.CapacityGate = &v2alpha1.CapacityGate{Mode: mode}
		r.Spec.PendingPodDeadline = &metav1.Duration{Duration: 10 * time.Minute}
	}
}

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

			r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))

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

// TestCapacityGate_SchedulerVerdictDeclinesOnAnUnschedulablePod is the Phase 1 signal
// path: the scheduler's own verdict, already published as WorkersUnschedulable,
// becomes the intake decision.
func TestCapacityGate_SchedulerVerdictDeclinesOnAnUnschedulablePod(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	rec := events.NewFakeRecorder(16)
	r := capReconciler(t, now, pod)
	r.Recorder = rec

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))

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

// TestCapacityGate_ClearsWhenThePodIsReaped is the trickle property at the condition
// layer: the gate is derived from the existence of a stuck pod, so the reaper deleting
// that pod at pendingPodDeadline is what reopens intake. Without this the first close
// would be permanent and the mode would starve a tenant rather than throttle them.
func TestCapacityGate_ClearsWhenThePodIsReaped(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")

	r := capReconciler(t, now, pod)
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	// The reaper deletes the stuck pod; nothing else about the set changes.
	require.NoError(t, r.Delete(context.Background(), pod))
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))

	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "reaping the stuck pod must reopen intake")
	assert.Equal(t, v2alpha1.ReasonCapacityAvailable, c.Reason)
}

// TestCapacityGate_OptingOutRetractsTheCondition: a set that turns the gate off while
// it is declining must not leave a True condition behind — the rung reads that
// condition, so a stale True would keep intake gated forever.
func TestCapacityGate_OptingOutRetractsTheCondition(t *testing.T) {
	now := time.Now()
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict))
	pod := capWorkerPod("ns", "set", "worker-stuck", corev1.PodPending, now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "untolerated taint")
	r := capReconciler(t, now, pod)

	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))
	require.True(t, meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))

	rs.Spec.CapacityGate.Mode = v2alpha1.CapacityGateModeOff
	r.applyWorkerCapacityConditions(context.Background(), rs, capTemplate(""))

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined),
		"opting out must retract the condition, not leave a stale True gating intake")
}

// TestCapacityGate_ClearedWhenReferencesDoNotResolve: with no listeners running and no
// worker pods being provisioned, a lingering True would gate a set that is already
// stopped for a louder reason.
func TestCapacityGate_ClearedWhenReferencesDoNotResolve(t *testing.T) {
	rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict))
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionTrue,
		Reason: v2alpha1.ReasonPodsUnschedulable, Message: "stale",
	})
	r := capReconciler(t, time.Now())

	r.clearWorkerCapacityConditions(rs)

	assert.Nil(t, meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined))
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
		{"a declining gate", declining(v2alpha1.CapacityGateModeSchedulerVerdict), true},
		{"gate on, condition False", func(r *v2alpha1.RunnerSet) {
			gateOn(v2alpha1.CapacityGateModeSchedulerVerdict)(r)
			meta.SetStatusCondition(&r.Status.Conditions, metav1.Condition{
				Type: v2alpha1.ConditionWorkerCapacityDeclined, Status: metav1.ConditionFalse,
				Reason: v2alpha1.ReasonCapacityAvailable, Message: "fine",
			})
		}, false},
		{"gate on, condition not computed yet", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict), false},
		// The load-bearing one: a set whose mode flipped to Off must stop gating on the
		// very next delivered job, without waiting for a reconcile to retract a
		// condition it is still carrying.
		{"mode Off with a stale True condition", func(r *v2alpha1.RunnerSet) {
			declining(v2alpha1.CapacityGateModeSchedulerVerdict)(r)
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
		gateOn(v2alpha1.CapacityGateModeSchedulerVerdict)(r)
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
		rs := rsObj("set", "ns", gateOn(v2alpha1.CapacityGateModeSchedulerVerdict))
		c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
		target := &runnerSetTarget{client: c, key: keyOf(rs)}

		_, bounded := target.DeclinedCapacity(ctx, 10)

		assert.False(t, bounded, "fail-open here means the ceiling and quota rungs stand alone")
	})
}

// keyOf is the object's namespace/name as the Target adapter stores it.
func keyOf(rs *v2alpha1.RunnerSet) client.ObjectKey {
	return client.ObjectKey{Namespace: rs.Namespace, Name: rs.Name}
}
