package controller

import (
	"fmt"
	"net"
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
	agcCredsVolumeName = "github-app-credentials"
	agcCredsMountPath  = "/etc/actions-gateway/github-app"

	proxyServiceName = gmcnames.ProxyName
	proxyPort        = int32(8080)
	// healthMetricsPort is the proxy's plaintext health listener port
	// (/healthz, /readyz) probed by the kubelet. It carries no Prometheus
	// metrics anymore — those moved to the mTLS metricsPort below so blanket
	// client-cert enforcement on the metrics listener does not break the
	// certless kubelet probes. The AGC has no health probes, so this port is
	// unused on the AGC side.
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
	// Mirrors the kubebuilder convention used by the GMC's own
	// allow-metrics-traffic NetworkPolicy
	// (cmd/gmc/config/network-policy/allow-metrics-traffic.yaml): an operator
	// labels their Prometheus namespace `metrics: enabled`. Kubelet probe
	// traffic originates from the node and is exempted from NetworkPolicy
	// enforcement by every CNI this project targets, so no explicit kubelet
	// ingress rule is needed — and node IPs are not portably expressible as a
	// NetworkPolicy peer anyway.
	metricsScrapeNamespaceLabel = "metrics"
	metricsScrapeNamespaceValue = "enabled"

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

// buildProxyNetworkPolicy constructs the NetworkPolicy for proxy pods.
// Proxy pods may reach GitHub CIDRs on 443 (for CONNECT tunneling) and DNS.
// Only workload pods (AGC and workers) may initiate connections to the proxy.
func buildProxyNetworkPolicy(ag *gmcv1alpha1.ActionsGateway, githubCIDRs []net.IPNet) *networkingv1.NetworkPolicy {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	egress := []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
			{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
		},
	}}
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
func buildWorkloadNetworkPolicy(ag *gmcv1alpha1.ActionsGateway) *networkingv1.NetworkPolicy {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
				{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
			},
		},
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
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
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
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npAGCName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{metricsScrapeIngressRule()},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
						{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
					},
				},
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

func buildResourceQuota(ag *gmcv1alpha1.ActionsGateway) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "actions-gateway", Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec:       corev1.ResourceQuotaSpec{Hard: ag.Spec.NamespaceQuota},
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
			corev1.ResourceCPU:    resource.MustParse("100m"),
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
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
								Weight: 100,
								PodAffinityTerm: corev1.PodAffinityTerm{
									LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
									TopologyKey:   "kubernetes.io/hostname",
								},
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

func buildProxyService(ag *gmcv1alpha1.ActionsGateway) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": proxyAppName},
			Ports:    []corev1.ServicePort{{Name: "proxy", Port: proxyPort, TargetPort: intstr.FromInt32(proxyPort), Protocol: corev1.ProtocolTCP}},
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
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
	}
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
						// metricsPort (cmd/agc/main.go); declaring the port documents
						// the listener that buildAGCNetworkPolicy's metrics-scrape
						// ingress rule admits.
						Ports: []corev1.ContainerPort{
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

// buildNoProxy merges user-provided CIDRs with mandatory cluster-internal exclusions.
func buildNoProxy(userCIDRs []string) string {
	if len(userCIDRs) > 0 {
		return strings.Join(userCIDRs, ",") + "," + defaultNoProxy
	}
	return defaultNoProxy
}
