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
	assert.False(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeFQDN}))
	assert.False(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN}))
	assert.False(t, egressUsesCIDR(gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCalicoFQDN}))
}

func TestParseFQDNBackend(t *testing.T) {
	cases := []struct {
		in      string
		want    FQDNBackend
		wantErr bool
	}{
		{"", FQDNBackendNone, false}, // empty ⇒ secure default
		{"none", FQDNBackendNone, false},
		{"cilium", FQDNBackendCilium, false},
		{"calico", FQDNBackendCalico, false},
		{"gke", FQDNBackendGKE, false},
		{"Cilium", "", true}, // case-sensitive; a typo fails loudly
		{"aks", "", true},    // deferred backend, not yet valid
		{"bogus", "", true},
	}
	for _, tc := range cases {
		got, err := ParseFQDNBackend(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "ParseFQDNBackend(%q) should error", tc.in)
			continue
		}
		require.NoError(t, err, "ParseFQDNBackend(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ParseFQDNBackend(%q)", tc.in)
	}
}

// TestResolveFQDNEmitter is the table that replaces the old per-CNI enum switch (Q245):
// deprecated CiliumFQDN/CalicoFQDN pin their namesake mechanism regardless of backend,
// FQDN intent defers to the operator backend, FQDN+none emits nothing (admission-rejected),
// and CIDR/empty never emit.
func TestResolveFQDNEmitter(t *testing.T) {
	cases := []struct {
		name    string
		mode    gmcv2alpha1.EgressPolicyMode
		backend FQDNBackend
		want    fqdnEmitterKind
	}{
		{"CIDR any backend", gmcv2alpha1.EgressPolicyModeCIDR, FQDNBackendGKE, fqdnEmitNone},
		{"empty mode", "", FQDNBackendCilium, fqdnEmitNone},
		{"deprecated Cilium ignores backend none", gmcv2alpha1.EgressPolicyModeCiliumFQDN, FQDNBackendNone, fqdnEmitCilium},
		{"deprecated Cilium ignores backend gke", gmcv2alpha1.EgressPolicyModeCiliumFQDN, FQDNBackendGKE, fqdnEmitCilium},
		{"deprecated Calico ignores backend none", gmcv2alpha1.EgressPolicyModeCalicoFQDN, FQDNBackendNone, fqdnEmitCalico},
		{"FQDN + none emits nothing", gmcv2alpha1.EgressPolicyModeFQDN, FQDNBackendNone, fqdnEmitNone},
		{"FQDN + cilium", gmcv2alpha1.EgressPolicyModeFQDN, FQDNBackendCilium, fqdnEmitCilium},
		{"FQDN + calico", gmcv2alpha1.EgressPolicyModeFQDN, FQDNBackendCalico, fqdnEmitCalico},
		{"FQDN + gke", gmcv2alpha1.EgressPolicyModeFQDN, FQDNBackendGKE, fqdnEmitGKE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveFQDNEmitter(tc.mode, tc.backend))
		})
	}
}

