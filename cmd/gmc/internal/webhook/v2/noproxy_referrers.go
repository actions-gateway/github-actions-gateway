package v2

import (
	"context"
	"fmt"
	"net/url"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/noproxy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file holds the referrer-graph half of the noProxyCIDRs GitHub-bypass guard
// (Q322). Unlike v1 — where the gitHubURL and the noProxyCIDRs live on the same
// ActionsGateway CR and one webhook sees both — the v2 decomposition splits them:
// the EgressProxy carries the noProxyCIDRs, while the GitHub host (including a
// GitHub Enterprise Server host) lives on the referring ActionsGateway's gitHubURL.
// A bypass can therefore be assembled from EITHER side, so both admission paths
// consult the other object through the uncached API reader:
//
//   - EgressProxy create/update collects the GitHub hosts of every referrer —
//     gateways whose defaultProxyRef names the proxy, and RunnerSets whose proxyRef
//     names it (their host is their gateway's gitHubURL) — and rejects any
//     noProxyCIDRs entry matching one (referrerGitHubHosts).
//   - ActionsGateway / RunnerSet create/update resolves the referenced proxy and
//     rejects the write when the bound gitHubURL host falls in its noProxyCIDRs
//     (validateGitHubHostAgainstProxy).
//
// Referential integrity itself stays a runtime condition (§H.7): a ref naming a
// missing object is admitted, because with no referent there is no noProxyCIDRs/
// gitHubURL pair to conflict — whichever object arrives later closes the pair and
// is checked then. Read errors other than NotFound fail closed, matching the other
// reader-backed guards (singleton, ScaleSet label uniqueness).
//
// A proxy reference may name another namespace (§H.9), and it binds only with the
// provider's consent, so both directions resolve ProxyObjectRef.Namespace and count
// a cross-namespace pair only where the proxy grants the referrer's namespace — the
// binding the reconciler's resolveReferrerGitHubHosts applies. An ungranted
// reference is admitted unread from the referrer side: it wires nothing, and judging
// it would tell a tenant what another namespace's proxy excludes. The grant itself
// is an EgressProxy write, which is where that pair is checked.
//
// On update each side judges only the pairs its own write can newly bind, so a pair
// stored before a rule tightened does not refuse an edit to an unrelated field.

// gitHubURLHost returns the hostname of a gateway gitHubURL, or "" when the URL
// does not parse or names no host (URL well-formedness is validated elsewhere;
// this guard only protects a host it can actually extract).
func gitHubURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// referrerHost is a GitHub host a referrer binds to an EgressProxy, with the
// referrer's namespace and a human-readable description of the binding for the
// rejection message.
type referrerHost struct {
	host      string
	namespace string
	boundBy   string
}

// proxyRefKey returns the EgressProxy a referrer in referrerNS names, resolving an
// empty ref namespace to the referrer's own. ok is false for a nil ref.
func proxyRefKey(ref *agcv2.ProxyObjectRef, referrerNS string) (key client.ObjectKey, ok bool) {
	if ref == nil {
		return client.ObjectKey{}, false
	}
	ns := ref.Namespace
	if ns == "" {
		ns = referrerNS
	}
	return client.ObjectKey{Namespace: ns, Name: ref.Name}, true
}

// referrerGitHubHosts lists the GitHub hosts its referrers bind to ep: every
// ActionsGateway whose defaultProxyRef resolves to the proxy contributes its
// gitHubURL host, and every RunnerSet whose proxyRef resolves to it contributes its
// gateway's gitHubURL host. A referrer in another namespace counts only when ep
// grants that namespace. A RunnerSet whose gateway is not yet applied contributes
// nothing — the gateway's own admission validates the pair when it arrives (it
// checks the proxies bound via its RunnerSets too).
//
// The Lists are cluster-wide only for a proxy that grants a namespace; one sharing
// nothing can have no cross-namespace referrer and lists its own namespace.
func referrerGitHubHosts(ctx context.Context, reader client.Reader, ep *agcv2.EgressProxy) ([]referrerHost, error) {
	self := client.ObjectKeyFromObject(ep)
	var scope []client.ListOption
	if ep.Spec.Sharing == nil || len(ep.Spec.Sharing.AllowedNamespaces) == 0 {
		scope = []client.ListOption{client.InNamespace(ep.Namespace)}
	}
	binds := func(ref *agcv2.ProxyObjectRef, referrerNS string) bool {
		key, ok := proxyRefKey(ref, referrerNS)
		return ok && key == self && (referrerNS == ep.Namespace || controller.ProxyShareGranted(ep, referrerNS))
	}
	// describe qualifies a referrer's name with its namespace when that is not the
	// proxy's own.
	describe := func(namespace, name string) string {
		if namespace == ep.Namespace {
			return fmt.Sprintf("%q", name)
		}
		return fmt.Sprintf("%q", namespace+"/"+name)
	}

	var out []referrerHost
	var gateways agcv2.ActionsGatewayList
	if err := reader.List(ctx, &gateways, scope...); err != nil {
		return nil, fmt.Errorf("list ActionsGateways: %w", err)
	}
	// A RunnerSet's gatewayRef is same-namespace, and two namespaces may hold
	// same-named gateways.
	gatewayByKey := make(map[client.ObjectKey]*agcv2.ActionsGateway, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		gatewayByKey[client.ObjectKeyFromObject(gw)] = gw
		if !binds(gw.Spec.DefaultProxyRef, gw.Namespace) {
			continue
		}
		if h := gitHubURLHost(gw.Spec.GitHubURL); h != "" {
			out = append(out, referrerHost{
				host:      h,
				namespace: gw.Namespace,
				boundBy:   fmt.Sprintf("ActionsGateway %s (spec.defaultProxyRef)", describe(gw.Namespace, gw.Name)),
			})
		}
	}
	var runnerSets agcv2.RunnerSetList
	if err := reader.List(ctx, &runnerSets, scope...); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if !binds(rs.Spec.ProxyRef, rs.Namespace) {
			continue
		}
		gw, ok := gatewayByKey[client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}]
		if !ok {
			continue
		}
		if h := gitHubURLHost(gw.Spec.GitHubURL); h != "" {
			out = append(out, referrerHost{
				host:      h,
				namespace: rs.Namespace,
				boundBy: fmt.Sprintf("RunnerSet %s via ActionsGateway %q (spec.proxyRef)",
					describe(rs.Namespace, rs.Name), gw.Name),
			})
		}
	}
	return out, nil
}

