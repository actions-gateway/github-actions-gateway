package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Two jobs of one run may share a display name and a completion carries no attempt, so
// only the run's job list tells a job's re-run from the other one (
// docs/design/04-operational-flows.md §4.2).

const (
	// runJobsBudget bounds one runJobs.
	runJobsBudget = 10 * time.Second
	// defaultPairingDelay bounds the reads to one list per run in that time, and gives an
	// attempt's list time to fill.
	defaultPairingDelay = 30 * time.Second
	// maxPendingPairings bounds the completions awaiting a list.
	maxPendingPairings = 1024
	// pairingWriteBudget bounds one batch's ledger write.
	pairingWriteBudget = 30 * time.Second
)

// reranIn returns the attempt in which the job that ran on runner re-ran the one that
// failed on failedRunner, given the jobs of every attempt of their run, or 0 where it is
// not known to have. It is a re-run where the jobs of its name differ between its attempt
// and the one before by those two alone. The earlier attempt has finished, so it lists
// every job; the later one need not yet. A job with no runner, a skipped one included,
// matches nothing in the other attempt. A failedRunner of "" is unknown, and pairs only a
// name that one job held.
func reranIn(jobs []workflowJobSummary, runner, failedRunner string) int {
	if runner == "" {
		return 0
	}
	var job workflowJobSummary
	for _, j := range jobs {
		if j.RunnerName == runner && (job.RunAttempt == 0 || j.RunAttempt < job.RunAttempt) {
			job = j
		}
	}
	var before, after []string
	for _, j := range jobs {
		switch {
		case j.Name != job.Name:
		case j.RunAttempt == job.RunAttempt-1:
			before = append(before, j.RunnerName)
		case j.RunAttempt == job.RunAttempt:
			after = append(after, j.RunnerName)
		}
	}
	without := func(a, b []string) []string {
		return slices.DeleteFunc(slices.Clone(a), func(r string) bool { return r != "" && slices.Contains(b, r) })
	}
	gone, added := without(before, after), without(after, before)
	if len(gone) != 1 || len(added) != 1 {
		return 0
	}
	if gone[0] == failedRunner || (failedRunner == "" && len(before) == 1) {
		return job.RunAttempt
	}
	return 0
}

// jobCompletion is one terminal JobCompleted as detection uses it.
type jobCompletion struct {
	target                        Target
	job                           ScaleSetJob
	runID, jobName, result, cause string
	context                       json.RawMessage
	// hold is the cancelled job a succeeded completion may have re-run, nil for none.
	hold *cancelHold
}

// apply records a failed c in l, or pairs c with the record it re-ran, which takes the
// run's jobs over every attempt: nil where they are not read. It reports whether l changed.
func (c *jobCompletion) apply(l *rerunLedger, now time.Time, jobs []workflowJobSummary) (o completionPairing, changed bool) {
	fkey, failed := failureKey(c.runID, c.jobName), c.result != jobResultSucceeded
	f := l.Failures[fkey]
	if f == nil {
		if !failed {
			return o, false
		}
		l.Failures[fkey] = &failureRecord{
			ExpiresAt: now.Add(failureRecordTTL), FailedAt: now, JobID: c.job.JobID, Runner: c.job.RunnerName, Cause: c.cause,
			Event: c.job.EventName, WorkflowRef: c.job.WorkflowRef, Context: c.context,
		}
		o.recorded = true
		return o, true
	}
	if f.JobID == c.job.JobID {
		return o, false
	}
	attempt := reranIn(jobs, c.job.RunnerName, f.Runner)
	if attempt == 0 {
		o.matched = true
		return o, false
	}
	o.paired, o.event = true, f.Event
	o.trigger, o.cause = pairFailure(l.Reruns[c.runID], attempt-1, c.jobName)
	if !failed {
		o.flaky = o.trigger != rerunTriggerGAG && f.Cause == ""
		delete(l.Failures, fkey)
		return o, true
	}
	o.recorded = true
	f.JobID, f.Runner, f.Cause, f.ExpiresAt = c.job.JobID, c.job.RunnerName, c.cause, now.Add(failureRecordTTL)
	return o, true
}

