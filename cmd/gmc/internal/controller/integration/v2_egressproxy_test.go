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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// TestV2_EgressProxy_LogLevelChange_RollsProxy verifies the per-pool verbosity
// knob (Q327, v1 parity): an EgressProxy with no spec.logLevel lands LOG_LEVEL=info
// on the proxy container (the apiserver applies the CRD default), and flipping it
// to debug updates the pod template — the rolling-restart path that makes the new
// level take effect. Mirrors the v1 TestGMC_LogLevelChange_RollsAGCAndProxy.
func TestV2_EgressProxy_LogLevelChange_RollsProxy(t *testing.T) {
	const ns = "v2-ep-loglevel"
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
	assert.Equal(t, "info", containerEnv(t, dep)["LOG_LEVEL"],
		"proxy container must start with LOG_LEVEL=info by default")

	// Flip spec.logLevel to debug (retry on conflict — the reconciler may still be
	// writing status on first reconcile).
	require.Eventually(t, func() bool {
		var fetched gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyName}, &fetched); err != nil {
			return false
		}
		fetched.Spec.LogLevel = "debug"
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update EgressProxy spec.logLevel to debug")

	require.Eventually(t, func() bool {
		var got appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			return false
		}
		return containerEnv(t, got)["LOG_LEVEL"] == "debug"
	}, 10*time.Second, 100*time.Millisecond,
		"proxy Deployment must roll to LOG_LEVEL=debug after spec.logLevel=debug")
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

// TestV2_EgressProxy_ProvisionsMetricsMTLS proves the EgressProxy reconciler wires the
// proxy metrics-mTLS stack end to end (Q324): a per-EgressProxy server bundle mounted
// into the proxy, a scraper client bundle published for monitoring, the metrics port
// on the Service, the PROXY_METRICS_* env on the container, and the metrics-scrape
// ingress rule on the NetworkPolicy — all at parity with the classic proxy, with no
// plaintext regression (the three env vars are what force the proxy binary onto the
// dedicated mTLS listener).
func TestV2_EgressProxy_ProvisionsMetricsMTLS(t *testing.T) {
	const ns = "v2-ep-metrics-mtls"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "metered", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	name := proxyChildName("metered")

	// Wait for the NetworkPolicy — the last child applied in a reconcile pass — so a
	// successful Get guarantees every earlier child (both metrics Secrets, the
	// Deployment, the Service) already exists, avoiding a mid-reconcile read race.
	var np networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &np) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy NetworkPolicy should be created")

	// Server bundle Secret: created, owned, TLS type, carries the metrics CA so the
	// proxy can verify scraper client certs.
	var serverSec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "metered-metrics-tls"}, &serverSec))
	assert.True(t, hasControllerOwnerRef(serverSec.OwnerReferences, "metered"))
	assert.Equal(t, corev1.SecretTypeTLS, serverSec.Type)
	assert.NotEmpty(t, serverSec.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, serverSec.Data[corev1.TLSPrivateKeyKey])
	assert.NotEmpty(t, serverSec.Data["ca.crt"], "server bundle must carry the metrics CA to verify scraper certs")

	// Scraper client bundle Secret: published for monitoring, owned, TLS type + CA.
	var clientSec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "metered-metrics-client"}, &clientSec))
	assert.True(t, hasControllerOwnerRef(clientSec.OwnerReferences, "metered"))
	assert.NotEmpty(t, clientSec.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, clientSec.Data["ca.crt"])

	// Deployment: metrics-mTLS volume + all three PROXY_METRICS_* files set (the mTLS
	// gate) + the metrics container port.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep))
	env := containerEnv(t, dep)
	assert.NotEmpty(t, env["PROXY_METRICS_TLS_CERT_FILE"])
	assert.NotEmpty(t, env["PROXY_METRICS_TLS_KEY_FILE"])
	assert.NotEmpty(t, env["PROXY_METRICS_CLIENT_CA_FILE"], "the client-CA file is what enables mTLS (not plaintext) metrics")
	assert.Equal(t, "8443", env["PROXY_METRICS_PORT"])
	metricsVolFound := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "metered-metrics-tls" {
			metricsVolFound = true
		}
	}
	assert.True(t, metricsVolFound, "proxy pod must mount the metrics server bundle")

	// Service: fronts the mTLS metrics port so a ServiceMonitor can target it by name.
	var svc corev1.Service
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc))
	metricsPortFound := false
	for _, p := range svc.Spec.Ports {
		if p.Name == "metrics" {
			metricsPortFound = true
			assert.Equal(t, int32(8443), p.Port)
		}
	}
	assert.True(t, metricsPortFound, "proxy Service must front the metrics port")

	// NetworkPolicy (already fetched above): monitoring-namespace scrape of the
	// metrics port is admitted.
	metricsIngressFound := false
	for _, rule := range np.Spec.Ingress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 8443 {
				metricsIngressFound = true
				require.NotEmpty(t, rule.From)
				assert.Equal(t, "enabled", rule.From[0].NamespaceSelector.MatchLabels["metrics"],
					"metrics scrape must be admitted only from monitoring namespaces")
			}
		}
	}
	assert.True(t, metricsIngressFound, "NetworkPolicy must admit the metrics-port scrape")
}

