package controller

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// TestListenerShutdown_BlocksUntilDrainCompletes is the Q222 guard: the Runnable
// must not return before the drain it triggers has finished, because the
// manager's graceful shutdown uses Start's return as "safe to exit the process"
// — and returning early is what let SIGTERM kill listener goroutines mid-DELETE.
func TestListenerShutdown_BlocksUntilDrainCompletes(t *testing.T) {
	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	var drained atomic.Bool

	s := &listenerShutdown{
		owner: "Test",
		log:   slog.Default(),
		stop: func() <-chan struct{} {
			done := make(chan struct{})
			go func() {
				close(drainStarted)
				<-releaseDrain
				drained.Store(true)
				close(done)
			}()
			return done
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan error, 1)
	go func() { startReturned <- s.Start(ctx) }()

	// Before cancellation the Runnable must sit idle — it must not drain a live
	// manager's listeners.
	select {
	case <-drainStarted:
		t.Fatal("drain started before the manager context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-drainStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never started after cancellation")
	}

	// The drain is in flight: Start must still be blocked.
	select {
	case <-startReturned:
		t.Fatal("Start returned before the drain completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseDrain)
	select {
	case err := <-startReturned:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the drain completed")
	}
	assert.True(t, drained.Load(), "Start must return only after the drain finished")
}

// TestListenerShutdown_RunsOnEveryReplica pins the leader-election opt-out: a
// replica owns the listener goroutines it spawned regardless of whether it still
// holds leadership, so its drain must run on shutdown either way.
func TestListenerShutdown_RunsOnEveryReplica(t *testing.T) {
	assert.False(t, (&listenerShutdown{}).NeedLeaderElection())
}

// TestStopListeners_WaitsForTheScaleSetTierToo is the Q689 half of the Q222 barrier.
// Scale-set listeners live in their own map and were never in the drain, so SIGTERM
// cancelled them and the process exited without waiting — taking the exit read of
// outstanding conclusions, the delete half of the ack, and the session DELETE with it.
func TestStopListeners_WaitsForTheScaleSetTierToo(t *testing.T) {
	exited := make(chan struct{})
	var teardownRan atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		// Stand in for the poll loop's exit defers, which is what the barrier exists
		// to let finish.
		time.Sleep(50 * time.Millisecond)
		teardownRan.Store(true)
		close(exited)
	}()

	key := types.NamespacedName{Namespace: "ns", Name: "linux"}
	r := &RunnerSetReconciler{
		scaleSetListeners: map[types.NamespacedName]*scaleSetListenerHandle{
			key: {cancel: cancel, done: exited},
		},
	}

	select {
	case <-r.stopListeners():
	case <-time.After(5 * time.Second):
		t.Fatal("stopListeners did not drain the scale-set listener")
	}
	assert.True(t, teardownRan.Load(), "the drain must wait for the poll loop's exit teardown")
	assert.Empty(t, r.scaleSetListeners, "the drained listener must be dropped from the map")
}

// TestSnapshotMultiplexers_ClearsMapSoAReconcileCannotResurrect verifies the
// snapshot empties the map under the mutex: a reconcile racing the drain must not
// find — and keep feeding — a multiplexer that is already being stopped.
func TestSnapshotMultiplexers_ClearsMapSoAReconcileCannotResurrect(t *testing.T) {
	r := &RunnerGroupReconciler{
		multiplexers: map[types.NamespacedName]*listener.Multiplexer{
			{Namespace: "ns", Name: "a"}: listener.NewMultiplexer(nil, 1, slog.Default()),
			{Namespace: "ns", Name: "b"}: listener.NewMultiplexer(nil, 1, slog.Default()),
		},
	}
	muxes := snapshotMultiplexers(&r.multiplexersMu, r.multiplexers)
	assert.Len(t, muxes, 2)
	assert.Empty(t, r.multiplexers)

	// Draining an already-empty reconciler is a no-op that still closes its done
	// channel, so the Runnable never hangs on a reconciler that never spawned.
	select {
	case <-r.stopListeners():
	case <-time.After(5 * time.Second):
		t.Fatal("stopListeners on an empty reconciler must complete immediately")
	}
}
