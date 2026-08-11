// Package scalesetlistener implements the AGC's runner-scale-set acquisition tier
// (Q264 Option E, P3). One Listener runs per ScaleSet-protocol RunnerSet: it holds a
// single message-queue session against the scale set and provisions exactly one
// worker pod per assigned job, replacing — for ScaleSet sets only — the classic
// multiplexer/maxListeners/planID-dedup/SpawnReplacement machinery. The classic tier
// is untouched; a RunnerSet stays on it unless spec.acquisitionProtocol is ScaleSet.
//
// # Why this removes the fan-out race
//
// The classic tier's many-acquirers topology fans one logical job out to N sibling
// sessions that all acquire it (shared planID), leaving N−1 dangling deliveries
// GitHub eventually cancels (Q260/Q224). The scale-set protocol enqueues each job
// once in the scale set's single serialized queue, claimed by this Listener's single
// session: 1 job : 1 queue entry : 1 acquirer : 1 runner. There are no sibling
// deliveries, so nothing to dedup or reconcile — the entire Q260/Q247-completion/
// Q259-recycle class cannot occur.
//
// # The loop
//
// After ensuring the scale set and opening a session, the Listener long-polls the
// queue advertising its worker capacity as the X-ScaleSetMaxCapacity header (the
// admission ladder as one integer). GitHub holds totalAssignedJobs at or below that
// value, assigning each job exactly once (dotcom auto-assign; on GHES the JobAvailable→AcquireJobs path claims
// them — one rule, §5a-U8). Each JobAssigned mints a per-job JIT config and provisions
// one worker; the worker pulls its job through its own session and reports its own
// completion (the runner renews/completes its job, not the AGC — §2.4). The
// server-authoritative statistics.totalAssignedJobs is the ARC clamp target the
// Listener reconciles against and reports as status.
//
// # Recovery
//
// There is no in-memory acquisition registry: queue messages the Listener has not
// deleted replay to a re-created session (poll from cursor 0), so a session drop or an
// AGC restart re-reads assigned-but-unprovisioned jobs from the queue (§2b-3).
// Provisioning is idempotent per jobID (a deterministic worker name), so a replay never
// double-runs a job.
//
// What bounds that replay is the delete half of the ack: a message is deleted once
// every job it names has concluded, so a restart re-reads only work still owed a worker
// rather than the scale set's whole history (Q583). The conclusions themselves are
// persisted through Config.Guards before any delete is issued, so a hard kill between
// a conclusion and its DELETE does not turn the replay back into a re-provision (Q606).
// A conclusion the loop had not yet read is the third case, and needs no kill at all:
// the exit path reads what the queue is still holding before it flushes (Q689).
//
// One job class is outside that replay: an assignment the Listener acked past because it
// could not be provisioned — a runner name a stale registration holds (Q551), or a worker
// ceiling already full (Q576). The cursor has moved beyond it, so no session will deliver
// it again — the Listener keeps it and re-offers it until it runs, reporting the stall as
// JobProvisionStalled meanwhile. Redelivery is reserved for genuinely transient failures,
// because the queue redelivers immediately: a condition that will still hold a moment
// later would spin the loop, and each pass would mint (and then have to deregister)
// another runner registration.
//
// Holding a job is only safe if something ends the hold. A terminal JobCompleted does,
// but the backend does not always send one for an assignment it has stopped honouring,
// and a re-offer of a job that no longer exists provisions a worker with nothing to run —
// which is what livelocks a drain (Q553). So the Listener also reconciles what it holds
// against the scale set's server-authoritative statistics: see reconcileDeferred.
//
// # Security
//
// The Listener's Actions Service traffic routes through the per-tenant egress proxy
// exactly like the classic tier: its scaleset.Client clones the proxy-patched
// http.DefaultTransport. The App/admin/queue tokens never leave the AGC; only the
// per-job JIT config reaches a worker pod (staged into its Secret by the provisioner).
package scalesetlistener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label type for every one of the scale set's runs-on match targets. ARC sends System
// for the name label and each extra label alike (measured against 0.14.0, Q726).
const systemLabelType = "System"

// defaultRunnerGroupID is GitHub's default runner group, where a scale set lands when
// no group is named.
const defaultRunnerGroupID = 1

// defaultPollBackoff is how long the loop waits before retrying after a transient
// poll error (a non-401/404 error, or a rate-limit with no Retry-After). A healthy
// long-poll blocks ~50s server-side, so this only paces the error path.
const defaultPollBackoff = 2 * time.Second

// minPollInterval is the floor on the time between two polls that deliver nothing (a
// 202). It exists because the loop otherwise re-polls a 202 with no pause at all: a
// server that answers "nothing to deliver" promptly — a GHES tenant with a short poll
// window, an intermediary that terminates the long poll, a backend that declines to
// hold a zero-capacity poll — would spin the Listener into a request storm against
// GitHub, and the rate limiter would answer for us. Every error path already backs off;
// this is the same pacing for the one path that lacked it.
//
// It is a floor on the *interval*, not a sleep per poll, so a server that really does
// long-poll (the real queue blocks ~50s) never waits on it: only a 202 that came back
// faster than the floor is padded out to it. The cost on such a server is exactly zero,
// and the delay it can add to a job assignment is bounded by minPollInterval.
const minPollInterval = 100 * time.Millisecond

// defaultWorkFolder is the runner work directory baked into a JIT config when Config
// leaves it empty (the actions-runner convention).
const defaultWorkFolder = "_work"

// sweepTimeout bounds the start-up registration-record sweep (Q550). The sweep runs on
// the reconcile path, and its listing pages over every runner registered for the owner,
// so a large org or a slow API must not hold a reconcile open: past this the sweep gives
// up and the next listener start retries it. Nothing depends on it completing — it is
// the backstop for records the reap path could not deregister.
const sweepTimeout = 30 * time.Second

// teardownBudget bounds each teardown call the loop makes on its way out — the delete
// half of the ack for anything already concluded, then the session delete. Both run on
// a detached context inside the AGC pod's 60s grace period, so each gets a slice large
// enough for a slow answer and small enough that a queue which never answers cannot
// spend the budget the other one needs.
const teardownBudget = 10 * time.Second

// drainBudget bounds the exit read of conclusions the loop had not got to yet (Q689).
// Far smaller than teardownBudget, and not for symmetry: the session cannot be deleted
// until this returns, a scale set allows one session at a time, and a successor's
// CreateSession answers SessionConflictError until the predecessor's is gone. Every
// millisecond spent here is a millisecond the next AGC cannot start acquiring.
//
// A second buys the read without paying for a wait: the messages it exists to collect
// are already queued and come back immediately, while the poll that finds nothing left
// is held server-side for the backend's whole long-poll window — pure cost, since a
// queue with nothing to say has nothing this can use.
const drainBudget = time.Second

// defaultRateLimitConditionAfter is how long GetMessage must have been answered 429
// before the Listener surfaces RateLimited=True on the owning RunnerSet — the same
// ten-minute window the classic listener uses, so a transient burst does not flap
// the condition (Q325).
const defaultRateLimitConditionAfter = 10 * time.Minute

// maxJITNameConflictRetries bounds how many times provisioning re-mints a JIT config
// under a fresh runner name after a RunnerNameConflictError. Past it, the assignment is
// acked past (not replayed forever), so a persistently colliding name — a stale registered
// runner — cannot wedge the queue cursor behind it (Q270). The job is not dropped: it is
// deferred and re-offered on a backoff, because the queue does not re-assign it (Q551).
const maxJITNameConflictRetries = 3

// defaultDeferredRetryBackoff is the wait before the first re-offer of a deferred job,
// doubling per attempt up to maxDeferredRetryBackoff. The stall it paces is cleared by
// something outside this process — an operator, the offline-record sweep, or the live
// runner holding the name finishing — so the re-offer is a slow poll, not a retry loop.
const defaultDeferredRetryBackoff = 30 * time.Second

// maxDeferredRetryBackoff caps the exponential re-offer wait. A job stays assigned to
// the scale set until it runs or GitHub times it out, so the cap only bounds how long
// after a conflict clears the job waits to run.
const maxDeferredRetryBackoff = 5 * time.Minute

// defaultCapacityRetryInterval is the wait between re-offers of a job held because its
// owner is at its worker ceiling (Q576). Flat rather than exponential, and far shorter
// than the conflict ladder, because the two stalls clear on different clocks: a runner
// name is freed by an operator or the offline-record sweep, while a full ceiling frees
// itself the moment any worker finishes — so backing off to minutes would leave a slot
// idle long after it opened. A re-offer costs no GitHub call (the capacity check runs
// before the JIT config is minted), and the poll loop re-offers at most once per
// iteration, so a short interval is cheap.
const defaultCapacityRetryInterval = 5 * time.Second

// defaultAssignmentCheckInterval paces the check that the assignments the Listener is
// holding still exist at GitHub (Q553). It is deliberately far slower than either
// re-offer schedule: a job whose stall is about to clear costs nothing by being checked
// late, while the check itself is a session refresh rather than a free local read.
//
// The Listener only makes the call while it is holding something, so a healthy set
// never pays for it at all.
const defaultAssignmentCheckInterval = 60 * time.Second

// Job is one assigned job the Listener hands to its ProvisionFunc. The listener has
// already minted the JIT config; the provisioner stages it into the worker Secret and
// creates a run.sh --jitconfig worker pod.
type Job struct {
	// JobID is the job's server-assigned UUID — stable across replay, so it keys the
	// worker's deterministic name and the Listener's provisioned-once set.
	JobID string
	// RunnerName is the name the JIT config pre-registered the runner under.
	RunnerName string
	// JITConfig is the base64 run.sh --jitconfig blob for this one job.
	JITConfig string

	// Owner, Repository, and RunID identify the workflow run this job belongs to,
	// from the assignment message's JobMessageBase fields (scaleset.JobMessage.RunIdentity).
	// The provisioner stamps them onto the worker pod, which is what lets eviction
	// recovery name a run to re-run after the pod dies — the scale-set tier has no
	// acquired payload to read identity from the way the classic tier does (Q417).
	//
	// All three are empty together when the assignment carried no complete identity.
	// A worker still provisions and still runs its job; only automatic eviction
	// recovery degrades, observably (see provisioner.RecoverEvictedScaleSetWorkers).
	Owner      string
	Repository string
	RunID      string
	// JobName is the job's display name from the workflow YAML, stamped on the pod
	// for operator legibility. Best-effort like the identity triple.
	JobName string
}

// ErrCapacityUnavailable means the owner cannot run another worker pod right now
// because it is at the worker ceiling its spec declares. Both CapacityCheckFunc and
// ProvisionFunc signal it by returning an error that wraps it.
//
// It is the one provisioning failure the Listener must NOT treat as transient. A
// transient failure is retried by holding the queue cursor so the message redelivers,
// which on a long-poll queue is immediate — correct for an API blip, and a hot spin for
// a full ceiling, which is still full on the next delivery. A job rejected for capacity
// is deferred onto the re-offer backoff instead (Q576).
var ErrCapacityUnavailable = errors.New("scalesetlistener: no worker capacity for this job")

// ErrRunnerGroupNotFound means Config.RunnerGroupName names a runner group the GitHub
// installation does not have. Start fails with an error wrapping it so the owning
// reconciler can report RunnerGroupNotFound rather than a generic session failure.
//
// Registering into the default group instead would be the convenient answer and the
// wrong one: the runner group is GitHub's authorization point for which repositories
// may target these runners, so the fallback silently widens the boundary the operator
// asked for to the whole installation (Q712).
var ErrRunnerGroupNotFound = errors.New("scalesetlistener: no such runner group at GitHub")

// ProvisionFunc provisions one worker pod for an assigned job. It must be idempotent
// per Job.JobID: a re-created session replays unacked JobAssigned messages, so the
// same job may be handed over more than once, and a duplicate must be a no-op (the
// provisioner names the pod/Secret deterministically from the jobID). A non-nil error
// leaves the job un-provisioned so the Listener retries it on a later poll — except an
// error wrapping ErrCapacityUnavailable, which defers the job instead.
type ProvisionFunc func(ctx context.Context, job Job) error

