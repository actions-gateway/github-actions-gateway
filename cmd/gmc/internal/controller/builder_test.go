/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

func TestBuildAGCDeployment_SecretRefs(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080", nil)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	env := envMap(dep.Spec.Template.Spec.Containers[0].Env)

	for _, key := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_INSTALLATION_ID"} {
		e, ok := env[key]
		require.True(t, ok, "missing env %s", key)
		require.NotNil(t, e.ValueFrom)
		require.NotNil(t, e.ValueFrom.SecretKeyRef)
		assert.Equal(t, "github-app", e.ValueFrom.SecretKeyRef.Name)
	}
	assert.Equal(t, "appId", env["GITHUB_APP_ID"].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, "privateKey", env["GITHUB_APP_PRIVATE_KEY"].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, "installationId", env["GITHUB_APP_INSTALLATION_ID"].ValueFrom.SecretKeyRef.Key)
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
}

func TestBuildNetworkPolicy_ProxyEgress(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")
	_, cidr2, _ := net.ParseCIDR("192.30.252.0/22")
	cidrs := []net.IPNet{*cidr1, *cidr2}

	np := buildNetworkPolicy(ag, "10.96.0.1", cidrs)
	require.NotNil(t, np)

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

func TestBuildNetworkPolicy_ManagedFalse(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	ag.Spec.Proxy.ManagedNetworkPolicy = ptr(false)
	_, cidr1, _ := net.ParseCIDR("140.82.112.0/20")

	np := buildNetworkPolicy(ag, "", []net.IPNet{*cidr1})

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				// Should not have GitHub CIDR peers when managedNetworkPolicy is false.
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						assert.NotEqual(t, "140.82.112.0/20", peer.IPBlock.CIDR)
					}
				}
			}
		}
	}
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

func TestBuildNoProxy_AlwaysContainsKubeAPIServer(t *testing.T) {
	result := buildNoProxy([]string{"10.0.0.0/8"})
	assert.Contains(t, result, "kubernetes.default.svc.cluster.local")
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

// §2 — buildNetworkPolicy DNS and AGC/worker egress

func TestBuildNetworkPolicy_AGCWorkerEgressToProxy(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildNetworkPolicy(ag, "10.96.0.1", nil)

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
	assert.True(t, found, "expected AGC/worker egress rule to proxy ClusterIP on port 8080")
}

func TestBuildNetworkPolicy_DNSEgressAlwaysPresent(t *testing.T) {
	for _, managed := range []bool{true, false} {
		ag := newTestAG("gateway", "team-a")
		ag.Spec.Proxy.ManagedNetworkPolicy = ptr(managed)
		np := buildNetworkPolicy(ag, "", nil)

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
		assert.True(t, udpFound, "expected DNS UDP egress when managedNetworkPolicy=%v", managed)
		assert.True(t, tcpFound, "expected DNS TCP egress when managedNetworkPolicy=%v", managed)
	}
}

func TestBuildNetworkPolicy_NoProxyEgressWhenClusterIPEmpty(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildNetworkPolicy(ag, "", nil)

	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == proxyPort {
				t.Errorf("unexpected egress rule on proxy port when clusterIP is empty")
			}
		}
	}
}

func TestBuildNetworkPolicy_IngressFromNamespaceOnly(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	np := buildNetworkPolicy(ag, "", nil)

	// Exactly one ingress rule with a single empty-PodSelector peer
	// (allows all pods in the same namespace, denies external traffic).
	require.Len(t, np.Spec.Ingress, 1)
	rule := np.Spec.Ingress[0]
	require.Len(t, rule.From, 1)
	peer := rule.From[0]
	require.NotNil(t, peer.PodSelector, "expected PodSelector peer for namespace-scoped ingress")
	assert.Empty(t, peer.PodSelector.MatchLabels, "empty MatchLabels = all pods in namespace")
	assert.Nil(t, peer.IPBlock, "IPBlock must be nil — ingress must not allow arbitrary IPs")
}

// §4 — buildProxyServiceAddr format

func TestBuildProxyServiceAddr_Format(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	addr := buildProxyServiceAddr(ag)
	assert.Equal(t, "http://actions-gateway-proxy.team-a.svc.cluster.local:8080", addr)
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
