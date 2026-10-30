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

// shutDown marks a pod as a node shutdown leaves it (Q1179): PodFailed and a
// DisruptionTarget with reason — TerminationByKubelet from the kubelet's shutdown
// manager, which also sets Status.Reason "Terminated", or DeletionByPodGC once the node
// is gone.
func shutDown(pod *corev1.Pod, reason string) *corev1.Pod {
	pod.Status.Phase = corev1.PodFailed
	if reason == corev1.PodReasonTerminationByKubelet {
		pod.Status.Reason = "Terminated"
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: reason,
	})
	return pod
}

// ranRunner records the runner container as having run and been killed, as the
// kubelet does for a worker whose job was cut off.
func ranRunner(pod *corev1.Pod) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  WorkerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}},
	}}
	return pod
}

// outOfService marks a pod as a dead node's out-of-service taint leaves it (Q1207): the
// taint manager's DisruptionTarget and graceful delete, then PodFailed from PodGC over a
// container no kubelet ever recorded stopping.
func outOfService(pod *corev1.Pod) *corev1.Pod {
	delTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
	pod.DeletionTimestamp = &delTime
	pod.Finalizers = []string{"test.actions-gateway.com/hold"} // fake client rejects a delete-marked object without one
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "runner",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(delTime.Add(-time.Minute))}},
	}}
	return shutDown(pod, podReasonDeletionByTaintManager)
}

// TestLostToNodeShutdown pins the node-shutdown discriminator: both writers' reasons, on
// a failed pod, with a true status — and nothing else that shares the condition type.
func TestLostToNodeShutdown(t *testing.T) {
	cond := func(s corev1.ConditionStatus, reason string) []corev1.PodCondition {
		return []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: s, Reason: reason}}
	}
	status := func(st corev1.ContainerState) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: "runner", State: st}}
	}
	running := status(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	terminated := status(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}})
	waiting := status(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}})
	tests := []struct {
		name       string
		phase      corev1.PodPhase
		conditions []corev1.PodCondition
		containers []corev1.ContainerStatus
		agcDeleted bool
		want       bool
	}{
		{name: "failed, no conditions", phase: corev1.PodFailed},
		{name: "kubelet shutdown", phase: corev1.PodFailed, conditions: cond(corev1.ConditionTrue, corev1.PodReasonTerminationByKubelet), containers: terminated, want: true},
		{name: "PodGC orphan", phase: corev1.PodFailed, conditions: cond(corev1.ConditionTrue, podReasonDeletionByPodGC), containers: running, want: true},
		{
			// Q1198: the kubelet fails a still-Pending worker too. It ran no job, so it is
			// the abandoned handling's (LostToNodeShutdownBeforeStart).
			name: "kubelet shutdown, worker never started", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, corev1.PodReasonTerminationByKubelet), containers: waiting,
		},
		{
			name: "PodGC orphan, worker never started", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByPodGC),
		},
		{
			// PodGC failed the pod over a dead kubelet: the container is still recorded Running.
			name: "out-of-service node", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager), containers: running, want: true,
		},
		{
			// A regular sidecar (dind) outlived the runner, whose job had already ended:
			// only the runner's state says whether a job was cut off.
			name: "out-of-service node, runner exited before, sidecar still running", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager),
			containers: append(append([]corev1.ContainerStatus{}, terminated...), corev1.ContainerStatus{
				Name: "dind", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}),
		},
		{
			// The AGC deleted this worker itself (reaper, job_abandoned) before the node
			// died; its own deletions are never a disruption.
			name: "out-of-service node, AGC-deleted worker", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager), containers: running,
			agcDeleted: true,
		},
		{
			name: "PodGC orphan of an AGC-deleted worker", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByPodGC), agcDeleted: true,
		},
		{
			// A live kubelet finishing the taint manager's delete records the exit first;
			// that pod is the deletion arm's.
			name: "NoExecute taint on a live node", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager), containers: terminated,
		},
		{
			// Never started: the abandoned handling's, not a re-run of a job that never ran.
			name: "out-of-service node, worker never started", phase: corev1.PodFailed,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager), containers: waiting,
		},
		{
			// A NotReady node with no out-of-service taint: the delete never completes.
			name: "taint-manager delete stuck terminating", phase: corev1.PodRunning,
			conditions: cond(corev1.ConditionTrue, podReasonDeletionByTaintManager), containers: running,
		},
		{
			// The kubelet leaves a Succeeded phase alone: the job finished and reported.
			name: "kubelet shutdown after the job succeeded", phase: corev1.PodSucceeded,
			conditions: cond(corev1.ConditionTrue, corev1.PodReasonTerminationByKubelet),
		},
		{name: "still running", phase: corev1.PodRunning, conditions: cond(corev1.ConditionTrue, corev1.PodReasonTerminationByKubelet)},
		{name: "status False", phase: corev1.PodFailed, conditions: cond(corev1.ConditionFalse, corev1.PodReasonTerminationByKubelet)},
		{name: "scheduler preemption", phase: corev1.PodFailed, conditions: cond(corev1.ConditionTrue, corev1.PodReasonPreemptionByScheduler)},
		{name: "eviction API", phase: corev1.PodFailed, conditions: cond(corev1.ConditionTrue, "EvictionByEvictionAPI")},
		{
			name: "right reason on the wrong condition type", phase: corev1.PodFailed,
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: corev1.PodReasonTerminationByKubelet}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tc.phase, Conditions: tc.conditions, ContainerStatuses: tc.containers}}
			if tc.agcDeleted {
				pod.Annotations = map[string]string{AnnotationDeletionReason: "job_abandoned"}
			}
			assert.Equal(t, tc.want, LostToNodeShutdown(pod))
		})
	}
}

