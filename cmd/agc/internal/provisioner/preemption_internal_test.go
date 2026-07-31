package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q497: scheduler preemption reaches the same automatic re-run a kubelet eviction does.
//
// Q423 measured that it did not: a priorityTiers preemption deletes its victim rather
// than failing it, so neither tier's Evicted-shaped detection saw anything and the
// displaced run needed a manual re-run. These tests pin the discriminator that closes
// it, both tiers' use of it, and — as importantly — the removals it must still NOT fire
// on, since over-firing would re-run work an operator deliberately stopped.

// drained marks a pod as a graceful external removal (a drain, a `kubectl delete pod`)
// leaves it when the kubelet's terminal-phase update wins the race against the object's
// removal — the shape Q459 measured at live GitHub: PodFailed with an empty reason, the
// deletion mark, and a container exit after the delete was issued.
func drained(pod *corev1.Pod) *corev1.Pod {
	delTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
	pod.DeletionTimestamp = &delTime
	pod.Finalizers = []string{"test.actions-gateway.com/hold"} // fake client rejects a delete-marked object without one
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			FinishedAt: metav1.NewTime(delTime.Add(5 * time.Second)),
		}},
	}}
	return pod
}

// preempted marks a pod as kube-scheduler does when it selects it as a preemption
// victim: the DisruptionTarget condition, plus the deletionTimestamp from the delete
// that immediately follows.
func preempted(pod *corev1.Pod) *corev1.Pod {
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"test.actions-gateway.com/hold"} // fake client rejects a delete-marked object without one
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonPreemptionByScheduler,
	})
	return pod
}

// TestPreemptedByScheduler_RequiresTheFullTriple pins the discriminator itself. The
// whole safety argument for recovering this path rests on PreemptionByScheduler having
// exactly one writer, so anything short of the full type/status/reason triple must read
// as "not a preemption" — a near-miss that returned true would recover removals that are
// deliberately out of scope (Q459's drain and human-cancel slice).
func TestPreemptedByScheduler_RequiresTheFullTriple(t *testing.T) {
	cond := func(t corev1.PodConditionType, s corev1.ConditionStatus, reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: t, Status: s, Reason: reason}
	}
	tests := []struct {
		name       string
		conditions []corev1.PodCondition
		want       bool
	}{
		{name: "no conditions"},
		{
			name:       "the preemption marker",
			conditions: []corev1.PodCondition{cond(corev1.DisruptionTarget, corev1.ConditionTrue, corev1.PodReasonPreemptionByScheduler)},
			want:       true,
		},
		{
			name: "found among unrelated conditions",
			conditions: []corev1.PodCondition{
				cond(corev1.PodScheduled, corev1.ConditionTrue, ""),
				cond(corev1.PodReady, corev1.ConditionFalse, ""),
				cond(corev1.DisruptionTarget, corev1.ConditionTrue, corev1.PodReasonPreemptionByScheduler),
			},
			want: true,
		},
		{
			// The kubelet's own graceful termination shares the condition type. It is
			// NOT a preemption and must not be recovered here.
			name:       "DisruptionTarget from the kubelet",
			conditions: []corev1.PodCondition{cond(corev1.DisruptionTarget, corev1.ConditionTrue, corev1.PodReasonTerminationByKubelet)},
		},
		{
			// The eviction API (a drain, a PDB-mediated eviction) also writes
			// DisruptionTarget, with its own reason. That is Q459's slice, not this one.
			name:       "DisruptionTarget from the eviction API",
			conditions: []corev1.PodCondition{cond(corev1.DisruptionTarget, corev1.ConditionTrue, "EvictionByEvictionAPI")},
		},
		{
			name:       "right reason on the wrong condition type",
			conditions: []corev1.PodCondition{cond(corev1.PodScheduled, corev1.ConditionTrue, corev1.PodReasonPreemptionByScheduler)},
		},
		{
			name:       "status False",
			conditions: []corev1.PodCondition{cond(corev1.DisruptionTarget, corev1.ConditionFalse, corev1.PodReasonPreemptionByScheduler)},
		},
		{
			name:       "status Unknown",
			conditions: []corev1.PodCondition{cond(corev1.DisruptionTarget, corev1.ConditionUnknown, corev1.PodReasonPreemptionByScheduler)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: tc.conditions}}
			assert.Equal(t, tc.want, PreemptedByScheduler(pod))
		})
	}
}

