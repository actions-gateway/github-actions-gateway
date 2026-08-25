package listener

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// tokenPropagationMaxAttempts bounds the broker OAuth token-exchange retries a
// freshly recycled agent makes while GitHub's token endpoint still reports its
// just-created registration as "not found" (Q267). Bounded so a registration that
// genuinely never appears cannot spin; the total wait is roughly
// (attempts-1) × TokenPropagationRetryBackoff, well inside the propagation window
// observed in practice (sub-second to a few seconds).
const tokenPropagationMaxAttempts = 6

// refreshBrokerToken fetches a fresh OAuth token and sets it on cfg.Broker.
func refreshBrokerToken(ctx context.Context, cfg Config) error {
	cctx, cancel := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
	defer cancel()
	token, err := githubapp.FetchRunnerOAuthToken(cctx, cfg.Agent.Creds, cfg.Agent.PrivateKey, cfg.HTTPClient)
	if err != nil {
		return fmt.Errorf("refresh broker token: %w", err)
	}
	cfg.Broker.Token = token
	return nil
}

// refreshBrokerTokenAfterRecycle fetches a broker OAuth token for a freshly
// recycled agent, riding out the transient "Registration … was not found" 400
// that GitHub's token endpoint returns in the brief window between
// generate-jitconfig creating the runner record and the OAuth service
// recognizing it (registration propagation, Q267). Without the retry, one such
// 400 kills the listener goroutine and churns its polling slot with a new
// runner record; under a sustained fan-out burst at a wide maxListeners that
// drains the online pool to near zero (the Q259/Q114 wide-pool recycle seam).
// The retry is bounded (attempts + ctx cancellation) and jittered, and re-uses
// the SAME fresh credentials — it never re-registers — so a registration that
// genuinely never appears cannot spin or multiply records; on give-up the
// error is returned and the caller exits (the Multiplexer re-registers).
//
// It is applied only to the fresh, just-registered credentials on the recycle
// path — not to the stored-credential exchange in healSession, where a token
// rejection is the deliberate signal that a single-use JIT record was consumed
// and must be recycled (Q114). Distinguishing the two is why isRegistrationNotFound
// is narrower than isTokenRejected.
func refreshBrokerTokenAfterRecycle(ctx context.Context, cfg Config, log *slog.Logger) error {
	for attempt := 1; ; attempt++ {
		err := refreshBrokerToken(ctx, cfg)
		if err == nil {
			return nil
		}
		if !isRegistrationNotFound(err) || attempt >= tokenPropagationMaxAttempts {
			return err
		}
		if cfg.Metrics != nil {
			cfg.Metrics.BrokerTokenPropagationRetriesTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
		}
		wait := jitterBackoff(cfg.tokenPropagationRetryBackoff())
		log.Debug("broker token exchange: freshly recycled registration not yet propagated; backing off and retrying",
			"attempt", attempt, "maxAttempts", tokenPropagationMaxAttempts, "backoff", wait, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cfg.Clock.After(wait):
		}
	}
}

// Bounds on a detached session DELETE (Q222). sessionDeleteTimeout is the total
// budget across all attempts; it caps how long one goroutine can hold a shutdown
// open, and the manager's drain runs goroutines concurrently so the whole AGC's
// teardown is bounded by it too — comfortably inside controller-runtime's 30s
// GracefulShutdownTimeout. sessionDeleteAttemptTimeout bounds each individual
// round trip so a black-holed connection cannot consume the whole budget.
const (
	sessionDeleteTimeout        = 10 * time.Second
	sessionDeleteAttemptTimeout = 3 * time.Second
	sessionDeleteAttempts       = 3
	sessionDeleteRetryBackoff   = 250 * time.Millisecond
)

