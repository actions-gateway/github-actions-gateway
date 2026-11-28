package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"slices"
)

// Job-granular re-runs. rerun-failed-jobs re-runs every failed job of the run, so
// a class firing on one job would re-run, and could hide, a sibling's genuine failure. A
// parked re-run therefore re-runs only the jobs whose workers the class killed, found in
// the latest attempt's jobs list by their runners' names. Display names are not enough: two
// jobs of one run can share one. The operator-facing account is
// docs/design/04-operational-flows.md §4.2.

// rerunWithheldReasonJobNotFound is the EvictionRerunWithheld reason for a parked re-run
// no failed job of the run ran on the runner of, so there is nothing to re-run.
const rerunWithheldReasonJobNotFound = "job_not_found"

// errJobNotCurrent marks GitHub's refusal of a job ID from an earlier attempt: the run
// was re-run since the jobs list was read. The next pass reads the latest list.
var errJobNotCurrent = errors.New("the job is not in the run's latest attempt")

// killedWorker is one worker a parked re-run was recorded for.
type killedWorker struct {
	// Job is the display name on the worker's pod, which need not be the job its runner
	// took.
	Job string `json:"job"`
	// Runner is the GitHub runner registered for the worker, "" where the pod names none.
	Runner string `json:"runner,omitempty"`
	// Disruption is the worker pod, as in rerunAttempt.Disruptions.
	Disruption string `json:"disruption"`
	extra      map[string]json.RawMessage
}

// find returns the listed job w's runner ran. Without a runner name it is the one job of
// w's display name, and ambiguous where the list holds none or several.
func (w killedWorker) find(jobs []workflowJobSummary) (job workflowJobSummary, found, ambiguous bool) {
	n := 0
	for _, j := range jobs {
		if w.Runner != "" {
			if j.RunnerName == w.Runner {
				return j, true, false
			}
			continue
		}
		if j.Name == w.Job {
			job = j
			n++
		}
	}
	return job, n == 1, w.Runner == "" && n != 1
}

// planJobRerun decides what releasing a parked re-run of workers re-runs, given the jobs of
// the run's latest, concluded attempt. It returns the scope and the workers whose failure
// stands in that attempt, none of them where nothing is left to re-run.
//
// rerunScopeJob re-runs the first of them alone. rerunScopeFailedJobs is for the cases
// where every other failed job of the attempt was cancelled, so that call re-runs no job
// that ended failure on its own, or where the list does not say which jobs they are: an
// unread or empty one, or a worker matched by a name that does not name one job.
// dependents, the red dependents of a job re-run before, are no other failure.
//
// rerun-failed-jobs takes the cancelled jobs too, which are a fail-fast matrix's siblings
// of a killed leg. The list spells a job past its own timeout the same way, and that one
// is re-run with them.
func planJobRerun(workers []killedWorker, jobs []workflowJobSummary, dependents map[int64]bool) (scope string, standing []killedWorker) {
	if len(jobs) == 0 || len(workers) == 0 {
		return rerunScopeFailedJobs, workers
	}
	killed := map[int64]bool{}
	for _, w := range workers {
		j, found, ambiguous := w.find(jobs)
		if ambiguous {
			return rerunScopeFailedJobs, workers
		}
		if found && j.failed() {
			killed[j.ID] = true
			standing = append(standing, w)
		}
	}
	if anotherFailed(jobs, killed, dependents) {
		return rerunScopeJob, standing
	}
	return rerunScopeFailedJobs, standing
}

// anotherFailed reports whether a job of the attempt failed that is neither killed, a red
// dependent nor cancelled.
func anotherFailed(jobs []workflowJobSummary, killed, dependents map[int64]bool) bool {
	return slices.ContainsFunc(jobs, func(j workflowJobSummary) bool {
		return j.failed() && j.Conclusion != runConclusionCancelled && !killed[j.ID] && !dependents[j.ID]
	})
}

// onlyCancelledLeft reports whether what a run needs re-run is its cancelled jobs and their
// red dependents.
func onlyCancelledLeft(jobs []workflowJobSummary, dependents map[int64]bool) bool {
	return cancelledBeside(jobs, nil) && !anotherFailed(jobs, nil, dependents)
}

// cancelledBeside reports whether GitHub cancelled a job that ran on none of workers.
func cancelledBeside(jobs []workflowJobSummary, workers []killedWorker) bool {
	return slices.ContainsFunc(jobs, func(j workflowJobSummary) bool {
		return j.Conclusion == runConclusionCancelled && !slices.ContainsFunc(workers, func(w killedWorker) bool {
			return w.Runner == j.RunnerName && w.Runner != "" || w.Runner == "" && w.Job == j.Name
		})
	})
}

// startedJob is an accepted single-job re-run: the attempt GitHub started for it, and the
// job's display name.
type startedJob struct {
	attempt int
	job     string
}

// redDependents returns the IDs of the jobs of now, run's latest attempt, that failed in it
// and in the attempt before because job did: a single-job re-run of job started the attempt,
// and GitHub re-runs a job's dependents with it and nothing else (the design's M6). One red
// in the latest alone, such as a job its failed need had kept skipped, failed on its own. It
// is nil where names do not tell the jobs apart.
func redDependents(job string, run runState, before, now []workflowJobSummary) map[int64]bool {
	named, red, ran := map[string]int{}, map[string]int{}, 0
	for _, j := range before {
		named[j.Name]++
		if j.Conclusion == runConclusionFailure {
			red[j.Name]++
		}
	}
	for _, j := range now {
		if j.Name == job && !j.carried(run) {
			ran++
		}
	}
	if ran != 1 {
		return nil
	}
	ids := map[int64]bool{}
	for _, j := range now {
		if j.Name != job && !j.carried(run) && j.Conclusion == runConclusionFailure &&
			named[j.Name] == 1 && red[j.Name] == 1 {
			ids[j.ID] = true
		}
	}
	return ids
}

// rerunJob re-runs the job d's worker ran, by its ID in attempt, the run's latest. GitHub
// re-runs the job's dependents with it, whether or not they had failed.
func (p *Provisioner) rerunJob(ctx context.Context, owner, repo, runID string, attempt int, d rerunDisruption, log *slog.Logger) error {
	jobs, err := p.attemptJobs(ctx, owner, repo, runID, attempt)
	if err != nil {
		return fmt.Errorf("list the run's jobs to re-run one: %w", err)
	}
	j, found, _ := killedWorker{Job: d.job, Runner: d.runner}.find(jobs)
	if !found || j.ID == 0 {
		return fmt.Errorf("the jobs list of attempt %d names no job for runner %q", attempt, d.runner)
	}
	log.Info("re-running one job of the run", "runID", runID, "runAttempt", attempt, "job", j.Name, "jobID", j.ID, "runner", d.runner)
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/rerun",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), j.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build job rerun request: %w", err)
	}
	if err := p.sendRerun(ctx, req); err != nil {
		return err
	}
	// GitHub takes an ID of the latest attempt alone, and none while the run is in progress,
	// so the attempt it started is the next.
	if d.onJobStarted != nil {
		d.onJobStarted(attempt+1, j.Name)
	}
	return nil
}
