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

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// Label type for the scale set's single System label (its runs-on match target).
const systemLabelType = "System"

// defaultPollBackoff is how long the loop waits before retrying after a transient
// poll error (a non-401/404 error, or a rate-limit with no Retry-After). A healthy
// long-poll blocks ~50s server-side, so this only paces the error path.
const defaultPollBackoff = 2 * time.Second

// defaultWorkFolder is the runner work directory baked into a JIT config when Config
// leaves it empty (the actions-runner convention).
const defaultWorkFolder = "_work"

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

// CapacityFunc returns the number of free worker slots right now — the value
// advertised as X-ScaleSetMaxCapacity, so GitHub assigns at most that many jobs. It
// is the scale-set expression of the Q59 admission gate (maxWorkers/priorityTiers
// minus in-flight worker pods). A non-positive return advertises zero capacity: the
// Listener still polls (to drain JobStarted/JobCompleted and keep the session alive)
// but GitHub assigns nothing.
type CapacityFunc func(ctx context.Context) int

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
	// Metrics records job accounting. Nil is safe.
	Metrics MetricsRecorder
	// Log is the structured logger. Nil selects slog.Default().
	Log *slog.Logger
	// PollBackoff paces the transient-error retry path. Non-positive selects
	// defaultPollBackoff.
	PollBackoff time.Duration
}

// Listener owns one scale set's acquisition session and provisions workers for its
// assigned jobs. Construct it with New; drive it with Start.
type Listener struct {
	cfg         Config
	log         *slog.Logger
	workFolder  string
	pollBackoff time.Duration

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
		cfg:         cfg,
		log:         cfg.Log,
		workFolder:  cfg.WorkFolder,
		pollBackoff: cfg.PollBackoff,
		provisioned: make(map[string]bool),
		completed:   make(map[string]bool),
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
	return l, nil
}

// Status is a snapshot of the Listener's accounting, for the reconciler to publish on
// RunnerSet status.
type Status struct {
	// ScaleSetID is the server-assigned scale-set id (0 until ensured).
	ScaleSetID int
	// AssignedJobs is the server-authoritative totalAssignedJobs from the last poll —
	// the ARC clamp target and the RunnerSet's ActiveSessions/ActiveJobs proxy.
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
		return nil, fmt.Errorf("scalesetlistener: ensure scale set %q: %w", l.cfg.ScaleSetName, err)
	}
	l.mu.Lock()
	l.scaleSetID = ssID
	l.mu.Unlock()

	sess, err := l.createSession(ctx, ssID)
	if err != nil {
		return nil, fmt.Errorf("scalesetlistener: open session for %q: %w", l.cfg.ScaleSetName, err)
	}

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

		msg, err := l.cfg.Client.GetMessage(ctx, sess, capacity, cursor)
		if err != nil {
			if !l.handlePollError(ctx, ssID, sess, err) {
				return
			}
			continue
		}
		if msg == nil { // 202 — nothing to deliver
			continue
		}
		l.handleMessage(ctx, ssID, sess, msg)
	}
}

// handlePollError reacts to a GetMessage error, returning true to continue the loop
// and false to stop. A 401/403 refreshes the queue token; a 404/410 re-creates the
// session; anything else backs off (unless ctx is cancelled).
func (l *Listener) handlePollError(ctx context.Context, ssID int, sess *scaleset.RunnerScaleSetSession, err error) bool {
	switch {
	case ctx.Err() != nil:
		return false
	case isUnauthorized(err):
		if rerr := l.cfg.Client.RefreshSession(ctx, ssID, sess); rerr != nil {
			l.log.Warn("scaleset: refresh session token failed", "scaleSet", l.cfg.ScaleSetName, "err", rerr)
			return l.backoff(ctx)
		}
		return true
	case isNotFound(err):
		// The session is gone; re-create it and replay from the queue head. A fresh
		// session replays unacked messages (§2b-3), so provisioned jobs are skipped by
		// the idempotency set and any un-provisioned ones are re-read.
		l.log.Info("scaleset: session gone, re-creating", "scaleSet", l.cfg.ScaleSetName)
		fresh, cerr := l.createSession(ctx, ssID)
		if cerr != nil {
			l.log.Warn("scaleset: re-create session failed", "scaleSet", l.cfg.ScaleSetName, "err", cerr)
			return l.backoff(ctx)
		}
		*sess = *fresh
		l.mu.Lock()
		l.lastMessageID = 0
		l.mu.Unlock()
		return true
	default:
		l.log.Warn("scaleset: poll error", "scaleSet", l.cfg.ScaleSetName, "err", err)
		return l.backoff(ctx)
	}
}

// backoff waits pollBackoff or until ctx is cancelled, returning true to continue and
// false if ctx was cancelled.
func (l *Listener) backoff(ctx context.Context) bool {
	t := time.NewTimer(l.pollBackoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
		l.completeJob(cj)
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

// generateJITConfig mints a JIT config for a job, recovering from a runner-name conflict
// by retrying under a fresh name with backoff, bounded by maxJITNameConflictRetries. It
// returns the config and the runner name actually registered on success (provisionAcked);
// provisionSkip when the name conflict persists past the bound (fail this one job rather
// than replay the same request forever — Q270); and provisionRetry on any other error, so
// the message redelivers. Replaying the same colliding name would 409 indefinitely and
// wedge the queue cursor, so a fresh name — not a bare replay — is the correct recovery.
func (l *Listener) generateJITConfig(ctx context.Context, ssID int, jobID string) (*scaleset.JITRunnerConfig, string, provisionOutcome) {
	for attempt := 0; ; attempt++ {
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
// once per job even if the message replays to a re-created session.
func (l *Listener) completeJob(cj scaleset.JobMessage) {
	l.mu.Lock()
	first := !l.completed[cj.JobID]
	l.completed[cj.JobID] = true
	l.mu.Unlock()
	if first && l.cfg.Metrics != nil {
		l.cfg.Metrics.IncJobCompleted(cj.Result)
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

// isRunnerNameConflict reports whether err is (or wraps) a scaleset.RunnerNameConflictError.
func isRunnerNameConflict(err error) bool {
	var rce *scaleset.RunnerNameConflictError
	return errors.As(err, &rce)
}
