//go:build integration

package integration_test

import (
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests exercise the Q242 G.1 EgressProxy validating webhook end-to-end
// against the real apiserver: the suite registers it wired to egressTestAllowlist
// (static FQDN suffix golang.org; CIDRs 10.0.0.0/8 and 199.36.153.0/24). The webhook
// gates the tenant-authorable destinationFQDNs/destinationCIDRs against that
// platform allowlist — a request inside the allowlist is admitted, one outside is
// rejected, and an EgressProxy with no extra destinations always passes.

func TestV2_EgressProxy_Admission_AllowsCoveredDestinations(t *testing.T) {
	const ns = "v2-ep-adm-allow"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "covered", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			// FQDNs require an FQDN egress mode (CRD CEL); proxy.golang.org is a
			// subdomain of the allowlisted golang.org, and 10.20.0.0/16 ⊆ 10.0.0.0/8.
			EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN,
			DestinationFQDNs: []string{"proxy.golang.org", "sum.golang.org"},
			DestinationCIDRs: []string{"10.20.0.0/16"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "destinations covered by the platform allowlist must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
}

func TestV2_EgressProxy_Admission_RejectsOffAllowlistFQDN(t *testing.T) {
	const ns = "v2-ep-adm-deny-fqdn"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN,
			DestinationFQDNs: []string{"evil.example.com"},
		},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err, "an off-allowlist destinationFQDNs entry must be rejected")
	assert.Contains(t, err.Error(), "platform egress allowlist")
}

func TestV2_EgressProxy_Admission_RejectsOffAllowlistCIDR(t *testing.T) {
	const ns = "v2-ep-adm-deny-cidr"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			DestinationCIDRs: []string{"8.8.8.0/24"},
		},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err, "an off-allowlist destinationCIDRs entry must be rejected")
	assert.Contains(t, err.Error(), "platform egress allowlist")
}

func TestV2_EgressProxy_Admission_RejectsTooBroadCIDR(t *testing.T) {
	const ns = "v2-ep-adm-broad-cidr"
	createNamespace(t, ns)

	// 10.0.0.0/7 is broader than the allowlisted 10.0.0.0/8 — subnet containment
	// must reject it (a tenant cannot widen beyond what the platform permitted).
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "broad", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			DestinationCIDRs: []string{"10.0.0.0/7"},
		},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err, "a CIDR broader than the allowlisted range must be rejected")
}

func TestV2_EgressProxy_Admission_NoDestinationsAlwaysAllowed(t *testing.T) {
	const ns = "v2-ep-adm-empty"
	createNamespace(t, ns)

	// An EgressProxy with no extra destinations is GitHub-only and must always be
	// admitted regardless of the (here, non-empty) platform allowlist.
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "an EgressProxy with no extra destinations must always be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
}