// deleteSessionDetached issues a best-effort DELETE for sessionID on a context
// DETACHED from the caller's — never cancelled by the caller's, and bounded by
// sessionDeleteTimeout. It retries a failed DELETE within that budget and reports
// whether the session was actually deleted.
//
// Both properties are load-bearing, not defensive.
//
// Detachment: the heal and recycle paths delete the old session as the first step
// of an ownership handoff, having already cleared the goroutine's own sessionID so
// the exit defer will not double-DELETE (in the v2 flow DELETE is keyed by bearer
// token, so a re-delete could tear down the session the heal just created). On the
// caller's context, a SIGTERM landing in that window would cancel this call
// instantly — and with sessionID already surrendered, nothing downstream would
// ever delete the session: it would leak for the lifetime of the broker-side
// session on every rollout that catches a listener in its post-job recycle.
//
// Retry: this is the only DELETE a session will ever get. A single unretried
// transient failure — a connection reset by the broker as the fleet tears down,
// a pool exhausted by sibling goroutines' long-polls unwinding at once — would
// leak the session just as permanently as no attempt at all.
func deleteSessionDetached(cfg *Config, sessionID string, log *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= sessionDeleteAttempts; attempt++ {
		aCtx, cancelAttempt := context.WithTimeout(ctx, sessionDeleteAttemptTimeout)
		lastErr = cfg.Broker.DeleteSession(aCtx, sessionID)
		cancelAttempt()
		if lastErr == nil {
			return true
		}
		if ctx.Err() != nil {
			break // out of budget
		}
		select {
		case <-ctx.Done():
		case <-time.After(sessionDeleteRetryBackoff):
		}
	}
	if log != nil {
		log.Warn("DeleteSession failed; the broker session is leaked until it expires server-side",
			"sessionId", sessionID, "attempts", sessionDeleteAttempts, "error", lastErr)
	}
	// Count it too: the log line is per-session and easy to miss, while the leak
	// itself is silent — no condition, no event, and the listener recovers into a
	// fresh session as if nothing happened (Q436).
	if cfg.Metrics != nil {
		cfg.Metrics.BrokerSessionLeaksTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}
	return false
}

// sessionState bundles the session ID and its derived AES message-decryption key.
// aesKey is nil when the server did not return an encryption key.
type sessionState struct {
	sessionID string
	aesKey    []byte
}

// createSession calls CreateSession, handles non-retriable errors, and derives
// the AES-256-CBC message key from the server's RSA-encrypted session key.
func createSession(ctx context.Context, cfg Config, log *slog.Logger) (sessionState, error) {
	// The agent's own registered name, not a second derivation of it. The pool
	// kind-scopes the registered name (rs-<set>-<index> for a RunnerSet, Q466) and
	// re-deriving it here from cfg.Group produced the RunnerGroup form for both
	// kinds, so a RunnerSet sent an agent.name/ownerName naming no runner GitHub
	// had registered (Q677). The listener knows no Scheme, so the derivation
	// belongs to the pool alone.
	cctx, cancel := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
	defer cancel()
	sess, err := cfg.Broker.CreateSession(cctx, cfg.Agent.AgentID, cfg.Agent.Name, cfg.Agent.RunnerVersion)
	if err != nil {
		var vtooOld *broker.VersionTooOldError
		if errors.As(err, &vtooOld) {
			setCondition(cfg, v1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionTrue,
				v1alpha1.ReasonVersionTooOld, vtooOld.Message)
			recordEvent(cfg, corev1.EventTypeWarning, "RunnerVersionTooOld", "CreateSession",
				fmt.Sprintf("session creation failed permanently — the runner version is too old for GitHub: %s", vtooOld.Message))
			return sessionState{}, &NonRetriableError{Cause: err}
		}
		if isUnauthorized(err) {
			setCondition(cfg, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				v1alpha1.ReasonSessionUnauthorized, err.Error())
			recordEvent(cfg, corev1.EventTypeWarning, "SessionUnauthorized", "CreateSession",
				fmt.Sprintf("session creation rejected as unauthorized; the agent credentials are invalid or revoked: %v", err))
			return sessionState{}, &NonRetriableError{Cause: err}
		}
		return sessionState{}, err // retriable
	}

	state := sessionState{sessionID: sess.SessionID}

	if len(sess.EncryptionKey) > 0 {
		if sess.EncryptionKeyEncrypted {
			// Session key is RSA-OAEP encrypted; only decryptable with an RSA key.
			// Ed25519 agents receive it unencrypted (EncryptionKeyEncrypted=false)
			// or the broker omits encryption entirely.
			if rsaKey, ok := cfg.Agent.PrivateKey.(*rsa.PrivateKey); ok {
				aesKey, decErr := broker.DecryptSessionKey(sess.EncryptionKey, rsaKey)
				if decErr != nil {
					log.Warn("failed to decrypt session key; messages will be parsed as plaintext", "error", decErr)
				} else {
					state.aesKey = aesKey
				}
			} else {
				log.Warn("server returned RSA-encrypted session key but agent key is not RSA; messages will be parsed as plaintext")
			}
		} else {
			state.aesKey = sess.EncryptionKey
		}
	}

	return state, nil
}

