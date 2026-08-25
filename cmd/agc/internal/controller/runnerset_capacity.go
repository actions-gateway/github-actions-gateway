package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Worker-capacity conditions on the v2 RunnerSet (Q303). This is the v2 port of the
// two v1 RunnerGroup evaluations that a stalled set would otherwise hide behind a
// rising status.pendingJobs with Ready=True:
//   - the two-tier namespace-ResourceQuota ladder (WorkerQuotaPressure/Exceeded, Q82),
//     ported from worker_quota.go, and
//   - the scheduler-verdict signal (WorkersUnschedulable, Q157), ported from
//     runnergroup_unschedulable.go.
//
// Both reuse the owner-agnostic cores those v1 files now expose (evalWorkerQuotaFor,
// countActiveWorkerPodsByLabel, evalWorkersUnschedulableForPods). Only the v2 sources
// of the pod shape (the resolved RunnerTemplate) and the ceiling
// (spec.priorityTiers/maxWorkers) differ.
//
// A third, v2-only, joins them: the kubelet's startup verdict (WorkersNotStarting,
// Q906). That one is computed by the same shared core but published nowhere on v1,
// because a RunnerGroup has no capacity gate for the fact to have reached first.
//
// None of them gates Ready — they are advisory capacity signals, mirroring v1.

// applyWorkerCapacityConditions computes and merges the WorkerQuota ladder and the
// WorkersUnschedulable and WorkersNotStarting conditions onto the RunnerSet status,
// emitting a Warning Event on a genuine False→True transition of either signal (never
// every reconcile). It is called on both acquisition paths (classic and scale-set)
// after references resolve, with the resolved worker template supplying the quota
// footprint and the resolved gateway supplying the cluster facts the capacity gate
// depends on (Q470).
//
// It returns the soonest re-check any of those signals needs (0 = none), which the
// caller folds into RequeueAfter. Two sources, because the Pod watch fires on phase
// changes only and both signals move without one: a Pending pod crossing its
// scheduling grace (Q157), and a bound pod that has yet to declare a startup verdict
// either way (Q714, published unconditionally since Q906).
func (r *RunnerSetReconciler) applyWorkerCapacityConditions(ctx context.Context, rs *v2alpha1.RunnerSet, tmpl *v2alpha1.RunnerTemplateSpec, gw *v2alpha1.ActionsGateway) time.Duration {
	wq := r.evalRunnerSetWorkerQuota(ctx, rs, tmpl)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionWorkerQuotaPressure,
		Status:             boolConditionStatus(wq.pressure),
		Reason:             wq.pressureReason,
		Message:            wq.pressureMessage,
		ObservedGeneration: rs.Generation,
	})
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionWorkerQuotaExceeded,
		Status:             boolConditionStatus(wq.exceeded),
		Reason:             wq.exceededReason,
		Message:            wq.exceededMessage,
		ObservedGeneration: rs.Generation,
	})

	unsched := r.evalRunnerSetWorkersUnschedulable(ctx, rs)
	wasUnsched := meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkersUnschedulable)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionWorkersUnschedulable,
		Status:             boolConditionStatus(unsched.unschedulable),
		Reason:             unsched.reason,
		Message:            unsched.message,
		ObservedGeneration: rs.Generation,
	})
	if unsched.unschedulable && !wasUnsched {
		r.recordEvent(rs, corev1.EventTypeWarning, "WorkersUnschedulable", "Reconcile", unsched.message)
	}

	r.applyWorkersNotStartingCondition(rs, unsched)

	gateRecheck := r.applyCapacityGateCondition(ctx, rs, unsched, gw)
	// startupRecheck is folded in HERE rather than only inside the gate (Q906). A bound
	// worker pod that has not resolved either way is invisible to the Pod watch, and
	// WorkersNotStarting is now published on every set — so on an ungated set, which is
	// the default, nothing else would wake the reconciler to publish or clear it. The
	// gate adds the same re-check on its own path; soonest makes the overlap free.
	return soonest(soonest(unsched.requeueAfter, startupRecheck(unsched)), gateRecheck)
}

