package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// evictionEntry is the value stored in evictionCounts. count is the number of
// reruns already reserved for the run (capped at maxRetries — Q106); lastUpdate
// is the time of the most recent eviction of the run, refreshed on every
// eviction whether or not a retry slot was granted. The sweeper reclaims an
// entry once lastUpdate is older than the TTL, by which point the run can no
// longer produce an evicted pod (see sweepEvictionCounts).
type evictionEntry struct {
	count      int
	lastUpdate time.Time
}

// evictionLockShards is the number of mutexes in the sharded eviction lock.
// Eviction is a rare, node-pressure-driven event, so a small fixed pool keeps
// contention between distinct run_ids negligible without unbounded growth.
const evictionLockShards = 64

const (
	// defaultEvictionCounterTTL bounds how long a per-run eviction-retry counter
	// is retained after the run's last eviction. It is chosen well beyond a
	// realistic GitHub Actions run lifetime: an entry is reclaimed only once its
	// run can no longer produce an evicted pod, because reclaiming a live run's
	// counter would reset it to zero and refill the retry budget — the Q106 bug.
	// (Q141)
	defaultEvictionCounterTTL = 24 * time.Hour
	// defaultEvictionSweepInterval is how often the background sweeper scans
	// evictionCounts for entries older than the TTL.
	defaultEvictionSweepInterval = time.Hour

	// defaultEvictionRerunWindow bounds how long a disruption recovery keeps retrying
	// a re-run that GitHub refuses because the original run has not concluded.
	//
	// GitHub answers rerun-failed-jobs with `403 This workflow is already running`
	// until it concludes the run itself, and after an ungraceful kill (SIGKILLed
	// runner, nothing reported) that takes until the job lock's TTL lapses — measured
	// at 9m36s on live GitHub, designed as "at worst ~10 minutes from the last
	// renewal" (Q396/Q503). Fifteen minutes covers that bound with headroom; a run
	// still refusing past it is a finding, not a wait worth extending.
	defaultEvictionRerunWindow = 15 * time.Minute
	// defaultEvictionRerunRetryInterval paces the refused re-run attempts inside the
	// window. The wait is bounded and its length known (~10 minutes), so a fixed
	// interval is enough — backoff would only delay the recovery past the moment
	// GitHub starts accepting.
	defaultEvictionRerunRetryInterval = 30 * time.Second
)

// evictionRecoveryAPIBudget is the slack a recovery gets for its GitHub calls beyond
// the retry delay and the re-run window, bounding the detached context every recovery
// runs on. The bound is what keeps a wedged GitHub call from leaving a goroutine
// behind indefinitely.
const evictionRecoveryAPIBudget = 60 * time.Second

// errRunNotConcluded marks the rerun-failed-jobs refusal that means "not yet": GitHub
// answers 403 with "This workflow is already running" until it has concluded the
// original run, so the caller should retry rather than treat the attempt as spent
// (Q503).
var errRunNotConcluded = errors.New("the original run has not concluded")

// errRunCancelled marks a run GitHub concluded `cancelled`, which rerun-failed-jobs
// accepts (measured 2026-08-05, Q683). Recovery stands down rather than re-queueing a
// job a human stopped (Q811).
var errRunCancelled = errors.New("the run was cancelled at GitHub")

// errRerunCovered marks a scale-set recovery that found the run already re-run past the
// attempt its job failed in: GAG's re-run of that attempt landed, from this disruption's
// reservation or another's, or a human re-ran it. Re-running again would be a second
// re-run of one attempt (Q1182).
var errRerunCovered = errors.New("the run was already re-run past the attempt the job failed in")

// errRunConclusionUnreadable marks a conclusion check that took no verdict for a reason
// a later attempt could answer differently, so nothing is known yet about whether a
// re-run would undo a cancel. Retried like the still-running refusal, and terminal only
// when the re-run window closes on it: standing down costs a disrupted job its automatic
// recovery, and re-running anyway is the harm the check exists to prevent (Q811). A
// verdict that will not change inside the window is terminal at once instead, and is not
// marked with this — see runConclusion.
var errRunConclusionUnreadable = errors.New("the run's conclusion could not be read")

// Reason label values for the EvictionRerunFailures counter: the recovery's re-run
// window closed with GitHub still refusing, the window closed with the run's conclusion
// still unreadable, or the API answered with a terminal failure (anything other than
// the still-running refusal).
//
// budget_unrecorded is the scale-set tier's: the run's budget decision could not be
// written to the failure ledger, so the re-run was withheld rather than made without a
// slot (Q1182).
const (
	rerunFailureReasonNeverConcluded    = "run_never_concluded"
	rerunFailureReasonAPIError          = "api_error"
	rerunFailureReasonConclusionUnknown = "conclusion_unknown"
	rerunFailureReasonBudgetUnrecorded  = "budget_unrecorded"
)

