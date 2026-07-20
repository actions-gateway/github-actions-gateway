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
// queue advertising its free worker slots as the X-ScaleSetMaxCapacity header (the
// Q59 admission gate as batch size). GitHub assigns at most that many jobs, exactly
// once each (dotcom auto-assign; on GHES the JobAvailable→AcquireJobs path claims
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

// defaultRateLimitConditionAfter is how long GetMessage must have been answered 429
// before the Listener surfaces RateLimited=True on the owning RunnerSet — the same
// ten-minute window the classic listener uses, so a transient burst does not flap
// the condition (Q325).
const defaultRateLimitConditionAfter = 10 * time.Minute

// maxJITNameConflictRetries bounds how many times provisioning re-mints a JIT config
// under a fresh runner name after a RunnerNameConflictError. Past it, the assignment is
// skipped (not replayed forever), so a persistently colliding name — a stale registered
// runner — cannot wedge the queue cursor behind it (Q270). The skipped job is
// re-assigned or timed out server-side.
const maxJITNameConflictRetries = 3

// Job is one assigned job the Listener hands to its ProvisionFunc. The listener has
// already minted the JIT config; the provisioner stages it into the worker Secret and
// creates a run.sh --jitconfig worker pod.
type Job struct {
	// JobID is the job's server-assigned UUID — stable across replay, so it keys the
	// worker's deterministic name and the Listener's provisioned-once set.
	JobID string
	// RunnerName is the name the JIT config pre-registered the runner under.
	RunnerName string
	// RunnerRequestID is the acquire-flow id (0 on the auto-assign backend).
	RunnerRequestID int64
	// JITConfig is the base64 run.sh --jitconfig blob for this one job.
	JITConfig string
}

// ProvisionFunc provisions one worker pod for an assigned job. It must be idempotent
// per Job.JobID: a re-created session replays unacked JobAssigned messages, so the
// same job may be handed over more than once, and a duplicate must be a no-op (the
// provisioner names the pod/Secret deterministically from the jobID). A non-nil error
// leaves the job un-provisioned so the Listener retries it on a later poll.
type ProvisionFunc func(ctx context.Context, job Job) error

// CleanupFunc releases the per-job resources ProvisionFunc staged — the worker's
// JIT-config Secret — once the job is terminally complete (Q373). It is called with
// the jobID of every terminal JobCompleted the queue delivers, and must be idempotent:
// a re-created session replays completions from cursor 0, so the same job may be
// cleaned up more than once, and a job may complete having never been provisioned by
// this process. The worker pod itself is NOT this hook's concern — the owning
// reconciler's reaper collects terminal pods on spec.completedPodTTL.
type CleanupFunc func(ctx context.Context, jobID string) error

// CapacityFunc returns the number of free worker slots right now — the value
// advertised as X-ScaleSetMaxCapacity, so GitHub assigns at most that many jobs. It
// is the scale-set expression of the Q59 admission gate (maxWorkers/priorityTiers
// minus in-flight worker pods). A non-positive return advertises zero capacity: the
// Listener still polls (to drain JobStarted/JobCompleted and keep the session alive)
// but GitHub assigns nothing.
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
	// IncProvisionError counts a failed provision (the job is retried).
	IncProvisionError()
	// IncJobCompleted counts a terminal JobCompleted, labelled by result — the
	// completion signal the classic protocol never delivered (§2b-6).
	IncJobCompleted(result string)
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
	// Capacity returns the free worker slots to advertise each poll. Required.
	Capacity CapacityFunc
	// Cleanup releases a completed job's staged worker resources (its JIT-config
	// Secret). Nil disables reclaim, which leaks one Secret per job until the owning
	// RunnerSet is deleted — so the reconciler always wires it (Q373).
	Cleanup CleanupFunc
	// Metrics records job accounting. Nil is safe.
	Metrics MetricsRecorder
	// Conditions publishes the Listener's session-failure conditions onto the owning
	// RunnerSet (Q325): Degraded=True/Unauthorized when a session call is rejected as
	// unauthorized, and RateLimited=True/SustainedRateLimit when message polling has
	// been answered 429 past RateLimitConditionAfter — the classic listener's failure
	// vocabulary. Unlike the classic path, the Listener also publishes the healthy
	// (False) states on start and clears an abnormal state when the session recovers.
	// Nil disables condition reporting.
	Conditions ConditionSetter
	// Events records an owner-scoped Warning event (SessionUnauthorized) when the
	// Degraded condition trips, so the incident surfaces in `kubectl describe`.
	// Emitted once per episode, on the transition into the state. Nil disables.
	Events EventSink
	// RateLimitConditionAfter is how long message polling must have been answered 429
	// before RateLimited=True is surfaced. Non-positive selects
	// defaultRateLimitConditionAfter (ten minutes, classic parity). Overridable in
	// tests to drive the condition deterministically.
	RateLimitConditionAfter time.Duration
	// Log is the structured logger. Nil selects slog.Default().
	Log *slog.Logger
	// PollBackoff paces the transient-error retry path. Non-positive selects
	// defaultPollBackoff.
	PollBackoff time.Duration
}

