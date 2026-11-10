//go:build integration

package integration_test

import (
	"strings"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "covered", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			// FQDNs require an FQDN egress mode (CRD CEL); proxy.golang.org is a
			// subdomain of the allowlisted golang.org, and 10.20.0.0/16 ⊆ 10.0.0.0/8.
			// The suite wires the webhook with --fqdn-policy-backend=cilium, so the
			// FQDN intent is admitted without a backend rejection.
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
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

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			EgressPolicyMode: gmcv2.EgressPolicyModeFQDN,
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

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
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
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "broad", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
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

	bad := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "noproxy-github", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			NoProxyCIDRs: []string{"github.com"},
		},
	}
	err := k8sClient.Create(ctx, bad)
	require.Error(t, err, "a GitHub host in noProxyCIDRs must be rejected by the webhook through the apiserver")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy",
		"rejection must come from the GMC validating webhook")

	good := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "noproxy-ok", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
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
func ghesAdmissionGateway(name, ns, gitHubURL, proxyRef string) *gmcv2.ActionsGateway {
	gw := &gmcv2.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2.ActionsGatewaySpec{
			Credentials: gmcv2.GitHubCredentials{
				Type:      gmcv2.CredentialTypeGitHubApp,
				GitHubApp: &gmcv2.LocalSecretReference{Name: "github-app"},
			},
			GitHubURL: gitHubURL,
		},
	}
	if proxyRef != "" {
		gw.Spec.DefaultProxyRef = &gmcv2.ProxyObjectRef{Name: proxyRef}
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

	bad := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-proxy", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
			NoProxyCIDRs: []string{"ghes.corp.example"},
		},
	}
	err := k8sClient.Create(ctx, bad)
	require.Error(t, err, "a noProxyCIDRs entry covering the referring gateway's GHES host must be rejected (Q322)")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	assert.Contains(t, err.Error(), "ghes-gw", "the rejection must name the referrer that binds the GHES host")

	good := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-proxy", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
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

	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-proxy", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{
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

	badRS := &gmcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-set", Namespace: ns},
		Spec: gmcv2.RunnerSetSpec{
			GatewayRef:   gmcv2.ObjectRef{Name: "ghes-gw"},
			RunnerLabels: []string{"ghes-test"},
			ProxyRef:     &gmcv2.ProxyObjectRef{Name: "corp-proxy"},
		},
	}
	err = k8sClient.Create(ctx, badRS)
	require.Error(t, err, "a RunnerSet binding the gateway's GHES host to the excluding proxy must be rejected (Q322)")
	assert.Contains(t, err.Error(), "spec.proxyRef")

	goodRS := &gmcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-set", Namespace: ns},
		Spec: gmcv2.RunnerSetSpec{
			GatewayRef:   gmcv2.ObjectRef{Name: "ghes-gw"},
			RunnerLabels: []string{"ghes-test"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, goodRS),
		"a set with no proxyRef inherits nothing conflicting here and must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, goodRS) })
}

// TestV2_EgressProxy_Admission_NoProxyForms verifies the apiserver rejects the
// NO_PROXY forms that exempt a GitHub host without spelling it as a bare suffix
// (Q1322): a comma-carrying entry, a wildcard, and a port-qualified host.
func TestV2_EgressProxy_Admission_NoProxyForms(t *testing.T) {
	const ns = "v2-ep-adm-noproxy-forms"
	createNamespace(t, ns)

	for _, entry := range []string{"x,github.com", "*", "*.github.com", "github.com:443", "x github.com", "github.com."} {
		ep := &gmcv2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "forms", Namespace: ns},
			Spec:       gmcv2.EgressProxySpec{NoProxyCIDRs: []string{"10.0.0.0/8", entry}},
		}
		err := k8sClient.Create(ctx, ep)
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, ep)) })
		require.Errorf(t, err, "noProxyCIDRs entry %q must be rejected", entry)
		assert.Contains(t, err.Error(), "spec.noProxyCIDRs[1]")
		assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
	}

	ok := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "forms", Namespace: ns},
		Spec: gmcv2.EgressProxySpec{NoProxyCIDRs: []string{
			"10.0.0.0/8,192.168.0.0/16", "*.internal.example.com", "registry.internal.example.com:5000",
		}},
	}
	require.NoError(t, k8sClient.Create(ctx, ok), "the same forms naming no GitHub host must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })
}

