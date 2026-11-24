package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Deferred retry-class re-runs (Q1244). A class re-run takes its slot when the class
// fires, as the disruption arms' do, and then waits in the failure ledger's pending map
// until GitHub has concluded the run, because either re-run call answers `403 This
// workflow is already running` until every job of the run has concluded, and a job's
// siblings can run far past the arms' 15-minute window. The pending sweep polls each
// waiting run on a widening cadence and releases it into handleScaleSetEviction, whose
// reservation resumes the slot already held, to re-run the killed jobs and no other
// where it can tell them apart (job_rerun.go). The operator-facing account is
// docs/design/04-operational-flows.md §4.2.
//
// The waiting set is the ledger, so a restart drops nothing; what a restart drops is
// which sets to sweep. The owning reconciler hands every set to AdoptPendingReruns once
// per process, which is how a new process finds the sets that already hold entries: the
// AGC runs no ConfigMap informer, and a ledger does not hold the Target its release
// needs.

const (
	// pendingRerunMaxWait bounds how long a parked re-run waits for its run to conclude.
	// GitHub lets a run go on far longer, but by then a person has seen the failure, and
	// the cap bounds the polling at about 175 reads an entry.
	pendingRerunMaxWait = 24 * time.Hour
	// pendingRerunExpiryGrace keeps an entry in the ledger past pendingRerunMaxWait, so
	// the sweep reports its expiry before a prune on another write drops it silently.
	pendingRerunExpiryGrace = time.Hour
	// pendingRerunFinalReadWindow is how long past pendingRerunMaxWait a failed run read
	// is retried, at the poll cadence, before the entry expires: about four reads, well
	// inside pendingRerunExpiryGrace.
	pendingRerunFinalReadWindow = pendingRerunExpiryGrace / 2
)

// pendingRerun is one parked re-run, keyed by failureKey.
type pendingRerun struct {
	ExpiresAt time.Time `json:"expiresAt"`
	FiredAt   time.Time `json:"firedAt"`
	Cause     string    `json:"cause"`
	// Repository is "owner/repo", the run's address with RunID.
	Repository string `json:"repository"`
	RunID      string `json:"runID"`
	Job        string `json:"job"`
	// Disruption is the worker pod whose reservation holds the slot, so the release
	// resumes it rather than reserving another.
	Disruption string `json:"disruption"`
	// FailedAttempt is the run attempt the job failed in. A run past it has already been
	// re-run, and the entry is dropped rather than released.
	FailedAttempt int `json:"failedAttempt"`
	// Workers is every worker of the attempt this re-run was parked for, up to
	// maxMergedPerAttempt. An entry an older AGC wrote holds none, and its release re-runs
	// every failed job.
	Workers []killedWorker `json:"workers,omitempty"`
	extra   map[string]json.RawMessage
}

// parkRerun records r in l, reporting whether it changed l. An attempt has one entry, which
// lists every worker killed in it (Q1327).
func parkRerun(l *rerunLedger, now time.Time, r pendingRerun) bool {
	for _, e := range l.Pending {
		if e != nil && r.FailedAttempt > 0 && e.RunID == r.RunID && e.FailedAttempt == r.FailedAttempt {
			return e.addWorkers(r.Workers)
		}
	}
	key := failureKey(r.RunID, r.Job)
	if l.Pending[key] != nil {
		return false
	}
	r.FiredAt = now
	r.ExpiresAt = now.Add(pendingRerunMaxWait + pendingRerunExpiryGrace)
	l.Pending[key] = &r
	return true
}

// addWorkers lists workers in r, up to maxMergedPerAttempt. A list that is not complete
// stays that way: an entry an older AGC wrote is left without one.
func (r *pendingRerun) addWorkers(workers []killedWorker) bool {
	changed := false
	for _, w := range workers {
		if len(r.Workers) == 0 || len(r.Workers) >= maxMergedPerAttempt ||
			slices.ContainsFunc(r.Workers, func(h killedWorker) bool { return h.Disruption == w.Disruption }) {
			continue
		}
		r.Workers = append(r.Workers, w)
		changed = true
	}
	return changed
}

