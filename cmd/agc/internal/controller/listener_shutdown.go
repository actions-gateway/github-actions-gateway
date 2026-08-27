package controller

import (
	"context"
	"log/slog"
	"sync"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"k8s.io/apimachinery/pkg/types"
)

// listenerShutdown is a manager.Runnable that drains a reconciler's listener
// goroutines when the manager's context is cancelled (SIGTERM).
//
// It exists because nothing else waits for them (Q222). Listener goroutines are
// spawned from Reconcile onto the manager's context, so a SIGTERM cancels them —
// but mgr.Start does not track them, so it returned as soon as the controllers
// stopped and main() exited out from under every goroutine still unwinding its
// poll loop. Each one deletes its broker session in an exit defer, so the process
// was routinely killed mid-DELETE: the AGC leaked GitHub-side sessions on every
// rollout, and those sessions hold their runner records online until GitHub
// expires them server-side.
//
// Registering the drain as a Runnable puts it inside the manager's graceful
// shutdown: mgr.Start now blocks on Start returning, which happens only after
// every listener goroutine has run its exit defer. The manager's
// GracefulShutdownTimeout (30s by default) caps the wait; each session DELETE is
// itself bounded well inside that (listener.sessionDeleteTimeout).
type listenerShutdown struct {
	// stop drains every listener the owning reconciler runs — multiplexers on the
	// classic tier, poll loops on the scale-set one — and returns a channel closed
	// once every listener goroutine has exited.
	stop func() <-chan struct{}
	log  *slog.Logger
	// owner names the reconciler in the shutdown log line ("RunnerGroup"/"RunnerSet").
	owner string
}

// Start blocks until the manager's context is cancelled, then drains the
// reconciler's listener goroutines. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *listenerShutdown) Start(ctx context.Context) error {
	<-ctx.Done()
	s.log.Info("draining listener goroutines before shutdown", "owner", s.owner)
	<-s.stop()
	// The claim is now the guard's to keep rather than the drain's to hope for: the
	// stopped flag is set before this returns, so nothing reopens a session behind it.
	// It also covers both tiers on a RunnerSet, where "broker" named only the classic
	// one while the scale-set session was the one that could leak (Q968).
	s.log.Info("listener goroutines drained; every session they held is deleted", "owner", s.owner)
	return nil
}

// NeedLeaderElection reports that the drain must run on every replica, not only
// the leader: a replica that held leadership and then lost it still owns the
// listener goroutines it spawned, and they must be drained on its shutdown.
func (s *listenerShutdown) NeedLeaderElection() bool { return false }

// stopMultiplexers stops every multiplexer in muxes concurrently and returns a
// done channel closed once all of them have exited. Concurrent rather than
// sequential so shutdown costs the slowest multiplexer, not their sum — an AGC
// serving many RunnerGroups would otherwise risk the manager's
// GracefulShutdownTimeout. Callers must not hold the map's mutex: the caller
// passes a snapshot taken under it.
func stopMultiplexers(muxes []*listener.Multiplexer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, mux := range muxes {
			wg.Add(1)
			go func(mux *listener.Multiplexer) {
				defer wg.Done()
				mux.Stop()
			}(mux)
		}
		wg.Wait()
	}()
	return done
}

// snapshotMultiplexers copies the live multiplexers out of m under mu and clears
// the map, so a concurrent reconcile cannot resurrect a multiplexer the drain has
// already stopped. Returns nil when there is nothing to drain.
func snapshotMultiplexers(mu *sync.Mutex, m map[types.NamespacedName]*listener.Multiplexer) []*listener.Multiplexer {
	mu.Lock()
	defer mu.Unlock()
	if len(m) == 0 {
		return nil
	}
	muxes := make([]*listener.Multiplexer, 0, len(m))
	for key, mux := range m {
		muxes = append(muxes, mux)
		delete(m, key)
	}
	return muxes
}

// A drain is one-way: both stopListeners below set the reconciler's stopped flag
// before draining, and every listener start path refuses once it is set. Without that
// the drain is only advisory — the reconciler keeps serving queued reconciles while
// the manager shuts down, and one of them starting a listener opens a session this
// process will never delete, because the drain that would have deleted it has already
// run. On the scale-set tier that leaks the scale set's single session and locks the
// successor AGC out; on the classic tier it leaks the broker sessions this file's own
// shutdown log line claims are gone (Q968, and the property Q222 exists to hold).

// stopListeners drains every listener goroutine this RunnerGroup reconciler owns.
// It returns a done channel closed once they have all exited (and so have all
// deleted their broker sessions), per the repo's async convention.
func (r *RunnerGroupReconciler) stopListeners() <-chan struct{} {
	r.stopped.Store(true)
	return stopMultiplexers(snapshotMultiplexers(&r.multiplexersMu, r.multiplexers))
}

// stopListeners drains every listener goroutine this RunnerSet reconciler owns, on
// both acquisition tiers. See RunnerGroupReconciler.stopListeners.
//
// The scale-set tier needs the barrier at least as much as the classic one: its poll
// loop's exit defers read the conclusions the queue is still holding (Q689), delete the
// messages those conclusions release (Q603), and only then delete the session. A process
// that exits out from under them strands a concluded job's assignment in the queue, and
// the next AGC provisions a worker for a job that is over.
//
// The two tiers drain concurrently: both helpers spawn before either is awaited, so
// shutdown costs the slower tier rather than their sum.
func (r *RunnerSetReconciler) stopListeners() <-chan struct{} {
	r.stopped.Store(true)
	classic := stopMultiplexers(snapshotMultiplexers(&r.multiplexersMu, r.multiplexers))
	scaleSet := stopScaleSetHandles(snapshotScaleSetListeners(&r.scaleSetListenersMu, r.scaleSetListeners))
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-classic
		<-scaleSet
	}()
	return done
}

// stopScaleSetHandles cancels every scale-set listener in handles concurrently and
// returns a done channel closed once all of their poll loops have exited — and so have
// run their exit teardown. Concurrent for the same reason multiplexers are: an AGC
// serving many RunnerSets must not spend the manager's GracefulShutdownTimeout on the
// sum of their teardown budgets.
func stopScaleSetHandles(handles []*scaleSetListenerHandle) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, h := range handles {
			wg.Add(1)
			go func(h *scaleSetListenerHandle) {
				defer wg.Done()
				h.cancel()
				<-h.done
			}(h)
		}
		wg.Wait()
	}()
	return done
}

// snapshotScaleSetListeners copies the live scale-set listeners out of m under mu and
// clears the map, so a concurrent reconcile cannot resurrect one the drain has already
// stopped. Returns nil when there is nothing to drain.
func snapshotScaleSetListeners(mu *sync.Mutex, m map[types.NamespacedName]*scaleSetListenerHandle) []*scaleSetListenerHandle {
	mu.Lock()
	defer mu.Unlock()
	if len(m) == 0 {
		return nil
	}
	handles := make([]*scaleSetListenerHandle, 0, len(m))
	for key, h := range m {
		handles = append(handles, h)
		delete(m, key)
	}
	return handles
}