// applyWorkersNotStartingCondition publishes the kubelet's startup verdict as its own
// advisory condition (Q906), emitting a Warning Event on a genuine False->True
// transition and never every reconcile.
//
// Unconditional, unlike the capacity gate beside it: the fact is an observation, not a
// decision, so it is reported whether or not the set opted into spec.capacityGate. That
// is the whole point of the condition. Before it, the same evaluation reached an
// operator only through WorkerCapacityDeclined/PodsNotStarting (Q714), which is present
// only on a gated set, so the default set published nothing between the kubelet's
// verdict and the reaper's WorkerPodStuckPending Event one pendingPodDeadline later.
//
// Set False rather than removed when clear, mirroring WorkersUnschedulable and NOT
// WorkerCapacityDeclined: absence is meaningful for the gate (it says "this set has no
// gate") and meaningless here, where every set is evaluated.
func (r *RunnerSetReconciler) applyWorkersNotStartingCondition(rs *v2alpha1.RunnerSet, unsched workersUnschedulable) {
	notStarting := len(unsched.notStartingPods) > 0
	reason := v2alpha1.ReasonWorkersStarting
	message := "no worker pod is bound to a node and failing to start"
	if notStarting {
		reason = v2alpha1.ReasonPodsNotStarting
		message = notStartingObservation(unsched.notStartingPods)
	}
	was := meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkersNotStarting)
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionWorkersNotStarting,
		Status:             boolConditionStatus(notStarting),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rs.Generation,
	})
	if notStarting && !was {
		r.recordEvent(rs, corev1.EventTypeWarning, "WorkersNotStarting", "Reconcile", message)
	}
}

// soonest returns the earliest non-zero of two re-check intervals (0 = none), so a
// caller can fold several independent "look again at" answers into one RequeueAfter.
func soonest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// applyCapacityGateCondition publishes the WorkerCapacityDeclined condition — the
// decision the admission ladder's placeability rung reads back (Q405). The rung does
// not re-derive the verdict; this condition IS the decision, so an operator's
// `kubectl describe` and the AGC's intake behavior cannot disagree. It returns the
// re-check interval the gate's own signal needs (0 = none), which the caller folds
// into RequeueAfter.
//
// Every mode is fed by the WorkersUnschedulable evaluation the caller just computed
// rather than by a second pod list: on a fixed-size cluster the gate IS that evaluation,
// and on an elastic one it reads Events for exactly the pods it found stuck. Computing the
// stuck set twice would let the two disagree across a single reconcile, and would
// double the pod-list cost of the rung.
//
// A set that did not opt in carries NO such condition at all: it is removed rather
// than published False. Absence is what the default costs — no cost, no stale alarm
// after opting out, and the condition's presence is itself the "this set has a
// capacity gate" signal.
func (r *RunnerSetReconciler) applyCapacityGateCondition(ctx context.Context, rs *v2alpha1.RunnerSet, unsched workersUnschedulable, gw *v2alpha1.ActionsGateway) time.Duration {
	mode := runnerSetCapacityGateMode(rs)
	if mode == v2alpha1.CapacityGateModeOff {
		meta.RemoveStatusCondition(&rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
		return 0
	}

	declined, reason, message, recheck := r.evalCapacityGate(ctx, mode, unsched, gw)

	// The latch (Q512): a not-declined verdict reached only because the stuck pods
	// are gone — the reaper deleted the gate's own evidence — retains the decline
	// instead of clearing it. Clearing here is what §9e measured as the no-op: on
	// the scale-set tier it restored the full advertisement every deadline window,
	// so a burst of N wasted claims stayed N. The latched reason is what the two
	// rung forms read to admit exactly one probe job; the pod that job produces is
	// the evidence that resolves the latch, in whichever direction it lands.
	prev := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	if !declined && capacityGateLatchHolds(prev, reason, unsched) {
		declined = true
		reason = v2alpha1.ReasonAwaitingProbe
		message = latchedCapacityMessage(prev)
		// Nothing re-triggers a reconcile when a probe pod binds but stays Pending
		// (image pull) — the Pod watch fires on phase changes only — so the latched
		// state polls at the same cadence the elastic signal already uses, or faster
		// while a bound probe has yet to declare itself.
		recheck = soonest(autoscalerVerdictRecheck, startupRecheck(unsched))
	}

	wasDeclined := prev != nil && prev.Status == metav1.ConditionTrue
	meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
		Type:               v2alpha1.ConditionWorkerCapacityDeclined,
		Status:             boolConditionStatus(declined),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rs.Generation,
	})
	// Warning on the False→True transition only. It deliberately duplicates the
	// WorkersUnschedulable Event's underlying fact, because it reports a different
	// consequence — this set has STOPPED taking jobs — and that is the moment an
	// operator needs to know about, not the stuck pod that preceded it.
	if declined && !wasDeclined {
		r.recordEvent(rs, corev1.EventTypeWarning, "WorkerCapacityDeclined", "Reconcile", message)
	}
	return recheck
}

