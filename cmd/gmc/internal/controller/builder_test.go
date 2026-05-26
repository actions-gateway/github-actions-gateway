package controller

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
)

func newTestAG(name, ns string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "github-app"},
		},
	}
}

func envMap(envs []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(envs))
	for _, e := range envs {
		m[e.Name] = e
	}
	return m
}

func TestBuildAGCDeployment_CredentialMount(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	env := envMap(c.Env)

	// GitHub App credentials must NOT appear as env var secretKeyRefs.
	for _, key := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_INSTALLATION_ID"} {
		_, ok := env[key]
		assert.False(t, ok, "credential env var %s must not be present; use file mount instead", key)
	}

	// Secret must be mounted as a volume.
	var credVol *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		if dep.Spec.Template.Spec.Volumes[i].Name == agcCredsVolumeName {
			credVol = &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	require.NotNil(t, credVol, "github-app-credentials volume must be present")
	require.NotNil(t, credVol.Secret, "volume source must be Secret")
	assert.Equal(t, "github-app", credVol.Secret.SecretName)
	require.NotNil(t, credVol.Secret.DefaultMode)
	// 0o440 paired with fsGroup so the non-root distroless AGC user reads
	// credentials via group ownership; 0o400 alone is unreadable for non-root.
	assert.Equal(t, int32(0o440), *credVol.Secret.DefaultMode, "credentials must be mounted group-readable (0440)")
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext, "PodSpec must set fsGroup")
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(65532), *dep.Spec.Template.Spec.SecurityContext.FSGroup)

	// VolumeMount must be present on the AGC container.
	var credMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == agcCredsVolumeName {
			credMount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, credMount, "github-app-credentials VolumeMount must be present")
	assert.Equal(t, agcCredsMountPath, credMount.MountPath)
	assert.True(t, credMount.ReadOnly, "credential mount must be read-only")
}

func TestBuildAGCDeployment_SecurityContext(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot)
	require.NotNil(t, sc.ReadOnlyRootFilesystem)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Contains(t, sc.Capabilities.Drop, corev1.Capability("ALL"))
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

func TestBuildAGCDeployment_ProxyEnv(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	proxyAddr := "http://actions-gateway-proxy.team-a.svc.cluster.local:8080"
	dep := buildAGCDeployment(ag, "agc:latest", proxyAddr, nil)
	env := envMap(dep.Spec.Template.Spec.Containers[0].Env)

	assert.Equal(t, proxyAddr, env["HTTP_PROXY"].Value)
	assert.Equal(t, proxyAddr, env["HTTPS_PROXY"].Value)
	// NO_PROXY must contain cluster-internal entries even with no user CIDRs.
	noProxy := env["NO_PROXY"].Value
	for _, entry := range strings.Split(defaultNoProxy, ",") {
		assert.Contains(t, noProxy, entry, "NO_PROXY missing mandatory cluster-internal entry %q", entry)
	}
}

func TestBuildAGCDeployment_WorkerSA(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	env := envMap(dep.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, workerSAName, env["WORKER_SERVICE_ACCOUNT"].Value)
}

func TestBuildProxyDeployment_DefaultResources(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, resource.MustParse("10m"), c.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("32Mi"), c.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("100m"), c.Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("64Mi"), c.Resources.Limits[corev1.ResourceMemory])
}

func TestBuildProxyDeployment_CustomResources(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}
	dep := buildProxyDeployment(ag, "proxy:latest")
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, resource.MustParse("50m"), c.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("500m"), c.Resources.Limits[corev1.ResourceCPU])
	// Memory defaults must survive a partial override (per-key merge, not replacement).
	assert.Equal(t, resource.MustParse("32Mi"), c.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("64Mi"), c.Resources.Limits[corev1.ResourceMemory])
}

func TestBuildProxyDeployment_LimitsOnlyPreservesCPURequest(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	dep := buildProxyDeployment(ag, "proxy:latest")
	c := dep.Spec.Template.Spec.Containers[0]
	// Default cpu request must be preserved — HPA needs it to compute utilization.
	assert.Equal(t, resource.MustParse("10m"), c.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("128Mi"), c.Resources.Limits[corev1.ResourceMemory])
}

