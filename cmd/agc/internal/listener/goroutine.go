package listener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/karlkfi/github-actions-gateway/githubapp"
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
type JobHandlerFunc func(ctx context.Context, runServiceURL, planID string, payload []byte) error

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
	JobHandler    JobHandlerFunc
	Clock         Clock
	Log           *slog.Logger

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
		return fmt.Errorf("listener: no agent available (pool exhausted)")
	}

	log := cfg.Log.With("group", cfg.Group, "namespace", cfg.Namespace,
		"agentIndex", cfg.Agent.Index)

	// 1. Fetch broker OAuth token for this agent.
	brokerToken, err := githubapp.FetchRunnerOAuthToken(ctx, cfg.Agent.Creds, cfg.Agent.PrivateKey, cfg.HTTPClient)
	if err != nil {
		return fmt.Errorf("fetch runner OAuth token: %w", err)
	}
	cfg.Broker.Token = brokerToken

	// 2. Create a session.
	var sessionID string
	sessionID, err = createSession(ctx, cfg, log)
	if err != nil {
		return err
	}

	defer func() {
		// Best-effort session cleanup on exit.
		dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cfg.Broker.Token = brokerToken // token may have been refreshed above
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
				sessionID, err = createSession(ctx, cfg, log)
				if err != nil {
					return err
				}
				consecutiveEmpty = 0
				pollErrors = 0
				continue
			}

			pollErrors++
			if cfg.Metrics != nil {
				cfg.Metrics.MessagePollErrorsTotal.WithLabelValues(cfg.Namespace, "other").Inc()
			}
			wait := backoffDelay(pollErrors, cfg.Clock)
			log.Warn("GetMessage error", "error", pollErr, "backoff", wait)
			select {
			case <-ctx.Done():
				return nil
			case <-cfg.Clock.After(wait):
			}
			continue
		}

		pollErrors = 0

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

		// 4. Parse the job request body. In M2 we don't decrypt (no session key
		// management in the listener); treat body as opaque and pass through.
		// TODO(milestone-3): decrypt message body using session key.
		log.Info("job message received", "messageId", msg.MessageID)

		if err := handleJob(ctx, cfg, log, sessionID, msg); err != nil {
			log.Error("job handling error", "error", err)
			// Recoverable: continue polling on the same session.
		}
	}
}

// createSession calls CreateSession and handles non-retriable errors.
func createSession(ctx context.Context, cfg Config, log *slog.Logger) (string, error) {
	agentName := fmt.Sprintf("%s-%d", cfg.Group, cfg.Agent.Index)
	sess, err := cfg.Broker.CreateSession(ctx, cfg.Agent.AgentID, agentName, cfg.Agent.RunnerVersion)
	if err != nil {
		var vtooOld *broker.VersionTooOldError
		if errors.As(err, &vtooOld) {
			setCondition(cfg, "RunnerVersionTooOld", metav1.ConditionTrue,
				"VersionTooOld", vtooOld.Message)
			return "", fmt.Errorf("non-retriable: %w", err)
		}
		if isUnauthorized(err) {
			setCondition(cfg, "Degraded", metav1.ConditionTrue,
				"Unauthorized", err.Error())
			return "", fmt.Errorf("non-retriable: %w", err)
		}
		return "", err // retriable
	}
	return sess.SessionID, nil
}

// handleJob acquires a job, notifies the multiplexer, starts the renew loop,
// calls the job handler, and returns. The session is NOT closed after the job.
func handleJob(ctx context.Context, cfg Config, log *slog.Logger, _ string, msg *broker.TaskAgentMessage) error {
	// Treat the message body as the job request payload for M2 stub purposes.
	// A real implementation would decrypt and parse RunnerJobRequestBody here.
	// For M2 the JobHandler receives the raw body bytes and planID="stub".
	payload := []byte(msg.Body)
	planID := "stub"
	runServiceURL := ""

	// Notify multiplexer to spawn a replacement listener before blocking on job handler.
	if cfg.SpawnReplacement != nil {
		cfg.SpawnReplacement(ctx)
	}

	// Start renew loop for this job.
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if runServiceURL == "" {
					continue // M2 stub: no real run service URL
				}
				_, err := cfg.Broker.RenewJob(renewCtx, runServiceURL, broker.RenewJobRequest{
					PlanID: planID,
					JobID:  strconv.FormatInt(msg.MessageID, 10),
				})
				if err != nil {
					if cfg.Metrics != nil {
						cfg.Metrics.RenewJobErrorsTotal.WithLabelValues(cfg.Namespace).Inc()
					}
					log.Warn("RenewJob error (non-fatal)", "error", err)
				}
			}
		}
	}()

	if cfg.Metrics != nil {
		cfg.Metrics.JobsAcquiredTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}

	if cfg.JobHandler != nil {
		return cfg.JobHandler(ctx, runServiceURL, planID, payload)
	}
	return nil
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
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "access denied")
}

func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "session") &&
		(strings.Contains(s, "expired") || strings.Contains(s, "404") || strings.Contains(s, "410"))
}

// backoffDelay returns a jittered delay matching the two-tier policy from
// MessageListener.cs: up to 5 errors → [15s,30s]; beyond 5 → [30s,60s].
func backoffDelay(consecutiveErrors int, _ Clock) time.Duration {
	if consecutiveErrors <= 5 {
		return 15 * time.Second
	}
	return 30 * time.Second
}
