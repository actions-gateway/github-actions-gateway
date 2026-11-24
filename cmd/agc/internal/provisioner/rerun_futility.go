package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strconv"
	"time"
)

// Known-futile re-runs on the scale-set tier (Q1181; docs/design/04-operational-flows.md
// §4.2 has the operator account). A disruption arm's re-run that GitHub will refuse,
// or that a newer commit has made pointless, still costs a budget slot, a node and queue
// time, and ends in an Event asking for a manual re-run that would not help either.
// futileRerun judges the run read handleScaleSetEviction already makes, before the
// reservation, so a withheld re-run spends nothing. The classic tier makes no read before
// its in-memory reservation and is not covered.

// errJobsUnsettled marks a moved attempt too young for its jobs list to be complete. A
// later read can answer, so it wraps errRunConclusionUnreadable.
var errJobsUnsettled = fmt.Errorf("the latest attempt's jobs list may still be filling: %w", errRunConclusionUnreadable)

// Reason labels on EvictionRerunWithheld for the futility checks.
const (
	rerunWithheldReasonAlreadyRerun      = "already_rerun"
	rerunWithheldReasonRunSucceeded      = "run_succeeded"
	rerunWithheldReasonAttemptLimit      = "attempt_limit"
	rerunWithheldReasonRunTooOld         = "run_too_old"
	rerunWithheldReasonSuperseded        = "superseded"
	rerunWithheldReasonWorkerUnstartable = "worker_unstartable"
)

const (
	// githubRerunLimit is how many times GitHub lets a workflow run be re-run, counting
	// full and partial re-runs alike (changelog 2026-04-10, "Actions workflows are
	// limited to 50 reruns"), so attempt githubRerunLimit+1 is the last.
	githubRerunLimit = 50
	// attemptStartSkew is how much later than a job's assignment an attempt must start to
	// be a later one, since the two times come from different clocks.
	attemptStartSkew = time.Minute
	// jobListSettle is how old a running attempt must be before a job missing from its list
	// is one it re-ran. GitHub fills the list over the attempt's first seconds: in 62 re-run
	// attempts across 15 repositories, every carried job was created within 15 s of the
	// start (2026-11-23). The rest allows for the AGC's clock running ahead.
	jobListSettle = 2 * time.Minute
	// githubRerunWindow is how long after a run is created GitHub lets it be re-run.
	githubRerunWindow = 30 * 24 * time.Hour
)

// supersedableEvents are the events whose branch can move under a run, so a newer run
// of the same workflow, event and branch makes this one's re-run pointless.
var supersedableEvents = map[string]bool{"push": true, "pull_request": true}

// futileRerun reports whether a re-run of run would be futile, and where it would,
// withholds it: it logs, counts the reason on EvictionRerunWithheld and events the reason
// with its fix. The checks run in the design's order; a field GitHub did not send skips
// its check rather than withholding.
//
// It sets d.failedAttempt to run's attempt where the run moved past the attempt the job
// failed in without re-running the job: the failure stands in the latest attempt too, so
// that is the attempt a re-run starts from.
func (p *Provisioner) futileRerun(ctx context.Context, target Target, owner, repo, runID, cause string, run runState, d *rerunDisruption, log *slog.Logger) bool {
	switch run.rerun {
	case jobRerun:
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonAlreadyRerun, fmt.Sprintf("worker pod for run %s was lost to %s, but the run has been re-run since, up to attempt %d, and that re-run covers the lost job, so no automatic re-run was requested; nothing to do",
			runID, cause, run.attempt))
	case jobNotRerun:
		d.failedAttempt = run.attempt
	}
	switch {
	case run.status == runStatusCompleted && run.conclusion == runConclusionCancelled &&
		p.selfCancelledAttempt(ctx, target, runID) != run.attempt:
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonRunCancelled, runCancelledMessage(runID, cause))
	case run.status == runStatusCompleted && run.conclusion == runConclusionSuccess:
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonRunSucceeded, runSucceededMessage(runID, cause))
	case run.attempt > githubRerunLimit:
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonAttemptLimit, fmt.Sprintf("worker pod for run %s was lost to %s, but the run is at attempt %d and GitHub allows at most %d re-runs of a run, so no automatic re-run was requested; start a new run to retry the job",
			runID, cause, run.attempt, githubRerunLimit))
	case !run.createdAt.IsZero() && p.nowFn().Sub(run.createdAt) > githubRerunWindow:
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonRunTooOld, fmt.Sprintf("worker pod for run %s was lost to %s, but the run was created %s, and GitHub re-runs only runs created in the last 30 days, so no automatic re-run was requested; start a new run to retry the job",
			runID, cause, run.createdAt.UTC().Format(time.RFC3339)))
	}
	if newer, ok := p.supersedingRun(ctx, owner, repo, runID, run, log); ok {
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonSuperseded, fmt.Sprintf("worker pod for run %s was lost to %s, but run %d of the same workflow has since started on %s for a newer commit (%.7s), so no automatic re-run was requested; nothing to do, the newer commit is building",
			runID, cause, newer.ID, run.headBranch, newer.HeadSHA))
	}
	if reader, ok := target.(StartupFailureReader); ok {
		if detail, failing := reader.WorkersFailingToStart(ctx); failing {
			return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonWorkerUnstartable, fmt.Sprintf("worker pod for run %s was lost to %s, but no worker of this set can start: %s. A re-run's worker would fail the same way, so no automatic re-run was requested; fix the image or its pull credentials, then re-run it by hand",
				runID, cause, detail))
		}
	}
	return false
}