func TestBuildProxyDeployment_FullOverrideWins(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	dep := buildProxyDeployment(ag, "proxy:latest")
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, resource.MustParse("200m"), c.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("256Mi"), c.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("1"), c.Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), c.Resources.Limits[corev1.ResourceMemory])
}

func TestBuildProxyDeployment_SecurityContext(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot)
	require.NotNil(t, sc.ReadOnlyRootFilesystem)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Contains(t, sc.Capabilities.Drop, corev1.Capability("ALL"))
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

func TestBuildProxyNetworkPolicy_GitHubEgress(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")
	_, cidr2, _ := net.ParseCIDR("192.30.252.0/22")
	cidrs := []net.IPNet{*cidr1, *cidr2}

	np := buildProxyNetworkPolicy(ag, cidrs)
	require.NotNil(t, np)
	assert.Equal(t, npProxyName, np.Name)
	assert.Equal(t, proxyAppName, np.Spec.PodSelector.MatchLabels["app"], "proxy NP must select proxy pods")

	// Find egress rule containing port 443 with GitHub CIDRs.
	found := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				found = true
				assert.Len(t, rule.To, 2)
			}
		}
	}
	assert.True(t, found, "expected egress rule for port 443 with GitHub CIDRs")
}

func TestBuildProxyNetworkPolicy_ManagedFalse(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.ManagedNetworkPolicy = ptr(false)
	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")

	np := buildProxyNetworkPolicy(ag, []net.IPNet{*cidr1})

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						assert.NotEqual(t, "140.82.112.0/20", peer.IPBlock.CIDR)
					}
				}
			}
		}
	}
}

func TestBuildProxyNetworkPolicy_IngressFromWorkloadOnly(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildProxyNetworkPolicy(ag, nil)

	require.Len(t, np.Spec.Ingress, 1)
	rule := np.Spec.Ingress[0]
	require.Len(t, rule.From, 1)
	peer := rule.From[0]
	require.NotNil(t, peer.PodSelector)
	assert.Equal(t, componentWorkload, peer.PodSelector.MatchLabels[labelComponent],
		"proxy ingress must only allow workload-labeled pods")
	assert.Nil(t, peer.IPBlock, "IPBlock must be nil — ingress must not allow arbitrary IPs")
	// Must restrict to proxyPort only.
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, proxyPort, rule.Ports[0].Port.IntVal)
}

func TestBuildRole_AGCPermissions(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	role := buildAGCRole(ag)
	require.NotEmpty(t, role.Rules)

	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			assert.NotEqual(t, "*", verb, "wildcard verb found in rule: %v", rule)
		}
	}
}

// TestBuildRole_SecretsVerbs_IncludesListWatch verifies the AGC's Role has the
// list/watch verbs the agent pool needs to enumerate per-runner Secrets. An
// earlier revision removed these to "tighten" RBAC, which broke
// agentpool.Pool.listSecrets() and stalled session registration in e2e.
//
// The "don't hold Secret bodies in the controller cache" property that
// removing list/watch was trying to provide is now enforced separately at the
// AGC's manager via Client.Cache.DisableFor[*corev1.Secret] (in cmd/agc/main.go) —
// Secret reads go direct to the API server instead of being held in the
// controller-runtime cache.
func TestBuildRole_SecretsVerbs_IncludesListWatch(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	role := buildAGCRole(ag)

	var verbs []string
	for _, rule := range role.Rules {
		isSecrets := false
		for _, r := range rule.Resources {
			if r == "secrets" {
				isSecrets = true
			}
		}
		if !isSecrets {
			continue
		}
		verbs = append(verbs, rule.Verbs...)
	}
	require.NotEmpty(t, verbs, "AGC Role must include a secrets rule")
	for _, expected := range []string{"get", "list", "watch", "create", "delete"} {
		assert.Contains(t, verbs, expected, "AGC secrets rule must include %q", expected)
	}
}