// TestDisruptionAwaitingRecovery covers the scale-set scan's filter: which pods it picks
// up, with which cause, and — the load-bearing half — which removals it still leaves
// alone: the AGC's own reaper deletions, cleanup of already-failed pods, ordinary
// failures (which a human cancel is indistinguishable from at the pod — no deletion
// mark, measured by Q459), and pods deleted without ever reaching a terminal phase.
func TestDisruptionAwaitingRecovery(t *testing.T) {
	deleted := func(pod *corev1.Pod) *corev1.Pod {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		return pod
	}
	handled := func(pod *corev1.Pod) *corev1.Pod {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationEvictionHandledAt] = "2026-07-29T00:00:00Z"
		return pod
	}
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantCause string
		wantOK    bool
	}{
		{
			name:      "kubelet eviction",
			pod:       evicted(scaleSetWorkerPod("p", identityAnnotations())),
			wantCause: recoveryCauseEviction, wantOK: true,
		},
		{
			name:      "scheduler preemption",
			pod:       preempted(scaleSetWorkerPod("p", identityAnnotations())),
			wantCause: recoveryCausePreemption, wantOK: true,
		},
		{
			// The terminal phase on the preemption path is the interrupted container's
			// own exit status, so a preempted victim can land in Succeeded (Q423
			// measured exactly that). The condition, not the phase, is what decides.
			name: "preemption whose container exited 0",
			pod: func() *corev1.Pod {
				pod := preempted(scaleSetWorkerPod("p", identityAnnotations()))
				pod.Status.Phase = corev1.PodSucceeded
				return pod
			}(),
			wantCause: recoveryCausePreemption, wantOK: true,
		},
		{
			name: "running worker, untouched",
			pod: func() *corev1.Pod {
				pod := scaleSetWorkerPod("p", identityAnnotations())
				pod.Status.Phase = corev1.PodRunning
				return pod
			}(),
		},
		{
			// Deleted without ever publishing a terminal phase — nothing ran to a
			// reportable end, so there is no failed job to re-run. This is also the
			// shape the reaper's pending-deadline deletions take.
			name: "gracefully deleted without a terminal phase",
			pod:  deleted(scaleSetWorkerPod("p", identityAnnotations())),
		},
		{
			// The Q502 arm: an external graceful deletion whose victim reached Failed
			// before the object went away. The mark predates the container exit, which
			// is what separates a disrupting delete from a later cleanup.
			name:      "drained worker that reached a terminal phase",
			pod:       drained(scaleSetWorkerPod("p", identityAnnotations())),
			wantCause: recoveryCauseDeletion, wantOK: true,
		},
		{
			// The reaper stamps its own deletions; recovering them would re-run every
			// job the AGC itself gave up on (Q502's reaper exclusion).
			name: "reaper-deleted worker that reached a terminal phase",
			pod: func() *corev1.Pod {
				pod := drained(scaleSetWorkerPod("p", identityAnnotations()))
				pod.Annotations[AnnotationDeletionReason] = "orphaned_running"
				return pod
			}(),
		},
		{
			// An operator cleaning up an already-failed pod: the delete postdates the
			// container exit, so it is cleanup, not a disruption — re-running here
			// would re-run genuinely failing work.
			name: "failed pod deleted later by an operator",
			pod: func() *corev1.Pod {
				pod := drained(scaleSetWorkerPod("p", identityAnnotations()))
				pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt =
					metav1.NewTime(pod.DeletionTimestamp.Add(-time.Hour))
				return pod
			}(),
		},
		{
			// An ordinary job failure. Recovering it would re-run genuinely failing work.
			name: "failed with no disruption reason",
			pod: func() *corev1.Pod {
				pod := scaleSetWorkerPod("p", identityAnnotations())
				pod.Status.Phase = corev1.PodFailed
				return pod
			}(),
		},
		{
			name: "already-claimed eviction",
			pod:  handled(evicted(scaleSetWorkerPod("p", identityAnnotations()))),
		},
		{
			// At-most-once must hold for preemption too, or a second reconcile inside the
			// victim's grace period would spend another slot of the run's budget.
			name: "already-claimed preemption",
			pod:  handled(preempted(scaleSetWorkerPod("p", identityAnnotations()))),
		},
		{
			// And for the drain arm, whose victim stays readable until the kubelet
			// finishes tearing it down.
			name: "already-claimed drain",
			pod:  handled(drained(scaleSetWorkerPod("p", identityAnnotations()))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cause, ok := disruptionAwaitingRecovery(tc.pod)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantCause, cause)
		})
	}
}