// pairingQueue holds the completions awaiting their run's job list, by RunnerSet and run.
type pairingQueue struct {
	mu      sync.Mutex
	byRun   map[string]*pairingBatch
	waiting int
}

// pairingBatch is the completions one job list pairs.
type pairingBatch struct {
	completions []*jobCompletion
	done        chan struct{}
}

// queuePairing pairs c once its run's jobs are read, which it does off the caller's
// goroutine: the listener handles one message at a time, and a list costs up to 10
// requests. A run's completions gather for pairingDelay and share one list and one ledger
// write. The channel closes when c has been paired or given up on.
func (p *Provisioner) queuePairing(ctx context.Context, c *jobCompletion) <-chan struct{} {
	q := &p.pairings
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.waiting >= maxPendingPairings {
		p.completionLog(c).Warn("too many completions await their run's jobs; a re-run goes uncounted")
		return closedChan()
	}
	q.waiting++
	run := strings.Join([]string{c.target.Key().String(), c.job.Owner, c.job.Repository, c.runID}, "/")
	b := q.byRun[run]
	if b == nil {
		b = &pairingBatch{done: make(chan struct{})}
		if q.byRun == nil {
			q.byRun = map[string]*pairingBatch{}
		}
		q.byRun[run] = b
		// A listener restart must not lose the batch.
		go p.pairBatch(context.WithoutCancel(ctx), run, b)
	}
	b.completions = append(b.completions, c)
	return b.done
}

// pairBatch pairs b's completions, in the order they arrived, after pairingDelay.
func (p *Provisioner) pairBatch(ctx context.Context, run string, b *pairingBatch) {
	defer close(b.done)
	time.Sleep(p.pairingDelay)
	q := &p.pairings
	q.mu.Lock()
	delete(q.byRun, run)
	q.waiting -= len(b.completions)
	q.mu.Unlock()

	first := b.completions[0]
	log := p.logForKey(first.target.Key()).With("runID", first.runID, "completions", len(b.completions))
	jobs, err := p.runJobs(ctx, first.job.Owner, first.job.Repository, first.runID)
	if err != nil {
		log.Warn("could not read the run's jobs to pair its failed jobs' re-runs; they go uncounted", "error", err)
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, pairingWriteBudget)
	defer cancel()
	pairings, held := make([]completionPairing, len(b.completions)), make([]*heldRerun, len(b.completions))
	err = p.updateRerunLedger(writeCtx, first.target, func(l *rerunLedger, now time.Time) (changed bool) {
		for i, c := range b.completions {
			var wrote bool
			pairings[i], wrote = c.apply(l, now, jobs)
			changed = changed || wrote
			// One completion re-ran one job, and a record of it is the better evidence.
			held[i] = nil
			if !pairings[i].paired {
				held[i] = heldRerunOf(c, jobs, l.Reruns[c.runID])
			}
		}
		return changed
	})
	if err != nil {
		log.Warn("could not record the re-runs of a run's failed jobs; they go uncounted", "error", err)
		return
	}
	for i, c := range b.completions {
		switch o := pairings[i]; {
		case o.paired:
			p.reportRerunJob(c.target, c.runID, c.jobName, c.result, o, p.completionLog(c))
		case o.matched && held[i] == nil:
			p.completionLog(c).Info("scale-set job is not known to re-run the failed job that shares its name; paired with nothing")
		}
	}
	if held = slices.DeleteFunc(held, func(h *heldRerun) bool { return h == nil }); len(held) > 0 {
		p.pairTimedOut(ctx, held, log)
	}
}

func (p *Provisioner) completionLog(c *jobCompletion) *slog.Logger {
	return p.logForKey(c.target.Key()).With("runID", c.runID, "job", c.jobName, "jobID", c.job.JobID, "result", c.result)
}

// runJobs calls GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs for the jobs of every
// attempt of the run.
func (p *Provisioner) runJobs(ctx context.Context, owner, repo, runID string) ([]workflowJobSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, runJobsBudget)
	defer cancel()
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/jobs",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), neturl.PathEscape(runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build run-jobs request: %w", err)
	}
	return p.listJobs(ctx, owner, repo, "run-jobs", req, neturl.Values{"filter": {"all"}})
}
