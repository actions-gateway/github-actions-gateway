package agentpool_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newPoolWithRegistrar mirrors newPool but exposes the registrar for
// single-use JIT simulation hooks.
func newPoolWithRegistrar(fb *fake.ClientBuilder, ns, group string) (*agentpool.Pool, *agentpool.StubRegistrar, client.Client) {
	registrar := agentpool.NewStubRegistrar()
	c := fb.Build()
	return agentpool.NewPool(c, ns, group, "2.335.1", []string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519), registrar, c
}

func agentSecretField(t *testing.T, c client.Client, ns, group string, index int, key string) string {
	t.Helper()
	var sec corev1.Secret
	name := "agentpool-" + group + "-" + strconv.Itoa(index)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sec))
	return string(sec.Data[key])
}

func TestPool_Recycle_ReregistersAndRewritesSecret(t *testing.T) {
	ctx := context.Background()
	pool, registrar, c := newPoolWithRegistrar(fake.NewClientBuilder().WithScheme(scheme()), "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	oldID := a.AgentID

	// A job acquisition consumes the record: GitHub deletes it server-side.
	pool.MarkConsumed(a)
	registrar.SimulateServerDelete(oldID)

	fresh, err := pool.Recycle(ctx, a, "token")
	require.NoError(t, err)
	assert.NotEqual(t, oldID, fresh.AgentID, "recycle must mint a fresh registration")
	assert.Equal(t, a.Index, fresh.Index, "index is stable across recycles")

	// The Secret was rewritten in place with the fresh credentials.
	assert.Equal(t, strconv.FormatInt(fresh.AgentID, 10),
		agentSecretField(t, c, "default", "my-rg", a.Index, "agentId"))

	// The claim is preserved: the pool is still exhausted for other claimants.
	assert.Nil(t, pool.ClaimAgent())

	// Releasing hands the *fresh* agent back to the pool (consumed was cleared).
	pool.ReleaseAgent(a)
	reclaimed := pool.ClaimAgent()
	require.NotNil(t, reclaimed)
	assert.Equal(t, fresh.AgentID, reclaimed.AgentID)
}

func TestPool_Recycle_ResolvesNameConflict(t *testing.T) {
	ctx := context.Background()
	pool, registrar, _ := newPoolWithRegistrar(fake.NewClientBuilder().WithScheme(scheme()), "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)

	// Simulate the partial-failure 409: the record under our name survives with
	// an ID the pool does not know (the Secret-backed ID is already dead).
	registrar.SimulateServerDelete(a.AgentID)
	survivor, err := registrar.Register(ctx, "token", agentpool.RegisterParams{Name: "my-rg-0"})
	require.NoError(t, err)
	require.NotEqual(t, a.AgentID, survivor.AgentID)

	fresh, err := pool.Recycle(ctx, a, "token")
	require.NoError(t, err)
	assert.NotEqual(t, survivor.AgentID, fresh.AgentID, "the conflicting survivor must be replaced")
	assert.Equal(t, fresh.AgentID, registrar.AgentIDForName("my-rg-0"),
		"the fresh registration owns the name after conflict resolution")
}

func TestPool_ReleaseAgent_ParksConsumed(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := newPoolWithRegistrar(fake.NewClientBuilder().WithScheme(scheme()), "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	pool.MarkConsumed(a)

	// A consumed agent's credentials are dead: release must park it, not
	// re-issue it to the next claimant.
	pool.ReleaseAgent(a)
	assert.Nil(t, pool.ClaimAgent(), "parked consumed agent must not be claimable")
}

func TestPool_EnsureAgents_RepairsParkedAgent(t *testing.T) {
	ctx := context.Background()
	pool, registrar, _ := newPoolWithRegistrar(fake.NewClientBuilder().WithScheme(scheme()), "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	oldID := a.AgentID
	pool.MarkConsumed(a)
	registrar.SimulateServerDelete(oldID)
	pool.ReleaseAgent(a) // parked

	// The reconcile repair pass re-registers parked agents and surfaces them.
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))
	repaired := pool.ClaimAgent()
	require.NotNil(t, repaired, "repaired agent must be claimable again")
	assert.NotEqual(t, oldID, repaired.AgentID, "repair must have re-registered")
}

func TestPool_EnsureAgents_ConsumedSurvivesReloadWhileClaimed(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := newPoolWithRegistrar(fake.NewClientBuilder().WithScheme(scheme()), "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	pool.MarkConsumed(a)

	// A reconcile while the consumed agent is still claimed (job running) must
	// not repair it out from under its holder, and must keep the consumed mark
	// across reload so a later release still parks it.
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))
	pool.ReleaseAgent(a)
	assert.Nil(t, pool.ClaimAgent(), "consumed mark must survive reload and park on release")
}
