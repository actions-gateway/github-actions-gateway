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
	// reapReasonOrphanedRunning labels a pod deleted because it was still Running
	// long after its job went terminal at GitHub — a worker that never received its
	// job, or one held open past the runner's exit (Q420).
	reapReasonOrphanedRunning = "orphaned_running"
	// reapReasonLifetimeExceeded labels a pod deleted after the kubelet killed it
	// for exceeding spec.maxWorkerLifetime (the pod's activeDeadlineSeconds). It is
	// split out from completed_ttl so an operator debugging a killed long job sees
	// the lifetime cap as the cause rather than a mystery termination (Q438).
	reapReasonLifetimeExceeded = "lifetime_exceeded"
)

// podReasonDeadlineExceeded is the Pod.Status.Reason the kubelet sets when it kills
// a pod for exceeding activeDeadlineSeconds. It is disjoint from the eviction path's
// "Evicted" (see eviction_scaleset.go), so a lifetime kill can never be mistaken for
// a node-pressure eviction and can never trigger a spurious rerun-failed-jobs.
const podReasonDeadlineExceeded = "DeadlineExceeded"

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
//     listener and the slot; that goroutine's cleanup deletes the job Secret;
//   - Running pods still alive completedJobRunningGrace after their job went
//     terminal at GitHub. Classic pods are never stamped with the completion
//     annotation this arm reads (provision() owns them through to a terminal
//     phase), so in practice this arm only fires on the scale-set tier (Q420).
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
		// No deregisterRunner hook: the v1 tier's workers carry no pre-registered
		// scale-set runner record — its JIT agents re-register per job (Q114).
		reapHooks{
			emitStuckPending: func(podName string, deadline time.Duration) {
				// Operator-visible: a stuck-Pending pod means the job never ran —
				// usually an unpullable workerImage or unschedulable podTemplate.
				r.recordEvent(rg, corev1.EventTypeWarning, "WorkerPodStuckPending", "ReapWorkerPods",
					"worker pod %s was Pending for more than %s and has been deleted; "+
						"check the pod template image and scheduling constraints", podName, deadline)
			},
			emitOrphanedRunning: func(podName string, grace time.Duration) {
				// Operator-visible: the pod outlived its own job, so it was holding a
				// concurrency slot and a node for nothing.
				r.recordEvent(rg, corev1.EventTypeWarning, "WorkerPodOrphanedRunning", "ReapWorkerPods",
					"worker pod %s was still Running %s after its job completed and has been deleted; "+
						"the runner never received its job, or a container in the pod outlived it", podName, grace)
			},
			emitLifetimeExceeded: func(podName string) {
				// Operator-visible: say plainly that the lifetime cap killed the pod and
				// name the field to raise, so a legitimately long job that hit it is a
				// one-line diagnosis rather than an unexplained failure.
				r.recordEvent(rg, corev1.EventTypeWarning, "WorkerPodLifetimeExceeded", "ReapWorkerPods",
					"worker pod %s was killed by the kubelet after exceeding the %s worker lifetime "+
						"(spec.maxWorkerLifetime) and has been deleted; if the job was legitimately "+
						"this long, raise spec.maxWorkerLifetime on this RunnerGroup or set it to 0s "+
						"to disable the cap", podName, provisioner.EffectiveMaxWorkerLifetime(rg))
			},
		})
}

// nowFunc returns the clock used by the reaper: Now when set (test seam),
// time.Now otherwise.
func (r *RunnerGroupReconciler) nowFunc() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}