// pendingRerunPollInterval is how long after its last read a run that has waited age
// is read again: often while it is likely to conclude soon, rarely once it has not.
func pendingRerunPollInterval(age time.Duration) time.Duration {
	switch {
	case age < 5*time.Minute:
		return 30 * time.Second
	case age < time.Hour:
		return 2 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// pendingSets is the sets the pending sweep reads, and its per-entry bookkeeping. Its
// zero value is ready to use.
type pendingSets struct {
	mu    sync.Mutex
	byKey map[types.NamespacedName]*pendingSet
	// lastRead is when each entry's run was last read, keyed by pendingEntryKey.
	lastRead map[string]time.Time
	// releasing is the runs a release is in flight for, by set and run ID: GitHub takes one
	// re-run of a run at a time.
	releasing map[string]bool
}

type pendingSet struct {
	target Target
	// gen counts registrations, so a sweep that read an empty ledger unregisters the set
	// only where nothing parked an entry since.
	gen int
}

func pendingEntryKey(set types.NamespacedName, key string) string {
	return set.String() + "|" + key
}

// watchPendingReruns adds target's set to the pending sweep.
func (p *Provisioner) watchPendingReruns(target Target) {
	s := &p.pendingSets
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey == nil {
		s.byKey = map[types.NamespacedName]*pendingSet{}
	}
	e := s.byKey[target.Key()]
	if e == nil {
		e = &pendingSet{}
		s.byKey[target.Key()] = e
	}
	e.target = target
	e.gen++
}

// AdoptPendingReruns adds target's set to the pending sweep where its ledger holds
// parked re-runs, once per process per set: entries this process parks are added as
// they are parked, so the question is only what a previous process left. The owning
// reconciler calls it for every scale-set RunnerSet. A returned error means the ledger
// could not be read; the claim is released so the next reconcile asks again.
func (p *Provisioner) AdoptPendingReruns(ctx context.Context, target Target) error {
	key := target.Key().String()
	if !p.pendingAdoptions.claim(key) {
		return nil
	}
	l, _, err := p.readRerunLedger(ctx, target)
	if err != nil {
		p.pendingAdoptions.release(key)
		return err
	}
	if len(l.Pending) > 0 {
		p.watchPendingReruns(target)
	}
	return nil
}

// snapshot copies the watched sets so the sweep holds no lock across a ledger read or a
// GitHub call.
func (s *pendingSets) snapshot() []pendingSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pendingSet, 0, len(s.byKey))
	for _, e := range s.byKey {
		out = append(out, *e)
	}
	return out
}

// unwatch removes set unless it was registered again since the sweep read gen.
func (s *pendingSets) unwatch(set types.NamespacedName, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.byKey[set]; e != nil && e.gen == gen {
		delete(s.byKey, set)
	}
}

// due reports whether entry's run should be read at now, and marks it read if so. A read
// is due at once where the last one was taken before readFrom.
func (s *pendingSets) due(entry, run string, age time.Duration, now, readFrom time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasing[run] {
		return false
	}
	if last, ok := s.lastRead[entry]; ok && !last.Before(readFrom) && now.Sub(last) < pendingRerunPollInterval(age) {
		return false
	}
	if s.lastRead == nil {
		s.lastRead = map[string]time.Time{}
	}
	s.lastRead[entry] = now
	return true
}

// claimRelease marks a release of run in flight, reporting whether this caller holds it.
func (s *pendingSets) claimRelease(run string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasing[run] {
		return false
	}
	if s.releasing == nil {
		s.releasing = map[string]bool{}
	}
	s.releasing[run] = true
	return true
}

// settle ends entry's turn and its claim on run. Its bookkeeping goes with the entry where
// dropped, and otherwise keeps lastRead, so the next attempt waits out the poll cadence.
func (s *pendingSets) settle(entry, run string, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.releasing, run)
	if dropped {
		delete(s.lastRead, entry)
	}
}

// sweepPendingReruns is one pass over every parked re-run of every watched set: expire
// the ones whose run is still running, or cannot be read, at pendingRerunMaxWait, drop
// the ones whose run has already been re-run past the attempt they failed in, release
// the ones whose run has concluded, and leave the rest for a later pass. It returns one
// done channel per release, closed once the release has finished and its entry is gone.
func (p *Provisioner) sweepPendingReruns(ctx context.Context) []<-chan struct{} {
	var released []<-chan struct{}
	for _, set := range p.pendingSets.snapshot() {
		setKey := set.target.Key()
		log := p.logForKey(setKey)
		l, _, err := p.readRerunLedger(ctx, set.target)
		if err != nil {
			log.Warn("could not read the failure ledger for parked re-runs; will retry", "error", err)
			continue
		}
		if len(l.Pending) == 0 {
			p.pendingSets.unwatch(setKey, set.gen)
			continue
		}
		for key, e := range l.Pending {
			if done, ok := p.sweepPendingRerun(ctx, set.target, key, *e); ok {
				released = append(released, done)
			}
		}
	}
	return released
}