// TestV2_EgressProxy_ProvisionsServiceMonitor proves that with the tenant
// ServiceMonitor toggle on, the reconciler creates the per-EgressProxy ServiceMonitor
// (Q324) with the scrape TLS config Prometheus needs — owned for GC, targeting the
// metrics port over HTTPS, presenting the scraper client bundle and pinning serverName
// to a SAN on the metrics server cert.
func TestV2_EgressProxy_ProvisionsServiceMonitor(t *testing.T) {
	const ns = "v2-ep-servicemonitor"
	createNamespace(t, ns)
	startEgressProxyReconcilerWithServiceMonitor(t, nil)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "scraped", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	smGVK := schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(smGVK)
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "scraped-proxy-metrics"}, sm) == nil
	}, 10*time.Second, 100*time.Millisecond, "per-EgressProxy ServiceMonitor should be created")

	assert.True(t, hasControllerOwnerRef(sm.GetOwnerReferences(), "scraped"), "ServiceMonitor must be owned for GC")

	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)
	ep0 := endpoints[0].(map[string]interface{})
	assert.Equal(t, "metrics", ep0["port"])
	assert.Equal(t, "https", ep0["scheme"])
	tlsCfg := ep0["tlsConfig"].(map[string]interface{})
	assert.Equal(t, "scraped-proxy."+ns+".svc", tlsCfg["serverName"])
	ca := tlsCfg["ca"].(map[string]interface{})["secret"].(map[string]interface{})
	assert.Equal(t, "scraped-metrics-client", ca["name"], "scrape must present the per-EgressProxy client bundle")
}

