package provisioner

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Namespace-ResourceQuota headroom arithmetic, shared by the two consumers that
// must agree on it:
//
//   - the pre-acquisition admission gate (Q59/#784), which refuses to claim a job
//     when the quota cannot admit the worker pod that job would need, and
//   - the advisory WorkerQuota{Pressure,Exceeded} conditions (Q82) the v1
//     RunnerGroup and v2 RunnerSet reconcilers publish.
//
// It lives in the provisioner package because the gate does — the controller
// package already imports provisioner, so the reverse direction would cycle — and
// because the footprint must reflect the pod the provisioner would actually build,
// resource defaults included.

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

// WorkerFootprint returns the quota footprint of `count` worker pods built from
// the given containers: the per-pod container requests/limits (summed across
// containers, after the same resource gap-fill buildPod stamps) scaled by count,
// plus the pod count. Keys mirror ResourceQuota hard keys. Linear in count.
//
// Owner-agnostic so the v1 RunnerGroup (spec.podTemplate) and v2 RunnerSet
// (resolved RunnerTemplate) capacity checks share one footprint calc. Init
// containers are excluded, matching applyResourceDefaults: the wrapper init
// container is short-lived and the quota's `used` tracks the max of init and
// regular requests, which for a worker pod is the runner container's.
func WorkerFootprint(containers []corev1.Container, count int32) corev1.ResourceList {
	if count < 0 {
		count = 0
	}
	var reqCPU, reqMem, limCPU, limMem resource.Quantity
	for i := range containers {
		res := gapFilledResources(&containers[i])
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

// QuotaHeadroomViolations reports whether `demand` exceeds the remaining headroom
// (hard − used) of any quota for any mapped resource, with a human-readable
// message. Mirrors the GMC proxy headroom check; the logic is intentionally
// duplicated there because the two controllers live in separate Go modules and the
// shared convention (not shared code) keeps them consistent.
//
// Quota scopes are ignored: face-value hard/used is a conservative approximation,
// and both callers treat a miss as fail-open (see WorkerQuotaExhausted).
func QuotaHeadroomViolations(demand corev1.ResourceList, quotas []corev1.ResourceQuota, msgPrefix string) (bool, string) {
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

// WorkerQuotaExhausted reports whether the namespace ResourceQuotas in ns
// currently lack the headroom to admit one more worker pod with the given
// containers, plus a human-readable detail naming the binding resource.
//
// It is the live read behind the admission gate's quota rung (#784). Deliberately
// FAIL-OPEN: a quota it cannot list, or a namespace with no quota at all, yields
// (false, "") so the gate keeps today's behaviour and the provisioner's
// maxQuotaRetries loop remains the backstop. The read is cache-backed (both
// reconcilers already watch ResourceQuota), so it is cheap enough to run once per
// delivered job, and fresher than the WorkerQuotaExceeded condition it mirrors —
// which is only as current as the last reconcile.
//
// The signal is not authoritative and does not try to be: `.status.used` is
// eventually consistent, a sibling AGC can consume the headroom between this read
// and pod creation, and quota scopes are ignored. Closing the gate on it converts
// the common case — a tenant sitting at its own quota ceiling — from "claim the job
// and burn lock time retrying" into "leave it queued at GitHub", which is what the
// Q59 gate exists to do.
func WorkerQuotaExhausted(ctx context.Context, c client.Reader, ns string, containers []corev1.Container) (exhausted bool, detail string) {
	var quotas corev1.ResourceQuotaList
	if err := c.List(ctx, &quotas, client.InNamespace(ns)); err != nil {
		return false, ""
	}
	if len(quotas.Items) == 0 {
		return false, ""
	}
	return QuotaHeadroomViolations(WorkerFootprint(containers, 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod: ")
}