// capacityGateLatchHolds reports whether a fresh not-declined verdict must instead
// retain the previous decline as the latched AwaitingProbe state (Q512).
//
// Entry is deliberately narrow — all four must hold:
//
//   - The condition is currently True (a live decline, or an already-held latch).
//   - The gate actually evaluated and found capacity (reason CapacityAvailable). An
//     unsupported mode's fail-open is not a capacity verdict and must not latch.
//   - No stuck pod exists. A not-declined verdict reached WITH stuck pods present is
//     the autoscaler's own answer (an acting signal, or fail-open on an unreadable or
//     unrecognized vocabulary), and there the fail-open contract owns the decision.
//     A not-starting pod needs no arm here — that verdict declines outright, so a
//     fresh CapacityAvailable already means there is none.
//   - No worker pod has STARTED since the condition became True. A post-decline
//     container start — whenever the pod was created — is capacity returning, and
//     clears.
//
// Starting rather than scheduling is Q714's correction to Q512. Binding was a proxy
// for "a worker can run here", and the kubelet's startup verdict is exactly the case
// where the proxy is wrong: a probe pod binds within a second and only reveals that
// it cannot start seconds later, so releasing on the bind would restore the full
// advertisement inside that gap — reintroducing the §9e no-op this latch removed. For
// the two placeability reasons the stronger evidence is free, since a pod that starts
// has necessarily bound; it costs a healthy probe the seconds between the two.
//
// Holding on ABSENCE of evidence inverts the rung's fail-open habit, and is safe
// only because the latch never closes intake: its floor is one probe job per
// deadline window, so the cost of a wrongly-held latch is a briefly-throttled
// tenant, never a starved one.
func capacityGateLatchHolds(prev *metav1.Condition, freshReason string, unsched workersUnschedulable) bool {
	if prev == nil || prev.Status != metav1.ConditionTrue {
		return false
	}
	if freshReason != v2alpha1.ReasonCapacityAvailable {
		return false
	}
	if len(unsched.stuckPods) > 0 {
		return false
	}
	return !unsched.lastStartedAt.After(prev.LastTransitionTime.Time)
}

// latchedCapacityMessage carries the reaped verdict into the latched condition, so
// the operator still sees WHICH signal declined after its pod is gone. A latch
// re-published over itself keeps its message rather than re-wrapping it.
func latchedCapacityMessage(prev *metav1.Condition) string {
	if prev.Reason == v2alpha1.ReasonAwaitingProbe {
		return prev.Message
	}
	return fmt.Sprintf("job intake is limited to one probe job per pending-pod deadline window: "+
		"the declined worker pods were reaped before capacity returned (%s: %s); the gate reopens when a worker pod starts",
		prev.Reason, truncate(prev.Message, 200))
}

