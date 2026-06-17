package controller

import (
	"fmt"
	"net"
	"sort"
	"strings"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	gmcnames "github.com/actions-gateway/github-actions-gateway/gmc/names"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	labelManagedBy    = "app.kubernetes.io/managed-by"
	labelManagerValue = "actions-gateway-gmc"

	agcSAName    = agcnames.ControllerName
	workerSAName = agcnames.WorkerSAName
	proxyAppName = gmcnames.ProxyName
	agcAppName   = agcnames.ControllerName

	// agcTenantRoleName is the shipped singleton ClusterRole that defines
	// the AGC permission set. Per-tenant RoleBindings reference it; the GMC
	// holds `bind` on this exact name so it never needs `escalate` or to
	// hold the AGC's full permission set itself.
	agcTenantRoleName = "agc-tenant-role"

	// agcCredsVolumeName / agcCredsMountPath define how the GitHub App Secret is
	// projected into the AGC pod. Keys (appId, installationId, privateKey) are
	// mounted as read-only files; no credential ever appears in an env var.
	agcCredsVolumeName = "github-app-credentials"          //nolint:gosec // G101: a volume name, not a credential
	agcCredsMountPath  = "/etc/actions-gateway/github-app" //nolint:gosec // G101: a mount-path constant, not a credential

	proxyServiceName = gmcnames.ProxyName
	// proxyServiceMonitorName / agcServiceMonitorName are the per-tenant
	// Prometheus-Operator ServiceMonitors that scrape the proxy and AGC mTLS
	// metrics ports (Q72). One per component so each carries the correct TLS
	// serverName for its Service.
	proxyServiceMonitorName = proxyServiceName + "-metrics"
	agcServiceMonitorName   = agcAppName + "-metrics"
	proxyPort               = int32(8080)
	// healthMetricsPort is the plaintext health listener port (/healthz,
	// /readyz) probed by the kubelet on both the proxy and the AGC pods. It
	// carries no Prometheus metrics — those moved to the mTLS metricsPort below
	// so blanket client-cert enforcement on the metrics listener does not break
	// the certless kubelet probes. The AGC manager pins its health bind address
	// to this port (healthProbeBindAddress in cmd/agc/main.go).
	healthMetricsPort = int32(8081)

	// metricsPort is the dedicated mTLS Prometheus-metrics listener port for
	// both the proxy and the AGC. Each serves /metrics over HTTPS and requires a
	// client certificate signed by the per-tenant metrics CA (see
	// metrics_cert.go); the metrics-scrape NetworkPolicy ingress rule targets
	// this port. The GMC pins the AGC manager's metrics bind address here
	// (cmd/agc/main.go) so the ingress rule provably matches the live listener.
	metricsPort = int32(8443)

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

	// npProxyName is the NetworkPolicy that restricts proxy pod egress to GitHub CIDRs.
	npProxyName = gmcnames.ProxyName
	// npAGCName is the NetworkPolicy that gives AGC pods Kubernetes API server access (port 443).
	// Combined with npWorkloadName (additive), AGC pods can reach: DNS + proxy + k8s API.
	npAGCName = agcnames.ControllerName
	// npWorkloadName is the NetworkPolicy that restricts AGC and worker pod egress to the proxy only.
	npWorkloadName = gmcnames.WorkloadNetworkPolicyName

	// labelComponent / componentWorkload identify AGC and worker pods as "workload" for
	// NetworkPolicy podSelector matching.
	labelComponent    = "actions-gateway/component"
	componentWorkload = "workload"

	finalizerName = "actions-gateway.github.com/gmc-cleanup"

	// defaultNoProxy excludes cluster-internal traffic from the proxy. svc.cluster.local
	// covers all Kubernetes Services (e.g. fakegithub.e2e-infra.svc.cluster.local) so
	// the proxy is only used for external (GitHub.com) traffic as intended.
	defaultNoProxy = "svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12"

	// defaultSecurityProfile is the PSA enforcement level applied when an
	// ActionsGateway omits spec.securityProfile. Mirrors the CRD's
	// +kubebuilder:default; kept here so hand-applied CRs without the field
	// still get baseline rather than an empty (unenforced) profile.
	defaultSecurityProfile = "baseline"
)

