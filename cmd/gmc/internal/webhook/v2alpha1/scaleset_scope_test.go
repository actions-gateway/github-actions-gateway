package v2alpha1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			want := gitHubScope(tc.urls[0])
			require.NotEmpty(t, want)
			for _, u := range tc.urls[1:] {
				assert.Equal(t, want, gitHubScope(u), "%q must share a scope with %q", u, tc.urls[0])
			}
		})
	}

	t.Run("distinct scopes stay distinct", func(t *testing.T) {
		distinct := []string{
			"https://github.com/acme",
			"https://github.com/acme-corp",
			"https://github.com/acme/repo",
			"https://ghes.corp.example/acme",
		}
		seen := map[string]string{}
		for _, u := range distinct {
			s := gitHubScope(u)
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
			assert.Empty(t, gitHubScope(u), "%q must not yield a scope key", u)
		}
	})
}

// TestScaleSetClaimCollidesWith pins the two shapes that mean one scale set: the same
// gateway object (whose scope is shared whatever it resolves to, so this holds before
// the gateway is applied) and the same resolved GitHub scope across namespaces.
func TestScaleSetClaimCollidesWith(t *testing.T) {
	claim := func(ns, name, gw, label, scope string) scaleSetClaim {
		return scaleSetClaim{namespace: ns, name: name, gatewayRef: gw, label: label, scope: scope}
	}
	const acme = "github.com/acme"

	t.Run("same gateway, no resolvable scope", func(t *testing.T) {
		a := claim("tenant", "a", "gw", "linux", "")
		b := claim("tenant", "b", "gw", "linux", "")
		assert.True(t, a.collidesWith(b))
	})

	t.Run("same scope, different namespaces", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "linux", acme)
		assert.True(t, a.collidesWith(b))
	})

	t.Run("different labels never collide", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "windows", acme)
		assert.False(t, a.collidesWith(b))
	})

	t.Run("different scopes do not collide", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw-a", "linux", acme)
		b := claim("tenant-b", "b", "gw-b", "linux", "github.com/other")
		assert.False(t, a.collidesWith(b))
	})

	t.Run("unresolvable scopes do not match each other", func(t *testing.T) {
		// An empty key means "unknown", not "the same unknown" — two sets under
		// different unapplied gateways must not be treated as one scope.
		a := claim("tenant-a", "a", "gw-a", "linux", "")
		b := claim("tenant-b", "b", "gw-b", "linux", "")
		assert.False(t, a.collidesWith(b))
	})

	t.Run("same gateway name in another namespace is a different gateway", func(t *testing.T) {
		a := claim("tenant-a", "a", "gw", "linux", "")
		b := claim("tenant-b", "b", "gw", "linux", "")
		assert.False(t, a.collidesWith(b), "gatewayRef is same-namespace only")
	})
}

// TestScaleSetInventoryOf checks the inventory attributes scopes through gatewayRef,
// skips claims it cannot place (§H.7), and lets a pending gateway place its own
// referrers during that gateway's admission.
func TestScaleSetInventoryOf(t *testing.T) {
	ctx := context.Background()

	t.Run("classic sets and unresolvable gateways", func(t *testing.T) {
		inv, err := scaleSetInventoryOf(ctx, fakeReader(t,
			v2Gateway("tenant-a", "gw-a", "https://github.com/acme", ""),
			scaleSetRS("scaled", "tenant-a", "gw-a", "linux"),
			classicRS("classic", "tenant-a", "gw-a", "linux"),
			scaleSetRS("orphan", "tenant-b", "gw-missing", "linux"),
		), nil)
		require.NoError(t, err)

		byName := map[string]scaleSetClaim{}
		for _, c := range inv.claims {
			byName[c.name] = c
		}
		require.Len(t, inv.claims, 2, "the Classic set claims no scale-set name")
		assert.Equal(t, "github.com/acme", byName["scaled"].scope)
		assert.Empty(t, byName["orphan"].scope, "an unapplied gateway resolves no scope")
		assert.NotContains(t, byName, "classic")
	})

	t.Run("pending gateway places its own referrers", func(t *testing.T) {
		reader := fakeReader(t, scaleSetRS("waiting", "tenant-b", "gw-b", "linux"))
		inv, err := scaleSetInventoryOf(ctx, reader, &pendingGateway{
			key:   client.ObjectKey{Namespace: "tenant-b", Name: "gw-b"},
			scope: "github.com/acme",
		})
		require.NoError(t, err)
		require.Len(t, inv.claims, 1)
		assert.Equal(t, "github.com/acme", inv.claims[0].scope)
		assert.Equal(t, "github.com/acme", inv.scopeOf("tenant-b", "gw-b"))
	})

	t.Run("read error propagates", func(t *testing.T) {
		_, err := scaleSetInventoryOf(ctx, failingReader{}, nil)
		require.Error(t, err)
	})
}
