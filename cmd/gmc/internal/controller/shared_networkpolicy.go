package controller

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The NetworkPolicy rule vocabulary shared by every policy the GMC emits — the v1
// ActionsGateway's proxy/workload/AGC policies, the v2 ActionsGateway's
// workload/AGC policies, and the standalone EgressProxy's pool policy. Defining
// each rule once is what keeps the egress posture from drifting between versions:
// a change to DNS confinement or the GitHub allowlist lands everywhere at once.

const (
	// metricsScrapeNamespaceLabel / metricsScrapeNamespaceValue select the
	// namespace(s) permitted to scrape the proxy and AGC metrics port.
	// Mirrors the convention used by the GMC's own metrics-allow
	// NetworkPolicy shipped by the chart
	// (charts/actions-gateway/templates/networkpolicy.yaml): an operator
	// labels their Prometheus namespace `metrics: enabled`. Kubelet probe
	// traffic originates from the node and is exempted from NetworkPolicy
	// enforcement by every CNI this project targets, so no explicit kubelet
	// ingress rule is needed — and node IPs are not portably expressible as a
	// NetworkPolicy peer anyway.
	metricsScrapeNamespaceLabel = "metrics"
	metricsScrapeNamespaceValue = "enabled"

	// dnsNamespaceLabel / dnsNamespaceValue and dnsPodLabel / dnsPodValue select
	// the cluster DNS service (CoreDNS / kube-dns) as the sole permitted DNS
	// egress peer. The namespace is matched via the well-known immutable
	// `kubernetes.io/metadata.name` label that every Kubernetes ≥1.21 stamps on
	// each namespace (so no manual labelling of kube-system is required); the
	// pods via the conventional `k8s-app: kube-dns` label CoreDNS carries by
	// default in every distribution this controller targets. See dnsEgressRule
	// for why egress is confined to this peer (Q105).
	dnsNamespaceLabel = "kubernetes.io/metadata.name"
	dnsNamespaceValue = "kube-system"
	dnsPodLabel       = "k8s-app"
	dnsPodValue       = "kube-dns"

	// dnsNodeLocalPodValue selects the NodeLocal DNSCache pods (`node-local-dns`),
	// the third permitted DNS egress peer (Q229). On clusters where the CNI
	// transparently redirects cluster-DNS traffic to a per-node cache *pod* — GKE
	// Dataplane V2 (Cilium) drives this via a `RedirectService`/Cilium Local
	// Redirect that rewrites the kube-dns ClusterIP to the local node-local-dns
	// pod — the policy is enforced against that redirect *backend's* identity, not
	// the kube-dns Service or pods. node-local-dns carries `k8s-app: node-local-dns`
	// (not `kube-dns`) and, on Dataplane V2, runs with a regular pod IP and
	// `-setupinterface=false` (no 169.254.x link-local address), so neither the
	// kube-dns podSelector nor the link-local ipBlock below matches it and DNS is
	// dropped. Selecting node-local-dns by its conventional label restores
	// resolution while staying within Q105's attribution property (still cluster
	// DNS only, port 53 only — never an arbitrary resolver). Harmless on clusters
	// without NodeLocal DNSCache: the selector simply matches no pod. See
	// dnsEgressRule.
	dnsNodeLocalPodValue = "node-local-dns"

	// dnsNodeLocalCIDR is the IPv4 link-local block (RFC 3927). On clusters
	// running NodeLocal DNSCache (node-local-dns), pods send DNS to a link-local
	// address — 169.254.20.10 by the kube-standard `__PILLAR__LOCAL__DNS__`
	// convention — served by a hostNetwork DNSCache pod on each node, which the
	// kube-dns podSelector cannot match. Allowing the whole 169.254.0.0/16 block
	// is the simplest correct rule and stays within Q105's attribution property:
	// link-local is non-routable and node-scoped, so it cannot reach an arbitrary
	// external resolver — the DNS-exfiltration channel Q105 closed stays closed
	// (Q136). See dnsEgressRule.
	dnsNodeLocalCIDR = "169.254.0.0/16"
)

