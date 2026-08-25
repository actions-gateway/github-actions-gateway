package listener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// staleEOFThreshold is the number of consecutive GetMessage 200-with-empty-body
// responses (JSON decode EOF) after which the session is treated as stale and
// healed. GitHub serves this signature when the session's single-use JIT
// runner record has been deleted (Q114); a lower count could be a transient
// network blip, which the generic backoff absorbs without re-registration
// traffic.
const staleEOFThreshold = 3

// Run executes the listener goroutine. It blocks until the context is cancelled
// or an unrecoverable error occurs (VersionTooOldError, unauthorized).
// The caller (Multiplexer) is responsible for restarting it after a recoverable exit.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Clock == nil {
		cfg.Clock = RealClock
	}
	if cfg.IdleThreshold == 0 {
		cfg.IdleThreshold = 50
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Agent == nil {
		return &NonRetriableError{Cause: fmt.Errorf("pool exhausted: no agent available")}
	}

	// baseLog carries the per-listener correlation fields (group, namespace,
	// agentIndex). log adds the session-scoped sessionId once a session exists and
	// is rebound on every heal/recycle, so every line beneath it traces back to
	// the live session — making one session→job→pod followable in a log pipeline
	// (Q87, Theme F).
	baseLog := cfg.Log.With("group", cfg.Group, "namespace", cfg.Namespace,
		"agentIndex", cfg.Agent.Index)

	// 1+2. Fetch a broker OAuth token and create a session. healSession with no
	// prior session is exactly that, plus one agent-recycle retry if the stored
	// credentials are rejected — the signature of a single-use JIT agent that
	// was consumed before a restart (Q114).
	sess, err := healSession(ctx, &cfg, baseLog, "")
	if err != nil {
		return err
	}
	sessionID := sess.sessionID
	aesKey := sess.aesKey
	log := baseLog.With("sessionId", sessionID)

	defer func() {
		// Session cleanup on exit, on a context detached from the (by now usually
		// cancelled) reconcile context. sessionID is empty while a heal owns the
		// session handoff; re-deleting would double-DELETE — and in the v2 flow,
		// where DELETE is keyed by bearer token, could tear down another
		// goroutine's session. The handoff runs the same detached, retrying delete
		// (deleteSessionDetached), so the session is deleted exactly once on every
		// exit path, cancellation included (Q222).
		if sessionID != "" {
			deleteSessionDetached(&cfg, sessionID, log)
		}
		if cfg.Metrics != nil {
			cfg.Metrics.ActiveSessions.WithLabelValues(cfg.Namespace, cfg.Group).Dec()
		}
	}()

	if cfg.Metrics != nil {
		cfg.Metrics.ActiveSessions.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}
	// Per-session lifecycle line: one per listener spawn, so kept at Debug to hold
	// down log volume at thousands of concurrent sessions (Q87, Theme D).
	log.Debug("listener goroutine started")

	// Publish the healthy baseline for the session-failure conditions this goroutine
	// owns (Q332), mirroring the ScaleSet listener. Unconditional (not transition-
	// guarded): an abnormal episode may have been surfaced by a PREVIOUS goroutine
	// instance that then exited — createSession pushes Degraded=True/Unauthorized and
	// returns a NonRetriableError, and the Multiplexer restarts a fresh goroutine that
	// carries no in-memory flag for it. Without the baseline that Degraded=True would
	// sit stale on the owner forever after the credentials were fixed.
	setCondition(cfg, v1alpha1.ConditionDegraded, metav1.ConditionFalse,
		v1alpha1.ReasonSessionAuthorized, "listener session established")
	setCondition(cfg, v1alpha1.ConditionRateLimited, metav1.ConditionFalse,
		v1alpha1.ReasonPollingHealthy, "message polling healthy")
	// RunnerVersionTooOld joins the baseline for the same reason and needs it more
	// (Q795): agent.version is the AGC's own compile-time pin, so a session-sourced
	// True is fixed by upgrading the gateway — which restarts this process and clears
	// every in-memory flag while the condition survives in the owner's status. The
	// session reaching here is GitHub having accepted that version. The reconciler
	// drops this push when a live condition stands whose reason is not the listener's
	// own, so the unconditional form cannot overwrite a worker-image verdict (Q715).
	setCondition(cfg, v1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionFalse,
		v1alpha1.ReasonVersionAccepted, "GitHub accepted the runner version at session creation")

	// 3. Poll loop.
	consecutiveEmpty := 0
	pollErrors := 0
	staleEOFs := 0
	var firstRateLimitAt time.Time
	// rateLimitedActive tracks whether RateLimited=True has been published for the
	// current sustained-429 episode, so it is pushed once per episode and cleared on
	// recovery (Q332) rather than left stale until the process restarts.
	rateLimitedActive := false

	for {
		if ctx.Err() != nil {
			return nil
		}

		polledAt := time.Now()
		msg, pollErr := cfg.Broker.GetMessage(ctx, sessionID)
		if pollErr != nil {
			if ctx.Err() != nil {
				return nil
			}

			// A long-poll that stalled past the broker client's
			// ResponseHeaderTimeout — a black-holed connection the broker accepts
			// but never answers — surfaces here as a client-side timeout. It is
			// benign: treat it like an empty poll and retry, without escalating
			// backoff or healing the session (Q108). The bound itself lives in the
			// broker client's tuned HTTPClient (broker.NewHTTPClient); without it
			// the goroutine would block on a single GetMessage for the multi-minute
			// OS TCP timeout, wedging this listener.
			if isPollTimeout(pollErr) {
				if cfg.Metrics != nil {
					cfg.Metrics.MessagePollErrorsTotal.WithLabelValues(cfg.Namespace, "timeout").Inc()
				}
				log.Debug("GetMessage long-poll timed out; retrying", "error", pollErr)
				continue
			}

			var rlErr *broker.RateLimitError
			if errors.As(pollErr, &rlErr) {
				if cfg.Metrics != nil {
					cfg.Metrics.MessagePollErrorsTotal.WithLabelValues(cfg.Namespace, "rate_limited").Inc()
				}
				// Track sustained rate limiting; surface condition after 10 min, once
				// per episode (Q332 — the next successful poll clears it).
				if firstRateLimitAt.IsZero() {
					firstRateLimitAt = cfg.Clock.Now()
				} else if !rateLimitedActive && cfg.Clock.Now().Sub(firstRateLimitAt) >= 10*time.Minute {
					rateLimitedActive = true
					setCondition(cfg, v1alpha1.ConditionRateLimited, metav1.ConditionTrue,
						v1alpha1.ReasonSustainedRateLimit, "GetMessage returning 429 for >10 minutes")
				}
				wait := rlErr.RetryAfter
				if wait < 0 {
					wait = 30 * time.Second
				}
				select {
				case <-ctx.Done():
					return nil
				case <-cfg.Clock.After(wait):
				}
				continue
			}

			// Classify session-level failures that need a heal rather than plain
			// backoff: 404/410 (session expired), 401/403 (expired broker token
			// or a dead single-use agent — healSession sorts out which), and a
			// run of 200-with-empty-body responses (GitHub's deleted-JIT-runner
			// signature, Q114). healSession recreates the session and escalates
			// to an agent recycle only when fresh credentials are still rejected.
			healReason := ""
			switch {
			case isSessionExpired(pollErr):
				healReason = "session expired"
			case isUnauthorized(pollErr):
				healReason = "unauthorized"
			case isDecodeEOF(pollErr):
				staleEOFs++
				if staleEOFs >= staleEOFThreshold {
					healReason = "repeated empty 200 responses"
				}
			}
			if healReason != "" {
				// Per-session heal event; sessionId is already on the logger context.
				// Debug to keep steady-state heal churn out of info volume (Q87, Theme D).
				log.Debug("healing stale session", "reason", healReason, "error", pollErr)
				// Hand session ownership to the heal: it deletes the old session
				// up front, so the exit defer must not re-delete it if the heal
				// fails partway.
				oldSession := sessionID
				sessionID = ""
				newSess, healErr := healSession(ctx, &cfg, log, oldSession)
				if healErr != nil {
					if ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("heal stale session: %w", healErr)
				}
				sessionID = newSess.sessionID
				aesKey = newSess.aesKey
				log = baseLog.With("sessionId", sessionID)
				consecutiveEmpty = 0
				pollErrors = 0
				staleEOFs = 0
				firstRateLimitAt = time.Time{}
				continue
			}

			pollErrors++
			if cfg.Metrics != nil {
				cfg.Metrics.MessagePollErrorsTotal.WithLabelValues(cfg.Namespace, "other").Inc()
			}
			wait := BackoffDelay(pollErrors, cfg.Clock)
			log.Warn("GetMessage error", "error", pollErr, "backoff", wait)
			select {
			case <-ctx.Done():
				return nil
			case <-cfg.Clock.After(wait):
			}
			continue
		}

		// Successful poll — reset rate-limit tracking and error counters. Clear a
		// RateLimited=True published for the now-recovered episode (Q332), mirroring
		// the ScaleSet listener's clear-on-recovery so the condition does not sit
		// stale until the process restarts.
		pollErrors = 0
		staleEOFs = 0
		firstRateLimitAt = time.Time{}
		if rateLimitedActive {
			rateLimitedActive = false
			setCondition(cfg, v1alpha1.ConditionRateLimited, metav1.ConditionFalse,
				v1alpha1.ReasonPollingHealthy, "message polling recovered")
		}

		if msg == nil {
			// 202 — no job queued.
			consecutiveEmpty++
			if !keepPollingAfterEmpty(ctx, cfg, log, consecutiveEmpty, time.Since(polledAt)) {
				return nil // idle exit or cancellation; Multiplexer will not restart this one
			}
			continue
		}

		if msg.MessageType != "RunnerJobRequest" {
			log.Debug("ignoring non-job message", "type", msg.MessageType)
			continue
		}

		// Reset idle counter on job delivery.
		consecutiveEmpty = 0

		// One per delivered job — dominates volume at scale, so Debug (Q87, Theme D).
		log.Debug("job message received", "messageId", msg.MessageID)

		// Leaving the poll loop to run a job: stop counting as a poller so a
		// sibling that is the genuine last poller is not allowed to idle-exit
		// while this goroutine is busy (Q152). Re-counted as a poller at the
		// bottom of the loop once the job (and any recycle) completes.
		if cfg.SetPolling != nil {
			cfg.SetPolling(false)
		}

		acquired, jobErr := handleJob(ctx, cfg, log, aesKey, msg)
		if jobErr != nil {
			log.Error("job handling error", "error", jobErr)
			// Recoverable: continue polling.
		}

		if acquired && cfg.RecycleAgent != nil {
			// JIT runners are single-use: the acquisition consumed this agent's
			// runner record server-side and the session dies with it — polling on
			// would degrade into empty-200/401 loops forever (Q114). Re-register
			// the agent and open a fresh session; the goroutine keeps its
			// listener slot throughout, so maxListeners capacity is preserved.
			// One per completed job; sessionId is already on the logger context.
			// Debug to keep the per-job recycle churn out of info volume (Q87, Theme D).
			log.Debug("job finished; recycling single-use JIT agent")
			// As in the poll-loop heal: the recycle deletes the old session up
			// front, so the exit defer must not re-delete it on failure.
			oldSession := sessionID
			sessionID = ""
			newSess, healErr := recycleAndRestart(ctx, &cfg, log, oldSession, "post_job")
			if healErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("post-job agent recycle: %w", healErr)
			}
			sessionID = newSess.sessionID
			aesKey = newSess.aesKey
			log = baseLog.With("sessionId", sessionID)
			staleEOFs = 0
		}

		// Back in the poll loop: count as a poller again. Any path above that
		// could not return to polling (recycle error) already returned from Run,
		// and the Multiplexer reconciles the poller count on goroutine exit.
		if cfg.SetPolling != nil {
			cfg.SetPolling(true)
		}
	}
}

// keepPollingAfterEmpty resolves a 202: it decides idle shutdown, then paces the
// empty path so a server that did not actually hold the poll cannot spin the
// loop (broker.MinPollInterval). elapsed is how long the poll itself took, so a
// real long-poll already outlasts the floor and waits for nothing. It reports
// whether to poll again; false is an idle exit or a cancelled context.
//
// The floor is applied after the idle decision, never before it, so it can
// neither delay nor suppress an idle exit (Q152).
func keepPollingAfterEmpty(ctx context.Context, cfg Config, log *slog.Logger,
	consecutiveEmpty int, elapsed time.Duration) bool {
	if consecutiveEmpty >= cfg.IdleThreshold && (cfg.IsLastPoller == nil || !cfg.IsLastPoller()) {
		// One per idle listener exit — high-cardinality per-session noise, so
		// Debug (Q87, Theme D).
		log.Debug("idle shutdown: consecutive empty polls reached threshold", "count", consecutiveEmpty)
		return false
	}
	return broker.PaceEmptyPoll(ctx, elapsed)
}