// CapacityCheckFunc reports whether the owner can run another worker pod right now. The
// Listener calls it for each newly assigned job BEFORE minting that job's JIT config,
// because minting one registers a runner at GitHub: without the check, a job the
// provisioner is about to reject for capacity still costs a registration, which the next
// attempt's name conflict then has to deregister — 704 deregister calls for one job on
// the rc.3 dogfood gate (Q576).
//
// A nil return means provision. An error wrapping ErrCapacityUnavailable defers the job
// onto the re-offer backoff, having registered nothing. Any other error means the check
// itself could not be made, and is logged and ignored: this is an optimisation over the
// authoritative check ProvisionFunc still makes, so failing it open keeps an unreadable
// pod count from stalling assignments.
//
// Nil disables the pre-check, leaving the authoritative check as the only one.
type CapacityCheckFunc func(ctx context.Context) error

// CleanupFunc releases the per-job resources ProvisionFunc staged — the worker's
// JIT-config Secret — once the job is terminally complete (Q373). It is called with
// the jobID of every terminal JobCompleted the queue delivers, and must be idempotent:
// a re-created session replays completions from cursor 0, so the same job may be
// cleaned up more than once, and a job may complete having never been provisioned by
// this process. The worker pod itself is NOT this hook's concern — the owning
// reconciler's reaper collects terminal pods on spec.completedPodTTL.
type CleanupFunc func(ctx context.Context, jobID string) error

// GuardState is the durable half of the replay guards: the jobs this listener has
// concluded — completed or abandoned — whose queue messages may not all be deleted yet.
// provisioned is deliberately absent: replaying a still-running job is the recovery
// path (provisioning is idempotent per jobID), so losing that set costs nothing.
type GuardState struct {
	Completed []string `json:"completed,omitempty"`
	Abandoned []string `json:"abandoned,omitempty"`
}

// GuardStore persists GuardState across a process boundary (Q606). The in-memory
// guards close every stop the exit flush can reach, but a hard kill — SIGKILL at grace
// expiry, OOM, node loss — between a conclusion and its message's DELETE loses the
// conclusion, and the next process replays the assignment and provisions a worker for a
// job that is over. The store is written ahead of the deletes (flushDeletes), so once a
// DELETE is even attempted the conclusion that authorised it survives a kill.
//
// Load is called once, before the poll loop starts; a Load error fails Start, because
// polling without the guards silently reopens the window. Save replaces the whole
// state — it is bounded by the messages still in the queue (Q597), not by history.
// Both are called from the poll goroutine only. Nil disables persistence.
type GuardStore interface {
	Load(ctx context.Context) (GuardState, error)
	Save(ctx context.Context, state GuardState) error
}

// CapacityFunc returns the value to advertise as X-ScaleSetMaxCapacity on the next
// poll. It is a TOTAL, not a free-slot delta: GitHub holds totalAssignedJobs at or
// below it, so jobs already assigned to this set count against it and a return equal
// to the current assigned count means "no more".
//
// It is the scale-set expression of the whole admission ladder — the declared worker
// ceiling (maxWorkers/priorityTiers) bounded by live namespace-ResourceQuota headroom
// (Q443) — evaluated once per poll rather than once per delivered job. Every rung the
// classic tier's Provisioner.Admit walks must be represented here too; see
// provisioner.AdvertiseCapacity, which is what the reconciler wires in.
//
// A non-positive return advertises zero capacity: the Listener still polls (to drain
// JobStarted/JobCompleted and keep the session alive) but GitHub assigns nothing.
type CapacityFunc func(ctx context.Context) int

// ConditionSetter publishes one status condition onto the owning RunnerSet. Like
// MetricsRecorder, the implementation is owner-bound — the reconciler closes over
// the RunnerSet identity when wiring it — and must be non-blocking (the reconciler
// adapts its buffered condition channel, dropping on backpressure), so the poll
// loop never stalls on status delivery (Q325).
type ConditionSetter interface {
	SetCondition(cond metav1.Condition)
}

// EventSink records one owner-scoped Kubernetes Event for a session failure the
// Listener detects, complementing the condition that tracks the same state. Like
// ConditionSetter it is owner-bound and must be non-blocking.
type EventSink interface {
	Event(eventtype, reason, action, note string)
}

// MetricsRecorder records Listener-level statistics. Nil is safe. It is separate from
// scaleset.MetricsRecorder (poll-error/token-refresh on the client); this records the
// acquisition tier's job accounting.
type MetricsRecorder interface {
	// IncJobAssigned counts each JobAssigned the queue delivers.
	IncJobAssigned()
	// IncJobProvisioned counts each worker successfully provisioned.
	IncJobProvisioned()
	// IncProvisionError counts a failed provision (the job is retried). A job held for
	// capacity is not an error and is not counted here — it is backpressure, and
	// SetDeferredJobs carries it (Q576).
	IncProvisionError()
	// IncJobCompleted counts a terminal JobCompleted, labelled by result — the
	// completion signal the classic protocol never delivered (§2b-6).
	IncJobCompleted(result string)
	// SetDeferredJobs publishes how many assigned jobs the Listener is holding for a
	// later re-offer because they cannot be provisioned (Q551) — the alertable mirror
	// of the JobProvisionStalled condition — keyed by DeferReason*. Every reason is
	// present on every call, carrying an explicit zero when it holds nothing, so a
	// reader never has to tell "not deferring for this reason" from a series that
	// stopped being written.
	SetDeferredJobs(byReason map[string]int)
	// IncJobsAbandoned counts n assignments the Listener gave up on because the scale
	// set no longer counts them as assigned (Q553). Each is a workflow run that will
	// never run, so unlike the deferred gauge this is a loss, not backpressure.
	IncJobsAbandoned(n int)
}

// DeferReason* name why the Listener is holding an assigned job, and are the values of
// the deferred-jobs gauge's reason label. They separate expected backpressure from an
// anomaly, which is what makes the gauge alertable: a set at its ceiling is working as
// configured, while a job that cannot register a name needs an operator (Q576).
const (
	// DeferReasonCeiling: the owner is at the worker ceiling its spec declares
	// (maxWorkers, or the last priorityTiers threshold). Clears as workers finish.
	DeferReasonCeiling = "ceiling"
	// DeferReasonNameConflict: the runner name the job needs is held by a registration
	// that neither a deregister nor a bounded run of fresh names could clear (Q551).
	DeferReasonNameConflict = "name_conflict"
)

// PollErrorRecorder counts GetMessage failures into the cross-tier
// actions_gateway_message_poll_errors_total counter (Q446). It is separate from
// MetricsRecorder because the counter is not this tier's: it is the shared
// (namespace, reason) series the classic listener also writes, so parity means an
// operator's existing dashboards and alerts survive the classic removal (Q264). The
// implementation binds the namespace — runnercore.Metrics.PollErrors — leaving the
// Listener to supply only the reason. Nil is safe.
type PollErrorRecorder interface {
	IncPollError(reason string)
}

// Config configures a Listener. Client, ScaleSetName, Provision, and Capacity are
// required.
type Config struct {
	// Client is the GAG-owned scale-set protocol client (one per RunnerSet). It owns
	// the two-hop auth bootstrap, the admin-JWT refresh, and the egress-proxy-aware
	// HTTP transports.
	Client *scaleset.Client
	// ScaleSetName is the scale set's name and its first runs-on label — the owning
	// RunnerSet's runnerLabels[0]. It is the set's identity at GitHub: the scale set
	// is looked up by it, and every runner record is named from it.
	ScaleSetName string
	// ExtraLabels are the owning RunnerSet's runnerLabels after the first. They are
	// registered on the scale set alongside the name label so a workflow can target
	// the set with an array (runs-on: [linux, gpu]); GitHub matches them the way it
	// matches a plain self-hosted runner's labels (Q726). Empty is the single-label
	// shape, which produces the create request this tier has always sent.
	ExtraLabels []string
	// RunnerGroupName is the GitHub runner group to place the scale set in, from the
	// owner's spec.runnerGroup or its gateway's defaultRunnerGroup. Empty leaves a new
	// scale set in GitHub's default group and an existing one where it already is; a
	// name that resolves moves an adopted scale set into it, and one that does not
	// fails Start with ErrRunnerGroupNotFound rather than falling back (Q712).
	RunnerGroupName string
	// OwnerName identifies this listener on the session (e.g. "<gateway>/<runnerset>").
	OwnerName string
	// WorkFolder is the runner work directory for minted JIT configs. Empty selects
	// defaultWorkFolder.
	WorkFolder string
	// Provision provisions one worker per assigned job. Required.
	Provision ProvisionFunc
	// CheckCapacity reports whether another worker fits under the owner's ceiling,
	// asked before a job's JIT config is minted so a ceiling-blocked job registers no
	// runner at GitHub (Q576). Nil disables the pre-check.
	CheckCapacity CapacityCheckFunc
	// Capacity returns the total worker capacity to advertise each poll. Required.
	Capacity CapacityFunc
	// Cleanup releases a completed job's staged worker resources (its JIT-config
	// Secret). Nil disables reclaim, which leaks one Secret per job until the owning
	// RunnerSet is deleted — so the reconciler always wires it (Q373).
	Cleanup CleanupFunc
	// Guards persists the concluded-job guards across a process boundary, closing the
	// hard-kill half of the settle→DELETE gap (Q606). Nil disables persistence, leaving
	// the guards process-scoped (the pre-Q606 behaviour).
	Guards GuardStore
	// Metrics records job accounting. Nil is safe.
	Metrics MetricsRecorder
	// PollErrors counts GetMessage failures into the shared cross-tier poll-error
	// counter, by reason. Nil is safe.
	PollErrors PollErrorRecorder
	// Conditions publishes the Listener's session-failure conditions onto the owning
	// RunnerSet (Q325): Degraded=True/Unauthorized when a session call is rejected as
	// unauthorized, and RateLimited=True/SustainedRateLimit when message polling has
	// been answered 429 past RateLimitConditionAfter — the classic listener's failure
	// vocabulary. Unlike the classic path, the Listener also publishes the healthy
	// (False) states on start and clears an abnormal state when the session recovers.
	// Nil disables condition reporting.
	Conditions ConditionSetter
	// Events records an owner-scoped Warning event (SessionUnauthorized when the
	// Degraded condition trips, JobProvisionStalled when a job cannot be provisioned),
	// so the incident surfaces in `kubectl describe`. Emitted once per episode, on the
	// transition into the state. Nil disables.
	Events EventSink
	// RateLimitConditionAfter is how long message polling must have been answered 429
	// before RateLimited=True is surfaced. Non-positive selects
	// defaultRateLimitConditionAfter (ten minutes, classic parity). Overridable in
	// tests to drive the condition deterministically.
	RateLimitConditionAfter time.Duration
	// ClaimedRunnerNames returns the runner names currently claimed by live worker
	// pods, and is what makes the start-up registration sweep safe (Q550): a record is
	// only deletable if no worker pod is relying on it. A worker that has not started
	// yet has a legitimately OFFLINE record, so "offline" cannot be the sweep's only
	// test, and the REST runner object carries no timestamp to age records by.
	//
	// An error aborts the sweep — an unreadable claim set is not evidence that nothing
	// is claimed. Nil disables the sweep entirely.
	ClaimedRunnerNames func(ctx context.Context) (map[string]struct{}, error)
	// Log is the structured logger. Nil selects slog.Default().
	Log *slog.Logger
	// PollBackoff paces the transient-error retry path. Non-positive selects
	// defaultPollBackoff.
	PollBackoff time.Duration
	// DeferredRetryBackoff is the wait before the first re-offer of a job whose runner
	// name will not register; each further attempt doubles it, capped at
	// maxDeferredRetryBackoff. Non-positive selects defaultDeferredRetryBackoff.
	// Overridable in tests to drive the re-offer path deterministically.
	DeferredRetryBackoff time.Duration
	// CapacityRetryInterval is the flat wait between re-offers of a job held because
	// the owner is at its worker ceiling — a separate knob because that stall clears on
	// a different clock than a name conflict (see defaultCapacityRetryInterval, which a
	// non-positive value selects).
	CapacityRetryInterval time.Duration
	// AssignmentCheckInterval is the wait between the server-authoritative checks that
	// the jobs being re-offered still exist at GitHub (Q553). Non-positive selects
	// defaultAssignmentCheckInterval. Overridable in tests to drive the check
	// deterministically.
	AssignmentCheckInterval time.Duration
}

