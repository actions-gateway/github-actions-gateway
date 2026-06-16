package listener

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the production Clock implementation.
var RealClock Clock = realClock{}

// ConditionUpdater submits RunnerGroup condition updates to the reconciler.
// Implementations must be non-blocking.
type ConditionUpdater interface {
	SetCondition(namespace, name string, cond metav1.Condition)
}

// JobHandlerFunc is called with the AcquireJob response bytes after a successful
// acquisition. In M2 this is a stub; in M3 it becomes the pod provisioner.
//
// jitConfig is the agent's raw encoded JIT config blob (the base64-encoded JSON
// map of runner config files from GitHub's generate-jitconfig endpoint). The
// provisioner forwards it into the worker Secret so the entrypoint wrapper can
// materialize .runner / .credentials / .credentials_rsaparams in /home/runner/
// before invoking Runner.Worker. May be empty when the agent was created by a
// registrar that does not produce a JIT blob (e.g. stub-only tests).
type JobHandlerFunc func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) error

// Config holds the dependencies injected into a listener goroutine.
type Config struct {
	Group     string // RunnerGroup name
	Namespace string
	Agent     *agentpool.Agent

	// Broker is a per-goroutine Client instance. The goroutine sets
	// Broker.Token before each API call via the agent's OAuth credentials.
	Broker     *broker.Client
	HTTPClient *http.Client // used for OAuth token fetch; nil uses a bounded httpx.NewClient()

	Conditions    ConditionUpdater
	Metrics       *Metrics
	IdleThreshold int // consecutive 202s before idle shutdown; 0 means 50
	// RenewInterval is the cadence of the per-job RenewJob loop. 0 means 60s.
	RenewInterval time.Duration
	// ControlPlaneTimeout bounds each non-long-poll broker call on the
	// session-establishment path — the OAuth token exchange and CreateSession —
	// so a slow or unresponsive broker cannot wedge the goroutine indefinitely.
	// Without it those calls inherit only the long-lived manager context (the
	// broker's long-poll client carries no overall read deadline by design), so a
	// broker that accepts the connection but is slow to respond — e.g. an overloaded
	// shared fakegithub under parallel CI load — blocks the goroutine inside a
	// single attempt and the RunnerGroup never registers a session (Q134). With
	// a deadline the call fails fast and retriably, so the Multiplexer restarts
	// the baseline and retries. Zero selects defaultControlPlaneTimeout. The
	// GetMessage long-poll is deliberately excluded — it holds the connection
	// open for the broker's poll interval by design.
	ControlPlaneTimeout time.Duration
	JobHandler          JobHandlerFunc
	Clock               Clock
	Log                 *slog.Logger

	// RunnerOS is passed to AcquireJob (e.g. "Linux").
	RunnerOS string

	// IsLastListener returns true if this goroutine is the only running listener
	// for its RunnerGroup. When true, idle shutdown is suppressed.
	IsLastListener func() bool
	// SpawnReplacement requests the Multiplexer to spawn an additional listener
	// after this goroutine acquires a job.
	SpawnReplacement func(ctx context.Context)
	// ReleaseAgent returns this goroutine's claimed pool agent to the available
	// pool when the goroutine exits. The Multiplexer invokes it exactly once after
	// Run returns. Without it a pool agent is leaked on every goroutine exit (idle
	// shutdown, error, or cancellation), so the pool is permanently exhausted after
	// maxListeners total spawns — and the permanent baseline can no longer reclaim
	// an agent to restart, draining the RunnerGroup to zero listeners. Nil for a
	// goroutine that never claimed an agent (pool exhausted at spawn).
	ReleaseAgent func()
	// MarkAgentConsumed records on the agent pool that this goroutine's
	// single-use JIT runner record has been spent by a job acquisition (Q114).
	// Called immediately after AcquireJob succeeds, before the job handler
	// blocks, so the pool parks the agent rather than re-issuing its dead
	// credentials if this goroutine exits without recycling. Nil disables the
	// bookkeeping (stub-only tests).
	MarkAgentConsumed func()
	// RecycleAgent re-registers this goroutine's agent under its stable name
	// after its single-use JIT runner record was consumed, and returns the
	// fresh agent (Q114). The goroutine swaps it into its Config and opens a
	// new session, so the listener slot is never released. Nil disables
	// self-healing: after a job the goroutine keeps polling its old session
	// (pre-Q114 behavior, appropriate for stub registrars whose agents are not
	// single-use).
	RecycleAgent func(ctx context.Context) (*agentpool.Agent, error)
}

// staleEOFThreshold is the number of consecutive GetMessage 200-with-empty-body
// responses (JSON decode EOF) after which the session is treated as stale and
// healed. GitHub serves this signature when the session's single-use JIT
// runner record has been deleted (Q114); a lower count could be a transient
// network blip, which the generic backoff absorbs without re-registration
// traffic.
const staleEOFThreshold = 3

