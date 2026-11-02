package controller

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
)

// managedNetworkPolicies returns every NetworkPolicy the GMC authors: a proxy pool's,
// and a proxied and a direct-egress gateway's AGC and workload policies. managed sets
// the pool's spec.managedNetworkPolicy.
func managedNetworkPolicies(managed bool) []*networkingv1.NetworkPolicy {
	_, githubCIDR, _ := net.ParseCIDR("140.82.112.0/20")
	githubCIDRs := []net.IPNet{*githubCIDR}
	ep := newEP("proxy", "team-a", func(ep *gmcv2.EgressProxy) { ep.Spec.ManagedNetworkPolicy = ptr(managed) })
	proxied := v2Gateway("gateway", "team-a", "github-app", "proxy")
	direct := v2Gateway("direct", "team-a", "github-app", "")
	return []*networkingv1.NetworkPolicy{
		buildEgressProxyNetworkPolicy(ep, githubCIDRs),
		buildWorkloadNetworkPolicyV2(proxied, githubCIDRs, false, nil),
		buildWorkloadNetworkPolicyV2(direct, githubCIDRs, true, nil),
		buildAGCNetworkPolicyV2(proxied, []string{"10.0.0.0/24"}, githubCIDRs, false, nil),
		buildAGCNetworkPolicyV2(direct, []string{"10.0.0.0/24"}, githubCIDRs, true, nil),
	}
}

func TestBuildNetworkPolicy_DNSEgressAlwaysPresent(t *testing.T) {
	for _, managed := range []bool{true, false} {
		for _, np := range managedNetworkPolicies(managed) {
			udpFound, tcpFound := false, false
			for _, rule := range np.Spec.Egress {
				for _, port := range rule.Ports {
					if port.Port != nil && port.Port.IntVal == 53 {
						if port.Protocol != nil && *port.Protocol == corev1.ProtocolUDP {
							udpFound = true
						}
						if port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP {
							tcpFound = true
						}
					}
				}
			}
			assert.True(t, udpFound, "expected DNS UDP egress in %s when managedNetworkPolicy=%v", np.Name, managed)
			assert.True(t, tcpFound, "expected DNS TCP egress in %s when managedNetworkPolicy=%v", np.Name, managed)
		}
	}
}

// TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS locks in Q105 + Q136: the
// port-53 egress rule on every GMC-managed NetworkPolicy must target cluster DNS
// only and must NOT be open (To: nil ≡ any resolver). An open DNS path is an
// unattributed exfiltration side-channel that bypasses the per-tenant egress-IP
// attribution every other egress path enforces.
//
// Q136 + Q229 widened the rule to three OR'd peers so every NodeLocal DNSCache
// topology resolves:
//
//   - the kube-dns / CoreDNS pods in kube-system (AND of namespace + pod
//     selector — the direct path),
//   - the node-local-dns pods in kube-system (AND of namespace + pod selector —
//     the redirect-to-pod path on GKE Dataplane V2 / Cilium Local Redirect, Q229),
//     and
//   - the link-local block 169.254.0.0/16 (ipBlock — the classic link-local cache
//     path, Q136).
//
// All three peers stay within Q105's attribution property: each is cluster DNS on
// port 53 only, never an arbitrary external resolver (link-local is non-routable
// and node-scoped). This is an authoring/spec-level guard because kindnet does not
// enforce egress NetworkPolicy (see Q7b) — mirroring the egress-negative guard
// pattern. The node-local-dns peer's runtime effect was verified end-to-end on a
// live GKE Dataplane V2 cluster (Q229).
func TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS(t *testing.T) {
	for _, np := range managedNetworkPolicies(true) {
		var dnsRules []networkingv1.NetworkPolicyEgressRule
		for _, rule := range np.Spec.Egress {
			for _, port := range rule.Ports {
				if port.Port != nil && port.Port.IntVal == 53 {
					dnsRules = append(dnsRules, rule)
					break
				}
			}
		}
		require.Len(t, dnsRules, 1, "%s must carry exactly one port-53 egress rule", np.Name)

		rule := dnsRules[0]
		require.NotEmpty(t, rule.To,
			"%s port-53 rule must have a To peer — an empty To opens DNS to any resolver (Q105)", np.Name)
		require.Len(t, rule.To, 3,
			"%s DNS rule must select kube-dns, node-local-dns, AND the link-local block (Q136/Q229)", np.Name)

		// Peer 1: the cluster DNS Service (kube-dns / CoreDNS) selector peer.
		kubeDNS := rule.To[0]
		assert.Nil(t, kubeDNS.IPBlock, "%s kube-dns DNS peer must not be an ipBlock", np.Name)
		require.NotNil(t, kubeDNS.NamespaceSelector, "%s DNS peer must select the kube-dns namespace", np.Name)
		require.NotNil(t, kubeDNS.PodSelector, "%s DNS peer must select the kube-dns pods", np.Name)
		assert.Equal(t, dnsNamespaceValue, kubeDNS.NamespaceSelector.MatchLabels[dnsNamespaceLabel],
			"%s DNS peer namespace selector must match kube-system", np.Name)
		assert.Equal(t, dnsPodValue, kubeDNS.PodSelector.MatchLabels[dnsPodLabel],
			"%s DNS peer pod selector must match k8s-app=kube-dns", np.Name)

		// Peer 2: the NodeLocal DNSCache redirect-to-pod selector peer (Q229). AND of
		// namespace + pod selector, scoped to k8s-app=node-local-dns in kube-system —
		// never a bare podSelector that would admit node-local-dns in any namespace.
		nodeLocalPod := rule.To[1]
		assert.Nil(t, nodeLocalPod.IPBlock, "%s node-local-dns DNS peer must not be an ipBlock", np.Name)
		require.NotNil(t, nodeLocalPod.NamespaceSelector, "%s node-local-dns DNS peer must select the namespace", np.Name)
		require.NotNil(t, nodeLocalPod.PodSelector, "%s node-local-dns DNS peer must select the pods", np.Name)
		assert.Equal(t, dnsNamespaceValue, nodeLocalPod.NamespaceSelector.MatchLabels[dnsNamespaceLabel],
			"%s node-local-dns DNS peer namespace selector must match kube-system", np.Name)
		assert.Equal(t, dnsNodeLocalPodValue, nodeLocalPod.PodSelector.MatchLabels[dnsPodLabel],
			"%s node-local-dns DNS peer pod selector must match k8s-app=node-local-dns", np.Name)

		// Peer 3: the NodeLocal DNSCache link-local ipBlock (Q136).
		nodeLocal := rule.To[2]
		require.NotNil(t, nodeLocal.IPBlock, "%s DNS rule must allow the node-local link-local block (Q136)", np.Name)
		assert.Nil(t, nodeLocal.NamespaceSelector, "%s node-local DNS peer must be a bare ipBlock", np.Name)
		assert.Nil(t, nodeLocal.PodSelector, "%s node-local DNS peer must be a bare ipBlock", np.Name)
		assert.Equal(t, "169.254.0.0/16", nodeLocal.IPBlock.CIDR,
			"%s node-local DNS peer must be the link-local block 169.254.0.0/16", np.Name)
		assert.Empty(t, nodeLocal.IPBlock.Except,
			"%s node-local DNS ipBlock must not carve out exceptions", np.Name)
	}
}

// TestBuildAGCNetworkPolicy_MetricsScrapeIngress locks in L-8 for the AGC: the
// policy must declare PolicyTypeIngress (default-deny) and admit metrics scrapes
// only from monitoring namespaces, so no other pod in the namespace can scrape
// per-tenant controller-runtime metrics.
func TestBuildAGCNetworkPolicy_MetricsScrapeIngress(t *testing.T) {
	np := buildAGCNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"), nil, nil, false, nil)

	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress,
		"AGC NP must declare PolicyTypeIngress so ingress defaults to deny")
	found := false
	for _, rule := range np.Spec.Ingress {
		hasMetricsPort := false
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == metricsPort {
				hasMetricsPort = true
			}
		}
		if !hasMetricsPort {
			continue
		}
		found = true
		require.Len(t, rule.From, 1, "metrics-scrape rule must have exactly one peer")
		peer := rule.From[0]
		require.NotNil(t, peer.NamespaceSelector,
			"metrics-scrape rule must select by namespace, not pod or IP")
		assert.Equal(t, metricsScrapeNamespaceValue,
			peer.NamespaceSelector.MatchLabels[metricsScrapeNamespaceLabel],
			"metrics-scrape rule must restrict to the monitoring namespace selector")
		assert.Nil(t, peer.PodSelector, "metrics-scrape rule must not allow arbitrary pods")
		assert.Nil(t, peer.IPBlock, "metrics-scrape rule must not allow arbitrary IPs")
	}
	assert.True(t, found, "expected an ingress rule for the metrics port %d", metricsPort)
}

