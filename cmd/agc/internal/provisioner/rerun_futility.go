package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
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
	case run.cancelled:
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
		return p.withholdFutileRerun(target, runID, cause, run.attempt, log, rerunWithheldReasonSuperseded, supersededMessage(runID, cause, run, newer))
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

// supersededMessage is the superseded Event, shared the same way.
func supersededMessage(runID, cause string, run runState, newer workflowRunSummary) string {
	if run.event == mergeGroupEvent {
		return fmt.Sprintf("worker pod for run %s was lost to %s, but the merge queue has since built another group for the pull request (run %d, commit %.7s), so no automatic re-run was requested; nothing to do, the queue no longer waits on this group",
			runID, cause, newer.ID, newer.HeadSHA)
	}
	return fmt.Sprintf("worker pod for run %s was lost to %s, but run %d of the same workflow has since started on %s for a newer commit (%.7s), so no automatic re-run was requested; nothing to do, the newer commit is building",
		runID, cause, newer.ID, run.headBranch, newer.HeadSHA)
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
	// maxJobPages bounds how much of a job list is read.
	maxJobPages = 10
)

// workflowJobSummary is the part read of one job of one attempt of a run. A job an
// attempt did not re-run is listed in it again on the runner that ran it. RunnerName is
// "" until the job has a runner.
type workflowJobSummary struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	RunnerName  string    `json:"runner_name"`
	RunAttempt  int       `json:"run_attempt"`
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
	return jobRerunIn(jobs, run, d)
}

// jobRerunIn is jobRerunSince's verdict on the listed jobs of run's latest attempt.
func jobRerunIn(jobs []workflowJobSummary, run runState, d rerunDisruption) (jobRerunVerdict, error) {
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

// cancelledByOther reports whether run, where GitHub concluded it cancelled, was cancelled
// by anybody but GAG's own force-cancel. GitHub also concludes an attempt cancelled that
// only carries cancelled jobs from an earlier one, as a single-job re-run beside a
// fail-fast matrix's cancelled legs does (measured on rust-lang/cargo's runs 40960902618
// and 38222465819, 2026-11-24). So an attempt that ran no job it lists as cancelled is
// judged by the attempt before it. What cannot be read counts as somebody's cancel; an
// error wraps errRunConclusionUnreadable.
func (p *Provisioner) cancelledByOther(ctx context.Context, target Target, owner, repo, runID string, run runState) (bool, error) {
	if run.status != runStatusCompleted || run.conclusion != runConclusionCancelled {
		return false, nil
	}
	for self := p.selfCancelledAttempt(ctx, target, runID); run.conclusion == runConclusionCancelled; {
		switch {
		case run.attempt == self:
			return false, nil
		case run.attempt <= 1:
			return true, nil
		}
		jobs, err := p.attemptJobs(ctx, owner, repo, runID, run.attempt)
		if errors.Is(err, errRunConclusionUnreadable) {
			return true, err
		}
		if err != nil || len(jobs) == 0 || slices.ContainsFunc(jobs, func(j workflowJobSummary) bool {
			return j.Conclusion == runConclusionCancelled && !j.carried(run)
		}) {
			return true, nil
		}
		before, err := p.readRun(ctx, owner, repo, runID, run.attempt-1)
		if errors.Is(err, errRunConclusionUnreadable) {
			return true, err
		}
		if err != nil || before.attempt != run.attempt-1 || before.conclusion == "" {
			return true, nil
		}
		run = before
	}
	return false, nil
}

// attemptJobs lists every job of one attempt of a run.
func (p *Provisioner) attemptJobs(ctx context.Context, owner, repo, runID string, attempt int) ([]workflowJobSummary, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/attempts/%d/jobs",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), neturl.PathEscape(runID), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build attempt-jobs request: %w", err)
	}
	return p.listJobs(ctx, owner, repo, "attempt-jobs", req, neturl.Values{})
}

