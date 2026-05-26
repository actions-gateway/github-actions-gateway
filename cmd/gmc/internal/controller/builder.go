package controller

import (
	"fmt"
	"net"
	"strings"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
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

	agcSAName    = "actions-gateway-agc"
	workerSAName = "actions-gateway-worker"
	proxyAppName = "actions-gateway-proxy"
	agcAppName   = "actions-gateway-agc"

	// agcCredsVolumeName / agcCredsMountPath define how the GitHub App Secret is
	// projected into the AGC pod. Keys (appId, installationId, privateKey) are
	// mounted as read-only files; no credential ever appears in an env var.
	agcCredsVolumeName = "github-app-credentials"
	agcCredsMountPath  = "/etc/actions-gateway/github-app"

	proxyServiceName = "actions-gateway-proxy"
	proxyPort        = int32(8080)
	proxyHealthPort  = int32(8081)

	// npProxyName is the NetworkPolicy that restricts proxy pod egress to GitHub CIDRs.
	npProxyName = "actions-gateway-proxy"
	// npAGCName is the NetworkPolicy that gives AGC pods Kubernetes API server access (port 443).
	// Combined with npWorkloadName (additive), AGC pods can reach: DNS + proxy + k8s API.
	npAGCName = "actions-gateway-agc"
	// npWorkloadName is the NetworkPolicy that restricts AGC and worker pod egress to the proxy only.
	npWorkloadName = "actions-gateway-workload"

	// labelComponent / componentWorkload identify AGC and worker pods as "workload" for
	// NetworkPolicy podSelector matching.
	labelComponent    = "actions-gateway/component"
	componentWorkload = "workload"

	finalizerName = "actions-gateway.github.com/gmc-cleanup"

	// defaultNoProxy excludes cluster-internal traffic from the proxy. svc.cluster.local
	// covers all Kubernetes Services (e.g. fakegithub.e2e-infra.svc.cluster.local) so
	// the proxy is only used for external (GitHub.com) traffic as intended.
	defaultNoProxy = "svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12"
)

func ptr[T any](v T) *T { return &v }

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

// buildAGCRole builds the namespace-scoped Role for the AGC ServiceAccount.
func buildAGCRole(ag *gmcv1alpha1.ActionsGateway) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"pods/status"}, Verbs: []string{"get"}},
			// list/watch are required: the agent pool enumerates its own per-runner
			// Secrets to reconcile pool state (agentpool.Pool.listSecrets). RBAC does
			// not support glob resourceNames and the agent Secret names are dynamic,
			// so we cannot constrain by name. The "don't hold Secret bodies in process
			// memory" property from W3 is preserved at the AGC's manager level by
			// Client.Cache.DisableFor[*corev1.Secret] — reads go direct to the API
			// server rather than being held in the controller-runtime cache.
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			{APIGroups: []string{"actions-gateway.github.com"}, Resources: []string{"runnergroups"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"actions-gateway.github.com"}, Resources: []string{"runnergroups/status", "runnergroups/finalizers"}, Verbs: []string{"get", "update", "patch"}},
		},
	}
}

func buildAGCRoleBinding(ag *gmcv1alpha1.ActionsGateway) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: agcSAName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcSAName, Namespace: ag.Namespace}},
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

	// Ingress: only workload pods (AGC and workers) may connect to the proxy on proxyPort.
	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelComponent: componentWorkload},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
	}}

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
// Workload pods may only reach the proxy service (not GitHub CIDRs directly) and DNS.
// This enforces the design invariant that all GitHub-bound traffic routes through the
// per-tenant proxy pool, preserving egress-IP attribution per tenant.
func buildWorkloadNetworkPolicy(ag *gmcv1alpha1.ActionsGateway, proxyClusterIP string) *networkingv1.NetworkPolicy {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	egress := []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
			{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
		},
	}}
	if proxyClusterIP != "" {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: proxyClusterIP + "/32"}}},
		})
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
// It allows egress on port 443 (Kubernetes API server) in addition to the DNS
// and proxy egress that the workload NetworkPolicy already grants. Because
// NetworkPolicies are additive, AGC pods (selected by both this policy and
// buildWorkloadNetworkPolicy) end up with: DNS + proxy + k8s API egress.
// Worker pods (selected only by buildWorkloadNetworkPolicy) are limited to
// DNS + proxy — they cannot directly reach GitHub or the k8s API server.
//
// Port 443 egress has no `To` restriction because the Kubernetes API server
// is not a regular pod; its ClusterIP is not predictable at controller deploy
// time across all cloud providers.
func buildAGCNetworkPolicy(ag *gmcv1alpha1.ActionsGateway) *networkingv1.NetworkPolicy {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npAGCName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &proto53UDP, Port: ptr(intstr.FromInt32(53))},
						{Protocol: &proto53TCP, Port: ptr(intstr.FromInt32(53))},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
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

	false_ := false
	true_ := true

	// The proxy container image is gcr.io/distroless/static:nonroot which runs
	// as UID 65532. Mode 0o440 (rw-r-----) plus fsGroup 65532 below means the
	// non-root container reads the cert via group ownership without making it
	// world-readable. Mode 0o400 alone leaves files owned by root:root and the
	// container can't open them at all — that was the regression that surfaced
	// in e2e: `open /etc/actions-gateway/proxy-tls/tls.crt: permission denied`.
	tlsMode := int32(0o440)
	nonrootGID := int64(65532)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": proxyAppName, labelManagedBy: labelManagerValue}},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: &nonrootGID,
					},
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
					Volumes: []corev1.Volume{{
						Name: proxyTLSVolumeName,
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  proxyTLSSecretName,
								DefaultMode: &tlsMode,
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:      "proxy",
						Image:     proxyImage,
						Resources: res,
						Ports: []corev1.ContainerPort{
							{Name: "proxy", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: proxyHealthPort, Protocol: corev1.ProtocolTCP},
						},
						Env: []corev1.EnvVar{
							{Name: "PROXY_TLS_CERT_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_TLS_KEY_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      proxyTLSVolumeName,
							MountPath: proxyTLSMountPath,
							ReadOnly:  true,
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(proxyHealthPort)},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(proxyHealthPort)},
							},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &true_,
							ReadOnlyRootFilesystem:   &true_,
							AllowPrivilegeEscalation: &false_,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
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
	}
	env = append(env, extraEnv...)

	secretName := ag.Spec.GitHubAppRef.Name
	// 0o440 + fsGroup 65532 — see the matching block in buildProxyDeployment
	// for why 0o400 alone leaves the file unreadable to the non-root AGC user.
	credMode := int32(0o440)
	caMode := int32(0o444)
	nonrootGID := int64(65532)

	false_ := false
	true_ := true

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": agcAppName, labelManagedBy: labelManagerValue, labelComponent: componentWorkload}},
				Spec: corev1.PodSpec{
					ServiceAccountName: agcSAName,
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: &nonrootGID,
					},
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
					},
					Containers: []corev1.Container{{
						Name:  "agc",
						Image: agcImage,
						Env:   env,
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
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             &true_,
							ReadOnlyRootFilesystem:   &true_,
							AllowPrivilegeEscalation: &false_,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
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
