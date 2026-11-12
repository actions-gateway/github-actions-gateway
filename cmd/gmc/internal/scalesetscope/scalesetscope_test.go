package scalesetscope

import (
	"context"
	"errors"
	"testing"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReader returns a client.Reader preloaded with the given objects.
func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// failingReader errors every read, for the caller-defined error paths.
type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("apiserver unavailable")
}

func gateway(namespace, name, gitHubURL string) *agcv2.ActionsGateway {
	return &agcv2.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func scaleSetRS(name, namespace, gw, label string) *agcv2.RunnerSet {
	return &agcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2.RunnerSetSpec{
			GatewayRef:   agcv2.ObjectRef{Name: gw},
			RunnerLabels: []string{label},
		},
	}
}

// TestGitHubScope pins the normalization that decides whether two gateways share a
// scale-set namespace at GitHub (Q791). Owner and repo names are case-insensitive
// there, so casing and a trailing slash must not split one scope into two — that
// direction of error would admit the collision the guard exists to reject.
func TestGitHubScope(t *testing.T) {
	same := []struct {
		name string
		urls []string
	}{
		{"casing and trailing slash", []string{
			"https://github.com/acme", "https://github.com/Acme", "https://GitHub.com/ACME/",
		}},
		{"port is dropped, erring toward one scope", []string{
			"https://ghes.corp.example/my-org", "https://ghes.corp.example:8443/my-org",
		}},
		{"repo path casing", []string{
			"https://github.com/acme/Repo", "https://github.com/Acme/repo",
		}},
		// The AGC addresses all three hosts as public GitHub, so a scope keyed on the
		// spelling would let two tenants claim one name in one org (Q1322).
		{"public GitHub aliases", []string{
			"https://github.com/acme", "https://www.github.com/acme", "https://api.github.com/acme",
			"https://WWW.GitHub.com/Acme/", "https://api.github.com:443/acme",
		}},
		// One DNS name, so one scope, for public GitHub and for an appliance.
		{"a trailing dot on the host", []string{
			"https://github.com/acme", "https://github.com./acme", "https://www.github.com./acme",
		}},
		{"a trailing dot on an appliance host", []string{
			"https://ghes.corp.example/my-org", "https://ghes.corp.example./my-org",
		}},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			want := GitHubScope(tc.urls[0])
			require.NotEmpty(t, want)
			for _, u := range tc.urls[1:] {
				assert.Equal(t, want, GitHubScope(u), "%q must share a scope with %q", u, tc.urls[0])
			}
		})
	}

	t.Run("distinct scopes stay distinct", func(t *testing.T) {
		distinct := []string{
			"https://github.com/acme",
			"https://github.com/acme-corp",
			"https://github.com/acme/repo",
			"https://ghes.corp.example/acme",
			// Only the exact alias hosts fold: a GHES appliance under a www or api
			// label is its own Actions service.
			"https://www.ghes.corp.example/acme",
			"https://api.ghes.corp.example/acme",
			"https://www.github.com.corp.example/acme",
		}
		seen := map[string]string{}
		for _, u := range distinct {
			s := GitHubScope(u)
			require.NotEmpty(t, s)
			if prev, dup := seen[s]; dup {
				t.Fatalf("%q and %q collapsed to one scope %q", prev, u, s)
			}
			seen[s] = u
		}
	})

	t.Run("unusable URLs yield no scope", func(t *testing.T) {
		// validation.GitHubURL rejects these at the gateway's own admission; an empty
		// key makes them unresolvable rather than a wildcard that matches everything.
		for _, u := range []string{"", "https://github.com", "https://github.com/", "not a url", "://"} {
			assert.Empty(t, GitHubScope(u), "%q must not yield a scope key", u)
		}
	})
}