func TestBuildHPA_MinMaxReplicas(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.MinReplicas = ptr(int32(3))
	ag.Spec.Proxy.MaxReplicas = ptr(int32(15))

	hpa := buildHPA(ag)
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(3), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(15), hpa.Spec.MaxReplicas)
}

// §1 — buildNoProxy merge tests

func TestBuildNoProxy_DefaultWhenEmpty(t *testing.T) {
	assert.Equal(t, defaultNoProxy, buildNoProxy(nil))
}

func TestBuildNoProxy_UserCIDRsPrependedToDefaults(t *testing.T) {
	result := buildNoProxy([]string{"192.168.0.0/16"})
	assert.True(t, strings.HasPrefix(result, "192.168.0.0/16,"), "user CIDR should be first")
	for _, entry := range strings.Split(defaultNoProxy, ",") {
		assert.Contains(t, result, entry)
	}
}

func TestBuildNoProxy_AlwaysContainsClusterLocal(t *testing.T) {
	result := buildNoProxy([]string{"10.0.0.0/8"})
	// svc.cluster.local covers all Kubernetes Services, including
	// kubernetes.default.svc.cluster.local (the kube-apiserver).
	assert.Contains(t, result, "svc.cluster.local")
}

func TestBuildAGCDeployment_NoProxyContainsDefaults(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.NoProxyCIDRs = []string{"172.16.0.0/12"}
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	env := envMap(dep.Spec.Template.Spec.Containers[0].Env)
	noProxy := env["NO_PROXY"].Value
	assert.Contains(t, noProxy, "172.16.0.0/12")
	for _, entry := range strings.Split(defaultNoProxy, ",") {
		assert.Contains(t, noProxy, entry)
	}
}

// §2 — buildWorkloadNetworkPolicy egress and buildProxyNetworkPolicy DNS

func TestBuildWorkloadNetworkPolicy_EgressToProxy(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildWorkloadNetworkPolicy(ag, "10.96.0.1")
	assert.Equal(t, npWorkloadName, np.Name)
	assert.Equal(t, componentWorkload, np.Spec.PodSelector.MatchLabels[labelComponent],
		"workload NP must select workload-labeled pods")

	found := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == proxyPort {
				for _, peer := range rule.To {
					if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.96.0.1/32" {
						found = true
					}
				}
			}
		}
	}
	assert.True(t, found, "expected workload egress rule to proxy ClusterIP on port 8080")
}

func TestBuildWorkloadNetworkPolicy_NoGitHubEgress(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildWorkloadNetworkPolicy(ag, "10.96.0.1")

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				t.Errorf("workload NP must not allow direct egress on port 443")
			}
		}
	}
}

func TestBuildWorkloadNetworkPolicy_NoProxyEgressWhenClusterIPEmpty(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildWorkloadNetworkPolicy(ag, "")

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == proxyPort {
				t.Errorf("unexpected egress rule on proxy port when clusterIP is empty")
			}
		}
	}
}

func TestBuildNetworkPolicy_DNSEgressAlwaysPresent(t *testing.T) {
	for _, managed := range []bool{true, false} {
		ag := newTestAG("gateway", "team-a")
		ag.Spec.Proxy.ManagedNetworkPolicy = ptr(managed)

		for _, np := range []*networkingv1.NetworkPolicy{
			buildProxyNetworkPolicy(ag, nil),
			buildWorkloadNetworkPolicy(ag, ""),
			buildAGCNetworkPolicy(ag),
		} {
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

func TestBuildAGCNetworkPolicy_PodSelectorIsAGCOnly(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildAGCNetworkPolicy(ag)

	assert.Equal(t, npAGCName, np.Name)
	// Must select AGC pods by app label, not the broad workload label.
	assert.Equal(t, agcAppName, np.Spec.PodSelector.MatchLabels["app"],
		"AGC NP must select AGC pods by app label")
	_, hasWorkloadLabel := np.Spec.PodSelector.MatchLabels[labelComponent]
	assert.False(t, hasWorkloadLabel, "AGC NP must not use the broad workload label as selector")
}

func TestBuildAGCNetworkPolicy_KubernetesAPIEgressAllowed(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildAGCNetworkPolicy(ag)

	found := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				found = true
				// No destination restriction — k8s API server IP is not known at deploy time.
				assert.Empty(t, rule.To, "port-443 rule must allow egress to any destination for k8s API access")
			}
		}
	}
	assert.True(t, found, "AGC NP must include egress rule for port 443 (Kubernetes API server)")
}