// Listener owns one scale set's acquisition session and provisions workers for its
// assigned jobs. Construct it with New; drive it with Start.
type Listener struct {
	cfg                Config
	log                *slog.Logger
	workFolder         string
	pollBackoff        time.Duration
	rateLimitAfter     time.Duration
	deferredBackoff    time.Duration
	capacityInterval   time.Duration
	assignmentInterval time.Duration

	// Session-failure condition state (Q325), owned by Start and then the run
	// goroutine — the goroutine-creation happens-before makes the handoff safe
	// without mu. Each abnormal condition is pushed once per episode (on the
	// transition into the state) and cleared on recovery.
	rateLimitedSince time.Time // first 429 of the current episode; zero while polling is healthy
	rateLimitedCond  bool      // RateLimited=True has been pushed for the current episode
	unauthorizedCond bool      // Degraded=True/Unauthorized has been pushed
	stalledCond      bool      // JobProvisionStalled=True has been pushed
	// stalledEventReason is the Event reason recorded for the current stall episode, so
	// an episode that changes class (a name conflict joining a ceiling hold) records the
	// new class's Event instead of being suppressed as "already surfaced".
	stalledEventReason string

	// deferred holds assignments that could not be provisioned, keyed by jobID, each
	// carrying the time of its next re-offer (Q551). Same ownership as the condition
	// flags above: written only by the run goroutine (handleMessage, retryDeferred,
	// completeJob, reconcileDeferred), so it needs no mu.
	deferred map[string]*deferredJob
	// Assignment-check state, same run-goroutine ownership (Q553). nextAssignmentCheck
	// paces the session refresh the check reads its statistics from;
	// lastZeroAssignedAt is when the current run of zero-assigned readings began, and
	// is zero whenever the last reading counted an assignment.
	nextAssignmentCheck time.Time
	lastZeroAssignedAt  time.Time

	// identityWarnOnce bounds the "assignment carried no run identity" warning to one
	// line per listener (Q417). Unlike the condition flags above it is touched from the
	// per-job provision path, so it is a sync.Once rather than a plain bool.
	identityWarnOnce sync.Once

	mu         sync.Mutex
	scaleSetID int
	// registeredLabels is what the server said the scale set carries, recorded when
	// ensureScaleSet created or found it (Q726). It is the server's answer rather than
	// the ask, which is the whole point: a GHES appliance that dropped the extra
	// labels reports the shortfall here and nowhere else.
	registeredLabels []string
	// The three replay guards. Each answers a redelivered JobAssigned, and each is retired
	// by retireGuards once every message carrying one for its job has been deleted — so
	// they are bounded by the work still in the queue, not by the jobs the listener has
	// handled over its lifetime (Q597). completed and abandoned — the conclusions — are
	// additionally persisted through Config.Guards ahead of each delete, so a hard kill
	// between a conclusion and its DELETE no longer loses them (Q606).
	provisioned map[string]bool // jobIDs provisioned this process (idempotency + replay guard)
	// completed holds jobIDs GitHub has reported terminal. It guards double-counting on
	// replay, and gates provisioning: a worker for a completed job would stall on the
	// Secret that job's completion reclaimed (Q575).
	completed map[string]bool
	// abandoned holds the jobIDs concluded gone at GitHub (Q553). Without it the fix
	// would be undone by the ordinary 404 heal: a re-created session polls from cursor 0
	// and replays the very JobAssigned the check acted on, and a jobID is a job's UUID —
	// one GitHub has stopped holding is not coming back under the same id.
	abandoned map[string]bool
	// pending holds every message acked by cursor but not yet deleted, keyed by message
	// id. An entry whose unsettled set is empty is settled and awaiting its delete, which
	// flushDeletes issues and retries (Q583).
	pending       map[int64]*pendingMessage
	lastStats     scaleset.RunnerScaleSetStatistic
	lastMessageID int64
	// guardsDirty is set whenever completed/abandoned membership changes, and cleared
	// by a successful GuardStore save. flushDeletes refuses to issue any DELETE while
	// it is set and the save fails — the write-ahead ordering Q606 rests on.
	guardsDirty bool
	// retained holds the jobIDs whose guards must survive the drained-queue sweep: a
	// delete the wire reported as removing nothing dropped their message from pending
	// while its replay may still be in the queue, so the guards are the only thing left
	// that would recognise it (the Q609 rule, extended to sweepStaleGuards).
	retained map[string]bool
}

// pendingMessage is one cursor-acked message the Listener has not yet deleted.
type pendingMessage struct {
	// assigned is the jobs the message carries a JobAssigned for, kept for the whole life
	// of the entry — it is what says which guards the delete retires. Deliberately not
	// the same set as unsettled, which settle empties before the delete lands, and
	// deliberately assignments only: those are the deliveries the guards answer.
	assigned map[string]bool
	// unsettled is the jobs that have not concluded. Empty means ready to delete.
	unsettled map[string]bool
}

// New validates cfg and builds a Listener.
func New(cfg Config) (*Listener, error) {
	if cfg.Client == nil {
		return nil, errors.New("scalesetlistener: Config.Client is required")
	}
	if cfg.ScaleSetName == "" {
		return nil, errors.New("scalesetlistener: Config.ScaleSetName is required")
	}
	if cfg.Provision == nil {
		return nil, errors.New("scalesetlistener: Config.Provision is required")
	}
	if cfg.Capacity == nil {
		return nil, errors.New("scalesetlistener: Config.Capacity is required")
	}
	l := &Listener{
		cfg:                cfg,
		log:                cfg.Log,
		workFolder:         cfg.WorkFolder,
		pollBackoff:        cfg.PollBackoff,
		rateLimitAfter:     cfg.RateLimitConditionAfter,
		deferredBackoff:    cfg.DeferredRetryBackoff,
		capacityInterval:   cfg.CapacityRetryInterval,
		assignmentInterval: cfg.AssignmentCheckInterval,
		provisioned:        make(map[string]bool),
		completed:          make(map[string]bool),
		abandoned:          make(map[string]bool),
		pending:            make(map[int64]*pendingMessage),
		deferred:           make(map[string]*deferredJob),
		retained:           make(map[string]bool),
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.workFolder == "" {
		l.workFolder = defaultWorkFolder
	}
	if l.pollBackoff <= 0 {
		l.pollBackoff = defaultPollBackoff
	}
	if l.rateLimitAfter <= 0 {
		l.rateLimitAfter = defaultRateLimitConditionAfter
	}
	if l.deferredBackoff <= 0 {
		l.deferredBackoff = defaultDeferredRetryBackoff
	}
	if l.capacityInterval <= 0 {
		l.capacityInterval = defaultCapacityRetryInterval
	}
	if l.assignmentInterval <= 0 {
		l.assignmentInterval = defaultAssignmentCheckInterval
	}
	return l, nil
}

// Status is a snapshot of the Listener's accounting, for the reconciler to publish on
// RunnerSet status.
type Status struct {
	// ScaleSetID is the server-assigned scale-set id (0 until ensured).
	ScaleSetID int
	// AssignedJobs is the server-authoritative totalAssignedJobs from the last reading —
	// the ARC clamp target and the RunnerSet's ActiveSessions/ActiveJobs proxy. It is
	// not a count of provisioned workers and leads one: a single poll may assign a
	// whole batch, and every envelope carries the fresh statistics, so the first
	// JobAssigned the Listener handles already reports the entire batch as assigned.
	AssignedJobs int
	// RunningJobs is the server-authoritative totalRunningJobs from the last poll.
	RunningJobs int
	// RegisteredLabels are the label names the scale set carries at GitHub, as the
	// server reported them when this Listener ensured the set — not what was asked
	// for. The two differ whenever a GHES appliance below 3.21 dropped the labels past
	// the first, or the scale set predates a label the owner has since appended, and
	// the caller reports that difference (Q726).
	//
	// Nil means no observation — not ensured yet, or a response that did not carry
	// labels — and a caller must publish nothing rather than a total shortfall.
	RegisteredLabels []string
}

// Status returns the latest accounting snapshot.
func (l *Listener) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{
		ScaleSetID:       l.scaleSetID,
		AssignedJobs:     l.lastStats.TotalAssignedJobs,
		RunningJobs:      l.lastStats.TotalRunningJobs,
		RegisteredLabels: append([]string(nil), l.registeredLabels...),
	}
}

// Start ensures the scale set and opens the session synchronously (so a caller fails
// fast on an auth or registration error), then runs the poll loop in a goroutine and
// returns a done channel closed when the loop exits (ctx cancellation or an
// unrecoverable error). On exit it deletes the session so a later Listener replays the
// queue to a fresh session.
func (l *Listener) Start(ctx context.Context) (<-chan struct{}, error) {
	ssID, err := l.ensureScaleSet(ctx)
	if err != nil {
		if isUnauthorized(err) {
			l.surfaceUnauthorized("EnsureScaleSet", err)
		}
		return nil, fmt.Errorf("scalesetlistener: ensure scale set %q: %w", l.cfg.ScaleSetName, err)
	}
	l.mu.Lock()
	l.scaleSetID = ssID
	l.mu.Unlock()

	// Before the session exists: a failure here must not leak a session, and the poll
	// loop must never run without the persisted guards — that would silently reopen the
	// very window the store closes (Q606).
	if err := l.loadGuards(ctx); err != nil {
		return nil, fmt.Errorf("scalesetlistener: load concluded-job guards for %q: %w", l.cfg.ScaleSetName, err)
	}

	sess, err := l.createSession(ctx, ssID)
	if err != nil {
		if isUnauthorized(err) {
			l.surfaceUnauthorized("CreateSession", err)
		}
		return nil, fmt.Errorf("scalesetlistener: open session for %q: %w", l.cfg.ScaleSetName, err)
	}

	// Collect registration records no live worker claims before polling. Runs after the
	// session is open so a listener that cannot work at all does not delete records
	// first, and best-effort throughout: a scale set must still acquire jobs when the
	// sweep fails.
	l.sweepUnclaimedRunners(ctx)

	// Session open — publish the healthy baseline for the conditions this Listener
	// owns. Unconditional (not transition-guarded) because an unauthorized episode
	// may have been surfaced by a PREVIOUS Listener instance that failed Start and
	// was discarded: this fresh instance carries no flag for it, and without the
	// baseline the abnormal condition would sit stale on the RunnerSet forever
	// after the credentials were fixed (Q325).
	l.setCondition(v2alpha1.ConditionDegraded, metav1.ConditionFalse,
		v2alpha1.ReasonSessionAuthorized, "scale-set session established")
	l.setCondition(v2alpha1.ConditionRateLimited, metav1.ConditionFalse,
		v2alpha1.ReasonPollingHealthy, "message polling healthy")
	l.setCondition(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionFalse,
		v2alpha1.ReasonJobsProvisioning, "no assigned job is waiting on a runner name or on worker capacity")

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.run(ctx, ssID, sess)
	}()
	return done, nil
}

// resolveRunnerGroup returns the runner group id the scale set must sit in, and
// whether Config named one at all. An unnamed group reports (defaultRunnerGroupID,
// false): a new scale set lands in GitHub's default group, and an existing one is
// left wherever it is rather than being dragged into the default group by omission.
//
// A named group that does not resolve is an error, never the default group — see
// ErrRunnerGroupNotFound.
func (l *Listener) resolveRunnerGroup(ctx context.Context) (int, bool, error) {
	if l.cfg.RunnerGroupName == "" {
		return defaultRunnerGroupID, false, nil
	}
	id, ok, err := l.cfg.Client.ResolveRunnerGroup(ctx, l.cfg.RunnerGroupName)
	if err != nil {
		return 0, false, fmt.Errorf("resolve runner group %q: %w", l.cfg.RunnerGroupName, err)
	}
	if !ok {
		return 0, false, fmt.Errorf("%w: %q", ErrRunnerGroupNotFound, l.cfg.RunnerGroupName)
	}
	return id, true, nil
}

