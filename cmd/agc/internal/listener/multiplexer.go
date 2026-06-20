package listener

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// listenerState tracks one running listener goroutine.
type listenerState struct {
	cancel context.CancelFunc
	done   chan struct{}
	isPerm bool // permanent baseline goroutine; always restarted on exit
	// polling is true while this goroutine is long-polling for work and false
	// while it is executing a job (inside JobHandler). It is the per-goroutine
	// bit the Multiplexer reconciles into pollerCount; the goroutine flips it via
	// the SetPolling closure, and the exit handler clears it exactly once.
	polling atomic.Bool
}

// ConfigFactory creates a Config for a new listener goroutine at the given
// index. The Multiplexer passes IsLastPoller, SpawnReplacement, and SetPolling
// closures before handing the Config to Run.
type ConfigFactory func(index int) Config

// Multiplexer manages the adaptive pool of listener goroutines for one RunnerGroup.
// It ensures at least one goroutine is always running (the permanent baseline)
// and spawns additional goroutines on demand up to maxListeners.
type Multiplexer struct {
	mu          sync.Mutex
	active      map[int]*listenerState
	activeCount atomic.Int32 // maintained in sync with active; allows lock-free reads
	// pollerCount is the number of running goroutines currently long-polling for
	// work — a subset of activeCount that excludes goroutines busy inside
	// JobHandler. The last-poller decision (IsLastPoller) is based on this, not
	// activeCount, so a single real poller is not allowed to idle-exit just
	// because a sibling goroutine is mid-job (Q152). Lock-free atomic read.
	pollerCount atomic.Int32
	// restarting holds permanent-baseline states waiting out RestartDelay after
	// a recoverable crash. They are out of active (ActiveCount excludes them)
	// but Stop must still cancel and wait for them.
	restarting   map[int]*listenerState
	nextIndex    int
	maxListeners atomic.Int32
	// permAlive is true while a permanent baseline goroutine is running or
	// restart-pending. It makes Start idempotent: a reconcile firing during the
	// RestartDelay window (when ActiveCount is 0) must not stack a second
	// permanent baseline on top of the pending restart.
	permAlive bool
	// stopped is set by Stop; it suppresses all further spawns and restarts.
	stopped bool
	factory ConfigFactory
	log     *slog.Logger
	// RestartDelay is the backoff before restarting a crashed permanent listener
	// goroutine. Zero defaults to one second. Override to a smaller value in tests.
	RestartDelay time.Duration
}

// NewMultiplexer creates a Multiplexer for one RunnerGroup.
func NewMultiplexer(factory ConfigFactory, maxListeners int32, log *slog.Logger) *Multiplexer {
	if log == nil {
		log = slog.Default()
	}
	m := &Multiplexer{
		active:     make(map[int]*listenerState),
		restarting: make(map[int]*listenerState),
		factory:    factory,
		log:        log,
	}
	m.maxListeners.Store(maxListeners)
	return m
}

// Start launches the permanent baseline listener goroutine. It is idempotent:
// while a baseline is running — or waiting out RestartDelay after a recoverable
// crash — further calls are no-ops, so reconcile loops may call it freely.
// After a non-retriable baseline exit Start spawns a fresh baseline; after Stop
// it is a no-op (a stopped Multiplexer is retired — create a new one instead).
// ctx must remain live for the duration of the Multiplexer's operation.
func (m *Multiplexer) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.permAlive {
		return nil
	}
	m.spawn(ctx, true)
	return nil
}

// SetMaxListeners updates the ceiling. If the new ceiling is lower than the
// current active count, excess idle goroutines shut down at their next 202.
func (m *Multiplexer) SetMaxListeners(maxListeners int32) {
	if maxListeners < 1 {
		maxListeners = 1
	}
	m.maxListeners.Store(maxListeners)
}

// SpawnReplacement spawns one additional listener goroutine if the active
// count is below maxListeners. Called by a listener goroutine after it acquires
// a job to maintain polling capacity.
func (m *Multiplexer) SpawnReplacement(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || int32(len(m.active)) >= m.maxListeners.Load() {
		return
	}
	m.spawn(ctx, false)
}

// ActiveCount returns the current number of running listener goroutines.
// This is a lock-free read via an atomic counter maintained alongside the map.
func (m *Multiplexer) ActiveCount() int32 {
	return m.activeCount.Load()
}

// PollerCount returns the current number of running goroutines that are
// long-polling for work, excluding any busy executing a job. Lock-free read.
func (m *Multiplexer) PollerCount() int32 {
	return m.pollerCount.Load()
}

// setPolling reconciles a goroutine's poller status into the shared pollerCount.
// A goroutine counts as a poller while long-polling and stops counting while it
// executes a job. The atomic Swap makes this idempotent per state — only a real
// transition adjusts the counter — and races safely with the exit handler's
// final Swap(false): whichever runs first wins, so the counter stays consistent.
func (m *Multiplexer) setPolling(state *listenerState, polling bool) {
	if state.polling.Swap(polling) == polling {
		return // no transition
	}
	if polling {
		m.pollerCount.Add(1)
	} else {
		m.pollerCount.Add(-1)
	}
}

