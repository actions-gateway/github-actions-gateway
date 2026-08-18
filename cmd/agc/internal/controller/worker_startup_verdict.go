package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The kubelet's startup verdict (Q714) — the third source under the capacity gate,
// and the sibling of the scheduler verdict podUnschedulable reads.
//
// podUnschedulable answers "no node can host this pod". This file answers the
// opposite half of PodScheduled: a pod a node DID accept, whose container the kubelet
// then could not start. An unpullable worker image is that case, and it is the one a
// first-time operator meets, because both 1.4 DinD templates ship an example.invalid
// placeholder to be replaced. Such a pod binds, sits Pending in ImagePullBackOff, and
// is reaped at pendingPodDeadline — spending a single-use JIT runner record and
// holding a GitHub job lock for the whole window, once per delivered job.
//
// Why it gates on every cluster, while the scheduler's verdict gates only where the
// operator asserted nothing can add a node: the asymmetry that split is built on is
// whether another actor is waiting on the pod to make capacity appear (§D.8). A
// Pending unschedulable pod may BE the request for a node. A bound one is not a
// request for anything — it is already placed, and no node an autoscaler adds changes
// whether its image resolves.

// podStartupBackoff reports whether the kubelet has bound this pod and then failed to
// start it, returning the kubelet's own waiting message for the first such container.
//
// Narrow by construction, in three ways, because the rung may under-gate freely and
// must never over-gate:
//
//   - The pod must still be Pending. A Running pod whose sidecar is restarting is a
//     job in progress, not a worker that never started.
//   - The verdict is the kubelet's BACKOFF state, not its first failure. ErrImagePull
//     is one failed pull, which a registry blip produces and the next attempt clears;
//     ImagePullBackOff means the kubelet has already tried, failed, and scheduled a
//     retry. That distinction is this signal's grace — see startupBackoffReasons.
//   - Only the image-pull family counts. Placeability is a property of the pod SHAPE
//     (§4), and so is an unpullable image: every worker this owner would create next
//     carries the same reference and fails identically. A per-pod config error is not
//     evidence about the next pod, so CreateContainerConfigError and friends are
//     deliberately out.
//
// Init containers and native sidecars count alongside regular containers: a worker
// whose injected proxy sidecar cannot pull never runs its job either.
func podStartupBackoff(pod *corev1.Pod) (bool, string) {
	if pod.Status.Phase != corev1.PodPending {
		return false, ""
	}
	if !podBound(pod) {
		return false, ""
	}
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.InitContainerStatuses,
		pod.Status.ContainerStatuses,
	} {
		for i := range statuses {
			w := statuses[i].State.Waiting
			if w == nil {
				continue
			}
			if _, ok := startupBackoffReasons[w.Reason]; ok {
				return true, w.Message
			}
		}
	}
	return false, ""
}

// startupBackoffReasons are the container waiting reasons that count as the kubelet's
// startup verdict. One entry today, and the set is the whole matcher, so widening it
// is a deliberate edit with a test row rather than a side effect of some other change.
//
// ImagePullBackOff is the kubelet's own conclusion that a pull has failed and been
// backed off, which is why this signal needs no time-based grace of its own (see
// applyCapacityGateCondition's caller). ErrImagePull — the single, pre-backoff
// failure — is excluded for the same reason: it is the attempt, not the conclusion.
var startupBackoffReasons = map[string]struct{}{
	"ImagePullBackOff": {},
}

// podBound reports whether the scheduler has placed this pod on a node. True is the
// precondition for the startup verdict and the exact complement of the PodScheduled
// half podUnschedulable reads, so the two signals can never both fire on one pod.
//
// The condition is authoritative rather than spec.nodeName: a pod can carry a node
// name from a scheduler that has not yet recorded the binding, and the kubelet has
// nothing to report until it has.
func podBound(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// podStartedAt returns when this pod's containers first ran, and whether any ever did.
// It is the evidence that releases the capacity gate's latch (Q512, corrected by
// Q714): a pod that started demonstrates the cluster can run a worker, where a pod
// that merely bound only suggests it.
//
// Every container is considered and the EARLIEST start wins, because the question is
// when this pod stopped being an unresolved probe, not when its last container caught
// up. A terminated container counts — a probe job that ran and finished is the
// strongest evidence there is — and so does a restarted one, read through lastState.
func podStartedAt(pod *corev1.Pod) (time.Time, bool) {
	var earliest time.Time
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.InitContainerStatuses,
		pod.Status.ContainerStatuses,
	} {
		for i := range statuses {
			for _, st := range []corev1.ContainerState{statuses[i].State, statuses[i].LastTerminationState} {
				at, ok := containerStartedAt(st)
				if !ok {
					continue
				}
				if earliest.IsZero() || at.Before(earliest) {
					earliest = at
				}
			}
		}
	}
	return earliest, !earliest.IsZero()
}

// containerStartedAt returns the moment a container state says its process began, if
// it began at all. Waiting never has one; Running and Terminated both do.
func containerStartedAt(st corev1.ContainerState) (time.Time, bool) {
	switch {
	case st.Running != nil && !st.Running.StartedAt.IsZero():
		return st.Running.StartedAt.Time, true
	case st.Terminated != nil && !st.Terminated.StartedAt.IsZero():
		return st.Terminated.StartedAt.Time, true
	}
	return time.Time{}, false
}
