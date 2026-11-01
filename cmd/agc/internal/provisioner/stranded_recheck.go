package provisioner

import (
	"context"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// strandedRecheckWindow bounds how long a declined stranded-job walk waits for every
// Running worker's start to be recorded (Q1235). GitHub cancels a job no runner started
// at its ~15-minute unstarted-job timeout, which runs from the job's queueing, before
// its worker was lost, so a walk still declined after ten minutes would at best
// force-cancel a run GitHub is about to cancel itself.
const strandedRecheckWindow = 10 * time.Minute

// strandedRecheckKey identifies a declined walk by owner and lost worker, so a re-check
// fires at most once per lost worker however many passes see it.
type strandedRecheckKey struct {
	owner   client.ObjectKey
	podName string
}

// pendingStrandedRecheck is one lost worker whose walk declined because a Running
// worker's start was not recorded yet.
type pendingStrandedRecheck struct {
	target Target
	lostAt time.Time
}

// strandedJobFor names the job podName's loss left without a runner
// (strandedScaleSetJob). Where the walk declines only because a Running worker's start
// may be unrecorded, it registers podName for one re-check once every start is
// recorded, which sweepStrandedRechecks takes.
func (p *Provisioner) strandedJobFor(ctx context.Context, target Target, workers scaleSetWorkerSet, podName string, log *slog.Logger) (OrphanedWorker, bool) {
	stranded, found := p.strandedScaleSetJob(ctx, target, workers, podName, log)
	if !found && workers.unrecorded > 0 {
		p.deferStrandedWalk(target, podName)
	}
	return stranded, found
}

// deferStrandedWalk registers podName's walk for a re-check. In process memory, like the
// abandoned arm's waiting set: a restart drops it, and the stranded job then waits out
// GitHub's unstarted-job timeout, as it did before Q1235.
func (p *Provisioner) deferStrandedWalk(target Target, podName string) {
	key := strandedRecheckKey{owner: target.Key(), podName: podName}
	p.strandedRechecksMu.Lock()
	defer p.strandedRechecksMu.Unlock()
	if p.strandedRechecks == nil {
		p.strandedRechecks = make(map[strandedRecheckKey]pendingStrandedRecheck)
	}
	if _, waiting := p.strandedRechecks[key]; waiting {
		return
	}
	p.strandedRechecks[key] = pendingStrandedRecheck{target: target, lostAt: p.nowFn()}
}

// peekStrandedRecheck returns an entry without claiming it.
func (p *Provisioner) peekStrandedRecheck(key strandedRecheckKey) (pendingStrandedRecheck, bool) {
	p.strandedRechecksMu.Lock()
	defer p.strandedRechecksMu.Unlock()
	e, ok := p.strandedRechecks[key]
	return e, ok
}

// takeStrandedRecheck removes and returns an entry, reporting whether it was still
// waiting. Removing it is the claim that keeps the re-check at-most-once.
func (p *Provisioner) takeStrandedRecheck(key strandedRecheckKey) (pendingStrandedRecheck, bool) {
	p.strandedRechecksMu.Lock()
	defer p.strandedRechecksMu.Unlock()
	e, ok := p.strandedRechecks[key]
	if ok {
		delete(p.strandedRechecks, key)
	}
	return e, ok
}

// pendingStrandedRecheckKeys snapshots the waiting keys so the sweep does not hold the
// lock across a pod list or a GitHub call.
func (p *Provisioner) pendingStrandedRecheckKeys() []strandedRecheckKey {
	p.strandedRechecksMu.Lock()
	defer p.strandedRechecksMu.Unlock()
	keys := make([]strandedRecheckKey, 0, len(p.strandedRechecks))
	for k := range p.strandedRechecks {
		keys = append(keys, k)
	}
	return keys
}

// sweepStrandedRechecks is one pass over the declined walks: drop the ones past
// strandedRecheckWindow, leave the ones whose set still has a Running worker with no
// start recorded, and walk the rest once more, force-cancelling the job the walk names.
// Each entry is taken before its walk, so a lost worker is re-checked at most once.
//
// It returns one done channel per force-cancel it started, for the caller to wait on
// (tests) or not (the sweeper loop).
func (p *Provisioner) sweepStrandedRechecks(ctx context.Context) []<-chan struct{} {
	var armed []<-chan struct{}
	for _, key := range p.pendingStrandedRecheckKeys() {
		e, ok := p.peekStrandedRecheck(key)
		if !ok {
			continue
		}
		log := p.logForKey(key.owner).With("podName", key.podName)

		if p.nowFn().Sub(e.lostAt) >= strandedRecheckWindow {
			if _, claimed := p.takeStrandedRecheck(key); claimed {
				log.Info("a worker's job start stayed unrecorded for the whole re-check window; the job the lost worker left without a runner, if any, waits out GitHub's unstarted-job timeout",
					"window", strandedRecheckWindow)
			}
			continue
		}

		workers, err := p.liveScaleSetWorkers(ctx, e.target)
		if err != nil {
			log.Warn("could not list the set's workers to re-check the job left without a runner; retrying next pass", "error", err)
			continue
		}
		if workers.unrecorded > 0 {
			continue
		}
		if _, claimed := p.takeStrandedRecheck(key); !claimed {
			continue
		}
		stranded, found := p.strandedScaleSetJob(ctx, e.target, workers, key.podName, log)
		if !found {
			log.Info("re-checked the lost worker's walk once every job start was recorded, and no job was left without a runner; not force-cancelling")
			continue
		}
		log.Info("re-checked the lost worker's walk once every job start was recorded; recovering the job left without a runner",
			"jobID", stranded.JobID, "runID", stranded.RunID)
		armed = append(armed, p.forceCancelStranded(ctx, e.target, key.podName, stranded, log))
	}
	return armed
}

// forceCancelStranded force-cancels the run of a job a lost worker left without a
// runner and registers it for the capacity-gated re-run, keyed by the lost worker
// (Q1195, Q1235). The job never started, so it takes the abandoned arm's action and
// labels: cause abandoned, one slot of the run's scale-set budget.
func (p *Provisioner) forceCancelStranded(ctx context.Context, target Target, podName string, job OrphanedWorker, log *slog.Logger) <-chan struct{} {
	if job.Owner == "" || job.Repository == "" || job.RunID == "" || job.RunID == "0" {
		log.Warn("the job left without a runner has no complete run identity; not force-cancelling", "jobID", job.JobID)
		return closedChan()
	}
	return p.forceCancelInBackground(ctx, target, podName, job.Owner, job.Repository, job.RunID, "", log)
}

// forceCancelInBackground runs forceCancelAbandonedRun on a context detached from ctx,
// since a reconcile's context ends when Reconcile returns and the POST must outlive it.
// The returned channel closes once the force-cancel has finished.
func (p *Provisioner) forceCancelInBackground(ctx context.Context, target Target, podName, owner, repo, runID, job string, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	fctx := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		p.forceCancelAbandonedRun(fctx, target, owner, repo, runID, evictionTierScaleSet, log,
			rerunDisruption{key: podName, job: job})
	}()
	return done
}