func ptr[T any](v T) *T { return &v }

// securityProfileOrDefault returns the configured security profile, falling
// back to defaultSecurityProfile when unset.
func securityProfileOrDefault(profile string) string {
	if profile == "" {
		return defaultSecurityProfile
	}
	return profile
}

// hardenedContainerSecurityContext returns the restricted container
// SecurityContext applied to every GMC-managed container (AGC and proxy):
// non-root, read-only root filesystem, no privilege escalation, all Linux
// capabilities dropped, and the RuntimeDefault seccomp profile. Defining it once
// keeps the security baseline from drifting between Deployments — hardening (or
// accidentally relaxing) one container must not silently leave the other behind.
func hardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr(true),
		ReadOnlyRootFilesystem:   ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// nonrootPodSecurityContext returns the pod-level SecurityContext shared by the
// AGC and proxy Deployments: fsGroup 65532 so the distroless nonroot UID can read
// group-owned mounted Secrets (cert/key files projected with mode 0o440). See the
// TLS-mode comment in buildProxyDeployment for the 0o440-vs-0o400 rationale.
func nonrootPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{FSGroup: ptr(int64(65532))}
}

func managedLabels(ag *gmcv1alpha1.ActionsGateway) map[string]string {
	return map[string]string{
		labelManagedBy:               labelManagerValue,
		"actions-gateway/owner-name": ag.Name,
		"actions-gateway/owner-ns":   ag.Namespace,
	}
}

func buildAGCServiceAccount(ag *gmcv1alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
	}
}

func buildWorkerServiceAccount(ag *gmcv1alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: workerSAName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
	}
}

// buildAGCRoleBinding binds the per-tenant AGC ServiceAccount to the shipped
// agc-tenant-role ClusterRole. A RoleBinding to a ClusterRole scopes those
// permissions to the binding's namespace only. This pattern lets the GMC
// avoid `escalate` and avoid holding the AGC's full permission set itself —
// it only needs `bind` on the agc-tenant-role ClusterRole name.
func buildAGCRoleBinding(ag *gmcv1alpha1.ActionsGateway) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: agcTenantRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcSAName, Namespace: ag.Namespace}},
	}
}

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
// them. Two `To` peers (OR'd) cover the two ways a pod reaches cluster DNS:
//
//  1. The kube-dns / CoreDNS Service in kube-system, matched by an AND of
//     namespace + pod selector (the direct path on a cluster without NodeLocal
//     DNSCache).
//  2. The IPv4 link-local block 169.254.0.0/16, matched by an ipBlock (the path
//     on a cluster running NodeLocal DNSCache, where pods send DNS to a
//     link-local address served by a per-node hostNetwork cache — Q136).
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
// kindnet does not enforce egress NetworkPolicy (see Q7b in
// docs/plan/worker-egress-proxy.md), so this restriction is guarded at the
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

// buildProxyNetworkPolicy constructs the NetworkPolicy for proxy pods.
// Proxy pods may reach GitHub CIDRs on 443 (for CONNECT tunneling) and DNS.
// Only workload pods (AGC and workers) may initiate connections to the proxy.
func buildProxyNetworkPolicy(ag *gmcv1alpha1.ActionsGateway, githubCIDRs []net.IPNet) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgressRule()}
	managed := ag.Spec.Proxy.ManagedNetworkPolicy == nil || *ag.Spec.Proxy.ManagedNetworkPolicy
	if managed && len(githubCIDRs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(githubCIDRs))
		for _, cidr := range githubCIDRs {
			c := cidr.String()
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
			To:    peers,
		})
	}

	// Ingress: only workload pods (AGC and workers) may connect to the proxy on
	// proxyPort; only monitoring namespaces may scrape the health/metrics port.
	ingress := []networkingv1.NetworkPolicyIngressRule{
		{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelComponent: componentWorkload},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
		},
		metricsScrapeIngressRule(),
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npProxyName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      egress,
			Ingress:     ingress,
		},
	}
}

