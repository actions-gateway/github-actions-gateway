package provisioner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Abandoned-run recovery on the scale-set tier (Q766).
//
// # Why this is not a fourth arm of disruptionAwaitingRecovery
//
// That predicate scans SURVIVING pods and every arm it matches feeds handleEviction,
// which calls rerun-failed-jobs. Neither fits a worker removed before its container
// ever ran. The dominant cause — the reaper's pendingPodDeadline reap — leaves no pod
// for a later scan to find at all; and a job that never ran produced no failed job for
// rerun-failed-jobs to act on, which is exactly the exclusion that predicate's third
// arm carries deliberately (externallyDeletedBeforeTerminal, on the missing container
// exit record).
//
// The exclusion therefore stays. What the never-started shape gets instead is its own
// detection and its own action, in that order: force-cancel the run FIRST, which is
// what makes a re-run legal (a cancelled run accepts rerun-failed-jobs where the false
// green refused it — Q683), then register it for the same capacity-gated re-run the
// classic tier defers (Q691).
//
// # Where it is detected
//
// Two seams, disjoint by construction because the reaper stamps
// AnnotationDeletionReason before it deletes and the scan arm requires the absence of
// that stamp:
//
//   - The reaper's own pending_deadline delete, via reapHooks.recoverAbandoned. The
//     reaper holds the pod, so this is the scale-set analogue of the classic tier's
//     informer delete event — and a sounder one, since it runs synchronously with the
//     delete decision rather than racing the object's teardown.
//   - abandonedAwaitingRecovery, in the RecoverEvictedScaleSetWorkers scan, for an
//     external delete of a still-Pending worker. A real kubelet publishes a transient
//     Failed carrying the deletion mark with no container exit record for that shape.
//
// Run identity comes off the pod's run-id/repository annotations, the same
// runIdentityFromPod read the Q417 eviction port relies on. The classic tier reads it
// from the AcquireJob payload its goroutine still holds; a scale-set worker has neither
// payload nor goroutine.
//
// What is deliberately NOT recovered: a completed_pending reap (the job is already
// terminal at GitHub, so there is nothing to cancel and the re-run would re-run
// finished work), a gateway teardown reap (Target.Resolve is about to fail and no later
// worker pod will ever bind to satisfy the wait), and a never-started worker that
// vanishes without publishing that transient Failed — the same inherent residual
// preemption and drain recovery already carry on this tier, because the evidence is the
// pod and the delete removes it.

// Detection labels for the recovery's log line: which of the two seams observed the
// abandonment.
const (
	// abandonedDetectionReaped is the reaper deleting a worker that never left Pending.
	abandonedDetectionReaped = "pending_deadline_reap"
	// abandonedDetectionDeleted is an external delete of a still-Pending worker, caught
	// in the scan by the transient Failed-with-mark it publishes.
	abandonedDetectionDeleted = "external_deletion"
)

// RecoverAbandonedScaleSetWorker is the reaper's entry point: call it after deleting a
// worker pod that never left Pending past its pendingPodDeadline, with the pod object
// the reaper still holds.
func (p *Provisioner) RecoverAbandonedScaleSetWorker(ctx context.Context, target Target, pod *corev1.Pod) <-chan struct{} {
	return p.recoverAbandoned(ctx, target, pod, abandonedDetectionReaped)
}

// recoverAbandoned force-cancels the workflow run behind a scale-set worker pod that
// was removed before any of its containers ran, and registers the run for automatic
// re-run once the owner places a worker pod again (Q766). detection names which seam
// observed it, for the log line only.
//
// It returns a done channel that closes once the force-cancel has finished, so a caller
// may block on it (tests) or ignore it (the reconciler and the reaper, neither of which
// may stall on GitHub). The call runs on a context detached from ctx — a reconcile's
// context is cancelled the moment Reconcile returns, and the POST must outlive it —
// bounded by forceCancelAbandonedRun's own timeout.
//
// A no-op for a pod that is not a scale-set worker: the classic tier recovers the same
// shape from the goroutine that owns the pod, and recovering it twice would spend a
// second slot of the run's shared retry budget.
func (p *Provisioner) recoverAbandoned(ctx context.Context, target Target, pod *corev1.Pod, detection string) <-chan struct{} {
	if pod.Labels[LabelAcquisitionProtocol] != AcquisitionProtocolScaleSet {
		return closedChan()
	}
	key := target.Key()
	log := p.logForKey(key).With("podName", pod.Name)

	owner, repo, runID, ok := runIdentityFromPod(pod)
	if !ok {
		// Counted as the tier's identity-unknown failure rather than as a force-cancel
		// outcome, so the two counters stay disjoint: on this tier
		// abandoned_run_force_cancels_total{outcome="identity_unknown"} is unreachable
		// by construction, and this is the series that says the assignment message
		// carried no run identity.
		log.Warn("scale-set worker was removed before it ran but its run identity is unknown; force-cancel and automatic re-run skipped",
			"detection", detection)
		if p.Metrics != nil {
			p.Metrics.EvictionRecoveryIdentityUnknown.WithLabelValues(key.Namespace, key.Name, recoveryCauseAbandoned).Inc()
		}
		target.RecordEvent(corev1.EventTypeWarning, "EvictionRecoveryIdentityUnknown", "RecoverAbandonedWorker",
			fmt.Sprintf("worker pod %s was removed before it ran but carries no workflow-run identity, so its run cannot be force-cancelled or re-run automatically; GitHub cancels it at its ~15-minute unstarted-job timeout and a manual re-run is required", pod.Name))
		return closedChan()
	}

	log.Warn("scale-set worker pod was removed before it ran; force-cancelling its run",
		"detection", detection, "runID", runID)
	done := make(chan struct{})
	fctx := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		p.forceCancelAbandonedRun(fctx, target, owner, repo, runID, evictionTierScaleSet, log)
	}()
	return done
}

// abandonedAwaitingRecovery reports whether pod is a scale-set worker that was
// externally deleted before any of its containers ran, and whose abandonment has not
// been adjudicated yet.
//
// This is the shape externallyDeletedBeforeTerminal excludes — a deletion mark with no
// container exit record — routed to the force-cancel path instead of being dropped. The
// exclusion there is still correct for what it guards: handleEviction would call
// rerun-failed-jobs on a job that never ran. Here the run is concluded first, which is
// what makes the re-run legal (Q683/Q766).
//
// Three conditions narrow it, and each rules out a shape that must not be recovered:
//
//   - The AGC's own deletes are excluded by AnnotationDeletionReason. The reaper's
//     pending_deadline delete reaches the same recovery through reapHooks.recoverAbandoned,
//     which fires once per delete; without this exclusion the transient Failed it
//     publishes would be recovered a second time here.
//   - PodFailed is required. A pod still Pending is not being deleted at all, and a pod
//     that reached any other terminal phase published something to report.
//   - No container ever started. A worker deleted mid-run reported whatever it had done,
//     and belongs to the disruption path (a real failed job, which rerun-failed-jobs can
//     act on) rather than this one.
func abandonedAwaitingRecovery(pod *corev1.Pod) bool {
	if _, handled := pod.Annotations[AnnotationEvictionHandledAt]; handled {
		return false
	}
	if pod.DeletionTimestamp.IsZero() || deletedByAGC(pod) {
		return false
	}
	if pod.Status.Phase != corev1.PodFailed {
		return false
	}
	return !podEverStarted(pod)
}
