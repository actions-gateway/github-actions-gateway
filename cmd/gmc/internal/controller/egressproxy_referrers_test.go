package controller

import (
	"context"
	"strings"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ghesGateway(name, ns, githubURL, proxyRef string) *gmcv2alpha1.ActionsGateway {
	ag := v2Gateway(name, ns, "github-app", proxyRef)
	ag.Spec.GitHubURL = githubURL
	return ag
}

func runnerSetBoundTo(name, ns, gateway, proxyRef string) *gmcv2alpha1.RunnerSet {
	rs := &gmcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.RunnerSetSpec{
			GatewayRef: gmcv2alpha1.ObjectRef{Name: gateway},
		},
	}
	if proxyRef != "" {
		rs.Spec.ProxyRef = &gmcv2alpha1.ProxyObjectRef{Name: proxyRef}
	}
	return rs
}

func TestResolveReferrerGitHubHosts(t *testing.T) {
	const ns = "team-a"
	cases := []struct {
		name  string
		objs  []client.Object
		want  []string
		notes string
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(actionsGatewayV2TestScheme(t)).
				WithObjects(tc.objs...).
				Build()
			got, err := resolveReferrerGitHubHosts(context.Background(), c, &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shared"}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEvalGitHubEgressIncomplete pins the one GHES gap the GMC cannot close (Q506 #3).
// CIDR mode programs the ranges api.github.com/meta publishes and an appliance appears
// in none of them, so the condition names the operator obligation instead of leaving a
// GHES tenant with a connect timeout and no cause.
func TestEvalGitHubEgressIncomplete(t *testing.T) {
	unmanaged := false
	cases := []struct {
		name           string
		spec           gmcv2alpha1.EgressProxySpec
		hosts          []string
		wantIncomplete bool
		wantReason     string
	}{
		{
			name:           "CIDR mode with a GHES referrer and no ranges",
			hosts:          []string{"ghes.example.com"},
			wantIncomplete: true,
			wantReason:     "ApplianceRangesRequired",
		},
		{
			name:       "public-GitHub referrers only",
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "the operator supplied ranges",
			spec:       gmcv2alpha1.EgressProxySpec{DestinationCIDRs: []string{"10.0.0.0/8"}},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "an FQDN mode already carries the host",
			spec:       gmcv2alpha1.EgressProxySpec{EgressPolicyMode: gmcv2alpha1.EgressPolicyModeCiliumFQDN},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
		{
			name:       "an operator-maintained policy is not ours to judge",
			spec:       gmcv2alpha1.EgressProxySpec{ManagedNetworkPolicy: &unmanaged},
			hosts:      []string{"ghes.example.com"},
			wantReason: "GitHubEgressAllowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalGitHubEgressIncomplete(&gmcv2alpha1.EgressProxy{Spec: tc.spec}, tc.hosts)
			assert.Equal(t, tc.wantIncomplete, got.incomplete)
			assert.Equal(t, tc.wantReason, got.reason)
			assert.NotEmpty(t, got.message, "an operator reading the condition needs the why")
			if tc.wantIncomplete {
				assert.Contains(t, got.message, "ghes.example.com", "the message must name the unreachable host")
				assert.Contains(t, got.message, "destinationCIDRs", "the message must name the remedy")
			}
		})
	}
}

// TestEgressFQDNs_CarriesReferrerHost is the FQDN-policy half of Q506 #2: without it
// a GHES tenant's CNI policy names six public hosts and none of the appliance its
// traffic actually uses.
func TestEgressFQDNs_CarriesReferrerHost(t *testing.T) {
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"artifactory.corp"}},
	}
	got := egressFQDNs(ep, []string{"ghes.example.com"})
	assert.Subset(t, got, githubEgressFQDNs, "the built-in GitHub set is never dropped")
	assert.Contains(t, got, "ghes.example.com")
	assert.Contains(t, got, "artifactory.corp", "operator destinationFQDNs still apply")
}

// TestProxyAllowlistEnv_CarriesReferrerHost is the CONNECT half. The env is emitted
// only when the operator opted in with an extra destination, so the fixture sets one.
func TestProxyAllowlistEnv_CarriesReferrerHost(t *testing.T) {
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2alpha1.EgressProxySpec{DestinationFQDNs: []string{"artifactory.corp"}},
	}
	env := proxyAllowlistEnv(ep, []string{"ghes.example.com"})
	require.NotEmpty(t, env)
	suffixes := strings.Split(env[0].Value, ",")
	assert.Contains(t, suffixes, "ghes.example.com",
		"a GHES tenant's CONNECT to its own appliance must be permitted")
	assert.Contains(t, suffixes, "api.github.com")
	assert.Contains(t, suffixes, "artifactory.corp")
}
