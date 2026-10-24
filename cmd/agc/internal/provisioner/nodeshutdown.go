package provisioner

import (
	corev1 "k8s.io/api/core/v1"
)

// Node-shutdown recovery (Q1179): a worker whose node went away under it — a Spot or
// preemptible reclamation, or any other node shutdown — reaches the same automatic
// re-run an eviction does.
//
// Neither existing arm sees it. On a graceful node shutdown the kubelet's shutdown
// manager fails the pod with Status.Reason "Terminated", not "Evicted", and stamps
// DisruptionTarget=True/TerminationByKubelet, not PreemptionByScheduler; nothing deletes
// the pod, so the deletion arm's mark is absent too. When the node is gone before the
// kubelet can write that status, PodGC fails the orphan with DisruptionTarget=True/
// DeletionByPodGC and force-deletes it — the phase lands before the mark, which is the
// order the deletion arm reads as a cleanup of an already-failed pod. Both shapes read
// from kubernetes v1.35.0 (pkg/kubelet/nodeshutdown/nodeshutdown_manager.go,
// pkg/controller/podgc/gc_controller.go). The operator-facing boundary is the table in
// docs/design/04-operational-flows.md §4.2.

// podReasonDeletionByPodGC is the DisruptionTarget reason PodGC writes on a pod whose
// node no longer exists. k8s.io/api exports no constant for it.
const podReasonDeletionByPodGC = "DeletionByPodGC"

// LostToNodeShutdown reports whether pod failed because its node shut down or
// disappeared: a PodFailed pod carrying DisruptionTarget=True with reason
// TerminationByKubelet or DeletionByPodGC.
//
// Both reasons have only cluster-side writers — the kubelet and the PodGC controller —
// so, like PreemptionByScheduler, no human action and no failing job produces them, and
// the arm takes no conclusion check. TerminationByKubelet is also what the kubelet
// stamps on a node-pressure eviction and on a kubelet critical-pod preemption; the
// eviction arm, ordered ahead of this one, keeps the first labelled as an eviction, and
// the second is a node-side kill of a running job like the shutdown it is grouped with.
// PodFailed is required because the kubelet leaves an already-Succeeded pod's phase
// alone: a job that finished before the shutdown reported its own success.
func LostToNodeShutdown(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodFailed {
		return false
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.DisruptionTarget &&
			c.Status == corev1.ConditionTrue &&
			(c.Reason == corev1.PodReasonTerminationByKubelet || c.Reason == podReasonDeletionByPodGC) {
			return true
		}
	}
	return false
}
