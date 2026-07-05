package listener

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
)

// jobClaim is the accounting state of one claimed planID (Q260). It collapses
// GitHub's per-delivery fan-out onto one provisioning winner and, under Option A,
// carries what the winner needs to reconcile the deduped-away sibling deliveries on
// GitHub's books when its job finishes.
type jobClaim struct {
	// expireAt is the zero Time while the job is in-flight (held until the winner
	// concludes); it becomes a future instant once the winner concludes and the
	// claim lingers for ClaimLinger. A still-present entry therefore always denies a
	// fresh claim — either in-flight or completed within the trailing linger window.
	expireAt time.Time
	// concluded is set once the winner reports its terminal result. A sibling
	// delivered after this point (a late redelivery within the linger window) is
	// resolved with result rather than registered.
	concluded bool
	// result is the winner's terminal result, valid once concluded.
	result broker.TaskResult
	// siblings are the deduped sibling deliveries registered against this planID
	// while the winner ran, awaiting the winner's fan-out completion (Q260 Option A).
	siblings []SiblingDelivery
	// concludedCh is closed exactly once when the winner concludes (in Complete).
	// A deduped loser whose winner is still running waits on it before recycling
	// its slot: the winner's conclusion is when it fans completjob out to the
	// loser's delivery (Option A), releasing GitHub's assignment on the loser's
	// deduped runner so its recycle 422 finally clears (Q266). Created with the
	// claim; never nil for a live claim.
	concludedCh chan struct{}
}

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

	// jobClaimsMu guards jobClaims. It is separate from mu so the hot per-job
	// claim/release path never contends with spawn/Stop bookkeeping.
	jobClaimsMu sync.Mutex
	// jobClaims maps a claimed planID to its accounting state (see jobClaim). It
	// deduplicates a job that GitHub's broker fans out to multiple sibling sessions
	// under a concurrent burst: the fan-out delivers distinct RunnerRequestIDs but
	// one shared planID, so the first goroutine to claim the planID provisions it,
	// and siblings that see it already claimed skip provisioning (and recycle their
	// runner) rather than colliding on the shared "job-<planID>" worker Secret
	// (Q260).
	//
	// A claim's expireAt is the zero Time while the job is in-flight (held until the
	// winning goroutine concludes). On conclusion the entry is not deleted
	// immediately but retained with a future expiry — ClaimLinger past completion —
	// so a LATE GitHub redelivery of an already-completed planID is still deduped
	// while the winner's terminal-but-not-yet-reaped worker pod lingers. Without the
	// linger the redelivery would pass the (freshly released) claim gate,
	// re-provision, and collide on `create Pod runner-…-<planID>` with the winner's
	// Completed pod (the Q260 redelivery residual). Expired lingering entries are
	// swept lazily on the next claim, so the map holds only in-flight jobs plus
	// those completed within the trailing ClaimLinger window.
	//
	// Each claim also records the deduped sibling deliveries (so the winner can fan
	// completion out to them, Q260 Option A) and, once concluded, the winner's
	// terminal result (so a late redelivery within the linger window resolves with
	// the same result rather than dangling).
	jobClaims map[string]*jobClaim
	// ClaimLinger is how long a planID claim is retained after the owning goroutine
	// releases it (see jobClaims). It is sized to the owner's completedPodTTL — the
	// window during which a Completed worker pod lingers before the reaper GCs it —
	// so the claim outlives the pod it names. Zero (the owner deletes terminal pods
	// synchronously on completion, so none linger) keeps the original
	// delete-on-release behavior. Set once after NewMultiplexer, before Start.
	ClaimLinger time.Duration
	// now returns the current time; nil means time.Now. A test seam for driving the
	// ClaimLinger expiry deterministically.
	now func() time.Time
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
		jobClaims:  make(map[string]*jobClaim),
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

