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

	proxyServiceName = "actions-gateway-proxy"
	proxyPort        = int32(8080)
	proxyHealthPort  = int32(8081)

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

// buildNetworkPolicy constructs the tenant NetworkPolicy enforcing the two-tier egress model.
func buildNetworkPolicy(ag *gmcv1alpha1.ActionsGateway, proxyClusterIP string, githubCIDRs []net.IPNet) *networkingv1.NetworkPolicy {
	proto53UDP := corev1.ProtocolUDP
	proto53TCP := corev1.ProtocolTCP

	// Proxy pod egress: DNS always, GitHub CIDRs on 443 when managedNetworkPolicy is true.
	proxyEgress := []networkingv1.NetworkPolicyEgressRule{{
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
		proxyEgress = append(proxyEgress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
			To:    peers,
		})
	}

	// AGC + worker egress: proxy Service ClusterIP on proxyPort.
	var agcWorkerEgress []networkingv1.NetworkPolicyEgressRule
	if proxyClusterIP != "" {
		agcWorkerEgress = []networkingv1.NetworkPolicyEgressRule{{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: proxyClusterIP + "/32"}}},
		}}
	}

	// Ingress to proxy pods: from within the namespace only.
	proxyIngress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
	}}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "actions-gateway", Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			// NetworkPolicy applies per pod-selector; we use a single policy with multiple egress entries.
			// The "proxy" selector egress rules are listed first; AGC/worker egress appended.
			Egress:  append(proxyEgress, agcWorkerEgress...),
			Ingress: proxyIngress,
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
	if ag.Spec.Proxy.Resources.Requests != nil || ag.Spec.Proxy.Resources.Limits != nil {
		res = ag.Spec.Proxy.Resources
	}

	false_ := false
	true_ := true

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: proxyServiceName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": proxyAppName, labelManagedBy: labelManagerValue}},
				Spec: corev1.PodSpec{
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
					Containers: []corev1.Container{{
						Name:      "proxy",
						Image:     proxyImage,
						Resources: res,
						Ports: []corev1.ContainerPort{
							{Name: "proxy", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: proxyHealthPort, Protocol: corev1.ProtocolTCP},
						},
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
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", proxyServiceName, ag.Namespace, proxyPort)
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
func buildAGCDeployment(ag *gmcv1alpha1.ActionsGateway, agcImage, proxyServiceAddr string, extraEnv []corev1.EnvVar) *appsv1.Deployment {
	env := []corev1.EnvVar{
		{Name: "GITHUB_APP_ID", ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "appId")},
		{Name: "GITHUB_APP_PRIVATE_KEY", ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "privateKey")},
		{Name: "GITHUB_APP_INSTALLATION_ID", ValueFrom: secretKeyRef(ag.Spec.GitHubAppRef, "installationId")},
		{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
		{Name: "WORKER_SERVICE_ACCOUNT", Value: workerSAName},
		{Name: "HTTP_PROXY", Value: proxyServiceAddr},
		{Name: "HTTPS_PROXY", Value: proxyServiceAddr},
		{Name: "NO_PROXY", Value: buildNoProxy(ag.Spec.Proxy.NoProxyCIDRs)},
	}
	env = append(env, extraEnv...)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace, Labels: managedLabels(ag)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agcAppName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": agcAppName, labelManagedBy: labelManagerValue}},
				Spec: corev1.PodSpec{
					ServiceAccountName: agcSAName,
					Containers: []corev1.Container{{
						Name:  "agc",
						Image: agcImage,
						Env:   env,
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

func secretKeyRef(ref gmcv1alpha1.SecretReference, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
			Key:                  key,
		},
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