// rerunWithheldReasonRunCancelled is the reason label on the EvictionRerunWithheld
// counter: the run concluded `cancelled`, so no re-run was requested.
const rerunWithheldReasonRunCancelled = "run_cancelled"

// runConclusionCancelled is the conclusion GitHub records for a cancelled run — the one
// value that stands a recovery down.
const runConclusionCancelled = "cancelled"

// runStatusCompleted is the run status at which conclusion is populated; before it,
// conclusion is null and says nothing.
const runStatusCompleted = "completed"

// handleEviction reserves a slot from the run's retry budget and, if one remains,
// waits out retryDelay and asks GitHub to re-run the run's failed jobs, retrying
// while GitHub refuses because the original run has not concluded (Q503). It is shared
// by both acquisition tiers: the classic path calls it from provision() once the
// worker pod it is watching is disrupted, and the scale-set path from the owning
// reconciler's RecoverEvictedScaleSetWorkers pass.
//
// On the classic tier the budget check, metrics, and Events run synchronously; the
// delay and the GitHub calls run on a goroutine whose completion the returned done channel signals, on a
// context detached from ctx and bounded by retryDelay + the re-run window + an API
// budget. Detached, because the wait outlives every caller: GitHub concludes an
// ungracefully killed run only when its job lock TTL lapses (~10 minutes, measured
// 9m36s — Q396), which is far past both a reconcile and the classic job goroutine's
// obligation to report its result. Callers may block on the channel (tests), select
// with a timeout, or ignore it.
//
// tier labels the metrics and the operator-facing wording, and selects the budget. The
// classic tier's is the in-process evictionCounts, keyed by run_id; the scale-set
// tier's is the owner's failure ledger (handleScaleSetEviction, Q1182), which survives
// a restart. Within a tier the Q106 cap of maxRetries re-runs per run holds across every
// disruption cause together, so a run alternately evicted and preempted cannot spend
// two budgets. cause labels the same surfaces and, on the classic tier, selects the
// conclusion check (attemptRerun, Q811), which only recoveryCauseDeletion takes there. d names the
// disruption to the scale-set ledger and is ignored on the classic tier.
func (p *Provisioner) handleEviction(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, maxRetries int, retryDelay time.Duration, tier, cause string, d rerunDisruption) <-chan struct{} {
	if runID == "0" || runID == "" {
		log.Warn("worker pod disrupted but run_id unknown; skipping auto-retry", "cause", cause)
		return closedChan()
	}
	if tier == evictionTierScaleSet {
		return p.handleScaleSetEviction(ctx, target, owner, repo, runID, log, maxRetries, retryDelay, cause, d)
	}

	// Reserve a retry slot atomically. This guards against the read-modify-write
	// race where two concurrent evictions of the same run both read the same
	// count, both pass the budget check, and both fire a rerun — exceeding
	// maxRetries (Q106). At most maxRetries evictions ever pass the gate, so the
	// budget bounds re-run recoveries (not HTTP calls: one recovery may retry a
	// refused re-run several times before it lands) at maxRetries per run.
	attempt, ok := p.reserveEvictionRetry(runID, maxRetries)
	if !ok {
		p.recordRetriesExhausted(target, runID, log, maxRetries, tier, cause)
		return closedChan()
	}
	p.recordRetryStarted(target, runID, log, attempt, tier, cause)

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		retryDelay+p.rerunWindow()+evictionRecoveryAPIBudget)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()

		// Brief delay before calling GitHub so any in-flight state settles.
		select {
		case <-rctx.Done():
			return
		case <-time.After(retryDelay):
		}
		p.rerunUntilAccepted(rctx, target, owner, repo, runID, log, attempt, tier, cause, 0)
	}()
	return done
}

