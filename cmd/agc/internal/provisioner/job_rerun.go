package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// Job-granular re-runs (Q1319). rerun-failed-jobs re-runs every failed job of the run, so
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
//
// rerun-failed-jobs takes the cancelled jobs too, which are a fail-fast matrix's siblings
// of a killed leg. The list spells a job past its own timeout the same way, and that one
// is re-run with them.
func planJobRerun(workers []killedWorker, jobs []workflowJobSummary) (scope string, standing []killedWorker) {
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
	for _, j := range jobs {
		if j.failed() && j.Conclusion != runConclusionCancelled && !killed[j.ID] {
			return rerunScopeJob, standing
		}
	}
	return rerunScopeFailedJobs, standing
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
	return p.sendRerun(ctx, req)
}
