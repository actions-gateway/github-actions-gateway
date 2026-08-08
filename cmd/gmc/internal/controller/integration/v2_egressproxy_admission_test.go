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

// TestV2_EgressProxy_Admission_NoProxyCIDRs verifies the apiserver rejects a
// spec.noProxyCIDRs entry that NO_PROXY-matches a public GitHub host (which would
// route GitHub traffic around the per-tenant egress proxy) and admits CIDRs and
// non-GitHub domain suffixes — the v2 twin of TestWebhookAdmission_NoProxyCIDRs.
func TestV2_EgressProxy_Admission_NoProxyCIDRs(t *testing.T) {
	const ns = "v2-ep-adm-noproxy"
	createNamespace(t, ns)

	bad := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "noproxy-github", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			NoProxyCIDRs: []string{"github.com"},
		},
	}
	err := k8sClient.Create(ctx, bad)
	require.Error(t, err, "a GitHub host in noProxyCIDRs must be rejected by the webhook through the apiserver")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy",
		"rejection must come from the GMC validating webhook")

	good := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "noproxy-ok", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			NoProxyCIDRs: []string{"10.0.0.0/8", "203.0.113.5/32", "svc.cluster.local"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, good),
		"CIDRs and non-GitHub domain suffixes in noProxyCIDRs must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, good) })
}

// ghesAdmissionGateway builds a v2 ActionsGateway bound to the given (GHES) GitHub
// URL for the Q322 admission cases. The credentials Secret need not exist: admission
// never resolves it (the reconciler surfaces a missing Secret as a runtime condition).
func ghesAdmissionGateway(name, ns, gitHubURL, proxyRef string) *gmcv2alpha1.ActionsGateway {
	gw := &gmcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.ActionsGatewaySpec{
			Credentials: gmcv2alpha1.GitHubCredentials{
				Type:      gmcv2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &gmcv2alpha1.LocalSecretReference{Name: "github-app"},
			},
			GitHubURL: gitHubURL,
		},
	}
	if proxyRef != "" {
		gw.Spec.DefaultProxyRef = &gmcv2alpha1.ProxyObjectRef{Name: proxyRef}
	}
	return gw
}

// TestV2_EgressProxy_Admission_ReferrerGHESHost is the proxy side of the Q322 guard
// end-to-end: with a gateway bound to a GitHub Enterprise Server host referencing the
// proxy, a spec.noProxyCIDRs entry covering that GHES host is rejected through the
// real apiserver, while internal entries stay admitted.
func TestV2_EgressProxy_Admission_ReferrerGHESHost(t *testing.T) {
	const ns = "v2-ep-adm-ghes"
	createNamespace(t, ns)

	gw := ghesAdmissionGateway("ghes-gw", ns, "https://ghes.corp.example/my-org", "ghes-proxy")
	require.NoError(t, k8sClient.Create(ctx, gw), "a gateway referencing a not-yet-applied proxy is well-formed (§H.7)")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	bad := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-proxy", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			NoProxyCIDRs: []string{"ghes.corp.example"},
		},
	}
	err := k8sClient.Create(ctx, bad)
	require.Error(t, err, "a noProxyCIDRs entry covering the referring gateway's GHES host must be rejected (Q322)")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	assert.Contains(t, err.Error(), "ghes-gw", "the rejection must name the referrer that binds the GHES host")

	good := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-proxy", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			NoProxyCIDRs: []string{"10.0.0.0/8", "svc.cluster.local", "internal.example.com"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, good),
		"internal noProxyCIDRs entries must stay admitted with a GHES referrer present")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, good) })
}

// TestV2_Referrer_Admission_GHESHostInExistingProxy is the referrer side of the Q322
// guard end-to-end: a proxy whose noProxyCIDRs would exclude a GHES host is admitted
// while nothing binds that host, but the gateway (defaultProxyRef) or RunnerSet
// (proxyRef) write that assembles the bypass pair afterwards is rejected.
func TestV2_Referrer_Admission_GHESHostInExistingProxy(t *testing.T) {
	const ns = "v2-ref-adm-ghes"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-proxy", Namespace: ns},
		Spec: gmcv2alpha1.EgressProxySpec{
			NoProxyCIDRs: []string{".corp.example"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep),
		"a non-GitHub suffix is admitted while no referrer binds a host under it")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	badGw := ghesAdmissionGateway("ghes-gw", ns, "https://ghes.corp.example/my-org", "corp-proxy")
	err := k8sClient.Create(ctx, badGw)
	require.Error(t, err, "binding a GHES host to a proxy that excludes it must be rejected (Q322)")
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")

	// The same gateway with no proxy bound is fine — direct egress excludes nothing.
	gw := ghesAdmissionGateway("ghes-gw", ns, "https://ghes.corp.example/my-org", "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	badRS := &gmcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-set", Namespace: ns},
		Spec: gmcv2alpha1.RunnerSetSpec{
			GatewayRef:   gmcv2alpha1.ObjectRef{Name: "ghes-gw"},
			RunnerLabels: []string{"ghes-test"},
			ProxyRef:     &gmcv2alpha1.ProxyObjectRef{Name: "corp-proxy"},
		},
	}
	err = k8sClient.Create(ctx, badRS)
	require.Error(t, err, "a RunnerSet binding the gateway's GHES host to the excluding proxy must be rejected (Q322)")
	assert.Contains(t, err.Error(), "spec.proxyRef")

	goodRS := &gmcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-set", Namespace: ns},
		Spec: gmcv2alpha1.RunnerSetSpec{
			GatewayRef:   gmcv2alpha1.ObjectRef{Name: "ghes-gw"},
			RunnerLabels: []string{"ghes-test"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, goodRS),
		"a set with no proxyRef inherits nothing conflicting here and must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, goodRS) })
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
