//go:build integration

package integration_test

import (
	"net"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests exercise the v2 EgressProxy reconciler (Q163, M2) end-to-end against
// the real apiserver: a standalone EgressProxy is reconciled into a proxy pool it
// owns (Deployment/Service/HPA/PDB/NetworkPolicy + self-signed proxy TLS Secret),
// every child carries a controller owner reference for cascade GC, defaulting flows
// into the children, and the uniform status contract is surfaced. envtest runs no
// kubelet, so proxy pods never become ready — readyReplicas stays 0 and Ready is
// reported False with reason ProxyNotReady, which is the correct observation.

const egressProxyName = "shared"

func proxyChildName(ep string) string { return ep + "-proxy" }
func proxyTLSName(ep string) string   { return ep + "-proxy-tls" }
func proxyIdentityLabel() string      { return "actions-gateway.com/egress-proxy" }

// hasControllerOwnerRef reports whether refs contains a controller owner reference
// to an EgressProxy named epName — the mechanism that drives cascade GC in a real
// cluster (envtest runs no GC controller, so the owner reference itself is what we
// assert).
func hasControllerOwnerRef(refs []metav1.OwnerReference, epName string) bool {
	for _, r := range refs {
		if r.Kind == "EgressProxy" && r.Name == epName && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func TestV2_EgressProxy_ReconcilesOwnedProxyPool(t *testing.T) {
	const ns = "v2-ep-reconcile"
	createNamespace(t, ns)

	// Seed GitHub CIDRs so the proxy NetworkPolicy gets its egress allowlist on the
	// first reconcile (mirrors steady-state where the IP fetch has already run).
	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconciler(t, ipCache)

	minR := int32(3)
	maxR := int32(7)
	targetCPU := int32(55)
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			MinReplicas:                    &minR,
			MaxReplicas:                    &maxR,
			TargetCPUUtilizationPercentage: &targetCPU,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName(egressProxyName)

	// Wait for the NetworkPolicy — the last child applied in a reconcile pass — so a
	// successful Get here guarantees every earlier child (cert Secret, Deployment,
	// Service, HPA, PDB) already exists, avoiding a mid-reconcile read race.
	var np networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &np) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy NetworkPolicy should be created")

	// Deployment: created, owned, replicas == minReplicas, identity label, TLS mount.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep))
	assert.True(t, hasControllerOwnerRef(dep.OwnerReferences, egressProxyName), "Deployment must be owned by the EgressProxy")
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(3), *dep.Spec.Replicas, "replicas should track minReplicas")
	assert.Equal(t, egressProxyName, dep.Spec.Template.Labels[proxyIdentityLabel()], "pod template carries the egress-proxy identity label")
	assert.Equal(t, egressProxyName, dep.Spec.Selector.MatchLabels[proxyIdentityLabel()], "selector keys on the egress-proxy identity")
	// Q205: recommended app.kubernetes.io/* metadata on the Deployment and its pods,
	// additive to the functional identity selector above.
	assert.Equal(t, "actions-gateway-proxy", dep.Labels[apilabels.Name])
	assert.Equal(t, egressProxyName, dep.Labels[apilabels.Instance])
	assert.Equal(t, "proxy", dep.Labels[apilabels.Component])
	assert.Equal(t, apilabels.PartOfValue, dep.Labels[apilabels.PartOf])
	assert.Equal(t, "actions-gateway-gmc", dep.Labels[apilabels.ManagedBy])
	assert.Equal(t, "proxy", dep.Spec.Template.Labels[apilabels.Component], "pods carry the recommended labels too")

	// Service: created, owned, identity selector, proxy port.
	var svc corev1.Service
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc))
	assert.True(t, hasControllerOwnerRef(svc.OwnerReferences, egressProxyName))
	assert.Equal(t, egressProxyName, svc.Spec.Selector[proxyIdentityLabel()])

	// HPA: created, owned, min/max/targetCPU reflect the spec, scaleTargetRef → Deployment.
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &hpa))
	assert.True(t, hasControllerOwnerRef(hpa.OwnerReferences, egressProxyName))
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(3), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(7), hpa.Spec.MaxReplicas)
	assert.Equal(t, name, hpa.Spec.ScaleTargetRef.Name)
	require.Len(t, hpa.Spec.Metrics, 1)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(55), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)

	// PDB: created, owned, minAvailable 1.
	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pdb))
	assert.True(t, hasControllerOwnerRef(pdb.OwnerReferences, egressProxyName))
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, int32(1), pdb.Spec.MinAvailable.IntVal)

	// NetworkPolicy: owned, GitHub CIDR egress present (secure lockdown).
	assert.True(t, hasControllerOwnerRef(np.OwnerReferences, egressProxyName))
	foundGitHubEgress := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
				foundGitHubEgress = true
			}
		}
	}
	assert.True(t, foundGitHubEgress, "proxy NetworkPolicy must restrict egress to the seeded GitHub CIDR")

	// Proxy TLS Secret: created, owned, TLS type with cert+key.
	var sec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyTLSName(egressProxyName)}, &sec))
	assert.True(t, hasControllerOwnerRef(sec.OwnerReferences, egressProxyName))
	assert.Equal(t, corev1.SecretTypeTLS, sec.Type)
	assert.NotEmpty(t, sec.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, sec.Data[corev1.TLSPrivateKeyKey])

	// Status: uniform contract. No kubelet ⇒ 0 ready pods ⇒ Ready False / ProxyNotReady.
	require.Eventually(t, func() bool {
		var got gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyName}, &got); err != nil {
			return false
		}
		readyCond := findCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
		degradedCond := findCondition(got.Status.Conditions, gmcv2alpha1.ConditionDegraded)
		return readyCond != nil && readyCond.Status == metav1.ConditionFalse &&
			readyCond.Reason == gmcv2alpha1.ReasonProxyNotReady &&
			degradedCond != nil && degradedCond.Status == metav1.ConditionFalse &&
			got.Status.ObservedGeneration == got.Generation
	}, 10*time.Second, 100*time.Millisecond, "EgressProxy status should surface Ready=False/ProxyNotReady, Degraded=False, observedGeneration set")
}