// StartupFailureReader is implemented by a Target that can tell when every next worker
// pod of its owner would be placed and then fail to start. It is optional: a Target
// without it skips the worker_unstartable check.
type StartupFailureReader interface {
	// WorkersFailingToStart reports whether the owner's workers are failing to start,
	// with the observation behind it. An answer it cannot read is false.
	WorkersFailingToStart(ctx context.Context) (detail string, failing bool)
}

// withholdFutileRerun logs and records one futility check's withhold, and reports true.
func (p *Provisioner) withholdFutileRerun(target Target, runID, cause string, attempt int, log *slog.Logger, reason, message string) bool {
	log.Info("the disrupted run's re-run would be futile; no automatic re-run",
		"runID", runID, "runAttempt", attempt, "cause", cause, "reason", reason)
	p.recordRerunWithheld(target, runID, cause, reason, message)
	return true
}

// runCancelledMessage is the run_cancelled Event, shared by the pre-reservation check and
// the re-run loop's.
func runCancelledMessage(runID, cause string) string {
	return fmt.Sprintf("worker pod for run %s was lost to %s, but GitHub concluded the run cancelled, so no automatic re-run was requested; re-run it by hand if the cancellation was not intended", runID, cause)
}

// runSucceededMessage is the run_succeeded Event, shared the same way.
func runSucceededMessage(runID, cause string) string {
	return fmt.Sprintf("worker pod for run %s was lost to %s, but GitHub concluded the run success, and refuses to re-run a successful run's failed jobs, so no automatic re-run was requested; nothing to do", runID, cause)
}

// attemptMoved reports whether run has started an attempt after the one d's job failed
// in. Where that attempt is not known, the job was assigned in an earlier one if run's
// latest attempt started after the assignment; without both times nothing says so.
func attemptMoved(run runState, d rerunDisruption) bool {
	if d.failedAttempt > 0 {
		return run.attempt > d.failedAttempt
	}
	return run.attempt > 1 && !d.assignedAt.IsZero() && run.startedAt.After(d.assignedAt.Add(attemptStartSkew))
}

// jobRerunVerdict is whether a run past the attempt a job failed in re-ran that job.
type jobRerunVerdict int

const (
	// jobRerunUnknown: the latest attempt's jobs do not say, or could not be read.
	jobRerunUnknown jobRerunVerdict = iota
	// jobNotRerun: the latest attempt carries the job's failure.
	jobNotRerun
	// jobRerun: the job has an attempt after the one it failed in.
	jobRerun
)

const (
	// jobsPerPage is the jobs API's page size limit.
	jobsPerPage = 100
	// maxJobPages bounds how much of an attempt's job list is read.
	maxJobPages = 10
)

// workflowJobSummary is the part of a listed job the already_rerun check reads.
type workflowJobSummary struct {
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	RunnerName  string    `json:"runner_name"`
	CompletedAt time.Time `json:"completed_at"`
}

// carried reports a job that run's attempt did not run: GitHub lists every job of the run
// under each attempt, and one the attempt did not re-run keeps the runner_name and
// completed_at of the attempt it ran in (measured on this repository's run 45325262167,
// 2026-11-23). Its created_at is new and no guide: a job that never ran is listed as
// completed a second before it was created.
func (j workflowJobSummary) carried(run runState) bool {
	return !j.CompletedAt.IsZero() && j.CompletedAt.Before(run.startedAt)
}

func (j workflowJobSummary) failed() bool {
	switch j.Conclusion {
	case "", runConclusionSuccess, "skipped", "neutral":
		return false
	}
	return true
}

// jobRerunSince lists the jobs of run's latest attempt to say whether d's job, which
// failed in an earlier one, was re-run. A worker whose runner took the job is matched by
// runner name, which a carried job keeps; otherwise the job was re-run where the attempt
// carries no failed job at all, or by display name. Anything that rests on a job missing
// from the list waits until the list is complete (errJobsUnsettled). An error means
// jobRerunUnknown, and wraps errRunConclusionUnreadable where a later read could answer.
func (p *Provisioner) jobRerunSince(ctx context.Context, owner, repo, runID string, run runState, d rerunDisruption) (jobRerunVerdict, error) {
	jobs, err := p.attemptJobs(ctx, owner, repo, runID, run.attempt)
	if err != nil {
		return jobRerunUnknown, err
	}
	var carriedFailure, carriedByName, rerunByName bool
	for _, j := range jobs {
		if d.runner != "" && j.RunnerName == d.runner {
			return jobNotRerun, nil
		}
		named := d.job != "" && j.Name == d.job
		switch {
		case !j.carried(run):
			rerunByName = rerunByName || named
		case j.failed():
			carriedFailure = true
			carriedByName = carriedByName || named
		}
	}
	switch {
	case d.runner == "" && carriedByName:
		return jobNotRerun, nil
	case run.status != runStatusCompleted && time.Since(run.startedAt) < jobListSettle:
		return jobRerunUnknown, errJobsUnsettled
	case len(jobs) == 0 || run.startedAt.IsZero():
		return jobRerunUnknown, nil
	case d.runner != "", !carriedFailure, rerunByName:
		return jobRerun, nil
	}
	return jobRerunUnknown, nil
}