// TestV2_EgressProxy_Admission_CrossNamespaceReferrer verifies the noProxyCIDRs
// guard compares a shared EgressProxy with the referrers in the namespaces it
// grants, from both sides, through the real apiserver (Q1322).
func TestV2_EgressProxy_Admission_CrossNamespaceReferrer(t *testing.T) {
	const provider, consumer = "v2-ep-adm-xns-provider", "v2-ep-adm-xns-consumer"
	createNamespace(t, provider)
	createNamespace(t, consumer)

	sharedProxy := func(name string, entries ...string) *gmcv2.EgressProxy {
		return &gmcv2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: provider},
			Spec: gmcv2.EgressProxySpec{
				NoProxyCIDRs: entries,
				Sharing:      &gmcv2.ProxySharing{AllowedNamespaces: []string{consumer}},
			},
		}
	}
	consumerGateway := func(name, proxy string) *gmcv2.ActionsGateway {
		gw := ghesAdmissionGateway(name, consumer, "https://ghes.corp.example/my-org", "")
		gw.Spec.DefaultProxyRef = &gmcv2.ProxyObjectRef{Name: proxy, Namespace: provider}
		return gw
	}

	// Proxy side: the referrer exists first, in another namespace.
	gw := consumerGateway("early-gw", "late-proxy")
	require.NoError(t, k8sClient.Create(ctx, gw), "a gateway naming a not-yet-applied shared proxy is well-formed (§H.7)")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, gw) })

	bad := sharedProxy("late-proxy", "corp.example")
	err := k8sClient.Create(ctx, bad)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, bad)) })
	require.Error(t, err, "a shared proxy excluding a granted consumer's GHES host must be rejected")
	assert.Contains(t, err.Error(), "ghes.corp.example")
	assert.Contains(t, err.Error(), consumer+"/early-gw", "the rejection must name the cross-namespace referrer")

	ungranted := sharedProxy("late-proxy", "corp.example")
	ungranted.Spec.Sharing = nil
	require.NoError(t, k8sClient.Create(ctx, ungranted), "with no grant the consumer's gateway binds nothing")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ungranted) })

	var stored gmcv2.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: provider, Name: "late-proxy"}, &stored))
	stored.Spec.Sharing = &gmcv2.ProxySharing{AllowedNamespaces: []string{consumer}}
	err = k8sClient.Update(ctx, &stored)
	require.Error(t, err, "granting the consumer binds its gateway's GHES host to the unchanged entries")
	assert.Contains(t, err.Error(), consumer+"/early-gw")

	// Referrer side: the shared proxy exists first.
	early := sharedProxy("early-proxy", ".corp.example")
	require.NoError(t, k8sClient.Create(ctx, early), "a non-GitHub suffix is admitted while no referrer binds a host under it")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, early) })

	err = k8sClient.Create(ctx, consumerGateway("late-gw", "early-proxy"))
	require.Error(t, err, "a consumer gateway binding its GHES host to a shared proxy that excludes it must be rejected")
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")

	rs := &gmcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "late-set", Namespace: consumer},
		Spec: gmcv2.RunnerSetSpec{
			GatewayRef:   gmcv2.ObjectRef{Name: "early-gw"},
			RunnerLabels: []string{"xns-ghes-test"},
			ProxyRef:     &gmcv2.ProxyObjectRef{Name: "early-proxy", Namespace: provider},
		},
	}
	err = k8sClient.Create(ctx, rs)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, rs)) })
	require.Error(t, err, "a RunnerSet binding its gateway's GHES host to the excluding shared proxy must be rejected")
	assert.Contains(t, err.Error(), "spec.proxyRef")
}