// handleScaleSetEviction is handleEviction's scale-set half, whose budget is the
// owner's failure ledger rather than process memory (Q1182). The decision takes the
// attempt the disrupted job failed in, so everything after the runID check runs on the
// detached goroutine: reading the run is a GitHub call, and a reconcile must not wait
// on one.
//
// The run is read before the retry delay. For a disruption detected live, the attempt
// it reports is the one the job ran in: GitHub cannot conclude the run, and so cannot
// accept another disruption's re-run, until this job has concluded too. One detected
// late — the vanished arm, or a scan after a restart — can read an attempt another
// re-run already started, and then reserves against that one; the design's already_rerun
// check, not built, is what would close it. A disruption that already knows its attempt
// (the abandoned arm's) still takes the read, for the futility checks.
//
// The futility checks (futileRerun, Q1181) run on that read, before the reservation, so
// a re-run they withhold spends no slot. The reservation then resumes, merges, refuses or reserves (reserveScaleSetRerun).
// Every outcome but a refusal goes on to the re-run loop, which reads the run before
// each call and stands down once its attempt has moved past the one the job failed in
// (errRerunCovered). That is what makes a merge safe: a merged disruption re-runs only
// if the reservation it joined never landed, because its owner's loop failed or its
// process restarted, and when that re-run does land, every other disruption of the
// attempt sees the attempt move and stops. A run that cannot be read, or a decision
// that cannot be written, withholds the re-run: never a re-run without a recorded slot.
func (p *Provisioner) handleScaleSetEviction(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, maxRetries int, retryDelay time.Duration, cause string, d rerunDisruption) <-chan struct{} {
	const tier = evictionTierScaleSet
	if maxRetries <= 0 {
		p.recordRetriesExhausted(target, runID, log, maxRetries, tier, cause)
		return closedChan()
	}

	// Two windows: one for the run read, which retries an unreadable answer like the
	// re-run loop does, and one for the loop itself.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		retryDelay+2*p.rerunWindow()+evictionRecoveryAPIBudget)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()

		if !p.canReadRun(owner, repo) {
			// Nothing can reach GitHub, so no re-run could be made either; the
			// same no-op rerunFailedJobs takes.
			log.Warn("run cannot be addressed; cannot reserve its re-run budget or trigger a rerun",
				"runID", runID, "cause", cause)
			return
		}
		run, err := p.readRerunCandidate(rctx, owner, repo, runID)
		if err != nil {
			reason := rerunFailureReasonAPIError
			if errors.Is(err, errRunConclusionUnreadable) {
				reason = rerunFailureReasonConclusionUnknown
			}
			log.Error("could not read the disrupted run, so its re-run budget cannot be reserved; manual rerun may be required",
				"runID", runID, "cause", cause, "reason", reason, "error", err)
			p.recordRerunFailure(target, runID, tier, cause, reason, err)
			return
		}
		if d.failedAttempt == 0 {
			d.failedAttempt = run.attempt
		}
		// A futile re-run is withheld here, before the reservation, so it spends no slot.
		if p.futileRerun(rctx, target, owner, repo, runID, tier, cause, run, log) {
			return
		}

		outcome, number, err := p.reserveScaleSetRerun(rctx, target, runID, d, cause, maxRetries)
		switch {
		case err != nil:
			log.Error("could not record the run's re-run budget decision; the re-run is withheld, manual rerun may be required",
				"runID", runID, "cause", cause, "error", err)
			p.recordRerunFailure(target, runID, tier, cause, rerunFailureReasonBudgetUnrecorded, err)
			return
		case outcome == rerunRefused:
			p.recordRetriesExhausted(target, runID, log, maxRetries, tier, cause)
			return
		case outcome == rerunMerged:
			log.Info("a re-run is already reserved for this run attempt; this disruption spends no slot and re-runs only if that one never lands",
				"runID", runID, "runAttempt", d.failedAttempt, "cause", cause)
		case outcome == rerunResumed:
			log.Info("worker pod disrupted; resuming the re-run this disruption already reserved",
				"runID", runID, "attempt", number, "cause", cause)
		default:
			p.recordRetryStarted(target, runID, log, number, tier, cause)
		}

		select {
		case <-rctx.Done():
			return
		case <-time.After(retryDelay):
		}
		p.rerunUntilAccepted(rctx, target, owner, repo, runID, log, number, tier, cause, d.failedAttempt)
	}()
	return done
}

// readRerunCandidate reads the run, retrying an unreadable answer on the re-run loop's
// interval until its window closes. A 4xx is terminal at once, as it is in the loop.
func (p *Provisioner) readRerunCandidate(ctx context.Context, owner, repo, runID string) (runState, error) {
	windowC, stopWindow := p.startRerunWindow()
	defer stopWindow()
	for {
		run, err := p.runConclusion(ctx, owner, repo, runID)
		if err == nil && run.attempt == 0 {
			// Every disruption of every attempt would merge into one key, so a run
			// that names no attempt cannot be budgeted; it will not name one later.
			return runState{}, errors.New("the run carried no run_attempt")
		}
		if err == nil {
			return run, nil
		}
		if !errors.Is(err, errRunConclusionUnreadable) {
			return runState{}, err
		}
		select {
		case <-ctx.Done():
			return runState{}, fmt.Errorf("%v: %w", ctx.Err(), err)
		case <-windowC:
			return runState{}, fmt.Errorf("the run stayed unreadable for %s: %w", p.rerunWindow(), err)
		case <-p.rerunRetryTick():
		}
	}
}

