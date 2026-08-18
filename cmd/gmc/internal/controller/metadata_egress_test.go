package controller

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// cloudMetadataIP is the link-local cloud metadata server (GCE, EC2 IMDS, Azure
// IMDS all answer here). It sits inside dnsNodeLocalCIDR, the link-local block
// the DNS rule admits, so nothing denies it by address — see
// TestBuildNetworkPolicy_DeniesCloudMetadataServer.
var cloudMetadataIP = net.ParseIP("169.254.169.254")

// egressAdmits reports whether np has an egress rule admitting proto/port to ip.
//
// It evaluates the two peer shapes that can match a bare IP: an empty To (which
// admits every destination) and an ipBlock. A selector peer is deliberately
// treated as no match — selectors resolve to pod IPs, and the metadata server is
// a node-scoped link-local address that no pod carries, so a selector peer
// cannot admit it however it is labelled.
//
// Port semantics follow NetworkPolicy: no Ports means every port, and a nil
// Protocol means TCP.
func egressAdmits(np *networkingv1.NetworkPolicy, ip net.IP, proto corev1.Protocol, port int32) bool {
	for _, rule := range np.Spec.Egress {
		if !ruleAdmitsPort(rule, proto, port) {
			continue
		}
		// An empty To is "any destination" — the shape that silently reopens the
		// metadata server no matter what the other rules say.
		if len(rule.To) == 0 {
			return true
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil && ipBlockContains(peer.IPBlock, ip) {
				return true
			}
		}
	}
	return false
}

func ruleAdmitsPort(rule networkingv1.NetworkPolicyEgressRule, proto corev1.Protocol, port int32) bool {
	if len(rule.Ports) == 0 {
		return true
	}
	for _, p := range rule.Ports {
		ruleProto := corev1.ProtocolTCP
		if p.Protocol != nil {
			ruleProto = *p.Protocol
		}
		if ruleProto != proto {
			continue
		}
		if p.Port == nil {
			return true
		}
		if p.EndPort != nil {
			if port >= p.Port.IntVal && port <= *p.EndPort {
				return true
			}
			continue
		}
		if p.Port.IntVal == port {
			return true
		}
	}
	return false
}

func ipBlockContains(block *networkingv1.IPBlock, ip net.IP) bool {
	_, cidr, err := net.ParseCIDR(block.CIDR)
	if err != nil || !cidr.Contains(ip) {
		return false
	}
	for _, except := range block.Except {
		if _, ex, err := net.ParseCIDR(except); err == nil && ex.Contains(ip) {
			return false
		}
	}
	return true
}

// TestBuildNetworkPolicy_DeniesCloudMetadataServer pins the property Q716 found
// untested: no policy governing a worker pod admits the cloud metadata server at
// 169.254.169.254:80.
//
// The mechanism is easy to misread, and the docs advising "a NetworkPolicy
// denying 169.254.169.254/32" describe an operator's cluster rather than this
// one. NetworkPolicy is allowlist-only and has no deny primitive, so nothing here
// names the metadata address at all. What keeps it unreachable is that the ONLY
// rule admitting its address — the link-local ipBlock 169.254.0.0/16 that
// NodeLocal DNSCache needs (Q136) — is scoped to port 53, which the metadata
// service does not serve. Widening that rule's ports, or adding any rule with an
// empty To, hands a worker pod the node's cloud credentials.
//
// Kata does not help: it bounds the kernel, not the pod network. Q226 measured
// the address still reachable from inside a Kata micro-VM on GKE, with the token
// endpoint returning HTTP 200 (docs/operations/kata-dind-workloads.md).
//
// This is the authoring-level half of the guard and runs on every PR. Live
// enforcement needs a policy-enforcing CNI and is asserted by
// E2E_V2_DirectEgress_MetadataServerBlocked on the Calico lane.
func TestBuildNetworkPolicy_DeniesCloudMetadataServer(t *testing.T) {
	require.NotNil(t, cloudMetadataIP, "metadata IP must parse")

	v1AG := newTestAG("gateway", "team-a")
	v2AG := v2Gateway("gateway", "team-a", "github-app", "proxy")
	v2Direct := v2Gateway("direct", "team-a", "github-app", "")
	_, githubCIDR, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	githubCIDRs := []net.IPNet{*githubCIDR}

	// Every policy that selects a worker pod, in both API versions and — for v2 —
	// both egress modes, because direct mode adds GitHub ipBlocks and is the shape
	// most likely to grow an over-broad peer.
	for _, np := range []*networkingv1.NetworkPolicy{
		buildProxyNetworkPolicy(v1AG, githubCIDRs),
		buildWorkloadNetworkPolicy(v1AG),
		buildAGCNetworkPolicy(v1AG, []string{"10.0.0.0/24"}),
		buildWorkloadNetworkPolicyV2(v2AG, githubCIDRs, false, nil),
		buildWorkloadNetworkPolicyV2(v2Direct, githubCIDRs, true, nil),
		buildAGCNetworkPolicyV2(v2AG, []string{"10.0.0.0/24"}, githubCIDRs, false, nil),
		buildAGCNetworkPolicyV2(v2Direct, []string{"10.0.0.0/24"}, githubCIDRs, true, nil),
	} {
		assert.False(t, egressAdmits(np, cloudMetadataIP, corev1.ProtocolTCP, 80),
			"%s admits TCP/80 to the cloud metadata server %s — a worker pod can read the node's "+
				"cloud credentials (Q716). NetworkPolicy cannot deny an address, so this means some "+
				"rule's ipBlock covers it on port 80, or a rule has an empty To.", np.Name, cloudMetadataIP)

		// 443 too: GKE serves metadata on 80, but IMDS variants and any future
		// metadata endpoint are not guaranteed to, and an allowlist that leaked 443
		// to link-local would be the same defect.
		assert.False(t, egressAdmits(np, cloudMetadataIP, corev1.ProtocolTCP, 443),
			"%s admits TCP/443 to the cloud metadata server %s (Q716)", np.Name, cloudMetadataIP)

		// The discriminator. Without this the test above would also pass if the
		// link-local DNS peer were deleted outright — which would break NodeLocal
		// DNSCache (Q136) while looking like a security improvement. Asserting the
		// port-53 path is still open is what makes the two failures distinguishable.
		assert.True(t, egressAdmits(np, cloudMetadataIP, corev1.ProtocolUDP, 53),
			"%s no longer admits UDP/53 to the link-local block — the NodeLocal DNSCache peer "+
				"(Q136) is gone, so the metadata assertions above prove nothing", np.Name)
	}
}