// defaultControlPlaneTimeout is the per-call deadline applied to the listener's
// non-long-poll broker operations (OAuth token exchange, CreateSession) when
// Config.ControlPlaneTimeout is unset. 30s is generous for a healthy round-trip
// to GitHub yet tight enough that several retries fit inside the e2e
// session-registration budget when the broker stalls (Q134).
const defaultControlPlaneTimeout = 30 * time.Second

// controlPlaneTimeout returns the per-call deadline for the goroutine's
// non-long-poll broker operations, defaulting when unset.
func (cfg Config) controlPlaneTimeout() time.Duration {
	if cfg.ControlPlaneTimeout > 0 {
		return cfg.ControlPlaneTimeout
	}
	return defaultControlPlaneTimeout
}

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

	log := cfg.Log.With("group", cfg.Group, "namespace", cfg.Namespace,
		"agentIndex", cfg.Agent.Index)

	// 1+2. Fetch a broker OAuth token and create a session. healSession with no
	// prior session is exactly that, plus one agent-recycle retry if the stored
	// credentials are rejected — the signature of a single-use JIT agent that
	// was consumed before a restart (Q114).
	sess, err := healSession(ctx, &cfg, log, "")
	if err != nil {
		return err
	}
	sessionID := sess.sessionID
	aesKey := sess.aesKey

	defer func() {
		// Best-effort session cleanup on exit. sessionID is empty while a heal
		// owns the session handoff (it has already deleted the old session);
		// re-deleting would double-DELETE — and in the v2 flow, where DELETE is
		// keyed by bearer token, could tear down another goroutine's session.
		if sessionID != "" {
			dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if delErr := cfg.Broker.DeleteSession(dCtx, sessionID); delErr != nil {
				log.Warn("DeleteSession failed on goroutine exit", "error", delErr)
			}
		}
		if cfg.Metrics != nil {
			cfg.Metrics.ActiveSessions.WithLabelValues(cfg.Namespace, cfg.Group).Dec()
		}
	}()

	if cfg.Metrics != nil {
		cfg.Metrics.ActiveSessions.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}
	log.Info("listener goroutine started", "sessionId", sessionID)

	// 3. Poll loop.
	consecutiveEmpty := 0
	pollErrors := 0
	staleEOFs := 0
	var firstRateLimitAt time.Time

	for {
		if ctx.Err() != nil {
			return nil
		}

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
				// Track sustained rate limiting; surface condition after 10 min.
				if firstRateLimitAt.IsZero() {
					firstRateLimitAt = cfg.Clock.Now()
				} else if cfg.Clock.Now().Sub(firstRateLimitAt) >= 10*time.Minute {
					setCondition(cfg, "RateLimited", metav1.ConditionTrue,
						"SustainedRateLimit", "GetMessage returning 429 for >10 minutes")
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
				log.Info("healing stale session", "reason", healReason, "error", pollErr, "sessionId", sessionID)
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

		// Successful poll — reset rate-limit tracking and error counters.
		pollErrors = 0
		staleEOFs = 0
		firstRateLimitAt = time.Time{}

		if msg == nil {
			// 202 — no job queued.
			consecutiveEmpty++
			if consecutiveEmpty >= cfg.IdleThreshold {
				if cfg.IsLastListener == nil || !cfg.IsLastListener() {
					log.Info("idle shutdown: consecutive empty polls reached threshold", "count", consecutiveEmpty)
					return nil // idle exit; Multiplexer will not restart this one
				}
			}
			continue
		}

		if msg.MessageType != "RunnerJobRequest" {
			log.Debug("ignoring non-job message", "type", msg.MessageType)
			continue
		}

		// Reset idle counter on job delivery.
		consecutiveEmpty = 0

		log.Info("job message received", "messageId", msg.MessageID)

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
			log.Info("job finished; recycling single-use JIT agent", "sessionId", sessionID)
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
			staleEOFs = 0
		}
	}
}

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

// NonRetriableError wraps an error from Run that indicates a permanent failure
// condition for this goroutine (e.g. version too old, unauthorized). The
// Multiplexer uses this to suppress automatic restart of the permanent baseline.
type NonRetriableError struct {
	Cause error
}

func (e *NonRetriableError) Error() string { return "non-retriable: " + e.Cause.Error() }
func (e *NonRetriableError) Unwrap() error { return e.Cause }

// sessionState bundles the session ID and its derived AES message-decryption key.
// aesKey is nil when the server did not return an encryption key.
type sessionState struct {
	sessionID string
	aesKey    []byte
}

