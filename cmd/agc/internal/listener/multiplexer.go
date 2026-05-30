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
}

// ConfigFactory creates a Config for a new listener goroutine at the given
// index. The Multiplexer passes IsLastListener and SpawnReplacement closures
// before handing the Config to Run.
type ConfigFactory func(index int) Config

// Multiplexer manages the adaptive pool of listener goroutines for one RunnerGroup.
// It ensures at least one goroutine is always running (the permanent baseline)
// and spawns additional goroutines on demand up to maxListeners.
type Multiplexer struct {
	mu           sync.Mutex
	active       map[int]*listenerState
	activeCount  atomic.Int32 // maintained in sync with active; allows lock-free reads
	nextIndex    int
	maxListeners atomic.Int32
	factory      ConfigFactory
	log          *slog.Logger
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
		active:  make(map[int]*listenerState),
		factory: factory,
		log:     log,
	}
	m.maxListeners.Store(maxListeners)
	return m
}

// Start launches the permanent baseline listener. Must be called once when
// the RunnerGroup is first reconciled. ctx must remain live for the duration
// of the Multiplexer's operation.
func (m *Multiplexer) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawn(ctx, true)
	return nil
}

// SetMaxListeners updates the ceiling. If the new ceiling is lower than the
// current active count, excess idle goroutines shut down at their next 202.
func (m *Multiplexer) SetMaxListeners(max int32) {
	if max < 1 {
		max = 1
	}
	m.maxListeners.Store(max)
}

// SpawnReplacement spawns one additional listener goroutine if the active
// count is below maxListeners. Called by a listener goroutine after it acquires
// a job to maintain polling capacity.
func (m *Multiplexer) SpawnReplacement(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int32(len(m.active)) >= m.maxListeners.Load() {
		return
	}
	m.spawn(ctx, false)
}

// ActiveCount returns the current number of running listener goroutines.
// This is a lock-free read via an atomic counter maintained alongside the map.
func (m *Multiplexer) ActiveCount() int32 {
	return m.activeCount.Load()
}

// Stop cancels all listener goroutines and waits for them to exit cleanly.
func (m *Multiplexer) Stop() {
	m.mu.Lock()
	states := make([]*listenerState, 0, len(m.active))
	for _, s := range m.active {
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

	lCtx, cancel := context.WithCancel(ctx)
	state := &listenerState{
		cancel: cancel,
		done:   make(chan struct{}),
		isPerm: isPerm,
	}
	m.active[idx] = state
	m.activeCount.Add(1)

	cfg := m.factory(idx)
	cfg.IsLastListener = func() bool { return m.ActiveCount() <= 1 }
	cfg.SpawnReplacement = func(ctx context.Context) { m.SpawnReplacement(ctx) }

	go func() {
		defer close(state.done)

		runErr := Run(lCtx, cfg)
		if runErr != nil {
			m.log.Warn("listener goroutine exited with error", "error", runErr, "index", idx)
		}

		m.mu.Lock()
		delete(m.active, idx)
		m.activeCount.Add(-1)
		var nre *NonRetriableError
		// Only restart the permanent baseline for recoverable exits. A
		// NonRetriableError (VersionTooOld, unauthorized) means the goroutine
		// should not loop — the condition is already surfaced on the RunnerGroup.
		shouldRestart := isPerm && lCtx.Err() == nil && !errors.As(runErr, &nre)
		m.mu.Unlock()

		if shouldRestart {
			// Permanent baseline goroutine exited for a recoverable reason.
			// Restart it after a brief backoff.
			delay := m.RestartDelay
			if delay == 0 {
				delay = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			m.mu.Lock()
			m.spawn(ctx, true)
			m.mu.Unlock()
		}
	}()
}