// recordRetriesExhausted surfaces a disruption refused for an exhausted budget.
func (p *Provisioner) recordRetriesExhausted(target Target, runID string, log *slog.Logger, maxRetries int, tier, cause string) {
	key := target.Key()
	log.Warn("disruption retry budget exhausted; manual rerun required",
		"runID", runID, "maxRetries", maxRetries, "cause", cause)
	if p.Metrics != nil {
		p.Metrics.EvictionRetriesExhausted.WithLabelValues(key.Namespace, key.Name, tier, cause).Inc()
	}
	target.RecordEvent(corev1.EventTypeWarning, "EvictionRetriesExhausted", "RetryEvictedJob",
		fmt.Sprintf("worker pod for run %s was lost to %s and the auto-retry budget (%d) is exhausted; a manual re-run is required", runID, cause, maxRetries))
}

// recordRetryStarted counts a reserved slot, before any re-run call is made.
func (p *Provisioner) recordRetryStarted(target Target, runID string, log *slog.Logger, attempt int, tier, cause string) {
	key := target.Key()
	log.Info("worker pod disrupted; scheduling auto-retry",
		"runID", runID, "attempt", attempt, "tier", tier, "cause", cause)
	if p.Metrics != nil {
		p.Metrics.EvictionRetries.WithLabelValues(key.Namespace, key.Name, tier, cause).Inc()
	}
}

// rerunUntilAccepted calls rerun-failed-jobs for the run until GitHub accepts it,
// treating the 403 "This workflow is already running" refusal as "not yet" and
// retrying on a fixed interval within the re-run window (Q503). Any other failure is
// terminal: the reserved budget slot stays spent either way (the Q106 hard cap counts
// recoveries, and re-reserving on failure would let one disruption burn the whole
// budget), so a re-run that never lands is surfaced to the operator via the
// EvictionRerunFailures counter and an owner Event rather than retried further.
//
// failedAttempt, where non-zero, is the run attempt the job failed in, and a pass that
// finds the run already past it stands down (errRerunCovered, the scale-set tier only).
func (p *Provisioner) rerunUntilAccepted(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, attempt int, tier, cause string, failedAttempt int) {
	windowC, stopWindow := p.startRerunWindow()
	defer stopWindow()

	for call := 1; ; call++ {
		err := p.attemptRerun(ctx, target, owner, repo, runID, log, tier, cause, failedAttempt)
		switch {
		case err == nil:
			log.Info("disruption auto-retry triggered",
				"runID", runID, "attempt", attempt, "cause", cause, "rerunCalls", call)
			return
		case errors.Is(err, errRerunCovered):
			log.Info("the run was already re-run past the attempt this job failed in; no further re-run",
				"runID", runID, "runAttempt", failedAttempt, "cause", cause, "rerunCalls", call)
			return
		case errors.Is(err, errRunCancelled):
			log.Info("the run was cancelled at GitHub, so the disruption auto-retry stands down rather than undoing the cancel",
				"runID", runID, "attempt", attempt, "cause", cause, "rerunCalls", call)
			p.recordRerunWithheld(target, runID, tier, cause, rerunWithheldReasonRunCancelled, runCancelledMessage(runID, cause))
			return
		case !errors.Is(err, errRunNotConcluded) && !errors.Is(err, errRunConclusionUnreadable):
			log.Error("disruption auto-retry failed; manual rerun may be required",
				"runID", runID, "cause", cause, "error", err)
			p.recordRerunFailure(target, runID, tier, cause, rerunFailureReasonAPIError, err)
			return
		}

		// Either the re-run was refused because the original run is still winding down
		// (expected after an ungraceful kill), or the conclusion check took no verdict.
		// Loud once so an operator tailing the log sees the wait is deliberate, quiet on
		// the repeats.
		refusedLog := log.Debug
		if call == 1 {
			refusedLog = log.Info
		}
		refusedLog("no re-run made this pass; will keep retrying",
			"runID", runID, "cause", cause, "reason", err, "retryInterval", p.rerunRetryInterval())

		select {
		case <-ctx.Done():
			log.Warn("disruption auto-retry abandoned before the original run concluded",
				"runID", runID, "cause", cause, "error", ctx.Err())
			return
		case <-windowC:
			reason := rerunFailureReasonNeverConcluded
			if errors.Is(err, errRunConclusionUnreadable) {
				reason = rerunFailureReasonConclusionUnknown
			}
			err := fmt.Errorf("no re-run landed within %s: %w", p.rerunWindow(), err)
			log.Error("disruption auto-retry failed within the re-run window; manual rerun may be required",
				"runID", runID, "cause", cause, "reason", reason, "window", p.rerunWindow(), "rerunCalls", call)
			p.recordRerunFailure(target, runID, tier, cause, reason, err)
			return
		case <-p.rerunRetryTick():
		}
	}
}

