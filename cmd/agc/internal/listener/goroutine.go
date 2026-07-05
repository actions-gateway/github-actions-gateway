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
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
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

// EventRecorder records a Kubernetes Event about the owning RunnerGroup/RunnerSet
// (identified by namespace/name). The reconciler drains these and records them on
// the live owner object, so job-lifecycle incidents surface in `kubectl describe`
// and event watchers — complementing the metrics/conditions that already track the
// same state. Like ConditionUpdater, implementations must be non-blocking (drop on
// a full channel) so a listener or provisioner goroutine never blocks on event
// delivery. action and note follow the client-go events API (the "what happened"
// verb and the human-readable message).
type EventRecorder interface {
	Event(namespace, name, eventtype, reason, action, note string)
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
//
// The returned broker.TaskResult is the handler's POD-PHASE PROXY of the job's
// outcome (PodFailed→failed, else succeeded) — NOT the workflow's real
// succeeded/failed, which only the worker's runner binary knows and reports for the
// winner's own delivery. The AGC uses it solely as the result to report when it fans
// completion out to the deduped sibling deliveries of a fanned-out job (Q260 Option
// A); an empty result (an error before the pod reached a terminal phase) is treated
// as succeeded by that fan-out. The error return is the provisioning error as
// before (recoverable; the poll loop logs and continues).
type JobHandlerFunc func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) (broker.TaskResult, error)

// SiblingDelivery identifies one deduped sibling delivery of a fanned-out job so the
// winner can complete it on GitHub's books when its job finishes (Q260 Option A).
// Each field is the sibling's OWN per-delivery value: RunnerRequestID is the
// delivery's job id (distinct per sibling under fan-out), and RunServiceURL /
// JobToken are what completejob needs to resolve that specific assignment.
type SiblingDelivery struct {
	RunnerRequestID string
	RunServiceURL   string
	JobToken        string
}

// ClaimResult is returned by Config.ClaimJob for one delivery of a job (Q260). It
// reports whether this caller won the planID claim — and, for the winner, how to
// reconcile the deduped-away sibling deliveries on GitHub's books when the job
// finishes (Option A).
type ClaimResult struct {
	// Won is true for the first caller to claim planID — the goroutine that
	// provisions and runs the job. False for a deduped sibling (loser).
	Won bool
	// Complete is called exactly once by the winner when its job finishes or is
	// abandoned. It records the winner's terminal result on the claim (so a late
	// redelivery within the linger window resolves with the same result),
	// transitions the claim into its post-completion linger, and returns the deduped
	// sibling deliveries registered so far so the winner can fan completjob out to
	// each. It is idempotent. Nil for a loser.
	Complete func(result broker.TaskResult) []SiblingDelivery
	// LateResult is set for a LOSER whose planID has ALREADY concluded — a late
	// redelivery arriving during the linger window, after the winner is gone. The
	// caller resolves its own delivery immediately with this result rather than
	// waiting for a winner that has already finished. Empty for a winner, or for a
	// loser whose winner is still running (that loser was registered on the claim
	// for the winner to complete).
	LateResult broker.TaskResult
	// WinnerConcluded is set for a LOSER whose winner is STILL RUNNING. It is closed
	// when the winner concludes — the point at which the winner fans completjob out
	// to this loser's delivery (Option A), releasing GitHub's assignment on this
	// deduped runner so its recycle 422 clears. The loser waits on it before
	// recycling its slot rather than recycling eagerly into a 422 that cannot clear
	// for the winner's whole runtime — which would exhaust the bounded Q259 backoff
	// and exit the listener, collapsing the pool under sustained fan-out burst
	// (Q266). Nil for a winner, or for a loser resolved via LateResult.
	WinnerConcluded <-chan struct{}
}

// AdmitFunc gates job acquisition on available worker capacity (Q59). It is
// called after a job is delivered but before AcquireJob claims it from GitHub.
// ok=false means there is no capacity: the listener skips the acquire, leaving
// the job queued at GitHub for redelivery to a sibling session — rather than
// claiming a job whose worker pod it cannot place, which would be cancelled when
// the unrenewed lock lapses. ok=true returns release, which the listener calls
// exactly once when the reserved slot is freed (acquire failure or job
// completion) so the gate's in-flight count tracks only live jobs.
type AdmitFunc func(ctx context.Context) (release func(), ok bool)

