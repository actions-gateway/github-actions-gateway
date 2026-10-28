package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	neturl "net/url"
	"strconv"
	"time"
)

// Known-futile re-runs on the scale-set tier (Q1181, R18 of
// docs/plan/q555-flaky-job-retry.md). A disruption arm's re-run that GitHub will refuse,
// or that a newer commit has made pointless, still costs a budget slot, a node and queue
// time, and ends in an Event asking for a manual re-run that would not help either.
// futileRerun judges the run read handleScaleSetEviction already makes, before the
// reservation, so a withheld re-run spends nothing. The classic tier makes no read before
// its in-memory reservation and is not covered.

// Reason labels on EvictionRerunWithheld for the futility checks.
const (
	rerunWithheldReasonAttemptLimit = "attempt_limit"
	rerunWithheldReasonRunTooOld    = "run_too_old"
	rerunWithheldReasonSuperseded   = "superseded"
)

const (
	// githubRerunLimit is how many times GitHub lets a workflow run be re-run, counting
	// full and partial re-runs alike (changelog 2026-04-10, "Actions workflows are
	// limited to 50 reruns"), so attempt githubRerunLimit+1 is the last.
	githubRerunLimit = 50
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
func (p *Provisioner) futileRerun(ctx context.Context, target Target, owner, repo, runID, tier, cause string, run runState, log *slog.Logger) bool {
	switch {
	case run.status == runStatusCompleted && run.conclusion == runConclusionCancelled &&
		p.selfCancelledAttempt(ctx, target, runID) != run.attempt:
		return p.withholdFutileRerun(target, runID, tier, cause, run.attempt, log, rerunWithheldReasonRunCancelled, runCancelledMessage(runID, cause))
	case run.attempt > githubRerunLimit:
		return p.withholdFutileRerun(target, runID, tier, cause, run.attempt, log, rerunWithheldReasonAttemptLimit, fmt.Sprintf("worker pod for run %s was lost to %s, but the run is at attempt %d and GitHub allows at most %d re-runs of a run, so no automatic re-run was requested; start a new run to retry the job",
			runID, cause, run.attempt, githubRerunLimit))
	case !run.createdAt.IsZero() && p.nowFn().Sub(run.createdAt) > githubRerunWindow:
		return p.withholdFutileRerun(target, runID, tier, cause, run.attempt, log, rerunWithheldReasonRunTooOld, fmt.Sprintf("worker pod for run %s was lost to %s, but the run was created %s, and GitHub re-runs only runs created in the last 30 days, so no automatic re-run was requested; start a new run to retry the job",
			runID, cause, run.createdAt.UTC().Format(time.RFC3339)))
	}
	if newer, ok := p.supersedingRun(ctx, owner, repo, run, log); ok {
		return p.withholdFutileRerun(target, runID, tier, cause, run.attempt, log, rerunWithheldReasonSuperseded, fmt.Sprintf("worker pod for run %s was lost to %s, but run %d of the same workflow has since started on %s for a newer commit (%.7s), so no automatic re-run was requested; nothing to do, the newer commit is building",
			runID, cause, newer.ID, run.headBranch, newer.HeadSHA))
	}
	return false
}

// withholdFutileRerun logs and records one futility check's withhold, and reports true.
func (p *Provisioner) withholdFutileRerun(target Target, runID, tier, cause string, attempt int, log *slog.Logger, reason, message string) bool {
	log.Info("the disrupted run's re-run would be futile; no automatic re-run",
		"runID", runID, "runAttempt", attempt, "cause", cause, "reason", reason)
	p.recordRerunWithheld(target, runID, tier, cause, reason, message)
	return true
}

// runCancelledMessage is the run_cancelled Event, shared by the pre-reservation check and
// the re-run loop's.
func runCancelledMessage(runID, cause string) string {
	return fmt.Sprintf("worker pod for run %s was lost to %s, but GitHub concluded the run cancelled, so no automatic re-run was requested; re-run it by hand if the cancellation was not intended", runID, cause)
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
// upstream's of the same name (M5 in docs/plan/q555-automatic-recovery-design.md).
//
// A list that cannot be read answers false: the check only ever withholds a re-run, so
// an unreadable answer must not withhold one.
func (p *Provisioner) supersedingRun(ctx context.Context, owner, repo string, run runState, log *slog.Logger) (workflowRunSummary, bool) {
	if !supersedableEvents[run.event] || run.workflowID == 0 || run.headBranch == "" ||
		run.headSHA == "" || run.headRepo == "" {
		return workflowRunSummary{}, false
	}
	query := neturl.Values{"branch": {run.headBranch}, "event": {run.event}, "per_page": {"1"}}
	status, body, err := p.githubGET(ctx, owner, repo,
		"actions/workflows/"+strconv.FormatInt(run.workflowID, 10)+"/runs", query)
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
			"runID", run.id, "error", err)
		return workflowRunSummary{}, false
	}
	if len(list.Runs) == 0 {
		return workflowRunSummary{}, false
	}
	newest := list.Runs[0]
	if newest.ID == run.id || newest.HeadSHA == run.headSHA ||
		newest.HeadRepository.FullName != run.headRepo || !newest.CreatedAt.After(run.createdAt) {
		return workflowRunSummary{}, false
	}
	return newest, true
}