// claimJob reserves exclusive provisioning of planID within this RunnerGroup and,
// for a deduped sibling, reconciles its delivery against the winner (Q260 Option A).
// delivery describes THIS caller's own per-delivery assignment so the winner can
// complete it on GitHub's books if this caller loses the claim.
//
// The returned ClaimResult.Won is false when the claim is still held — either a
// sibling goroutine is provisioning this planID right now (a duplicate broker
// delivery of the same job under a concurrent burst), or the job already completed
// but its claim is still lingering because the winner's terminal worker pod has not
// yet been reaped (a LATE GitHub redelivery). A loser must skip provisioning and
// recycle its runner instead of colliding on the shared "job-<planID>" Secret or
// the lingering "runner-…-<planID>" pod. For a loser whose winner is still running,
// this delivery is registered so the winner completes it on finish; for a loser
// whose planID has already concluded, ClaimResult.LateResult carries the winner's
// terminal result so the caller resolves its own delivery immediately.
//
// On Won=true the returned Complete must be called exactly once when the job
// finishes or is abandoned; it is idempotent. Complete records the winner's
// terminal result on the claim (so a late redelivery within the linger window
// resolves with the same result), transitions the claim into its ClaimLinger window
// (or frees it immediately when ClaimLinger is zero — the owner reaps terminal pods
// synchronously), and returns the deduped sibling deliveries registered so far so
// the winner can fan completion out to each.
func (m *Multiplexer) claimJob(planID string, delivery SiblingDelivery) ClaimResult {
	m.jobClaimsMu.Lock()
	defer m.jobClaimsMu.Unlock()
	// Drop lingering claims whose pod-linger window has elapsed, so a still-present
	// entry always denies (in-flight, or completed within the trailing window).
	m.sweepExpiredClaimsLocked(m.nowFn())
	if c, held := m.jobClaims[planID]; held {
		if c.concluded {
			// Late redelivery after the winner concluded: resolve this delivery with
			// the winner's recorded result during the linger window (the winner is
			// gone, so it cannot complete this one).
			return ClaimResult{LateResult: c.result}
		}
		// The winner is still running: register this deduped sibling so the winner
		// fans completion out to its delivery when the job finishes, and hand back
		// the winner's conclusion signal so this loser can defer its slot recycle
		// until the winner releases its assignment (Q266).
		c.siblings = append(c.siblings, delivery)
		return ClaimResult{WinnerConcluded: c.concludedCh}
	}
	c := &jobClaim{concludedCh: make(chan struct{})} // in-flight (zero expiry: held until Complete)
	m.jobClaims[planID] = c
	var once sync.Once
	complete := func(result broker.TaskResult) []SiblingDelivery {
		var siblings []SiblingDelivery
		once.Do(func() {
			m.jobClaimsMu.Lock()
			defer m.jobClaimsMu.Unlock()
			c.concluded = true
			c.result = result
			siblings = c.siblings
			c.siblings = nil
			// Wake any deduped losers deferring their recycle: the winner has
			// concluded, so it is fanning completjob out to their deliveries now,
			// clearing their recycle 422 (Q266). Closed once under the once guard.
			close(c.concludedCh)
			if m.ClaimLinger <= 0 {
				// No pod lingers after completion (owner reaps synchronously), so
				// free the planID immediately for any redelivery.
				delete(m.jobClaims, planID)
				return
			}
			// Retain the concluded claim for the pod-linger window so a late
			// redelivery is deduped (and resolved with result) rather than colliding
			// on the winner's not-yet-reaped pod.
			c.expireAt = m.nowFn().Add(m.ClaimLinger)
		})
		return siblings
	}
	return ClaimResult{Won: true, Complete: complete}
}

// sweepExpiredClaimsLocked deletes jobClaims entries whose lingering expiry has
// passed (a completed job whose worker pod has since been reaped). In-flight
// entries (zero expiry) are never swept. Caller must hold jobClaimsMu. Deleting
// during a map range is safe in Go, and per-group job rates keep the map small
// (in-flight jobs plus those completed within the trailing ClaimLinger window).
func (m *Multiplexer) sweepExpiredClaimsLocked(now time.Time) {
	for id, c := range m.jobClaims {
		if !c.expireAt.IsZero() && !now.Before(c.expireAt) {
			delete(m.jobClaims, id)
		}
	}
}

// nowFn returns the current time, honouring the test-injected m.now override.
func (m *Multiplexer) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
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
	// Dedup duplicate broker deliveries of one job (same planID) across this
	// group's sibling sessions (Q260). Shared across all goroutines the Multiplexer
	// spawns.
	cfg.ClaimJob = m.claimJob

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
