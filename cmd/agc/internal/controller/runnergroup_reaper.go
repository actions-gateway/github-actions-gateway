package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
func (r *RunnerGroupReconciler) reapWorkerPods(ctx context.Context, log *slog.Logger, rg *v1alpha1.RunnerGroup) (time.Duration, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(rg.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerGroup: rg.Name},
	); err != nil {
		return 0, fmt.Errorf("reaper: list worker pods: %w", err)
	}

	now := r.nowFunc()()
	ttl := provisioner.EffectiveCompletedPodTTL(rg)
	deadline := provisioner.EffectivePendingPodDeadline(rg)

	var next time.Duration
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		var due time.Time
		var reason string
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
			due = podTerminalTime(pod).Add(ttl)
			reason = reapReasonCompletedTTL
		case corev1.PodPending:
			due = pod.CreationTimestamp.Add(deadline)
			reason = reapReasonPendingDeadline
		default:
			// Running pods are bounded by GitHub's job-level timeout and the
			// job-lock renewal contract, not by an AGC-side deadline.
			continue
		}

		if wait := due.Sub(now); wait > 0 {
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}

		// Precondition on UID so a slow reconcile cannot delete a newer pod
		// that reused the name after this one was already removed.
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue // already gone (goroutine cleanup, external delete)
			}
			return next, fmt.Errorf("reaper: delete worker pod %s: %w", pod.Name, err)
		}

		log.Info("reaped worker pod", "pod", pod.Name, "phase", pod.Status.Phase, "reason", reason)
		if r.Metrics != nil {
			r.Metrics.WorkerPodsReaped.WithLabelValues(rg.Namespace, rg.Name, reason).Inc()
		}
		if reason == reapReasonPendingDeadline {
			// Operator-visible: a stuck-Pending pod means the job never ran —
			// usually an unpullable workerImage or unschedulable podTemplate.
			r.recordEvent(rg, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods",
				"worker pod %s was Pending for more than %s and has been deleted; "+
					"check the pod template image and scheduling constraints", pod.Name, deadline)
		}
	}
	return next, nil
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
