package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// On a cluster that can grow, the capacity gate (Q406) reads the cluster autoscaler's OWN
// declination off a stuck worker pod's Events, rather than the scheduler's verdict.
//
// # Why the source has to change on an elastic cluster
//
// Where nothing can add a node, the gate reads PodScheduled=False/Unschedulable (Q405), sound
// only where nothing will act on that pod. Where an autoscaler runs, an unschedulable
// pod IS the request for a node, so gating on it suppresses the very signal that would
// have rescued the tenant. Only the autoscaler declaring that it will not act is
// evidence that no node is coming (docs/design/appendix-d-alternatives-considered.md
// §D.8).
//
// # Two matchers, not a provider abstraction
//
// Both open-source autoscaler projects emit these events from shared core code that
// every cloud provider vendors — cluster-autoscaler from
// processors/status/eventing_scale_up_processor.go and Karpenter from
// pkg/controllers/provisioning/scheduling/events.go — so ~46 provider
// implementations collapse to two event vocabularies. The managed offerings are not a
// third: GKE's autoscaler is CA-derived, EKS runs upstream CA or Karpenter, AKS runs
// CA or Node Auto Provisioning (which is Karpenter), OpenShift's MachineAutoscaler
// wraps CA. A registry plus an interface plus a config field would be pure overhead
// over two switch arms, because every autoscaler yields the same boolean and the same
// action — pluggable backends earn their keep when the BEHAVIOR differs per provider,
// not when only the recognized input does.
//
// # The safety asymmetry, and what it buys
//
// A missed declination costs nothing: the gate stays open, which is exactly today's
// behavior. A wrongly-read one starves a tenant of jobs the cluster would have run. So
// recognition stays deliberately narrow and broad autoscaler coverage is explicitly
// not a goal — a proprietary optimizer that emits its own vocabulary, or no events at
// all, simply never closes this gate.
//
// Two consequences of that asymmetry are load-bearing here:
//
//   - FailedScheduling is ALSO kube-scheduler's own reason, so it may only be read as
//     a declination when the event positively did not come from the scheduler. The
//     discriminator is the reporting controller, never the reason string alone.
//   - An autoscaler that declined and then acted must reopen the gate. The verdict is
//     therefore whichever relevant event is NEWEST, not merely whether a declination
//     exists — a pod declined on one loop and scaled up for on the next is being
//     rescued, and gating on the stale declination would starve exactly the tenant
//     this mode exists to protect.

// Event reasons the two autoscaler projects emit on a pending pod. Each project has
// one declination reason and one acting reason; nothing else is recognized.
const (
	// reasonNotTriggerScaleUp is cluster-autoscaler's declination: "pod didn't trigger
	// scale-up: <per-node-group reasons>", emitted only when a loop concluded WITHOUT
	// attempting a scale-up. Unique to CA — kube-scheduler never emits it — so unlike
	// FailedScheduling it needs no reporter discrimination.
	reasonNotTriggerScaleUp = "NotTriggerScaleUp"
	// reasonTriggeredScaleUp is cluster-autoscaler acting: a node group is growing for
	// this pod, so the pod is a live request and the gate must stay open.
	reasonTriggeredScaleUp = "TriggeredScaleUp"
	// reasonFailedScheduling is Karpenter's declination ("Failed to schedule pod,
	// <err>") AND kube-scheduler's ordinary transient scheduling failure. Only a
	// non-scheduler reporter makes it the former.
	reasonFailedScheduling = "FailedScheduling"
	// reasonNominated is Karpenter acting: it has picked a NodeClaim for this pod.
	reasonNominated = "Nominated"
)

// defaultSchedulerName is kube-scheduler's own name, and the value the API server
// defaults an unset pod.spec.schedulerName to.
const defaultSchedulerName = "default-scheduler"

// autoscalerEventClass is what one Event says about whether a node is coming.
type autoscalerEventClass int

const (
	// classIrrelevant: the event says nothing about autoscaling this pod.
	classIrrelevant autoscalerEventClass = iota
	// classDeclined: an autoscaler recorded that it will not add a node for this pod.
	classDeclined
	// classActing: an autoscaler is adding (or has picked) a node for this pod.
	classActing
)

