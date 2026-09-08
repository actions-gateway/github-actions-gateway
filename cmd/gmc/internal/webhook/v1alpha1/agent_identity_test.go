package v1alpha1

import (
	"context"
	"strings"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func identityValidator(t *testing.T, objs ...client.Object) *ActionsGatewayCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv1alpha1.AddToScheme(scheme))
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	require.NoError(t, gmcv1alpha1.AddToScheme(scheme))
	v := NewActionsGatewayCustomValidator("gmc-system", nil)
	v.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return v
}

// identityGateway builds a v1 gateway with one runnerGroups entry whose first label
// is firstLabel, so the derived RunnerGroup name is under the caller's control.
func identityGateway(namespace, name, gitHubURL, firstLabel string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubURL: gitHubURL,
			RunnerGroups: []agcv1alpha1.RunnerGroupSpec{
				{RunnerLabels: []string{firstLabel}},
			},
		},
	}
}

func identityV2Gateway(namespace, name, gitHubURL string) *agcv2alpha1.ActionsGateway {
	return &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func identityRunnerSet(namespace, name, gateway string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:   agcv2alpha1.ObjectRef{Name: gateway},
			RunnerLabels: []string{"linux"},
		},
	}
}

// TestGatewayAgentIdentityUniqueness drives the reverse direction of the Q1011 guard.
// A RunnerGroup carries no name of its own: the GMC derives it from the gateway, so
// the gateway write is where a collision can still be refused.
func TestGatewayAgentIdentityUniqueness(t *testing.T) {
	ctx := context.Background()
	const acme = "https://github.com/acme"

	// A labeled entry's derived name is "<gateway>-<label>-<7 hex>", because
	// apinames.Segment always appends a hash. So the reachable collision needs a
	// gateway whose name starts "rs" AND a RunnerSet named for the rest of the
	// derived name — narrower than a RunnerGroup literally named "rs-<x>", and the
	// shape the guard has to catch all the same.
	derived := apinames.RunnerGroupName("rs", []string{"build"}, 0)
	require.True(t, strings.HasPrefix(derived, apinames.RunnerSetStemPrefix),
		"the fixture needs a derived name under the RunnerSet stem prefix, got %q", derived)
	// collidingSet is the RunnerSet name whose stem equals the derived name.
	collidingSet := strings.TrimPrefix(derived, apinames.RunnerSetStemPrefix)
	require.Equal(t, derived, apinames.RunnerSetAgentStem(collidingSet))

	t.Run("a derived RunnerGroup name colliding in-namespace is rejected", func(t *testing.T) {
		v := identityValidator(t,
			identityV2Gateway("tenant", "gw2", acme),
			identityRunnerSet("tenant", collidingSet, "gw2"),
		)
		_, err := v.ValidateCreate(ctx, identityGateway("tenant", "rs", acme, "build"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.runnerGroups[0]",
			"the entry has no name field, so the message must say which entry to change")
		assert.Contains(t, err.Error(), derived)
		assert.Contains(t, err.Error(), "RunnerSet")
	})

	t.Run("the same collision across namespaces under one org is rejected", func(t *testing.T) {
		v := identityValidator(t,
			identityV2Gateway("tenant-b", "gw2", "https://github.com/ACME"),
			identityRunnerSet("tenant-b", collidingSet, "gw2"),
		)
		_, err := v.ValidateCreate(ctx, identityGateway("tenant-a", "rs", acme, "build"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "github.com/acme")
		assert.NotContains(t, err.Error(), "tenant-b", "another tenant's namespace must not be disclosed")
	})

	t.Run("a different GitHub scope is not a collision", func(t *testing.T) {
		v := identityValidator(t,
			identityV2Gateway("tenant-b", "gw2", "https://github.com/other"),
			identityRunnerSet("tenant-b", collidingSet, "gw2"),
		)
		_, err := v.ValidateCreate(ctx, identityGateway("tenant-a", "rs", acme, "build"))
		assert.NoError(t, err)
	})

	t.Run("a non-colliding derived name admits", func(t *testing.T) {
		v := identityValidator(t,
			identityV2Gateway("tenant", "gw2", acme),
			identityRunnerSet("tenant", collidingSet, "gw2"),
		)
		// Gateway "ci" derives "ci-build-<hash>"; no RunnerSet stem can equal that,
		// since every RunnerSet stem starts "rs-".
		_, err := v.ValidateCreate(ctx, identityGateway("tenant", "ci", acme, "build"))
		assert.NoError(t, err)
	})

	t.Run("an update is not rejected against the gateway's own RunnerGroups", func(t *testing.T) {
		stored := identityGateway("tenant", "rs", acme, "build")
		rg := &agcv1alpha1.RunnerGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name: derived, Namespace: "tenant",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: gmcv1alpha1.GroupVersion.String(),
					Kind:       "ActionsGateway", Name: "rs", UID: "uid",
				}},
			},
			Spec: agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"build"}},
		}
		v := identityValidator(t, stored, rg)
		updated := stored.DeepCopy()
		updated.Spec.RunnerGroups[0].RunnerLabels = []string{"build", "x64"}
		_, err := v.ValidateUpdate(ctx, stored, updated)
		assert.NoError(t, err, "the stored RunnerGroup is the generation this write replaces")
	})

	t.Run("a gateway with no runnerGroups skips the check", func(t *testing.T) {
		v := identityValidator(t)
		ag := &gmcv1alpha1.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "tenant"},
			Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubURL: acme},
		}
		_, err := v.ValidateCreate(ctx, ag)
		assert.NoError(t, err)
	})
}