func TestBuildAGCNetworkPolicy_NoDirectGitHubEgressByItself(t *testing.T) {
	// Verify the AGC NetworkPolicy allows port 443 (k8s API) but that this is distinct
	// from the proxy-only restriction that buildWorkloadNetworkPolicy applies to workers.
	// Workers lack the `app: actions-gateway-agc` selector, so this policy doesn't apply to them.
	ag := newTestAG("gateway", "team-a")
	np := buildAGCNetworkPolicy(ag)

	// The AGC NP selector must NOT match worker pods (which have labelComponent: workload
	// but not app: agcAppName). This is a structural check — only app: agcAppName is selected.
	_, hasComponentLabel := np.Spec.PodSelector.MatchLabels[labelComponent]
	assert.False(t, hasComponentLabel,
		"AGC NP pod selector must not include the workload label — it would broaden scope to workers")
}

// §4 — buildProxyServiceAddr format

func TestBuildProxyServiceAddr_Format(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	addr := buildProxyServiceAddr(ag)
	assert.Equal(t, "https://actions-gateway-proxy.team-a.svc.cluster.local:8080", addr)
}

// §5 — untested resource constructors

func TestBuildAGCRoleBinding_WiresCorrectSA(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	rb := buildAGCRoleBinding(ag)
	assert.Equal(t, "Role", rb.RoleRef.Kind)
	assert.Equal(t, agcSAName, rb.RoleRef.Name)
	require.Len(t, rb.Subjects, 1)
	assert.Equal(t, agcSAName, rb.Subjects[0].Name)
	assert.Equal(t, ag.Namespace, rb.Subjects[0].Namespace)
}

func TestBuildProxyService_PortAndSelector(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	svc := buildProxyService(ag)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, proxyPort, svc.Spec.Ports[0].Port)
	assert.Equal(t, corev1.ProtocolTCP, svc.Spec.Ports[0].Protocol)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, proxyAppName, svc.Spec.Selector["app"])
}

func TestBuildRunnerGroup_SetsSpecAndLabels(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"self-hosted"}, MaxListeners: 5}
	rg := buildRunnerGroup(ag, spec, "rg-1")
	assert.Equal(t, spec, rg.Spec)
	assert.Equal(t, "rg-1", rg.Name)
	assert.Equal(t, ag.Namespace, rg.Namespace)
	assert.NotEmpty(t, rg.Labels[labelManagedBy])
}

func TestBuildResourceQuota_PassesThrough(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.NamespaceQuota = corev1.ResourceList{
		corev1.ResourcePods: resource.MustParse("50"),
	}
	quota := buildResourceQuota(ag)
	assert.Equal(t, ag.Spec.NamespaceQuota, quota.Spec.Hard)
}

func TestBuildPDB_MinAvailableAndSelector(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	pdb := buildPDB(ag)
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, int32(1), pdb.Spec.MinAvailable.IntVal)
	require.NotNil(t, pdb.Spec.Selector)
	assert.Equal(t, proxyAppName, pdb.Spec.Selector.MatchLabels["app"])
}