// buildWorkloadNetworkPolicy constructs the NetworkPolicy for AGC and worker pods.
// Workload pods may only reach the proxy pods (not GitHub CIDRs directly) and DNS.
// This enforces the design invariant that all GitHub-bound traffic routes through the
// per-tenant proxy pool, preserving egress-IP attribution per tenant.
//
// The egress rule targets the proxy by PodSelector rather than the Service ClusterIP.
// kube-proxy DNATs ClusterIP → PodIP before NetworkPolicy enforcement, so an
// `ipBlock: <ClusterIP>/32` rule never matches actual packets and silently drops all
// proxy-bound traffic. Selecting the proxy pods directly matches post-DNAT destinations.
//
// Ingress: the policy declares PolicyTypeIngress with no ingress rules — default-deny
// all inbound to workload pods (Q128). Worker pods run untrusted GitHub Actions job
// code and are outbound-only by design (they long-poll/dial out to GitHub via the proxy
// and to the AGC); nothing in the cluster legitimately initiates a connection *to* a
// worker. Without this, worker pods were default-allow ingress, so any pod in the
// cluster could open connections to untrusted job code — a lateral-movement /
// cross-tenant channel that contradicts the per-tenant isolation model. kubelet
// liveness/readiness probes originate from the node and are not subject to
// NetworkPolicy, so default-deny does not break health checks. The AGC pod is also
// selected here (it carries the workload label), but its own buildAGCNetworkPolicy
// additively re-admits the monitoring metrics scrape, so default-deny here costs it
// nothing.
func buildWorkloadNetworkPolicy(ag *gmcv1alpha1.ActionsGateway) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{
		dnsEgressRule(),
		{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			}},
		},
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npWorkloadName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: componentWorkload}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      egress,
			// No Ingress rules: PolicyTypeIngress with an empty rule set is default-deny.
		},
	}
}

// buildAGCNetworkPolicy constructs the NetworkPolicy for AGC pods only.
// It allows egress on the Kubernetes API server ports (443 and 6443) in
// addition to the DNS and proxy egress that the workload NetworkPolicy
// already grants. Because NetworkPolicies are additive, AGC pods (selected by
// both this policy and buildWorkloadNetworkPolicy) end up with: DNS + proxy +
// k8s API egress. Worker pods (selected only by buildWorkloadNetworkPolicy)
// are limited to DNS + proxy — they cannot directly reach GitHub or the k8s
// API server.
//
// Why both 443 and 6443: NetworkPolicy port matches are evaluated against the
// *post-DNAT* destination port. In production clusters the `kubernetes`
// Service typically points at backends listening on 443, so a 443 rule
// matches. In kind, the apiserver runs inside the control-plane container on
// 6443 and the Service does port translation (10.96.0.1:443 → node:6443), so
// the policy evaluator sees 6443 — a 443-only rule never matches, and the
// AGC silently loses k8s API access. This is the port-axis equivalent of the
// `ipBlock: <ClusterIP>/32` trap that bit the proxy NP in PR #59. See
// docs/development/networkpolicy-port-matching.md for the full diagnosis. Allowing both keeps the
// policy precise (only apiserver-style ports) while working in both kind and
// every production deployment topology this controller targets.
//
// The egress rule has no `To` restriction because the Kubernetes API server
// is not a regular pod; its ClusterIP/node IPs are not predictable at
// controller deploy time across all cloud providers.
//
// Ingress: the policy declares PolicyTypeIngress (default-deny) and admits only
// monitoring-namespace scrapes of the metrics port. Nothing else connects to
// the AGC on ingress — it is a pure client (it long-polls the GitHub broker,
// calls the k8s API, and dials the proxy), so default-deny closes L-8: without
// this, the AGC NP carried no ingress policy type and any pod in the namespace
// could scrape per-tenant metrics off the controller-runtime metrics server.
func buildAGCNetworkPolicy(ag *gmcv1alpha1.ActionsGateway) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npAGCName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{metricsScrapeIngressRule()},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: ptr(intstr.FromInt32(443))},
						{Port: ptr(intstr.FromInt32(6443))},
					},
				},
			},
		},
	}
}