// metricsScrapeIngressRule returns a NetworkPolicy ingress rule that permits
// Prometheus scrapes of the mTLS metrics port (metricsPort) from any namespace
// labelled metrics=enabled. It is applied to both the proxy and AGC
// NetworkPolicies so per-tenant traffic-volume metrics (CONNECT counts, active
// tunnels, dial errors) are reachable only by the operator's monitoring stack,
// not by every pod in the tenant namespace (L-8). The plaintext kubelet probe
// port (healthMetricsPort) carries no metrics and needs no rule — see
// metricsScrapeNamespaceLabel for why kubelet probe traffic is already exempt.
func metricsScrapeIngressRule() networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{metricsScrapeNamespaceLabel: metricsScrapeNamespaceValue},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(metricsPort))}},
	}
}

// dnsEgressRule returns a NetworkPolicy egress rule permitting DNS (UDP/TCP 53)
// to the cluster DNS service ONLY — never to any destination. It is shared by
// the proxy, workload, and AGC policies so the DNS posture cannot drift between
// them. Three `To` peers (OR'd) cover the ways a pod reaches cluster DNS:
//
//  1. The kube-dns / CoreDNS Service in kube-system, matched by an AND of
//     namespace + pod selector (the direct path on a cluster without NodeLocal
//     DNSCache).
//  2. The NodeLocal DNSCache pods (`k8s-app: node-local-dns`) in kube-system,
//     matched by an AND of namespace + pod selector. On a CNI that redirects
//     cluster-DNS traffic to a per-node cache *pod* — GKE Dataplane V2 (Cilium)
//     does this via a RedirectService / Cilium Local Redirect — the egress is
//     enforced against the redirect backend's identity, which is node-local-dns,
//     not kube-dns; without this peer DNS is silently dropped (Q229).
//  3. The IPv4 link-local block 169.254.0.0/16, matched by an ipBlock (the path
//     on a cluster running NodeLocal DNSCache in the classic link-local mode,
//     where pods send DNS to a link-local address served by a per-node
//     hostNetwork cache — Q136).
//
// An unrestricted port-53 rule (To: nil ≡ any server) is an unattributed
// data-exfiltration side-channel: DNS queries can smuggle data to an
// attacker-controlled resolver, bypassing the per-tenant egress-IP attribution
// that is a headline isolation property of this system (Q105). Every other
// egress path forces traffic through the tenant proxy, whose source IPs are
// attributable; confining DNS to the in-cluster resolver keeps it on that
// attributable path — kube-dns recurses upstream on the pod's behalf, so the
// proxy can still resolve GitHub hostnames to do its job. Both peers preserve
// that property: link-local 169.254.0.0/16 is non-routable and node-scoped, so
// it cannot reach an external resolver. Only the open "any resolver" breadth is
// removed, not legitimate resolution.
//
// kindnet does not enforce egress NetworkPolicy (see Q7b, worker-egress
// isolation), so this restriction is guarded at the
// spec/authoring level by TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS
// rather than by a live e2e deny test; a runtime negative needs a
// policy-enforcing CNI such as Calico.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			// A single peer with both selectors set is an AND: kube-dns pods *within*
			// kube-system. Splitting them into two peers would be an OR and would also
			// admit any pod labelled k8s-app=kube-dns in any namespace.
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{dnsNamespaceLabel: dnsNamespaceValue},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{dnsPodLabel: dnsPodValue},
				},
			},
			// NodeLocal DNSCache redirect-to-pod path (Q229): on a CNI that redirects
			// the kube-dns ClusterIP to a per-node node-local-dns *pod* (GKE Dataplane
			// V2 / Cilium Local Redirect), policy is enforced against that pod's
			// identity. It carries k8s-app=node-local-dns, not kube-dns, so the peer
			// above does not match it. AND of namespace + pod selector, same as kube-dns.
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{dnsNamespaceLabel: dnsNamespaceValue},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{dnsPodLabel: dnsNodeLocalPodValue},
				},
			},
			// NodeLocal DNSCache path: pods reach the per-node hostNetwork cache at a
			// link-local address (169.254.20.10 by convention). hostNetwork pods are
			// not matched by any pod/namespace selector, so this peer is an ipBlock.
			// The block is non-routable, so it does not widen the exfil surface.
			{
				IPBlock: &networkingv1.IPBlock{CIDR: dnsNodeLocalCIDR},
			},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
			{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
		},
	}
}

