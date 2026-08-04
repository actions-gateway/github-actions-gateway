// Package listener implements the per-RunnerGroup listener goroutine pool for the
// classic long-poll acquisition protocol. The version- and protocol-neutral
// contracts it reports through — the metric set, the capacity gate, the condition
// and event sinks — live in internal/runnercore.
package listener

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/broker"
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
// winner's own delivery. The AGC uses it as the result to report when it fans
// completion out to the deduped sibling deliveries of a fanned-out job (Q260 Option
// A); an empty result (an error before the pod reached a terminal phase) is treated
// as succeeded by that fan-out.
//
// broker.TaskResultAbandoned means the worker was removed before it ran, so no
// runner registered and nothing will ever report this delivery. The listener
// reports nothing for it either — completing the winner's own sole delivery
// concludes the whole run as success, a false green (measured, Q645/Q676; see
// handleJob) — and leaves the job to the acquire lock's lapse.
//
// The error return is the provisioning error as before (recoverable; the poll loop
// logs and continues).
type JobHandlerFunc func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) (broker.TaskResult, error)

// SiblingDelivery identifies one deduped sibling delivery of a fanned-out job so the
// winner can complete it on GitHub's books when its job finishes (Q260 Option A).
// Each field is the sibling's OWN per-delivery value: RunnerRequestID is the
// delivery's job id (distinct per sibling under fan-out), and RunServiceURL /
// JobToken are what completejob needs to resolve that specific assignment.
//
// It also addresses a session's own delivery when that delivery needs the same
// release — a worker removed before it ran (Q628).
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

// Config holds the dependencies injected into a listener goroutine.
type Config struct {
	Group     string // RunnerGroup name
	Namespace string
	Agent     *agentpool.Agent

	// Broker is a per-goroutine Client instance. The goroutine sets
	// Broker.Token before each API call via the agent's OAuth credentials.
	Broker     *broker.Client
	HTTPClient *http.Client // used for OAuth token fetch; nil uses a bounded httpx.NewClient()

	Conditions runnercore.ConditionUpdater
	// Events records owner-scoped Kubernetes Events for job-lifecycle incidents that
	// this goroutine detects (acquisition failure, non-retriable session failure).
	// Nil disables event recording (the metric/condition remains the signal).
	Events        runnercore.EventRecorder
	Metrics       *runnercore.Metrics
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
	Admit runnercore.AdmitFunc
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
	// the pod is reaped provisions again. Keying on planID rather than the
	// pre-acquire RunnerRequestID (which differs per sibling delivery) is what
	// collapses the siblings. Nil
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
	// terminal result. Losers do NOT complete early.
	//
	// It does NOT cover the winner's own delivery when its worker was removed
	// before it ran: completing that delivery concludes the whole run as success —
	// a false green (measured, Q645/Q676) — so the listener never completes its
	// own unrun assignment, regardless of this switch (see handleJob).
	//
	// ON BY DEFAULT: the run service's completion is per-delivery, not
	// planID-scoped — completejob on a sibling's OWN jobID resolves only that
	// assignment, while the winner's own delivery still carries the real workflow
	// result reported by its runner binary, so the sibling pod-phase proxy cannot
	// green a red workflow. Opt out with AGC env AGC_FANOUT_COMPLETION=false. The
	// operator runbook is the Q260 redelivery-accounting section in
	// docs/operations/troubleshooting.md.
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
