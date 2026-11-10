package controller

import (
	"context"
	"strings"
	"testing"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ghesGateway(name, ns, githubURL, proxyRef string) *gmcv2.ActionsGateway {
	ag := v2Gateway(name, ns, "github-app", proxyRef)
	ag.Spec.GitHubURL = githubURL
	return ag
}

func runnerSetBoundTo(name, ns, gateway, proxyRef string) *gmcv2.RunnerSet {
	rs := &gmcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2.RunnerSetSpec{
			GatewayRef: gmcv2.ObjectRef{Name: gateway},
		},
	}
	if proxyRef != "" {
		rs.Spec.ProxyRef = &gmcv2.ProxyObjectRef{Name: proxyRef}
	}
	return rs
}

func TestResolveReferrerGitHubHosts(t *testing.T) {
	const ns = "team-a"
	// The platform allowlist every case runs under: one suffix, so a host is covered
	// by being example.com or under it.
	al := allowlist.NewEgressDestination([]string{"example.com"}, nil)
	cases := []struct {
		name        string
		objs        []client.Object
		want        []string
		wantRefused []string
	}{
		{
			name: "no referrers",
			objs: nil,
			want: nil,
		},
		{
			name: "a public-GitHub referrer adds nothing the built-in list lacks",
			objs: []client.Object{ghesGateway("gw", ns, "https://github.com/acme", "shared")},
			want: nil,
		},
		{
			name: "defaultProxyRef contributes its GHES host",
			objs: []client.Object{ghesGateway("gw", ns, "https://ghes.example.com/acme", "shared")},
			want: []string{"ghes.example.com"},
		},
		{
			name: "a gateway bound to another proxy is ignored",
			objs: []client.Object{ghesGateway("gw", ns, "https://ghes.example.com/acme", "other")},
			want: nil,
		},
		{
			name: "a RunnerSet proxyRef contributes its gateway's host",
			objs: []client.Object{
				ghesGateway("gw", ns, "https://ghes.example.com/acme", ""),
				runnerSetBoundTo("rs", ns, "gw", "shared"),
			},
			want: []string{"ghes.example.com"},
		},
		{
			name: "a RunnerSet whose gateway is not applied contributes nothing",
			objs: []client.Object{runnerSetBoundTo("rs", ns, "missing", "shared")},
			want: nil,
		},
		{
			name: "two referrers on the same host yield one entry",
			objs: []client.Object{
				ghesGateway("gw", ns, "https://ghes.example.com/acme", "shared"),
				runnerSetBoundTo("rs", ns, "gw", "shared"),
			},
			want: []string{"ghes.example.com"},
		},
		{
			name: "distinct hosts are sorted so the emitted policy does not churn",
			objs: []client.Object{
				ghesGateway("b", ns, "https://zeta.example.com/acme", "shared"),
				ghesGateway("a", ns, "https://alpha.example.com/acme", "shared"),
			},
			want: []string{"alpha.example.com", "zeta.example.com"},
		},
		{
			name: "a referrer in another namespace is out of scope",
			objs: []client.Object{ghesGateway("gw", "team-b", "https://ghes.example.com/acme", "shared")},
			want: nil,
		},
		// Q1322: githubURL is tenant-authored, so its host is emitted only when the
		// platform egress allowlist covers it, as a destinationFQDNs entry is.
		{
			name:        "a host the platform allowlist does not cover is refused",
			objs:        []client.Object{ghesGateway("gw", ns, "https://exfil.example.net/acme", "shared")},
			wantRefused: []string{"exfil.example.net"},
		},
		{
			name: "allowed and refused hosts are split, each sorted",
			objs: []client.Object{
				ghesGateway("a", ns, "https://zeta.example.net/acme", "shared"),
				ghesGateway("b", ns, "https://ghes.example.com/acme", "shared"),
				ghesGateway("c", ns, "https://alpha.example.net/acme", "shared"),
			},
			want:        []string{"ghes.example.com"},
			wantRefused: []string{"alpha.example.net", "zeta.example.net"},
		},
		{
			// CoversFQDN reads "*.example.com" as example.com, which the allowlist
			// covers; emitted, the wildcard would be a pattern matching every host
			// under it.
			name:        "a wildcard host is refused though its parent is allowlisted",
			objs:        []client.Object{ghesGateway("gw", ns, "https://*.example.com/acme", "shared")},
			wantRefused: []string{"*.example.com"},
		},
		{
			name:        "a host that only ends with an allowlisted name is refused",
			objs:        []client.Object{ghesGateway("gw", ns, "https://notexample.com/acme", "shared")},
			wantRefused: []string{"notexample.com"},
		},
		{
			name: "letter case and a trailing dot do not defeat the allowlist",
			objs: []client.Object{ghesGateway("gw", ns, "https://GHES.Example.COM./acme", "shared")},
			want: []string{"GHES.Example.COM."},
		},
		{
			// url.Parse keeps the host's case, so an exact match against the built-in
			// list would read this gateway as an appliance and refuse it.
			name: "public GitHub in another letter case is still the built-in list's",
			objs: []client.Object{
				ghesGateway("gw1", ns, "https://GitHub.com/acme", "shared"),
				ghesGateway("gw2", ns, "https://API.GitHub.com./acme", "shared"),
			},
			want: nil,
		},
		{
			// www.github.com is public GitHub and not on the built-in list. It is
			// emitted as it always was; no allowlist names it.
			name: "the www spelling of public GitHub needs no allowlist entry",
			objs: []client.Object{ghesGateway("gw", ns, "https://WWW.github.com/acme", "shared")},
			want: []string{"WWW.github.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(actionsGatewayV2TestScheme(t)).
				WithObjects(tc.objs...).
				Build()
			got, err := resolveReferrerGitHubHosts(context.Background(), c, &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shared"}}, al)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.allowed)
			assert.Equal(t, tc.wantRefused, got.refused)
		})
	}

	t.Run("no allowlist covers nothing", func(t *testing.T) {
		// The secure default: a GMC with no egress allowlist emits the built-in
		// GitHub hosts and nothing a tenant names.
		c := fake.NewClientBuilder().
			WithScheme(actionsGatewayV2TestScheme(t)).
			WithObjects(ghesGateway("gw", ns, "https://ghes.example.com/acme", "shared")).
			Build()
		for _, none := range []*allowlist.EgressDestinationAllowlist{nil, allowlist.NewEgressDestination(nil, nil)} {
			got, err := resolveReferrerGitHubHosts(context.Background(), c, &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shared"}}, none)
			require.NoError(t, err)
			assert.Empty(t, got.allowed)
			assert.Equal(t, []string{"ghes.example.com"}, got.refused)
		}
	})
}

// TestEvalGitHubEgressIncomplete pins the one GHES gap the GMC cannot close (Q506 #3).
// CIDR mode programs the ranges api.github.com/meta publishes and an appliance appears
// in none of them, so the condition names the operator obligation instead of leaving a
// GHES tenant with a connect timeout and no cause.
func TestEvalGitHubEgressIncomplete(t *testing.T) {
	unmanaged := false
	fqdn := gmcv2.EgressProxySpec{EgressPolicyMode: gmcv2.EgressPolicyModeFQDN}
	cases := []struct {
		name           string
		spec           gmcv2.EgressProxySpec
		hosts          []string
		refused        []string
		wantIncomplete bool
		wantReason     string
		wantRemedy     string
	}{
		{
			name:           "CIDR mode with a GHES referrer and no ranges",
			hosts:          []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "ApplianceRangesRequired",
			wantRemedy:     "destinationCIDRs",
		},
		{
			// CIDR mode reaches an appliance by range, so the name's allowlist
			// standing does not change what the operator owes.
			name:           "CIDR mode names a refused host by the ranges it needs",
			refused:        []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "ApplianceRangesRequired",
			wantRemedy:     "destinationCIDRs",
		},
		{
			name:       "CIDR mode with ranges reaches a refused host by address",
			spec:       gmcv2.EgressProxySpec{DestinationCIDRs: []string{"10.0.0.0/8"}},
			refused:    []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:           "an FQDN mode leaves a refused host out of its policy",
			spec:           fqdn,
			refused:        []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "GitHubHostNotAllowlisted",
			wantRemedy:     "--allowed-egress-fqdns",
		},
		{
			name: "an FQDN mode with one refused host among allowed ones",
			spec: fqdn, hosts: []string{"other.example.com"},
			refused:        []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "GitHubHostNotAllowlisted",
			wantRemedy:     "--allowed-egress-fqdns",
		},
		{
			// The policy is the operator's, but the CONNECT allowlist the
			// destinationFQDNs switch on is still the GMC's, and it lacks the host.
			name: "an operator-maintained policy with a CONNECT allowlist and no ranges",
			spec: gmcv2.EgressProxySpec{
				ManagedNetworkPolicy: &unmanaged,
				DestinationFQDNs:     []string{"artifactory.corp"},
			},
			refused:        []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "GitHubHostNotAllowlisted",
			wantRemedy:     "--allowed-egress-fqdns",
		},
		{
			name:       "an operator-maintained policy with no CONNECT allowlist emits nothing to refuse",
			spec:       gmcv2.EgressProxySpec{ManagedNetworkPolicy: &unmanaged},
			refused:    []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "public-GitHub referrers only",
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "the operator supplied ranges",
			spec:       gmcv2.EgressProxySpec{DestinationCIDRs: []string{"10.0.0.0/8"}},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "an FQDN mode already carries the host",
			spec:       gmcv2.EgressProxySpec{EgressPolicyMode: gmcv2.EgressPolicyModeFQDN},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "an operator-maintained policy is not ours to judge",
			spec:       gmcv2.EgressProxySpec{ManagedNetworkPolicy: &unmanaged},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalGitHubEgressIncomplete(&gmcv2.EgressProxy{Spec: tc.spec},
				referrerGitHubHosts{allowed: tc.hosts, refused: tc.refused})
			assert.Equal(t, tc.wantIncomplete, got.incomplete)
			assert.Equal(t, tc.wantReason, got.reason)
			assert.NotEmpty(t, got.message, "an operator reading the condition needs the why")
			if tc.wantIncomplete {
				assert.Contains(t, got.message, "ghes.example.com", "the message must name the unreachable host")
				assert.Contains(t, got.message, tc.wantRemedy, "the message must name the remedy")
			}
		})
	}
}

// TestEgressFQDNs_CarriesReferrerHost is the FQDN-policy half of Q506 #2: without it
// a GHES tenant's CNI policy names six public hosts and none of the appliance its
// traffic actually uses.
func TestEgressFQDNs_CarriesReferrerHost(t *testing.T) {
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2.EgressProxySpec{DestinationFQDNs: []string{"artifactory.corp"}},
	}
	got := egressFQDNs(ep, []string{"ghes.example.com"})
	assert.Subset(t, got, githubEgressFQDNs, "the built-in GitHub set is never dropped")
	assert.Contains(t, got, "ghes.example.com")
	assert.Contains(t, got, "artifactory.corp", "operator destinationFQDNs still apply")
}

// TestProxyAllowlistEnv_CarriesReferrerHost is the CONNECT half. The env is emitted
// only when the operator opted in with an extra destination, so the fixture sets one.
func TestProxyAllowlistEnv_CarriesReferrerHost(t *testing.T) {
	ep := &gmcv2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2.EgressProxySpec{DestinationFQDNs: []string{"artifactory.corp"}},
	}
	env := proxyAllowlistEnv(ep, []string{"ghes.example.com"})
	require.NotEmpty(t, env)
	suffixes := strings.Split(env[0].Value, ",")
	assert.Contains(t, suffixes, "ghes.example.com",
		"a GHES tenant's CONNECT to its own appliance must be permitted")
	assert.Contains(t, suffixes, "api.github.com")
	assert.Contains(t, suffixes, "artifactory.corp")
}