func TestBuildAGCNetworkPolicy_KubernetesAPIEgressAllowed(t *testing.T) {
	for _, direct := range []bool{false, true} {
		np := buildAGCNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"), nil, nil, direct, nil)

		// The AGC NP must allow egress on BOTH 443 and 6443:
		//   - 443: production clusters where the kubernetes Service backends listen on 443.
		//   - 6443: kind (and any cluster where the apiserver Endpoints listen on 6443) —
		//     kube-proxy DNATs the Service ClusterIP:443 → node-ip:6443, and NetworkPolicy
		//     enforcement evaluates the post-DNAT port. A 443-only rule silently drops
		//     k8s API traffic in kind. See docs/development/networkpolicy-port-matching.md.
		saw443, saw6443 := false, false
		for _, rule := range np.Spec.Egress {
			for _, port := range rule.Ports {
				if port.Port == nil {
					continue
				}
				switch port.Port.IntVal {
				case 443:
					saw443 = true
					assert.Empty(t, rule.To,
						"port-443 rule must allow egress to any destination for k8s API access")
				case 6443:
					saw6443 = true
					assert.Empty(t, rule.To,
						"port-6443 rule must allow egress to any destination for k8s API access")
				}
			}
		}
		assert.True(t, saw443, "AGC NP must include egress rule for port 443 (k8s API server, production)")
		assert.True(t, saw6443,
			"AGC NP must include egress rule for port 6443 (k8s API server post-DNAT in kind — "+
				"docs/development/networkpolicy-port-matching.md)")
	}
}

// TestBuildAGCNetworkPolicy_APIServerCIDRsScoped locks in Q145: when the
// operator supplies an apiserver-CIDR allowlist, the AGC NP's 443/6443 egress
// rule is scoped to those CIDRs via ipBlock peers (a strict tightening) while
// the ports are unchanged.
func TestBuildAGCNetworkPolicy_APIServerCIDRsScoped(t *testing.T) {
	cidrs := []string{"10.0.0.1/32", "172.16.0.0/12"}
	np := buildAGCNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"), cidrs, nil, false, nil)

	// Find the apiserver-ports egress rule (the one carrying 443/6443).
	var apiRule *networkingv1.NetworkPolicyEgressRule
	for i := range np.Spec.Egress {
		for _, port := range np.Spec.Egress[i].Ports {
			if port.Port != nil && (port.Port.IntVal == 443 || port.Port.IntVal == 6443) {
				apiRule = &np.Spec.Egress[i]
				break
			}
		}
	}
	require.NotNil(t, apiRule, "AGC NP must still carry the apiserver-ports egress rule")

	// Ports unchanged: both 443 and 6443 still present.
	saw443, saw6443 := false, false
	for _, port := range apiRule.Ports {
		if port.Port == nil {
			continue
		}
		switch port.Port.IntVal {
		case 443:
			saw443 = true
		case 6443:
			saw6443 = true
		}
	}
	assert.True(t, saw443 && saw6443, "scoping must not drop either apiserver port")

	// `To` must list exactly one ipBlock peer per supplied CIDR.
	require.Len(t, apiRule.To, len(cidrs),
		"apiserver egress must be scoped to one ipBlock peer per operator CIDR")
	got := make([]string, 0, len(apiRule.To))
	for _, peer := range apiRule.To {
		require.NotNil(t, peer.IPBlock, "scoped apiserver egress peer must be an ipBlock")
		assert.Nil(t, peer.PodSelector, "scoped apiserver egress peer must not select pods")
		assert.Nil(t, peer.NamespaceSelector, "scoped apiserver egress peer must not select namespaces")
		got = append(got, peer.IPBlock.CIDR)
	}
	assert.ElementsMatch(t, cidrs, got, "scoped CIDRs must match the operator allowlist")
}