func buildProxyDeployment(ag *gmcv1alpha1.ActionsGateway, proxyImage string) *appsv1.Deployment {
	replicas := int32(2)
	if ag.Spec.Proxy.MinReplicas != nil {
		replicas = *ag.Spec.Proxy.MinReplicas
	}

	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			// 500m, not 100m: a 100m limit throttles the proxy before the HPA's
			// 60%-utilization signal can trip, so the pool never scales out under
			// real CONNECT load. Operators can override via spec.proxy.resources.
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	for k, v := range ag.Spec.Proxy.Resources.Requests {
		res.Requests[k] = v
	}
	for k, v := range ag.Spec.Proxy.Resources.Limits {
		res.Limits[k] = v
	}

	// The proxy container image is gcr.io/distroless/static:nonroot which runs
	// as UID 65532. Mode 0o440 (rw-r-----) plus fsGroup 65532 (nonrootPodSecurityContext)
	// means the non-root container reads the cert via group ownership without making it
	// world-readable. Mode 0o400 alone leaves files owned by root:root and the
	// container can't open them at all — that was the regression that surfaced
	// in e2e: `open /etc/actions-gateway/proxy-tls/tls.crt: permission denied`.
	tlsMode := int32(0o440)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": proxyAppName, labelManagedBy: labelManagerValue}},
				Spec: corev1.PodSpec{
					SecurityContext: nonrootPodSecurityContext(),
					// 60s lets in-flight CONNECT tunnels drain on rollout/eviction;
					// the proxy's SIGTERM handler closes the listener and shuts the
					// servers down (cmd/proxy/proxy.go) within this window rather than
					// being SIGKILLed mid-tunnel.
					TerminationGracePeriodSeconds: ptr(int64(60)),
					// Required (not preferred) anti-affinity: proxy replicas must
					// land on distinct nodes so a single node failure or drain never
					// takes the whole tenant's egress pool down at once. Preferred
					// scheduling let both replicas co-locate, which silently defeated
					// the PodDisruptionBudget. Trade-off: the proxy pool needs at
					// least Spec.Proxy.MinReplicas schedulable nodes — on a
					// single-node dev/kind cluster (test/kind-config-1worker.yaml)
					// the second replica stays Pending; set proxy.minReplicas=1
					// there. The default kind config ships two worker nodes.
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
								TopologyKey:   "kubernetes.io/hostname",
							}},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: proxyTLSVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  proxyTLSSecretName,
									DefaultMode: &tlsMode,
								},
							},
						},
						{
							// Metrics mTLS server bundle (ca.crt + tls.crt + tls.key).
							// The proxy serves /metrics over mTLS on metricsPort and
							// verifies scraper client certs against ca.crt.
							Name: metricsTLSVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  metricsTLSSecretName,
									DefaultMode: &tlsMode,
								},
							},
						},
					},
					Containers: []corev1.Container{{
						Name:      "proxy",
						Image:     proxyImage,
						Resources: res,
						Ports: []corev1.ContainerPort{
							{Name: "proxy", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: healthMetricsPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP},
						},
						Env: []corev1.EnvVar{
							{Name: "PROXY_TLS_CERT_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_TLS_KEY_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
							{Name: "PROXY_METRICS_PORT", Value: fmt.Sprintf("%d", metricsPort)},
							{Name: "PROXY_METRICS_TLS_CERT_FILE", Value: metricsTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_METRICS_TLS_KEY_FILE", Value: metricsTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
							{Name: "PROXY_METRICS_CLIENT_CA_FILE", Value: metricsTLSMountPath + "/" + metricsCACertKey},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      proxyTLSVolumeName,
								MountPath: proxyTLSMountPath,
								ReadOnly:  true,
							},
							{
								Name:      metricsTLSVolumeName,
								MountPath: metricsTLSMountPath,
								ReadOnly:  true,
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(healthMetricsPort)},
							},
						},
						SecurityContext: hardenedContainerSecurityContext(),
					}},
				},
			},
		},
	}
}

// metricsServiceLabels returns the metadata labels stamped on a metrics Service
// so its per-tenant ServiceMonitor can select it precisely: the managed labels
// (which include the owner-name/owner-ns that scope the selector to a single
// tenant) plus the component `app` label that distinguishes the proxy Service
// from the AGC Service within the namespace.
func metricsServiceLabels(ag *gmcv1alpha1.ActionsGateway, appName string) map[string]string {
	labels := managedLabels(ag)
	labels["app"] = appName
	return labels
}

