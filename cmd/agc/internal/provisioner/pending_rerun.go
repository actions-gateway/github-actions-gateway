package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Deferred retry-class re-runs (Q1244). A class re-run takes its slot when the class
// fires, as the disruption arms' do, and then waits in the failure ledger's pending map
// until GitHub has concluded the run, because rerun-failed-jobs answers `403 This
// workflow is already running` until every job of the run has concluded, and a job's
// siblings can run far past the arms' 15-minute window. The pending sweep polls each
// waiting run on a widening cadence and releases it into handleScaleSetEviction, whose
// reservation resumes the slot already held. The operator-facing account is
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
	extra         map[string]json.RawMessage
}

// parkRerun records r in l, reporting whether it was absent.
func parkRerun(l *rerunLedger, now time.Time, r pendingRerun) bool {
	key := failureKey(r.RunID, r.Job)
	if l.Pending[key] != nil {
		return false
	}
	r.FiredAt = now
	r.ExpiresAt = now.Add(pendingRerunMaxWait + pendingRerunExpiryGrace)
	l.Pending[key] = &r
	return true
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
	// lastRead is when each entry's run was last read, and releasing the entries a
	// release is in flight for, both keyed by pendingEntryKey.
	lastRead  map[string]time.Time
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

// due reports whether entry's run should be read at now, and marks it read if so.
func (s *pendingSets) due(entry string, age time.Duration, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasing[entry] {
		return false
	}
	if last, ok := s.lastRead[entry]; ok && now.Sub(last) < pendingRerunPollInterval(age) {
		return false
	}
	if s.lastRead == nil {
		s.lastRead = map[string]time.Time{}
	}
	s.lastRead[entry] = now
	return true
}

// claimRelease marks entry's release in flight, reporting whether this caller holds it.
func (s *pendingSets) claimRelease(entry string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasing[entry] {
		return false
	}
	if s.releasing == nil {
		s.releasing = map[string]bool{}
	}
	s.releasing[entry] = true
	return true
}

// forget drops entry's bookkeeping.
func (s *pendingSets) forget(entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastRead, entry)
	delete(s.releasing, entry)
}

// sweepPendingReruns is one pass over every parked re-run of every watched set: expire
// the ones that have waited pendingRerunMaxWait, drop the ones whose run has already
// been re-run past the attempt they failed in, release the ones whose run has concluded,
// and leave the rest for a later pass. It returns one done channel per release, closed
// once the release has finished and its entry is gone.
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
	log := p.logForKey(target.Key())
	now := p.nowFn()
	age := now.Sub(e.FiredAt)

	if age >= pendingRerunMaxWait {
		if !p.pendingSets.claimRelease(entry) {
			return nil, false
		}
		defer p.pendingSets.forget(entry)
		log.Error("the run never concluded while its re-run waited; manual rerun may be required",
			"runID", e.RunID, "job", e.Job, "cause", e.Cause, "waited", pendingRerunMaxWait)
		p.recordRerunFailure(target, e.RunID, tier, e.Cause, rerunFailureReasonNeverConcluded,
			fmt.Errorf("the run did not conclude within %s of the job failing", pendingRerunMaxWait))
		p.dropPendingRerun(ctx, target, key, e.Disruption, log)
		return nil, false
	}
	if !p.pendingSets.due(entry, age, now) {
		return nil, false
	}

	owner, repo, _ := strings.Cut(e.Repository, "/")
	rctx, cancel := context.WithTimeout(ctx, evictionRecoveryAPIBudget)
	run, err := p.runConclusion(rctx, owner, repo, e.RunID)
	cancel()
	switch {
	case errors.Is(err, errRunConclusionUnreadable):
		log.Debug("could not read a parked re-run's run; will retry", "runID", e.RunID, "error", err)
		return nil, false
	case err != nil:
		if !p.pendingSets.claimRelease(entry) {
			return nil, false
		}
		defer p.pendingSets.forget(entry)
		log.Error("could not read a parked re-run's run; manual rerun may be required",
			"runID", e.RunID, "cause", e.Cause, "error", err)
		p.recordRerunFailure(target, e.RunID, tier, e.Cause, rerunFailureReasonAPIError, err)
		p.dropPendingRerun(ctx, target, key, e.Disruption, log)
		return nil, false
	case run.attempt > e.FailedAttempt:
		if !p.pendingSets.claimRelease(entry) {
			return nil, false
		}
		defer p.pendingSets.forget(entry)
		log.Info("the run was already re-run past the attempt this job failed in; the parked re-run is dropped",
			"runID", e.RunID, "runAttempt", e.FailedAttempt, "cause", e.Cause)
		p.dropPendingRerun(ctx, target, key, e.Disruption, log)
		return nil, false
	case run.status != runStatusCompleted:
		return nil, false
	}

	if !p.pendingSets.claimRelease(entry) {
		return nil, false
	}
	log.Info("the run concluded; releasing its parked re-run", "runID", e.RunID, "job", e.Job,
		"cause", e.Cause, "waited", age.Round(time.Second))
	// No retry delay: the run has concluded, which is the state rerun-failed-jobs accepts.
	recovered := p.handleEviction(ctx, target, owner, repo, e.RunID, log, p.ownerMaxEvictionRetries(ctx, target), 0,
		tier, e.Cause, rerunDisruption{key: e.Disruption, job: e.Job, failedAttempt: e.FailedAttempt})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer p.pendingSets.forget(entry)
		<-recovered
		// Dropped only once the release has finished, so a restart mid-release releases
		// it again and the reservation resumes rather than reserving a second slot.
		p.dropPendingRerun(context.WithoutCancel(ctx), target, key, e.Disruption, log)
	}()
	return done, true
}

// dropPendingRerun deletes the entry at key where it is still disruption's.
func (p *Provisioner) dropPendingRerun(ctx context.Context, target Target, key, disruption string, log *slog.Logger) {
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, _ time.Time) bool {
		if r := l.Pending[key]; r == nil || r.Disruption != disruption {
			return false
		}
		delete(l.Pending, key)
		return true
	})
	if err != nil {
		log.Warn("could not remove a finished parked re-run from the failure ledger; a later sweep retries it",
			"entry", key, "error", err)
	}
}
