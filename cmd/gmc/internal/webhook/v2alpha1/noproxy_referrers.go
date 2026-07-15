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

package v2alpha1

import (
	"context"
	"fmt"
	"net/url"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
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

// referrerHost is a GitHub host a referrer binds to an EgressProxy, with a
// human-readable description of the binding for the rejection message.
type referrerHost struct {
	host    string
	boundBy string
}

// referrerGitHubHosts lists the GitHub hosts bound to the named EgressProxy by its
// referrers in the same namespace: every ActionsGateway whose defaultProxyRef names
// the proxy contributes its gitHubURL host, and every RunnerSet whose proxyRef
// names it contributes its gateway's gitHubURL host. A RunnerSet whose gateway is
// not yet applied contributes nothing — the gateway's own admission validates the
// pair when it arrives (it checks the proxies bound via its RunnerSets too).
func referrerGitHubHosts(ctx context.Context, reader client.Reader, namespace, proxyName string) ([]referrerHost, error) {
	var out []referrerHost
	var gateways agcv2alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list ActionsGateways in %q: %w", namespace, err)
	}
	gatewayByName := make(map[string]*agcv2alpha1.ActionsGateway, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		gatewayByName[gw.Name] = gw
		if gw.Spec.DefaultProxyRef != nil && gw.Spec.DefaultProxyRef.Name == proxyName {
			if h := gitHubURLHost(gw.Spec.GitHubURL); h != "" {
				out = append(out, referrerHost{
					host:    h,
					boundBy: fmt.Sprintf("ActionsGateway %q (spec.defaultProxyRef)", gw.Name),
				})
			}
		}
	}
	var runnerSets agcv2alpha1.RunnerSetList
	if err := reader.List(ctx, &runnerSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list RunnerSets in %q: %w", namespace, err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if rs.Spec.ProxyRef == nil || rs.Spec.ProxyRef.Name != proxyName {
			continue
		}
		gw, ok := gatewayByName[rs.Spec.GatewayRef.Name]
		if !ok {
			continue
		}
		if h := gitHubURLHost(gw.Spec.GitHubURL); h != "" {
			out = append(out, referrerHost{
				host:    h,
				boundBy: fmt.Sprintf("RunnerSet %q via ActionsGateway %q (spec.proxyRef)", rs.Name, gw.Name),
			})
		}
	}
	return out, nil
}

// validateGitHubHostAgainstProxy rejects a referrer write that binds the given
// GitHub host to an EgressProxy whose noProxyCIDRs would route that host around
// the proxy. A missing proxy admits the write (§H.7 — the proxy's own admission
// checks the pair when it is created); any other read error fails closed.
// refField names the referrer field that binds the pair, for the rejection message.
func validateGitHubHostAgainstProxy(ctx context.Context, reader client.Reader, namespace, proxyName, refField, host string) error {
	if host == "" || proxyName == "" {
		return nil
	}
	var ep agcv2alpha1.EgressProxy
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: proxyName}, &ep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("cannot verify EgressProxy %q noProxyCIDRs against GitHub host %q: %w", proxyName, host, err)
	}
	if err := noproxy.ValidateEntries(
		fmt.Sprintf("EgressProxy %q spec.noProxyCIDRs", proxyName), ep.Spec.NoProxyCIDRs, []string{host}); err != nil {
		return fmt.Errorf("%s: %w", refField, err)
	}
	return nil
}
