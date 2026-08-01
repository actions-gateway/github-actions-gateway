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
// There is no in-memory acquisition registry: unacked queue messages replay to a
// re-created session (poll from cursor 0), so a session drop or an AGC restart
// re-reads assigned-but-unprovisioned jobs from the queue (§2b-3). Provisioning is
// idempotent per jobID (a deterministic worker name), so a replay never double-runs a
// job.
//
// One job class is outside that replay: an assignment the Listener acked past because it
// could not be provisioned — a runner name a stale registration holds (Q551), or a worker
// ceiling already full (Q576). The cursor has moved beyond it, so no session will deliver
// it again — the Listener keeps it and re-offers it until it runs or GitHub reports it
// complete, reporting the stall as JobProvisionStalled meanwhile. Redelivery is reserved
// for genuinely transient failures, because the queue redelivers immediately: a condition
// that will still hold a moment later would spin the loop, and each pass would mint (and
// then have to deregister) another runner registration.
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

// Label type for the scale set's single System label (its runs-on match target).
const systemLabelType = "System"

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
	// ScaleSetName is the scale set's name AND its single runs-on label (the tenant's
	// single runnerLabel; CEL guarantees exactly one for a ScaleSet set).
	ScaleSetName string
	// RunnerGroupName is the runner group to place the scale set in. Empty resolves to
	// GitHub's default group (id 1).
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
}

// Listener owns one scale set's acquisition session and provisions workers for its
// assigned jobs. Construct it with New; drive it with Start.
type Listener struct {
	cfg              Config
	log              *slog.Logger
	workFolder       string
	pollBackoff      time.Duration
	rateLimitAfter   time.Duration
	deferredBackoff  time.Duration
	capacityInterval time.Duration

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
	// completeJob), so it needs no mu.
	deferred map[string]*deferredJob

	// identityWarnOnce bounds the "assignment carried no run identity" warning to one
	// line per listener (Q417). Unlike the condition flags above it is touched from the
	// per-job provision path, so it is a sync.Once rather than a plain bool.
	identityWarnOnce sync.Once

	mu            sync.Mutex
	scaleSetID    int
	provisioned   map[string]bool // jobIDs provisioned this process (idempotency + replay guard)
	completed     map[string]bool // jobIDs seen completed (guards double-counting on replay)
	lastStats     scaleset.RunnerScaleSetStatistic
	lastMessageID int64
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
		cfg:              cfg,
		log:              cfg.Log,
		workFolder:       cfg.WorkFolder,
		pollBackoff:      cfg.PollBackoff,
		rateLimitAfter:   cfg.RateLimitConditionAfter,
		deferredBackoff:  cfg.DeferredRetryBackoff,
		capacityInterval: cfg.CapacityRetryInterval,
		provisioned:      make(map[string]bool),
		completed:        make(map[string]bool),
		deferred:         make(map[string]*deferredJob),
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
	return l, nil
}

// Status is a snapshot of the Listener's accounting, for the reconciler to publish on
// RunnerSet status.
type Status struct {
	// ScaleSetID is the server-assigned scale-set id (0 until ensured).
	ScaleSetID int
	// AssignedJobs is the server-authoritative totalAssignedJobs from the last poll —
	// the ARC clamp target and the RunnerSet's ActiveSessions/ActiveJobs proxy. It is
	// not a count of provisioned workers and leads one: a single poll may assign a
	// whole batch, and every envelope carries the fresh statistics, so the first
	// JobAssigned the Listener handles already reports the entire batch as assigned.
	AssignedJobs int
	// RunningJobs is the server-authoritative totalRunningJobs from the last poll.
	RunningJobs int
}

