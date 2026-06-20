package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
const (
	conditionWorkerQuotaPressure = "WorkerQuotaPressure"
	conditionWorkerQuotaExceeded = "WorkerQuotaExceeded"
)

// quotaCheck maps a worker-footprint resource to the ResourceQuota hard key that
// constrains it (including the legacy cpu/memory aliases for requests).
type quotaCheck struct {
	footprint corev1.ResourceName
	hardKey   corev1.ResourceName
}

var workerQuotaChecks = []quotaCheck{
	{corev1.ResourcePods, corev1.ResourcePods},
	{corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU},
	{corev1.ResourceRequestsCPU, corev1.ResourceCPU},
	{corev1.ResourceRequestsMemory, corev1.ResourceRequestsMemory},
	{corev1.ResourceRequestsMemory, corev1.ResourceMemory},
	{corev1.ResourceLimitsCPU, corev1.ResourceLimitsCPU},
	{corev1.ResourceLimitsMemory, corev1.ResourceLimitsMemory},
}

// workerFootprint returns the quota footprint of `count` worker pods: the
// per-pod container requests/limits (summed across containers) scaled by count,
// plus the pod count. Keys mirror ResourceQuota hard keys. Linear in count.
func workerFootprint(rg *v1alpha1.RunnerGroup, count int32) corev1.ResourceList {
	if count < 0 {
		count = 0
	}
	var reqCPU, reqMem, limCPU, limMem resource.Quantity
	for i := range rg.Spec.PodTemplate.Spec.Containers {
		res := rg.Spec.PodTemplate.Spec.Containers[i].Resources
		reqCPU.Add(res.Requests[corev1.ResourceCPU])
		reqMem.Add(res.Requests[corev1.ResourceMemory])
		limCPU.Add(res.Limits[corev1.ResourceCPU])
		limMem.Add(res.Limits[corev1.ResourceMemory])
	}
	out := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(int64(count), resource.DecimalSI),
	}
	add := func(key corev1.ResourceName, per resource.Quantity) {
		if per.IsZero() {
			return
		}
		out[key] = mulQuantity(per, int64(count))
	}
	add(corev1.ResourceRequestsCPU, reqCPU)
	add(corev1.ResourceRequestsMemory, reqMem)
	add(corev1.ResourceLimitsCPU, limCPU)
	add(corev1.ResourceLimitsMemory, limMem)
	return out
}

// mulQuantity returns q multiplied by n via repeated addition (n is bounded by
// the worker ceiling). resource.Quantity has no scalar-multiply primitive that
// preserves the canonical form across DecimalSI and BinarySI.
func mulQuantity(q resource.Quantity, n int64) resource.Quantity {
	out := resource.Quantity{Format: q.Format}
	for i := int64(0); i < n; i++ {
		out.Add(q)
	}
	return out
}

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

	// Error tier — can the quota admit even one more worker pod?
	if over, msg := quotaHeadroomViolations(workerFootprint(rg, 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod; new jobs will be rejected: "); over {
		st.exceeded = true
		st.exceededReason = "QuotaExhausted"
		st.exceededMessage = msg
	}

	// Warning tier — can the pool still grow to its ceiling?
	if ceiling, bounded := provisioner.WorkerCeiling(rg); bounded {
		current := r.countActiveWorkerPods(ctx, rg)
		if additional := ceiling - current; additional > 0 {
			if over, msg := quotaHeadroomViolations(workerFootprint(rg, additional), quotas.Items,
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
// its ceiling: non-terminal (Pending/Running) and not being deleted. Terminal
// pods awaiting reaping do not count toward the ceiling (they still consume the
// quota's `used`, which the headroom check reads separately and conservatively).
func (r *RunnerGroupReconciler) countActiveWorkerPods(ctx context.Context, rg *v1alpha1.RunnerGroup) int32 {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(rg.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerGroup: rg.Name},
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

// quotaHeadroomViolations reports whether `demand` exceeds the remaining headroom
// (hard − used) of any quota for any mapped resource, with a human-readable
// message. Mirrors the GMC proxy headroom check; the logic is intentionally
// duplicated because the two controllers live in separate Go modules and the
// shared convention (not shared code) keeps them consistent.
func quotaHeadroomViolations(demand corev1.ResourceList, quotas []corev1.ResourceQuota, msgPrefix string) (bool, string) {
	var violations []string
	for i := range quotas {
		q := &quotas[i]
		hard := q.Status.Hard
		if len(hard) == 0 {
			hard = q.Spec.Hard
		}
		for _, c := range workerQuotaChecks {
			need, ok := demand[c.footprint]
			if !ok {
				continue
			}
			limit, ok := hard[c.hardKey]
			if !ok {
				continue
			}
			remaining := limit.DeepCopy()
			if u, ok := q.Status.Used[c.hardKey]; ok {
				remaining.Sub(u)
			}
			if need.Cmp(remaining) > 0 {
				violations = append(violations, fmt.Sprintf(
					"needs %s more %s but quota %q has %s free", need.String(), c.hardKey, q.Name, remaining.String()))
			}
		}
	}
	if len(violations) == 0 {
		return false, ""
	}
	return true, msgPrefix + strings.Join(violations, "; ")
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