// TestLostToNodeShutdownBeforeStart pins the never-started half of the node-loss shape
// (Q1198): every node-loss reason on a failed pod no container of which started, and
// nothing LostToNodeShutdown takes, so no pod reaches both a re-run and a force-cancel.
func TestLostToNodeShutdownBeforeStart(t *testing.T) {
	cond := func(reason string) []corev1.PodCondition {
		return []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reason}}
	}
	status := func(st corev1.ContainerState) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: "runner", State: st}}
	}
	running := status(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	terminated := status(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}})
	waiting := status(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}})
	tests := []struct {
		name       string
		phase      corev1.PodPhase
		reason     string
		conditions []corev1.PodCondition
		containers []corev1.ContainerStatus
		agcDeleted bool
		want       bool
	}{
		{name: "kubelet shutdown while pulling", phase: corev1.PodFailed, reason: "Terminated", conditions: cond(corev1.PodReasonTerminationByKubelet), containers: waiting, want: true},
		{name: "kubelet shutdown, no container status yet", phase: corev1.PodFailed, conditions: cond(corev1.PodReasonTerminationByKubelet), want: true},
		{name: "PodGC orphan", phase: corev1.PodFailed, conditions: cond(podReasonDeletionByPodGC), want: true},
		{name: "out-of-service node", phase: corev1.PodFailed, conditions: cond(podReasonDeletionByTaintManager), containers: waiting, want: true},
		{name: "kubelet shutdown mid-job", phase: corev1.PodFailed, conditions: cond(corev1.PodReasonTerminationByKubelet), containers: terminated},
		{name: "PodGC orphan mid-job", phase: corev1.PodFailed, conditions: cond(podReasonDeletionByPodGC), containers: running},
		{
			// A kubelet eviction stamps TerminationByKubelet too; it keeps its own arm.
			name: "evicted while pulling", phase: corev1.PodFailed, reason: podReasonEvicted,
			conditions: cond(corev1.PodReasonTerminationByKubelet), containers: waiting,
		},
		{name: "AGC-deleted worker", phase: corev1.PodFailed, conditions: cond(podReasonDeletionByPodGC), agcDeleted: true},
		{name: "still Pending", phase: corev1.PodPending, conditions: cond(corev1.PodReasonTerminationByKubelet), containers: waiting},
		{name: "scheduler preemption", phase: corev1.PodFailed, conditions: cond(corev1.PodReasonPreemptionByScheduler)},
		{name: "failed, no conditions", phase: corev1.PodFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tc.phase, Reason: tc.reason, Conditions: tc.conditions, ContainerStatuses: tc.containers}}
			if tc.agcDeleted {
				pod.Annotations = map[string]string{AnnotationDeletionReason: "job_abandoned"}
			}
			assert.Equal(t, tc.want, LostToNodeShutdownBeforeStart(pod))
			if tc.want {
				assert.False(t, LostToNodeShutdown(pod), "a pod must not reach both the re-run and the force-cancel")
			}
		})
	}
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
			// Q1179: a graceful node shutdown fails the pod with reason Terminated and a
			// kubelet DisruptionTarget, and deletes nothing.
			name:      "node shutdown",
			pod:       ranRunner(shutDown(scaleSetWorkerPod("p", identityAnnotations()), corev1.PodReasonTerminationByKubelet)),
			wantCause: recoveryCauseNodeShutdown, wantOK: true,
		},
		{
			// Q1198: shut down before any container started; abandonedAwaitingRecovery's.
			name: "node shutdown, worker never started",
			pod:  shutDown(scaleSetWorkerPod("p", identityAnnotations()), corev1.PodReasonTerminationByKubelet),
		},
		{
			// The node vanished before its kubelet wrote anything: PodGC patches the
			// orphan to Failed, then force-deletes it. The watch sees the Failed update
			// before any deletion mark, and the deletion arm would read the later delete
			// as cleanup.
			name:      "orphan failed by PodGC",
			pod:       ranRunner(shutDown(scaleSetWorkerPod("p", identityAnnotations()), podReasonDeletionByPodGC)),
			wantCause: recoveryCauseNodeShutdown, wantOK: true,
		},
		{
			// Q1207: an admin tainted the dead node out-of-service; PodGC failed the pod and
			// no kubelet wrote the termination record the deletion arm orders against.
			name:      "out-of-service node",
			pod:       outOfService(scaleSetWorkerPod("p", identityAnnotations())),
			wantCause: recoveryCauseNodeShutdown, wantOK: true,
		},
		{
			// A NoExecute taint on a live node: the kubelet records the exit after the
			// delete, so the deletion arm keeps it and its run-conclusion check.
			name: "NoExecute taint on a live node",
			pod: func() *corev1.Pod {
				pod := drained(scaleSetWorkerPod("p", identityAnnotations()))
				return shutDown(pod, podReasonDeletionByTaintManager)
			}(),
			wantCause: recoveryCauseDeletion, wantOK: true,
		},
		{
			// A dind sidecar outlived a runner whose job had already ended; the taint
			// manager's delete came after the runner's exit, so it is cleanup on every arm.
			name: "out-of-service node, sidecar outlived the runner",
			pod: func() *corev1.Pod {
				pod := outOfService(scaleSetWorkerPod("p", identityAnnotations()))
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{
					{Name: WorkerContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, FinishedAt: metav1.NewTime(pod.DeletionTimestamp.Add(-time.Minute)),
					}}},
					{Name: "dind", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				}
				return pod
			}(),
		},
		{
			// Never started on the out-of-service node: abandonedAwaitingRecovery's.
			name: "out-of-service node, worker never started",
			pod: func() *corev1.Pod {
				pod := outOfService(scaleSetWorkerPod("p", identityAnnotations()))
				pod.Status.ContainerStatuses = nil
				return pod
			}(),
		},
		{
			// A kubelet eviction carries the same TerminationByKubelet condition; the
			// eviction arm, ordered first, keeps it attributed to node pressure.
			name: "kubelet eviction carrying the kubelet DisruptionTarget",
			pod: func() *corev1.Pod {
				pod := shutDown(scaleSetWorkerPod("p", identityAnnotations()), corev1.PodReasonTerminationByKubelet)
				pod.Status.Reason = podReasonEvicted
				return pod
			}(),
			wantCause: recoveryCauseEviction, wantOK: true,
		},
		{
			name: "already-claimed node shutdown",
			pod:  handled(shutDown(scaleSetWorkerPod("p", identityAnnotations()), corev1.PodReasonTerminationByKubelet)),
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