// attemptRerun is one pass of the loop above: the conclusion check the cause calls for,
// then the re-run call itself.
//
// On the classic tier the check is the Q811 arm, and it runs only for the
// graceful-deletion cause, because that is the only one an operator produces
// deliberately. The cancel runbook's remedy
// for a worker that will not stop is to delete its pod, which hand-supplies the deletion
// mark the arm keys on, and rerun-failed-jobs then re-queues the job the cancel stopped:
// a `cancelled` conclusion accepts the call where a `success` conclusion refuses it
// (measured 2026-08-05, Q683). An eviction, a preemption and a vanished worker are
// signals only the cluster writes, so gating them would cost every recovery a GitHub
// call to discriminate a case no operator action produces.
//
// It runs before every attempt rather than once, because at detection the run has not
// concluded yet and its conclusion is null. GitHub concludes it minutes later, which is
// the same wait the refusal loop already exists to sit through.
//
// On the scale-set tier a cancel GAG produced itself is not a user's: the abandoned arm
// records the attempt it force-cancels in the failure ledger before cancelling, and a
// cancelled conclusion on that attempt re-runs. Without it, a drained worker of a run
// whose other job was abandoned would read GAG's own cancel as a human's and stand down,
// and the abandoned arm's re-run would merge into that withheld one (Q1182).
//
// The scale-set tier reads the run on every pass, whatever the cause, for the coverage
// check: a run already past failedAttempt has been re-run for this job (errRerunCovered).
// It takes the cancel check on every cause too, because selfCancelledAttempt tells GAG's
// own cancel from anyone else's, so a user's or a concurrency group's cancel is honoured
// on every arm without an exemption for the abandoned one (Q1181).
func (p *Provisioner) attemptRerun(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, tier, cause string, failedAttempt int) error {
	covering := failedAttempt > 0
	checksCancel := cause == recoveryCauseDeletion || tier == evictionTierScaleSet
	if (covering || checksCancel) && p.canReadRun(owner, repo) {
		run, err := p.runConclusion(ctx, owner, repo, runID)
		if err != nil {
			return fmt.Errorf("read the run's conclusion before re-running it: %w", err)
		}
		if covering && run.attempt > failedAttempt {
			return errRerunCovered
		}
		if checksCancel && run.status == runStatusCompleted && run.conclusion == runConclusionCancelled {
			if tier != evictionTierScaleSet || run.attempt == 0 ||
				p.selfCancelledAttempt(ctx, target, runID) != run.attempt {
				return errRunCancelled
			}
			log.Info("the run was cancelled by GAG's own force-cancel of this attempt, not by a user; re-running it",
				"runID", runID, "runAttempt", run.attempt)
		}
	}
	return p.rerunFailedJobs(ctx, owner, repo, runID, log)
}

// canReadRun reports whether a run-scoped GET can be made at all. Where it cannot, the
// re-run POST cannot be made either (rerunFailedJobs holds the same guards), so skipping
// the check cannot let a cancel be undone: nothing reaches GitHub on either call.
func (p *Provisioner) canReadRun(owner, repo string) bool {
	return owner != "" && repo != "" && p.TokenFunc != nil && p.GitHubAPIURL != ""
}

// recordRerunWithheld surfaces a recovery that deliberately did not ask for a re-run.
// The Event is Normal, not Warning: the withhold is the correct outcome, but without it
// the story reads as a recovery that silently did nothing (Q811).
//
// Its reasons: run_cancelled from attemptRerun's conclusion check (every cause on the
// scale-set tier, deletion only on the classic), own_limit from withholdOwnLimitRerun
// (eviction only, Q1177), and the scale-set tier's pre-reservation futility checks
// (futileRerun, Q1181).
func (p *Provisioner) recordRerunWithheld(target Target, runID, tier, cause, reason, message string) {
	key := target.Key()
	if p.Metrics != nil && p.Metrics.EvictionRerunWithheld != nil {
		p.Metrics.EvictionRerunWithheld.WithLabelValues(key.Namespace, key.Name, tier, cause, reason).Inc()
	}
	target.RecordEvent(corev1.EventTypeNormal, "EvictionRerunWithheld", "RetryEvictedJob", message)
}