// autoscalerDeclination reports whether the autoscaler has declined to make room for
// pod, given the Events recorded against it, and returns the autoscaler's own text so
// the condition message names the taint, quota, or node-group ceiling that stopped the
// scale-up rather than merely asserting that something did.
//
// The verdict is the class of the NEWEST relevant event, so a declination followed by
// a scale-up reopens the gate. Ties resolve to not-declined: Event timestamps have
// one-second resolution, so a same-second declination and scale-up are genuinely
// ambiguous, and the fail-open direction is the only safe reading of an ambiguity here.
//
// No relevant events — an autoscaler this matcher does not recognize, an autoscaler
// that has not looked at the pod yet, or no autoscaler at all — yields false, which is
// today's ungated behavior.
func autoscalerDeclination(pod *corev1.Pod, evts []corev1.Event) (declined bool, detail string) {
	schedulerName := pod.Spec.SchedulerName
	if schedulerName == "" {
		schedulerName = defaultSchedulerName
	}

	var newest time.Time
	newestClass, newestMsg := classIrrelevant, ""
	for i := range evts {
		e := &evts[i]
		class := classifyAutoscalerEvent(e, schedulerName)
		if class == classIrrelevant {
			continue
		}
		at := eventTime(e)
		switch {
		case newestClass == classIrrelevant: // the first relevant event wins by default
		case at.After(newest): // a strictly newer verdict supersedes
		case at.Equal(newest) && class == classActing: // a tie resolves to not-declined
		default:
			continue
		}
		newest, newestClass, newestMsg = at, class, truncate(e.Message, 200)
	}
	if newestClass != classDeclined {
		return false, ""
	}
	return true, newestMsg
}

// classifyAutoscalerEvent maps one Event to what it says about a node arriving for the
// pod it was recorded against. schedulerName is the pod's own scheduler, so a cluster
// running a non-default scheduler discriminates against THAT name rather than against
// a hardcoded one.
func classifyAutoscalerEvent(e *corev1.Event, schedulerName string) autoscalerEventClass {
	switch e.Reason {
	case reasonNotTriggerScaleUp:
		return classDeclined
	case reasonTriggeredScaleUp:
		return classActing
	case reasonFailedScheduling:
		// The ambiguous one. kube-scheduler emits this for every ordinary transient
		// placement failure, and reading those as declinations would gate a set the
		// cluster was about to grow for — the exact over-gating this mode exists to
		// avoid. Only a positively non-scheduler reporter makes it Karpenter's.
		if reportedByScheduler(e, schedulerName) {
			return classIrrelevant
		}
		return classDeclined
	case reasonNominated:
		// Karpenter has picked a NodeClaim for this pod. Not reporter-discriminated:
		// treating a stray Nominated as "a node is coming" is the fail-open direction.
		return classActing
	}
	return classIrrelevant
}

// reportedByScheduler reports whether e came from the scheduler rather than from an
// autoscaler, checking both the new-style ReportingController and the legacy
// Source.Component (cluster-autoscaler and Karpenter both still record through the
// legacy recorder, and the API server serves the two representations of one event).
//
// An event with NEITHER field set counts as the scheduler's. That is not a guess about
// its origin: the check exists so that an ambiguous reason is read as a declination
// only when the AGC can positively tell it did not come from the scheduler, and an
// unattributable event cannot clear that bar.
func reportedByScheduler(e *corev1.Event, schedulerName string) bool {
	reporter := e.ReportingController
	if reporter == "" {
		reporter = e.Source.Component
	}
	return reporter == "" || reporter == schedulerName || reporter == defaultSchedulerName
}

// eventTime is the most recent moment an Event is known to have been recorded, taking
// the latest of every timestamp field an Event may carry. The two recorder generations
// populate different ones — the legacy recorder sets FirstTimestamp/LastTimestamp and
// bumps LastTimestamp on each repeat, the new-style recorder sets EventTime — so
// reading only one field would silently order every event from the other generation at
// the zero time.
func eventTime(e *corev1.Event) time.Time {
	latest := e.CreationTimestamp.Time
	for _, t := range []time.Time{e.FirstTimestamp.Time, e.LastTimestamp.Time, e.EventTime.Time} {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}