// Status returns the latest accounting snapshot.
func (l *Listener) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{
		ScaleSetID:   l.scaleSetID,
		AssignedJobs: l.lastStats.TotalAssignedJobs,
		RunningJobs:  l.lastStats.TotalRunningJobs,
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

// ensureScaleSet returns the id of the scale set named Config.ScaleSetName, creating
// it (ephemeral, single System label = its name) if it does not yet exist. Reusing an
// existing scale set is the restart-safe path: one scale-set object per group,
// created once (§2.1).
func (l *Listener) ensureScaleSet(ctx context.Context) (int, error) {
	existing, err := l.cfg.Client.GetRunnerScaleSetByName(ctx, l.cfg.ScaleSetName)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return existing.ID, nil
	}
	groupID := 1 // GitHub's default runner group
	if l.cfg.RunnerGroupName != "" {
		id, ok, err := l.cfg.Client.ResolveRunnerGroup(ctx, l.cfg.RunnerGroupName)
		if err != nil {
			return 0, fmt.Errorf("resolve runner group %q: %w", l.cfg.RunnerGroupName, err)
		}
		if ok {
			groupID = id
		}
	}
	created, err := l.cfg.Client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          l.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: l.cfg.ScaleSetName, Type: systemLabelType}},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return 0, err
	}
	return created.ID, nil
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

	for {
		if ctx.Err() != nil {
			return
		}
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
			// Pace the empty path: a server that did not actually hold the poll must not
			// spin the loop (minPollInterval). A real long-poll already outlasts the floor.
			if !l.paceEmptyPoll(ctx, time.Since(polledAt)) {
				return
			}
			continue
		}
		l.handleMessage(ctx, ssID, sess, msg)
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
		// Advance past an undecodable message so it does not wedge the cursor.
		l.advanceCursor(msg.MessageID)
		return
	}

	// GHES path: claim each offered job so it comes back as JobAssigned. On the dotcom
	// auto-assign backend there are none.
	if ids := scaleset.AvailableJobIDs(jobs); len(ids) > 0 {
		if _, aerr := l.cfg.Client.AcquireJobs(ctx, ssID, sess, ids); aerr != nil {
			l.log.Warn("scaleset: acquire jobs", "scaleSet", l.cfg.ScaleSetName, "err", aerr)
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
	for _, cj := range completedJobs(jobs) {
		l.completeJob(ctx, cj)
	}

	// Ack (advance the cursor) unless a job needs a redelivery retry. A provisioned or
	// already-provisioned job is ackable; so is a deferred one (advancing past it is what
	// stops one stuck assignment from wedging the batch — Q270; the Listener re-offers it
	// itself). Only a transient failure (provisionRetry) holds the cursor so the message
	// redelivers — which on a long-poll queue is immediate, so nothing that will still be
	// true on the next delivery may take this path (Q576).
	if ackable {
		l.advanceCursor(msg.MessageID)
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
}

// deferJob schedules an unprovisionable assignment for a re-offer under the given
// DeferReason* and, when it is the first of an episode, surfaces the stall on the owning
// RunnerSet. Re-deferring a job already held reschedules it; an empty reason keeps the
// one it is already held under, which is what a re-offer that failed transiently does.
func (l *Listener) deferJob(aj scaleset.JobMessage, reason string) {
	d, held := l.deferred[aj.JobID]
	if !held {
		d = &deferredJob{job: aj, reason: DeferReasonNameConflict}
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
	already := l.provisioned[aj.JobID]
	l.mu.Unlock()
	if already {
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
func (l *Listener) completeJob(ctx context.Context, cj scaleset.JobMessage) {
	l.mu.Lock()
	first := !l.completed[cj.JobID]
	l.completed[cj.JobID] = true
	l.mu.Unlock()
	// A deferred job GitHub has given up on (timed out, or the run was cancelled) stops
	// being re-offered here: the completion is the only signal that its assignment is
	// gone (Q551).
	l.resolveDeferred(cj.JobID)
	if first && l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobCompleted(cj.Result)
	}
	if l.cfg.Cleanup == nil {
		return
	}
	// Best-effort: a failed reclaim leaves the Secret to the RunnerSet's cascade-GC
	// (the pre-Q373 behaviour) rather than holding the cursor, which would redeliver
	// the whole batch and re-provision nothing useful.
	if err := l.cfg.Cleanup(ctx, cj.JobID); err != nil {
		l.log.Warn("scaleset: reclaim completed job's worker Secret",
			"scaleSet", l.cfg.ScaleSetName, "jobID", cj.JobID, "err", err)
	}
}

// advanceCursor moves lastMessageID past a handled message. Acking is cursor-advance
// only: within a session the cursor prevents redelivery, which is the ack semantics
// the live probe proved (§2b-4). The listener deliberately does NOT delete-ack via
// Client.DeleteMessage, whose wire shape is source-derived but unproven live (§2.2
// caveat, a P2-surfaced unknown for P4). The consequence is that a re-created session
// polls from cursor 0 and replays every message; the process-scoped provisioned/
// completed sets make that replay idempotent (no double-provision, no double-count).
//
// Because those sets are never pruned, they grow with the jobs a listener handles over
// its lifetime — an accepted P3 cost of cursor-only acking. Once P4 confirms the
// DeleteMessage wire shape, delete-acking prunes the queue so the sets can be bounded
// to in-flight jobs.
func (l *Listener) advanceCursor(messageID int64) {
	l.mu.Lock()
	if messageID > l.lastMessageID {
		l.lastMessageID = messageID
	}
	l.mu.Unlock()
}

// deleteSession tears the session down on loop exit, on a fresh background context so
// it still runs when the loop's ctx is already cancelled. A later Listener re-creates
// the session and replays any unacked messages.
func (l *Listener) deleteSession(ssID int, sess *scaleset.RunnerScaleSetSession) {
	if sess == nil || sess.SessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