// recordRerunFailure surfaces a recovery whose re-run never landed: the retry budget
// was spent and eviction_retries_total incremented, but the job was not re-run, so
// without this the operator-visible story reads as a recovery that happened (Q503).
func (p *Provisioner) recordRerunFailure(target Target, runID, tier, cause, reason string, err error) {
	key := target.Key()
	if p.Metrics != nil && p.Metrics.EvictionRerunFailures != nil {
		p.Metrics.EvictionRerunFailures.WithLabelValues(key.Namespace, key.Name, tier, cause, reason).Inc()
	}
	target.RecordEvent(corev1.EventTypeWarning, "EvictionRerunFailed", "RetryEvictedJob",
		fmt.Sprintf("the automatic re-run for run %s (lost to %s) was never accepted by GitHub (%v); a manual re-run is required", runID, cause, err))
}

// rerunWindow returns the re-run retry window, honouring the EvictionRerunWindow
// override (zero means the default).
func (p *Provisioner) rerunWindow() time.Duration {
	if p.EvictionRerunWindow > 0 {
		return p.EvictionRerunWindow
	}
	return defaultEvictionRerunWindow
}

// rerunRetryInterval returns the pacing between refused re-run attempts, honouring
// the EvictionRerunRetryInterval override (zero means the default).
func (p *Provisioner) rerunRetryInterval() time.Duration {
	if p.EvictionRerunRetryInterval > 0 {
		return p.EvictionRerunRetryInterval
	}
	return defaultEvictionRerunRetryInterval
}

// startRerunWindow starts the timer that ends the re-run loop, returning its channel
// and stop func. Tests override rerunWindowC to close the window on a refusal count
// instead of on wall clock (Q1089).
func (p *Provisioner) startRerunWindow() (<-chan time.Time, func()) {
	if p.rerunWindowC != nil {
		return p.rerunWindowC(p.rerunWindow())
	}
	t := time.NewTimer(p.rerunWindow())
	return t.C, func() { t.Stop() }
}

// rerunRetryTick is one wait between refused re-run attempts.
func (p *Provisioner) rerunRetryTick() <-chan time.Time {
	if p.rerunRetryC != nil {
		return p.rerunRetryC(p.rerunRetryInterval())
	}
	return time.After(p.rerunRetryInterval())
}

// reserveEvictionRetry atomically checks the per-run eviction-retry budget and,
// if a slot remains, increments the count and returns the 1-based attempt
// number. It returns ok=false once the budget is exhausted. Serializing the
// check-and-increment per run_id — under the sharded evictionLocks — is what
// guarantees N concurrent evictions of the same run trigger at most maxRetries
// reruns (Q106). The lock is held only across the counter update, never across
// the retry delay or the GitHub API call.
func (p *Provisioner) reserveEvictionRetry(runID string, maxRetries int) (attempt int, ok bool) {
	mu := &p.evictionLocks[evictionShard(runID)]
	mu.Lock()
	defer mu.Unlock()

	now := p.nowFn()
	var count int
	if v, loaded := p.evictionCounts.Load(runID); loaded {
		count = v.(evictionEntry).count
	}
	if count >= maxRetries {
		// Budget is a hard lifetime cap: leave the count pinned so every later
		// eviction of this run is a no-op. We deliberately do NOT delete the
		// entry here — deleting reset the count to zero and let the budget refill
		// on the next eviction, which both defeats the cap and (combined with the
		// concurrent read-modify-write) is the Q106 over-budget bug. We DO refresh
		// lastUpdate: an exhausted but still-evicting run is provably live, so its
		// entry must not be a sweep candidate yet (Q141).
		p.evictionCounts.Store(runID, evictionEntry{count: count, lastUpdate: now})
		return 0, false
	}
	p.evictionCounts.Store(runID, evictionEntry{count: count + 1, lastUpdate: now})
	return count + 1, true
}