// ensureScaleSet returns the id of the scale set named Config.ScaleSetName, creating
// it (ephemeral, one System label per declared runnerLabel) if it does not yet exist.
// Reusing an existing scale set is the restart-safe path: one scale-set object per
// group, created once (§2.1) — reconciled into Config.RunnerGroupName when one is
// declared.
//
// Either way it records the label set the SERVER reports back, which is not always the
// one asked for: a reused scale set predates any label appended since, and a GHES
// appliance below 3.21 keeps only the name label and drops the rest with no error
// (Q726). Nothing here corrects that — the caller reports the difference. The runner
// group is the one property an existing set IS reconciled on, because it is an
// authorization boundary rather than a match target (Q712).
func (l *Listener) ensureScaleSet(ctx context.Context) (int, error) {
	groupID, pinned, err := l.resolveRunnerGroup(ctx)
	if err != nil {
		return 0, err
	}
	existing, err := l.cfg.Client.GetRunnerScaleSetByName(ctx, l.cfg.ScaleSetName)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		// From the GET, which carries labels; the group PATCH below is not a label
		// observation and must not overwrite this one.
		l.recordRegisteredLabels(existing)
		if !pinned || existing.RunnerGroupID == groupID {
			return existing.ID, nil
		}
		// Adoption carries the group the scale set was created in, so declaring
		// runnerGroup on a set that already registered would otherwise leave the
		// wider group in force with nothing reporting it (Q712). Reconciling it here
		// is what makes the field mean the same thing on an existing set as on a new
		// one.
		if _, err := l.cfg.Client.UpdateRunnerScaleSet(ctx, existing.ID, scaleset.RunnerScaleSet{
			Name:          existing.Name,
			RunnerGroupID: groupID,
		}); err != nil {
			return 0, fmt.Errorf("move scale set %q into runner group %q: %w",
				l.cfg.ScaleSetName, l.cfg.RunnerGroupName, err)
		}
		l.log.Info("scaleset: moved scale set into its declared runner group",
			"scaleSet", l.cfg.ScaleSetName, "runnerGroup", l.cfg.RunnerGroupName,
			"fromGroupID", existing.RunnerGroupID, "toGroupID", groupID)
		return existing.ID, nil
	}
	created, err := l.cfg.Client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          l.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        l.desiredLabels(),
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return 0, err
	}
	l.recordRegisteredLabels(created)
	return created.ID, nil
}

// desiredLabels is the scale set's label set as this Listener asks for it: the name
// label first, then each extra label, duplicates dropped — the composition ARC sends.
// A duplicate would ask GitHub to hold one match target twice; the CRD does not
// enforce uniqueness within runnerLabels, so one can reach here.
func (l *Listener) desiredLabels() []scaleset.Label {
	seen := map[string]bool{l.cfg.ScaleSetName: true}
	out := []scaleset.Label{{Name: l.cfg.ScaleSetName, Type: systemLabelType}}
	for _, name := range l.cfg.ExtraLabels {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, scaleset.Label{Name: name, Type: systemLabelType})
	}
	return out
}

// recordRegisteredLabels stores the label names ss carries, as the server reported
// them.
//
// A response carrying NO labels records nothing rather than recording "carries
// nothing". A scale set always has at least the name label — the name is a label — so
// an empty list cannot be a scale set's true state; it means that response did not
// carry labels. Whether every read route returns them is not something this repo has
// measured, and reading a silent omission as a total shortfall would report every
// declared label missing on a set that is working perfectly.
func (l *Listener) recordRegisteredLabels(ss *scaleset.RunnerScaleSet) {
	if len(ss.Labels) == 0 {
		return
	}
	names := make([]string, 0, len(ss.Labels))
	for _, lbl := range ss.Labels {
		names = append(names, lbl.Name)
	}
	l.mu.Lock()
	l.registeredLabels = names
	l.mu.Unlock()
}

// sweepUnclaimedRunners deletes this scale set's registration records that no live
// worker pod claims — the recovery half of the registration-leak fix (Q550).
//
// Every generatejitconfig pre-registers a runner under a name derived from the job ID,
// and a worker that never runs its job leaves that record behind. Deregistering on reap
// closes the leak going forward, but nothing re-derives the name of a record whose pod
// is already gone (an AGC that crashed between the two) or of the suffixed names the
// conflict-retry path mints and never revisits. Those records hold the very names the
// affected jobs need, so a scale set can wedge against its own leftovers until an
// operator prunes them by hand — which is what happened to the 22 stale `gag-ci-e2e`
// records in the 2026-07-31 RC window.
//
// Three tests keep a record: claimed by a live worker pod, busy, or online. Claimed is
// the load-bearing one — a pod that has not started yet has an offline, unclaimed-looking
// record that is nonetheless about to be used, and deleting it would strand exactly the
// job this function exists to protect. So an unreadable claim set aborts the sweep
// rather than licensing a delete.
// It runs synchronously on Start, before the poll loop, so no assignment this listener
// makes can race it. That ordering is worth the latency, but it is on the reconcile
// path, so the whole sweep is bounded by sweepTimeout: a slow or paging-heavy listing
// gives up rather than holding a reconcile open.
func (l *Listener) sweepUnclaimedRunners(ctx context.Context) {
	if l.cfg.ClaimedRunnerNames == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()

	claimed, err := l.cfg.ClaimedRunnerNames(ctx)
	if err != nil {
		l.log.Warn("scaleset: skipping stale runner-record sweep; claimed runner names unavailable",
			"scaleSet", l.cfg.ScaleSetName, "err", err)
		return
	}

	prefix := l.cfg.ScaleSetName + "-"
	records, err := l.cfg.Client.ListRunnersWithPrefix(ctx, prefix)
	if err != nil {
		l.log.Warn("scaleset: list runner records for sweep", "scaleSet", l.cfg.ScaleSetName, "err", err)
		return
	}

	swept := 0
	for _, rec := range records {
		if _, ok := claimed[rec.Name]; ok {
			continue
		}
		if rec.Busy || rec.Online() {
			continue
		}
		if err := l.cfg.Client.DeregisterRunnerByID(ctx, rec.ID, rec.Name); err != nil {
			// A record that turned busy between the list and the delete is one the
			// sweep must leave alone; anything else is logged and skipped.
			l.log.Debug("scaleset: sweep could not delete stale runner record",
				"scaleSet", l.cfg.ScaleSetName, "runner", rec.Name, "err", err)
			continue
		}
		swept++
	}
	if swept > 0 {
		l.log.Info("scaleset: swept stale runner records left by workers that never ran their job",
			"scaleSet", l.cfg.ScaleSetName, "swept", swept, "examined", len(records))
	}
}

// createSession opens the scale set's single message-queue session.
func (l *Listener) createSession(ctx context.Context, ssID int) (*scaleset.RunnerScaleSetSession, error) {
	return l.cfg.Client.CreateSession(ctx, ssID, l.cfg.OwnerName)
}

// run is the poll loop. It long-polls the queue advertising free capacity, provisions
// a worker per assigned job, refreshes the queue token on 401, and re-creates the
// session (replaying unacked messages from cursor 0) on 404. It returns when ctx is
// cancelled or an unrecoverable error occurs, deleting the session on the way out.
//
// Every path out of a poll is paced: an error backs off (pollBackoff), and a 202 that
// the server did not hold is padded to minPollInterval, so no server response can turn
// the loop into a spin.
func (l *Listener) run(ctx context.Context, ssID int, sess *scaleset.RunnerScaleSetSession) {
	defer l.deleteSession(ssID, sess)
	// Ordered before the session delete by defer's LIFO: DeleteMessage is issued against
	// the session, and a dead one answers the 404 the client reads as a successful ack.
	defer l.flushDeletesOnExit(sess)
	// Ordered before the flush, for the same reason the flush is ordered before the
	// session delete: it is what gives the flush something to delete (Q689).
	defer l.drainConclusionsOnExit(sess)

	for {
		if ctx.Err() != nil {
			return
		}
		// Drop what GitHub no longer holds before re-offering the rest (Q553), so a
		// dangling assignment is never handed to the provisioner again.
		l.reconcileDeferred(ctx, ssID, sess)
		// Delete the messages whose jobs have all concluded (Q583). Called here and again
		// after handleMessage because those are the two places a job concludes, and any
		// network work between a conclusion and its delete is a window in which a stop
		// strands the message for the next process to replay (Q603).
		l.flushDeletes(ctx, sess)
		// Re-offer any job the queue will not deliver again (Q551). Once per poll cycle
		// is the natural cadence: each job carries its own backoff deadline, and the
		// deadlines are minutes while the loop turns over at worst every long-poll window.
		l.retryDeferred(ctx, ssID)
		capacity := l.cfg.Capacity(ctx)
		if capacity < 0 {
			capacity = 0
		}
		l.mu.Lock()
		cursor := l.lastMessageID
		l.mu.Unlock()

		polledAt := time.Now()
		msg, err := l.cfg.Client.GetMessage(ctx, sess, capacity, cursor)
		if err != nil {
			if !l.handlePollError(ctx, ssID, sess, err) {
				return
			}
			continue
		}
		l.pollHealthy()
		if msg == nil { // 202 — nothing to deliver
			// The queue is drained, so a guard no held message assigns has nothing left
			// to answer (Q606).
			l.sweepStaleGuards()
			// Pace the empty path: a server that did not actually hold the poll must not
			// spin the loop (minPollInterval). A real long-poll already outlasts the floor.
			if !l.paceEmptyPoll(ctx, time.Since(polledAt)) {
				return
			}
			continue
		}
		l.handleMessage(ctx, ssID, sess, msg)
		// The completions this delivery concluded, deleted before the next long poll
		// rather than after it (Q603).
		l.flushDeletes(ctx, sess)
	}
}

// paceEmptyPoll enforces minPollInterval between polls that delivered nothing: it waits
// out whatever is left of the floor after a poll that took elapsed, and returns false if
// ctx was cancelled while waiting. A poll that blocked at least minPollInterval — every
// poll against a server that honours the long poll — returns immediately.
func (l *Listener) paceEmptyPoll(ctx context.Context, elapsed time.Duration) bool {
	remaining := minPollInterval - elapsed
	if remaining <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(remaining)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// handlePollError reacts to a GetMessage error, returning true to continue the loop
// and false to stop. A 401/403 refreshes the queue token; a 404/410 re-creates the
// session; a 429 tracks the sustained-rate-limit condition and waits out any
// Retry-After; anything else backs off (unless ctx is cancelled). Session calls
// rejected as unauthorized surface the Degraded condition (Q325); a successful
// refresh or re-create clears it.
//
// The failures that are poll failures rather than heal triggers — a 429, and the
// transport/decode errors of the default branch — also increment the shared
// actions_gateway_message_poll_errors_total counter (Q446), giving the tier the
// rate-able signal its conditions cannot carry: a condition only trips once an
// episode outlasts rateLimitAfter, so a stream of brief episodes is invisible to it.
// The reason labels are the classic tier's, and the two heal branches deliberately
// count nothing, because the classic listener heals a 401/403 or 404/410 without
// touching the counter either — an operator's alert on the series keeps meaning what
// it did before the classic machinery goes away.
func (l *Listener) handlePollError(ctx context.Context, ssID int, sess *scaleset.RunnerScaleSetSession, err error) bool {
	switch {
	case ctx.Err() != nil:
		return false
	case isUnauthorized(err):
		if rerr := l.cfg.Client.RefreshSession(ctx, ssID, sess); rerr != nil {
			l.log.Warn("scaleset: refresh session token failed", "scaleSet", l.cfg.ScaleSetName, "err", rerr)
			if isUnauthorized(rerr) {
				// The refresh itself was rejected: not an expired queue token but
				// invalid/revoked credentials — the failure class the classic path
				// reports as Degraded/Unauthorized.
				l.surfaceUnauthorized("RefreshSession", rerr)
			}
			return l.backoff(ctx)
		}
		l.clearUnauthorized()
		return true
	case isNotFound(err):
		// The session is gone; re-create it and replay from the queue head. A fresh
		// session replays unacked messages (§2b-3), so provisioned jobs are skipped by
		// the idempotency set and any un-provisioned ones are re-read.
		l.log.Info("scaleset: session gone, re-creating", "scaleSet", l.cfg.ScaleSetName)
		fresh, cerr := l.createSession(ctx, ssID)
		if cerr != nil {
			l.log.Warn("scaleset: re-create session failed", "scaleSet", l.cfg.ScaleSetName, "err", cerr)
			if isUnauthorized(cerr) {
				l.surfaceUnauthorized("CreateSession", cerr)
			}
			return l.backoff(ctx)
		}
		l.clearUnauthorized()
		*sess = *fresh
		l.mu.Lock()
		l.lastMessageID = 0
		l.mu.Unlock()
		return true
	case isRateLimited(err):
		l.metricsIncPollError("rate_limited")
		// Track sustained rate limiting; surface the condition once the episode
		// outlasts rateLimitAfter (classic parity: ten minutes by default). The
		// next successful poll clears it (pollHealthy).
		now := time.Now()
		if l.rateLimitedSince.IsZero() {
			l.rateLimitedSince = now
		} else if !l.rateLimitedCond && now.Sub(l.rateLimitedSince) >= l.rateLimitAfter {
			l.rateLimitedCond = true
			l.setCondition(v2alpha1.ConditionRateLimited, metav1.ConditionTrue,
				v2alpha1.ReasonSustainedRateLimit,
				fmt.Sprintf("GetMessage returning 429 for over %s", l.rateLimitAfter))
		}
		l.log.Warn("scaleset: rate limited", "scaleSet", l.cfg.ScaleSetName, "err", err)
		// Honor a server-provided Retry-After; the queue rarely sends one (§2a-5),
		// so the usual wait is the plain pollBackoff.
		wait := l.pollBackoff
		var rl *scaleset.RateLimitError
		if errors.As(err, &rl) && rl.RetryAfter > 0 {
			wait = rl.RetryAfter
		}
		return l.backoffFor(ctx, wait)
	default:
		l.metricsIncPollError(pollErrorReason(err))
		l.log.Warn("scaleset: poll error", "scaleSet", l.cfg.ScaleSetName, "err", err)
		return l.backoff(ctx)
	}
}

// pollErrorReason classifies a poll failure that is neither a 429 nor a heal trigger
// into the classic tier's remaining reason labels: "timeout" for a long poll the
// server accepted but never answered (the client's response-header deadline fires,
// surfacing as a net.Error timeout — the black-holed-connection class), and "other"
// for every remaining transport or decode error. The loop returns early on parent
// context cancellation, so a cancelled ctx never reaches here as a timeout.
func pollErrorReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "other"
}