// validateGitHubHostAgainstProxy rejects a write by a referrer in referrerNS that
// binds the given GitHub host to the EgressProxy ref names, when that proxy's
// noProxyCIDRs would route the host around it. A missing proxy admits the write
// (§H.7 — the proxy's own admission checks the pair when it is created), and so
// does a proxy in another namespace that does not grant referrerNS; any other read
// error fails closed. refField names the referrer field that binds the pair, for
// the rejection message.
func validateGitHubHostAgainstProxy(ctx context.Context, reader client.Reader, referrerNS string, ref *agcv2.ProxyObjectRef, refField, host string) error {
	key, ok := proxyRefKey(ref, referrerNS)
	if !ok || host == "" {
		return nil
	}
	var ep agcv2.EgressProxy
	if err := reader.Get(ctx, key, &ep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("cannot verify EgressProxy %q noProxyCIDRs against GitHub host %q: %w", key.Name, host, err)
	}
	if key.Namespace != referrerNS && !controller.ProxyShareGranted(&ep, referrerNS) {
		return nil
	}
	if err := noproxy.ValidateEntries(
		fmt.Sprintf("EgressProxy %q spec.noProxyCIDRs", key.Name), ep.Spec.NoProxyCIDRs, []string{host}); err != nil {
		return fmt.Errorf("%s: %w", refField, err)
	}
	return nil
}
