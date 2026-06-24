package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	corev1 "k8s.io/api/core/v1"
)

// Reap reasons, used as the `reason` label of
// actions_gateway_worker_pods_reaped_total.
const (
	reapReasonCompletedTTL    = "completed_ttl"
	reapReasonPendingDeadline = "pending_deadline"
)

// reapWorkerPods deletes worker pods the RunnerGroup no longer needs:
//
//   - pods in a terminal phase (Succeeded/Failed/Unknown) older than
//     spec.completedPodTTL — completed pods consume no compute but accumulate
//     without bound if never deleted;
//   - Pending pods older than spec.pendingPodDeadline — a pod stuck on an
//     unpullable image or unschedulable constraints otherwise holds a
//     concurrency-ceiling slot forever (activePodCount counts Pending).
//     Deleting it resolves the waiting session goroutine (the
//     InformerPodWaiter treats deletion as completion), which releases the
//     listener and the slot; that goroutine's cleanup deletes the job Secret.
//
// Running this from the reconciler rather than the provision goroutine makes
// cleanup restart-safe: the goroutine dies with the AGC process, while the
// reaper also covers pods orphaned by a crash. The reconciler's Pod watch
// re-triggers on phase transitions; the returned duration — the time until
// the earliest retained pod becomes due (0 = none) — is propagated as
// RequeueAfter to cover the purely time-based expiries in between.
func (r *RunnerGroupReconciler) reapWorkerPods(ctx context.Context, log *slog.Logger, rg *v1alpha1.RunnerGroup) (time.Duration, workerPodCounts, error) {
	return reapWorkerPodsByLabel(ctx, r.Client, r.nowFunc()(), rg.Namespace, rg.Name,
		provisioner.LabelRunnerGroup,
		provisioner.EffectiveCompletedPodTTL(rg), provisioner.EffectivePendingPodDeadline(rg),
		log, r.Metrics,
		func(podName string, deadline time.Duration) {
			// Operator-visible: a stuck-Pending pod means the job never ran —
			// usually an unpullable workerImage or unschedulable podTemplate.
			r.recordEvent(rg, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods",
				"worker pod %s was Pending for more than %s and has been deleted; "+
					"check the pod template image and scheduling constraints", podName, deadline)
		})
}

// podTerminalTime returns when pod reached its terminal phase: the latest
// container terminated.finishedAt (set by the kubelet). Pods with no
// termination record (e.g. Unknown after node loss) fall back to the
// creation timestamp, which overstates the age and so reaps sooner — the
// conservative direction for a pod that is already terminal.
func podTerminalTime(pod *corev1.Pod) time.Time {
	var t time.Time
	for i := range pod.Status.ContainerStatuses {
		if term := pod.Status.ContainerStatuses[i].State.Terminated; term != nil && term.FinishedAt.Time.After(t) {
			t = term.FinishedAt.Time
		}
	}
	if t.IsZero() {
		return pod.CreationTimestamp.Time
	}
	return t
}

// nowFunc returns the clock used by the reaper: Now when set (test seam),
// time.Now otherwise.
func (r *RunnerGroupReconciler) nowFunc() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}
