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
// resolve, with the resolved worker template supplying the quota footprint. It
// returns the soonest re-check needed for a still-within-grace Pending worker pod to
// cross its scheduling grace (0 = none), which the caller folds into RequeueAfter so
// WorkersUnschedulable flips without waiting for a phase-changing Pod event (Q157).
func (r *RunnerSetReconciler) applyWorkerCapacityConditions(ctx context.Context, rs *v2alpha1.RunnerSet, tmpl *v2alpha1.RunnerTemplateSpec) time.Duration {
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

	r.applyCapacityGateCondition(rs, unsched)
	return unsched.requeueAfter
}

// applyCapacityGateCondition publishes the WorkerCapacityDeclined condition — the
// decision the admission ladder's placeability rung reads back (Q405). The rung does
// not re-derive the verdict; this condition IS the decision, so an operator's
// `kubectl describe` and the AGC's intake behavior cannot disagree.
//
// It reuses the WorkersUnschedulable evaluation the caller just computed rather than
// re-deriving from the pod list: in mode SchedulerVerdict the two read the same
// underlying fact (a worker pod Pending past the scheduling grace with
// PodScheduled=False/Unschedulable), and computing it twice would let them disagree
// across a single reconcile. Later modes (Q406/Q407) change the source feeding this
// condition without changing the condition or the rung.
//
// A set that did not opt in carries NO such condition at all: it is removed rather
// than published False. Absence is what the default costs — no cost, no stale alarm
// after opting out, and the condition's presence is itself the "this set has a
// capacity gate" signal.
func (r *RunnerSetReconciler) applyCapacityGateCondition(rs *v2alpha1.RunnerSet, unsched workersUnschedulable) {
	if runnerSetCapacityGateMode(rs) == v2alpha1.CapacityGateModeOff {
		meta.RemoveStatusCondition(&rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
		return
	}

	declined, reason, message := unsched.unschedulable, v2alpha1.ReasonCapacityAvailable,
		"the cluster can place this runner set's worker pods; job intake is not gated"
	if declined {
		reason = v2alpha1.ReasonPodsUnschedulable
		message = "job intake is gated: " + unsched.message
	}

	wasDeclined := meta.IsStatusConditionTrue(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
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
