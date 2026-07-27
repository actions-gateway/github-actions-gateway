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
//
// Every consumer sizes a worker from a whole *corev1.PodSpec, resolved through
// ResolveWorkerPodSpec, and never from spec.containers alone: native sidecars and
// RuntimeClass overhead are both quota-charged and both invisible in the container
// list (Q450). QuotaHeadroomPods and WorkerQuotaCapacity are the scale-set tier's
// integer form of the same question, so they must read the identical footprint.

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
// the given pod spec: the pod's effective per-pod requests/limits scaled by count,
// plus the pod count. Keys mirror ResourceQuota hard keys. Linear in count.
//
// Owner-agnostic so the v1 RunnerGroup (spec.podTemplate) and v2 RunnerSet
// (resolved RunnerTemplate) capacity checks share one footprint calc.
//
// It takes the whole PodSpec, not just the containers, because a worker pod's
// quota charge is not the container sum (Q450): native sidecars and pod overhead
// both count, and neither is visible in spec.containers. See
// pod_effective_resources.go for the arithmetic and its upstream provenance. Pass
// a spec that has been through ResolveWorkerPodSpec so .Overhead is populated.
func WorkerFootprint(spec *corev1.PodSpec, count int32) corev1.ResourceList {
	if count < 0 {
		count = 0
	}
	out := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(int64(count), resource.DecimalSI),
	}
	if spec == nil {
		return out
	}
	reqs := podEffectiveRequests(spec)
	limits := podEffectiveLimits(spec)
	add := func(key corev1.ResourceName, per resource.Quantity) {
		if per.IsZero() {
			return
		}
		out[key] = mulQuantity(per, int64(count))
	}
	add(corev1.ResourceRequestsCPU, reqs[corev1.ResourceCPU])
	add(corev1.ResourceRequestsMemory, reqs[corev1.ResourceMemory])
	add(corev1.ResourceLimitsCPU, limits[corev1.ResourceCPU])
	add(corev1.ResourceLimitsMemory, limits[corev1.ResourceMemory])
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

// QuotaHeadroomPods returns how many additional worker pods of the given shape the
// quotas can admit right now: the largest n in [0, max] whose WorkerFootprint clears
// QuotaHeadroomViolations. It is the *integer* form of the same question
// WorkerQuotaExhausted answers as a boolean, and it exists because the two acquisition
// tiers need different shapes of the one answer — the classic tier decides per
// delivered job ("is there room for one more?"), while the scale-set tier advertises a
// number of jobs per poll ("how many more fit?") (Q443).
//
// It is deliberately a search over the shared arithmetic rather than a division: the
// footprint spans several resources with different units and quantity formats, so
// reusing WorkerFootprint/QuotaHeadroomViolations is what guarantees the two tiers can
// never disagree about what fits. The predicate is monotone (the footprint is linear in
// n, so a violating n keeps violating), which makes the binary search exact in
// ⌈log2(max+1)⌉ evaluations. max is required and small in practice — the caller passes
// the remaining distance to its own worker ceiling — so the search is bounded work.
//
// Returns 0 when not even one more pod fits, including when a quota is already
// over-used. It reports nothing about read failures: a caller that cannot list quotas
// must fail open before calling (see WorkerQuotaCapacity).
func QuotaHeadroomPods(spec *corev1.PodSpec, quotas []corev1.ResourceQuota, max int32) int32 {
	if max <= 0 {
		return 0
	}
	fits := func(n int32) bool {
		over, _ := QuotaHeadroomViolations(WorkerFootprint(spec, n), quotas, "")
		return !over
	}
	lo, hi := int32(0), max
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// WorkerQuotaCapacity returns the total number of worker pods one owner may have in
// flight given live namespace-ResourceQuota headroom — its own non-terminal worker
// pods plus room for more — never reporting above max. It is the scale-set tier's
// expression of the admission gate's quota rung (Q443): that tier advertises a total
// (X-ScaleSetMaxCapacity bounds GitHub's totalAssignedJobs) where the classic tier
// decides per job, so the observed headroom *delta* has to be converted to a total.
//
// active is the owner's own worker pods that already count against the quota's `used`,
// so adding it back converts "how many more fit" into "how many may exist at once".
// Using the owner's pod count rather than the number of jobs GitHub has assigned biases
// the answer LOW, which is the safe direction: the two diverge across an assignment the
// AGC has not provisioned yet, and under-advertising only delays a job, while
// over-advertising reproduces the claim-and-stall this rung exists to prevent.
//
// Deliberately FAIL-OPEN like WorkerQuotaExhausted: a namespace with no quota, or one
// whose quotas cannot be listed, yields bounded=false so the caller keeps its declared
// ceiling and the provisioner's maxQuotaRetries loop remains the backstop. The signal is
// not authoritative — `.status.used` is eventually consistent and quota scopes are
// ignored — and it does not need to be: it can only ever lower the advertised number,
// and the per-pod ceilingCheck still backstops every pod that is actually created.
func WorkerQuotaCapacity(ctx context.Context, c client.Reader, ns string, spec *corev1.PodSpec, active, max int32) (limit int32, bounded bool) {
	var quotas corev1.ResourceQuotaList
	if err := c.List(ctx, &quotas, client.InNamespace(ns)); err != nil {
		return 0, false
	}
	if len(quotas.Items) == 0 {
		return 0, false
	}
	if active < 0 {
		active = 0
	}
	headroom := QuotaHeadroomPods(ResolveWorkerPodSpec(ctx, c, spec), quotas.Items, max-active)
	return active + headroom, true
}

// WorkerQuotaExhausted reports whether the namespace ResourceQuotas in ns
// currently lack the headroom to admit one more worker pod of the given shape,
// plus a human-readable detail naming the binding resource.
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
func WorkerQuotaExhausted(ctx context.Context, c client.Reader, ns string, spec *corev1.PodSpec) (exhausted bool, detail string) {
	var quotas corev1.ResourceQuotaList
	if err := c.List(ctx, &quotas, client.InNamespace(ns)); err != nil {
		return false, ""
	}
	if len(quotas.Items) == 0 {
		return false, ""
	}
	return QuotaHeadroomViolations(WorkerFootprint(ResolveWorkerPodSpec(ctx, c, spec), 1), quotas.Items,
		"namespace ResourceQuota cannot admit another worker pod: ")
}
