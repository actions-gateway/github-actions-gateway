package provisioner

import (
	corev1 "k8s.io/api/core/v1"
)

// Node-shutdown recovery (Q1179, Q1207): a worker whose node went away under it — a Spot
// or preemptible reclamation the kubelet shuts down gracefully, a Node object deleted
// under it, or a dead node an admin taints out-of-service — reaches the same automatic
// re-run an eviction does. A node that goes NotReady and keeps its Node object with no
// such taint reaches none of these shapes: the taint manager's DeletionByTaintManager
// delete never completes without a kubelet, and the phase stays Running.
//
// Neither existing arm sees them. On a graceful node shutdown the kubelet's shutdown
// manager fails the pod with Status.Reason "Terminated", not "Evicted", and stamps
// DisruptionTarget=True/TerminationByKubelet, not PreemptionByScheduler; nothing deletes
// the pod, so the deletion arm's mark is absent too. When the node is gone before the
// kubelet can write that status, PodGC fails the orphan with DisruptionTarget=True/
// DeletionByPodGC and force-deletes it — the phase lands before the mark, which is the
// order the deletion arm reads as a cleanup of an already-failed pod. When an admin
// applies the node.kubernetes.io/out-of-service taint to a NotReady node (non-graceful
// node shutdown), the taint manager stamps DisruptionTarget=True/DeletionByTaintManager
// and deletes, then PodGC fails the still-terminating pod without a condition of its own
// and force-deletes it; no kubelet ever writes a termination record, which the deletion
// arm requires. All three shapes read from kubernetes v1.35.2
// (pkg/kubelet/nodeshutdown/nodeshutdown_manager.go, pkg/controller/podgc/gc_controller.go,
// pkg/controller/tainteviction/taint_eviction.go); the out-of-service shape is measured
// on kind v1.36.1 (Q1207). The operator-facing boundary is the table in
// docs/design/04-operational-flows.md §4.2.

// podReasonDeletionByPodGC is the DisruptionTarget reason PodGC writes on a pod whose
// node no longer exists. k8s.io/api exports no constant for it.
const podReasonDeletionByPodGC = "DeletionByPodGC"

// podReasonDeletionByTaintManager is the DisruptionTarget reason the taint manager writes
// on a pod it deletes for a NoExecute taint the pod does not tolerate. k8s.io/api exports
// no constant for it.
const podReasonDeletionByTaintManager = "DeletionByTaintManager"

// LostToNodeShutdown reports whether pod failed because its node shut down, its Node
// object was deleted, or its dead node was tainted out-of-service: a PodFailed pod the
// AGC did not delete itself, carrying DisruptionTarget=True with reason
// TerminationByKubelet or DeletionByPodGC, or with reason DeletionByTaintManager while
// the runner container is still recorded Running.
//
// All three reasons are written by cluster components — the kubelet, the PodGC
// controller and the taint manager — so, like PreemptionByScheduler, no failing job
// produces them, and the arm takes no conclusion check. The AGC's own deletions
// (AnnotationDeletionReason) are excluded as on the deletion arm: a reaped or reclaimed
// worker whose node later died is cleanup, not a disruption. TerminationByKubelet is also what the kubelet
// stamps on a node-pressure eviction and on a kubelet critical-pod preemption; the
// eviction arm, ordered ahead of this one, keeps the first labelled as an eviction, and
// the second is a node-side kill of a running job like the shutdown it is grouped with.
//
// DeletionByTaintManager follows any untolerated NoExecute taint, including one a human
// applies to a healthy node. There a live kubelet stops the containers and records their
// exit before it publishes PodFailed, and the deletion arm takes the pod; a Running
// runner is what only PodGC's out-of-service path leaves, because it fails the pod over
// a kubelet that is gone. Only the runner counts: a regular sidecar such as dind can
// outlive a runner whose job already ended, and that pod's delete is cleanup. A worker
// that never started has no Running runner either, and belongs to the abandoned
// handling instead.
//
// PodFailed is required because the kubelet leaves an already-Succeeded pod's phase
// alone: a job that finished before the shutdown reported its own success.
//
// A worker none of whose containers ever started is excluded on every reason: it ran
// no job, so there is no failed job for rerun-failed-jobs to act on, and
// LostToNodeShutdownBeforeStart routes it to the abandoned handling instead (Q1198).
func LostToNodeShutdown(pod *corev1.Pod) bool {
	reason, ok := nodeLossReason(pod)
	if !ok || !podEverStarted(pod) {
		return false
	}
	return reason != podReasonDeletionByTaintManager || runnerStillRunning(pod)
}

// LostToNodeShutdownBeforeStart reports whether pod is a worker whose node went away
// before any of its containers started: the node-loss shape LostToNodeShutdown takes,
// on a pod with no container start record, and not a kubelet eviction, which keeps its
// own arm.
//
// The kubelet's shutdown manager kills every pod it has admitted, Pending ones included,
// and fails each one that has not succeeded (kubernetes v1.35.0,
// pkg/kubelet/nodeshutdown/nodeshutdown_manager.go killPods over GetActivePods), so a
// worker still pulling its image when a Spot node is reclaimed takes this shape. Its job
// never ran, so the run is still in progress at GitHub and refuses a re-run until its
// unstarted-job timeout concludes it. It is the never-started worker the abandoned
// handling exists for — force-cancel the run, then re-run it — whatever removed it.
//
// DeletionByTaintManager needs no Running-runner check here: a never-started pod has
// no job for a sidecar to outlive, so a live node's taint delete and a dead node's
// out-of-service delete are both an abandonment.
func LostToNodeShutdownBeforeStart(pod *corev1.Pod) bool {
	if _, ok := nodeLossReason(pod); !ok || podEverStarted(pod) {
		return false
	}
	return pod.Status.Reason != podReasonEvicted
}

// nodeLossReason returns the DisruptionTarget=True reason a node-loss writer left on
// pod, where pod is PodFailed and the AGC did not delete it itself.
func nodeLossReason(pod *corev1.Pod) (string, bool) {
	if pod.Status.Phase != corev1.PodFailed || deletedByAGC(pod) {
		return "", false
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type != corev1.DisruptionTarget || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case corev1.PodReasonTerminationByKubelet, podReasonDeletionByPodGC, podReasonDeletionByTaintManager:
			return c.Reason, true
		}
	}
	return "", false
}

// runnerStillRunning reports whether pod's runner container is last recorded Running.
func runnerStillRunning(pod *corev1.Pod) bool {
	for i := range pod.Status.ContainerStatuses {
		if cs := &pod.Status.ContainerStatuses[i]; cs.Name == WorkerContainerName {
			return cs.State.Running != nil
		}
	}
	return false
}