func buildProxyService(ag *gmcv1alpha1.ActionsGateway) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: metricsServiceLabels(ag, proxyAppName)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": proxyAppName},
			Ports: []corev1.ServicePort{
				{Name: "proxy", Port: proxyPort, TargetPort: intstr.FromInt32(proxyPort), Protocol: corev1.ProtocolTCP},
				// metrics: the mTLS Prometheus listener (metricsPort/:8443). Q69
				// ships the serving side; this port + the proxy ServiceMonitor make
				// it scrapeable (Q72). Named "metrics" so the ServiceMonitor endpoint
				// can target it by name without scraping the proxy's :8080 data port.
				{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromInt32(metricsPort), Protocol: corev1.ProtocolTCP},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// buildAGCService builds the AGC metrics Service. The AGC pod is otherwise a
// pure client (no Service today), but its controller-runtime metrics server
// listens on metricsPort (:8443); a Service is needed so a ServiceMonitor can
// discover and scrape it (Q72). It is named agcAppName so the metrics server
// cert SANs (metricsServerSANs) already cover its DNS names and the scrape
// verifies without insecureSkipVerify.
func buildAGCService(ag *gmcv1alpha1.ActionsGateway) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace, Labels: metricsServiceLabels(ag, agcAppName)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": agcAppName},
			Ports:    []corev1.ServicePort{{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromInt32(metricsPort), Protocol: corev1.ProtocolTCP}},
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

// metricsServiceDNSName returns the in-cluster `<svc>.<ns>.svc` DNS name used as
// the TLS serverName when scraping a metrics Service. It is one of the SANs on
// the per-tenant metrics server cert (metricsServerSANs), so a ServiceMonitor
// setting serverName to this value verifies the scrape against the metrics CA.
func metricsServiceDNSName(ag *gmcv1alpha1.ActionsGateway, svcName string) string {
	return fmt.Sprintf("%s.%s.svc", svcName, ag.Namespace)
}

// serviceMonitorGVK is the Prometheus-Operator ServiceMonitor GroupVersionKind.
// Per-tenant ServiceMonitors are built as unstructured objects so the GMC does
// not take a compile-time dependency on the prometheus-operator API module;
// the monitoring.coreos.com CRD is an optional, operator-installed prerequisite
// (see applyOrPruneServiceMonitors for the graceful CRD-absent handling).
var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
}