// nowFn returns the current time, honouring the test-injected p.now override.
func (p *Provisioner) nowFn() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// sweepEvictionCounts deletes per-run eviction-retry counters whose last
// eviction was more than ttl ago, returning the number of entries reclaimed.
//
// Correctness rests on a single fact: an evicted worker pod only ever exists for
// a live run, so a run that has produced no eviction for ttl can no longer
// produce one. With ttl chosen well beyond a realistic run lifetime, an entry is
// reclaimed only after its run is provably dead — so the LoadOrStore that a later
// eviction would do (refilling the budget to zero) can never happen for a run we
// swept. That preserves the Q106 invariant (at most maxRetries reruns per live
// run) while bounding evictionCounts to run_ids evicted within the trailing ttl
// window. The per-entry shard lock is taken (and lastUpdate re-checked under it)
// so a concurrent reserveEvictionRetry that just refreshed the entry is never
// raced away.
func (p *Provisioner) sweepEvictionCounts(ttl time.Duration) int {
	now := p.nowFn()
	var swept int
	p.evictionCounts.Range(func(key, value any) bool {
		if now.Sub(value.(evictionEntry).lastUpdate) <= ttl {
			return true
		}
		runID := key.(string)
		mu := &p.evictionLocks[evictionShard(runID)]
		mu.Lock()
		if v, loaded := p.evictionCounts.Load(runID); loaded &&
			now.Sub(v.(evictionEntry).lastUpdate) > ttl {
			p.evictionCounts.Delete(runID)
			swept++
		}
		mu.Unlock()
		return true
	})
	return swept
}

// EvictionSweeper periodically reclaims expired entries from a Provisioner's
// eviction-retry counter map. It implements sigs.k8s.io/controller-runtime/pkg/
// manager.Runnable; wire it with mgr.Add. Each AGC replica manages its own
// counters, so it runs on every replica (NeedLeaderElection is false).
type EvictionSweeper struct {
	p        *Provisioner
	interval time.Duration
	ttl      time.Duration
}

// NewEvictionSweeper returns an EvictionSweeper for p using the default sweep
// interval and counter TTL.
func NewEvictionSweeper(p *Provisioner) *EvictionSweeper {
	return &EvictionSweeper{
		p:        p,
		interval: defaultEvictionSweepInterval,
		ttl:      defaultEvictionCounterTTL,
	}
}

// Start runs the sweep loop until ctx is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *EvictionSweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log := s.p.logFor()
	log.Info("eviction-counter sweeper started", "interval", s.interval, "ttl", s.ttl)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n := s.p.sweepEvictionCounts(s.ttl); n > 0 {
				log.Info("reclaimed expired eviction-retry counters", "count", n)
			}
		}
	}
}

// NeedLeaderElection reports that the sweeper runs on every replica, not only
// the leader: each AGC instance owns the eviction counters for the pods it
// provisioned.
func (s *EvictionSweeper) NeedLeaderElection() bool { return false }

// evictionShard maps a run_id to one of evictionLockShards mutex indices.
func evictionShard(runID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(runID))
	return h.Sum32() % evictionLockShards
}

// runState is what runConclusion decodes from a run. conclusion is empty until status
// is `completed`, so the two are read together; attempt is the run's run_attempt, 1 for
// a run never re-run and 0 where GitHub sent none. The rest feed the futility checks
// (rerun_futility.go), and each is zero where GitHub sent none, which skips its check.
type runState struct {
	status, conclusion string
	attempt            int
	id, workflowID     int64
	createdAt          time.Time
	event              string
	headBranch         string
	headSHA            string
	// headRepo is head_repository.full_name: a fork's for a pull request from one.
	headRepo string
}

