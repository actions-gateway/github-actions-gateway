//go:build integration

package integration_test

import (
	"net"
	"testing"
	"time"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// These tests exercise the Q208 CNI-native FQDN egress opt-in on the v2 EgressProxy
// reconciler end-to-end against the real apiserver: selecting CiliumFQDN/CalicoFQDN
// emits the matching CNI-native policy (asserted as an unstructured object — the suite
// installs stub Cilium/Calico CRDs), drops the GitHub CIDR rule from the standard
// NetworkPolicy (fail-closed), and removes the other-mode policy. The CIDR default is
// covered by the existing v2_egressproxy_test.go emission test.

var (
	ciliumGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
	calicoGVK = schema.GroupVersionKind{Group: "projectcalico.org", Version: "v3", Kind: "NetworkPolicy"}
)

func getCNIPolicy(t *testing.T, ns, name string, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
	return u, err
}

// stdNPHasGitHubCIDR reports whether the standard NetworkPolicy carries a port-443
// ipBlock egress rule — the CIDR-mode GitHub allowlist that FQDN mode must drop. It is
// scoped to port 443 so the DNS rule's NodeLocal link-local ipBlock (on port 53) does
// not count as a GitHub CIDR rule.
func stdNPHasGitHubCIDR(np networkingv1.NetworkPolicy) bool {
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
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

func TestV2_EgressProxy_CiliumFQDNMode(t *testing.T) {
	const ns = "v2-ep-cilium-fqdn"
	createNamespace(t, ns)

	// Seed CIDRs so that, in CIDR mode, the standard NP *would* carry a GitHub rule —
	// proving FQDN mode drops it rather than it merely being absent for lack of data.
	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconciler(t, ipCache)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"

	// The CiliumNetworkPolicy is emitted, owned, scoped to 443 toFQDNs incl. api.github.com.
	var cnp *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		cnp, gerr = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "CiliumNetworkPolicy should be emitted in CiliumFQDN mode")

	assert.True(t, hasControllerOwnerRef(cnp.GetOwnerReferences(), egressProxyName), "Cilium policy must be owned by the EgressProxy")
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 2)
	githubRule := egress[1].(map[string]interface{})
	fqdns, found, err := unstructured.NestedSlice(githubRule, "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	sawAPI := false
	for _, f := range fqdns {
		if f.(map[string]interface{})["matchName"] == "api.github.com" {
			sawAPI = true
		}
	}
	assert.True(t, sawAPI, "toFQDNs must include api.github.com")

	// The standard NetworkPolicy drops the GitHub CIDR rule (fail-closed posture).
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.False(t, stdNPHasGitHubCIDR(np), "FQDN mode must drop the GitHub CIDR egress rule from the standard NetworkPolicy")

	// The other-mode (Calico) policy must not exist.
	_, err = getCNIPolicy(t, ns, fqdnName, calicoGVK)
	assert.True(t, apierrors.IsNotFound(err), "Calico policy must be absent in CiliumFQDN mode, got %v", err)
}

// TestV2_EgressProxy_CiliumFQDNExtraDestinations proves the Q242 G.1 deliverable-3
// plumbing in CiliumFQDN mode: an operator destinationFQDNs entry is appended to the
// CiliumNetworkPolicy toFQDNs set and to the proxy CONNECT host-suffix env, and a
// destinationCIDRs entry lands as an ipBlock peer on the standard NetworkPolicy
// (which is applied in FQDN mode too) and in PROXY_ALLOWED_CIDRS.
func TestV2_EgressProxy_CiliumFQDNExtraDestinations(t *testing.T) {
	const ns = "v2-ep-cilium-extra"
	createNamespace(t, ns)
	startEgressProxyReconciler(t, nil)

	const destFQDN = "proxy.golang.org"
	const destCIDR = "199.36.153.8/30"
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN,
			DestinationFQDNs: []string{destFQDN},
			DestinationCIDRs: []string{destCIDR},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"
	var cnp *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		cnp, gerr = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "CiliumNetworkPolicy should be emitted")

	// The extra FQDN is appended to the GitHub toFQDNs set (as a matchName entry).
	egress, found, err := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	githubRule := egress[len(egress)-1].(map[string]interface{})
	fqdns, found, err := unstructured.NestedSlice(githubRule, "toFQDNs")
	require.NoError(t, err)
	require.True(t, found)
	sawExtra, sawGitHub := false, false
	for _, f := range fqdns {
		switch f.(map[string]interface{})["matchName"] {
		case destFQDN:
			sawExtra = true
		case "api.github.com":
			sawGitHub = true
		}
	}
	assert.True(t, sawExtra, "toFQDNs must include the operator destinationFQDNs entry")
	assert.True(t, sawGitHub, "toFQDNs must still include the implicit GitHub hostnames")

	// Deployment env: opted in, GitHub + extra FQDN in host suffixes, CIDR set.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &dep))
	env := containerEnv(t, dep)
	assert.Contains(t, env["PROXY_ALLOWED_HOST_SUFFIXES"], destFQDN)
	assert.Contains(t, env["PROXY_ALLOWED_HOST_SUFFIXES"], "api.github.com")
	assert.Equal(t, destCIDR, env["PROXY_ALLOWED_CIDRS"])

	// Standard NetworkPolicy (applied in FQDN mode too) carries the CIDR ipBlock.
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.True(t, npHasCIDRPeer(np, destCIDR), "destinationCIDRs must become an ipBlock peer even in FQDN mode")
}

func TestV2_EgressProxy_CalicoFQDNMode(t *testing.T) {
	const ns = "v2-ep-calico-fqdn"
	createNamespace(t, ns)

	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconciler(t, ipCache)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCalicoFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"

	var calico *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		calico, gerr = getCNIPolicy(t, ns, fqdnName, calicoGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "Calico NetworkPolicy should be emitted in CalicoFQDN mode")

	assert.True(t, hasControllerOwnerRef(calico.GetOwnerReferences(), egressProxyName), "Calico policy must be owned by the EgressProxy")
	egress, found, err := unstructured.NestedSlice(calico.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	githubRule := egress[len(egress)-1].(map[string]interface{})
	domains, found, err := unstructured.NestedStringSlice(githubRule, "destination", "domains")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, domains, "api.github.com")

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np))
	assert.False(t, stdNPHasGitHubCIDR(np), "FQDN mode must drop the GitHub CIDR egress rule")

	_, err = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
	assert.True(t, apierrors.IsNotFound(err), "Cilium policy must be absent in CalicoFQDN mode, got %v", err)
}
