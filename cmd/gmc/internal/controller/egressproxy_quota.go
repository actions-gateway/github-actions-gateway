package controller

import (
	"context"
	"fmt"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// evalEgressProxyQuota computes the ProxyQuotaPressure (warning) and
// ProxyQuotaExceeded (error) conditions for a v2 EgressProxy (Q82, ported from the v1
// ActionsGateway). It delegates to the shared evalProxyPoolQuota, supplying the
// EgressProxy's namespace, maxReplicas, and per-replica proxy resources so the quota
// math stays identical across versions. Both conditions are advisory and do NOT gate
// Ready — the pool keeps serving at its current scale.
func (r *EgressProxyReconciler) evalEgressProxyQuota(ctx context.Context, ep *gmcv2alpha1.EgressProxy, proxyDep *appsv1.Deployment) proxyQuotaConditions {
	target := egressProxyMaxReplicas(ep)
	if !egressProxyManagedAutoscaling(ep) {
		// Bring-your-own autoscaler (Q173): spec.maxReplicas is inert and the GMC
		// cannot know the external scaler's ceiling, so predictive headroom is
		// measured to the Deployment's own desired count — Pressure fires only when
		// the namespace cannot fit what the external autoscaler currently asks for.
		// The observed-rejection (Exceeded) tier is scale-source agnostic.
		target = 0
		if proxyDep != nil && proxyDep.Spec.Replicas != nil {
			target = *proxyDep.Spec.Replicas
		}
	}
	return evalProxyPoolQuota(ctx, r, ep.Namespace, target, egressProxyResources(ep), proxyDep)
}

// egressUsesCIDRMode reports whether the EgressProxy's egress allowlist is expressed
// as GitHub CIDRs refreshed by the shared IP-range loop (the default mode) rather
// than a CNI-native FQDN policy. Only CIDR mode can go "stale" — the FQDN modes carry
// no refreshed CIDR rule. It also requires a managed NetworkPolicy: an unmanaged one
// is operator-maintained, not refreshed by this loop.
func egressUsesCIDRMode(ep *gmcv2alpha1.EgressProxy) bool {
	if ep.Spec.ManagedNetworkPolicy != nil && !*ep.Spec.ManagedNetworkPolicy {
		return false
	}
	return egressUsesCIDR(ep.Spec)
}

// evalEgressProxyEgressRulesStale computes the EgressRulesStale condition for a v2
// EgressProxy (Q157, ported from the v1 ActionsGateway): True when the shared GitHub
// IP-range refresh loop's last success is older than the staleness threshold, so the
// proxy NetworkPolicy CIDR allowlist may have drifted from GitHub's published ranges.
//
// It is False (not an alarm) in the "no evidence yet" cases: an unmanaged or non-CIDR
// (FQDN) egress mode — neither of which is CIDR-refreshed by this loop — and before
// the first refresh has completed (startup).
func (r *EgressProxyReconciler) evalEgressProxyEgressRulesStale(ep *gmcv2alpha1.EgressProxy, now time.Time) egressStale {
	if !egressUsesCIDRMode(ep) {
		return egressStale{reason: gmcv2alpha1.ReasonRefreshPending,
			message: "egress NetworkPolicy is not CIDR-refreshed (unmanaged or FQDN egress mode)"}
	}
	if r.IPCache == nil {
		return egressStale{reason: gmcv2alpha1.ReasonRefreshPending,
			message: "GitHub IP-range refresh has not run yet"}
	}
	last, ok := r.IPCache.LastRefresh()
	if !ok {
		return egressStale{reason: gmcv2alpha1.ReasonRefreshPending,
			message: "awaiting first GitHub IP-range refresh"}
	}
	threshold := r.egressStaleThreshold()
	age := now.Sub(last)
	if age > threshold {
		return egressStale{
			stale:  true,
			reason: gmcv2alpha1.ReasonRefreshStalled,
			message: fmt.Sprintf("GitHub egress IP-range allowlist last refreshed %s ago, exceeding the %s staleness window; "+
				"egress to rotated GitHub ranges may be silently dropped", age.Round(time.Minute), threshold),
		}
	}
	return egressStale{reason: gmcv2alpha1.ReasonRefreshCurrent,
		message: fmt.Sprintf("GitHub egress IP-range allowlist refreshed %s ago", age.Round(time.Minute))}
}

// egressStaleThreshold returns the configured EgressRulesStale threshold, defaulting
// to DefaultEgressStaleThreshold when unset (shared with the v1 ActionsGateway).
func (r *EgressProxyReconciler) egressStaleThreshold() time.Duration {
	if r.EgressStaleThreshold > 0 {
		return r.EgressStaleThreshold
	}
	return DefaultEgressStaleThreshold
}

// egressProxyEgressRecheckRequeue returns the cadence at which a Ready EgressProxy
// should be re-reconciled to keep its egress posture fresh, or 0 when there is
// nothing to re-check (an unmanaged NetworkPolicy, or CIDR mode without an IP cache
// — v1 parity). A fraction of the staleness window bounds detection and recovery
// latency (Q157) — the v2 analogue of the v1 egressRecheckRequeue.
//
// In CIDR mode the recheck keeps EgressRulesStale fresh: a stalled IP-range refresh
// loop produces no event for this controller, so without a periodic requeue the
// condition would never flip. In the FQDN modes it re-checks the CNI-native FQDN
// policy for out-of-band drift: the unstructured Cilium/Calico/GKE policy has no
// Owns() watch, so once Ready a deleted or edited policy would otherwise never be
// restored (Q326).
func (r *EgressProxyReconciler) egressProxyEgressRecheckRequeue(ep *gmcv2alpha1.EgressProxy) time.Duration {
	if ep.Spec.ManagedNetworkPolicy != nil && !*ep.Spec.ManagedNetworkPolicy {
		return 0 // operator-maintained egress policy: nothing of ours to re-check
	}
	if egressUsesCIDR(ep.Spec) && r.IPCache == nil {
		return 0 // CIDR staleness is not evaluated without the shared cache
	}
	d := r.egressStaleThreshold() / 8
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

// quotaToEgressProxies maps a ResourceQuota event to every EgressProxy in the same
// namespace, so a platform admin changing the namespace quota's .spec.hard refreshes
// the ProxyQuota{Pressure,Exceeded} conditions promptly (Q82/Q326) instead of lagging
// until an unrelated child event. Mirrors the v1 quotaToActionsGateways.
func (r *EgressProxyReconciler) quotaToEgressProxies(ctx context.Context, obj client.Object) []ctrl.Request {
	var list gmcv2alpha1.EgressProxyList
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