// createSession calls CreateSession, handles non-retriable errors, and derives
// the AES-256-CBC message key from the server's RSA-encrypted session key.
func createSession(ctx context.Context, cfg Config, log *slog.Logger) (sessionState, error) {
	agentName := fmt.Sprintf("%s-%d", cfg.Group, cfg.Agent.Index)
	cctx, cancel := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
	defer cancel()
	sess, err := cfg.Broker.CreateSession(cctx, cfg.Agent.AgentID, agentName, cfg.Agent.RunnerVersion)
	if err != nil {
		var vtooOld *broker.VersionTooOldError
		if errors.As(err, &vtooOld) {
			setCondition(cfg, "RunnerVersionTooOld", metav1.ConditionTrue,
				"VersionTooOld", vtooOld.Message)
			return sessionState{}, &NonRetriableError{Cause: err}
		}
		if isUnauthorized(err) {
			setCondition(cfg, "Degraded", metav1.ConditionTrue,
				"Unauthorized", err.Error())
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

// handleJob acquires a job, notifies the multiplexer, starts the renew loop,
// calls the job handler, and returns. acquired reports whether AcquireJob
// succeeded — the point at which GitHub considers the single-use JIT runner
// record spent (Q114); the caller recycles the agent afterwards. The session
// itself is NOT closed here. aesKey is the AES-256-CBC key derived from the
// session's encryptionKey; nil means no encryption and the body is parsed as
// plaintext JSON.
func handleJob(ctx context.Context, cfg Config, log *slog.Logger, aesKey []byte, msg *broker.TaskAgentMessage) (acquired bool, err error) {
	// Decrypt message body with the session key, then parse as RunnerJobRequestBody.
	bodyBytes := []byte(msg.Body)
	if aesKey != nil {
		decrypted, err := broker.DecryptMessageBody(msg.Body, aesKey)
		if err != nil {
			log.Warn("failed to decrypt message body; falling back to plaintext parse", "error", err)
		} else {
			bodyBytes = decrypted
		}
	}

	var jobBody broker.RunnerJobRequestBody
	if err := json.Unmarshal(bodyBytes, &jobBody); err != nil {
		log.Warn("could not parse job body; skipping AcquireJob", "error", err)
	}

	var (
		payload       []byte
		planID        = "stub"
		runServiceURL = jobBody.RunServiceURL
	)

	// Call AcquireJob if we have a runServiceURL. Bounded by the control-plane
	// timeout for the same reason as createSession: it is a short request/response
	// call (not the long-poll), so an unresponsive broker here must not wedge the
	// goroutine — that would block job pickup and the worker pod would never spawn
	// (Q134 class). A timeout surfaces as a recoverable AcquireJob error; the poll
	// loop logs it and continues, re-acquiring on the next delivery.
	if runServiceURL != "" {
		acqCtx, cancelAcq := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
		resp, rawBytes, acqErr := cfg.Broker.AcquireJob(acqCtx, runServiceURL, broker.JobAcquisitionRequest{
			JobMessageID:   jobBody.RunnerRequestID,
			RunnerOS:       cfg.RunnerOS,
			BillingOwnerID: jobBody.BillingOwnerID,
		})
		cancelAcq()
		if acqErr != nil {
			if cfg.Metrics != nil {
				cfg.Metrics.JobAcquisitionErrors.WithLabelValues(cfg.Namespace, "acquirejob_failed").Inc()
			}
			log.Error("AcquireJob failed", "error", acqErr)
			return false, acqErr
		}
		acquired = true
		// The acquisition just consumed this agent's single-use JIT runner
		// record. Record it on the pool now, before the long job wait, so the
		// agent is parked (not re-issued) if this goroutine dies mid-job (Q114).
		if cfg.MarkAgentConsumed != nil {
			cfg.MarkAgentConsumed()
		}
		planID = resp.Plan.PlanID
		payload = rawBytes
	} else {
		payload = []byte(msg.Body)
	}

	// Notify multiplexer to spawn a replacement listener before blocking on job handler.
	if cfg.SpawnReplacement != nil {
		cfg.SpawnReplacement(ctx)
	}

	// Start renew loop for this job.
	renewInterval := cfg.RenewInterval
	if renewInterval == 0 {
		renewInterval = 60 * time.Second
	}
	jobID := strconv.FormatInt(msg.MessageID, 10)
	stop, renewDone := StartRenewLoop(ctx, cfg.Broker, runServiceURL, planID, jobID,
		cfg.Metrics, cfg.Namespace, cfg.Clock, log, renewInterval)
	// Cancel the renew loop and wait for it to exit before returning, so the
	// goroutine never outlives the job it renews.
	defer func() { stop(); <-renewDone }()

	if cfg.Metrics != nil {
		cfg.Metrics.JobsAcquiredTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}

	if cfg.JobHandler != nil {
		return acquired, cfg.JobHandler(ctx, runServiceURL, planID, payload, cfg.Agent.EncodedJITConfig)
	}
	return acquired, nil
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
		_ = cfg.Broker.DeleteSession(ctx, oldSessionID) // best-effort; usually already dead
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
		_ = cfg.Broker.DeleteSession(ctx, oldSessionID) // best-effort; usually already dead
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
	if err := refreshBrokerToken(ctx, *cfg); err != nil {
		return sessionState{}, err
	}
	sess, err := createSession(ctx, *cfg, log)
	if err == nil && trigger != "post_job" {
		// The heal recovered from a credential rejection that may have set
		// Degraded=True (createSession does so on unauthorized). Clear it so the
		// RunnerGroup does not carry a stale alarm after self-healing.
		setCondition(*cfg, "Degraded", metav1.ConditionFalse,
			"AgentRecycled", "Re-registered single-use JIT agent after credential rejection")
	}
	return sess, err
}

// StartRenewLoop starts a per-job renewal goroutine that ticks on the given interval.
// It returns a stop function that cancels the loop and a done channel that closes
// once the loop goroutine has fully exited. Callers must call stop when the job
// completes to avoid goroutine leaks; they may then wait on done if they need to
// guarantee the goroutine has stopped before releasing shared resources.
func StartRenewLoop(
	ctx context.Context,
	client *broker.Client,
	runServiceURL, planID, jobID string,
	metrics *Metrics,
	namespace string,
	clk Clock,
	log *slog.Logger,
	renewInterval time.Duration,
) (stop func(), done <-chan struct{}) {
	stopCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-stopCtx.Done():
				return
			case <-clk.After(renewInterval):
				if runServiceURL == "" {
					continue // M2 stub: no real run service URL
				}
				_, err := client.RenewJob(stopCtx, runServiceURL, broker.RenewJobRequest{
					PlanID: planID,
					JobID:  jobID,
				})
				if err != nil {
					if metrics != nil {
						metrics.RenewJobErrorsTotal.WithLabelValues(namespace).Inc()
					}
					if log != nil {
						log.Warn("RenewJob error (non-fatal)", "error", err)
					}
				}
			}
		}
	}()
	return cancel, doneCh
}

