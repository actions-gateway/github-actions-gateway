/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
)

// evalEgressProxyQuota computes the ProxyQuotaPressure (warning) and
// ProxyQuotaExceeded (error) conditions for a v2 EgressProxy (Q82, ported from the v1
// ActionsGateway). It delegates to the shared evalProxyPoolQuota, supplying the
// EgressProxy's namespace, maxReplicas, and per-replica proxy resources so the quota
// math stays identical across versions. Both conditions are advisory and do NOT gate
// Ready — the pool keeps serving at its current scale.
func (r *EgressProxyReconciler) evalEgressProxyQuota(ctx context.Context, ep *gmcv2alpha1.EgressProxy, proxyDep *appsv1.Deployment) proxyQuotaConditions {
	return evalProxyPoolQuota(ctx, r, ep.Namespace, egressProxyMaxReplicas(ep), egressProxyResources(ep), proxyDep)
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

// egressProxyEgressRecheckRequeue returns the cadence at which the EgressProxy should
// be re-reconciled purely to keep EgressRulesStale fresh, or 0 when staleness is not
// evaluated (non-CIDR egress mode, or no IP cache). A stalled refresh loop produces no
// event for this controller, so without a periodic requeue the condition would never
// flip. A fraction of the staleness window bounds detection and recovery latency
// (Q157) — the v2 analogue of the v1 egressRecheckRequeue.
func (r *EgressProxyReconciler) egressProxyEgressRecheckRequeue(ep *gmcv2alpha1.EgressProxy) time.Duration {
	if !egressUsesCIDRMode(ep) || r.IPCache == nil {
		return 0
	}
	d := r.egressStaleThreshold() / 8
	if d < time.Minute {
		d = time.Minute
	}
	return d
}