func TestBuildEgressProxyGKEFQDNNetworkPolicy(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.DestinationFQDNs = []string{"proxy.golang.org"}
	})
	u := buildEgressProxyGKEFQDNNetworkPolicy(ep, nil)

	assert.Equal(t, "networking.gke.io/v1alpha1", u.GetAPIVersion())
	assert.Equal(t, "FQDNNetworkPolicy", u.GetKind())
	assert.Equal(t, "shared-proxy-fqdn", u.GetName())
	assert.Equal(t, "team-a", u.GetNamespace())
	assert.Equal(t, "shared", u.GetLabels()[egressProxyComponentLabel], "carries the managed identity label")

	// podSelector scopes to this pool's proxy pods.
	sel, found, err := unstructured.NestedStringMap(u.Object, "spec", "podSelector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, proxyAppName, sel["app"])
	assert.Equal(t, "shared", sel[egressProxyComponentLabel])

	// A single egress entry: GitHub (+ extra) FQDNs on TCP/443. NO DNS rule — DNS is
	// handled by the base NetworkPolicy, and GKE FQDN enforcement bypasses DNS (Q245).
	egress, found, err := unstructured.NestedSlice(u.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 1, "GKE FQDNNetworkPolicy carries a single GitHub-FQDN egress rule (no DNS rule)")

	rule := egress[0].(map[string]interface{})
	matches, found, err := unstructured.NestedSlice(rule, "matches")
	require.NoError(t, err)
	require.True(t, found)
	// Exact hosts become {name: …}; wildcards become {pattern: …}; the extra destination
	// is appended alongside the implicit GitHub set.
	assertGKEMatchPresent(t, matches, "name", "api.github.com")
	assertGKEMatchPresent(t, matches, "pattern", "*.actions.githubusercontent.com")
	assertGKEMatchPresent(t, matches, "name", "proxy.golang.org")
	assert.Len(t, matches, len(githubEgressFQDNs)+1, "matches = GitHub set + the one extra destination")

	ports, found, err := unstructured.NestedSlice(rule, "ports")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, ports, 1)
	p0 := ports[0].(map[string]interface{})
	assert.Equal(t, "TCP", p0["protocol"])
	assert.Equal(t, int64(443), p0["port"])
}

func assertGKEMatchPresent(t *testing.T, matches []interface{}, key, host string) {
	t.Helper()
	for _, m := range matches {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if mm[key] == host {
			return
		}
	}
	t.Fatalf("expected GKE match %s=%q", key, host)
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
		if hasGitHubCIDREgress(np, "140.82.112.0/20") {
			t.Fatalf("%s mode must not add the GitHub CIDR egress rule", mode)
		}
		assert.NotEmpty(t, np.Spec.Egress, "DNS egress is always present")
		// Two ingress rules regardless of egress mode: workload → proxy port, and the
		// monitoring → metrics-port scrape rule (Q324).
		require.Len(t, np.Spec.Ingress, 2, "%s mode keeps the workload + metrics ingress rules", mode)
	}
}

func TestBuildEgressProxyCiliumNetworkPolicy(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	u := buildEgressProxyCiliumNetworkPolicy(ep, nil)

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

	// The DNS rule must select both kube-dns and the node-local-dns redirect backend
	// (GKE Dataplane V2, Q229) as toEndpoints.
	toEndpoints, _, _ := unstructured.NestedSlice(dnsRule, "toEndpoints")
	require.Len(t, toEndpoints, 2, "DNS toEndpoints must cover kube-dns and node-local-dns (Q229)")
	dnsPods := map[string]bool{}
	for _, e := range toEndpoints {
		ml, _, _ := unstructured.NestedStringMap(e.(map[string]interface{}), "matchLabels")
		dnsPods[ml[dnsPodLabel]] = true
	}
	assert.True(t, dnsPods[dnsPodValue], "Cilium DNS rule must select kube-dns")
	assert.True(t, dnsPods[dnsNodeLocalPodValue], "Cilium DNS rule must select node-local-dns (Q229)")
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
	u := buildEgressProxyCalicoNetworkPolicy(ep, nil)

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
	require.Len(t, egress, 5, "DNS UDP/TCP to kube-dns + DNS UDP/TCP to node-local-dns + GitHub domains rule (Q229)")

	// The DNS rules must cover both kube-dns and the node-local-dns redirect backend
	// (GKE Dataplane V2, Q229), each on UDP and TCP.
	dnsSelectors := map[string]bool{}
	for _, e := range egress[:4] {
		sel, _, _ := unstructured.NestedString(e.(map[string]interface{}), "destination", "selector")
		dnsSelectors[sel] = true
	}
	assert.True(t, dnsSelectors[dnsPodLabel+" == '"+dnsPodValue+"'"], "Calico DNS rule must select kube-dns")
	assert.True(t, dnsSelectors[dnsPodLabel+" == '"+dnsNodeLocalPodValue+"'"], "Calico DNS rule must select node-local-dns (Q229)")

	githubRule := egress[4].(map[string]interface{})
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
