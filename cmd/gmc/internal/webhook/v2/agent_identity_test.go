package v2

import (
	"context"
	"testing"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func identityV2Gateway(namespace, name, gitHubURL string) *agcv2.ActionsGateway {
	return &agcv2.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

// TestRunnerSetAgentIdentityUniqueness drives the Q1011 guard: a RunnerSet is
// rejected when another RunnerSet already holds the agent-identity stem it derives
// in the same GitHub scope.
func TestRunnerSetAgentIdentityUniqueness(t *testing.T) {
	ctx := context.Background()
	const acme = "https://github.com/acme"

	t.Run("the same name across namespaces under one org is rejected", func(t *testing.T) {
		// Only the GitHub runner-name half collides here: the agent Secrets are in
		// different namespaces. Appendix E.6 shards one org this way deliberately.
		v := runnerSetValidatorWith(t,
			identityV2Gateway("tenant-a", "gw1", acme),
			identityV2Gateway("tenant-b", "gw2", "https://GitHub.com/Acme/"),
			labelledRS("build", "tenant-a", "gw1", "linux"),
		)
		_, err := v.ValidateCreate(ctx, labelledRS("build", "tenant-b", "gw2", "windows"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"rs-build"`, "the message must name the contested stem")
		assert.Contains(t, err.Error(), "github.com/acme", "the message names the scope, not the other tenant")
		assert.NotContains(t, err.Error(), "tenant-a", "another tenant's namespace must not be disclosed")
	})

	t.Run("a different GitHub scope is not a collision", func(t *testing.T) {
		v := runnerSetValidatorWith(t,
			identityV2Gateway("tenant-a", "gw1", acme),
			identityV2Gateway("tenant-b", "gw2", "https://github.com/other"),
			labelledRS("build", "tenant-a", "gw1", "linux"),
		)
		_, err := v.ValidateCreate(ctx, labelledRS("build", "tenant-b", "gw2", "windows"))
		assert.NoError(t, err)
	})

	t.Run("an update does not reject the set against its own stored copy", func(t *testing.T) {
		stored := labelledRS("build", "tenant", "gw2", "linux")
		v := runnerSetValidatorWith(t, identityV2Gateway("tenant", "gw2", acme), stored)
		updated := labelledRS("build", "tenant", "gw2", "linux", "x64")
		_, err := v.ValidateUpdate(ctx, stored, updated)
		assert.NoError(t, err)
	})

	t.Run("a nil reader disables the check", func(t *testing.T) {
		v := &RunnerSetCustomValidator{}
		_, err := v.ValidateCreate(ctx, labelledRS("build", "tenant", "gw2", "linux"))
		assert.NoError(t, err)
	})
}