// backoff waits pollBackoff or until ctx is cancelled, returning true to continue and
// false if ctx was cancelled.
func (l *Listener) backoff(ctx context.Context) bool {
	return l.backoffFor(ctx, l.pollBackoff)
}

// backoffFor waits d or until ctx is cancelled, returning true to continue and false
// if ctx was cancelled.
func (l *Listener) backoffFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pollHealthy resets the failure tracking after a successful poll, clearing any
// condition pushed for the now-recovered episode (Q325).
func (l *Listener) pollHealthy() {
	l.rateLimitedSince = time.Time{}
	if l.rateLimitedCond {
		l.rateLimitedCond = false
		l.setCondition(v2alpha1.ConditionRateLimited, metav1.ConditionFalse,
			v2alpha1.ReasonPollingHealthy, "message polling recovered")
	}
	l.clearUnauthorized()
}

// surfaceUnauthorized publishes Degraded=True/Unauthorized and a SessionUnauthorized
// Warning event for a session call rejected as unauthorized — invalid or revoked
// GitHub App credentials (Q325). Pushed once per episode (on the transition into the
// state), so the retrying poll loop does not spam the event stream; action names the
// rejected call. clearUnauthorized ends the episode.
func (l *Listener) surfaceUnauthorized(action string, err error) {
	if l.unauthorizedCond {
		return
	}
	l.unauthorizedCond = true
	l.setCondition(v2alpha1.ConditionDegraded, metav1.ConditionTrue,
		v2alpha1.ReasonSessionUnauthorized, err.Error())
	l.recordEvent(corev1.EventTypeWarning, "SessionUnauthorized", action,
		fmt.Sprintf("scale-set %s rejected as unauthorized; the gateway's GitHub App credentials are invalid or revoked: %v", action, err))
}

// clearUnauthorized ends an unauthorized episode after a session call succeeds,
// clearing the Degraded condition surfaceUnauthorized pushed. A no-op outside an
// episode.
func (l *Listener) clearUnauthorized() {
	if !l.unauthorizedCond {
		return
	}
	l.unauthorizedCond = false
	l.setCondition(v2alpha1.ConditionDegraded, metav1.ConditionFalse,
		v2alpha1.ReasonSessionAuthorized, "scale-set session calls authorized again")
}