func TestBuildHPA_DefaultCPUTarget(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	hpa := buildHPA(ag)
	require.Len(t, hpa.Spec.Metrics, 1)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(60), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestBuildHPA_CustomCPUTarget(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.TargetCPUUtilizationPercentage = ptr(int32(80))
	hpa := buildHPA(ag)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(80), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
}

func TestBuildHPA_ScaleTargetRef(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	hpa := buildHPA(ag)
	ref := hpa.Spec.ScaleTargetRef
	assert.Equal(t, "apps/v1", ref.APIVersion)
	assert.Equal(t, "Deployment", ref.Kind)
	assert.Equal(t, proxyServiceName, ref.Name)
}

func TestBuildHPA_MetricTypeAndResourceName(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	hpa := buildHPA(ag)
	require.Len(t, hpa.Spec.Metrics, 1)
	m := hpa.Spec.Metrics[0]
	assert.Equal(t, autoscalingv2.ResourceMetricSourceType, m.Type)
	require.NotNil(t, m.Resource)
	assert.Equal(t, corev1.ResourceCPU, m.Resource.Name)
	assert.Equal(t, autoscalingv2.UtilizationMetricType, m.Resource.Target.Type)
}

func TestBuildHPA_DefaultMinMaxReplicas(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	hpa := buildHPA(ag)
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(10), hpa.Spec.MaxReplicas)
}

func TestBuildProxyDeployment_InitialReplicas(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(2), *dep.Spec.Replicas)

	ag.Spec.Proxy.MinReplicas = ptr(int32(1))
	dep = buildProxyDeployment(ag, "proxy:latest")
	assert.Equal(t, int32(1), *dep.Spec.Replicas)
}

func TestBuildProxyDeployment_AntiAffinity(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")
	require.NotNil(t, dep.Spec.Template.Spec.Affinity)
	aa := dep.Spec.Template.Spec.Affinity.PodAntiAffinity
	require.NotNil(t, aa)
	require.Len(t, aa.PreferredDuringSchedulingIgnoredDuringExecution, 1)
	term := aa.PreferredDuringSchedulingIgnoredDuringExecution[0]
	assert.Equal(t, int32(100), term.Weight)
	assert.Equal(t, "kubernetes.io/hostname", term.PodAffinityTerm.TopologyKey)
}

func TestBuildProxyDeployment_Probes(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")
	c := dep.Spec.Template.Spec.Containers[0]
	require.NotNil(t, c.LivenessProbe)
	require.NotNil(t, c.LivenessProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, proxyHealthPort, c.LivenessProbe.HTTPGet.Port.IntVal)
	require.NotNil(t, c.ReadinessProbe)
	require.NotNil(t, c.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, proxyHealthPort, c.ReadinessProbe.HTTPGet.Port.IntVal)
}

func TestManagedLabels_ContainsOwnerRef(t *testing.T) {
	ag := newTestAG("my-gateway", "my-ns")
	labels := managedLabels(ag)
	assert.Equal(t, "my-gateway", labels["actions-gateway/owner-name"])
	assert.Equal(t, "my-ns", labels["actions-gateway/owner-ns"])
}

// §W7 — proxy TLS cert generation, secret, and deployment mounts

func TestGenerateProxyCert_SANsAndValidity(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	certPEM, keyPEM, err := generateProxyCert(ag)
	require.NoError(t, err)
	require.NotEmpty(t, certPEM)
	require.NotEmpty(t, keyPEM)

	cert, err := parseCertPEM(certPEM)
	require.NoError(t, err)

	// FQDN SAN must be present.
	assert.Contains(t, cert.DNSNames, "actions-gateway-proxy.team-a.svc.cluster.local",
		"cert must include the fully-qualified proxy Service DNS name as SAN")
	// Short names must also be SANs so in-namespace lookups work.
	assert.Contains(t, cert.DNSNames, "actions-gateway-proxy")

	// Validity must be approximately 1 year.
	remaining := cert.NotAfter.Sub(cert.NotBefore)
	assert.GreaterOrEqual(t, remaining, 364*24*time.Hour, "cert must be valid for at least 364 days")
	assert.Less(t, remaining, 366*24*time.Hour, "cert must not be valid for more than 366 days")
}

