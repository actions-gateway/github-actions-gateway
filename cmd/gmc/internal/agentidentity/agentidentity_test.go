package agentidentity

import (
	"testing"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func v2Gateway(namespace, name, gitHubURL string) agcv2.ActionsGateway {
	return agcv2.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func runnerSet(namespace, name, gateway string) agcv2.RunnerSet {
	return agcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2.RunnerSetSpec{
			GatewayRef:   agcv2.ObjectRef{Name: gateway},
			RunnerLabels: []string{"linux"},
		},
	}
}

// TestClaimCollidesWith pins the two boundaries: the agent Secret is namespaced, so
// one namespace collides whatever GitHub it reaches, and the registered runner name
// is scoped to the GitHub org/repo, so two namespaces collide when their gateways
// bind one scope.
func TestClaimCollidesWith(t *testing.T) {
	const acme = "github.com/acme"
	claim := func(ns, name, stem, scope string) Claim {
		return Claim{Namespace: ns, Kind: KindRunnerSet, Name: name, Stem: stem, Scope: scope}
	}

	t.Run("one namespace collides without any resolvable scope", func(t *testing.T) {
		// The agent Secret half needs no GitHub binding at all.
		a := claim("tenant", "build", "rs-build", "")
		b := claim("tenant", "other", "rs-build", "")
		assert.True(t, a.CollidesWith(b))
	})

	t.Run("same stem across namespaces collides only within one GitHub scope", func(t *testing.T) {
		a := claim("tenant-a", "build", "rs-build", acme)
		same := claim("tenant-b", "build", "rs-build", acme)
		other := claim("tenant-b", "build", "rs-build", "github.com/other")
		assert.True(t, a.CollidesWith(same), "one org, one runner-name space")
		assert.False(t, a.CollidesWith(other))
	})

	t.Run("an unresolvable scope is unknown, not a wildcard", func(t *testing.T) {
		a := claim("tenant-a", "build", "rs-build", "")
		b := claim("tenant-b", "build", "rs-build", "")
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("distinct stems never collide", func(t *testing.T) {
		a := claim("tenant", "build", "rs-build", acme)
		b := claim("tenant", "test", "rs-test", acme)
		assert.False(t, a.CollidesWith(b))
	})
}

// TestCollisionSkipsSelf pins that a stored object does not reject itself on update —
// the object under admission appears in the inventory it is checked against.
func TestCollisionSkipsSelf(t *testing.T) {
	self := Claim{Namespace: "tenant-a", Kind: KindRunnerSet, Name: "build", Stem: "rs-build", Scope: "github.com/acme"}
	inv := Inventory{Claims: []Claim{self}}
	assert.Nil(t, inv.Collision(self))

	inv.Claims = append(inv.Claims, Claim{
		Namespace: "tenant-b", Kind: KindRunnerSet, Name: "build", Stem: "rs-build", Scope: "github.com/acme",
	})
	holder := inv.Collision(self)
	require.NotNil(t, holder)
	assert.Equal(t, "tenant-b", holder.Namespace)
}

func TestFromLists(t *testing.T) {
	t.Run("claims are scoped through their gateways", func(t *testing.T) {
		inv := FromLists(
			[]agcv2.ActionsGateway{
				v2Gateway("tenant-a", "gw1", "https://github.com/acme"),
				v2Gateway("tenant-b", "gw2", "https://github.com/Acme/"),
			},
			[]agcv2.RunnerSet{runnerSet("tenant-a", "build", "gw1"), runnerSet("tenant-b", "build", "gw2")},
		)
		require.Len(t, inv.Claims, 2)
		for _, c := range inv.Claims {
			assert.Equal(t, KindRunnerSet, c.Kind)
			assert.Equal(t, "rs-build", c.Stem)
			assert.Equal(t, "github.com/acme", c.Scope,
				"casing and a trailing slash must not split one org into two scopes")
		}
		assert.True(t, inv.Claims[0].CollidesWith(inv.Claims[1]),
			"the cross-namespace runner-name half is what makes this a collision")
	})

	t.Run("a RunnerSet whose gateway is not stored resolves no scope", func(t *testing.T) {
		inv := FromLists(nil, []agcv2.RunnerSet{runnerSet("tenant", "build", "gw-missing")})
		require.Len(t, inv.Claims, 1)
		assert.Empty(t, inv.Claims[0].Scope)
	})

	t.Run("scope lookups", func(t *testing.T) {
		inv := FromLists([]agcv2.ActionsGateway{v2Gateway("tenant", "gw2", "https://ghes.corp.example/team")}, nil)
		assert.Equal(t, "ghes.corp.example/team", inv.V2Scope("tenant", "gw2"))
		assert.Empty(t, inv.V2Scope("tenant", "absent"))
	})
}
