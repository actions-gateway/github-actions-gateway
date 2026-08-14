package scalesetscope

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

// fakeReader returns a client.Reader preloaded with the given objects.
func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// failingReader errors every read, for the caller-defined error paths.
type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("apiserver unavailable")
}

func gateway(namespace, name, gitHubURL string) *agcv2alpha1.ActionsGateway {
	return &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func scaleSetRS(name, namespace, gw, label string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gw},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolScaleSet,
			RunnerLabels:        []string{label},
		},
	}
}

func classicRS(name, namespace, gw string, labels ...string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gw},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        labels,
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

	t.Run("classic sets and unresolvable gateways", func(t *testing.T) {
		inv, err := Of(ctx, fakeReader(t,
			gateway("tenant-a", "gw-a", "https://github.com/acme"),
			scaleSetRS("scaled", "tenant-a", "gw-a", "linux"),
			classicRS("classic", "tenant-a", "gw-a", "linux"),
			scaleSetRS("orphan", "tenant-b", "gw-missing", "linux"),
		), nil)
		require.NoError(t, err)

		byName := map[string]Claim{}
		for _, c := range inv.Claims {
			byName[c.Name] = c
		}
		require.Len(t, inv.Claims, 2, "the Classic set claims no scale-set name")
		assert.Equal(t, "github.com/acme", byName["scaled"].Scope)
		assert.Empty(t, byName["orphan"].Scope, "an unapplied gateway resolves no scope")
		assert.NotContains(t, byName, "classic")
	})

	t.Run("pending gateway places its own referrers", func(t *testing.T) {
		reader := fakeReader(t, scaleSetRS("waiting", "tenant-b", "gw-b", "linux"))
		inv, err := Of(ctx, reader, &PendingGateway{
			Key:   client.ObjectKey{Namespace: "tenant-b", Name: "gw-b"},
			Scope: "github.com/acme",
		})
		require.NoError(t, err)
		require.Len(t, inv.Claims, 1)
		assert.Equal(t, "github.com/acme", inv.Claims[0].Scope)
		assert.Equal(t, "github.com/acme", inv.ScopeOf("tenant-b", "gw-b"))
	})

	t.Run("read error propagates", func(t *testing.T) {
		_, err := Of(ctx, failingReader{}, nil)
		require.Error(t, err)
	})
}