// TestV2_EgressProxy_Admission_StoredNoProxyEntryKeepsWorking verifies an EgressProxy
// stored with an entry the guard now refuses still takes an edit that leaves
// noProxyCIDRs alone, while a new object or a changed list is refused (Q1322). The
// stored value is planted with the webhook scoped away from the namespace, which is
// the state an upgrade from a release without the rule leaves.
func TestV2_EgressProxy_Admission_StoredNoProxyEntryKeepsWorking(t *testing.T) {
	const ns = "v2-ep-adm-noproxy-stored"
	bypassAdmission(t, "vegressproxy-v2.kb.io")
	createNamespaceWithLabels(t, ns, map[string]string{admissionBypassLabel: "true"})

	newProxy := func(name string) *gmcv2.EgressProxy {
		return &gmcv2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       gmcv2.EgressProxySpec{NoProxyCIDRs: []string{"*.github.com"}},
		}
	}
	requireCreateEventually(t, newProxy("stored"))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, newProxy("stored")) })

	// Put the namespace back under the webhook, and wait until it is seen to bite.
	var namespace corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &namespace))
	delete(namespace.Labels, admissionBypassLabel)
	require.NoError(t, k8sClient.Update(ctx, &namespace))
	fresh := newProxy("fresh")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, fresh)) })
	require.Eventually(t, func() bool {
		err := k8sClient.Create(ctx, fresh)
		if err == nil {
			_ = k8sClient.Delete(ctx, fresh)
			return false
		}
		return strings.Contains(err.Error(), "around the per-tenant egress proxy")
	}, 30*time.Second, 200*time.Millisecond, "the webhook must reject a new object carrying the stored entry")

	update := func(mutate func(*gmcv2.EgressProxy)) error {
		var got gmcv2.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "stored"}, &got); err != nil {
			return err
		}
		mutate(&got)
		return k8sClient.Update(ctx, &got)
	}
	require.NoError(t, update(func(*gmcv2.EgressProxy) {}), "re-applying it unchanged must succeed")
	require.NoError(t, update(func(ep *gmcv2.EgressProxy) { ep.Spec.DestinationCIDRs = []string{"10.20.0.0/16"} }),
		"an edit that leaves noProxyCIDRs alone must succeed")

	err := update(func(ep *gmcv2.EgressProxy) { ep.Spec.NoProxyCIDRs = append(ep.Spec.NoProxyCIDRs, "10.0.0.0/8") })
	require.Error(t, err, "changing the list re-judges the stored entry")
	assert.Contains(t, err.Error(), "spec.noProxyCIDRs[0]")

	require.NoError(t, update(func(ep *gmcv2.EgressProxy) { ep.Spec.NoProxyCIDRs = []string{"10.0.0.0/8"} }),
		"removing the entry must succeed")
}

func TestV2_EgressProxy_Admission_NoDestinationsAlwaysAllowed(t *testing.T) {
	const ns = "v2-ep-adm-empty"
	createNamespace(t, ns)

	// An EgressProxy with no extra destinations is GitHub-only and must always be
	// admitted regardless of the (here, non-empty) platform allowlist.
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec:       gmcv2.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep), "an EgressProxy with no extra destinations must always be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
}

// TestV2_EgressProxy_AliasRejectedBySchema pins what replaced the Q1085 admission
// guard: v2 is the only served version and its enum does not define CiliumFQDN or
// CalicoFQDN, so the apiserver refuses either before admission runs.
func TestV2_EgressProxy_AliasRejectedBySchema(t *testing.T) {
	const ns = "v2-ep-alias-schema"
	createNamespace(t, ns)

	for _, alias := range []gmcv2.EgressPolicyMode{"CiliumFQDN", "CalicoFQDN"} {
		ep := &gmcv2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(string(alias)), Namespace: ns},
			Spec:       gmcv2.EgressProxySpec{EgressPolicyMode: alias},
		}
		err := k8sClient.Create(ctx, ep)
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, ep)) })
		require.Error(t, err, "%s is not a v2 enum member and must be rejected", alias)
		assert.True(t, apierrors.IsInvalid(err), "expected the schema's Invalid for %s, got %v", alias, err)
	}
}
