package provisioner

import (
	corev1 "k8s.io/api/core/v1"
)

// Preemption recovery (Q497).
//
// # Two disruptions, one recovery
//
// A worker pod can lose its job to two mechanisms that are both colloquially called
// eviction, and only one of them produces the shape the original recovery keys on:
//
//   - The kubelet's node-pressure eviction SIGKILLs the pod and leaves it PodFailed with
//     Status.Reason "Evicted". Detected by podReasonEvicted.
//   - kube-scheduler's preemption — what a RunnerGroup's priorityTiers actually drives —
//     removes the victim by DELETING it, after stamping a DisruptionTarget condition
//     with reason PreemptionByScheduler. Nothing about that resembles "Evicted".
//
// Q423 measured the consequence on a real cluster: a preempted worker reached no
// recovery on either tier, so oversubscription bought the packing guarantee but left
// the displaced run needing a manual re-run. This file supplies the missing
// discriminator; both tiers then feed it into the same handleEviction they already use.
//
// # Why the phase cannot be used instead, and why the condition can
//
// The same experiment ruled the terminal phase out entirely. A preempted worker lands in
// Pending, Succeeded or Failed depending on what its container was doing and what it
// exited with — so no phase/reason pair separates a disruption from an ordinary outcome.
//
// The condition has the property the phase lacks: kube-scheduler is the only writer of
// the PreemptionByScheduler reason. An operator's `kubectl delete pod`, a node drain, and
// a job failing on its own all leave a deletionTimestamp, and Q459 is still weighing
// whether that is safe to recover on for exactly that reason. None of them can produce
// this reason, so the preemption slice closes on its own.

// PreemptedByScheduler reports whether pod was removed by kube-scheduler preemption.
//
// The full triple is required — type DisruptionTarget, status True, reason
// PreemptionByScheduler — rather than the reason alone. DisruptionTarget is the shared
// condition for every disruption cause (the kubelet's own TerminationByKubelet, the
// eviction API, garbage collection), so the reason is what narrows it to preemption, and
// the status is what keeps a stale False condition from being read as a live one.
//
// It is exported for the owning reconcilers' worker-pod watch predicate, which must
// admit the update that adds this condition: a preemption changes no phase, so without
// that edge the recovery scan below is never woken while the victim still exists.
func PreemptedByScheduler(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.DisruptionTarget &&
			c.Status == corev1.ConditionTrue &&
			c.Reason == corev1.PodReasonPreemptionByScheduler {
			return true
		}
	}
	return false
}

// Recovery causes, carried into the log line, the Kubernetes Event, and the `cause`
// label on the eviction counters. The retry budget is deliberately NOT split by cause:
// maxEvictionRetries stays a hard lifetime cap per run_id across both causes together,
// so a run that is alternately evicted and preempted cannot spend two budgets. The label
// exists because the operator response differs — a climbing eviction count means node
// pressure, a climbing preemption count means the priorityTiers floor is displacing more
// opportunistic work than the tenant expected — and troubleshooting sends an operator
// looking for different things in each case.
const (
	recoveryCauseEviction   = "eviction"
	recoveryCausePreemption = "preemption"
)