// listJobs reads every page of the jobs list req asks for, or none past maxJobPages of
// them. An error wraps errRunConclusionUnreadable where a later read could answer.
func (p *Provisioner) listJobs(ctx context.Context, owner, repo, what string, req *http.Request, query neturl.Values) ([]workflowJobSummary, error) {
	if err := p.githubReady(owner, repo, what); err != nil {
		return nil, err
	}
	query.Set("per_page", strconv.Itoa(jobsPerPage))
	var jobs []workflowJobSummary
	for page := 1; page <= maxJobPages; page++ {
		query.Set("page", strconv.Itoa(page))
		req := req.Clone(ctx)
		req.URL.RawQuery = query.Encode()
		status, body, err := p.githubSend(ctx, req)
		switch {
		case errors.Is(err, errGitHubUnreachable):
			return nil, fmt.Errorf("%s API call: %v: %w", what, err, errRunConclusionUnreadable)
		case err != nil:
			return nil, err
		case status >= 500:
			return nil, fmt.Errorf("%s API returned %d: %w", what, status, errRunConclusionUnreadable)
		case status < 200 || status > 299:
			return nil, fmt.Errorf("%s API returned %d", what, status)
		}
		var list struct {
			TotalCount int                  `json:"total_count"`
			Jobs       []workflowJobSummary `json:"jobs"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode %s: %w", what, err)
		}
		if list.TotalCount > maxJobPages*jobsPerPage {
			return nil, fmt.Errorf("%s lists %d jobs, more than the %d read", what, list.TotalCount, maxJobPages*jobsPerPage)
		}
		jobs = append(jobs, list.Jobs...)
		if len(jobs) >= list.TotalCount {
			return jobs, nil
		}
		if len(list.Jobs) == 0 {
			break
		}
	}
	return nil, fmt.Errorf("%s list ended early, at %d jobs", what, len(jobs))
}

// workflowRunSummary is the part of a listed run the superseded check compares.
type workflowRunSummary struct {
	ID             int64     `json:"id"`
	HeadBranch     string    `json:"head_branch"`
	HeadSHA        string    `json:"head_sha"`
	CreatedAt      time.Time `json:"created_at"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// supersedingRun returns a newer run of run's workflow that makes run's result one nothing
// reads: on the same branch for a push or a pull request, for the same pull request in
// the merge queue. A list that cannot be read answers false: the check only ever
// withholds a re-run, so an unreadable answer must not withhold one.
func (p *Provisioner) supersedingRun(ctx context.Context, owner, repo, runID string, run runState, log *slog.Logger) (workflowRunSummary, bool) {
	if run.workflowID == 0 || run.headBranch == "" || run.headSHA == "" || run.headRepo == "" {
		return workflowRunSummary{}, false
	}
	var find func(context.Context, string, string, runState) (workflowRunSummary, bool, error)
	switch {
	case supersedableEvents[run.event]:
		find = p.newerBranchRun
	case run.event == mergeGroupEvent && !run.createdAt.IsZero():
		find = p.newerMergeGroupRun
	default:
		return workflowRunSummary{}, false
	}
	newer, ok, err := find(ctx, owner, repo, run)
	if err != nil {
		log.Warn("could not list the workflow's newer runs; re-running without the superseded check",
			"runID", runID, "error", err)
	}
	return newer, ok
}

// newerBranchRun returns the newest run of run's workflow, event and branch where it is a
// newer commit from the same head repository. Filtering on the event and comparing the
// head repository keeps a fork's branch from matching upstream's of the same name. The
// branch filter matches a fork pull request's head branch, but one result per page lets
// another fork's newer run hide this fork's own (measured on github/docs, 2026-11-22); a
// miss re-runs.
func (p *Provisioner) newerBranchRun(ctx context.Context, owner, repo string, run runState) (workflowRunSummary, bool, error) {
	runs, err := p.workflowRuns(ctx, owner, repo, run, neturl.Values{"branch": {run.headBranch}, "per_page": {"1"}})
	if err != nil || len(runs) == 0 {
		return workflowRunSummary{}, false, err
	}
	newest := runs[0]
	if newest.HeadSHA == run.headSHA ||
		newest.HeadRepository.FullName != run.headRepo || !newest.CreatedAt.After(run.createdAt) {
		return workflowRunSummary{}, false, nil
	}
	return newest, true, nil
}

// mergeGroupEvent is the event of a run the merge queue started.
const mergeGroupEvent = "merge_group"

// mergeGroupBranchRE captures what a merge group's branch shares with every other group
// of one pull request: the queue's target branch and the pull request's number. The
// commit after them differs between two groups.
var mergeGroupBranchRE = regexp.MustCompile(`^(gh-readonly-queue/.+/pr-[0-9]+-)[0-9a-f]+$`)

const (
	// mergeGroupRunsPerPage keeps a page of runs, about 16 KB each, inside the response
	// cap githubSend reads.
	mergeGroupRunsPerPage = 30
	// maxMergeGroupRunPages bounds how far back from the newest run the search goes.
	maxMergeGroupRunPages = 3
)

// newerMergeGroupRun returns a newer run of run's workflow for another group of the same
// pull request, at a different commit. A pull request holds one place in a queue, so the
// queue has abandoned run's group: GitHub deletes the group's ref but leaves its runs
// going (measured on this repository's queue, 2026-11-23). A group that merged has no
// newer one. The ref itself is not read, which needs Contents: read.
func (p *Provisioner) newerMergeGroupRun(ctx context.Context, owner, repo string, run runState) (workflowRunSummary, bool, error) {
	group := mergeGroupBranchRE.FindStringSubmatch(run.headBranch)
	if group == nil {
		return workflowRunSummary{}, false, nil
	}
	for page := 1; page <= maxMergeGroupRunPages; page++ {
		runs, err := p.workflowRuns(ctx, owner, repo, run, neturl.Values{
			"created":  {">=" + run.createdAt.UTC().Format(time.RFC3339)},
			"per_page": {strconv.Itoa(mergeGroupRunsPerPage)},
			"page":     {strconv.Itoa(page)},
		})
		if err != nil {
			return workflowRunSummary{}, false, err
		}
		for _, r := range runs {
			other := mergeGroupBranchRE.FindStringSubmatch(r.HeadBranch)
			if other != nil && other[1] == group[1] && r.HeadSHA != run.headSHA && r.CreatedAt.After(run.createdAt) {
				return r, true, nil
			}
		}
		if len(runs) < mergeGroupRunsPerPage {
			break
		}
	}
	return workflowRunSummary{}, false, nil
}

// workflowRuns lists the runs of run's workflow and event that query selects, newest first.
func (p *Provisioner) workflowRuns(ctx context.Context, owner, repo string, run runState, query neturl.Values) ([]workflowRunSummary, error) {
	if err := p.githubReady(owner, repo, "workflow-runs"); err != nil {
		return nil, err
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%d/runs",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), run.workflowID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build workflow-runs request: %w", err)
	}
	query.Set("event", run.event)
	req.URL.RawQuery = query.Encode()
	status, body, err := p.githubSend(ctx, req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("workflow-runs API returned %d", status)
	}
	var list struct {
		Runs []workflowRunSummary `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode workflow runs: %w", err)
	}
	return list.Runs, nil
}
