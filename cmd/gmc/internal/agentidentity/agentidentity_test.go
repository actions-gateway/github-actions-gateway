package agentidentity

import (
	"context"
	"errors"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv1alpha1.AddToScheme(scheme))
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	require.NoError(t, gmcv1alpha1.AddToScheme(scheme))
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
	case *gmcv1alpha1.ActionsGatewayList:
		if f.on == "v1gateways" {
			return errors.New("apiserver unavailable")
		}
	case *agcv2alpha1.ActionsGatewayList:
		if f.on == "v2gateways" {
			return errors.New("apiserver unavailable")
		}
	case *agcv1alpha1.RunnerGroupList:
		if f.on == "runnergroups" {
			return errors.New("apiserver unavailable")
		}
	case *agcv2alpha1.RunnerSetList:
		if f.on == "runnersets" {
			return errors.New("apiserver unavailable")
		}
	}
	return f.Reader.List(ctx, list, opts...)
}

func v1Gateway(namespace, name, gitHubURL string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func v2Gateway(namespace, name, gitHubURL string) *agcv2alpha1.ActionsGateway {
	return &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

// runnerGroup builds a RunnerGroup owned by the named v1 gateway, as the GMC
// materializes them (applyManagedChild stamps a controller owner reference).
func runnerGroup(namespace, name, owner string) *agcv1alpha1.RunnerGroup {
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"linux"}},
	}
	if owner != "" {
		rg.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: gmcv1alpha1.GroupVersion.String(),
			Kind:       "ActionsGateway",
			Name:       owner,
			UID:        types.UID("uid-" + owner),
		}}
	}
	return rg
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
	claim := func(ns, kind, name, stem, scope string) Claim {
		return Claim{Namespace: ns, Kind: kind, Name: name, Stem: stem, Scope: scope}
	}

	t.Run("cross-derivation collision in one namespace", func(t *testing.T) {
		rg := claim("tenant", KindRunnerGroup, "rs-build", "rs-build", acme)
		rs := claim("tenant", KindRunnerSet, "build", "rs-build", acme)
		assert.True(t, rs.CollidesWith(rg))
		assert.True(t, rg.CollidesWith(rs))
	})

	t.Run("one namespace collides without any resolvable scope", func(t *testing.T) {
		// The agent Secret half needs no GitHub binding at all.
		rg := claim("tenant", KindRunnerGroup, "rs-build", "rs-build", "")
		rs := claim("tenant", KindRunnerSet, "build", "rs-build", "")
		assert.True(t, rs.CollidesWith(rg))
	})

	t.Run("same stem across namespaces collides only within one GitHub scope", func(t *testing.T) {
		a := claim("tenant-a", KindRunnerSet, "build", "rs-build", acme)
		same := claim("tenant-b", KindRunnerSet, "build", "rs-build", acme)
		other := claim("tenant-b", KindRunnerSet, "build", "rs-build", "github.com/other")
		assert.True(t, a.CollidesWith(same), "one org, one runner-name space")
		assert.False(t, a.CollidesWith(other))
	})

	t.Run("an unresolvable scope is unknown, not a wildcard", func(t *testing.T) {
		a := claim("tenant-a", KindRunnerSet, "build", "rs-build", "")
		b := claim("tenant-b", KindRunnerSet, "build", "rs-build", "")
		assert.False(t, a.CollidesWith(b))
	})

	t.Run("distinct stems never collide", func(t *testing.T) {
		rg := claim("tenant", KindRunnerGroup, "build", "build", acme)
		rs := claim("tenant", KindRunnerSet, "build", "rs-build", acme)
		assert.False(t, rs.CollidesWith(rg), "Q466 keeps same-named CRs apart")
	})
}

