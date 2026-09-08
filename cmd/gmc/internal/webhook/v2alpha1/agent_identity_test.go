package v2alpha1

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func identityV1Gateway(namespace, name, gitHubURL string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func identityV2Gateway(namespace, name, gitHubURL string) *agcv2alpha1.ActionsGateway {
	return &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

func identityRunnerGroup(namespace, name, owner string) *agcv1alpha1.RunnerGroup {
	return &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gmcv1alpha1.GroupVersion.String(),
				Kind:       "ActionsGateway",
				Name:       owner,
				UID:        "uid",
			}},
		},
		Spec: agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"linux"}},
	}
}

// TestRunnerSetAgentIdentityUniqueness drives the forward direction of the Q1011
// guard: a RunnerSet named "<x>" is rejected when a RunnerGroup named "rs-<x>"
// already holds the stem they share.
func TestRunnerSetAgentIdentityUniqueness(t *testing.T) {
	ctx := context.Background()
	const acme = "https://github.com/acme"

	t.Run("RunnerGroup rs-<x> in the same namespace rejects RunnerSet <x>", func(t *testing.T) {
		v := runnerSetValidatorWith(t,
			identityV1Gateway("tenant", "gw1", acme),
			identityV2Gateway("tenant", "gw2", acme),
			identityRunnerGroup("tenant", "rs-build", "gw1"),
		)
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant", "gw2", "linux"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"rs-build"`, "the message must name the contested stem")
		assert.Contains(t, err.Error(), "RunnerGroup", "a same-namespace holder is named for the tenant who owns both")
	})

	t.Run("the same collision across namespaces under one org is rejected", func(t *testing.T) {
		// Only the GitHub runner-name half collides here: the agent Secrets are in
		// different namespaces. Appendix E.6 shards one org this way deliberately.
		v := runnerSetValidatorWith(t,
			identityV1Gateway("tenant-a", "gw1", acme),
			identityV2Gateway("tenant-b", "gw2", "https://GitHub.com/Acme/"),
			identityRunnerGroup("tenant-a", "rs-build", "gw1"),
		)
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant-b", "gw2", "linux"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "github.com/acme", "the message names the scope, not the other tenant")
		assert.NotContains(t, err.Error(), "tenant-a", "another tenant's namespace must not be disclosed")
	})

	t.Run("a different GitHub scope is not a collision", func(t *testing.T) {
		v := runnerSetValidatorWith(t,
			identityV1Gateway("tenant-a", "gw1", acme),
			identityV2Gateway("tenant-b", "gw2", "https://github.com/other"),
			identityRunnerGroup("tenant-a", "rs-build", "gw1"),
		)
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant-b", "gw2", "linux"))
		assert.NoError(t, err)
	})

	t.Run("a same-named RunnerGroup does not collide", func(t *testing.T) {
		// Q466's whole purpose: "build" and "build" coexist through a migration.
		v := runnerSetValidatorWith(t,
			identityV1Gateway("tenant", "gw1", acme),
			identityV2Gateway("tenant", "gw2", acme),
			identityRunnerGroup("tenant", "build", "gw1"),
		)
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant", "gw2", "linux"))
		assert.NoError(t, err)
	})

	t.Run("two RunnerSets of one name under one org collide", func(t *testing.T) {
		v := runnerSetValidatorWith(t,
			identityV2Gateway("tenant-a", "gw", acme),
			identityV2Gateway("tenant-b", "gw", acme),
			classicRS("build", "tenant-a", "gw", "linux"),
		)
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant-b", "gw", "linux"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"rs-build"`)
	})

	t.Run("an update does not reject the set against its own stored copy", func(t *testing.T) {
		stored := classicRS("build", "tenant", "gw2", "linux")
		v := runnerSetValidatorWith(t, identityV2Gateway("tenant", "gw2", acme), stored)
		updated := classicRS("build", "tenant", "gw2", "linux", "x64")
		_, err := v.ValidateUpdate(ctx, stored, updated)
		assert.NoError(t, err)
	})

	t.Run("a nil reader disables the check", func(t *testing.T) {
		v := &RunnerSetCustomValidator{}
		_, err := v.ValidateCreate(ctx, classicRS("build", "tenant", "gw2", "linux"))
		assert.NoError(t, err)
	})
}