// healSession replaces the goroutine's broker session: best-effort delete of
// the old session, token refresh, and session creation. If the broker rejects
// the agent's stored credentials — at the OAuth exchange or at CreateSession —
// the single-use JIT runner record behind them has been deleted (Q114), so the
// agent is recycled once and the sequence retried with fresh credentials.
// With oldSessionID empty it doubles as session startup. On success cfg.Agent
// may point at a fresh agent.
func healSession(ctx context.Context, cfg *Config, log *slog.Logger, oldSessionID string) (sessionState, error) {
	if oldSessionID != "" {
		deleteSessionDetached(cfg, oldSessionID, log)
	}
	err := refreshBrokerToken(ctx, *cfg)
	if err == nil {
		sess, serr := createSession(ctx, *cfg, log)
		if serr == nil {
			return sess, nil
		}
		if !isUnauthorized(serr) || cfg.RecycleAgent == nil {
			return sessionState{}, serr
		}
		log.Info("session creation unauthorized with stored credentials; recycling single-use agent")
	} else if isTokenRejected(err) && cfg.RecycleAgent != nil {
		log.Info("broker token exchange rejected stored credentials; recycling single-use agent", "error", err)
	} else {
		return sessionState{}, err
	}

	trigger := "stale_session"
	if oldSessionID == "" {
		trigger = "startup"
	}
	return recycleAndRestart(ctx, cfg, log, "", trigger)
}

// recycleAndRestart re-registers the goroutine's consumed agent via
// cfg.RecycleAgent, swaps the fresh agent into cfg, and opens a new session
// with the new credentials. oldSessionID, when non-empty, is deleted
// best-effort first. trigger labels the recycle metric (post_job, startup,
// stale_session). Callers must ensure cfg.RecycleAgent is non-nil.
func recycleAndRestart(ctx context.Context, cfg *Config, log *slog.Logger, oldSessionID, trigger string) (sessionState, error) {
	if oldSessionID != "" {
		deleteSessionDetached(cfg, oldSessionID, log)
	}
	fresh, err := cfg.RecycleAgent(ctx)
	if err != nil {
		if cfg.Metrics != nil {
			cfg.Metrics.AgentRecycleErrorsTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
		}
		return sessionState{}, fmt.Errorf("recycle agent: %w", err)
	}
	if cfg.Metrics != nil {
		cfg.Metrics.AgentRecyclesTotal.WithLabelValues(cfg.Namespace, cfg.Group, trigger).Inc()
	}
	cfg.Agent = fresh
	// The freshly registered runner record may not yet be recognized by GitHub's
	// OAuth token endpoint (generate-jitconfig → OAuth propagation lag), which
	// surfaces as a transient "Registration … was not found" 400. Ride it out with
	// a bounded retry rather than exiting and churning a new record — the wide-pool
	// recycle seam (Q267, the Q259/Q114 family).
	if err := refreshBrokerTokenAfterRecycle(ctx, *cfg, log); err != nil {
		return sessionState{}, err
	}
	sess, err := createSession(ctx, *cfg, log)
	if err == nil && trigger != "post_job" {
		// The heal recovered from a credential rejection that may have set
		// Degraded=True (createSession does so on unauthorized). Clear it so the
		// RunnerGroup does not carry a stale alarm after self-healing.
		setCondition(*cfg, v1alpha1.ConditionDegraded, metav1.ConditionFalse,
			"AgentRecycled", "Re-registered single-use JIT agent after credential rejection")
	}
	return sess, err
}
