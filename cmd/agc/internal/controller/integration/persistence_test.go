//go:build integration

package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// countingRegistrar wraps a Registrar and counts Register calls so tests can
// assert that agents are not re-registered after a controller restart.
type countingRegistrar struct {
	delegate      agentpool.Registrar
	registerCount atomic.Int64
}

func (r *countingRegistrar) Register(ctx context.Context, token string, params agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	r.registerCount.Add(1)
	return r.delegate.Register(ctx, token, params)
}

func (r *countingRegistrar) Deregister(ctx context.Context, token string, id int64) error {
	return r.delegate.Deregister(ctx, token, id)
}

// TestAGC_AgentSecretPersistence verifies that restarting the AGC controller
// reconstructs the agent pool from existing Secrets without re-registering agents
// with GitHub. This exercises the LoadAgents path in EnsureAgents.
func TestAGC_AgentSecretPersistence(t *testing.T) {
	const nsName = "agc-persistence-test"
	createNSForAGC(t, nsName)

	reg := &countingRegistrar{delegate: &brokerRegistrar{stub: brokerStub}}

	rg := newRunnerGroup(nsName, "persist-rg", 2)
	require.NoError(t, k8sClient.Create(ctx, rg))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })

	// First controller instance: creates 2 agents and registers them with "GitHub".
	cancelFirst, firstDone := startAGCReconcilerOpts(t, provisionerOptions{registrar: reg})

	require.Eventually(t, func() bool {
		var secrets corev1.SecretList
		if err := k8sClient.List(ctx, &secrets,
			client.InNamespace(nsName),
			client.MatchingLabels{"actions-gateway/runner-group": "persist-rg"},
		); err != nil {
			return false
		}
		return len(secrets.Items) == 2
	}, 15*time.Second, 50*time.Millisecond, "expected 2 agent Secrets created by first controller")

	firstRegistrationCount := int(reg.registerCount.Load())
	require.Equal(t, 2, firstRegistrationCount,
		"expected exactly 2 Register calls for maxListeners=2")

	// Wait for the first session to confirm the multiplexer started.
	require.Eventually(t, func() bool {
		return brokerStub.ActiveSessionCount() >= 1
	}, 15*time.Second, 1*time.Millisecond, "first controller must establish a session")

	// Snapshot sessions before the restart so we can detect new ones after.
	seenBefore := map[string]bool{}
	for _, id := range brokerStub.RegisteredSessions() {
		seenBefore[id] = true
	}

	// Simulate AGC restart: stop the first controller (mimicking a pod restart).
	// The t.Cleanup registered by startAGCReconcilerOpts also stops it, but we need
	// to stop it explicitly before starting the second controller so there is no
	// overlap between the two managers (they would conflict on the same resources).
	cancelFirst()
	<-firstDone

	// Second controller instance: must reconstruct pool from existing Secrets
	// without calling Register again.
	startAGCReconcilerOpts(t, provisionerOptions{registrar: reg})

	// Wait for the second controller to establish a new session (Multiplexer restarted).
	require.Eventually(t, func() bool {
		for _, id := range brokerStub.RegisteredSessions() {
			if !seenBefore[id] {
				return true
			}
		}
		return false
	}, 15*time.Second, 1*time.Millisecond, "second controller must establish a session after restart")

	// Register must not have been called again — agents loaded from existing Secrets.
	assert.Equal(t, firstRegistrationCount, int(reg.registerCount.Load()),
		"agent registration must not be called after restart when Secrets already exist")

	// The 2 Secrets must still exist (not recreated).
	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &secrets,
		client.InNamespace(nsName),
		client.MatchingLabels{"actions-gateway/runner-group": "persist-rg"},
	))
	assert.Equal(t, 2, len(secrets.Items),
		"original 2 agent Secrets should persist across controller restart")
}
