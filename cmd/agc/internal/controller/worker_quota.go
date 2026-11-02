package controller

import (
	"context"
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Worker ResourceQuota conditions (Q82). These follow the same two-tier
// convention as the proxy conditions on the ActionsGateway (see
// docs/development/kubernetes-conventions.md): a warning tier
// (WorkerQuotaPressure) and an error tier (WorkerQuotaExceeded), abnormal-is-True
// and mutually exclusive. They are scoped to the namespace ResourceQuota — the
// silent failure mode Q59's configured-ceiling admission gate does not cover
// (when the quota is tighter than maxWorkers, the gate never fires but worker
// pod creates are quota-rejected).
//
// workerQuotaConditions carries the computed status of the two worker
// namespace-quota conditions (mutually exclusive; error supersedes warning).
type workerQuotaConditions struct {
	pressure        bool
	pressureReason  string
	pressureMessage string
	exceeded        bool
	exceededReason  string
	exceededMessage string
}

// workerQuotaPool is the owner-shaped half of the worker-quota evaluation: the facts
// that differ between owners. The caller resolves them
// from its own spec; everything downstream of that is common.
type workerQuotaPool struct {
	namespace string
	// podSpec is the shape of the worker this pool would provision right now. Nil
	// (references unresolved) sizes an empty footprint.
	podSpec *corev1.PodSpec
	// ceiling is the pool's configured worker ceiling; bounded is false when it has none.
	ceiling int32
	bounded bool
	// label and name select this pool's worker pods.
	label string
	name  string
}

// evalWorkerQuotaFor computes WorkerQuotaPressure (warning) and WorkerQuotaExceeded
// (error) against the platform-owned namespace ResourceQuota. Both are advisory
// and do NOT gate Ready.
//
//   - WorkerQuotaExceeded (error): the quota cannot admit even one more worker
//     pod (remaining headroom < a single worker's footprint) — the next acquired
//     job's pod will be rejected.
//   - WorkerQuotaPressure (warning): the pool cannot grow from its current worker
//     count up to the configured ceiling (maxWorkers / max priorityTier
//     threshold) within current quota headroom.
//
// Both read live quota .status (hard − used), so they move with namespace load —
// a warning-grade signal, not a stable invariant. The headroom check ignores
// quota scopes; face-value hard/used is sufficient for an advisory signal.
//
// Shared by the v1 RunnerGroup and v2 RunnerSet evaluations so the two publish the
// same reasons and messages from the same arithmetic.
func evalWorkerQuotaFor(ctx context.Context, c client.Client, pool workerQuotaPool) workerQuotaConditions {
	st := workerQuotaConditions{
		pressureReason:  "QuotaHeadroomSufficient",
		pressureMessage: "namespace ResourceQuota admits scaling workers to the configured ceiling",
		exceededReason:  "NoRejection",
		exceededMessage: "namespace ResourceQuota can admit more worker pods",
	}

	var quotas corev1.ResourceQuotaList
	if err := c.List(ctx, &quotas, client.InNamespace(pool.namespace)); err != nil {
		st.pressureReason = "QuotaUnknown"
		st.pressureMessage = fmt.Sprintf("could not read namespace ResourceQuota: %v", err)
		return st
	}
	if len(quotas.Items) == 0 {
		st.pressureReason = "NoQuota"
		st.pressureMessage = "no namespace ResourceQuota constrains worker pods"
		return st
	}

	// Resolve the pod shape ONCE — the error and warning tiers below must size a
	// worker identically, and this is what folds in RuntimeClass overhead alongside
	// the native sidecars the footprint arithmetic already counts (Q450).
	spec := provisioner.ResolveWorkerPodSpec(ctx, c, pool.podSpec)

	// Error tier — can the quota admit even one more worker pod?
	if over, msg := provisioner.QuotaHeadroomViolations(provisioner.WorkerFootprint(spec, 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod; new jobs will be rejected: "); over {
		st.exceeded = true
		st.exceededReason = "QuotaExhausted"
		st.exceededMessage = msg
	}

	// Warning tier — can the pool still grow to its ceiling?
	if pool.bounded {
		current := countActiveWorkerPodsByLabel(ctx, c, pool.namespace, pool.label, pool.name)
		if additional := pool.ceiling - current; additional > 0 {
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

// countActiveWorkerPodsByLabel counts the worker pods in namespace ns selected by
// label==name that count toward a pool's ceiling: non-terminal (Pending/Running)
// and not being deleted. Terminal pods awaiting reaping do not count toward the
// ceiling (they still consume the quota's `used`, which the headroom check reads
// separately and conservatively).
func countActiveWorkerPodsByLabel(ctx context.Context, c client.Reader, ns, label, name string) int32 {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(ns),
		client.MatchingLabels{label: name},
	); err != nil {
		return 0
	}
	var n int32
	for i := range pods.Items {
		p := &pods.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
			continue
		}
		n++
	}
	return n
}

// --- metrics ---------------------------------------------------------------

// conditionGaugeValue maps a status condition to a gauge value: 1 when present
// and True, 0 otherwise — the project convention for exporting an alertable CRD
// condition as a controller-owned metric.
func conditionGaugeValue(conds []metav1.Condition, condType string) float64 {
	if meta.IsStatusConditionTrue(conds, condType) {
		return 1
	}
	return 0
}

// boolConditionStatus maps a Go bool to a metav1.ConditionStatus.
func boolConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// quotaHardChangedPredicate enqueues ResourceQuota create/delete and only those
// updates that change .spec.hard, ignoring the high-frequency .status.used churn.
func quotaHardChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldQ, ok1 := e.ObjectOld.(*corev1.ResourceQuota)
			newQ, ok2 := e.ObjectNew.(*corev1.ResourceQuota)
			if !ok1 || !ok2 {
				return true
			}
			return !resourceListEqual(oldQ.Spec.Hard, newQ.Spec.Hard)
		},
	}
}

// resourceListEqual reports whether two ResourceLists hold the same keys with
// numerically equal quantities. reflect.DeepEqual is unsuitable: resource.Quantity
// caches a formatted string in an unexported field.
func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va.Cmp(vb) != 0 {
			return false
		}
	}
	return true
}