// runConclusion calls GET /repos/{owner}/{repo}/actions/runs/{run_id} and returns the
// run's status, conclusion and attempt. The guards mirror rerunFailedJobs,
// except that a run this cannot address is an error rather than a warning: the caller
// asks in order to decide, and a silent empty answer would read as "not cancelled".
//
// Failures are split the way the re-run call's are, and for the same reason: only the
// ones a later attempt could answer differently — the request never completing, and a
// 5xx — are wrapped errRunConclusionUnreadable for the loop to retry. A 4xx and a body
// that will not decode are terminal, because neither changes within the re-run window: a
// 2xx carrying something other than a run is an endpoint that is not the API (a proxy's
// error page, a misconfigured GHES), and re-asking it thirty times only delays the Event
// that tells an operator so.
func (p *Provisioner) runConclusion(ctx context.Context, owner, repo, runID string) (runState, error) {
	status, body, err := p.githubGET(ctx, owner, repo, "actions/runs/"+neturl.PathEscape(runID), nil)
	switch {
	case errors.Is(err, errGitHubUnreachable):
		return runState{}, fmt.Errorf("run-status API call: %v: %w", err, errRunConclusionUnreadable)
	case err != nil:
		return runState{}, err
	case status >= 500:
		return runState{}, fmt.Errorf("run-status API returned %d: %s: %w",
			status, strings.TrimSpace(string(body)), errRunConclusionUnreadable)
	case status < 200 || status > 299:
		return runState{}, fmt.Errorf("run-status API returned %d: %s", status, strings.TrimSpace(string(body)))
	}

	var run struct {
		ID             int64     `json:"id"`
		Status         string    `json:"status"`
		Conclusion     string    `json:"conclusion"`
		RunAttempt     int       `json:"run_attempt"`
		WorkflowID     int64     `json:"workflow_id"`
		CreatedAt      time.Time `json:"created_at"`
		Event          string    `json:"event"`
		HeadBranch     string    `json:"head_branch"`
		HeadSHA        string    `json:"head_sha"`
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	if err := json.Unmarshal(body, &run); err != nil {
		return runState{}, fmt.Errorf("decode run status: %w", err)
	}
	return runState{
		status: run.Status, conclusion: run.Conclusion, attempt: run.RunAttempt,
		id: run.ID, workflowID: run.WorkflowID, createdAt: run.CreatedAt, event: run.Event,
		headBranch: run.HeadBranch, headSHA: run.HeadSHA, headRepo: run.HeadRepository.FullName,
	}, nil
}

// errGitHubUnreachable marks a githubGET whose request never completed, which a later
// attempt could answer differently.
var errGitHubUnreachable = errors.New("the GitHub API could not be reached")

// githubGET calls GET {GitHubAPIURL}/repos/{owner}/{repo}/{path}?{query} and returns the
// status and up to 1 MiB of the body. The guards mirror rerunFailedJobs; a request that
// never completed is wrapped errGitHubUnreachable.
func (p *Provisioner) githubGET(ctx context.Context, owner, repo, path string, query neturl.Values) (int, []byte, error) {
	if !repoSegmentRE.MatchString(owner) || !repoSegmentRE.MatchString(repo) {
		return 0, nil, fmt.Errorf("invalid owner/repo characters: %q/%q", owner, repo)
	}
	if p.TokenFunc == nil {
		return 0, nil, fmt.Errorf("TokenFunc is not configured")
	}
	token, err := p.TokenFunc(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("get installation token: %w", err)
	}
	// No silent default, for the same reason as the rerun call (Q504).
	if p.GitHubAPIURL == "" {
		return 0, nil, fmt.Errorf("GitHubAPIURL is not configured; refusing to guess an endpoint for the %s call", path)
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/%s",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), path)
	if len(query) > 0 {
		apiURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	hc := p.HTTPClient
	if hc == nil {
		hc = defaultProvisionerClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%v: %w", err, errGitHubUnreachable)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// rerunFailedJobs calls POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs.
func (p *Provisioner) rerunFailedJobs(ctx context.Context, owner, repo, runID string, log *slog.Logger) error {
	if owner == "" || repo == "" {
		log.Warn("owner/repo unknown; cannot trigger rerun", "runID", runID)
		return nil
	}
	if !repoSegmentRE.MatchString(owner) || !repoSegmentRE.MatchString(repo) {
		return fmt.Errorf("invalid owner/repo characters: %q/%q", owner, repo)
	}
	if p.TokenFunc == nil {
		log.Warn("TokenFunc not configured; cannot trigger rerun", "runID", runID)
		return nil
	}

	token, err := p.TokenFunc(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}

	// No silent default: refusing to guess an endpoint keeps the installation
	// token from being posted to a host that never issued it (see GitHubAPIURL, Q504).
	if p.GitHubAPIURL == "" {
		return fmt.Errorf("GitHubAPIURL is not configured; refusing to guess an endpoint for the rerun call")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/rerun-failed-jobs",
		p.GitHubAPIURL,
		neturl.PathEscape(owner),
		neturl.PathEscape(repo),
		neturl.PathEscape(runID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("build rerun request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	hc := p.HTTPClient
	if hc == nil {
		hc = defaultProvisionerClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("rerun API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	// GitHub returns 201 Created on success.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// The one refusal that means "again later" rather than "failed": until GitHub
		// concludes the original run it answers 403 with "This workflow is already
		// running", and after an ungraceful kill that conclusion is minutes away
		// (Q503). Discriminated by the message, not the status code alone — a 403 is
		// also what a permissions problem returns, and retrying that would be noise.
		if resp.StatusCode == http.StatusForbidden &&
			bytes.Contains(bytes.ToLower(body), []byte("already running")) {
			return fmt.Errorf("rerun API returned %d: %s: %w",
				resp.StatusCode, strings.TrimSpace(string(body)), errRunNotConcluded)
		}
		return fmt.Errorf("rerun API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