// Stop cancels all listener goroutines — including any permanent baseline
// waiting out its restart backoff — and waits for them to exit cleanly. The
// Multiplexer is retired afterwards: Start and SpawnReplacement become no-ops.
func (m *Multiplexer) Stop() {
	m.mu.Lock()
	m.stopped = true
	states := make([]*listenerState, 0, len(m.active)+len(m.restarting))
	for _, s := range m.active {
		s.cancel()
		states = append(states, s)
	}
	for _, s := range m.restarting {
		s.cancel()
		states = append(states, s)
	}
	m.mu.Unlock()

	for _, s := range states {
		<-s.done
	}
}

// spawn starts a new listener goroutine. Must be called with m.mu held.
func (m *Multiplexer) spawn(ctx context.Context, isPerm bool) {
	idx := m.nextIndex
	m.nextIndex++
	if isPerm {
		m.permAlive = true
	}

	lCtx, cancel := context.WithCancel(ctx)
	state := &listenerState{
		cancel: cancel,
		done:   make(chan struct{}),
		isPerm: isPerm,
	}
	// A freshly spawned goroutine starts in the poll loop, so it counts as a
	// poller until it enters JobHandler (SetPolling(false)) or exits.
	state.polling.Store(true)
	m.active[idx] = state
	m.activeCount.Add(1)
	m.pollerCount.Add(1)

	cfg := m.factory(idx)
	cfg.IsLastPoller = func() bool { return m.PollerCount() <= 1 }
	cfg.SpawnReplacement = func(ctx context.Context) { m.SpawnReplacement(ctx) }
	cfg.SetPolling = func(polling bool) { m.setPolling(state, polling) }

	go func() {
		defer close(state.done)
		// Release the child context registration on the long-lived parent ctx.
		// Runs after the restart select below, which watches lCtx.Done() as the
		// Stop signal, so it must not fire earlier.
		defer cancel()

		runErr := Run(lCtx, cfg)
		if runErr != nil {
			m.log.Warn("listener goroutine exited with error", "error", runErr, "index", idx)
		}

		// Return the claimed agent to the pool before any restart claims a fresh
		// one, so the permanent baseline can reclaim it. Released exactly once per
		// spawn; nil when this goroutine never claimed an agent.
		if cfg.ReleaseAgent != nil {
			cfg.ReleaseAgent()
		}

		m.mu.Lock()
		delete(m.active, idx)
		m.activeCount.Add(-1)
		// Reconcile the poller count: decrement only if this goroutine was still
		// counted as a poller (it exited from the poll loop, not mid-job). Run has
		// returned, so no SetPolling call can race this final Swap.
		if state.polling.Swap(false) {
			m.pollerCount.Add(-1)
		}
		var nre *NonRetriableError
		// Only restart the permanent baseline for recoverable exits. A
		// NonRetriableError (VersionTooOld, unauthorized) means the goroutine
		// should not loop — the condition is already surfaced on the RunnerGroup.
		shouldRestart := isPerm && !m.stopped && lCtx.Err() == nil && !errors.As(runErr, &nre)
		if isPerm {
			if shouldRestart {
				// Stay visible to Stop while waiting out RestartDelay, and keep
				// permAlive set so a concurrent Start is a no-op for the whole
				// window — otherwise it would stack a second baseline (Q100).
				m.restarting[idx] = state
			} else {
				// The baseline is gone for good (non-retriable exit, cancellation,
				// or Stop). A future Start may spawn a fresh one.
				m.permAlive = false
			}
		}
		m.mu.Unlock()

		if !shouldRestart {
			return
		}

		// Permanent baseline goroutine exited for a recoverable reason.
		// Restart it after a brief backoff.
		delay := m.RestartDelay
		if delay == 0 {
			delay = time.Second
		}
		// Without this Debug line the restart/backoff path is silent: an operator
		// sees only the repeated "exited with error" Warn above and cannot tell a
		// self-healing baseline from a dead loop. Kept at Debug so the steady-state
		// recoverable-crash churn does not add Info volume.
		m.log.Debug("permanent baseline listener exited; restarting after backoff", "index", idx, "delay", delay)
		aborted := false
		select {
		case <-ctx.Done():
			aborted = true
		case <-lCtx.Done():
			// Stop cancels restart-pending baselines via state.cancel.
			aborted = true
		case <-time.After(delay):
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.restarting, idx)
		if aborted || m.stopped {
			m.log.Debug("permanent baseline listener restart aborted (multiplexer stopping)", "index", idx)
			m.permAlive = false
			return
		}
		m.spawn(ctx, true)
	}()
}
