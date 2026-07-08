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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// These tests exercise the Q245 intent/backend split end-to-end against the real
// apiserver: the tenant expresses egressPolicyMode: FQDN (intent only) and the operator's
// --fqdn-policy-backend picks the mechanism. With backend=gke the GMC emits a managed GKE
// FQDNNetworkPolicy (asserted as an unstructured object — the suite installs a stub
// networking.gke.io CRD). The security-critical assertion is that the base default-deny
// NetworkPolicy PERSISTS alongside the additive-allow GKE object, so a wide-open egress
// hole cannot open even though the GKE FQDNNetworkPolicy is a union, not a default-deny.

var gkeFQDNGVK = schema.GroupVersionKind{Group: "networking.gke.io", Version: "v1alpha1", Kind: "FQDNNetworkPolicy"}

func TestV2_EgressProxy_FQDNIntent_GKEBackend(t *testing.T) {
	const ns = "v2-ep-fqdn-gke"
	createNamespace(t, ns)

	// Seed CIDRs so that, in CIDR mode, the standard NP *would* carry a GitHub rule —
	// proving FQDN intent drops it (fail-closed base) rather than it merely being absent.
	ipCache := &controller.IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	ipCache.Set([]net.IPNet{*cidr})
	startEgressProxyReconcilerWithBackend(t, ipCache, controller.FQDNBackendGKE)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "FQDN intent must be admitted (suite backend is non-none)")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"

	// The GKE FQDNNetworkPolicy is emitted, owned, and scoped to 443 with GitHub matches.
	var gke *unstructured.Unstructured
	require.Eventually(t, func() bool {
		var gerr error
		gke, gerr = getCNIPolicy(t, ns, fqdnName, gkeFQDNGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "GKE FQDNNetworkPolicy should be emitted for FQDN intent + gke backend")

	assert.True(t, hasControllerOwnerRef(gke.GetOwnerReferences(), egressProxyName), "GKE policy must be owned by the EgressProxy")
	egress, found, err := unstructured.NestedSlice(gke.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 1, "GKE FQDNNetworkPolicy carries a single GitHub-FQDN egress rule")
	matches, found, err := unstructured.NestedSlice(egress[0].(map[string]interface{}), "matches")
	require.NoError(t, err)
	require.True(t, found)
	sawAPI := false
	for _, m := range matches {
		if m.(map[string]interface{})["name"] == "api.github.com" {
			sawAPI = true
		}
	}
	assert.True(t, sawAPI, "GKE matches must include api.github.com")

	// SECURITY INVARIANT (Q245): the base default-deny NetworkPolicy must PERSIST alongside
	// the additive-allow GKE FQDNNetworkPolicy — the GKE object only widens the union, it
	// never replaces the base. It must exist, deny GitHub egress (CIDR rule dropped), and
	// still allow DNS. If the base NP were dropped, GKE's union semantics would open egress.
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyChildName(egressProxyName)}, &np),
		"base default-deny NetworkPolicy must persist alongside the GKE FQDNNetworkPolicy")
	assert.False(t, stdNPHasGitHubCIDR(np), "FQDN intent must drop the GitHub CIDR rule from the base NetworkPolicy")
	assert.NotEmpty(t, np.Spec.Egress, "base NetworkPolicy must keep its DNS-only egress allow")
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress, "base NetworkPolicy must remain an Egress policy (default-deny)")

	// The other-backend (Cilium/Calico) policies must not exist.
	_, err = getCNIPolicy(t, ns, fqdnName, ciliumGVK)
	assert.True(t, apierrors.IsNotFound(err), "Cilium policy must be absent for the gke backend, got %v", err)
	_, err = getCNIPolicy(t, ns, fqdnName, calicoGVK)
	assert.True(t, apierrors.IsNotFound(err), "Calico policy must be absent for the gke backend, got %v", err)
}

// TestV2_EgressProxy_FQDNIntent_CiliumBackend proves the intent value FQDN resolves through
// the operator backend (here cilium): the same tenant intent that emits a GKE object under
// backend=gke emits a CiliumNetworkPolicy under backend=cilium, with no tenant API change.
func TestV2_EgressProxy_FQDNIntent_CiliumBackend(t *testing.T) {
	const ns = "v2-ep-fqdn-cilium"
	createNamespace(t, ns)
	startEgressProxyReconcilerWithBackend(t, nil, controller.FQDNBackendCilium)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: egressProxyName, Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeFQDN},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	fqdnName := egressProxyName + "-proxy-fqdn"
	require.Eventually(t, func() bool {
		_, gerr := getCNIPolicy(t, ns, fqdnName, ciliumGVK)
		return gerr == nil
	}, 10*time.Second, 100*time.Millisecond, "FQDN intent + cilium backend should emit a CiliumNetworkPolicy")

	// No GKE object for the cilium backend.
	_, err := getCNIPolicy(t, ns, fqdnName, gkeFQDNGVK)
	assert.True(t, apierrors.IsNotFound(err), "GKE policy must be absent for the cilium backend, got %v", err)
}