// buildMetricsServiceMonitor builds a per-tenant ServiceMonitor that scrapes one
// component's (proxy or AGC) mTLS metrics port. It is built as an unstructured
// object (see serviceMonitorGVK). The ServiceMonitor lives in the tenant
// namespace and — with no namespaceSelector — selects only Services in that
// namespace, so a tenant's monitor can never select another tenant's pods.
//
// The selector matches the component's metrics Service by its managed labels
// (which include owner-name/owner-ns) plus the `app` label, and the single
// endpoint scrapes the Service's `metrics` port over HTTPS. mTLS is satisfied by
// presenting the per-tenant scraper client bundle from the
// actions-gateway-metrics-client Secret (published by buildMetricsClientSecret
// in this same namespace): tls.crt/tls.key authenticate the scraper to the
// listener, and ca.crt verifies the listener's server cert. serverName is the
// component Service's `<svc>.<ns>.svc` DNS name, which is a SAN on that server
// cert, so the scrape verifies without insecureSkipVerify.
func buildMetricsServiceMonitor(ag *gmcv1alpha1.ActionsGateway, smName, appName, svcName string) *unstructured.Unstructured {
	secretRef := func(key string) map[string]interface{} {
		return map[string]interface{}{
			"secret": map[string]interface{}{
				"name": metricsClientSecretName,
				"key":  key,
			},
		}
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(smName)
	sm.SetNamespace(ag.Namespace)
	sm.SetLabels(metricsServiceLabels(ag, appName))

	spec := map[string]interface{}{
		"selector": map[string]interface{}{
			"matchLabels": toStringMapIface(metricsServiceLabels(ag, appName)),
		},
		"endpoints": []interface{}{
			map[string]interface{}{
				"port":   "metrics",
				"path":   "/metrics",
				"scheme": "https",
				"tlsConfig": map[string]interface{}{
					"serverName": metricsServiceDNSName(ag, svcName),
					"ca":         secretRef(metricsCACertKey),
					"cert":       secretRef(corev1.TLSCertKey),
					// keySecret is a bare SecretKeySelector (no enclosing "secret").
					"keySecret": map[string]interface{}{
						"name": metricsClientSecretName,
						"key":  corev1.TLSPrivateKeyKey,
					},
				},
			},
		},
	}
	// unstructured.SetNestedMap deep-copies spec into sm.Object; it only errors on
	// non-JSON value types, and every value above is a JSON-compatible type.
	if err := unstructured.SetNestedMap(sm.Object, spec, "spec"); err != nil {
		panic(fmt.Sprintf("build ServiceMonitor spec: %v", err))
	}
	return sm
}

// toStringMapIface converts a map[string]string to the map[string]interface{}
// shape unstructured nested content requires.
func toStringMapIface(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func buildProxyServiceAddr(ag *gmcv1alpha1.ActionsGateway) string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", proxyServiceName, ag.Namespace, proxyPort)
}

// buildProxyCertSecret constructs the Secret that holds the proxy's self-signed TLS cert+key.
// The proxy Deployment mounts both; the AGC Deployment mounts only the cert (public part)
// via an Items projection so the private key never reaches the AGC container.
func buildProxyCertSecret(ag *gmcv1alpha1.ActionsGateway, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyTLSSecretName,
			Namespace: ag.Namespace,
			Labels:    managedLabels(ag),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
}

// buildMetricsTLSSecret constructs the server bundle Secret mounted into the AGC
// and proxy pods: the metrics CA cert (to verify scraper client certs) plus the
// metrics server cert+key. It is a kubernetes.io/tls Secret with an extra ca.crt key.
func buildMetricsTLSSecret(ag *gmcv1alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metricsTLSSecretName,
			Namespace: ag.Namespace,
			Labels:    managedLabels(ag),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.serverCertPEM,
			corev1.TLSPrivateKeyKey: b.serverKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

// buildMetricsClientSecret constructs the scraper bundle Secret published for the
// monitoring stack: the metrics CA cert (to verify the metrics server) plus the
// scraper client cert+key (to authenticate to the AGC/proxy metrics listeners).
// It is never mounted into AGC/proxy pods.
func buildMetricsClientSecret(ag *gmcv1alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metricsClientSecretName,
			Namespace: ag.Namespace,
			Labels:    managedLabels(ag),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.clientCertPEM,
			corev1.TLSPrivateKeyKey: b.clientKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

func buildPDB(ag *gmcv1alpha1.ActionsGateway) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
		},
	}
}

func buildHPA(ag *gmcv1alpha1.ActionsGateway) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas := int32(2)
	if ag.Spec.Proxy.MinReplicas != nil {
		minReplicas = *ag.Spec.Proxy.MinReplicas
	}
	maxReplicas := int32(10)
	if ag.Spec.Proxy.MaxReplicas != nil {
		maxReplicas = *ag.Spec.Proxy.MaxReplicas
	}
	targetCPU := int32(60)
	if ag.Spec.Proxy.TargetCPUUtilizationPercentage != nil {
		targetCPU = *ag.Spec.Proxy.TargetCPUUtilizationPercentage
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: proxyServiceName},
			MinReplicas:    &minReplicas,
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &targetCPU,
					},
				},
			}},
		},
	}
}

