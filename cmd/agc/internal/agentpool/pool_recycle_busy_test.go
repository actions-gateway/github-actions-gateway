package agentpool

// Q259: Recycle must survive GitHub's transient 422 "runner is currently running
// a job and cannot be deleted" after a single-use JIT job completes, so a
// concurrent burst does not collapse the pool to a single online listener. These
// are white-box tests: they set the pool's fast, deterministic recycle backoff
// hook (unexported) so the bounded retry loop runs without real sleeps.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func busyTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// newBusyTestPool builds a pool with a no-op recycle sleep so backoff is
// instant, and returns the pool and its stub registrar.
func newBusyTestPool(t *testing.T) (*Pool, *StubRegistrar) {
	t.Helper()
	registrar := NewStubRegistrar()
	c := fake.NewClientBuilder().WithScheme(busyTestScheme()).Build()
	p := NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, registrar, KeyTypeEd25519)
	// Deterministic, instant backoff — assert the retry logic, not wall time.
	p.recycleMaxAttempts = 6
	p.sleep = func(context.Context, time.Duration) error { return nil }
	return p, registrar
}

// TestRecycle_RetriesThroughTransientRunnerBusy proves the wedge is fixed: when
// GitHub refuses the deregister with 422 "currently running" for a few attempts
// (the ephemeral runner lingering after job completion), Recycle backs off,
// retries, and succeeds once the runner releases — returning a fresh, claimable
// agent instead of failing and dropping the listener slot.
func TestRecycle_RetriesThroughTransientRunnerBusy(t *testing.T) {
	ctx := context.Background()
	pool, registrar := newBusyTestPool(t)
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	oldID := a.AgentID

	// The job was acquired: mark consumed. GitHub still considers the ephemeral
	// runner "running" and refuses to delete it for the next 2 deregister calls;
	// the record (and its name) linger, so re-registration under the stable name
	// 409s until it releases.
	pool.MarkConsumed(a)
	registrar.SimulateRunnerBusy(oldID, 2)

	fresh, err := pool.Recycle(ctx, a, "token")
	require.NoError(t, err, "recycle must ride out the transient 422 and succeed")
	assert.NotEqual(t, oldID, fresh.AgentID, "recycle must mint a fresh registration")
	assert.Equal(t, a.Index, fresh.Index, "index is stable across recycles")

	// The recycled slot is usable again: release, then re-claim the fresh agent.
	pool.ReleaseAgent(a)
	reclaimed := pool.ClaimAgent()
	require.NotNil(t, reclaimed, "the recycled agent must be claimable again — pool not collapsed")
	assert.Equal(t, fresh.AgentID, reclaimed.AgentID)
}

// TestRecycle_GivesUpBoundedWhenRunnerNeverReleases proves the retry is bounded:
// a runner that never releases (permanent 422) makes Recycle give up after the
// attempt ceiling with a *RunnerBusyError, rather than spinning a hot loop. The
// caller (recycleAndRestart / EnsureAgents repair) records
// actions_gateway_agent_recycle_errors_total on that error.
func TestRecycle_GivesUpBoundedWhenRunnerNeverReleases(t *testing.T) {
	ctx := context.Background()
	pool, registrar := newBusyTestPool(t)
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	oldID := a.AgentID

	pool.MarkConsumed(a)
	registrar.SimulateRunnerBusy(oldID, -1) // busy forever

	fresh, err := pool.Recycle(ctx, a, "token")
	require.Error(t, err, "recycle must give up when the runner never releases")
	assert.Nil(t, fresh)
	var busy *RunnerBusyError
	assert.True(t, errors.As(err, &busy), "give-up error must carry the RunnerBusyError, got %v", err)

	// Bounded: one best-effort deregister up front, then one conflict-resolution
	// deregister per registerAgent attempt (recycleMaxAttempts of them). No hot
	// loop.
	assert.LessOrEqual(t, registrar.DeregisterCalls(), 1+pool.recycleMaxAttempts,
		"retry must be bounded, not a hot loop")
	assert.GreaterOrEqual(t, registrar.DeregisterCalls(), pool.recycleMaxAttempts,
		"retry must actually exhaust its attempts before giving up")
}

// TestRecycle_ContextCancellationAbortsBackoff proves a cancelled context aborts
// the retry backoff promptly instead of waiting out the bound.
func TestRecycle_ContextCancellationAbortsBackoff(t *testing.T) {
	registrar := NewStubRegistrar()
	c := fake.NewClientBuilder().WithScheme(busyTestScheme()).Build()
	pool := NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, registrar, KeyTypeEd25519)
	pool.recycleMaxAttempts = 6
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel on the first backoff so the loop returns ctx.Err() rather than looping.
	pool.sleep = func(c context.Context, _ time.Duration) error {
		cancel()
		return c.Err()
	}
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)
	pool.MarkConsumed(a)
	registrar.SimulateRunnerBusy(a.AgentID, -1)

	fresh, err := pool.Recycle(ctx, a, "token")
	require.Error(t, err)
	assert.Nil(t, fresh)
	assert.ErrorIs(t, err, context.Canceled)
}
