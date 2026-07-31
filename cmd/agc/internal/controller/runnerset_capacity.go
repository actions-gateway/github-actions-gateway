package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
// Both reuse the owner-agnostic cores those v1 files now expose
// (workerFootprintForContainers, countActiveWorkerPodsByLabel, quotaHeadroomViolations,
// evalWorkersUnschedulableForPods). Only the v2 sources of the pod shape (the resolved
// RunnerTemplate) and the ceiling (spec.priorityTiers/maxWorkers) differ. Neither
// condition gates Ready — they are advisory capacity signals, mirroring v1.

// applyWorkerCapacityConditions computes and merges the WorkerQuota ladder and the
// WorkersUnschedulable condition onto the RunnerSet status, emitting a Warning Event
// on a genuine WorkersUnschedulable False→True transition (never every reconcile).
// It is called on both acquisition paths (classic and scale-set) after references
// resolve, with the resolved worker template supplying the quota footprint and the
// resolved gateway supplying the cluster facts the capacity gate depends on (Q470).
// It returns the soonest re-check needed for a still-within-grace Pending worker pod
// to cross its scheduling grace (0 = none), which the caller folds into RequeueAfter
// so WorkersUnschedulable flips without waiting for a phase-changing Pod event (Q157).
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

	gateRecheck := r.applyCapacityGateCondition(ctx, rs, unsched, gw)
	return soonest(unsched.requeueAfter, gateRecheck)
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
		// state polls at the same cadence the elastic signal already uses.
		recheck = autoscalerVerdictRecheck
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
//   - No worker pod has scheduled since the condition became True. A post-decline
//     binding — whenever the pod was created — is capacity returning, and clears.
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
	return !unsched.lastScheduledAt.After(prev.LastTransitionTime.Time)
}

// latchedCapacityMessage carries the reaped verdict into the latched condition, so
// the operator still sees WHICH signal declined after its pod is gone. A latch
// re-published over itself keeps its message rather than re-wrapping it.
func latchedCapacityMessage(prev *metav1.Condition) string {
	if prev.Reason == v2alpha1.ReasonAwaitingProbe {
		return prev.Message
	}
	return fmt.Sprintf("job intake is limited to one probe job per pending-pod deadline window: "+
		"the declined worker pods were reaped before capacity returned (%s: %s); the gate reopens when a worker pod schedules",
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
		return false, v2alpha1.ReasonGateModeUnsupported, fmt.Sprintf(
			"capacity gate mode %q is not implemented by this AGC; job intake is not gated", mode), 0
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

// evalRunnerSetWorkerQuota computes the WorkerQuotaPressure (warning) and
// WorkerQuotaExceeded (error) conditions for a RunnerSet against the platform-owned
// namespace ResourceQuota, mirroring RunnerGroupReconciler.evalWorkerQuota. The pod
// footprint comes from the resolved RunnerTemplate's PodTemplate (the v2 source of the
// v1 spec.podTemplate), and the ceiling from the set's priorityTiers/maxWorkers. Both
// tiers read live quota .status (hard − used), so they move with namespace load — an
// advisory signal, not a stable invariant — and neither gates Ready.
func (r *RunnerSetReconciler) evalRunnerSetWorkerQuota(ctx context.Context, rs *v2alpha1.RunnerSet, tmpl *v2alpha1.RunnerTemplateSpec) workerQuotaConditions {
	st := workerQuotaConditions{
		pressureReason:  "QuotaHeadroomSufficient",
		pressureMessage: "namespace ResourceQuota admits scaling workers to the configured ceiling",
		exceededReason:  "NoRejection",
		exceededMessage: "namespace ResourceQuota can admit more worker pods",
	}

	// Size the footprint off the SAME shape the Target would provision — sizing
	// profile applied (Q359 Phase 3) — so these conditions and the admission gate's
	// quota rung (#784) never contradict each other for a set on a Binpack/Throughput
	// profile. Static/no profile passes the template through untouched.
	podSpec := runnerSetWorkerPodSpec(rs, tmpl)

	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(rs.Namespace)); err != nil {
		st.pressureReason = "QuotaUnknown"
		st.pressureMessage = fmt.Sprintf("could not read namespace ResourceQuota: %v", err)
		return st
	}
	if len(quotas.Items) == 0 {
		st.pressureReason = "NoQuota"
		st.pressureMessage = "no namespace ResourceQuota constrains worker pods"
		return st
	}

	// Resolve the pod shape ONCE, so the error and warning tiers below size a worker
	// identically and both see the RuntimeClass overhead a Kata set carries (Q450).
	spec := provisioner.ResolveWorkerPodSpec(ctx, r.Client, podSpec)

	// Error tier — can the quota admit even one more worker pod?
	if over, msg := provisioner.QuotaHeadroomViolations(provisioner.WorkerFootprint(spec, 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod; new jobs will be rejected: "); over {
		st.exceeded = true
		st.exceededReason = "QuotaExhausted"
		st.exceededMessage = msg
	}

	// Warning tier — can the pool still grow to its ceiling?
	if ceiling, bounded := provisioner.WorkerCeilingFromTiers(runnerSetTierThresholds(rs.Spec.PriorityTiers), rs.Spec.MaxWorkers); bounded {
		current := countActiveWorkerPodsByLabel(ctx, r.Client, rs.Namespace, provisioner.LabelRunnerSet, rs.Name)
		if additional := ceiling - current; additional > 0 {
			if over, msg := provisioner.QuotaHeadroomViolations(provisioner.WorkerFootprint(spec, additional), quotas.Items,
				"workers cannot scale to the configured ceiling with current quota headroom: "); over {
				st.pressure = true
				st.pressureReason = "InsufficientQuotaHeadroom"
				st.pressureMessage = msg
			}
		}
	}

	if st.exceeded {
		st.pressure = false
		st.pressureReason = "Superseded"
		st.pressureMessage = "superseded by WorkerQuotaExceeded"
	}
	return st
}

// runnerSetWorkerPodSpec returns the pod spec of the worker this set would provision
// right now: the resolved template's pod spec with the sizing profile applied,
// exactly as runnerSetTarget.Resolve builds it. Shared by the WorkerQuota conditions
// and the admission gate's quota rung so both size a worker identically. A nil
// template (references unresolved) yields nil.
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
	var list v2alpha1.RunnerSetList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace,
			Name:      list.Items[i].Name,
		}})
	}
	return reqs
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