// sweepPendingRerun takes one entry's turn of the sweep, returning a done channel where
// it released the entry.
func (p *Provisioner) sweepPendingRerun(ctx context.Context, target Target, key string, e pendingRerun) (<-chan struct{}, bool) {
	const tier = evictionTierScaleSet
	entry := pendingEntryKey(target.Key(), key)
	runKey := pendingEntryKey(target.Key(), e.RunID)
	log := p.logForKey(target.Key())
	now := p.nowFn()
	age := now.Sub(e.FiredAt)

	// Past pendingRerunMaxWait the entry's run is read once more at once, whatever the
	// cadence, so a run that concluded while no process swept, or since the last due read,
	// is still released. That read ends the entry unless it failed inside
	// pendingRerunFinalReadWindow, which keeps the wait bounded.
	deadline := e.FiredAt.Add(pendingRerunMaxWait)
	final := !now.Before(deadline)
	readFrom := time.Time{}
	if final {
		readFrom = deadline
	}
	if !p.pendingSets.due(entry, runKey, age, now, readFrom) {
		return nil, false
	}
	drop := func() {
		p.pendingSets.settle(entry, runKey, p.finishPendingRerun(ctx, target, key, e, nil, log))
	}
	expire := func(readErr error) {
		err := fmt.Errorf("the run did not conclude within %s of the job failing", pendingRerunMaxWait)
		msg := "the run never concluded while its re-run waited; manual rerun may be required"
		if readErr != nil {
			err = fmt.Errorf("the run was not seen to conclude within %s of the job failing: the last read of it failed: %w",
				pendingRerunMaxWait, readErr)
			msg = "the run could not be read when its re-run's wait ended; manual rerun may be required"
		}
		log.Error(msg, "runID", e.RunID, "job", e.Job, "cause", e.Cause, "waited", pendingRerunMaxWait, "error", err)
		p.recordRerunFailure(target, e.RunID, e.Cause, rerunFailureReasonNeverConcluded, err)
		drop()
	}
	retryRead := func(err error) {
		if final && !now.Before(deadline.Add(pendingRerunFinalReadWindow)) && p.pendingSets.claimRelease(runKey) {
			expire(err)
			return
		}
		log.Warn("could not read a parked re-run's run; will retry", "runID", e.RunID, "error", err)
	}

	owner, repo, _ := strings.Cut(e.Repository, "/")
	rctx, cancel := context.WithTimeout(ctx, evictionRecoveryAPIBudget)
	defer cancel()
	run, err := p.runConclusion(rctx, owner, repo, e.RunID)
	// Only a run that is gone ends the wait. Every other failure, a token that would not
	// mint, a rate limit, a 5xx, retries at the next poll: over a day of polling one bad
	// answer is expected, and the 24-hour expiry bounds a failure that never clears.
	switch {
	case errors.Is(err, errRunGone):
		if !p.pendingSets.claimRelease(runKey) {
			return nil, false
		}
		log.Error("a parked re-run's run is gone at GitHub; manual rerun may be required",
			"runID", e.RunID, "cause", e.Cause, "error", err)
		p.recordRerunFailure(target, e.RunID, e.Cause, rerunFailureReasonAPIError, err)
		drop()
		return nil, false
	case err != nil:
		retryRead(err)
		return nil, false
	}

	concluded, moved := run.status == runStatusCompleted, run.attempt > e.FailedAttempt
	var jobs []workflowJobSummary
	if moved || (concluded && len(e.Workers) > 0) {
		jobs, err = p.attemptJobs(rctx, owner, repo, e.RunID, run.attempt)
		switch {
		case errors.Is(err, errRunConclusionUnreadable):
			retryRead(err)
			return nil, false
		case err != nil:
			log.Warn("could not list the latest attempt's jobs for a parked re-run", "runID", e.RunID, "error", err)
			jobs = nil
		}
	}
	workers := e.workers()
	if moved {
		workers = notReRun(workers, jobs, run)
	}
	switch {
	case len(workers) == 0:
		if !p.pendingSets.claimRelease(runKey) {
			return nil, false
		}
		log.Info("the run was already re-run past the attempt this job failed in; the parked re-run is dropped",
			"runID", e.RunID, "runAttempt", e.FailedAttempt, "cause", e.Cause)
		drop()
		return nil, false
	case !concluded:
		if final && p.pendingSets.claimRelease(runKey) {
			expire(nil)
		}
		return nil, false
	}

	if !p.pendingSets.claimRelease(runKey) {
		return nil, false
	}
	scope := rerunScopeFailedJobs
	// A full entry may not list every worker killed, and a run that succeeded is the
	// futility checks' to withhold.
	if len(e.Workers) > 0 && len(e.Workers) < maxMergedPerAttempt && run.conclusion != runConclusionSuccess {
		if scope, workers = planJobRerun(workers, jobs); len(workers) == 0 {
			log.Info("no failed job of the run ran on the killed worker's runner; the parked re-run is dropped",
				"runID", e.RunID, "runAttempt", run.attempt, "job", e.Job, "cause", e.Cause)
			p.recordRerunWithheld(target, e.RunID, e.Cause, rerunWithheldReasonJobNotFound, fmt.Sprintf("worker pod for run %s was lost to %s, but no failed job of the run's attempt %d ran on that worker's runner, so there is nothing to re-run automatically; if a job was lost, find it in the run's other attempts or in another run and re-run it by hand",
				e.RunID, e.Cause, run.attempt))
			drop()
			return nil, false
		}
	}
	w := workers[0]
	log.Info("the run concluded; releasing its parked re-run", "runID", e.RunID, "job", w.Job, "runner", w.Runner,
		"cause", e.Cause, "scope", scope, "waited", age.Round(time.Second))
	var ran string
	var refused bool
	d := rerunDisruption{key: w.Disruption, job: w.Job, runner: w.Runner, failedAttempt: e.FailedAttempt,
		scope: scope, with: workers[1:], onRerun: func(scope string) { ran = scope }}
	if !final {
		d.onRefused = func() { refused = true }
	}
	// No retry delay: the run has concluded, which is the state a re-run needs.
	recovered := p.handleEviction(ctx, target, owner, repo, e.RunID, log, p.ownerMaxEvictionRetries(ctx, target), 0,
		tier, e.Cause, d)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-recovered
		// Somebody's re-run started first, another set's or a person's. The entry waits for
		// it as it waited for the run, with the slot it holds.
		if refused {
			p.pendingSets.settle(entry, runKey, false)
			return
		}
		// A single-job re-run leaves the other workers parked, for the attempt it started.
		var rest []killedWorker
		if ran == rerunScopeJob {
			rest = workers[1:]
		}
		// Dropped only once the release has finished, so a restart mid-release releases
		// it again and the reservation resumes rather than reserving a second slot.
		p.pendingSets.settle(entry, runKey, p.finishPendingRerun(context.WithoutCancel(ctx), target, key, e, rest, log))
	}()
	return done, true
}