// TestRecoverScaleSetWorkers_RerunsThePreemptedRun is the scale-set half of the fix:
// the reconciler's scan finds a preempted worker inside its termination grace period and
// re-runs the displaced run, attributed to the preemption cause so an operator does not
// read it as node pressure.
func TestRecoverScaleSetWorkers_RerunsThePreemptedRun(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, paths := recoveryFixture(t,
		preempted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	require.Equal(t, int64(1), rerunCount.Load(), "the preempted run must be re-run exactly once")
	select {
	case path := <-paths:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", path)
	default:
		t.Fatal("rerun API path was not recorded")
	}

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCausePreemption)),
		"a preemption recovery must be attributed to the preemption cause")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction)),
		"a preemption must not be counted as node-pressure eviction; the two demand different operator responses")

	// Claimed like any other recovery, so a second reconcile inside the grace period is
	// a no-op rather than a second slot of the run's budget.
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-job1"}, &pod))
	assert.Contains(t, pod.Annotations, AnnotationEvictionHandledAt,
		"the preempted pod must be stamped as handled")
}

// TestRecoverScaleSetWorkers_PreemptionIsAtMostOnce pins the claim across repeated
// scans. A preempted pod stays readable for its whole termination grace period, and the
// worker-pod watch fires more than once in that window (the condition update, then the
// phase change as the container exits), so the scan really does see the same victim
// again — and must not re-run it again.
func TestRecoverScaleSetWorkers_PreemptionIsAtMostOnce(t *testing.T) {
	ctx := context.Background()
	p, target, _, rerunCount, _ := recoveryFixture(t,
		preempted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	for i := 0; i < 3; i++ {
		done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
		require.NoError(t, err)
		<-done
	}

	assert.Equal(t, int64(1), rerunCount.Load(),
		"repeated scans of one preempted pod must re-run its run exactly once")
}

// TestRecoverScaleSetWorkers_DeleteWithoutTerminalPhaseIsNotRecovered pins the boundary
// of the Q502 arm: a deletion whose victim never published a terminal phase never ran
// its job to a reportable end, so there is no failed job for rerun-failed-jobs to act
// on. This is also the shape the reaper's pending-deadline deletions take.
func TestRecoverScaleSetWorkers_DeleteWithoutTerminalPhaseIsNotRecovered(t *testing.T) {
	ctx := context.Background()
	pod := scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"test.actions-gateway.com/hold"}

	p, target, _, rerunCount, _ := recoveryFixture(t, pod)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(),
		"a deletion with no terminal phase must not be re-run automatically")
}

// TestRecoverScaleSetWorkers_RerunsTheDrainedRun is the scale-set half of Q502: the
// scan finds a drained worker inside its teardown window — PodFailed with the deletion
// mark, no scheduler condition — and re-runs the interrupted run, attributed to the
// deletion cause so an operator does not read it as node pressure or preemption.
func TestRecoverScaleSetWorkers_RerunsTheDrainedRun(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, paths := recoveryFixture(t,
		drained(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	require.Equal(t, int64(1), rerunCount.Load(), "the drained run must be re-run exactly once")
	select {
	case path := <-paths:
		assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", path)
	default:
		t.Fatal("rerun API path was not recorded")
	}

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseDeletion)),
		"a drain recovery must be attributed to the deletion cause")

	// Claimed like any other recovery, so the further reconciles the teardown window
	// produces are no-ops rather than more slots of the run's budget.
	var pod corev1.Pod
	require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "runner-gpu-job1"}, &pod))
	assert.Contains(t, pod.Annotations, AnnotationEvictionHandledAt,
		"the drained pod must be stamped as handled")
}

// TestRecoverScaleSetWorkers_ReaperDeletionIsNotRecovered is the exclusion the Q459
// plan calls out by name: the reaper's own deletions publish the same
// Failed-with-deletion-mark shape a drain does, and recovering them would re-run every
// job the AGC itself gave up on. The AnnotationDeletionReason stamp the reaper writes
// before deleting is what keeps them apart.
func TestRecoverScaleSetWorkers_ReaperDeletionIsNotRecovered(t *testing.T) {
	ctx := context.Background()
	pod := drained(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations()))
	pod.Annotations[AnnotationDeletionReason] = "orphaned_running"

	p, target, _, rerunCount, _ := recoveryFixture(t, pod)

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(0), rerunCount.Load(),
		"the reaper's own deletions must never trigger a re-run")
}
