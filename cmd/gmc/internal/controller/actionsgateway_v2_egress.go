package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
)

// evalDirectEgressRulesStale computes the EgressRulesStale condition for a v2
// ActionsGateway (Q1249). In direct egress its AGC and workload NetworkPolicies carry
// the GitHub-CIDR allowlist from the shared IP-range cache, so the cache's state is
// reported with the evaluation the CIDR-mode EgressProxy uses. A proxied gateway's
// policies carry no GitHub rule, and its pool reports its own allowlist.
//
// While the cache serves no ranges, preserveExistingEgress keeps an already-programmed
// policy's rules (Q246), so whether GitHub egress is denied depends on what the
// policies hold rather than on the cache alone; the message reads them to say which.
func (r *ActionsGatewayV2Reconciler) evalDirectEgressRulesStale(ctx context.Context, ag *gmcv2.ActionsGateway, proxy *gmcv2.EgressProxy, now time.Time) egressStale {
	if proxy != nil {
		return egressStale{reason: gmcv2.ReasonRefreshPending,
			message: fmt.Sprintf("GitHub egress goes through EgressProxy %q, which reports its own allowlist's staleness", proxy.Name)}
	}
	if r.IPCache == nil {
		return egressStale{reason: gmcv2.ReasonRefreshPending,
			message: "GitHub IP-range refresh has not run yet"}
	}
	last, ok := r.IPCache.LastRefresh()
	if !ok {
		return egressStale{stale: true, reason: gmcv2.ReasonRefreshNeverSucceeded,
			message: "no GitHub IP-range refresh has succeeded, so the GMC holds no GitHub ranges; " + r.servedGitHubRules(ctx, ag)}
	}
	threshold := r.egressStaleThreshold()
	age := now.Sub(last)
	if age > threshold {
		msg := refreshStalledMessage(r.IPCache, age, threshold)
		if len(r.IPCache.Snapshot()) == 0 {
			// refreshStalledMessage would call GitHub egress denied; preserved rules may serve it.
			msg = fmt.Sprintf("GitHub egress IP-range allowlist last refreshed %s ago, exceeding the %s staleness window, and the GMC serves no ranges; %s",
				age.Round(time.Minute), threshold, r.servedGitHubRules(ctx, ag))
		}
		return egressStale{stale: true, reason: gmcv2.ReasonRefreshStalled, message: msg}
	}
	return egressStale{reason: gmcv2.ReasonRefreshCurrent,
		message: fmt.Sprintf("GitHub egress IP-range allowlist refreshed %s ago", age.Round(time.Minute))}
}

// servedGitHubRules describes what a direct-egress gateway's NetworkPolicies serve
// while the cache serves no ranges: the GitHub rule they were last programmed with, or
// none, in which case GitHub egress through that policy is denied.
func (r *ActionsGatewayV2Reconciler) servedGitHubRules(ctx context.Context, ag *gmcv2.ActionsGateway) string {
	vault, hasVault := vaultEgressRule(ag)
	var denied []string
	for _, p := range []struct{ name, who string }{
		{agcNameV2(ag), "the AGC"},
		{workloadNPNameV2(ag), "workers"},
	} {
		var np networkingv1.NetworkPolicy
		if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: p.name}, &np); err == nil &&
			hasGitHubCIDREgressRule(np.Spec.Egress, vault, hasVault) {
			continue
		}
		denied = append(denied, fmt.Sprintf("NetworkPolicy %q carries no GitHub rule, so GitHub egress from %s is denied", p.name, p.who))
	}
	if len(denied) == 0 {
		return "the NetworkPolicies keep the GitHub ranges they were last programmed with, which may have drifted from GitHub's published ranges"
	}
	return strings.Join(denied, "; ")
}

// hasGitHubCIDREgressRule reports whether rules hold the shape githubCIDREgressRule
// builds: TCP/443 alone, to IPBlock peers only. The Vault rule is the one other rule
// on these policies that can take that shape, so it is excluded by value.
func hasGitHubCIDREgressRule(rules []networkingv1.NetworkPolicyEgressRule, vault networkingv1.NetworkPolicyEgressRule, hasVault bool) bool {
	for _, rule := range rules {
		if hasVault && equality.Semantic.DeepEqual(rule, vault) {
			continue
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntValue() != 443 ||
			rule.Ports[0].EndPort != nil || len(rule.To) == 0 {
			continue
		}
		ipOnly := true
		for _, peer := range rule.To {
			if peer.IPBlock == nil || peer.PodSelector != nil || peer.NamespaceSelector != nil {
				ipOnly = false
				break
			}
		}
		if ipOnly {
			return true
		}
	}
	return false
}

// egressStaleThreshold returns the configured EgressRulesStale threshold, defaulting
// to DefaultEgressStaleThreshold when unset.
func (r *ActionsGatewayV2Reconciler) egressStaleThreshold() time.Duration {
	if r.EgressStaleThreshold > 0 {
		return r.EgressStaleThreshold
	}
	return DefaultEgressStaleThreshold
}

// directEgressRecheckRequeue returns the cadence at which a direct-egress gateway is
// re-reconciled to keep EgressRulesStale fresh, or 0 when it is not evaluated (proxied,
// or no IP cache). The refresh loop patches these policies without waking this
// reconciler, so a stall would otherwise never flip the condition.
func (r *ActionsGatewayV2Reconciler) directEgressRecheckRequeue(proxy *gmcv2.EgressProxy) time.Duration {
	if proxy != nil || r.IPCache == nil {
		return 0
	}
	// Until the first fetch lands, poll briefly so RefreshNeverSucceeded clears with it.
	if _, ok := r.IPCache.LastRefresh(); !ok {
		return time.Minute
	}
	return max(r.egressStaleThreshold()/8, time.Minute)
}