// buildAGCDeployment builds the AGC Deployment in the tenant namespace.
// GitHub App credentials are mounted from a Secret as files under agcCredsMountPath;
// they never appear in environment variables (kubectl describe pod is safe to run in
// the presence of an operator).
func buildAGCDeployment(ag *gmcv1alpha1.ActionsGateway, agcImage, proxyServiceAddr string, extraEnv []corev1.EnvVar) *appsv1.Deployment {
	env := []corev1.EnvVar{
		{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
		{Name: "WORKER_SERVICE_ACCOUNT", Value: workerSAName},
		// GITHUB_ORG_URL is the GitHub org/enterprise/repo URL the AGC registers
		// runners against, threaded from spec.gitHubURL. This is the first-class
		// production path; it replaces the testing-only AGC_EXTRA_GITHUB_ORG_URL
		// passthrough. Placed before extraEnv so a testing override (when
		// --allow-agc-extra-env is set) still wins on conflict, mirroring tracing.
		{Name: "GITHUB_ORG_URL", Value: ag.Spec.GitHubURL},
		{Name: "HTTP_PROXY", Value: proxyServiceAddr},
		{Name: "HTTPS_PROXY", Value: proxyServiceAddr},
		{Name: "NO_PROXY", Value: buildNoProxy(ag.Spec.Proxy.NoProxyCIDRs)},
		// PROXY_TLS_SECRET_NAME tells the AGC's pod provisioner which Secret
		// to project into worker pods so Runner.Worker can validate the
		// egress proxy's TLS cert. The Secret name is deterministic
		// per-tenant but is plumbed via env so the AGC pod spec
		// self-describes the dependency (and a `kubectl describe pod` on the
		// AGC surfaces it). Symmetric to the proxy-CA mount the AGC itself
		// already consumes for outbound API calls.
		{Name: "PROXY_TLS_SECRET_NAME", Value: proxyTLSSecretName},
		// SECURITY_PROFILE mirrors spec.securityProfile so the AGC's pod
		// provisioner scales the secure-by-default worker SecurityContext to the
		// namespace's PSA level. Default to baseline when unset (the API server
		// applies the CRD default, but be explicit so a hand-applied CR without
		// the field still gets the hardened defaults rather than none).
		{Name: "SECURITY_PROFILE", Value: securityProfileOrDefault(ag.Spec.SecurityProfile)},
		// GITHUB_RUNNER_VERSION is the pinned actions/runner version (single
		// source of truth: agcnames.RunnerVersion, which also drives the AGC's
		// default worker image). The AGC forwards it as agent.version on
		// CreateSession; GitHub validates the runner version at session
		// creation, so leaving it empty risks rejection (Q71/Q118).
		{Name: "GITHUB_RUNNER_VERSION", Value: agcnames.RunnerVersion},
	}
	// Tracing config (spec.tracing) maps to the standard OTEL_* env the AGC
	// reads. Appended before extraEnv so the testing-gated AGC_EXTRA_OTEL_*
	// passthrough (when enabled) still wins on conflict.
	env = append(env, tracingEnv(ag.Spec.Tracing)...)
	env = append(env, extraEnv...)

	secretName := ag.Spec.GitHubAppRef.Name
	// 0o440 + fsGroup 65532 — see the matching block in buildProxyDeployment
	// for why 0o400 alone leaves the file unreadable to the non-root AGC user.
	credMode := int32(0o440)
	caMode := int32(0o444)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": agcAppName, labelManagedBy: labelManagerValue, labelComponent: componentWorkload},
					// Record the referenced Secret name so that kubectl rollout history
					// shows the cause of any credential-rotation rolling update.
					Annotations: map[string]string{
						"actions-gateway/github-app-secret": ag.Spec.GitHubAppRef.Name,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: agcSAName,
					SecurityContext:    nonrootPodSecurityContext(),
					// 60s lets the AGC's signal handler (ctrl.SetupSignalHandler in
					// cmd/agc/main.go) drain in-flight session work and release its
					// listener-renewal lock cleanly on rollout instead of losing the
					// lock to a SIGKILL.
					TerminationGracePeriodSeconds: ptr(int64(60)),
					Volumes: []corev1.Volume{
						{
							Name: agcCredsVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  secretName,
									DefaultMode: &credMode,
								},
							},
						},
						{
							// Proxy CA cert (public part only — private key excluded via Items).
							// AGC uses this to pin the proxy's TLS cert rather than trusting
							// the cluster CA, preventing MITM even from a compromised cluster CA.
							Name: proxyCACertVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: proxyTLSSecretName,
									Items: []corev1.KeyToPath{{
										Key:  corev1.TLSCertKey,
										Path: corev1.TLSCertKey,
									}},
									DefaultMode: &caMode,
								},
							},
						},
						{
							// Metrics mTLS server bundle (ca.crt + tls.crt + tls.key).
							// The AGC's controller-runtime metrics server serves
							// /metrics over mTLS on metricsPort and verifies scraper
							// client certs against ca.crt.
							Name: metricsTLSVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  metricsTLSSecretName,
									DefaultMode: &credMode,
								},
							},
						},
					},
					Containers: []corev1.Container{{
						Name:  "agc",
						Image: agcImage,
						Env:   env,
						// The AGC pins its controller-runtime metrics server to
						// metricsPort and its health server to healthMetricsPort
						// (cmd/agc/main.go); declaring the ports documents the
						// listeners — metrics is the one buildAGCNetworkPolicy's
						// metrics-scrape ingress rule admits, health is kubelet-only.
						Ports: []corev1.ContainerPort{
							{Name: "health", ContainerPort: healthMetricsPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      agcCredsVolumeName,
								MountPath: agcCredsMountPath,
								ReadOnly:  true,
							},
							{
								Name:      proxyCACertVolumeName,
								MountPath: proxyCACertMountPath,
								ReadOnly:  true,
							},
							{
								Name:      metricsTLSVolumeName,
								MountPath: metricsTLSMountPath,
								ReadOnly:  true,
							},
						},
						// StartupProbe gives the AGC manager's informer cache room to
						// sync before liveness takes over (30 × 5s = 150s), mirroring
						// the GMC manager's probe. The AGC binds its health listener
						// early in mgr.Start — independently of the initial GitHub App
						// token fetch, which runs as a manager Runnable (see
						// cmd/agc/main.go) — so this budget covers cache sync, not the
						// token exchange.
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							FailureThreshold: 30,
							PeriodSeconds:    5,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							PeriodSeconds: 20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(healthMetricsPort)},
							},
							PeriodSeconds: 10,
						},
						SecurityContext: hardenedContainerSecurityContext(),
					}},
				},
			},
		},
	}
}

