package provisioner

import (
	corev1 "k8s.io/api/core/v1"
)

// Preemption recovery (Q497): the discriminator that lets a worker displaced by a
// higher priorityTiers tier reach the same automatic re-run a kubelet eviction does.
//
// Two mechanisms are both colloquially called eviction and only one produces the
// PodFailed/Evicted shape the original recovery keyed on. kube-scheduler preemption —
// what priorityTiers actually drives — instead DELETES its victim after stamping a
// DisruptionTarget condition. Both tiers feed this file's result into the same
// handleEviction they already use.
//
// Two constraints this file must not lose, both measured rather than assumed (Q423):
//
//   - Detection must key on the CONDITION, never the phase. A preempted worker lands in
//     Pending, Succeeded or Failed depending on what its container was doing, so no
//     phase/reason pair separates a disruption from an ordinary outcome.
//   - It must match the full type/status/REASON triple, never the condition type alone.
//     The eviction API stamps the same type with its own reason, so a type-only match
//     would silently pull in the drain path, which is gated separately — on the deletion
//     mark, not on this condition (Q459/Q502; see deletion.go).
//
// The full reasoning — why the scheduler deletes rather than evicts, why the worker's
// disruption-safety annotations and a PodDisruptionBudget cannot deflect it, why
// re-running a preempted job is not a double report, and what the design costs us — is
// in docs/design/04-operational-flows.md §4.2 ("Which disruptions are recovered",
// "Why preemption deletes rather than evicts"). Keep it there rather than regrowing it
// here; the operator-facing half is docs/operations/troubleshooting.md.

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
// maxEvictionRetries stays a hard lifetime cap per run_id across all causes together,
// so a run that is alternately evicted, preempted, and drained cannot spend multiple
// budgets. The label exists because the operator response differs — a climbing eviction
// count means node pressure, a climbing preemption count means the priorityTiers floor
// is displacing more opportunistic work than the tenant expected, a climbing deletion
// count means something (a drain, an operator, an autoscaler) is removing live workers
// — and troubleshooting sends an operator looking for different things in each case.
//
// recoveryCauseAbandoned is the one whose re-run is not immediate: the worker never
// started, so the run was force-cancelled (Q683) and the re-run waits for capacity to
// return before spending its slot (Q691, abandoned_rerun.go).
//
// recoveryCauseVanished is the one named for what was observed rather than for what
// happened: the worker was gone when the AGC came back, and which of the three deleting
// causes took it is exactly what the missing pod no longer says (Q844,
// orphaned_scaleset.go).
const (
	recoveryCauseEviction   = "eviction"
	recoveryCausePreemption = "preemption"
	recoveryCauseDeletion   = "deletion"
	recoveryCauseAbandoned  = "abandoned"
	recoveryCauseVanished   = "vanished"
)