// TestV2_EgressProxy_DefaultsFlowIntoChildren proves that an EgressProxy created
// with an empty spec (apiserver applies the CRD defaults) yields a proxy pool sized
// by those defaults: 2 replicas, max 10, target CPU 60.
func TestV2_EgressProxy_DefaultsFlowIntoChildren(t *testing.T) {
	const ns = "v2-ep-defaults"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName("defaulted")
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &hpa) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy HPA should be created")
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas, "default minReplicas")
	assert.Equal(t, int32(10), hpa.Spec.MaxReplicas, "default maxReplicas")
	require.Len(t, hpa.Spec.Metrics, 1)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(60), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization, "default targetCPU")
}

// TestV2_EgressProxy_ChildrenAdoptControllerOwnerRef double-checks the GC
// mechanism: deleting the proxy Deployment makes the reconciler recreate it, and
// the recreated object still carries the controller owner reference.
func TestV2_EgressProxy_RecreatesDeletedChild(t *testing.T) {
	const ns = "v2-ep-recreate"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "resilient", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName("resilient")
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond)

	require.NoError(t, k8sClient.Delete(ctx, &dep))
	require.Eventually(t, func() bool {
		var got appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			return false
		}
		return got.UID != dep.UID && hasControllerOwnerRef(got.OwnerReferences, "resilient")
	}, 10*time.Second, 200*time.Millisecond, "deleted proxy Deployment should be recreated with the owner reference intact")
}

// containerEnv returns the proxy container's env as a name→value map.
func containerEnv(t *testing.T, dep appsv1.Deployment) map[string]string {
	t.Helper()
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	out := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

// npHasCIDRPeer reports whether np carries a port-443 ipBlock egress rule for the
// exact CIDR (used to assert an operator destinationCIDRs entry lands as a peer).
func npHasCIDRPeer(np networkingv1.NetworkPolicy, cidr string) bool {
	for _, rule := range np.Spec.Egress {
		on443 := false
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				on443 = true
			}
		}
		if !on443 {
			continue
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				return true
			}
		}
	}
	return false
}

// TestV2_EgressProxy_DestinationCIDRsInjected proves the Q242 G.1 deliverable-3
// plumbing in the default CIDR mode: an EgressProxy that lists destinationCIDRs gets
// (1) the proxy CONNECT allowlist env injected — host suffixes carry the implicit
// GitHub hostnames and PROXY_ALLOWED_CIDRS carries the operator's range — and (2) the
// standard NetworkPolicy carries the range as a 443 ipBlock egress peer.
func TestV2_EgressProxy_DestinationCIDRsInjected(t *testing.T) {
	const ns = "v2-ep-dest-cidrs"
	createNamespace(t, ns)

	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconciler(t, ipCache)

	const destCIDR = "10.20.0.0/16"
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{destCIDR}},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName(egressProxyName)
	var np networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &np) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy NetworkPolicy should be created")

	// (1) Deployment env: allowlist injected, GitHub by hostname + operator CIDR.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep))
	env := containerEnv(t, dep)
	require.Contains(t, env, "PROXY_ALLOWED_HOST_SUFFIXES", "opting in must inject the host-suffix allowlist")
	assert.Contains(t, env["PROXY_ALLOWED_HOST_SUFFIXES"], "api.github.com", "GitHub stays reachable by hostname")
	assert.Equal(t, destCIDR, env["PROXY_ALLOWED_CIDRS"], "operator destinationCIDRs flow to PROXY_ALLOWED_CIDRS")

	// (2) Standard NetworkPolicy: the operator CIDR is a 443 ipBlock peer, alongside
	// the seeded GitHub CIDR rule.
	assert.True(t, npHasCIDRPeer(np, destCIDR), "destinationCIDRs must become an ipBlock egress peer")
	assert.True(t, npHasCIDRPeer(np, "140.82.112.0/20"), "GitHub CIDR egress must remain")
}

// TestV2_EgressProxy_NoDestinationsTransportOnly proves the secure-by-default,
// backward-compatible path: an EgressProxy with no extra destinations injects NO
// CONNECT allowlist env, so the proxy stays transport-only and the NetworkPolicy is
// the sole egress gate (byte-for-byte the pre-G.1 behavior).
func TestV2_EgressProxy_NoDestinationsTransportOnly(t *testing.T) {
	const ns = "v2-ep-transport-only"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName(egressProxyName)
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy Deployment should be created")

	env := containerEnv(t, dep)
	assert.NotContains(t, env, "PROXY_ALLOWED_HOST_SUFFIXES", "no destinations ⇒ no host-suffix allowlist (transport-only)")
	assert.NotContains(t, env, "PROXY_ALLOWED_CIDRS", "no destinations ⇒ no CIDR allowlist (transport-only)")
}

// findCondition returns the named condition or nil.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