func TestBuildProxyCertSecret(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	certPEM, keyPEM, err := generateProxyCert(ag)
	require.NoError(t, err)

	s := buildProxyCertSecret(ag, certPEM, keyPEM)
	assert.Equal(t, proxyTLSSecretName, s.Name)
	assert.Equal(t, ag.Namespace, s.Namespace)
	assert.Equal(t, corev1.SecretTypeTLS, s.Type)
	assert.Equal(t, certPEM, s.Data[corev1.TLSCertKey])
	assert.Equal(t, keyPEM, s.Data[corev1.TLSPrivateKeyKey])
}

func TestBuildProxyDeployment_TLSMount(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildProxyDeployment(ag, "proxy:latest")

	// TLS volume must reference the proxy TLS Secret.
	var tlsVol *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		if dep.Spec.Template.Spec.Volumes[i].Name == proxyTLSVolumeName {
			tlsVol = &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	require.NotNil(t, tlsVol, "proxy TLS volume must be present")
	require.NotNil(t, tlsVol.Secret, "proxy TLS volume source must be a Secret")
	assert.Equal(t, proxyTLSSecretName, tlsVol.Secret.SecretName)
	require.NotNil(t, tlsVol.Secret.DefaultMode)
	// 0o440 (group readable) plus fsGroup 65532 on the PodSpec so the
	// distroless nonroot container reads the cert via group ownership.
	// 0o400 alone left files as root:root and the non-root user couldn't
	// open them — the proxy crashed at startup with "permission denied".
	assert.Equal(t, int32(0o440), *tlsVol.Secret.DefaultMode)
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext, "PodSpec must set fsGroup")
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(65532), *dep.Spec.Template.Spec.SecurityContext.FSGroup,
		"fsGroup must match the distroless nonroot GID so kubelet chgrp's the mount")

	c := dep.Spec.Template.Spec.Containers[0]
	var tlsMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == proxyTLSVolumeName {
			tlsMount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, tlsMount, "proxy TLS VolumeMount must be present")
	assert.Equal(t, proxyTLSMountPath, tlsMount.MountPath)
	assert.True(t, tlsMount.ReadOnly)

	// Env vars must point to the mounted cert and key files.
	env := envMap(c.Env)
	assert.Equal(t, proxyTLSMountPath+"/"+corev1.TLSCertKey, env["PROXY_TLS_CERT_FILE"].Value)
	assert.Equal(t, proxyTLSMountPath+"/"+corev1.TLSPrivateKeyKey, env["PROXY_TLS_KEY_FILE"].Value)
}

func TestBuildAGCDeployment_NilExtraEnvNoLeaks(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		assert.False(t, strings.HasPrefix(e.Name, "AGC_EXTRA_"),
			"nil extraEnv must not produce any AGC_EXTRA_* env var; got %q", e.Name)
	}
}

func TestBuildAGCDeployment_ProxyCACertMount(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "https://proxy:8080", nil)

	// CA cert volume must project only tls.crt from the proxy TLS Secret —
	// the private key must NOT reach the AGC container.
	var caVol *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		if dep.Spec.Template.Spec.Volumes[i].Name == proxyCACertVolumeName {
			caVol = &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	require.NotNil(t, caVol, "proxy CA cert volume must be present on AGC Deployment")
	require.NotNil(t, caVol.Secret, "CA cert volume source must be a Secret")
	assert.Equal(t, proxyTLSSecretName, caVol.Secret.SecretName)

	// Items projection must expose only tls.crt — never tls.key.
	require.Len(t, caVol.Secret.Items, 1, "only tls.crt must be projected into the AGC — not tls.key")
	assert.Equal(t, corev1.TLSCertKey, caVol.Secret.Items[0].Key)
	assert.Equal(t, corev1.TLSCertKey, caVol.Secret.Items[0].Path)

	c := dep.Spec.Template.Spec.Containers[0]
	var caMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == proxyCACertVolumeName {
			caMount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, caMount, "proxy CA cert VolumeMount must be present")
	assert.Equal(t, proxyCACertMountPath, caMount.MountPath)
	assert.True(t, caMount.ReadOnly)
}
