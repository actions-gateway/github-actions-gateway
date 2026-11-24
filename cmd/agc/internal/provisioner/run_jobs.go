package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"slices"
	"strconv"
	"time"
)

// Two jobs of one run may share a display name and a completion carries no attempt, so
// only the run's job list tells a job's re-run from the other one (Q1375,
// docs/design/04-operational-flows.md §4.2).

const (
	// runJobsBudget bounds one runJobs, which holds the listener.
	runJobsBudget = 10 * time.Second
	// runJobsPageSize is GitHub's maximum, and runJobsMaxPages the most pages read.
	runJobsPageSize = 100
	runJobsMaxPages = 10
)

// runJob is one job of one attempt of a run. A job an attempt did not re-run is listed in
// it again on the runner that ran it. RunnerName is "" until the job has a runner.
type runJob struct {
	Name       string `json:"name"`
	RunnerName string `json:"runner_name"`
	RunAttempt int    `json:"run_attempt"`
}

// reranJob reports whether the job that ran on runner is a re-run of the one that failed
// on failedRunner, given the jobs of every attempt of their run. It is where the jobs of
// its name differ between its attempt and the one before by those two alone. The earlier
// attempt has finished, so it lists every job; the later one need not yet. A job with no
// runner, a skipped one included, matches nothing in the other attempt. A failedRunner of
// "" is unknown, and pairs only a name that one job held.
func reranJob(jobs []runJob, runner, failedRunner string) bool {
	if runner == "" {
		return false
	}
	var job runJob
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
		return false
	}
	return gone[0] == failedRunner || (failedRunner == "" && len(before) == 1)
}

// runJobs calls GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs for the jobs of every
// attempt of the run.
func (p *Provisioner) runJobs(ctx context.Context, owner, repo, runID string) ([]runJob, error) {
	if err := p.githubReady(owner, repo, "run-jobs"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, runJobsBudget)
	defer cancel()
	var jobs []runJob
	for page := 1; page <= runJobsMaxPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/jobs",
			p.GitHubAPIURL,
			neturl.PathEscape(owner),
			neturl.PathEscape(repo),
			neturl.PathEscape(runID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build run-jobs request: %w", err)
		}
		req.URL.RawQuery = neturl.Values{
			"filter": {"all"}, "per_page": {strconv.Itoa(runJobsPageSize)}, "page": {strconv.Itoa(page)},
		}.Encode()
		status, body, err := p.githubSend(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("run-jobs API call: %w", err)
		}
		if status < 200 || status > 299 {
			return nil, fmt.Errorf("run-jobs API returned %d", status)
		}
		var list struct {
			Jobs []runJob `json:"jobs"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode run jobs: %w", err)
		}
		jobs = append(jobs, list.Jobs...)
		if len(list.Jobs) < runJobsPageSize {
			return jobs, nil
		}
	}
	return nil, fmt.Errorf("the run lists %d jobs or more", runJobsPageSize*runJobsMaxPages)
}
