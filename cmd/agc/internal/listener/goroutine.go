package listener

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
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

func (realClock) Now() time.Time                        { return time.Now() }
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

	// Broker is a per-goroutine BrokerClient instance. The goroutine sets
	// Broker.Token before each API call via the agent's OAuth credentials.
	Broker     *broker.BrokerClient
	HTTPClient *http.Client // used for OAuth token fetch; nil uses http.DefaultClient

	Conditions    ConditionUpdater
	Metrics       *Metrics
	IdleThreshold int // consecutive 202s before idle shutdown; 0 means 50
	// RenewInterval is the cadence of the per-job RenewJob loop. 0 means 60s.
	RenewInterval time.Duration
	JobHandler    JobHandlerFunc
	Clock         Clock
	Log           *slog.Logger

	// RunnerOS is passed to AcquireJob (e.g. "Linux").
	RunnerOS string

	// IsLastListener returns true if this goroutine is the only running listener
	// for its RunnerGroup. When true, idle shutdown is suppressed.
	IsLastListener func() bool
	// SpawnReplacement requests the Multiplexer to spawn an additional listener
	// after this goroutine acquires a job.
	SpawnReplacement func(ctx context.Context)
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

	// 1. Fetch broker OAuth token for this agent.
	if err := refreshBrokerToken(ctx, cfg); err != nil {
		return err
	}

	// 2. Create a session.
	sess, err := createSession(ctx, cfg, log)
	if err != nil {
		return err
	}
	sessionID := sess.sessionID
	aesKey := sess.aesKey

	defer func() {
		// Best-effort session cleanup on exit.
		dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if delErr := cfg.Broker.DeleteSession(dCtx, sessionID); delErr != nil {
			log.Warn("DeleteSession failed on goroutine exit", "error", delErr)
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

			// Check for session-expired error — recreate session instead of exiting.
			if isSessionExpired(pollErr) {
				log.Info("session expired; recreating", "sessionId", sessionID)
				_ = cfg.Broker.DeleteSession(ctx, sessionID) // best-effort
				// Refresh token before recreating session.
				if err := refreshBrokerToken(ctx, cfg); err != nil {
					return err
				}
				newSess, err := createSession(ctx, cfg, log)
				if err != nil {
					return err
				}
				sessionID = newSess.sessionID
				aesKey = newSess.aesKey
				consecutiveEmpty = 0
				pollErrors = 0
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

		// Successful poll — reset rate-limit tracking and error counter.
		pollErrors = 0
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

		if err := handleJob(ctx, cfg, log, aesKey, msg); err != nil {
			log.Error("job handling error", "error", err)
			// Recoverable: continue polling on the same session.
		}
	}
}

// refreshBrokerToken fetches a fresh OAuth token and sets it on cfg.Broker.
func refreshBrokerToken(ctx context.Context, cfg Config) error {
	token, err := githubapp.FetchRunnerOAuthToken(ctx, cfg.Agent.Creds, cfg.Agent.PrivateKey, cfg.HTTPClient)
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
	sess, err := cfg.Broker.CreateSession(ctx, cfg.Agent.AgentID, agentName, cfg.Agent.RunnerVersion)
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
// calls the job handler, and returns. The session is NOT closed after the job.
// aesKey is the AES-256-CBC key derived from the session's encryptionKey; nil
// means no encryption and the body is parsed as plaintext JSON.
func handleJob(ctx context.Context, cfg Config, log *slog.Logger, aesKey []byte, msg *broker.TaskAgentMessage) error {
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

	// Call AcquireJob if we have a runServiceURL.
	if runServiceURL != "" {
		resp, rawBytes, err := cfg.Broker.AcquireJob(ctx, runServiceURL, broker.JobAcquisitionRequest{
			JobMessageID:   jobBody.RunnerRequestID,
			RunnerOS:       cfg.RunnerOS,
			BillingOwnerID: jobBody.BillingOwnerID,
		})
		if err != nil {
			if cfg.Metrics != nil {
				cfg.Metrics.JobAcquisitionErrors.WithLabelValues(cfg.Namespace, "acquirejob_failed").Inc()
			}
			log.Error("AcquireJob failed", "error", err)
			return err
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
	stop := StartRenewLoop(ctx, cfg.Broker, runServiceURL, planID, jobID,
		cfg.Metrics, cfg.Namespace, cfg.Clock, log, renewInterval)
	defer stop()

	if cfg.Metrics != nil {
		cfg.Metrics.JobsAcquiredTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}

	if cfg.JobHandler != nil {
		return cfg.JobHandler(ctx, runServiceURL, planID, payload, cfg.Agent.EncodedJITConfig)
	}
	return nil
}

// StartRenewLoop starts a per-job renewal goroutine that ticks on the given interval.
// The returned stop function cancels the loop and blocks until it exits;
// callers must call it when the job completes to avoid goroutine leaks.
func StartRenewLoop(
	ctx context.Context,
	client *broker.BrokerClient,
	runServiceURL, planID, jobID string,
	metrics *Metrics,
	namespace string,
	clk Clock,
	log *slog.Logger,
	renewInterval time.Duration,
) (stop func()) {
	stopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return func() { cancel(); <-done }
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

// BackoffDelay returns a jittered delay matching the two-tier policy from
// MessageListener.cs: up to 5 errors → [15s,30s); beyond 5 → [30s,60s).
func BackoffDelay(consecutiveErrors int, _ Clock) time.Duration {
	if consecutiveErrors <= 5 {
		return 15*time.Second + time.Duration(rand.Int63n(int64(15*time.Second))) //nolint:gosec // jitter, not crypto
	}
	return 30*time.Second + time.Duration(rand.Int63n(int64(30*time.Second))) //nolint:gosec // jitter, not crypto
}