func setCondition(cfg Config, condType string, status metav1.ConditionStatus, reason, msg string) {
	if cfg.Conditions == nil {
		return
	}
	cfg.Conditions.SetCondition(cfg.Namespace, cfg.Group, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

func isUnauthorized(err error) bool {
	var typed *broker.UnauthorizedError
	return errors.As(err, &typed)
}

func isSessionExpired(err error) bool {
	var typed *broker.SessionExpiredError
	return errors.As(err, &typed)
}

// isPollTimeout reports whether a GetMessage error is a client-side timeout
// rather than a broker-reported status. It fires when the broker client's
// ResponseHeaderTimeout (broker.LongPollResponseHeaderTimeout) elapses on a
// black-holed long-poll — the broker accepts the connection but never answers
// (Q108). The poll loop already returns early on parent-context cancellation, so
// the only timeout that reaches here is the per-request response-header (or
// connect) deadline, which is a benign "no message, retry" — not a session-level
// failure that should trip backoff or a heal.
func isPollTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isDecodeEOF reports whether a GetMessage error is a 200 response whose body
// could not be decoded because it was empty or truncated — observed live as
// GitHub's response signature once a session's single-use JIT runner record
// has been deleted (Q114, M4 §12: "decode response: EOF").
func isDecodeEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// isTokenRejected reports whether a broker OAuth token fetch failed because
// the token service rejected the client credentials (as opposed to a
// transport or server failure). For a single-use JIT agent this happens once
// GitHub deletes the runner record behind the credential (Q114).
func isTokenRejected(err error) bool {
	var typed *githubapp.TokenExchangeError
	if !errors.As(err, &typed) {
		return false
	}
	switch typed.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// 400 covers OAuth's invalid_client convention (RFC 6749 §5.2), which
		// some token services use instead of 401 for unknown clients.
		return true
	default:
		return false
	}
}

// BackoffDelay returns a jittered delay matching the two-tier policy from
// MessageListener.cs: up to 5 errors → [15s,30s); beyond 5 → [30s,60s).
func BackoffDelay(consecutiveErrors int, _ Clock) time.Duration {
	if consecutiveErrors <= 5 {
		return 15*time.Second + time.Duration(rand.Int63n(int64(15*time.Second))) //nolint:gosec // jitter, not crypto
	}
	return 30*time.Second + time.Duration(rand.Int63n(int64(30*time.Second))) //nolint:gosec // jitter, not crypto
}
