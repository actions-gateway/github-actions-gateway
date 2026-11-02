package controller

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// managedNetworkPolicies returns every NetworkPolicy the GMC authors: a proxy pool's,
// and a proxied and a direct-egress gateway's AGC and workload policies. managed sets
// the pool's spec.managedNetworkPolicy.
func managedNetworkPolicies(managed bool) []*networkingv1.NetworkPolicy {
	_, githubCIDR, _ := net.ParseCIDR("140.82.112.0/20")
	githubCIDRs := []net.IPNet{*githubCIDR}
	ep := newEP("proxy", "team-a", func(ep *gmcv2alpha1.EgressProxy) { ep.Spec.ManagedNetworkPolicy = ptr(managed) })
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
