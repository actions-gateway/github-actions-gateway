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
	"net"
	"strings"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEgressModeHelpers(t *testing.T) {
	// Empty string defaults to CIDR (a hand-built object that skipped defaulting).
	assert.Equal(t, gmcv2alpha1.EgressPolicyModeCIDR, egressModeOf(gmcv2alpha1.EgressProxySpec{}))
	assert.True(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{}))
	assert.True(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCIDR}))
	assert.False(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN}))
	assert.False(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCalicoFQDN}))
}

// TestBuildEgressProxyNetworkPolicy_FQDNModeDropsCIDR asserts the secure-by-default,
// fail-closed posture: in an FQDN mode the standard NetworkPolicy keeps DNS + ingress
// but omits the GitHub CIDR egress rule, so GitHub egress is denied unless the
// CNI-native policy re-allows it.
func TestBuildEgressProxyNetworkPolicy_FQDNModeDropsCIDR(t *testing.T) {
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cidrs := []net.IPNet{*cidr}

	for _, mode := range []gmcv2alpha1.EgressPolicyMode{gmcv2alpha1.EgressPolicyModeCiliumFQDN, gmcv2alpha1.EgressPolicyModeCalicoFQDN} {
		ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
			ep.Spec.EgressPolicyMode = mode
		})
		np := buildEgressProxyNetworkPolicy(ep, cidrs)
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
					t.Fatalf("%s mode must not add the GitHub CIDR egress rule", mode)
				}
			}
		}
		assert.NotEmpty(t, np.Spec.Egress, "DNS egress is always present")
		require.Len(t, np.Spec.Ingress, 1, "%s mode keeps the workload ingress rule", mode)
	}
}

func TestBuildEgressProxyCiliumNetworkPolicy(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	u := buildEgressProxyCiliumNetworkPolicy(ep)

	assert.Equal(t, "cilium.io/v2", u.GetAPIVersion())
	assert.Equal(t, "CiliumNetworkPolicy", u.GetKind())
	assert.Equal(t, "shared-proxy-fqdn", u.GetName())
	assert.Equal(t, "team-a", u.GetNamespace())
	assert.Equal(t, "shared", u.GetLabels()[egressProxyComponentLabel], "carries the managed identity label")

	// endpointSelector scopes to this pool's proxy pods.
	sel, found, err := unstructured.NestedStringMap(u.Object, "spec", "endpointSelector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, proxyAppName, sel["app"])
	assert.Equal(t, "shared", sel[egressProxyComponentLabel])

	egress, found, err := unstructured.NestedSlice(u.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 2, "DNS rule + GitHub FQDN rule")

	// The GitHub rule carries a toFQDNs entry for every configured hostname, on 443.
	githubRule, ok := egress[1].(map[string]interface{})
	require.True(t, ok)
	fqdns, found, err := unstructured.NestedSlice(githubRule, "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	assert.Len(t, fqdns, len(githubEgressFQDNs))
	assertCiliumFQDNPresent(t, fqdns, "api.github.com", "matchName")
	assertCiliumFQDNPresent(t, fqdns, "*.actions.githubusercontent.com", "matchPattern")

	// DNS rule has a dns-visibility block so Cilium's FQDN proxy can learn the IPs.
	dnsRule, ok := egress[0].(map[string]interface{})
	require.True(t, ok)
	toPorts, _, _ := unstructured.NestedSlice(dnsRule, "toPorts")
	require.Len(t, toPorts, 1)
	tp0 := toPorts[0].(map[string]interface{})
	_, hasDNS, _ := unstructured.NestedSlice(tp0, "rules", "dns")
	assert.True(t, hasDNS, "DNS visibility rule required for Cilium toFQDNs enforcement")
}

func assertCiliumFQDNPresent(t *testing.T, fqdns []interface{}, host, matchKey string) {
	t.Helper()
	for _, f := range fqdns {
		m, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		if m[matchKey] == host {
			return
		}
	}
	t.Fatalf("expected toFQDNs %s=%q", matchKey, host)
}

func TestBuildEgressProxyCalicoNetworkPolicy(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	u := buildEgressProxyCalicoNetworkPolicy(ep)

	assert.Equal(t, "projectcalico.org/v3", u.GetAPIVersion())
	assert.Equal(t, "NetworkPolicy", u.GetKind())
	assert.Equal(t, "shared-proxy-fqdn", u.GetName())
	assert.Equal(t, "team-a", u.GetNamespace())

	selector, found, err := unstructured.NestedString(u.Object, "spec", "selector")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, selector, "app == '"+proxyAppName+"'")
	assert.Contains(t, selector, egressProxyComponentLabel+" == 'shared'")

	types, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "types")
	assert.Equal(t, []string{"Egress"}, types, "Egress-only policy default-denies other egress")

	egress, found, err := unstructured.NestedSlice(u.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 3, "DNS UDP + DNS TCP + GitHub domains rule")

	githubRule := egress[2].(map[string]interface{})
	domains, found, err := unstructured.NestedStringSlice(githubRule, "destination", "domains")
	require.NoError(t, err)
	require.True(t, found)
	assert.ElementsMatch(t, githubEgressFQDNs, domains, "Calico domains mirror the GitHub FQDN set")
	assert.Equal(t, "Allow", githubRule["action"])
}

// TestGithubEgressFQDNs_CoversEndpointFamilies is a lightweight guard that the FQDN
// set keeps the api/git/checkout/blob endpoint families the CIDR feed covers, so an
// accidental deletion that would silently break GitHub egress fails CI.
func TestGithubEgressFQDNs_CoversEndpointFamilies(t *testing.T) {
	joined := strings.Join(githubEgressFQDNs, ",")
	for _, must := range []string{"api.github.com", "github.com", "actions.githubusercontent.com", "blob.core.windows.net"} {
		assert.Contains(t, joined, must)
	}
}