// setCondition publishes one condition via the owner-bound sink. A no-op when no
// sink is wired.
func (l *Listener) setCondition(condType string, status metav1.ConditionStatus, reason, msg string) {
	if l.cfg.Conditions == nil {
		return
	}
	l.cfg.Conditions.SetCondition(metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

// recordEvent records one owner-scoped Event via the owner-bound sink. A no-op when
// no sink is wired.
func (l *Listener) recordEvent(eventtype, reason, action, note string) {
	if l.cfg.Events == nil {
		return
	}
	l.cfg.Events.Event(eventtype, reason, action, note)
}

// handleMessage processes one queue envelope: claim any GHES-offered jobs, provision a
// worker per newly assigned job, account completions, then advance the cursor
// (see advanceCursor — the message is not delete-acked).
func (l *Listener) handleMessage(ctx context.Context, ssID int, sess *scaleset.RunnerScaleSetSession, msg *scaleset.RunnerScaleSetMessage) {
	if msg.Statistics != nil {
		l.mu.Lock()
		l.lastStats = *msg.Statistics
		l.mu.Unlock()
	}
	jobs, err := msg.Jobs()
	if err != nil {
		l.log.Warn("scaleset: decode message body", "scaleSet", l.cfg.ScaleSetName, "err", err)
		// Advance past an undecodable message so it does not wedge the cursor, and
		// delete it: it names no job, and nothing about a later delivery would make it
		// readable.
		l.advanceCursor(msg.MessageID)
		l.holdForDelete(msg.MessageID, nil, nil)
		return
	}

	// GHES path: claim each offered job so it comes back as JobAssigned. On the dotcom
	// auto-assign backend there are none.
	if ids := scaleset.AvailableJobIDs(jobs); len(ids) > 0 {
		if _, aerr := l.cfg.Client.AcquireJobs(ctx, ssID, sess, ids); aerr != nil {
			l.log.Warn("scaleset: acquire jobs", "scaleSet", l.cfg.ScaleSetName, "err", aerr)
		}
	}

	// Completions first, then assignments. A batch can carry both messages for one job
	// — a run cancelled between two polls, and every job in the queue when a re-created
	// session replays from cursor 0 — and provisioning first builds a worker whose
	// Secret the completion then deletes, stranding the pod Pending on a Secret that no
	// longer exists (Q575). Handling the completion first lets provisionAssigned ack
	// past the assignment instead, so no pod is created for a job already over.
	cleaned := make(map[string]bool)
	for _, cj := range completedJobs(jobs) {
		if l.completeJob(ctx, cj) {
			cleaned[cj.JobID] = true
		}
	}

	ackable := true
	for _, aj := range scaleset.AssignedJobs(jobs) {
		l.metricsIncAssigned()
		outcome := l.provisionAssigned(ctx, ssID, aj)
		switch {
		case outcome == provisionRetry:
			ackable = false
		case outcome.deferReason() != "":
			l.deferJob(aj, outcome.deferReason())
		}
	}

	// Ack (advance the cursor) unless a job needs a redelivery retry. A provisioned or
	// already-provisioned job is ackable; so is a deferred one (advancing past it is what
	// stops one stuck assignment from wedging the batch — Q270; the Listener re-offers it
	// itself). Only a transient failure (provisionRetry) holds the cursor so the message
	// redelivers — which on a long-poll queue is immediate, so nothing that will still be
	// true on the next delivery may take this path (Q576).
	if ackable {
		l.advanceCursor(msg.MessageID)
		// The delete half. A message whose jobs have all concluded goes on the next
		// flush; one still owed a worker is held, so a restart re-reads it (Q583).
		l.holdForDelete(msg.MessageID, assignedJobIDs(jobs), l.unsettledJobs(jobs, cleaned))
	}
}

// deferredJob is an assignment the Listener acked past but could not provision, held
// for a later re-offer. Nothing else will retry it: the queue re-delivers a message
// only to a re-created session, and even that stops at the advanced cursor — so
// dropping it here leaves the workflow run queued at GitHub forever (Q551).
type deferredJob struct {
	job      scaleset.JobMessage
	reason   string // DeferReason*, selecting the re-offer schedule and the reported stall
	attempts int
	nextAt   time.Time
	// deferredAt is when the job entered the set, which is what the assignment check
	// compares against: only a job already held when GitHub was first seen holding
	// nothing can be concluded gone (Q553).
	deferredAt time.Time
}

// deferJob schedules an unprovisionable assignment for a re-offer under the given
// DeferReason* and, when it is the first of an episode, surfaces the stall on the owning
// RunnerSet. Re-deferring a job already held reschedules it; an empty reason keeps the
// one it is already held under, which is what a re-offer that failed transiently does.
func (l *Listener) deferJob(aj scaleset.JobMessage, reason string) {
	d, held := l.deferred[aj.JobID]
	if !held {
		d = &deferredJob{job: aj, reason: DeferReasonNameConflict, deferredAt: time.Now()}
		l.deferred[aj.JobID] = d
	}
	reasonChanged := reason != "" && reason != d.reason
	if reason != "" {
		d.reason = reason
	}
	d.attempts++
	wait := l.waitForAttempt(d.reason, d.attempts)
	d.nextAt = time.Now().Add(wait)
	// Info, not Warn, for a ceiling hold: a saturated set re-offering its queue is
	// working as configured, and one line per job per interval is a steady stream.
	if d.reason == DeferReasonCeiling {
		l.log.Info("scaleset: no worker capacity for assigned job; re-offering",
			"scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID, "attempt", d.attempts, "retryIn", wait)
	} else {
		l.log.Warn("scaleset: job cannot be provisioned; re-offering after backoff",
			"scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID, "attempt", d.attempts, "retryIn", wait)
	}
	// The published stall names each reason's jobs, so a job that changed reason
	// republishes even though the membership did not.
	if !held || reasonChanged {
		l.refreshStalled()
	}
}

// resolveDeferred drops a job from the deferred set once it has provisioned or the
// queue has reported it complete, clearing the stall condition with the last one. A
// no-op for a job that was never deferred.
func (l *Listener) resolveDeferred(jobID string) {
	if _, held := l.deferred[jobID]; !held {
		return
	}
	delete(l.deferred, jobID)
	l.refreshStalled()
}

// retryDeferred re-offers every deferred job whose backoff has elapsed. A job that
// provisions leaves the set; one that still cannot is rescheduled on the next backoff
// step. Each re-offer walks the same conflict ladder the first attempt did, so a single
// stalled job can hold the loop for a few pollBackoffs — bounded, and paid at most once
// per backoff window per job.
func (l *Listener) retryDeferred(ctx context.Context, ssID int) {
	for _, d := range l.dueDeferred() {
		if ctx.Err() != nil {
			return
		}
		outcome := l.provisionAssigned(ctx, ssID, d.job)
		if outcome == provisionAcked {
			l.log.Info("scaleset: deferred job provisioned on re-offer",
				"scaleSet", l.cfg.ScaleSetName, "jobID", d.job.JobID, "attempts", d.attempts)
			l.resolveDeferred(d.job.JobID)
			continue
		}
		// A re-offer that failed transiently names no reason, so the job stays held
		// under the one it already had rather than being reclassified by a blip.
		l.deferJob(d.job, outcome.deferReason())
	}
}

// reconcileDeferred gives up on the assignments GitHub no longer holds, so the Listener
// stops re-offering — and eventually provisioning a worker for — a job that no longer
// exists (Q553).
//
// Until this existed the only thing that ended a re-offer was a terminal JobCompleted,
// and the backend does not always send one: a run deleted, a job re-queued elsewhere, a
// queued job GitHub concluded on its own. The v1.3.0-rc.3 dogfood gate recorded the
// end state — fifteen assignments still being retried against a scale set whose
// statistics reported zero — and it clears only by hand, because every exit from the
// deferred set is one the backend has to volunteer.
//
// The statistics are the missing signal. A deferred job is by construction assigned and
// not complete, so GitHub counts it in totalAssignedJobs; a reading of zero while the
// Listener holds any is a contradiction with exactly one resolution. Reading zero says
// nothing about *which* jobs are gone when it is positive, so the check acts only on the
// unambiguous case — and that is precisely the draining set this exists to unwedge.
//
// Two readings, not one. A count is server state the assignment may briefly lead, so a
// job is abandoned only once a zero reading brackets it on both sides; the cutoff walks
// forward each round, so a job deferred mid-run is caught on the round after the next.
//
// The reading comes from a session refresh because a stalled set has nothing to poll for:
// GetMessage answers 202, and a 202 carries no statistics. The call is made only while
// something is held, and at most once per assignmentInterval.
func (l *Listener) reconcileDeferred(ctx context.Context, ssID int, sess *scaleset.RunnerScaleSetSession) {
	if len(l.deferred) == 0 {
		l.nextAssignmentCheck = time.Time{}
		l.lastZeroAssignedAt = time.Time{}
		return
	}
	now := time.Now()
	if now.Before(l.nextAssignmentCheck) {
		return
	}
	l.nextAssignmentCheck = now.Add(l.assignmentInterval)

	if err := l.cfg.Client.RefreshSession(ctx, ssID, sess); err != nil {
		// Nothing to conclude from a failed reading, and nothing else depends on it —
		// the poll loop refreshes its own token on a 401.
		l.log.Debug("scaleset: session refresh for the assignment check failed",
			"scaleSet", l.cfg.ScaleSetName, "err", err)
		return
	}
	stats := sess.Statistics
	if stats == nil {
		// The backend reported no statistics, which is not the same as reporting zero.
		return
	}
	l.mu.Lock()
	l.lastStats = *stats
	l.mu.Unlock()

	if stats.TotalAssignedJobs > 0 {
		l.lastZeroAssignedAt = time.Time{}
		return
	}
	cutoff := l.lastZeroAssignedAt
	l.lastZeroAssignedAt = now
	if cutoff.IsZero() {
		return // first zero reading of this run — confirm it on the next
	}
	l.abandonDeferredBefore(cutoff)
}

// abandonDeferredBefore drops every deferred job that entered the set before cutoff,
// reporting the loss on the log, the abandoned counter, and an owner Event. Each one is
// a workflow run that will never run — GitHub has already stopped waiting for it, so
// this reports the loss rather than causing it.
func (l *Listener) abandonDeferredBefore(cutoff time.Time) {
	var gone []string
	for id, d := range l.deferred {
		if d.deferredAt.Before(cutoff) {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		return
	}
	sort.Strings(gone)
	l.mu.Lock()
	for _, id := range gone {
		delete(l.deferred, id)
		l.abandoned[id] = true
	}
	l.guardsDirty = true
	l.mu.Unlock()
	// A job GitHub has stopped holding has concluded as far as the queue goes, so it
	// releases the message that was held for it (Q583). Without this the assignment
	// replays to the next session and the give-up is undone by the restart.
	for _, id := range gone {
		l.settle(id)
	}
	l.log.Warn("scaleset: giving up on assigned jobs the scale set no longer holds",
		"scaleSet", l.cfg.ScaleSetName, "jobs", strings.Join(gone, ", "), "count", len(gone))
	l.metricsIncAbandoned(len(gone))
	// The note names no job ids, so repeats aggregate into one counted Event.
	l.recordEvent(corev1.EventTypeWarning, "AssignmentAbandoned", "ProvisionWorker",
		fmt.Sprintf("gave up on %d assigned job(s): the scale set reports no assigned jobs at all, so GitHub is no longer holding them and re-offering them would provision workers for jobs that no longer exist", len(gone)))
	l.refreshStalled()
}

// dueDeferred returns the deferred jobs whose next-attempt deadline has passed.
func (l *Listener) dueDeferred() []*deferredJob {
	now := time.Now()
	var due []*deferredJob
	for _, d := range l.deferred {
		if !d.nextAt.After(now) {
			due = append(due, d)
		}
	}
	return due
}

// waitForAttempt returns the wait before attempt n of a deferred job's re-offer. A job
// held for capacity gets a flat, short interval; one held on a runner-name conflict gets
// the exponential ladder. See defaultCapacityRetryInterval for why the two differ.
func (l *Listener) waitForAttempt(reason string, attempts int) time.Duration {
	if reason == DeferReasonCeiling {
		return l.capacityInterval
	}
	return l.backoffForAttempt(attempts)
}

// backoffForAttempt returns the wait before attempt n of a deferred job's re-offer: the
// configured base doubled per attempt, capped at maxDeferredRetryBackoff.
func (l *Listener) backoffForAttempt(attempts int) time.Duration {
	wait := l.deferredBackoff
	for i := 1; i < attempts && wait < maxDeferredRetryBackoff; i++ {
		wait *= 2
	}
	if wait > maxDeferredRetryBackoff {
		wait = maxDeferredRetryBackoff
	}
	return wait
}

// refreshStalled publishes the JobProvisionStalled condition for the current deferred
// set — True naming the held jobs, False once the last one clears — and records the
// event once per episode, on the transition in (surfaceUnauthorized's rule). Called on
// every membership change, so the message names the jobs an operator would look for.
func (l *Listener) refreshStalled() {
	counts := l.deferredCounts()
	l.metricsSetDeferred(counts)
	if len(l.deferred) == 0 {
		if l.stalledCond {
			l.stalledCond = false
			l.stalledEventReason = ""
			l.setCondition(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionFalse,
				v2alpha1.ReasonJobsProvisioning, "no assigned job is waiting on a runner name or on worker capacity")
		}
		return
	}
	// A name conflict outranks a full ceiling whenever both are held: it is the one an
	// operator can act on, and a ceiling clears itself as workers finish.
	reason := v2alpha1.ReasonWorkerCeilingReached
	eventType, eventReason := corev1.EventTypeNormal, "WorkerCeilingReached"
	if counts[DeferReasonNameConflict] > 0 {
		reason = v2alpha1.ReasonRunnerNameConflict
		eventType, eventReason = corev1.EventTypeWarning, "JobProvisionStalled"
	}
	l.setCondition(v2alpha1.ConditionJobProvisionStalled, metav1.ConditionTrue,
		reason, l.stalledMessage(counts))
	// Once per episode, on the transition in — and again when the episode changes class,
	// so a name conflict arriving while a set is already held at its ceiling still gets
	// its Warning rather than being swallowed by the Normal one already recorded.
	if !l.stalledCond || l.stalledEventReason != eventReason {
		l.stalledCond = true
		l.stalledEventReason = eventReason
		// The event note deliberately names no job ids: the apiserver aggregates
		// repeats of an identical note into one counted event, and a saturated set
		// enters and leaves this state as fast as its jobs turn over.
		l.recordEvent(eventType, eventReason, "ProvisionWorker", stalledEventNote(counts))
	}
}

// stalledMessage renders the deferred set as one condition message: a clause per reason
// holding jobs, each naming its ids and its re-offer cadence.
func (l *Listener) stalledMessage(counts map[string]int) string {
	var clauses []string
	if n := counts[DeferReasonNameConflict]; n > 0 {
		clauses = append(clauses, fmt.Sprintf("%d cannot register a runner name, re-offered up to every %s (%s)",
			n, maxDeferredRetryBackoff, strings.Join(l.deferredJobIDs(DeferReasonNameConflict), ", ")))
	}
	if n := counts[DeferReasonCeiling]; n > 0 {
		clauses = append(clauses, fmt.Sprintf("%d waiting for worker capacity, re-offered every %s (%s)",
			n, l.capacityInterval, strings.Join(l.deferredJobIDs(DeferReasonCeiling), ", ")))
	}
	return fmt.Sprintf("%d assigned job(s) cannot be provisioned yet: %s",
		len(l.deferred), strings.Join(clauses, "; "))
}

// stalledEventNote is the episode's event note — the same content as the condition
// message minus the job ids, so repeats aggregate into one counted Event.
func stalledEventNote(counts map[string]int) string {
	if counts[DeferReasonNameConflict] > 0 {
		return "one or more assigned jobs cannot register a runner name and are being re-offered on a backoff; the JobProvisionStalled condition names them"
	}
	return "assigned jobs are waiting for worker capacity (the set is at its worker ceiling) and are being re-offered; the JobProvisionStalled condition names them"
}

// deferredCounts tallies the held jobs by DeferReason*, carrying an explicit zero for
// every reason so the gauge never leaves a series frozen at its last non-zero value.
func (l *Listener) deferredCounts() map[string]int {
	counts := map[string]int{DeferReasonCeiling: 0, DeferReasonNameConflict: 0}
	for _, d := range l.deferred {
		counts[d.reason]++
	}
	return counts
}

// deferredJobIDs returns the jobIDs held under one reason in a stable order, so a
// condition message that names several does not churn on every republish.
func (l *Listener) deferredJobIDs(reason string) []string {
	ids := make([]string, 0, len(l.deferred))
	for id, d := range l.deferred {
		if d.reason == reason {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// provisionOutcome is the result of trying to provision one assigned job — the signal
// handleMessage uses to decide whether the message may be acked (Q270).
type provisionOutcome int

const (
	// provisionAcked: the job is provisioned (or already was) — safe to advance the cursor.
	provisionAcked provisionOutcome = iota
	// provisionRetry: a transient failure — leave the cursor so the message redelivers and
	// the job is retried on a later poll. Reserved for failures that a redelivery a moment
	// later could plausibly clear (an API blip, an apiserver 500).
	provisionRetry
	// provisionDeferConflict: a persistent runner-name conflict — advance the cursor
	// anyway so this one stuck assignment does not wedge the batch, and defer the job for
	// a later re-offer.
	provisionDeferConflict
	// provisionDeferCapacity: the owner is at its worker ceiling — same cursor handling,
	// on the shorter capacity schedule (Q576).
	provisionDeferCapacity
)

// deferReason maps an outcome to the DeferReason* the job is held under, or "" for an
// outcome that defers nothing.
func (o provisionOutcome) deferReason() string {
	switch o {
	case provisionDeferConflict:
		return DeferReasonNameConflict
	case provisionDeferCapacity:
		return DeferReasonCeiling
	default:
		return ""
	}
}

// provisionAssigned mints a JIT config for an assigned job and provisions its worker,
// idempotently. It returns provisionAcked when the job is provisioned (or already was),
// provisionRetry on a transient failure that should redeliver, and one of the
// provisionDefer* outcomes when the job cannot be provisioned now but will be re-offered
// (ack past it rather than wedge the cursor — Q270; the caller defers it — Q551/Q576).
func (l *Listener) provisionAssigned(ctx context.Context, ssID int, aj scaleset.JobMessage) provisionOutcome {
	l.mu.Lock()
	already, gone, over := l.provisioned[aj.JobID], l.abandoned[aj.JobID], l.completed[aj.JobID]
	l.mu.Unlock()
	if already {
		return provisionAcked
	}
	if gone {
		// A replay of an assignment the check concluded GitHub no longer holds. Ack past
		// it rather than build a worker with nothing to run (Q553).
		l.log.Debug("scaleset: skipping replayed assignment for a job GitHub no longer has",
			"scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID)
		return provisionAcked
	}
	if over {
		// GitHub already reported this job terminal, so its Secret is reclaimed and a
		// worker built now would stall Pending on a Secret that no longer exists (Q575).
		// Reached from the same batch (completions are handled first) and from a replay
		// after the completion.
		l.log.Debug("scaleset: skipping assignment for a job already completed",
			"scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID)
		return provisionAcked
	}

	// Capacity first, because minting the JIT config below registers a runner at GitHub:
	// a job the ceiling will reject anyway must not pay for a registration the next
	// attempt then has to deregister (Q576).
	if l.atCapacity(ctx) {
		return provisionDeferCapacity
	}

	jit, runnerName, outcome := l.generateJITConfig(ctx, ssID, aj.JobID)
	if outcome != provisionAcked {
		return outcome
	}
	// Run identity travels with the assignment, not with a payload: it is the only
	// point at which the AGC learns which workflow run this job belongs to, so it is
	// handed to the provisioner to stamp durably on the worker pod (Q417). An
	// assignment with no complete identity provisions exactly as before, with the
	// triple left empty.
	owner, repo, runID, haveIdentity := aj.RunIdentity()
	if !haveIdentity {
		// Warned once per listener, not once per job: a backend that omits the identity
		// omits it for every assignment, and this is a per-job hot path (Q87 Theme D).
		// The per-eviction signal is the one that matters operationally, and the
		// provisioner owns it (a counter plus an owner Event, on the evictions that
		// actually could not be recovered).
		l.identityWarnOnce.Do(func() {
			l.log.Warn("scaleset: assigned job carries no run identity; automatic eviction recovery will not fire for this scale set",
				"scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID)
		})
	}
	if err := l.cfg.Provision(ctx, Job{
		JobID:      aj.JobID,
		RunnerName: runnerName,
		JITConfig:  jit.EncodedJITConfig,
		Owner:      owner,
		Repository: repo,
		RunID:      runID,
		JobName:    aj.JobDisplayName,
	}); err != nil {
		// The pre-check above passed but the authoritative one did not — a sibling job
		// took the last slot in between. Backpressure, not a provisioning error: it is
		// counted by the deferred gauge, not the error counter (Q576).
		if errors.Is(err, ErrCapacityUnavailable) {
			return provisionDeferCapacity
		}
		l.log.Warn("scaleset: provision worker", "scaleSet", l.cfg.ScaleSetName, "jobID", aj.JobID, "err", err)
		l.metricsIncProvisionError()
		return provisionRetry
	}
	l.mu.Lock()
	l.provisioned[aj.JobID] = true
	l.mu.Unlock()
	l.metricsIncProvisioned()
	return provisionAcked
}

// atCapacity reports whether the owner's worker ceiling has no room for another worker,
// via the optional pre-check. A check that could not be made fails open: it is an
// optimisation over the authoritative check the provisioner still makes, and an
// unreadable pod count must not stall assignments.
func (l *Listener) atCapacity(ctx context.Context) bool {
	if l.cfg.CheckCapacity == nil {
		return false
	}
	err := l.cfg.CheckCapacity(ctx)
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCapacityUnavailable) {
		return true
	}
	l.log.Warn("scaleset: worker capacity check failed; provisioning anyway",
		"scaleSet", l.cfg.ScaleSetName, "err", err)
	return false
}

// generateJITConfig mints a JIT config for a job, returning the config and the runner
// name actually registered on success (provisionAcked); provisionDeferConflict when a
// runner-name conflict persists past the bound (give up on this attempt rather than
// replay the same request forever — Q270); and provisionRetry on any other error, so the
// message redelivers.
//
// The deterministic base name ({scaleSet}-{jobID}) collides when a reaped never-started
// worker left an offline record under it (Q334). The first recovery is to delete that
// stale record and re-register under the SAME base name — clearing the collision at its
// source so the offline records stop accumulating. Only if the record cannot be reclaimed
// (a live runner holds it, or a transient delete error) does it fall back to the bounded
// fresh-name retry (Q270): replaying the same colliding name would 409 indefinitely and
// wedge the queue cursor, so a suffixed fresh name is the safe last resort — the worker
// pod's own jobID-deterministic name keeps a fresh-name re-register from double-executing.
func (l *Listener) generateJITConfig(ctx context.Context, ssID int, jobID string) (*scaleset.JITRunnerConfig, string, provisionOutcome) {
	base := l.runnerName(jobID, 0)
	jit, err := l.cfg.Client.GenerateJITConfig(ctx, ssID, base, l.workFolder)
	if err == nil {
		return jit, base, provisionAcked
	}
	if !isRunnerNameConflict(err) {
		l.log.Warn("scaleset: generate JIT config", "scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "err", err)
		l.metricsIncProvisionError()
		return nil, "", provisionRetry
	}

	// Base name conflict: try to reclaim it by deleting the stale record (Q334), then
	// re-register under the same base name. A self-healed reclaim is not a provision error.
	if deleted, derr := l.cfg.Client.DeregisterRunnerByName(ctx, base); derr != nil {
		l.log.Debug("scaleset: deregister stale runner on name conflict failed; falling back to a fresh name",
			"scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "err", derr)
	} else if deleted {
		if jit, err := l.cfg.Client.GenerateJITConfig(ctx, ssID, base, l.workFolder); err == nil {
			l.log.Info("scaleset: reclaimed stale runner name after deregister",
				"scaleSet", l.cfg.ScaleSetName, "jobID", jobID)
			return jit, base, provisionAcked
		}
		// Base still unusable (re-created record, or a transient error) — fall through.
	} else {
		// The name is taken at generatejitconfig but the REST name filter matched
		// nothing, so the reclaim had nothing to delete. Every retry below then mints a
		// suffixed name this path never revisits, which is one way a set accumulates
		// records it can never clear itself — the start-up sweep is what collects them.
		l.log.Warn("scaleset: runner name is taken but no record resolves under it; the per-name reclaim cannot clear it",
			"scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "runner", base)
	}

	// Bounded fresh-name retry: the base name could not be reclaimed.
	for attempt := 1; ; attempt++ {
		name := l.runnerName(jobID, attempt)
		jit, err := l.cfg.Client.GenerateJITConfig(ctx, ssID, name, l.workFolder)
		if err == nil {
			return jit, name, provisionAcked
		}
		if !isRunnerNameConflict(err) {
			l.log.Warn("scaleset: generate JIT config", "scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "err", err)
			l.metricsIncProvisionError()
			return nil, "", provisionRetry
		}
		if attempt >= maxJITNameConflictRetries {
			l.log.Warn("scaleset: runner name conflict persists, deferring job",
				"scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "attempts", attempt+1, "err", err)
			l.metricsIncProvisionError()
			return nil, "", provisionDeferConflict
		}
		l.log.Info("scaleset: runner name conflict, retrying under a fresh name",
			"scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "attempt", attempt+1)
		if !l.backoff(ctx) { // ctx cancelled mid-backoff — a fresh listener re-reads the queue
			return nil, "", provisionRetry
		}
	}
}

// completeJob records a terminal JobCompleted, counting the completion metric at most
// once per job even if the message replays to a re-created session, and reclaiming the
// job's staged worker Secret (Q373).
//
// The metric is deduped; the cleanup deliberately is NOT. Re-running an idempotent
// delete costs one tolerated NotFound, and running it on every delivery makes replay a
// free backstop: a completion the previous process handled just before it crashed —
// after the queue message was written but before the Secret was deleted — is reclaimed
// when a re-created session replays that completion from cursor 0.
//
// It returns whether the job is now fully concluded — reclaim included. A false keeps
// the completion's message in the queue (Q583), which is what preserves that backstop
// now that a handled message is otherwise deleted.
func (l *Listener) completeJob(ctx context.Context, cj scaleset.JobMessage) bool {
	l.mu.Lock()
	first := !l.completed[cj.JobID]
	l.completed[cj.JobID] = true
	if first {
		l.guardsDirty = true
	}
	l.mu.Unlock()
	// A deferred job GitHub has given up on (timed out, or the run was cancelled) stops
	// being re-offered here. It is the prompt signal that an assignment is gone, but not
	// the only one — reconcileDeferred covers the losses that arrive as no message at all
	// (Q553).
	l.resolveDeferred(cj.JobID)
	if first && l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobCompleted(cj.Result)
	}
	if l.cfg.Cleanup == nil {
		l.settle(cj.JobID)
		return true
	}
	// Best-effort: a failed reclaim leaves the Secret to the RunnerSet's cascade-GC
	// (the pre-Q373 behaviour) rather than holding the cursor, which would redeliver
	// the whole batch and re-provision nothing useful.
	if err := l.cfg.Cleanup(ctx, cj.JobID); err != nil {
		l.log.Warn("scaleset: reclaim completed job's worker Secret",
			"scaleSet", l.cfg.ScaleSetName, "jobID", cj.JobID, "err", err)
		return false
	}
	l.settle(cj.JobID)
	return true
}

// advanceCursor moves lastMessageID past a handled message — the first half of the
// ack. Within a session the cursor alone prevents redelivery, but it is session-scoped
// at the backend while the queue log is not, so a cursor-only ack leaves the message to
// replay to the next session (Q583, measured live 2026-08-01). holdForDelete carries
// the second half.
func (l *Listener) advanceCursor(messageID int64) {
	l.mu.Lock()
	if messageID > l.lastMessageID {
		l.lastMessageID = messageID
	}
	l.mu.Unlock()
}

// holdForDelete registers a cursor-acked message for the delete half of the ack:
// assigned is the jobs it carries a JobAssigned for, unsettled the jobs it names that
// have not concluded. A message naming none is registered settled, so the next
// flushDeletes removes it.
//
// The wait is what keeps replay working where it is the recovery path rather than the
// bug: a job provisioned but still running, and a Q551 deferred job the previous
// process never provisioned at all, both hold their message in the queue, so a restart
// re-reads them. Only a job that has concluded — completed with its Secret reclaimed,
// or abandoned (Q553) — releases one.
func (l *Listener) holdForDelete(messageID int64, assigned, unsettled map[string]bool) {
	l.mu.Lock()
	l.pending[messageID] = &pendingMessage{assigned: assigned, unsettled: unsettled}
	l.mu.Unlock()
}

// settle marks a job concluded, releasing every held message waiting on it. A job is
// named by two messages — its JobAssigned and its JobCompleted — so this drops it from
// all of them rather than from one. The assigned sets are untouched: that is what
// retireGuards reads, and a settled job whose assignment is still queued still needs
// its guards.
func (l *Listener) settle(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.pending {
		delete(p.unsettled, jobID)
	}
}

// retireGuards drops the replay guards for the jobs a just-deleted message assigned,
// keeping any whose assignment another held message still carries. Caller holds mu, with
// the message already out of pending.
//
// The guards answer exactly one thing: a redelivered JobAssigned. So an entry is dead
// once every message carrying a JobAssigned for its job has been deleted — and at least
// one has, which is why a completion-only message retires nothing (its job's assignment
// may still be ahead of the cursor; Q575's replayed-after-completion case). Deleting is
// itself gated on the job having concluded, so this cannot retire a guard for work still
// in flight (Q597). The caller gates this on the wire confirming the delete removed
// something, so "deleted" here means the queue really no longer holds it.
//
// This is the prompt retirement path; sweepStaleGuards is the drained-queue backstop
// for the entries it structurally cannot reach (Q606).
func (l *Listener) retireGuards(p *pendingMessage) {
	if p == nil {
		return
	}
	for jobID := range p.assigned {
		if l.assignmentPending(jobID) {
			continue
		}
		delete(l.provisioned, jobID)
		// A retirement is a store change too; the next cycle's save garbage-collects
		// the entry (Q606). provisioned is not persisted, so it dirties nothing.
		if l.completed[jobID] || l.abandoned[jobID] {
			l.guardsDirty = true
		}
		delete(l.completed, jobID)
		delete(l.abandoned, jobID)
	}
}

// assignmentPending reports whether any undeleted message still carries a JobAssigned for
// jobID. Caller holds mu.
func (l *Listener) assignmentPending(jobID string) bool {
	for _, p := range l.pending {
		if p.assigned[jobID] {
			return true
		}
	}
	return false
}

// assignedJobIDs is the set of jobs a message carries a JobAssigned for — the deliveries
// the replay guards exist to answer, and so the guards its delete may retire.
func assignedJobIDs(jobs []scaleset.JobMessage) map[string]bool {
	assigned := make(map[string]bool)
	for _, j := range scaleset.AssignedJobs(jobs) {
		assigned[j.JobID] = true
	}
	return assigned
}

// flushDeletes issues the delete half of the ack for every settled message, dropping
// each one it deletes along with the replay guards that delete retires (Q597). A failure
// leaves the entry in place, so the next poll cycle retries it — which is why this runs
// per cycle rather than only at settle time.
//
// A 404/410 completes the ack too — a message already gone is nothing left to do — so
// the entry is dropped either way. That is only safe because the endpoint is known
// served (Investigation G measured it answering 204, Q583), which is why a delete the
// wire reports as already-gone is logged: systematic 404s mean the endpoint stopped
// serving deletes and the queue is no longer being pruned, and nothing else would say
// so (Q609).
func (l *Listener) flushDeletes(ctx context.Context, sess *scaleset.RunnerScaleSetSession) {
	// Write-ahead: a DELETE is irreversible at the queue, so the conclusion that
	// authorised it must be durable before any is issued. A failed save skips the whole
	// cycle's deletes — they retry next cycle, exactly like a failed delete (Q606).
	if !l.saveGuards(ctx) {
		return
	}
	l.mu.Lock()
	var ready []int64
	for messageID, p := range l.pending {
		if len(p.unsettled) == 0 {
			ready = append(ready, messageID)
		}
	}
	l.mu.Unlock()

	for _, messageID := range ready {
		if ctx.Err() != nil {
			return
		}
		deleted, err := l.cfg.Client.DeleteMessage(ctx, sess, messageID)
		if err != nil {
			l.log.Warn("scaleset: delete acked message; it will replay to the next session until this succeeds",
				"scaleSet", l.cfg.ScaleSetName, "messageID", messageID, "err", err)
			continue
		}
		if !deleted {
			l.log.Warn("scaleset: queue reported the acked message already gone; if this is not isolated the "+
				"delete endpoint is not being served and the queue is not being pruned",
				"scaleSet", l.cfg.ScaleSetName, "messageID", messageID)
		}
		l.mu.Lock()
		p := l.pending[messageID]
		delete(l.pending, messageID)
		// Retire only on a delete the wire confirms removed something. A 404 completes the
		// ack, but it is also how a backend that does not serve the endpoint answers — and
		// there the message is still in the queue, leaving the guards as the only thing
		// that would recognise its replay (Q609). Such guards are marked retained so the
		// drained-queue sweep leaves them alone too.
		if deleted {
			l.retireGuards(p)
		} else if p != nil {
			for jobID := range p.assigned {
				l.retained[jobID] = true
			}
		}
		l.mu.Unlock()
	}
}

// unsettledJobs returns the jobs in a message that have not concluded, which is what
// holds it back from the delete half of the ack. cleaned names the jobs whose
// completion this delivery finished handling, Secret reclaim included — a completion
// whose reclaim failed is deliberately not settled, so the message replays and the
// reclaim is retried (the Q373/Q575 backstop).
func (l *Listener) unsettledJobs(jobs []scaleset.JobMessage, cleaned map[string]bool) map[string]bool {
	unsettled := make(map[string]bool)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, j := range jobs {
		if cleaned[j.JobID] || l.abandoned[j.JobID] {
			continue
		}
		if l.completed[j.JobID] && j.MessageType != scaleset.MessageTypeJobCompleted {
			// The assignment for a job whose completion has already been handled.
			continue
		}
		unsettled[j.JobID] = true
	}
	return unsettled
}

// loadGuards seeds the completed/abandoned guards from the store before the poll loop
// starts, so a replayed assignment for a job a hard-killed predecessor concluded is
// acked past instead of provisioned (Q606). An entry whose assignment does replay is
// retired by that message's confirmed delete; one whose messages are already gone is
// retired by sweepStaleGuards once the queue proves drained.
func (l *Listener) loadGuards(ctx context.Context) error {
	if l.cfg.Guards == nil {
		return nil
	}
	state, err := l.cfg.Guards.Load(ctx)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, jobID := range state.Completed {
		l.completed[jobID] = true
	}
	for _, jobID := range state.Abandoned {
		l.abandoned[jobID] = true
	}
	return nil
}

// saveGuards persists the concluded-job guards when they have changed since the last
// successful save, returning whether the store now reflects them. Snapshot and flag are
// consistent without holding mu across the write: every mutation happens on the poll
// goroutine, which is also the only caller.
func (l *Listener) saveGuards(ctx context.Context) bool {
	if l.cfg.Guards == nil {
		return true
	}
	l.mu.Lock()
	if !l.guardsDirty {
		l.mu.Unlock()
		return true
	}
	state := GuardState{Completed: sortedKeys(l.completed), Abandoned: sortedKeys(l.abandoned)}
	l.mu.Unlock()
	if err := l.cfg.Guards.Save(ctx, state); err != nil {
		l.log.Warn("scaleset: persist concluded-job guards; holding message deletes until it succeeds",
			"scaleSet", l.cfg.ScaleSetName, "err", err)
		return false
	}
	l.mu.Lock()
	l.guardsDirty = false
	l.mu.Unlock()
	return true
}

// sweepStaleGuards retires, on an empty poll, every guard no held message assigns —
// the entries retireGuards structurally cannot reach: a store-loaded guard whose
// messages a previous process already deleted, and a completed guard re-added by a
// replayed completion after its assignment's message was gone. A 202 is what makes the
// judgement sound: it means the queue is drained (an unacked message redelivers
// instead), so a guard with no pending assignment has no message left anywhere that
// could replay it. Without this the persisted set would accrete such entries forever —
// the unbounded set again, in etcd (Q606).
//
// The retained set is the one exemption: a delete the wire reported as removing
// nothing dropped its message from pending while the message may still be in the
// queue, and there the guard is the only recognition left (Q609). Between 202s,
// nothing here fires — prompt retirement stays the confirmed delete's.
func (l *Listener) sweepStaleGuards() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, guards := range []map[string]bool{l.completed, l.abandoned} {
		for jobID := range guards {
			if l.assignmentPending(jobID) || l.retained[jobID] {
				continue
			}
			delete(guards, jobID)
			delete(l.provisioned, jobID)
			l.guardsDirty = true
		}
	}
}

// sortedKeys returns a map's keys in a stable order, for the persisted guard state and
// the test hooks that assert on it.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// drainConclusionsOnExit reads the conclusions the queue is already holding for the jobs
// this Listener still owes a delete, so the flush that follows can release their
// messages (Q689). A job concludes at GitHub, not here: its JobCompleted sits in the
// queue until a poll reads it, and the loop is single-goroutine, so everything it does
// between two polls — provisioning, re-offering a deferred job, refreshing the session —
// is a window in which a stop takes the process away with the conclusion unread. The
// held assignment then replays and the next process builds a worker for a job that ran.
//
// It settles nothing on its own authority. A job is concluded only by completeJob, off a
// terminal JobCompleted GitHub published, which is the same authority the poll loop acts
// on; the messages it reads go through the same holdForDelete bookkeeping, so one still
// naming an unconcluded job stays in the queue and replays. An assignment it reads is
// neither provisioned nor acked — the cursor it walks is local, and a cursor ack is
// session-scoped at the backend (Q583, measured 2026-08-01), so a message this skips
// still replays to the next session.
//
// The poll advertises zero capacity: the process is leaving and must not invite another
// assignment. That does not cost it the completions it came for — a Listener at its
// ceiling polls with zero capacity for as long as it is full, and a saturated scale set
// drains because those polls keep delivering JobCompleted.
func (l *Listener) drainConclusionsOnExit(sess *scaleset.RunnerScaleSetSession) {
	if sess == nil || sess.SessionID == "" {
		return
	}
	if !l.awaitingConclusion() {
		return
	}
	l.mu.Lock()
	cursor := l.lastMessageID
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
	defer cancel()
	settled := 0
	defer func() {
		if settled > 0 {
			l.log.Info("scaleset: read conclusions the queue was still holding at shutdown",
				"scaleSet", l.cfg.ScaleSetName, "jobs", settled)
		}
	}()
	for l.awaitingConclusion() {
		msg, err := l.cfg.Client.GetMessage(ctx, sess, 0, cursor)
		if err != nil {
			l.log.Debug("scaleset: read outstanding conclusions on shutdown",
				"scaleSet", l.cfg.ScaleSetName, "err", err)
			return
		}
		if msg == nil { // 202 — the queue has nothing more to say
			return
		}
		if msg.MessageID <= cursor {
			// A backend that ignores the cursor would otherwise spin this loop between
			// two deadline checks, since a delivered message costs no wait.
			return
		}
		cursor = msg.MessageID
		jobs, jerr := msg.Jobs()
		if jerr != nil {
			continue // names no job, so it holds nothing back; the loop deletes it on the next start
		}
		cleaned := make(map[string]bool)
		for _, cj := range completedJobs(jobs) {
			if l.completeJob(ctx, cj) {
				cleaned[cj.JobID] = true
				settled++
			}
		}
		l.holdForDelete(msg.MessageID, assignedJobIDs(jobs), l.unsettledJobs(jobs, cleaned))
	}
}

// awaitingConclusion reports whether any cursor-acked message is still held back from its
// delete by a job that has not concluded — the only state the exit drain can improve.
func (l *Listener) awaitingConclusion() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.pending {
		if len(p.unsettled) > 0 {
			return true
		}
	}
	return false
}

// flushDeletesOnExit issues the outstanding delete half of the ack as the loop exits,
// on a context detached from the cancelled one so it still runs (the deleteSession
// rule). A job concludes in memory at settle and its message is deleted by a later
// flushDeletes; a stop in between would otherwise leave that message in the queue for
// the next process, which polls from cursor 0 with its provisioned/completed/abandoned
// guards all empty and provisions a worker for a job that is over (Q603).
func (l *Listener) flushDeletesOnExit(sess *scaleset.RunnerScaleSetSession) {
	if sess == nil || sess.SessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	l.flushDeletes(ctx, sess)
}

// deleteSession tears the session down on loop exit, on a fresh background context so
// it still runs when the loop's ctx is already cancelled. A later Listener re-creates
// the session and replays any unacked messages.
func (l *Listener) deleteSession(ssID int, sess *scaleset.RunnerScaleSetSession) {
	if sess == nil || sess.SessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	if err := l.cfg.Client.DeleteSession(ctx, ssID, sess.SessionID); err != nil {
		l.log.Debug("scaleset: delete session on shutdown", "scaleSet", l.cfg.ScaleSetName, "err", err)
	}
}

// runnerName derives a deterministic runner name from a jobID: attempt 0 is the base
// name, so a replay of the same job (attempt 0 again) names the same runner and stays
// idempotent. A non-zero attempt appends a numeric suffix, the fresh name used to
// recover from a runner-name conflict — the base name collided with a stale registration
// (Q270).
func (l *Listener) runnerName(jobID string, attempt int) string {
	base := l.cfg.ScaleSetName + "-" + jobID
	if attempt == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, attempt)
}

func (l *Listener) metricsIncAssigned() {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobAssigned()
	}
}

func (l *Listener) metricsIncProvisioned() {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobProvisioned()
	}
}

func (l *Listener) metricsIncProvisionError() {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncProvisionError()
	}
}

// metricsSetDeferred publishes how many jobs are held for a re-offer, by reason (Q551,
// Q576).
func (l *Listener) metricsSetDeferred(byReason map[string]int) {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.SetDeferredJobs(byReason)
	}
}

// metricsIncAbandoned counts assignments given up on because GitHub no longer holds
// them (Q553).
func (l *Listener) metricsIncAbandoned(n int) {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobsAbandoned(n)
	}
}

// metricsIncPollError counts one GetMessage failure into the shared cross-tier
// poll-error counter (Q446), when a recorder is wired.
func (l *Listener) metricsIncPollError(reason string) {
	if l.cfg.PollErrors != nil {
		l.cfg.PollErrors.IncPollError(reason)
	}
}

// completedJobs returns the JobCompleted entries in a batched message body.
func completedJobs(jobs []scaleset.JobMessage) []scaleset.JobMessage {
	var out []scaleset.JobMessage
	for _, j := range jobs {
		if j.MessageType == scaleset.MessageTypeJobCompleted {
			out = append(out, j)
		}
	}
	return out
}

// isUnauthorized reports whether err is (or wraps) a scaleset.UnauthorizedError.
func isUnauthorized(err error) bool {
	var ue *scaleset.UnauthorizedError
	return errors.As(err, &ue)
}

// isNotFound reports whether err is (or wraps) a scaleset.NotFoundError.
func isNotFound(err error) bool {
	var ne *scaleset.NotFoundError
	return errors.As(err, &ne)
}

// isRateLimited reports whether err is (or wraps) a scaleset.RateLimitError.
func isRateLimited(err error) bool {
	var rl *scaleset.RateLimitError
	return errors.As(err, &rl)
}

// isRunnerNameConflict reports whether err is (or wraps) a scaleset.RunnerNameConflictError.
func isRunnerNameConflict(err error) bool {
	var rce *scaleset.RunnerNameConflictError
	return errors.As(err, &rce)
}
