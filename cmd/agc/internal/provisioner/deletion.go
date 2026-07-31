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
// on a drained/deleted worker, absent on a cancel (nothing deletes the pod) and on a
// real failure (nothing issued a delete). The full measurement and the decision are
// Q459's; the design boundary is the table in docs/design/04-operational-flows.md §4.2.
//
// The one deleter that must NOT trigger recovery is the AGC itself: the reaper deletes
// pods it gave up on (a stuck-Pending worker, an orphaned Running one), and re-running
// those would turn cleanup into a re-run trigger. Every AGC-issued worker-pod delete is
// therefore preceded by an AnnotationDeletionReason stamp, and both tiers' detection
// treats a stamped pod's deletion as the AGC's own. The same exclusion must be applied
// to any future AGC deletion path — e.g. a Q501 cancel-relay that deletes the worker.

// AnnotationDeletionReason is stamped on a worker pod, with the reap reason as its
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
// The mark alone is not enough, for two reasons that ordering it against the pod's
// terminal time fixes at once. A deletionTimestamp is also what a later cleanup of an
// already-failed pod carries — a disrupting delete precedes the container's exit, a
// cleanup follows it. And a real kubelet publishes a transient Failed-with-mark even
// for a deleted worker whose container never started (a drain catching a still-Pending
// pod — the shape the fake-GitHub drain spec pins): such a pod has no termination
// record, falls back to its creation time (see PodTerminalTime), reads as "deleted
// after", and is excluded — correctly, because a job that never ran produced no
// reportable failure for rerun-failed-jobs to act on.
func externallyDeletedBeforeTerminal(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp.IsZero() || deletedByAGC(pod) {
		return false
	}
	return !pod.DeletionTimestamp.Time.After(PodTerminalTime(pod))
}

// PodTerminalTime returns when pod reached its terminal phase: the latest container
// terminated.finishedAt (set by the kubelet). Pods with no termination record (e.g.
// Unknown after node loss) fall back to the creation timestamp, which overstates the
// age — the conservative direction for both of its callers (the reaper reaps sooner;
// externallyDeletedBeforeTerminal declines recovery).
func PodTerminalTime(pod *corev1.Pod) time.Time {
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
