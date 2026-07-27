package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
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
// The condition types are defined as exported consts in the api package so the
// reconciler, metrics collector, and tests share one source of truth (Q156).
// These package-local aliases keep the existing call sites terse.
const (
	conditionWorkerQuotaPressure = v1alpha1.ConditionWorkerQuotaPressure
	conditionWorkerQuotaExceeded = v1alpha1.ConditionWorkerQuotaExceeded
)

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

// evalWorkerQuota computes WorkerQuotaPressure (warning) and WorkerQuotaExceeded
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
func (r *RunnerGroupReconciler) evalWorkerQuota(ctx context.Context, rg *v1alpha1.RunnerGroup) workerQuotaConditions {
	st := workerQuotaConditions{
		pressureReason:  "QuotaHeadroomSufficient",
		pressureMessage: "namespace ResourceQuota admits scaling workers to the configured ceiling",
		exceededReason:  "NoRejection",
		exceededMessage: "namespace ResourceQuota can admit more worker pods",
	}

	var quotas corev1.ResourceQuotaList
	if err := r.List(ctx, &quotas, client.InNamespace(rg.Namespace)); err != nil {
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
	spec := provisioner.ResolveWorkerPodSpec(ctx, r.Client, &rg.Spec.PodTemplate.Spec)

	// Error tier — can the quota admit even one more worker pod?
	if over, msg := provisioner.QuotaHeadroomViolations(provisioner.WorkerFootprint(spec, 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod; new jobs will be rejected: "); over {
		st.exceeded = true
		st.exceededReason = "QuotaExhausted"
		st.exceededMessage = msg
	}

	// Warning tier — can the pool still grow to its ceiling?
	if ceiling, bounded := provisioner.WorkerCeiling(rg); bounded {
		current := r.countActiveWorkerPods(ctx, rg)
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

// countActiveWorkerPods counts this RunnerGroup's worker pods that count toward
// its ceiling. See countActiveWorkerPodsByLabel.
func (r *RunnerGroupReconciler) countActiveWorkerPods(ctx context.Context, rg *v1alpha1.RunnerGroup) int32 {
	return countActiveWorkerPodsByLabel(ctx, r.Client, rg.Namespace, provisioner.LabelRunnerGroup, rg.Name)
}

// countActiveWorkerPodsByLabel counts the worker pods in namespace ns selected by
// label==name that count toward a pool's ceiling: non-terminal (Pending/Running)
// and not being deleted. Terminal pods awaiting reaping do not count toward the
// ceiling (they still consume the quota's `used`, which the headroom check reads
// separately and conservatively). Owner-agnostic (LabelRunnerGroup for v1,
// LabelRunnerSet for v2) so both capacity checks count the same way.
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

// workerQuotaCollector exports the worker ResourceQuota conditions (Q82) as
// gauges so operators can alert on them without kube-state-metrics scraping CRD
// conditions. It reads at scrape time from the cached reader: a deleted
// RunnerGroup stops being listed, so its series disappears with no reconcile-path
// cost and no stale-series cleanup. The value mirrors the condition the
// reconciler wrote to .status.conditions (1 when True, 0 otherwise).
type workerQuotaCollector struct {
	reader   client.Reader
	pressure *prometheus.Desc
	exceeded *prometheus.Desc
}

func newWorkerQuotaCollector(reader client.Reader) *workerQuotaCollector {
	return &workerQuotaCollector{
		reader: reader,
		pressure: prometheus.NewDesc(
			"actions_gateway_worker_quota_pressure",
			"1 when the RunnerGroup WorkerQuotaPressure condition is True (workers cannot scale to the configured ceiling within the namespace ResourceQuota headroom), else 0.",
			[]string{"namespace", "runner_group"}, nil,
		),
		exceeded: prometheus.NewDesc(
			"actions_gateway_worker_quota_exceeded",
			"1 when the RunnerGroup WorkerQuotaExceeded condition is True (the namespace ResourceQuota cannot admit another worker pod), else 0.",
			[]string{"namespace", "runner_group"}, nil,
		),
	}
}

func (c *workerQuotaCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pressure
	ch <- c.exceeded
}

func (c *workerQuotaCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list v1alpha1.RunnerGroupList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		rg := &list.Items[i]
		if !rg.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.pressure, prometheus.GaugeValue,
			conditionGaugeValue(rg.Status.Conditions, conditionWorkerQuotaPressure), rg.Namespace, rg.Name)
		ch <- prometheus.MustNewConstMetric(c.exceeded, prometheus.GaugeValue,
			conditionGaugeValue(rg.Status.Conditions, conditionWorkerQuotaExceeded), rg.Namespace, rg.Name)
	}
}

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

// quotaToRunnerGroups maps a ResourceQuota event to every RunnerGroup in the same
// namespace, so an admin changing the namespace quota refreshes the worker-quota
// conditions (Q82).
func (r *RunnerGroupReconciler) quotaToRunnerGroups(ctx context.Context, obj client.Object) []ctrl.Request {
	var list v1alpha1.RunnerGroupList
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

// registerWorkerQuotaMetrics registers the worker-quota condition collector with
// the controller-runtime registry. It tolerates double registration (e.g. across
// test managers) by ignoring AlreadyRegisteredError.
func registerWorkerQuotaMetrics(reader client.Reader) {
	if err := crmetrics.Registry.Register(newWorkerQuotaCollector(reader)); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}