// buildRunnerGroup builds a RunnerGroup CR from a spec entry.
func buildRunnerGroup(ag *gmcv1alpha1.ActionsGateway, spec agcv1alpha1.RunnerGroupSpec, name string) *agcv1alpha1.RunnerGroup {
	return &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec:       spec,
	}
}

func fieldRef(path string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
}

// tracingEnv translates spec.tracing into the standard OpenTelemetry OTEL_*
// environment variables the AGC reads (cmd/agc/internal/tracing). It returns
// nil when tracing is off — Endpoint is the opt-in switch, so an empty Endpoint
// yields no OTEL_* env and the AGC keeps its no-op tracer provider. By design
// there is no mapping for OTEL_EXPORTER_OTLP_HEADERS: auth headers can carry
// credentials and this project keeps secrets out of environment variables.
func tracingEnv(t gmcv1alpha1.TracingConfig) []corev1.EnvVar {
	if t.Endpoint == "" {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: t.Endpoint},
	}
	if t.Insecure != nil && *t.Insecure {
		env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_TRACES_INSECURE", Value: "true"})
	}
	if t.Sampler != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER", Value: t.Sampler})
	}
	if t.SamplerArg != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER_ARG", Value: t.SamplerArg})
	}
	if attrs := formatResourceAttributes(t.ResourceAttributes); attrs != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: attrs})
	}
	return env
}

// formatResourceAttributes renders a resource-attribute map as the
// comma-separated key=value list OTEL_RESOURCE_ATTRIBUTES expects. Keys are
// sorted so the rendered value is deterministic — without this the random map
// iteration order would churn the AGC Deployment on every reconcile.
func formatResourceAttributes(attrs map[string]string) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+attrs[k])
	}
	return strings.Join(pairs, ",")
}

// buildNoProxy merges user-provided CIDRs with mandatory cluster-internal exclusions.
func buildNoProxy(userCIDRs []string) string {
	if len(userCIDRs) > 0 {
		return strings.Join(userCIDRs, ",") + "," + defaultNoProxy
	}
	return defaultNoProxy
}