// evalCapacityGate resolves an enabled gate to a verdict, the condition reason and
// message that explain it, and the re-check interval its signal needs.
//
// Two inputs, from two parties (Q470). The MODE is the tenant's: how hard should this
// set try not to claim work it cannot run. The SIGNAL is chosen from the platform
// operator's cluster fact, because whether an unschedulable pod is waste or a pending
// request is a property of the cluster, not of the set — and a tenant asked to assert
// it would be speaking for infrastructure they may not own. Splitting them is what
// makes the dangerous combination (gating on the scheduler's verdict where an
// autoscaler was about to add a node) unrepresentable rather than merely documented.
//
// An unrecognized mode fails OPEN rather than falling through to an implemented one.
// The CRDs ship as their own chart and can be upgraded ahead of the AGC, so a mode a
// newer CRD accepts can reach an older AGC; treating it as "some gate, near enough"
// would apply semantics the operator did not ask for.
func (r *RunnerSetReconciler) evalCapacityGate(ctx context.Context, mode string, unsched workersUnschedulable, gw *v2alpha1.ActionsGateway) (declined bool, reason, message string, recheck time.Duration) {
	if mode != v2alpha1.CapacityGateModeObserve {
		// Nothing is evaluated, so nothing is worth coming back for either.
		return false, v2alpha1.ReasonGateModeUnsupported, fmt.Sprintf(
			"capacity gate mode %q is not implemented by this AGC; job intake is not gated", mode), 0
	}
	// A bound worker pod that has not yet resolved either way is invisible to the Pod
	// watch, so every verdict below carries the re-check that will notice when it does.
	defer func() { recheck = soonest(recheck, startupRecheck(unsched)) }()

	// The kubelet's verdict comes first, and comes before the cluster fact splits the
	// other two signals, because it is the one signal no autoscaler can answer: these
	// pods are already placed, so nothing about a new node changes whether their image
	// resolves. Gating on it is sound on an elastic cluster and a fixed one alike.
	if len(unsched.notStartingPods) > 0 {
		return true, v2alpha1.ReasonPodsNotStarting, notStartingMessage(unsched.notStartingPods), 0
	}
	if gatewayNodeAutoscaling(gw) == v2alpha1.NodeAutoscalingAbsent {
		// The operator asserts nothing will add a node, so the scheduler's verdict is
		// final and a stuck pod is pure waste.
		if unsched.unschedulable {
			return true, v2alpha1.ReasonPodsUnschedulable, "job intake is gated: " + unsched.message, 0
		}
		return false, v2alpha1.ReasonCapacityAvailable,
			"the cluster can place this runner set's worker pods; job intake is not gated", 0
	}
	// A node may still arrive, so only the autoscaler's own declination proves one is
	// not coming. This is also the default, and it is the safe one: it can only ever
	// under-gate relative to the scheduler's verdict.
	return r.evalAutoscalerVerdictGate(ctx, unsched)
}

// startupVerdictRecheck is how often a gated set re-reads a bound worker pod that is
// still Pending with no startup verdict either way.
//
// A periodic re-check is required, not merely nice, for the reason the elastic path
// needs one: the Pod watch drops updates that change no phase, and a pod entering
// ImagePullBackOff changes none — so without it nothing would wake the reconciler
// between the pod's creation and its pendingPodDeadline reap, and the gate would
// learn of an unpullable image ten minutes after the kubelet did.
//
// It is a third of the elastic path's interval because it costs a third of nothing:
// that path spends uncached Event reads per stuck pod, while this one re-reads a pod
// list the reconcile already holds. §9j measured the kubelet reaching its verdict ~2s
// after the pod is created, so the interval — not the signal — is what bounds how
// long intake continues past the decision, and it bounds it at seconds against a
// ten-minute default deadline.
//
// Only the undecided direction needs it. Recovery does not: a pod that finally starts
// goes Pending→Running and a pod that is reaped is deleted, and the watch delivers
// both.
const startupVerdictRecheck = 10 * time.Second

// startupRecheck returns the re-check a not-yet-decided bound worker pod needs, or 0
// when every bound pod has already resolved one way or the other.
func startupRecheck(unsched workersUnschedulable) time.Duration {
	if unsched.startupPending {
		return startupVerdictRecheck
	}
	return 0
}

// notStartingObservation renders the kubelet's startup verdict as a plain observation:
// how many worker pods bound and failed to start, and each one's own waiting message,
// which is what names the image that will not pull.
//
// The observation and the intake decision are separate strings on purpose (Q906).
// WorkersNotStarting reports this fact on every set and decides nothing, so its message
// must not say intake is gated — on the default ungated set it is not.
func notStartingObservation(pods []notStartingPod) string {
	parts := make([]string, 0, len(pods))
	for _, p := range pods {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.name, p.detail))
	}
	return fmt.Sprintf("%d worker pod(s) were placed on a node but could not be started: %s",
		len(pods), strings.Join(parts, "; "))
}

