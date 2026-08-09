package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The GitHub hosts an EgressProxy's referrers bind to it (Q506 #2). The proxy's own
// spec names no GitHub URL — that lives on the referring ActionsGateway's githubURL —
// so the FQDN allowlist and the CONNECT suffix list were assembled from six public
// hostnames alone, and a GitHub Enterprise Server tenant in an FQDN egress mode got an
// allowlist naming six hosts none of its traffic uses.
//
// This is the reconcile-side twin of the v2 webhook's referrerGitHubHosts, which
// resolves the same graph in the opposite direction to guard noProxyCIDRs (Q322). The
// binding shapes are identical: an ActionsGateway whose defaultProxyRef names the
// proxy, and a RunnerSet whose proxyRef names it (contributing its gateway's host).

// resolveReferrerGitHubHosts returns the sorted, deduplicated GitHub hosts that
// referrers in the proxy's namespace bind to it, excluding hosts the built-in public
// GitHub allowlist already covers. An empty result — every referrer on public GitHub,
// or none yet applied — leaves the emitted allowlist byte-for-byte as before.
//
// A RunnerSet whose gateway is missing contributes nothing: with no githubURL there is
// no host to allow, and the gateway's own arrival requeues this proxy.
// A cross-namespace referrer counts too (M4, §H.9): its GitHub host is just as
// unreachable as a colocated one if the allowlist omits it, and a shared proxy exists
// precisely to carry other namespaces' traffic.
//
// The scan widens to cluster-wide only for a proxy that actually grants something.
// A proxy sharing nothing can have no cross-namespace referrer, so scanning past its
// own namespace could only cost — and it costs on every reconcile, for every proxy in
// the cluster, against a resync that re-enqueues them all. Keeping the unshared case
// namespace-scoped leaves it exactly as expensive as it was before M4.
func resolveReferrerGitHubHosts(ctx context.Context, c client.Client, ep *gmcv2alpha1.EgressProxy) ([]string, error) {
	namespace, proxyName := ep.Namespace, ep.Name

	scope := []client.ListOption{client.InNamespace(namespace)}
	if ep.Spec.Sharing != nil && len(ep.Spec.Sharing.AllowedNamespaces) > 0 {
		scope = nil
	}

	var gateways gmcv2alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways, scope...); err != nil {
		return nil, fmt.Errorf("list ActionsGateways: %w", err)
	}
	// Keyed namespace/name: a RunnerSet's gatewayRef is always same-namespace, so two
	// namespaces may hold same-named gateways with different GitHub URLs.
	type gwKey struct{ ns, name string }
	gatewayByKey := make(map[gwKey]*gmcv2alpha1.ActionsGateway, len(gateways.Items))

	// binds reports whether a referrer in referrerNS naming (refNS, refName) resolves
	// to this proxy AND is entitled to it. Consent is required for the cross-namespace
	// case so an unconsented referrer cannot inject a hostname into another tenant's
	// egress allowlist by naming their proxy.
	binds := func(ref *gmcv2alpha1.ProxyObjectRef, referrerNS string) bool {
		if ref == nil || ref.Name != proxyName {
			return false
		}
		refNS := ref.Namespace
		if refNS == "" {
			refNS = referrerNS
		}
		if refNS != namespace {
			return false
		}
		return referrerNS == namespace || proxyShareGranted(ep, referrerNS)
	}

	var hosts []string
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		gatewayByKey[gwKey{gw.Namespace, gw.Name}] = gw
		if binds(gw.Spec.DefaultProxyRef, gw.Namespace) {
			hosts = append(hosts, gitHubHostOf(gw.Spec.GitHubURL))
		}
	}

	var runnerSets gmcv2alpha1.RunnerSetList
	if err := c.List(ctx, &runnerSets, scope...); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if !binds(rs.Spec.ProxyRef, rs.Namespace) {
			continue
		}
		if gw, ok := gatewayByKey[gwKey{rs.Namespace, rs.Spec.GatewayRef.Name}]; ok {
			hosts = append(hosts, gitHubHostOf(gw.Spec.GitHubURL))
		}
	}

	slices.Sort(hosts)
	hosts = slices.Compact(slices.DeleteFunc(hosts, func(h string) bool {
		return h == "" || slices.Contains(githubEgressFQDNs, h)
	}))
	if len(hosts) == 0 {
		return nil, nil
	}
	return hosts, nil
}

