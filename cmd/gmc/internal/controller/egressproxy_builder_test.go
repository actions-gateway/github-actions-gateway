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

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func newEP(name, ns string, mut func(*gmcv2alpha1.EgressProxy)) *gmcv2alpha1.EgressProxy {
	ep := &gmcv2alpha1.EgressProxy{}
	ep.Name = name
	ep.Namespace = ns
	if mut != nil {
		mut(ep)
	}
	return ep
}

func TestEgressProxyDerivedNames(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	assert.Equal(t, "shared-proxy", proxyResourceName(ep))
	assert.Equal(t, "shared-proxy-tls", egressProxyTLSSecretName(ep))
}

func TestEgressProxyLabelsAndSelector(t *testing.T) {
	ep := newEP("shared", "team-a", nil)

	labels := egressProxyLabels(ep)
	assert.Equal(t, labelManagerValue, labels[labelManagedBy])
	assert.Equal(t, "shared", labels[egressProxyComponentLabel])

	sel := egressProxyPodSelector(ep)
	assert.Equal(t, proxyAppName, sel["app"])
	assert.Equal(t, "shared", sel[egressProxyComponentLabel], "selector must carry the per-EgressProxy identity")
}

func TestEgressProxyScalarDefaultsAndOverrides(t *testing.T) {
	// Defaults when spec is empty (a hand-built object that skipped apiserver defaulting).
	def := newEP("shared", "team-a", nil)
	assert.Equal(t, int32(2), egressProxyMinReplicas(def))
	assert.Equal(t, int32(10), egressProxyMaxReplicas(def))
	assert.Equal(t, int32(60), egressProxyTargetCPU(def))

	// Overrides win.
	over := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
		ep.Spec.MaxReplicas = ptr(int32(7))
		ep.Spec.TargetCPUUtilizationPercentage = ptr(int32(55))
	})
	assert.Equal(t, int32(3), egressProxyMinReplicas(over))
	assert.Equal(t, int32(7), egressProxyMaxReplicas(over))
	assert.Equal(t, int32(55), egressProxyTargetCPU(over))
}

func TestEgressProxyResourcesMergeOverDefaults(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")},
		}
	})
	res := egressProxyResources(ep)
	// Override applied.
	assert.Equal(t, "25m", res.Requests.Cpu().String())
	// Defaults preserved for the keys the override did not set.
	assert.Equal(t, "32Mi", res.Requests.Memory().String())
	assert.Equal(t, "500m", res.Limits.Cpu().String())
	assert.Equal(t, "64Mi", res.Limits.Memory().String())
}

func TestBuildEgressProxyDeployment(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test")

	assert.Equal(t, "shared-proxy", dep.Name)
	assert.Equal(t, "team-a", dep.Namespace)
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(3), *dep.Spec.Replicas)
	assert.Equal(t, "shared", dep.Spec.Selector.MatchLabels[egressProxyComponentLabel])
	assert.Equal(t, "shared", dep.Spec.Template.Labels[egressProxyComponentLabel])

	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "proxy:test", c.Image)
	// Hardened container + pod security contexts.
	require.NotNil(t, c.SecurityContext)
	require.NotNil(t, c.SecurityContext.RunAsNonRoot)
	assert.True(t, *c.SecurityContext.RunAsNonRoot)
	require.NotNil(t, c.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext)

	// Proxy TLS cert mounted; no metrics-mTLS volume in M2.
	require.Len(t, dep.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, proxyTLSVolumeName, dep.Spec.Template.Spec.Volumes[0].Name)
	assert.Equal(t, "shared-proxy-tls", dep.Spec.Template.Spec.Volumes[0].Secret.SecretName)

	// Required anti-affinity keyed on the pool identity.
	require.NotNil(t, dep.Spec.Template.Spec.Affinity)
	require.NotNil(t, dep.Spec.Template.Spec.Affinity.PodAntiAffinity)
	terms := dep.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	assert.Equal(t, "shared", terms[0].LabelSelector.MatchLabels[egressProxyComponentLabel])

	// Proxy TLS env points at the mounted cert; no metrics env.
	envNames := map[string]string{}
	for _, e := range c.Env {
		envNames[e.Name] = e.Value
	}
	assert.Contains(t, envNames, "PROXY_TLS_CERT_FILE")
	assert.Contains(t, envNames, "PROXY_TLS_KEY_FILE")
	assert.NotContains(t, envNames, "PROXY_METRICS_TLS_CERT_FILE")
}

func TestBuildEgressProxyServiceHPAPDB(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
		ep.Spec.MaxReplicas = ptr(int32(7))
		ep.Spec.TargetCPUUtilizationPercentage = ptr(int32(55))
	})

	svc := buildEgressProxyService(ep)
	assert.Equal(t, "shared-proxy", svc.Name)
	assert.Equal(t, "shared", svc.Spec.Selector[egressProxyComponentLabel])
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, proxyPort, svc.Spec.Ports[0].Port)

	hpa := buildEgressProxyHPA(ep)
	assert.Equal(t, "shared-proxy", hpa.Name)
	assert.Equal(t, "shared-proxy", hpa.Spec.ScaleTargetRef.Name)
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(3), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(7), hpa.Spec.MaxReplicas)
	require.Len(t, hpa.Spec.Metrics, 1)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(55), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)

	pdb := buildEgressProxyPDB(ep)
	assert.Equal(t, "shared-proxy", pdb.Name)
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, int32(1), pdb.Spec.MinAvailable.IntVal)
	assert.Equal(t, "shared", pdb.Spec.Selector.MatchLabels[egressProxyComponentLabel])
}

func TestBuildEgressProxyNetworkPolicy(t *testing.T) {
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cidrs := []net.IPNet{*cidr}

	// Managed (default): DNS + GitHub CIDR egress, workload ingress.
	managed := newEP("shared", "team-a", nil)
	np := buildEgressProxyNetworkPolicy(managed, cidrs)
	assert.Equal(t, "shared-proxy", np.Name)
	assert.Equal(t, "shared", np.Spec.PodSelector.MatchLabels[egressProxyComponentLabel])
	foundGitHub := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
				foundGitHub = true
			}
		}
	}
	assert.True(t, foundGitHub, "managed policy must allow egress to the GitHub CIDR")
	require.Len(t, np.Spec.Ingress, 1)
	assert.Equal(t, componentWorkload, np.Spec.Ingress[0].From[0].PodSelector.MatchLabels[labelComponent])

	// managedNetworkPolicy=false: GitHub CIDR egress omitted (additive model), DNS kept.
	unmanaged := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.ManagedNetworkPolicy = ptr(false)
	})
	npU := buildEgressProxyNetworkPolicy(unmanaged, cidrs)
	for _, rule := range npU.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
				t.Fatal("unmanaged policy must not add the GitHub CIDR egress rule")
			}
		}
	}
	assert.NotEmpty(t, npU.Spec.Egress, "DNS egress is always present")
}

func TestBuildEgressProxyCertSecret(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	sec := buildEgressProxyCertSecret(ep, []byte("cert"), []byte("key"))
	assert.Equal(t, "shared-proxy-tls", sec.Name)
	assert.Equal(t, corev1.SecretTypeTLS, sec.Type)
	assert.Equal(t, []byte("cert"), sec.Data[corev1.TLSCertKey])
	assert.Equal(t, []byte("key"), sec.Data[corev1.TLSPrivateKeyKey])
	assert.Equal(t, "shared", sec.Labels[egressProxyComponentLabel])
}
