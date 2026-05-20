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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080")
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
	dep := buildAGCDeployment(ag, "agc:latest", proxyAddr)
	env := envMap(dep.Spec.Template.Spec.Containers[0].Env)

	assert.Equal(t, proxyAddr, env["HTTP_PROXY"].Value)
	assert.Equal(t, proxyAddr, env["HTTPS_PROXY"].Value)
	assert.NotEmpty(t, env["NO_PROXY"].Value)
}

func TestBuildAGCDeployment_WorkerSA(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	dep := buildAGCDeployment(ag, "agc:latest", "http://proxy:8080")
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