// TestGitHubScope_AliasesMatchDeriveAPIBaseURL holds the fold to the hosts the AGC
// actually treats as public GitHub. A host DeriveAPIBaseURL starts collapsing without a
// matching entry here would split one org into two scopes again.
func TestGitHubScope_AliasesMatchDeriveAPIBaseURL(t *testing.T) {
	public := githubapp.DeriveAPIBaseURL("https://" + publicGitHubHost + "/acme")
	for _, host := range publicGitHubAliases {
		assert.Equal(t, public, githubapp.DeriveAPIBaseURL("https://"+host+"/acme"),
			"%s folds to %s, so the AGC must address it as public GitHub", host, publicGitHubHost)
	}
	// The guard may be coarser than the AGC, never finer: a host the AGC addresses as
	// public GitHub must share public GitHub's scope. github.com. is the coarser case,
	// folded here while DeriveAPIBaseURL reads it as an appliance.
	for _, host := range []string{"www.github.com", "api.github.com", "uploads.github.com", "github.com.", "gist.github.com"} {
		addressedAsPublic := githubapp.DeriveAPIBaseURL("https://"+host+"/acme") == public
		folded := GitHubScope("https://"+host+"/acme") == GitHubScope("https://"+publicGitHubHost+"/acme")
		if addressedAsPublic {
			assert.True(t, folded, "%s is addressed as public GitHub, so it must share its scope", host)
		}
	}
	for _, host := range []string{"uploads.github.com", "gist.github.com"} {
		assert.NotEqual(t, GitHubScope("https://"+publicGitHubHost+"/acme"), GitHubScope("https://"+host+"/acme"),
			"%s is not public GitHub's API host and keeps its own scope", host)
	}
}

func TestPublicGitHubAlias(t *testing.T) {
	for _, host := range []string{"www.github.com", "api.github.com", "WWW.GitHub.com"} {
		assert.True(t, PublicGitHubAlias(host), host)
	}
	// github.com is the name the aliases fold to, not an alias, and a host that only
	// contains one is somebody else's.
	for _, host := range []string{"github.com", "www.github.com.evil.example", "ghes.example.com", ""} {
		assert.False(t, PublicGitHubAlias(host), host)
	}
}

// TestGitHubScopeAsWritten pins the key an unchanged claim is compared by: every
// normalization GitHubScope applies except the alias fold and the trailing-dot drop.
func TestGitHubScopeAsWritten(t *testing.T) {
	assert.NotEqual(t, GitHubScopeAsWritten("https://github.com/acme"), GitHubScopeAsWritten("https://github.com./acme"))
	assert.Equal(t, "www.github.com/acme", GitHubScopeAsWritten("https://WWW.GitHub.com:443/Acme/"))
	assert.NotEqual(t, GitHubScopeAsWritten("https://github.com/acme"), GitHubScopeAsWritten("https://www.github.com/acme"))
	assert.Equal(t, GitHubScope("https://ghes.corp.example/acme"), GitHubScopeAsWritten("https://ghes.corp.example/acme"),
		"a host with no alias has one key")
	for _, u := range []string{"", "https://www.github.com", "not a url"} {
		assert.Empty(t, GitHubScopeAsWritten(u), "%q must not yield a scope key", u)
	}
}

// TestClaimCollidesAsWritten pins the narrower test an unchanged claim is held to: a
// pair split only by a public GitHub alias collides for a new claim and not for one an
// update leaves alone, while a pair on one spelling collides either way.
func TestClaimCollidesAsWritten(t *testing.T) {
	claim := func(ns, url string) Claim {
		return Claim{
			Namespace: ns, Name: "set", GatewayRef: "gw", Label: "linux",
			Scope: GitHubScope(url), ScopeAsWritten: GitHubScopeAsWritten(url),
		}
	}
	plain := claim("tenant-a", "https://github.com/acme")
	alias := claim("tenant-b", "https://www.github.com/acme")
	same := claim("tenant-c", "https://github.com/acme")

	assert.True(t, plain.CollidesWith(alias), "the fold makes the alias pair one scope")
	assert.False(t, plain.CollidesAsWritten(alias), "as written they are two")
	assert.True(t, plain.CollidesWith(same))
	assert.True(t, plain.CollidesAsWritten(same), "one spelling collides under either test")

	sibling := plain
	sibling.Name = "other"
	assert.True(t, plain.CollidesAsWritten(sibling), "the same gateway is one scope whatever it resolves to")
}