// TestCollisionSkipsSelf pins that a stored object does not reject itself on update —
// the object under admission appears in the inventory it is checked against.
func TestCollisionSkipsSelf(t *testing.T) {
	self := Claim{Namespace: "tenant", Kind: KindRunnerSet, Name: "build", Stem: "rs-build", Scope: "github.com/acme"}
	inv := Inventory{Claims: []Claim{self}}
	assert.Nil(t, inv.Collision(self))

	inv.Claims = append(inv.Claims, Claim{
		Namespace: "tenant", Kind: KindRunnerGroup, Name: "rs-build", Stem: "rs-build", Scope: "github.com/acme",
	})
	holder := inv.Collision(self)
	require.NotNil(t, holder)
	assert.Equal(t, KindRunnerGroup, holder.Kind)
	assert.Equal(t, "rs-build", holder.Name)
}

func TestOf(t *testing.T) {
	ctx := context.Background()

	t.Run("claims from both derivations, scoped through their gateways", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			v1Gateway("tenant-a", "gw1", "https://github.com/acme"),
			v2Gateway("tenant-b", "gw2", "https://github.com/Acme/"),
			runnerGroup("tenant-a", "rs-build", "gw1"),
			runnerSet("tenant-b", "build", "gw2"),
		), nil)
		require.NoError(t, err)
		require.Len(t, inv.Claims, 2)

		byKind := map[string]Claim{}
		for _, c := range inv.Claims {
			byKind[c.Kind] = c
		}
		assert.Equal(t, "rs-build", byKind[KindRunnerGroup].Stem)
		assert.Equal(t, "rs-build", byKind[KindRunnerSet].Stem)
		assert.Equal(t, "github.com/acme", byKind[KindRunnerGroup].Scope)
		assert.Equal(t, "github.com/acme", byKind[KindRunnerSet].Scope,
			"casing and a trailing slash must not split one org into two scopes")
		assert.True(t, byKind[KindRunnerSet].CollidesWith(byKind[KindRunnerGroup]),
			"the cross-namespace runner-name half is what makes this a collision")
	})

	t.Run("a RunnerSet whose gateway is not stored resolves no scope", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t, runnerSet("tenant", "build", "gw-missing")), nil)
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Empty(t, inv.Claims[0].Scope)
	})

	t.Run("an unowned RunnerGroup falls back to the namespace's sole gateway", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			v1Gateway("tenant", "gw1", "https://github.com/acme"),
			runnerGroup("tenant", "orphan", ""),
		), nil)
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Equal(t, "github.com/acme", inv.Claims[0].Scope)
	})

	t.Run("a pending gateway replaces its own stored RunnerGroups", func(t *testing.T) {
		// An update must not be rejected against the generation it replaces.
		reader := fakeReader(t,
			v1Gateway("tenant", "gw1", "https://github.com/acme"),
			runnerGroup("tenant", "gw1-linux", "gw1"),
			runnerSet("other", "unrelated", "gw-missing"),
		)
		inv, err := Of(ctx, reader, &PendingGateway{
			Key:   client.ObjectKey{Namespace: "tenant", Name: "gw1"},
			Scope: "github.com/acme",
		})
		require.NoError(t, err)
		for _, c := range inv.Claims {
			assert.NotEqual(t, "gw1-linux", c.Name, "the gateway's own stored groups are the generation being replaced")
		}
		assert.Equal(t, "github.com/acme", inv.V1Scope("tenant", "gw1"),
			"a gateway under admission is not stored, so its scope comes from the pending record")
	})

	t.Run("scope lookups", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			v1Gateway("tenant", "gw1", "https://github.com/acme"),
			v2Gateway("tenant", "gw2", "https://ghes.corp.example/team"),
		), nil)
		require.NoError(t, err)
		assert.Equal(t, "github.com/acme", inv.V1Scope("tenant", "gw1"))
		assert.Equal(t, "ghes.corp.example/team", inv.V2Scope("tenant", "gw2"))
		assert.Empty(t, inv.V2Scope("tenant", "absent"))
	})

	t.Run("every List failing closes the inventory", func(t *testing.T) {
		base := fakeReader(t, v1Gateway("tenant", "gw1", "https://github.com/acme"))
		for _, kind := range []string{"v1gateways", "v2gateways", "runnergroups", "runnersets"} {
			_, err := Of(ctx, failingReader{Reader: base, on: kind}, nil)
			assert.Error(t, err, "a failed %s List must not yield an inventory", kind)
		}
	})
}