// gitHubEgressGap is the computed GitHubEgressIncomplete condition (Q506 #3).
type gitHubEgressGap struct {
	incomplete bool
	reason     string
	message    string
}

// evalGitHubEgressIncomplete reports whether a CIDR-mode pool's egress allowlist
// provably cannot reach the GitHub its referrers bind to it. CIDR mode programs the
// ranges api.github.com/meta publishes; a GitHub Enterprise Server appliance sits in
// the customer's own address space and appears in none of them, so the NetworkPolicy
// denies the proxy's traffic to the one host it exists to reach — as a connect
// timeout, with nothing naming the cause.
//
// Unlike the FQDN surfaces, this one has no code answer: the appliance's ranges are
// knowable only to the operator. So the condition names the obligation rather than
// closing it. Supplying spec.destinationCIDRs clears it; whether those ranges cover
// the appliance is not checkable here.
func evalGitHubEgressIncomplete(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) gitHubEgressGap {
	allowed := func(msg string) gitHubEgressGap {
		return gitHubEgressGap{reason: apiconditions.ReasonGitHubEgressAllowed, message: msg}
	}
	if ep.Spec.ManagedNetworkPolicy != nil && !*ep.Spec.ManagedNetworkPolicy {
		return allowed("egress policy is operator-maintained")
	}
	if !egressUsesCIDR(ep.Spec) {
		return allowed("FQDN mode carries every referrer's GitHub host")
	}
	if len(gitHubHosts) == 0 {
		return allowed("every referrer targets public GitHub, which the CIDR allowlist covers")
	}
	if len(ep.Spec.DestinationCIDRs) > 0 {
		return allowed(fmt.Sprintf("spec.destinationCIDRs supplies ranges for %s", strings.Join(gitHubHosts, ", ")))
	}
	return gitHubEgressGap{
		incomplete: true,
		reason:     apiconditions.ReasonApplianceRangesRequired,
		message: fmt.Sprintf(
			"CIDR mode allows only the ranges GitHub publishes, which never contain %s; "+
				"set spec.destinationCIDRs to the appliance's ranges or switch spec.egressPolicyMode to an FQDN mode",
			strings.Join(gitHubHosts, ", ")),
	}
}

// referrerToEgressProxies maps an ActionsGateway or RunnerSet event to every
// EgressProxy in the same namespace. It does not narrow to the proxies the referrer
// names: a proxyRef *change* must also requeue the proxy the referrer just stopped
// naming, so its allowlist drops the host. A namespace holds a handful of proxies.
func (r *EgressProxyReconciler) referrerToEgressProxies(ctx context.Context, obj client.Object) []ctrl.Request {
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
	// A referrer naming a proxy in another namespace (M4, §H.9) has to requeue that
	// proxy too: it owns the CA projection into this namespace and the ingress rule
	// admitting it, and neither converges off a same-namespace list. Named explicitly
	// rather than swept, since a cross-namespace ref points at exactly one proxy.
	for _, ref := range remoteProxyRefsOf(obj) {
		reqs = append(reqs, ctrl.Request{NamespacedName: ref})
	}
	return reqs
}

// remoteProxyRefsOf returns the cross-namespace EgressProxy a referrer names, if any.
// Only the two referrer kinds this reconciler watches carry a proxy reference; any
// other object yields nothing.
func remoteProxyRefsOf(obj client.Object) []types.NamespacedName {
	var ref *gmcv2alpha1.ProxyObjectRef
	switch o := obj.(type) {
	case *gmcv2alpha1.ActionsGateway:
		ref = o.Spec.DefaultProxyRef
	case *gmcv2alpha1.RunnerSet:
		ref = o.Spec.ProxyRef
	default:
		return nil
	}
	if ref == nil || ref.Namespace == "" || ref.Namespace == obj.GetNamespace() {
		return nil
	}
	return []types.NamespacedName{{Namespace: ref.Namespace, Name: ref.Name}}
}

// gitHubHostOf returns the hostname of a gateway githubURL, or "" when it does not
// parse or names no host. Callers drop the empty result: an allowlist entry can only
// be added for a host that can actually be extracted.
func gitHubHostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