// TestClaimCollidesWith pins the two shapes that mean one scale set: the same gateway
// object (whose scope is shared whatever it resolves to, so this holds before the
// gateway is applied) and the same resolved GitHub scope across namespaces.
func TestClaimCollidesWith(t *testing.T) {
	claim := func(ns, name, gw, label, scope string) Claim {
		return Claim{Namespace: ns, Name: name, GatewayRef: gw, Label: label, Scope: scope}
	}
	const acme = "github.com/acme"

	t.Run("same gateway, no resolvable scope", func(t *testing.T) {
		a := claim("tenant", "a", "gw", "linux", "")
		b := claim("tenant", "b", "gw", "linux", "")
		assert.True(t, a.CollidesWith(b))
	})

	t.Run("same scope, different namespaces", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "linux", acme)
		assert.True(t, a.CollidesWith(b))
	})

	t.Run("different labels never collide", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "windows", acme)
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("labels differing only by case are distinct names", func(t *testing.T) {
		// The AGC refuses to adopt a set whose name differs from its own only by case
		// (scaleset.Client.GetRunnerScaleSetByName), so the pair can never drive one
		// scale set; which of them a runs-on reaches is the runner group's question,
		// as it is for any overlapping label (Q1325).
		a := claim("tenant-a", "a", "gw-a", "ci", acme)
		b := claim("tenant-b", "b", "gw-b", "CI", acme)
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("different scopes do not collide", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "linux", "github.com/other")
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("unresolvable scopes do not match each other", func(t *testing.T) {
		// An empty key means "unknown", not "the same unknown" — two sets under
		// different unapplied gateways must not be treated as one scope.
		a := claim("tenant-a", "a", "gw-a", "linux", "")
		b := claim("tenant-b", "b", "gw-b", "linux", "")
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("same gateway name in another namespace is a different gateway", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw", "linux", "")
		b := claim("tenant-b", "b", "gw", "linux", "")
		assert.False(t, a.CollidesWith(b), "gatewayRef is same-namespace only")
	})
}

// TestOf checks the inventory attributes scopes through gatewayRef, skips claims it
// cannot place (§H.7), and lets a pending gateway place its own referrers during that
// gateway's admission.
func TestOf(t *testing.T) {
	ctx := context.Background()

	t.Run("unresolvable gateways", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			gateway("tenant-a", "gw-a", "https://github.com/acme"),
			scaleSetRS("scaled", "tenant-a", "gw-a", "linux"),
			scaleSetRS("orphan", "tenant-b", "gw-missing", "linux"),
		), nil)
		require.NoError(t, err)

		byName := map[string]Claim{}
		for _, c := range inv.Claims {
			byName[c.Name] = c
		}
		require.Len(t, inv.Claims, 2)
		assert.Equal(t, "github.com/acme", byName["scaled"].Scope)
		assert.Empty(t, byName["orphan"].Scope, "an unapplied gateway resolves no scope")
	})

	t.Run("an alias gateway carries both keys", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			gateway("tenant-a", "gw-a", "https://www.github.com/acme"),
			scaleSetRS("scaled", "tenant-a", "gw-a", "linux"),
		), nil)
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Equal(t, "github.com/acme", inv.Claims[0].Scope)
		assert.Equal(t, "www.github.com/acme", inv.Claims[0].ScopeAsWritten)
		assert.Equal(t, "www.github.com/acme", inv.ScopeAsWrittenOf("tenant-a", "gw-a"))
	})

	t.Run("pending gateway places its own referrers", func(t *testing.T) {
		reader := fakeReader(t, scaleSetRS("waiting", "tenant-b", "gw-b", "linux"))
		inv, err := Of(ctx, reader, &PendingGateway{
			Key:       client.ObjectKey{Namespace: "tenant-b", Name: "gw-b"},
			GitHubURL: "https://api.github.com/acme",
		})
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Equal(t, "github.com/acme", inv.Claims[0].Scope)
		assert.Equal(t, "api.github.com/acme", inv.Claims[0].ScopeAsWritten)
		assert.Equal(t, "github.com/acme", inv.ScopeOf("tenant-b", "gw-b"))
	})

	t.Run("read error propagates", func(t *testing.T) {
		_, err := Of(ctx, failingReader{}, nil)
		require.Error(t, err)
	})
}
