package provisioner

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Drained-worker recovery (Q502): the discriminator that lets a worker taken away by a
// `kubectl drain` or a bare `kubectl delete pod` reach the same automatic re-run an
// eviction or a preemption does.
//
// The shape such a worker publishes — PodFailed with an empty reason — is identical to a
// genuinely failing job AND to a run a human cancelled at GitHub, so neither phase nor
// reason can carry recovery. What separates them, measured at live GitHub on both halves
// (Q459), is metadata.deletionTimestamp at the moment the terminal phase publishes: set
// on a drained/deleted worker, absent on a cancel (nothing in the gateway deletes a
// cancelled run's pod) and on a real failure (nothing issued a delete). The full
// measurement and the decision are Q459's; the design boundary is the table in
// docs/design/04-operational-flows.md §4.2.
//
// The cancel exclusion holds only while nothing deletes the pod. An operator who
// hand-deletes a cancelled run's worker to reclaim its slot supplies the mark
// themselves, and the re-run lands: a cancelled conclusion accepts rerun-failed-jobs
// (measured 2026-08-05, Q683) where a false green refuses it. Whether this predicate
// should read the run's conclusion first is Q811.
//
// The one deleter that must NOT trigger recovery is the AGC itself: the reaper deletes
// pods it gave up on (a stuck-Pending worker, an orphaned Running one), and the
// provisioner reclaims the worker of a job the listener abandoned (job_abandoned,
// Q501) — re-running either would turn cleanup into a re-run trigger. Every AGC-issued
// delete of a worker that has not already reached its terminal phase is therefore
// preceded by an AnnotationDeletionReason stamp, and both tiers' detection treats a
// stamped pod's deletion as the AGC's own. The completedPodTTL=0 cleanup in
// provisioner.go is the one unstamped delete and needs no stamp: it follows the
// container's recorded exit, which the ordering below already excludes.
//
// A deletion path added later owes that exclusion, and owes the specs too. What a
// worker pod publishes at its terminal phase is exactly what those specs assert, so a
// new deleter can invalidate one without failing it: #1032 added the Q501 reclaim while
// E2E_GitHub_CancelledRunLeavesNoDeletionMark still asserted a cancelled run's worker
// was never deleted, and that spec skips without live-GitHub credentials, so no gate
// went red (Q599). deletion_inventory_test.go is the tripwire — it fails on any added,
// moved or renamed client delete in this module and prints the full spec roster,
// marking the ones no gate can run.

// AnnotationDeletionReason is stamped on a worker pod, with the deletion reason as its
// value, immediately before the AGC deletes that pod itself. Its presence is how both
// tiers' graceful-deletion recovery (Q502) tells the AGC's own cleanup from an external
// drain or delete — without it, every reaper delete would look like a disruption and
// re-run a job the AGC deliberately gave up on.
//
// It is controller-set and informational: never set it by hand (a hand-set stamp
// suppresses automatic recovery for that pod) and never use it for security enforcement.
const AnnotationDeletionReason = "actions-gateway.com/deletion-reason"

// deletedByAGC reports whether the AGC marked pod's deletion as its own before
// issuing it (AnnotationDeletionReason).
func deletedByAGC(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[AnnotationDeletionReason]
	return ok
}

// externallyDeletedBeforeTerminal reports whether pod was externally deleted before it
// reached its terminal phase — the predicate both tiers' detection shares: the waiter's
// terminalPhase capture on classic, the recovery scan's arm on scale-set.
//
// The mark alone is not enough, for two reasons the two checks below fix at once. A
// deletionTimestamp is also what a later cleanup of an already-failed pod carries — a
// disrupting delete precedes the container's exit, a cleanup follows it — so the
// delete is ordered against the termination record. And a real kubelet publishes a
// transient Failed-with-mark even for a deleted worker whose container never started
// (a drain catching a still-Pending pod — the shape the fake-GitHub drain spec pins):
// such a pod has no termination record at all, and a job that never ran produced no
// reportable failure for rerun-failed-jobs to act on.
//
// The ordering uses the deletion REQUEST time, not deletionTimestamp itself. The
// apiserver stamps deletionTimestamp as request time PLUS the grace period, so on a
// drained running worker the mark sits a whole grace period (30s by default) in the
// future of the exit a SIGTERM-honouring runner records seconds after the request.
// Comparing the raw mark reads every such worker as "deleted after terminal" and
// recovers only one that ignored SIGTERM to its SIGKILL — the shipped Q502 form,
// caught by E2E_AGC_ScaleSetRecovery on a real kubelet (Q519); envtest never sees the
// offset because its unscheduled pods delete with grace collapsed to zero.
func externallyDeletedBeforeTerminal(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp.IsZero() || deletedByAGC(pod) {
		return false
	}
	terminal := podTerminationRecordTime(pod)
	if terminal.IsZero() {
		return false // no container ever terminated: nothing reportable to re-run
	}
	return !deletionRequestedAt(pod).After(terminal)
}

// deletionRequestedAt returns when pod's deletion was requested: deletionTimestamp
// minus the grace period the apiserver folded into it (the two fields are stamped
// together, and re-stamped together when a shorter-grace delete supersedes). Call only
// with a non-zero DeletionTimestamp.
func deletionRequestedAt(pod *corev1.Pod) time.Time {
	t := pod.DeletionTimestamp.Time
	if pod.DeletionGracePeriodSeconds != nil {
		t = t.Add(-time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second)
	}
	return t
}

// podTerminationRecordTime returns the latest container terminated.finishedAt the
// kubelet recorded, or the zero time when no container ever terminated.
func podTerminationRecordTime(pod *corev1.Pod) time.Time {
	var t time.Time
	for i := range pod.Status.ContainerStatuses {
		if term := pod.Status.ContainerStatuses[i].State.Terminated; term != nil && term.FinishedAt.Time.After(t) {
			t = term.FinishedAt.Time
		}
	}
	return t
}

// PodTerminalTime returns when pod reached its terminal phase: the latest container
// terminated.finishedAt (set by the kubelet). Pods with no termination record (e.g.
// Unknown after node loss) fall back to the creation timestamp, which overstates the
// age — the conservative direction for its reaper caller (it reaps sooner).
func PodTerminalTime(pod *corev1.Pod) time.Time {
	if t := podTerminationRecordTime(pod); !t.IsZero() {
		return t
	}
	return pod.CreationTimestamp.Time
}