// notStartingMessage is the gated form: the same observation, prefixed with the
// consequence only a set with a capacity gate has.
func notStartingMessage(pods []notStartingPod) string {
	return "job intake is gated: " + notStartingObservation(pods)
}

// gatewayNodeAutoscaling returns the gateway's effective node-autoscaling fact,
// applying the Present default for an unset spec.clusterCapacity (an older object
// stored before the field existed, or a gateway the AGC could not resolve).
//
// Present is the fail-safe direction rather than the common one: under it the gate
// refuses intake only on an explicit autoscaler declination, so a missing or wrong
// value can only under-gate — which is today's behavior — whereas defaulting to
// Absent would gate on the scheduler's verdict alone and refuse jobs on any elastic
// cluster that had not set the field.
func gatewayNodeAutoscaling(gw *v2alpha1.ActionsGateway) string {
	if gw == nil || gw.Spec.ClusterCapacity == nil || gw.Spec.ClusterCapacity.NodeAutoscaling == "" {
		return v2alpha1.NodeAutoscalingPresent
	}
	return gw.Spec.ClusterCapacity.NodeAutoscaling
}

// autoscalerVerdictRecheck is how often a gated set on an elastic cluster re-reads its
// stuck pods' Events while any pod is stuck.
//
// A periodic re-check is required rather than merely nice: the signal lives in Event
// objects, which nothing in the AGC watches (a cluster-wide Event informer is the load
// problem this mode is designed around), so neither the autoscaler's declination
// arriving nor its later scale-up would otherwise re-trigger a reconcile. Both
// directions matter — without it the gate would close late, and, worse, would stay
// closed after the autoscaler started acting.
const autoscalerVerdictRecheck = 30 * time.Second

// maxAutoscalerVerdictPodReads bounds the uncached Event reads one reconcile may spend.
// One recognized declination is enough to close the gate, and the pods are sorted
// oldest-first, so a set with a hundred stuck pods pays the same bounded cost as one
// with eight and still reads the pods most likely to carry a settled verdict. The
// truncation can only under-gate, which is the safe direction.
const maxAutoscalerVerdictPodReads = 8

// evalAutoscalerVerdictGate is the elastic-cluster path (Q406): gate only when the autoscaler
// itself has recorded, against one of this set's stuck worker pods, that it will not
// add a node for it.
//
// Reads are scoped twice over — only for pods the WorkersUnschedulable evaluation
// already found stuck past the scheduling grace, and only up to
// maxAutoscalerVerdictPodReads of them — so a healthy set costs nothing at all and a
// badly-stuck one costs a bounded handful of field-selected reads per re-check.
//
// Fail-open at every step: no stuck pods, no wired Event reader, an unreadable Event
// list, and an autoscaler whose vocabulary is not recognized all leave intake exactly
// as it is today. A read error does not abort the scan — a later pod may still carry a
// recognized declination — but it is reported in the message when nothing gated, so an
// operator sees "the gate could not evaluate" rather than a bare "capacity available".
func (r *RunnerSetReconciler) evalAutoscalerVerdictGate(ctx context.Context, unsched workersUnschedulable) (bool, string, string, time.Duration) {
	if len(unsched.stuckPods) == 0 {
		return false, v2alpha1.ReasonCapacityAvailable,
			"no worker pod is waiting on the cluster autoscaler; job intake is not gated", 0
	}
	if r.EventReader == nil {
		return false, v2alpha1.ReasonCapacityAvailable,
			"cannot read worker pod Events (no direct API reader is wired); job intake is not gated",
			autoscalerVerdictRecheck
	}

	pods := unsched.stuckPods
	truncated := 0
	if len(pods) > maxAutoscalerVerdictPodReads {
		truncated = len(pods) - maxAutoscalerVerdictPodReads
		pods = pods[:maxAutoscalerVerdictPodReads]
	}

	var readErr error
	for _, pod := range pods {
		evts, err := r.podEvents(ctx, pod)
		if err != nil {
			if readErr == nil {
				readErr = err
			}
			continue
		}
		if declined, detail := autoscalerDeclination(pod, evts); declined {
			return true, v2alpha1.ReasonScaleUpDeclined, fmt.Sprintf(
				"job intake is gated: the cluster autoscaler declined to add a node for worker pod %s: %s",
				pod.Name, detail), autoscalerVerdictRecheck
		}
	}

	if readErr != nil {
		return false, v2alpha1.ReasonCapacityAvailable, fmt.Sprintf(
			"could not read worker pod Events (%v); job intake is not gated", readErr), autoscalerVerdictRecheck
	}
	msg := fmt.Sprintf("%d worker pod(s) are unschedulable, but no cluster autoscaler has declined to add a node "+
		"for them; job intake is not gated", len(unsched.stuckPods))
	if truncated > 0 {
		msg += fmt.Sprintf(" (Events checked for the %d oldest; %d not checked this pass)", len(pods), truncated)
	}
	return false, v2alpha1.ReasonCapacityAvailable, msg, autoscalerVerdictRecheck
}