// githubCIDREgressRule returns an egress rule permitting TCP/443 to the given GitHub
// CIDRs, and false when the CIDR set is empty (before the IPRangeReconciler's first
// fetch) so the caller omits the rule entirely. Omitting it is fail-closed: GitHub
// egress is denied until the refresh loop patches the policy with the fetched ranges,
// never opened wide. Shared by the v1 proxy NetworkPolicy and the v2 direct-egress
// AGC/workload NetworkPolicies (§H.10).
func githubCIDREgressRule(githubCIDRs []net.IPNet) (networkingv1.NetworkPolicyEgressRule, bool) {
	if len(githubCIDRs) == 0 {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(githubCIDRs))
	for _, cidr := range githubCIDRs {
		c := cidr.String()
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
	}
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
		To:    peers,
	}, true
}

// agcAPIServerEgressRule returns the AGC's Kubernetes API-server egress rule
// (443/6443), optionally scoped to apiServerCIDRs (Q145): an empty list leaves
// `To` nil — any-destination, the secure default that does not depend on a
// predictable apiserver IP. Shared by the v1 and v2 AGC NetworkPolicy builders.
//
// Why both 443 and 6443: NetworkPolicy port matches are evaluated against the
// *post-DNAT* destination port. In production clusters the `kubernetes` Service
// typically points at backends listening on 443, so a 443 rule matches. In kind,
// the apiserver runs inside the control-plane container on 6443 and the Service
// does port translation (10.96.0.1:443 → node:6443), so the policy evaluator sees
// 6443 — a 443-only rule never matches, and the AGC silently loses k8s API access.
// This is the port-axis equivalent of the `ipBlock: <ClusterIP>/32` trap that bit
// the proxy NP in PR #59. See docs/development/networkpolicy-port-matching.md for
// the full diagnosis.
func agcAPIServerEgressRule(apiServerCIDRs []string) networkingv1.NetworkPolicyEgressRule {
	rule := networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Port: ptr(intstr.FromInt32(443))},
			{Port: ptr(intstr.FromInt32(6443))},
		},
	}
	if len(apiServerCIDRs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(apiServerCIDRs))
		for _, cidr := range apiServerCIDRs {
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
		}
		rule.To = peers
	}
	return rule
}

// buildAGCNetworkPolicyFrom assembles the AGC egress policy shared by the v1 and
// v2 ActionsGateway reconcilers: additive to the workload policy, it permits DNS +
// Kubernetes API server egress and admits monitoring-namespace metrics scrapes.
// The callers differ in the metadata labels and, under v2 multi-gateway, in the
// policy name and the `app` selector value (both per-gateway, so each AGC NP
// selects exactly its own gateway's AGC pods).
//
// Ingress: the policy declares PolicyTypeIngress (default-deny) and admits only
// monitoring-namespace scrapes of the metrics port. Nothing else connects to the
// AGC on ingress — it is a pure client (it long-polls the GitHub broker, calls the
// k8s API, and dials the proxy), so default-deny closes L-8: without this, the AGC
// NP carried no ingress policy type and any pod in the namespace could scrape
// per-tenant metrics off the controller-runtime metrics server.
func buildAGCNetworkPolicyFrom(namespace, name, appLabel string, labels map[string]string, apiServerCIDRs []string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{metricsScrapeIngressRule()},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				agcAPIServerEgressRule(apiServerCIDRs),
			},
		},
	}
}
