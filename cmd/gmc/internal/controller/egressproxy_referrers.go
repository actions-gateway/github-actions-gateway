package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"

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
func resolveReferrerGitHubHosts(ctx context.Context, c client.Client, namespace, proxyName string) ([]string, error) {
	var gateways gmcv2alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list ActionsGateways in %q: %w", namespace, err)
	}
	gatewayByName := make(map[string]*gmcv2alpha1.ActionsGateway, len(gateways.Items))
	var hosts []string
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		gatewayByName[gw.Name] = gw
		if gw.Spec.DefaultProxyRef != nil && gw.Spec.DefaultProxyRef.Name == proxyName {
			hosts = append(hosts, gitHubHostOf(gw.Spec.GitHubURL))
		}
	}

	var runnerSets gmcv2alpha1.RunnerSetList
	if err := c.List(ctx, &runnerSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list RunnerSets in %q: %w", namespace, err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if rs.Spec.ProxyRef == nil || rs.Spec.ProxyRef.Name != proxyName {
			continue
		}
		if gw, ok := gatewayByName[rs.Spec.GatewayRef.Name]; ok {
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
	return reqs
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