// podEvents lists the Events recorded against one pod, field-selected server-side to
// that pod alone and read through the UNCACHED EventReader.
//
// Uncached is the point: the alternative is an Event informer, and Events are the
// highest-churn object in a busy cluster, so caching them would trade a bounded
// per-stuck-pod read for an unbounded steady-state memory and watch cost on every AGC
// — including the vast majority that never enable this mode. The UID is part of the
// selector so a recycled pod name cannot inherit a previous pod's verdict.
func (r *RunnerSetReconciler) podEvents(ctx context.Context, pod *corev1.Pod) ([]corev1.Event, error) {
	var list corev1.EventList
	if err := r.EventReader.List(ctx, &list,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{
			"involvedObject.name": pod.Name,
			"involvedObject.uid":  string(pod.UID),
		},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// clearWorkerCapacityConditions resets the worker-capacity conditions to their benign
// (False) state. Called when references do not resolve: no listeners run and no new
// worker pods are provisioned, so a previously-True capacity condition must not linger
// as a stale alarm behind the dominant Ready=False/<Ref>NotFound signal. Mirrors
// setReapBlockingSidecarStatus(nil)'s clear-on-unresolved behavior.
func (r *RunnerSetReconciler) clearWorkerCapacityConditions(rs *v2alpha1.RunnerSet) {
	const unresolvedMsg = "references unresolved; no worker pods are provisioned"
	for _, c := range []struct {
		condType string
		reason   string
	}{
		{v2alpha1.ConditionWorkerQuotaPressure, "QuotaHeadroomSufficient"},
		{v2alpha1.ConditionWorkerQuotaExceeded, "NoRejection"},
		{v2alpha1.ConditionWorkersUnschedulable, v2alpha1.ReasonWorkersSchedulable},
		{v2alpha1.ConditionWorkersNotStarting, v2alpha1.ReasonWorkersStarting},
	} {
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{
			Type:               c.condType,
			Status:             metav1.ConditionFalse,
			Reason:             c.reason,
			Message:            unresolvedMsg,
			ObservedGeneration: rs.Generation,
		})
	}
	// The capacity gate reads a condition, so a stale True would keep intake gated for
	// a set that is provisioning nothing anyway. Remove it rather than set it False,
	// matching applyCapacityGateCondition's contract that the condition is present only
	// while a gate is actually being evaluated.
	meta.RemoveStatusCondition(&rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
}

// evalRunnerSetWorkerQuota computes the worker namespace-quota conditions for a
// RunnerSet: the footprint comes from the resolved RunnerTemplate's PodTemplate (the
// v2 source of the v1 spec.podTemplate) with the sizing profile applied, and the
// ceiling from the set's priorityTiers/maxWorkers. See evalWorkerQuotaFor.
func (r *RunnerSetReconciler) evalRunnerSetWorkerQuota(ctx context.Context, rs *v2alpha1.RunnerSet, tmpl *v2alpha1.RunnerTemplateSpec) workerQuotaConditions {
	ceiling, bounded := provisioner.WorkerCeilingFromTiers(runnerSetTierThresholds(rs.Spec.PriorityTiers), rs.Spec.MaxWorkers)
	return evalWorkerQuotaFor(ctx, r.Client, workerQuotaPool{
		namespace: rs.Namespace,
		podSpec:   runnerSetWorkerPodSpec(rs, tmpl),
		ceiling:   ceiling,
		bounded:   bounded,
		label:     provisioner.LabelRunnerSet,
		name:      rs.Name,
	})
}

// runnerSetWorkerPodSpec returns the pod spec of the worker this set would provision
// right now: the resolved template's pod spec with the sizing profile applied
// (Q359 Phase 3), exactly as runnerSetTarget.Resolve builds it. Shared by the
// WorkerQuota conditions and the admission gate's quota rung (#784) so both size a
// worker identically and cannot contradict each other on a Binpack/Throughput
// profile. Static/no profile passes the template through untouched. A nil template
// (references unresolved) yields nil.
//
// The whole spec, not just its containers: a worker's quota charge includes its
// native sidecars and its RuntimeClass overhead, and the DinD/Kata shapes this set
// most often carries declare the expensive half as a native sidecar (Q450).
func runnerSetWorkerPodSpec(rs *v2alpha1.RunnerSet, tmpl *v2alpha1.RunnerTemplateSpec) *corev1.PodSpec {
	if tmpl == nil {
		return nil
	}
	sized := applySizingProfile(tmpl.PodTemplate, rs.Spec.Sizing, tmpl, rs.Status.SizingRecommendation)
	return &sized.Spec
}

// quotaToRunnerSets maps a ResourceQuota event to every RunnerSet in the same
// namespace, so an admin changing the namespace quota refreshes the WorkerQuota
// conditions promptly (Q82/Q326) — mirrors the v1 quotaToRunnerGroups. The cache
// list is already gatewayRef-scoped on a GMC-provisioned AGC, and the Reconcile
// scoping guard drops any foreign set as defense-in-depth.
func (r *RunnerSetReconciler) quotaToRunnerSets(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.runnerSetsMatching(ctx, obj.GetNamespace(), func(*v2alpha1.RunnerSet) bool { return true })
}

// evalRunnerSetWorkersUnschedulable computes the WorkersUnschedulable condition for a
// RunnerSet, mirroring RunnerGroupReconciler.evalWorkersUnschedulable: True when at
// least one worker pod has sat Pending past the scheduling grace because the scheduler
// could not place it (PodScheduled=False/Unschedulable). A list failure yields a
// schedulable (False) result — the absence of evidence is not an alarm and the next
// reconcile retries.
func (r *RunnerSetReconciler) evalRunnerSetWorkersUnschedulable(ctx context.Context, rs *v2alpha1.RunnerSet) workersUnschedulable {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(rs.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerSet: rs.Name},
	); err != nil {
		return workersUnschedulable{
			reason:  v2alpha1.ReasonWorkersSchedulable,
			message: fmt.Sprintf("could not list worker pods: %v", err),
		}
	}
	return evalWorkersUnschedulableForPods(pods.Items, r.nowFunc()(), runnerSetUnschedulableGrace(rs),
		v2alpha1.ReasonWorkersSchedulable, v2alpha1.ReasonPodsUnschedulable)
}

// runnerSetUnschedulableGrace is how long a worker pod must sit Pending+Unschedulable
// before WorkersUnschedulable trips — half the set's pendingPodDeadline, so the
// condition latches and stays stable for a window before the reaper deletes the pod at
// the full deadline (mirrors unschedulableGrace for the v1 RunnerGroup; see its doc for
// why the factor of one-half avoids flapping).
func runnerSetUnschedulableGrace(rs *v2alpha1.RunnerSet) time.Duration {
	d := provisioner.PendingPodDeadlineOrDefault(rs.Spec.PendingPodDeadline) / 2
	if d <= 0 {
		d = time.Second
	}
	return d
}