// TestBuildAGCNetworkPolicy_APIServerCIDRsEmptyKeepsAnyDest asserts the
// secure-default Q145 behavior: an empty (or nil) allowlist leaves the apiserver
// ports open to any destination, no `To`.
func TestBuildAGCNetworkPolicy_APIServerCIDRsEmptyKeepsAnyDest(t *testing.T) {
	ag := v2Gateway("gateway", "team-a", "github-app", "proxy")
	for _, cidrs := range [][]string{nil, {}} {
		np := buildAGCNetworkPolicyV2(ag, cidrs, nil, false, nil)
		for _, rule := range np.Spec.Egress {
			for _, port := range rule.Ports {
				if port.Port != nil && (port.Port.IntVal == 443 || port.Port.IntVal == 6443) {
					assert.Empty(t, rule.To,
						"empty apiserver-CIDR allowlist must leave the apiserver egress any-destination")
				}
			}
		}
	}
}

func TestBuildWorkloadNetworkPolicy_EgressToProxy(t *testing.T) {
	np := buildWorkloadNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"), nil, false, nil)
	assert.Equal(t, componentWorkload, np.Spec.PodSelector.MatchLabels[labelComponent],
		"workload NP must select workload-labeled pods")

	// The proxy peer must be a PodSelector, not an ipBlock on the Service ClusterIP.
	// kube-proxy DNATs ClusterIP → PodIP before NetworkPolicy enforcement, so an
	// ipBlock rule on the Service IP never matches.
	found := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == proxyPort {
				for _, peer := range rule.To {
					if peer.PodSelector != nil {
						found = true
					}
					assert.Nil(t, peer.IPBlock,
						"workload NP must not target proxy via ipBlock — DNAT defeats it")
				}
			}
		}
	}
	assert.True(t, found, "expected workload egress rule to select proxy pods by label on port 8080")
}

// TestBuildWorkloadNetworkPolicy_NoGitHubEgressWhenProxied: a proxied gateway's
// workers reach GitHub only through the pool, so no 443 rule of any shape may
// appear, whatever GitHub CIDRs the cache holds.
func TestBuildWorkloadNetworkPolicy_NoGitHubEgressWhenProxied(t *testing.T) {
	_, githubCIDR, _ := net.ParseCIDR("140.82.112.0/20")
	np := buildWorkloadNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"),
		[]net.IPNet{*githubCIDR}, false, nil)

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				t.Errorf("workload NP must not allow direct egress on port 443")
			}
		}
	}
}

// TestBuildWorkloadNetworkPolicy_DefaultDenyIngress locks in Q128: worker pods run
// untrusted GitHub Actions job code and must accept no inbound connections. The
// authoring guard asserts the generated spec declares PolicyTypeIngress with an empty
// ingress rule set (= default-deny). This is the reliable CI gate because kindnet does
// not enforce ingress NetworkPolicy (see Q7b, worker-egress isolation); a
// runtime negative needs a policy-enforcing CNI such as Calico.
func TestBuildWorkloadNetworkPolicy_DefaultDenyIngress(t *testing.T) {
	_, githubCIDR, _ := net.ParseCIDR("140.82.112.0/20")
	for _, direct := range []bool{false, true} {
		np := buildWorkloadNetworkPolicyV2(v2Gateway("gateway", "team-a", "github-app", "proxy"),
			[]net.IPNet{*githubCIDR}, direct, nil)

		assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress,
			"workload NP must declare PolicyTypeIngress so ingress to worker pods defaults to deny")
		assert.Empty(t, np.Spec.Ingress,
			"workload NP must carry no ingress rules — default-deny all inbound to untrusted worker pods")
	}
}
