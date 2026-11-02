package agentidentity

import (
	"context"
	"errors"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// failingReader errors the List of one kind and delegates the rest, so each
// fail-closed path can be driven independently.
type failingReader struct {
	client.Reader
	on string
}

func (f failingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	switch list.(type) {
	case *agcv2alpha1.ActionsGatewayList:
		if f.on == "gateways" {
			return errors.New("apiserver unavailable")
		}
	case *agcv2alpha1.RunnerSetList:
		if f.on == "runnersets" {
			return errors.New("apiserver unavailable")
		}
	}
	return f.Reader.List(ctx, list, opts...)
}

func v2Gateway(namespace, name, gitHubURL string) *agcv2alpha1.ActionsGateway {
	return &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func runnerSet(namespace, name, gateway string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:   agcv2alpha1.ObjectRef{Name: gateway},
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
		b := claim("tenant", "build-2", "rs-build", "")
		assert.True(t, a.CollidesWith(b))
		assert.True(t, b.CollidesWith(a))
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
	self := Claim{Namespace: "tenant", Kind: KindRunnerSet, Name: "build", Stem: "rs-build", Scope: "github.com/acme"}
	inv := Inventory{Claims: []Claim{self}}
	assert.Nil(t, inv.Collision(self))

	inv.Claims = append(inv.Claims, Claim{
		Namespace: "other", Kind: KindRunnerSet, Name: "build", Stem: "rs-build", Scope: "github.com/acme",
	})
	holder := inv.Collision(self)
	require.NotNil(t, holder)
	assert.Equal(t, "other", holder.Namespace)
}

func TestOf(t *testing.T) {
	ctx := context.Background()

	t.Run("claims are scoped through their gateways", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			v2Gateway("tenant-a", "gw1", "https://github.com/acme"),
			v2Gateway("tenant-b", "gw2", "https://github.com/Acme/"),
			runnerSet("tenant-a", "build", "gw1"),
			runnerSet("tenant-b", "build", "gw2"),
		))
		require.NoError(t, err)
		require.Len(t, inv.Claims, 2)
		for _, c := range inv.Claims {
			assert.Equal(t, "rs-build", c.Stem)
			assert.Equal(t, "github.com/acme", c.Scope,
				"casing and a trailing slash must not split one org into two scopes")
		}
		assert.True(t, inv.Claims[0].CollidesWith(inv.Claims[1]),
			"the cross-namespace runner-name half is what makes this a collision")
	})

	t.Run("a RunnerSet whose gateway is not stored resolves no scope", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t, runnerSet("tenant", "build", "gw-missing")))
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Empty(t, inv.Claims[0].Scope)
	})

	t.Run("scope lookups", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t, v2Gateway("tenant", "gw2", "https://ghes.corp.example/team")))
		require.NoError(t, err)
		assert.Equal(t, "ghes.corp.example/team", inv.V2Scope("tenant", "gw2"))
		assert.Empty(t, inv.V2Scope("tenant", "absent"))
	})

	t.Run("every List failing closes the inventory", func(t *testing.T) {
		base := fakeReader(t, v2Gateway("tenant", "gw1", "https://github.com/acme"))
		for _, kind := range []string{"gateways", "runnersets"} {
			_, err := Of(ctx, failingReader{Reader: base, on: kind})
			assert.Error(t, err, "a failed %s List must not yield an inventory", kind)
		}
	})
}