// workers is every worker r was parked for that it names. An entry an older AGC wrote
// names one, and may have been parked for others.
func (r *pendingRerun) workers() []killedWorker {
	if len(r.Workers) > 0 {
		return r.Workers
	}
	return []killedWorker{{Job: r.Job, Disruption: r.Disruption}}
}

// notReRun returns the workers whose jobs run, an attempt past the one they failed in, did
// not re-run. One whose answer a later poll could change is kept; any other unknown is not,
// since a second re-run of one attempt is the worse outcome.
func notReRun(workers []killedWorker, jobs []workflowJobSummary, run runState) []killedWorker {
	return slices.DeleteFunc(slices.Clone(workers), func(w killedWorker) bool {
		verdict, err := jobRerunIn(jobs, run, rerunDisruption{job: w.Job, runner: w.Runner})
		return verdict != jobNotRerun && !errors.Is(err, errRunConclusionUnreadable)
	})
}

// finishPendingRerun leaves rest parked of the entry at key, e as its turn read it, or
// deletes the entry where rest is empty. It reports whether the entry is gone.
func (p *Provisioner) finishPendingRerun(ctx context.Context, target Target, key string, e pendingRerun, rest []killedWorker, log *slog.Logger) bool {
	gone := false
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, _ time.Time) bool {
		r := l.Pending[key]
		if gone = r == nil || r.Disruption != e.Disruption; gone {
			return false
		}
		if gone = len(rest) == 0; gone {
			delete(l.Pending, key)
			return true
		}
		// A worker killed since e was read keeps its place.
		left := slices.Clone(rest)
		for _, w := range r.Workers {
			if !slices.ContainsFunc(e.Workers, func(o killedWorker) bool { return o.Disruption == w.Disruption }) {
				left = append(left, w)
			}
		}
		r.Workers, r.Job, r.Disruption = left, left[0].Job, left[0].Disruption
		return true
	})
	if err != nil {
		log.Warn("could not remove a finished parked re-run from the failure ledger; a later sweep retries it",
			"entry", key, "error", err)
		return false
	}
	return gone
}
