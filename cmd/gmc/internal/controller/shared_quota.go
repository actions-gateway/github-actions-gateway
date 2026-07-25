package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// The namespace-ResourceQuota math for an HPA-scaled proxy pool, shared by the v1
// ActionsGateway's proxy (evalProxyQuota) and the v2 EgressProxy
// (evalEgressProxyQuota). Both surface the same two advisory conditions from the
// same arithmetic; only the pool's namespace, ceiling and per-replica resources
// come from the version-specific CR.

// quotaCheck maps one proxy-footprint resource to the ResourceQuota hard key
// that constrains it. A footprint resource may be capped by more than one quota
// key (e.g. CPU requests are limited by both "requests.cpu" and the legacy
// "cpu" alias), so the slice can hold multiple entries per footprint key.
type quotaCheck struct {
	footprint corev1.ResourceName // key into the footprint ResourceList
	hardKey   corev1.ResourceName // ResourceQuota .Hard key it is measured against
}

var proxyQuotaChecks = []quotaCheck{
	{corev1.ResourcePods, corev1.ResourcePods},
	{corev1.ResourceRequestsCPU, corev1.ResourceRequestsCPU},
	{corev1.ResourceRequestsCPU, corev1.ResourceCPU}, // legacy alias for requests.cpu
	{corev1.ResourceRequestsMemory, corev1.ResourceRequestsMemory},
	{corev1.ResourceRequestsMemory, corev1.ResourceMemory}, // legacy alias for requests.memory
	{corev1.ResourceLimitsCPU, corev1.ResourceLimitsCPU},
	{corev1.ResourceLimitsMemory, corev1.ResourceLimitsMemory},
}