// Config holds the dependencies injected into a listener goroutine.
type Config struct {
	Group     string // RunnerGroup name
	Namespace string
	Agent     *agentpool.Agent

	// Broker is a per-goroutine Client instance. The goroutine sets
	// Broker.Token before each API call via the agent's OAuth credentials.
	Broker     *broker.Client
	HTTPClient *http.Client // used for OAuth token fetch; nil uses a bounded httpx.NewClient()

	Conditions ConditionUpdater
	// Events records owner-scoped Kubernetes Events for job-lifecycle incidents that
	// this goroutine detects (acquisition failure, non-retriable session failure).
	// Nil disables event recording (the metric/condition remains the signal).
	Events        EventRecorder
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
	// Admit gates job acquisition on worker capacity (Q59). Called once per
	// delivered job, before AcquireJob: when it returns ok=false the listener
	// skips the acquire and the job stays queued at GitHub for redelivery. On
	// ok=true the returned release func is called when the reserved slot is freed
	// (acquire failure or job completion). Nil disables the gate, leaving the
	// provisioner's post-acquire ceilingCheck as the only (backstop) limit.
	Admit AdmitFunc
	// ClaimJob deduplicates provisioning of one job across the sibling listener
	// goroutines of this RunnerGroup (Q260). Under a concurrent burst GitHub's
	// broker fans one job out to several sibling sessions as messages with
	// DISTINCT RunnerRequestIDs; each sibling acquires its own delivery and the
	// AcquireJob response carries the SAME planID, so without a claim every
	// recipient then races to create the shared per-job worker Secret
	// "job-<planID>" — one wins and the rest collide ("already exists"), fail
	// provisioning, and die with their runner slot already burned (busy but
	// offline, no worker pod). ClaimJob is therefore called with the job's planID
	// AFTER AcquireJob (planID is only known post-acquire) but BEFORE provisioning:
	// ok=false means a sibling in this AGC is already provisioning this planID, so
	// the listener skips provisioning and returns acquired=true, letting the caller
	// recycle its consumed single-use runner back online (slot reclaimed cleanly)
	// rather than colliding on the Secret. On ok=true the returned release is
	// called exactly once when the job finishes or is abandoned; the claim then
	// lingers for the owner's completedPodTTL so a LATE redelivery arriving while
	// the winner's Completed-but-not-yet-reaped worker pod still exists is also
	// deduped (rather than colliding on `create Pod`) — a genuine redelivery after
	// the pod is reaped provisions again. Keying on planID (not the pre-acquire
	// RunnerRequestID, which differs per sibling and so never deduped the fan-out —
	// the ineffective first Q260 fix, c850764) is what collapses the siblings. Nil
	// disables dedup (stub-only tests, or a response with no planID). Passing this
	// caller's own delivery lets the winner reconcile it on GitHub's books under
	// Option A (see ClaimResult, SiblingDelivery, and FanoutCompletion).
	ClaimJob func(planID string, delivery SiblingDelivery) ClaimResult
	// FanoutCompletion, when true, makes the WINNER of a fanned-out job fan
	// completejob out to every deduped sibling delivery when its job finishes (Q260
	// Option A). Under a concurrent burst GitHub fans one logical job (one planID)
	// out to N sibling sessions as N deliveries with distinct RunnerRequestIDs; the
	// planID dedup (ClaimJob) collapses them to ONE runner, but the other N−1
	// acquired deliveries dangle and GitHub cancels the whole job at its ~15-minute
	// unstarted-job timeout even after the winner completed it. When enabled, the
	// winner issues completejob for each tracked sibling — keyed on the sibling's OWN
	// RunnerRequestID and job token — with the winner's pod-phase-proxy result, and a
	// late redelivery arriving during the linger window is resolved with the recorded
	// terminal result. Losers do NOT complete early (that was the rejected #513
	// per-loser-immediate path, live-tested worse than the default).
	//
	// ON BY DEFAULT. The re-route #5 dogfood experiment (2026-07-04) confirmed the
	// run service's completion is per-delivery, not planID-scoped: completejob on a
	// sibling's OWN jobID resolves only that assignment (returns OK, not "already
	// resolved"), while the winner's own delivery still carries the real workflow
	// result reported by its runner binary — so the sibling pod-phase proxy cannot
	// green a red workflow. Previously-wedged concurrent jobs concluded green with
	// the flag on, the Q259 recycle 422 cleared per job on winner completion, and no
	// job cancelled at the ~15-minute unstarted timeout. Opt out with AGC env
	// AGC_FANOUT_COMPLETION=false. The operator runbook is the Q260
	// redelivery-accounting section in docs/operations/troubleshooting.md.
	FanoutCompletion bool
	// LoserRecycleDeferTimeout bounds how long a deduped fan-out loser waits for its
	// winner to conclude before recycling its slot anyway (Q266). Zero selects
	// defaultLoserRecycleDeferTimeout. It is a backstop for a winner that never
	// concludes (a crash or a wedged worker the renew loop somehow does not tear
	// down): GitHub cancels the whole job at its ~15-minute unstarted-job timeout,
	// which releases the loser's assignment, so the bound sits just past that — the
	// loser's recycle 422 has cleared by the time the fallback fires. Overridable in
	// tests to drive the fallback deterministically.
	LoserRecycleDeferTimeout time.Duration
	// TokenPropagationRetryBackoff is the base delay between broker OAuth
	// token-exchange retries after an agent recycle, while GitHub's token endpoint
	// still returns a transient "Registration … was not found" 400 for the
	// just-created runner record (the generate-jitconfig → OAuth-service
	// propagation window, Q267). Zero selects defaultTokenPropagationRetryBackoff.
	// Overridable in tests to drive the retry deterministically. See
	// refreshBrokerTokenAfterRecycle.
	TokenPropagationRetryBackoff time.Duration
	Clock                        Clock
	Log                          *slog.Logger

	// RunnerOS is passed to AcquireJob (e.g. "Linux").
	RunnerOS string

	// IsLastPoller returns true if this goroutine is the only one still
	// long-polling for its RunnerGroup — siblings busy inside JobHandler do not
	// count. When true, idle shutdown is suppressed, so the group never drops to
	// zero pollers while a job is running and stops acquiring work (Q152).
	IsLastPoller func() bool
	// SpawnReplacement requests the Multiplexer to spawn an additional listener
	// after this goroutine acquires a job.
	SpawnReplacement func(ctx context.Context)
	// SetPolling reports this goroutine's poller status to the Multiplexer: false
	// while it executes a job (inside JobHandler and the post-job recycle), true
	// while it long-polls for work. The Multiplexer counts only polling goroutines
	// for the last-poller decision (IsLastPoller), so a busy goroutine is not
	// mistaken for available polling capacity. Nil disables the bookkeeping.
	SetPolling func(polling bool)
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

// tokenPropagationMaxAttempts bounds the broker OAuth token-exchange retries a
// freshly recycled agent makes while GitHub's token endpoint still reports its
// just-created registration as "not found" (Q267). Bounded so a registration that
// genuinely never appears cannot spin; the total wait is roughly
// (attempts-1) × TokenPropagationRetryBackoff, well inside the propagation window
// observed in practice (sub-second to a few seconds).
const tokenPropagationMaxAttempts = 6

// defaultTokenPropagationRetryBackoff is the base inter-attempt delay for the
// recycle token-exchange propagation retry when Config.TokenPropagationRetryBackoff
// is unset. Jittered per attempt (see jitterBackoff) so concurrent recyclers under
// a burst do not resynchronize their retries into a thundering herd.
const defaultTokenPropagationRetryBackoff = 2 * time.Second

// tokenPropagationRetryBackoff returns the base inter-attempt delay for the
// recycle token-exchange propagation retry, defaulting when unset.
func (cfg Config) tokenPropagationRetryBackoff() time.Duration {
	if cfg.TokenPropagationRetryBackoff > 0 {
		return cfg.TokenPropagationRetryBackoff
	}
	return defaultTokenPropagationRetryBackoff
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
	// Per-session lifecycle line: one per listener spawn, so kept at Debug to hold
	// down log volume at thousands of concurrent sessions (Q87, Theme D).
	log.Debug("listener goroutine started")

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
					setCondition(cfg, v1alpha1.ConditionRateLimited, metav1.ConditionTrue,
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

		// Successful poll — reset rate-limit tracking and error counters.
		pollErrors = 0
		staleEOFs = 0
		firstRateLimitAt = time.Time{}

		if msg == nil {
			// 202 — no job queued.
			consecutiveEmpty++
			if consecutiveEmpty >= cfg.IdleThreshold {
				if cfg.IsLastPoller == nil || !cfg.IsLastPoller() {
					// One per idle listener exit — high-cardinality per-session noise,
					// so Debug (Q87, Theme D).
					log.Debug("idle shutdown: consecutive empty polls reached threshold", "count", consecutiveEmpty)
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
// recognizing it (registration propagation, Q267). A single such 400 was
// previously fatal on the recycle path: recycleAndRestart returned it, the
// listener goroutine exited, and its polling slot churned a new runner record —
// and under a sustained fan-out burst at a wide maxListeners enough listeners
// exited that the online pool stayed near zero (the Q259/Q114 wide-pool recycle
// seam). The retry is bounded (attempts + ctx cancellation) and jittered, and
// re-uses the SAME fresh credentials — it never re-registers — so a registration
// that genuinely never appears cannot spin or multiply records; on give-up the
// error is returned and the caller exits exactly as before (the Multiplexer
// re-registers), no worse than the pre-Q267 behaviour.
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

// jitterBackoff returns d with full jitter applied over [d/2, d], so concurrent
// recyclers under a burst do not resynchronize their retries into a thundering
// herd. A non-positive d returns 0.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not crypto
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
			setCondition(cfg, v1alpha1.ConditionRunnerVersionTooOld, metav1.ConditionTrue,
				"VersionTooOld", vtooOld.Message)
			recordEvent(cfg, corev1.EventTypeWarning, "RunnerVersionTooOld", "CreateSession",
				fmt.Sprintf("session creation failed permanently — the runner version is too old for GitHub: %s", vtooOld.Message))
			return sessionState{}, &NonRetriableError{Cause: err}
		}
		if isUnauthorized(err) {
			setCondition(cfg, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				"Unauthorized", err.Error())
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

	// Admission gate (Q59): reserve worker capacity BEFORE AcquireJob claims the
	// job from GitHub. If the gate is full, skip the acquire so the job stays
	// queued at GitHub and is redelivered to a sibling session with capacity —
	// rather than claiming a job whose worker pod we cannot place, which would be
	// cancelled when its unrenewed lock lapses (failure shape 1 in the Q59 plan).
	// admitRelease frees the reserved worker slot; nil when the gate is disabled.
	// The AdmitFunc's closure is idempotent, so the deferred release and any earlier
	// explicit release (the deduped-loser path below) together free it exactly once.
	var admitRelease func()
	if cfg.Admit != nil {
		release, ok := cfg.Admit(ctx)
		if !ok {
			if cfg.Metrics != nil {
				cfg.Metrics.JobsAdmissionRejectedTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
			}
			// Per-delivery line that can be high-volume under sustained capacity
			// pressure; Debug, with the metric as the operator-facing signal (Q87, Theme D).
			log.Debug("job admission rejected: worker capacity full; leaving job queued for redelivery", "messageId", msg.MessageID)
			return false, nil
		}
		admitRelease = release
		// Hold the reservation until handleJob returns. On the acquire path that is
		// pod terminal (JobHandler has returned by then); on any earlier return it
		// fires immediately. Either way the gate's in-flight count tracks only live
		// jobs. Released exactly once via the AdmitFunc's idempotent closure.
		defer release()
	}

	var (
		payload       []byte
		planID        = "stub"
		runServiceURL = jobBody.RunServiceURL
		// jobToken is the job-scoped bearer token the run service returns in the
		// acquirejob response (the SystemVssConnection AccessToken). RenewJob must
		// present it: the run service rejects the broker session token for per-job
		// lock renewal with 401 "Not authorized for this job" (Q247).
		jobToken string
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
			recordEvent(cfg, corev1.EventTypeWarning, "JobAcquisitionFailed", "AcquireJob",
				fmt.Sprintf("failed to acquire a delivered job from GitHub: %v; the job stays queued at GitHub for redelivery to a sibling session", acqErr))
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
		jobToken = resp.JobAuthToken()
		if jobToken == "" {
			// The run service authorizes per-job renewal with this token; without it
			// RenewJob falls back to the broker session token, which the run service
			// rejects with 401 "Not authorized for this job", so the lock lapses at
			// its ~10-minute TTL (Q247). Warn so a protocol drift that drops the token
			// is visible rather than silently re-orphaning long jobs.
			log.Warn("AcquireJob response carried no SystemVssConnection token; " +
				"RenewJob will fall back to the broker token and the run service may reject the renewal (Q247)")
		}
	} else {
		payload = []byte(msg.Body)
	}

	// Dedup gate (Q260): claim this job by its planID — the job identity that
	// collapses across GitHub's broker fan-out and names the shared worker Secret
	// — AFTER AcquireJob (planID is only known post-acquire) but BEFORE
	// provisioning. Under a concurrent burst the broker fans one job out to several
	// sibling sessions as messages with DISTINCT RunnerRequestIDs; each sibling
	// acquires its own delivery and the response carries the SAME planID, so
	// without this every sibling would race to create the per-job worker Secret
	// "job-<planID>" — one wins, the rest collide ("already exists"), fail
	// provisioning, and die with their runner slot burned (busy but pod-less),
	// collapsing the pool to a single worker (the Q260 wedge). A losing sibling
	// skips provisioning and returns acquired=true so its consumed single-use
	// runner is recycled back online (slot reclaimed cleanly) rather than left
	// busy/offline. The claim is held for the whole job and released on return, so
	// a later GitHub redelivery is provisionable again. Keying on planID — not the
	// pre-acquire RunnerRequestID, which differs per sibling and so never deduped
	// the fan-out (the ineffective first fix, c850764) — is what converges the
	// siblings onto one provision.
	//
	// jobResult is the winner's pod-phase-proxy result, reported when it fans
	// completion out to the deduped sibling deliveries on completion (Q260 Option A).
	// It defaults to succeeded and is overwritten by the JobHandler's terminal
	// result below; it is unused on the loser path.
	jobResult := broker.TaskResultSucceeded
	if cfg.ClaimJob != nil && acquired && planID != "" {
		delivery := SiblingDelivery{
			RunnerRequestID: jobBody.RunnerRequestID,
			RunServiceURL:   runServiceURL,
			JobToken:        jobToken,
		}
		claim := cfg.ClaimJob(planID, delivery)
		if !claim.Won {
			if cfg.Metrics != nil {
				cfg.Metrics.JobsDuplicateDeliveryTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
			}
			// High-volume under a burst of duplicate deliveries; Debug, with the
			// metric as the operator-facing signal (Q87, Theme D). acquired stays
			// true so the caller recycles the consumed runner (slot reclaimed);
			// SpawnReplacement/renew/provision are all skipped for the loser.
			log.Debug("duplicate job delivery: planID already claimed by a sibling session; skipping provisioning and recycling the runner slot",
				"planID", planID, "runnerRequestID", jobBody.RunnerRequestID)
			// Q260 Option A (guarded): the loser already ran AcquireJob, so GitHub
			// holds a per-delivery assignment for it. If the winner is still running,
			// this delivery was registered on the claim and the winner completes it on
			// finish. If the job ALREADY concluded (a late redelivery within the
			// linger window, when the winner is gone), resolve this delivery here with
			// the winner's recorded result — keyed on this delivery's OWN jobID
			// (distinct from the winner's), so under the per-delivery lock model
			// (Q247) it resolves only this assignment. Off by default until the run
			// service's per-delivery completion semantics are live-confirmed (see
			// Config.FanoutCompletion).
			if cfg.FanoutCompletion && claim.LateResult != "" && runServiceURL != "" {
				completeSiblingDelivery(ctx, cfg, log, planID, delivery, claim.LateResult)
				return acquired, nil
			}
			// Q266: the winner is still running. GitHub considers THIS deduped runner
			// assigned to the job (the loser's own AcquireJob claimed a per-delivery
			// assignment), so its recycle 422s — "runner is currently running a job and
			// cannot be deleted" — for the winner's ENTIRE runtime. That far outlasts
			// the bounded Q259 recycle backoff, so recycling now would give up and exit
			// the listener; under sustained fan-out burst enough losers strand+exit to
			// collapse the pool. Instead HOLD this slot until the winner concludes —
			// when it fans completjob out to this delivery (Option A), releasing the
			// assignment so the 422 finally clears — then let the caller recycle
			// normally. Only fan-out completion clears the loser's 422, so the defer
			// applies only when it is enabled; with it off, fall through to the eager
			// recycle of the documented (worse) opt-out path.
			if cfg.FanoutCompletion && claim.WinnerConcluded != nil {
				// The worker slot reserved above is for a pod this loser will never
				// provision. Free it before parking so a deduped loser never pins
				// worker capacity while it waits — that would starve the winner's own
				// pod under a tight maxWorkers ceiling (Q248). Idempotent with the
				// deferred release.
				if admitRelease != nil {
					admitRelease()
				}
				outcome := waitForWinnerConclusion(ctx, cfg, log, planID, claim.WinnerConcluded)
				if cfg.Metrics != nil {
					cfg.Metrics.FanoutLoserRecycleDeferredTotal.WithLabelValues(cfg.Namespace, cfg.Group, outcome).Inc()
				}
			}
			return acquired, nil
		}
		// Winner: when the job finishes, conclude the claim (always — this replaces
		// the pre-Option-A release, so the claim still lingers past completion for
		// the #512 redelivery dedup) and, when enabled, fan completjob out to every
		// deduped sibling delivery so none dangles at GitHub's unstarted-job timeout.
		defer func() {
			siblings := claim.Complete(jobResult)
			if cfg.FanoutCompletion && runServiceURL != "" && len(siblings) > 0 {
				<-completeSiblingDeliveries(ctx, cfg, log, planID, siblings, jobResult)
			}
		}()
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
	// RenewJob's jobId is the job's RunnerRequestID — the same value AcquireJob
	// sends as jobMessageId — NOT the broker envelope's numeric MessageID. Sending
	// the MessageID renews a job the run service does not recognize, so the lock is
	// never actually renewed: on any job that outlives GitHub's lock TTL the job is
	// recycled and redelivered to a sibling session (a duplicate worker pod), while
	// this worker runs to completion and then orphans at CompleteJobAsync with
	// TaskOrchestrationJobNotFoundException (Q247). Short jobs finish before the TTL
	// lapses, which is why only long jobs (e.g. e2e) exposed it.
	jobID := jobBody.RunnerRequestID
	// Bound each RenewJob call with the same per-call deadline as AcquireJob, so a
	// black-holed renewal (egress path saturated under load) aborts instead of
	// wedging the loop and starving every later renewal until the lock lapses (the
	// Q247 residual — an exactly-~10-minute orphan even with the correct jobId).
	// jobToken authorizes the renewal: the run service rejects the broker session
	// token for per-job renewal (401 "Not authorized for this job") even though it
	// accepted the same token to claim the job — the third and final Q247 facet.
	// Derive a per-job context the renew loop can cancel. When the loop detects the
	// job's lock is definitively lost (a definitive job-gone response or a sustained
	// run of renewal failures), it calls cancelJob so the JobHandler's context is
	// cancelled and the worker tears down — rather than running on to completion as
	// an orphan pod while GitHub recycles the job and redelivers it to a sibling
	// session (a duplicate acquire). On the normal path cancelJob fires via defer
	// once the job completes (a no-op teardown of an already-finished worker) (Q254).
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	stop, renewDone := StartRenewLoop(jobCtx, cancelJob, cfg.Broker, runServiceURL, planID, jobID, jobToken,
		cfg.Metrics, cfg.Namespace, cfg.Clock, log, renewInterval, cfg.controlPlaneTimeout())
	// Cancel the renew loop and wait for it to exit before returning, so the
	// goroutine never outlives the job it renews.
	defer func() { stop(); <-renewDone }()

	if cfg.Metrics != nil {
		cfg.Metrics.JobsAcquiredTotal.WithLabelValues(cfg.Namespace, cfg.Group).Inc()
	}

	if cfg.JobHandler != nil {
		result, jobErr := cfg.JobHandler(jobCtx, runServiceURL, planID, payload, cfg.Agent.EncodedJITConfig)
		// Record the pod-phase proxy for the winner's deferred sibling fan-out; keep
		// the succeeded default on an empty result (the pod never reached a terminal
		// phase — e.g. a provisioning error), matching "PodFailed→failed, else
		// succeeded" (Q260 Option A).
		if result != "" {
			jobResult = result
		}
		return acquired, jobErr
	}
	return acquired, nil
}

// completeSiblingDeliveries fans completjob out to every deduped sibling delivery
// of a fanned-out job concurrently, on a background goroutine, and returns a done
// channel closed once all completions have been attempted (Q260 Option A). It is
// async per CLAUDE.md's channel convention: the winner may block on the channel (as
// handleJob does, so its recycle happens after the assignments are resolved) or
// ignore it. Each call is bounded and best-effort — see completeSiblingDelivery.
func completeSiblingDeliveries(ctx context.Context, cfg Config, log *slog.Logger, planID string, siblings []SiblingDelivery, result broker.TaskResult) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, sib := range siblings {
			wg.Add(1)
			go func(sib SiblingDelivery) {
				defer wg.Done()
				completeSiblingDelivery(ctx, cfg, log, planID, sib, result)
			}(sib)
		}
		wg.Wait()
	}()
	return done
}

// completeSiblingDelivery resolves one deduped sibling delivery's job assignment via
// completejob so GitHub does not leave it dangling until the ~15-minute
// unstarted-job timeout and cancel the whole job even after the winner completed it
// (Q260 Option A). sib.RunnerRequestID is the sibling delivery's OWN jobID, distinct
// from the winner's; under the per-delivery lock model (Q247) completing it resolves
// only that assignment. result is the winner's pod-phase proxy. Best-effort: the
// call is bounded by the control-plane timeout and failures are logged and counted,
// never fatal — the runner still recycles its slot. Reached only when
// Config.FanoutCompletion is enabled; see that field for why this outward call is
// off by default.
func completeSiblingDelivery(ctx context.Context, cfg Config, log *slog.Logger, planID string, sib SiblingDelivery, result broker.TaskResult) {
	cctx, cancel := context.WithTimeout(ctx, cfg.controlPlaneTimeout())
	defer cancel()
	err := cfg.Broker.CompleteJob(cctx, sib.RunServiceURL, broker.CompleteJobRequest{
		PlanID:    planID,
		JobID:     sib.RunnerRequestID,
		Result:    result,
		AuthToken: sib.JobToken,
	})

	outcome := "completed"
	var notFound *broker.JobNotFoundError
	switch {
	case err == nil:
		log.Debug("completed a deduped sibling delivery via completejob so GitHub does not cancel the job at its unstarted-job timeout",
			"planID", planID, "jobID", sib.RunnerRequestID, "result", result)
	case errors.As(err, &notFound):
		// The assignment is already gone server-side — nothing left to resolve.
		log.Debug("deduped sibling delivery already resolved server-side",
			"planID", planID, "jobID", sib.RunnerRequestID)
	default:
		outcome = "error"
		log.Warn("failed to complete a deduped sibling delivery; GitHub may cancel the job at its unstarted-job timeout",
			"planID", planID, "jobID", sib.RunnerRequestID, "error", err)
	}
	if cfg.Metrics != nil {
		cfg.Metrics.AbandonedDeliveryCompletionsTotal.WithLabelValues(cfg.Namespace, cfg.Group, outcome).Inc()
	}
}

// defaultLoserRecycleDeferTimeout bounds a deduped fan-out loser's wait for its
// winner to conclude before it recycles anyway (Q266). It sits just past GitHub's
// ~15-minute unstarted-job timeout: if the winner never concludes (crash/hang),
// GitHub cancels the whole job at that timeout and releases the loser's assignment,
// so the loser's recycle 422 has cleared by the time this fallback fires — the wait
// is a safety valve, not the normal path (a winner concludes in seconds to minutes
// and always signals via its deferred Complete).
const defaultLoserRecycleDeferTimeout = 16 * time.Minute

// waitForWinnerConclusion blocks a deduped fan-out loser until its winner concludes
// — the point at which the winner fans completjob out to this loser's delivery
// (Option A), releasing GitHub's assignment on the loser's deduped runner so its
// recycle 422 clears (Q266). It returns the outcome for the metric: "winner_concluded"
// on the signal, "fallback_timeout" if the winner did not conclude within the bound
// (a winner crash/hang; GitHub's unstarted-job timeout has released the assignment by
// then), or "context_cancelled" on shutdown. The loser holds its listener slot and
// pool agent throughout — it is not counted as a poller (SetPolling(false) was set
// before the job) so it is never mistaken for available capacity.
func waitForWinnerConclusion(ctx context.Context, cfg Config, log *slog.Logger, planID string, winnerConcluded <-chan struct{}) string {
	timeout := cfg.LoserRecycleDeferTimeout
	if timeout <= 0 {
		timeout = defaultLoserRecycleDeferTimeout
	}
	select {
	case <-winnerConcluded:
		return "winner_concluded"
	case <-cfg.Clock.After(timeout):
		log.Warn("deferred loser recycle: winner did not conclude within the fallback bound; recycling anyway "+
			"(GitHub's unstarted-job timeout should have released this deduped runner's assignment by now)",
			"planID", planID, "timeout", timeout)
		return "fallback_timeout"
	case <-ctx.Done():
		return "context_cancelled"
	}
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

// StartRenewLoop starts a per-job renewal goroutine that ticks on the given interval.
// It returns a stop function that cancels the loop and a done channel that closes
// once the loop goroutine has fully exited. Callers must call stop when the job
// completes to avoid goroutine leaks; they may then wait on done if they need to
// guarantee the goroutine has stopped before releasing shared resources.
//
// jobToken is the job-scoped bearer token from the acquirejob response
// (AcquireJobResponse.JobAuthToken). Each RenewJob call presents it instead of the
// broker session token: the run service rejects the session token for per-job lock
// renewal with 401 "Not authorized for this job" even though it accepted the same
// token to claim the job, so without jobToken every renewal fails and the lock
// lapses at its ~10-minute TTL (Q247). An empty jobToken falls back to the client's
// session token (test/stub use, or a run service that authorizes renewal with it).
//
// renewCallTimeout bounds each individual RenewJob call. It MUST be smaller than
// renewInterval and smaller than GitHub's lock TTL. The renewal call runs inline
// in the loop, so an unbounded call that black-holes (egress-proxy path saturated
// under heavy worker load — the Q247 residual) would wedge the goroutine and
// starve every subsequent tick until it returned, letting the job's lock lapse at
// the initial ~10-minute TTL even when the jobId is correct. A bounded call aborts
// (counted as a non-fatal RenewJob error), and the loop proceeds to the next tick,
// so a single slow renewal costs one renewal, not all of them. A zero value leaves
// the call unbounded (test/stub use only).
//
// cancelJob tears the worker down when the job's lock is definitively lost: a
// definitive job-gone response (broker.JobNotFoundError, 404/410) or a sustained
// run of renewFailureThreshold consecutive renewal failures. Without it the loop
// would keep logging every failure as non-fatal and the worker would run on to
// completion as an orphan pod while GitHub recycles the job and redelivers it to a
// sibling session (a duplicate acquire) — the Q247 residual. A single/transient
// failure stays non-fatal and is retried (GitHub grants ~10 min per renewal
// window). cancelJob may be nil (test/stub use); teardown is then a no-op.
func StartRenewLoop(
	ctx context.Context,
	cancelJob context.CancelFunc,
	client *broker.Client,
	runServiceURL, planID, jobID, jobToken string,
	metrics *Metrics,
	namespace string,
	clk Clock,
	log *slog.Logger,
	renewInterval, renewCallTimeout time.Duration,
) (stop func(), done <-chan struct{}) {
	stopCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		var consecutiveFailures int
		for {
			select {
			case <-stopCtx.Done():
				return
			case <-clk.After(renewInterval):
				if runServiceURL == "" {
					continue // M2 stub: no real run service URL
				}
				callCtx, cancelCall := stopCtx, context.CancelFunc(func() {})
				if renewCallTimeout > 0 {
					callCtx, cancelCall = context.WithTimeout(stopCtx, renewCallTimeout)
				}
				_, err := client.RenewJob(callCtx, runServiceURL, broker.RenewJobRequest{
					PlanID:    planID,
					JobID:     jobID,
					AuthToken: jobToken,
				})
				cancelCall()
				if err == nil {
					consecutiveFailures = 0
					continue
				}
				// A renewal aborted only because the loop itself is shutting down
				// (stop() cancelled stopCtx mid-call) is not a lock failure — don't
				// count it toward teardown; just exit.
				if stopCtx.Err() != nil {
					return
				}
				consecutiveFailures++
				if metrics != nil {
					metrics.RenewJobErrorsTotal.WithLabelValues(namespace).Inc()
				}
				if reason := renewTeardownReason(err, consecutiveFailures); reason != "" {
					if metrics != nil {
						metrics.RenewJobTeardownsTotal.WithLabelValues(namespace, reason).Inc()
					}
					if log != nil {
						log.Error("RenewJob: job lock definitively lost; cancelling worker to avoid an orphan pod and a sibling duplicate-acquire (Q254)",
							"reason", reason, "consecutiveFailures", consecutiveFailures, "error", err)
					}
					if cancelJob != nil {
						cancelJob()
					}
					return
				}
				if log != nil {
					log.Warn("RenewJob error (non-fatal)", "error", err, "consecutiveFailures", consecutiveFailures)
				}
			}
		}
	}()
	return cancel, doneCh
}

// renewFailureThreshold is the number of consecutive RenewJob failures that trips
// a worker teardown. With the default 60s renew interval and GitHub's ~10-minute
// lock TTL, 5 consecutive failures (~5 min of a sustained outage) is well past any
// single transient blip, yet still tears the worker down before the lock lapses at
// ~10 min — so the orphan pod is gone before GitHub can recycle the job and
// redeliver it to a sibling session (the duplicate-acquire window) (Q254).
const renewFailureThreshold = 5

// renewTeardownReason returns a non-empty metric reason when a RenewJob error
// means the job's lock is unrecoverably lost and the worker must be cancelled: a
// definitive job-gone response (broker.JobNotFoundError, 404/410), or a sustained
// run of consecutive failures reaching renewFailureThreshold. It returns "" for a
// transient failure that should stay non-fatal and be retried.
func renewTeardownReason(err error, consecutiveFailures int) string {
	var notFound *broker.JobNotFoundError
	if errors.As(err, &notFound) {
		return "job_not_found"
	}
	if consecutiveFailures >= renewFailureThreshold {
		return "consecutive_failures"
	}
	return ""
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

// recordEvent emits an owner-scoped Kubernetes Event via cfg.Events, mirroring
// setCondition. A no-op when no recorder is wired.
func recordEvent(cfg Config, eventtype, reason, action, note string) {
	if cfg.Events == nil {
		return
	}
	cfg.Events.Event(cfg.Namespace, cfg.Group, eventtype, reason, action, note)
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

// isRegistrationNotFound reports whether a broker OAuth token fetch failed with
// the transient "Registration … was not found" response GitHub's token endpoint
// returns for a runner record that exists but whose registration the OAuth
// service has not yet caught up to — the propagation window just after
// generate-jitconfig (Q267). It is deliberately narrower than isTokenRejected:
// only this specific body warrants riding out the lag by retrying the SAME fresh
// credentials, whereas a broad credential rejection means the record is genuinely
// gone and the agent must be recycled (Q114).
func isRegistrationNotFound(err error) bool {
	var typed *githubapp.TokenExchangeError
	if !errors.As(err, &typed) {
		return false
	}
	if typed.StatusCode != http.StatusBadRequest && typed.StatusCode != http.StatusNotFound {
		return false
	}
	body := strings.ToLower(typed.Body)
	return strings.Contains(body, "registration") && strings.Contains(body, "not found")
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