// attemptJobs lists every job of one attempt of a run.
func (p *Provisioner) attemptJobs(ctx context.Context, owner, repo, runID string, attempt int) ([]workflowJobSummary, error) {
	if err := p.githubReady(owner, repo, "attempt-jobs"); err != nil {
		return nil, err
	}
	var jobs []workflowJobSummary
	for page := 1; page <= maxJobPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/attempts/%d/jobs",
			p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), neturl.PathEscape(runID), attempt)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build attempt-jobs request: %w", err)
		}
		req.URL.RawQuery = neturl.Values{"per_page": {strconv.Itoa(jobsPerPage)}, "page": {strconv.Itoa(page)}}.Encode()
		status, body, err := p.githubSend(ctx, req)
		switch {
		case errors.Is(err, errGitHubUnreachable):
			return nil, fmt.Errorf("attempt-jobs API call: %v: %w", err, errRunConclusionUnreadable)
		case err != nil:
			return nil, err
		case status >= 500:
			return nil, fmt.Errorf("attempt-jobs API returned %d: %w", status, errRunConclusionUnreadable)
		case status < 200 || status > 299:
			return nil, fmt.Errorf("attempt-jobs API returned %d", status)
		}
		var list struct {
			TotalCount int                  `json:"total_count"`
			Jobs       []workflowJobSummary `json:"jobs"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode attempt jobs: %w", err)
		}
		jobs = append(jobs, list.Jobs...)
		if len(jobs) >= list.TotalCount {
			return jobs, nil
		}
		if len(list.Jobs) == 0 {
			break
		}
	}
	return nil, fmt.Errorf("attempt %d lists more jobs than the %d read", attempt, len(jobs))
}

// workflowRunSummary is the part of a listed run the superseded check compares.
type workflowRunSummary struct {
	ID             int64     `json:"id"`
	HeadSHA        string    `json:"head_sha"`
	CreatedAt      time.Time `json:"created_at"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// supersedingRun returns the newest run of run's workflow, event and branch where it is a
// newer commit from the same head repository, which supersedes run. Filtering on the
// event and comparing the head repository keeps a fork's branch from matching
// upstream's of the same name. The branch filter matches a fork pull request's head
// branch, but one result per page lets another fork's newer run hide this fork's own
// (measured on github/docs, 2026-11-22); a miss re-runs.
//
// A list that cannot be read answers false: the check only ever withholds a re-run, so
// an unreadable answer must not withhold one.
func (p *Provisioner) supersedingRun(ctx context.Context, owner, repo, runID string, run runState, log *slog.Logger) (workflowRunSummary, bool) {
	if !supersedableEvents[run.event] || run.workflowID == 0 || run.headBranch == "" ||
		run.headSHA == "" || run.headRepo == "" {
		return workflowRunSummary{}, false
	}
	if err := p.githubReady(owner, repo, "workflow-runs"); err != nil {
		log.Warn("could not list the workflow's newer runs; re-running without the superseded check",
			"runID", runID, "error", err)
		return workflowRunSummary{}, false
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%d/runs",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), run.workflowID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		log.Warn("could not list the workflow's newer runs; re-running without the superseded check",
			"runID", runID, "error", err)
		return workflowRunSummary{}, false
	}
	req.URL.RawQuery = neturl.Values{"branch": {run.headBranch}, "event": {run.event}, "per_page": {"1"}}.Encode()
	status, body, err := p.githubSend(ctx, req)
	if err == nil && (status < 200 || status > 299) {
		err = fmt.Errorf("workflow-runs API returned %d", status)
	}
	var list struct {
		Runs []workflowRunSummary `json:"workflow_runs"`
	}
	if err == nil {
		err = json.Unmarshal(body, &list)
	}
	if err != nil {
		log.Warn("could not list the workflow's newer runs; re-running without the superseded check",
			"runID", runID, "error", err)
		return workflowRunSummary{}, false
	}
	if len(list.Runs) == 0 {
		return workflowRunSummary{}, false
	}
	newest := list.Runs[0]
	if newest.HeadSHA == run.headSHA ||
		newest.HeadRepository.FullName != run.headRepo || !newest.CreatedAt.After(run.createdAt) {
		return workflowRunSummary{}, false
	}
	return newest, true
}