// footprintFromResources returns the ResourceQuota footprint of `replicas` pods with
// per-pod requests/limits `res`: the per-replica requests/limits scaled by replicas,
// plus the pod count. Keys mirror ResourceQuota hard keys (pods, requests.cpu,
// requests.memory, limits.cpu, limits.memory). It is linear in replicas, so the
// *additional* demand to grow a pool from m to n pods is
// footprintFromResources(res, n-m). Shared by the v1 ActionsGateway proxy
// (proxyFootprint) and the v2 EgressProxy quota eval (egressProxyFootprint).
func footprintFromResources(res corev1.ResourceRequirements, replicas int32) corev1.ResourceList {
	if replicas < 0 {
		replicas = 0
	}
	out := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(int64(replicas), resource.DecimalSI),
	}
	if q, ok := res.Requests[corev1.ResourceCPU]; ok {
		out[corev1.ResourceRequestsCPU] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Requests[corev1.ResourceMemory]; ok {
		out[corev1.ResourceRequestsMemory] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Limits[corev1.ResourceCPU]; ok {
		out[corev1.ResourceLimitsCPU] = mulQuantity(q, int64(replicas))
	}
	if q, ok := res.Limits[corev1.ResourceMemory]; ok {
		out[corev1.ResourceLimitsMemory] = mulQuantity(q, int64(replicas))
	}
	return out
}

// mulQuantity returns q multiplied by n. n is bounded by the CRD's
// proxy.maxReplicas maximum (100), so repeated addition stays cheap and exact
// (resource.Quantity has no scalar-multiply primitive that preserves the
// canonical form across both DecimalSI and BinarySI).
func mulQuantity(q resource.Quantity, n int64) resource.Quantity {
	out := resource.Quantity{Format: q.Format}
	for i := int64(0); i < n; i++ {
		out.Add(q)
	}
	return out
}

// proxyQuotaConditions carries the computed status of the two namespace-quota
// conditions for the proxy pool. They follow the project's two-tier convention
// (see docs/development/kubernetes-conventions.md): a warning tier
// (ProxyQuotaPressure) and an error tier (ProxyQuotaExceeded), mutually
// exclusive — the error supersedes the warning.
type proxyQuotaConditions struct {
	pressure        bool
	pressureReason  string
	pressureMessage string
	exceeded        bool
	exceededReason  string
	exceededMessage string
}

// evalProxyPoolQuota computes the ProxyQuotaPressure (warning) and
// ProxyQuotaExceeded (error) conditions for a namespace-ResourceQuota-bounded,
// HPA-scaled proxy pool against the platform-owned namespace ResourceQuota (Q82).
// Both are advisory and do NOT gate Ready — the pool keeps serving at its current
// scale. Shared by the v1 ActionsGateway proxy (evalProxyQuota) and the v2
// EgressProxy (evalEgressProxyQuota); the caller supplies the pool's namespace,
// maxReplicas, per-replica resources, and current proxy Deployment.
//
//   - ProxyQuotaExceeded (error): the proxy Deployment is *currently* having replica
//     creates rejected by quota — read from its ReplicaFailure condition, the
//     authoritative runtime signal.
//   - ProxyQuotaPressure (warning): predictive. Given the current remaining quota
//     headroom (hard − used), the pool cannot grow from its current replica count up
//     to maxReplicas.
//
// The headroom check ignores quota scopes; face-value hard/used is sufficient for an
// advisory signal and avoids false precision.
func evalProxyPoolQuota(ctx context.Context, reader client.Reader, namespace string, maxReplicas int32, res corev1.ResourceRequirements, proxyDep *appsv1.Deployment) proxyQuotaConditions {
	st := proxyQuotaConditions{
		pressureReason:  "QuotaHeadroomSufficient",
		pressureMessage: "namespace ResourceQuota admits scaling the proxy pool to maxReplicas",
		exceededReason:  "NoRejection",
		exceededMessage: "proxy replica creation is not being rejected by the namespace ResourceQuota",
	}

	// Error tier — observed rejection. The Deployment surfaces ReplicaFailure when
	// its ReplicaSet cannot create pods; a quota rejection carries an "exceeded
	// quota" message, which distinguishes it from other create failures (PSA,
	// image, scheduling).
	if proxyDep != nil {
		if c := findDeploymentCondition(proxyDep, appsv1.DeploymentReplicaFailure); c != nil &&
			c.Status == corev1.ConditionTrue && strings.Contains(c.Message, "exceeded quota") {
			st.exceeded = true
			st.exceededReason = "ReplicasRejected"
			st.exceededMessage = "proxy replica creation is being rejected by the namespace ResourceQuota: " + c.Message
		}
	}

	// Warning tier — predictive headroom check.
	var quotas corev1.ResourceQuotaList
	if err := reader.List(ctx, &quotas, client.InNamespace(namespace)); err != nil {
		st.pressureReason = "QuotaUnknown"
		st.pressureMessage = fmt.Sprintf("could not read namespace ResourceQuota: %v", err)
	} else if len(quotas.Items) == 0 {
		st.pressureReason = "NoQuota"
		st.pressureMessage = "no namespace ResourceQuota constrains the proxy pool"
	} else {
		current := int32(0)
		if proxyDep != nil {
			current = proxyDep.Status.Replicas
		}
		additional := maxReplicas - current
		if additional > 0 {
			demand := footprintFromResources(res, additional)
			st.pressure, st.pressureMessage = quotaHeadroomViolations(
				demand, quotas.Items, proxyQuotaChecks,
				"proxy cannot scale to maxReplicas with current quota headroom: ")
			if st.pressure {
				st.pressureReason = "InsufficientQuotaHeadroom"
			}
		}
	}

	// Error supersedes warning: while replicas are actively rejected, the
	// headroom warning is redundant noise. Keep the two conditions mutually
	// exclusive so each maps cleanly to one alert severity.
	if st.exceeded {
		st.pressure = false
		st.pressureReason = "Superseded"
		st.pressureMessage = "superseded by ProxyQuotaExceeded"
	}
	return st
}

// quotaHeadroomViolations reports whether `demand` exceeds the remaining headroom
// (hard − used) of any quota for any mapped resource, and a human-readable
// message. The bool is true when at least one resource is over headroom.
func quotaHeadroomViolations(demand corev1.ResourceList, quotas []corev1.ResourceQuota, checks []quotaCheck, msgPrefix string) (bool, string) {
	var violations []string
	for i := range quotas {
		q := &quotas[i]
		hard := q.Status.Hard
		if len(hard) == 0 {
			// Status not yet populated by the quota controller (e.g. just after
			// the admin edits .spec.hard); fall back to the requested cap.
			hard = q.Spec.Hard
		}
		for _, c := range checks {
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

// findDeploymentCondition returns the named Deployment status condition, or nil.
func findDeploymentCondition(dep *appsv1.Deployment, t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == t {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// resourceListEqual reports whether two ResourceLists hold the same keys with
// numerically equal quantities. It is used by the ResourceQuota watch predicate
// to ignore status-only churn (.status.used changes as pods come and go) and
// reconcile only when an admin changes a quota's .spec.hard. reflect.DeepEqual
// is unsuitable: resource.Quantity caches a formatted string in an unexported
// field, so equal values can compare unequal.
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

// quotaHardChangedPredicate enqueues ResourceQuota create/delete and only those
// updates that change .spec.hard, ignoring the high-frequency .status.used churn
// as pods come and go — only the hard cap feeds the ProxyQuota conditions (Q82).
// Shared by the v1 ActionsGateway and v2 EgressProxy quota watches (Q326).
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