// TestV2_EgressProxy_UnmanagedAutoscalingLifecycle proves the managedAutoscaling
// opt-out (Q173) against the real apiserver: the CRD defaults the field to true, a
// pool created with managedAutoscaling: false gets its Deployment (and the rest of
// the pool) but no HPA, flipping to true provisions the managed HPA, and flipping
// back to false deletes it — so an operator's replacement autoscaler never fights a
// leftover managed HPA.
func TestV2_EgressProxy_UnmanagedAutoscalingLifecycle(t *testing.T) {
	const ns = "v2-ep-unmanaged-hpa"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	unmanaged := false
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "byo", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{ManagedAutoscaling: &unmanaged},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	// The CRD default is managed (true): an empty-spec object must round-trip the
	// default, and this explicit false must survive it.
	var defaulted gmcv2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "byo"}, &defaulted))
	require.NotNil(t, defaulted.Spec.ManagedAutoscaling)
	assert.False(t, *defaulted.Spec.ManagedAutoscaling)

	name := proxyChildName("byo")

	// Wait for the NetworkPolicy — the last child applied in a reconcile pass — so
	// the absence of the HPA below is a real skip, not a mid-reconcile read race.
	var np networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &np) == nil
	}, 10*time.Second, 100*time.Millisecond, "proxy NetworkPolicy should be created")

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep))
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(2), *dep.Spec.Replicas, "minReplicas default seeds the initial count")

	var hpa autoscalingv2.HorizontalPodAutoscaler
	assert.Error(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &hpa),
		"no HPA should exist with managedAutoscaling: false")

	// Flip to managed: the HPA appears.
	require.Eventually(t, func() bool {
		var fetched gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "byo"}, &fetched); err != nil {
			return false
		}
		managed := true
		fetched.Spec.ManagedAutoscaling = &managed
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 100*time.Millisecond, "flip managedAutoscaling to true")
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &hpa) == nil
	}, 10*time.Second, 100*time.Millisecond, "flipping managedAutoscaling to true should provision the managed HPA")
	assert.True(t, hasControllerOwnerRef(hpa.OwnerReferences, "byo"))

	// Flip back to unmanaged: the managed HPA is deleted.
	require.Eventually(t, func() bool {
		var fetched gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "byo"}, &fetched); err != nil {
			return false
		}
		off := false
		fetched.Spec.ManagedAutoscaling = &off
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 100*time.Millisecond, "flip managedAutoscaling back to false")
	require.Eventually(t, func() bool {
		var got autoscalingv2.HorizontalPodAutoscaler
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got) != nil
	}, 10*time.Second, 100*time.Millisecond, "flipping managedAutoscaling to false should delete the managed HPA")
}

// TestV2_EgressProxy_AuditLoggingChange_RollsProxy verifies the per-pool egress
// audit knob (Q564) end to end through the reconciler: a pool that has not opted
// in carries NO audit env at all — not PROXY_AUDIT_LOGGING=Off, absent — so
// upgrading the GMC does not roll every existing pool, and flipping the field to
// Connections updates the pod template with both the mode and the downward-API
// namespace the record is stamped from. Mirrors TestV2_EgressProxy_LogLevelChange_RollsProxy.
func TestV2_EgressProxy_AuditLoggingChange_RollsProxy(t *testing.T) {
	const ns = "v2-ep-auditlog"
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
	assert.NotContains(t, env, "PROXY_AUDIT_LOGGING",
		"a pool defaulted to Off must carry no audit env, so upgrading the GMC rolls no existing pool")
	assert.NotContains(t, env, "POD_NAMESPACE",
		"the downward-API namespace rides with the audit opt-in, not on its own")

	require.Eventually(t, func() bool {
		var fetched gmcv2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyName}, &fetched); err != nil {
			return false
		}
		fetched.Spec.AuditLogging = "Connections"
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update EgressProxy spec.auditLogging to Connections")

	require.Eventually(t, func() bool {
		var got appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			return false
		}
		return containerEnv(t, got)["PROXY_AUDIT_LOGGING"] == "Connections"
	}, 10*time.Second, 100*time.Millisecond,
		"proxy Deployment must roll to PROXY_AUDIT_LOGGING=Connections")

	// The namespace must arrive by downward API rather than a formatted-in value:
	// the record names the namespace the pod actually runs in, and a field ref is
	// the one form a template substitution cannot get wrong.
	var rolled appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rolled))
	var podNS *corev1.EnvVar
	for i, e := range rolled.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "POD_NAMESPACE" {
			podNS = &rolled.Spec.Template.Spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, podNS, "opting in must inject POD_NAMESPACE")
	assert.Empty(t, podNS.Value, "POD_NAMESPACE must not be a literal")
	require.NotNil(t, podNS.ValueFrom, "POD_NAMESPACE must come from the downward API")
	require.NotNil(t, podNS.ValueFrom.FieldRef)
	assert.Equal(t, "metadata.namespace", podNS.ValueFrom.FieldRef.FieldPath)
}