// Listener owns one scale set's acquisition session and provisions workers for its
// assigned jobs. Construct it with New; drive it with Start.
type Listener struct {
	cfg            Config
	log            *slog.Logger
	workFolder     string
	pollBackoff    time.Duration
	rateLimitAfter time.Duration

	// Session-failure condition state (Q325), owned by Start and then the run
	// goroutine — the goroutine-creation happens-before makes the handoff safe
	// without mu. Each abnormal condition is pushed once per episode (on the
	// transition into the state) and cleared on recovery.
	rateLimitedSince time.Time // first 429 of the current episode; zero while polling is healthy
	rateLimitedCond  bool      // RateLimited=True has been pushed for the current episode
	unauthorizedCond bool      // Degraded=True/Unauthorized has been pushed

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
		cfg:            cfg,
		log:            cfg.Log,
		workFolder:     cfg.WorkFolder,
		pollBackoff:    cfg.PollBackoff,
		rateLimitAfter: cfg.RateLimitConditionAfter,
		provisioned:    make(map[string]bool),
		completed:      make(map[string]bool),
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
		l.log.Warn("scaleset: poll error", "scaleSet", l.cfg.ScaleSetName, "err", err)
		return l.backoff(ctx)
	}
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
// worker per newly assigned job, account completions, then advance the cursor and
// best-effort delete-ack the message.
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
		if l.provisionAssigned(ctx, ssID, aj) == provisionRetry {
			ackable = false
		}
	}
	for _, cj := range completedJobs(jobs) {
		l.completeJob(ctx, cj)
	}

	// Ack (advance the cursor) unless a job needs a redelivery retry. A provisioned or
	// already-provisioned job is ackable; so is a permanently-skipped one (advancing past
	// it is what stops one stuck assignment from wedging the batch — Q270). Only a
	// transient failure (provisionRetry) holds the cursor so the message redelivers.
	if ackable {
		l.advanceCursor(msg.MessageID)
	}
}

// provisionOutcome is the result of trying to provision one assigned job — the signal
// handleMessage uses to decide whether the message may be acked (Q270).
type provisionOutcome int

const (
	// provisionAcked: the job is provisioned (or already was) — safe to advance the cursor.
	provisionAcked provisionOutcome = iota
	// provisionRetry: a transient failure — leave the cursor so the message redelivers and
	// the job is retried on a later poll.
	provisionRetry
	// provisionSkip: the job cannot be provisioned (a persistent runner-name conflict) —
	// advance the cursor anyway so this one stuck assignment does not wedge the batch.
	provisionSkip
)

// provisionAssigned mints a JIT config for an assigned job and provisions its worker,
// idempotently. It returns provisionAcked when the job is provisioned (or already was),
// provisionRetry on a transient failure that should redeliver, and provisionSkip when a
// persistent runner-name conflict makes the job unprovisionable (skip it rather than
// wedge the cursor — Q270).
func (l *Listener) provisionAssigned(ctx context.Context, ssID int, aj scaleset.JobMessage) provisionOutcome {
	l.mu.Lock()
	already := l.provisioned[aj.JobID]
	l.mu.Unlock()
	if already {
		return provisionAcked
	}

	jit, runnerName, outcome := l.generateJITConfig(ctx, ssID, aj.JobID)
	if outcome != provisionAcked {
		return outcome
	}
	if err := l.cfg.Provision(ctx, Job{
		JobID:           aj.JobID,
		RunnerName:      runnerName,
		RunnerRequestID: aj.RunnerRequestID,
		JITConfig:       jit.EncodedJITConfig,
	}); err != nil {
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

// generateJITConfig mints a JIT config for a job, returning the config and the runner
// name actually registered on success (provisionAcked); provisionSkip when a runner-name
// conflict persists past the bound (fail this one job rather than replay the same request
// forever — Q270); and provisionRetry on any other error, so the message redelivers.
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
			l.log.Warn("scaleset: runner name conflict persists, skipping job",
				"scaleSet", l.cfg.ScaleSetName, "jobID", jobID, "attempts", attempt+1, "err", err)
			l.metricsIncProvisionError()
			return nil, "", provisionSkip
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
