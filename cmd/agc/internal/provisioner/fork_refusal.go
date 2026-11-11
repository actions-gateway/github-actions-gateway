package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ErrForkJobRefused marks a job RefuseForkJob refused: it is a fork pull request's, on a
// set that does not accept them, and its run has been force-cancelled.
var ErrForkJobRefused = errors.New("provisioner: refused a fork pull request job")

// IsForkJobRefused reports whether err is (or wraps) ErrForkJobRefused.
func IsForkJobRefused(err error) bool { return errors.Is(err, ErrForkJobRefused) }

// forkRefusalCheckName is the check run a refusal writes on the pull request's head.
const forkRefusalCheckName = "actions-gateway: fork pull request refused"

// mergeCommitEvent reports the events whose run executes the pull request's merge
// commit, so whose workflow file, runs-on included, carries the fork's edits.
// pull_request_target runs the base repository's workflow and is deliberately absent.
func mergeCommitEvent(event string) bool {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return true
	}
	return false
}

// RefuseForkJob decides, before a runner is minted, whether job is a fork pull
// request's and so refused on target, a set that does not accept them (Q1330). It
// returns nil to admit the job, an error wrapping ErrForkJobRefused once the job's run
// has been force-cancelled, and any other error when the decision or the cancel could
// not be made, which the caller holds and retries rather than admitting.
//
// A job whose message names an event outside mergeCommitEvent is admitted with no
// call. One naming none is read like a pull request job, since admitting it unread is
// the direction that fails open. The run's head repository is compared with the job's
// own owner/repo by name, case-insensitively; the run's fork flag is not read, because
// it is true for every pull request on a repository that is itself a fork.
func (p *Provisioner) RefuseForkJob(ctx context.Context, target Target, job ScaleSetJob) error {
	if job.EventName != "" && !mergeCommitEvent(job.EventName) {
		return nil
	}
	log := p.logForKey(target.Key()).With("jobID", job.JobID, "runID", job.RunID)
	if job.Owner == "" || job.Repository == "" || job.RunID == "" || job.RunID == "0" {
		// Nothing to read and nothing to cancel: the job gets no runner and waits for
		// GitHub's queue limit.
		log.Warn("refusing a job with no run identity on a set that refuses fork pull requests; its run cannot be read or cancelled")
		target.RecordEvent(corev1.EventTypeWarning, "ForkJobRefused", "ProvisionWorker",
			"refused a job whose assignment carries no run identity, because this set refuses fork pull request jobs (spec.forkPullRequests) and the job's source cannot be read; the job is not cancelled and waits until GitHub ends it")
		return fmt.Errorf("%w: the assignment carries no run identity", ErrForkJobRefused)
	}

	run, err := p.runConclusion(ctx, job.Owner, job.Repository, job.RunID)
	if err != nil {
		return fmt.Errorf("read run %s to check for a fork pull request: %w", job.RunID, err)
	}
	if job.EventName == "" && !mergeCommitEvent(run.event) {
		return nil
	}
	if strings.EqualFold(run.headRepo, job.Owner+"/"+job.Repository) {
		return nil
	}

	head := run.headRepo
	if head == "" {
		head = "an unknown repository"
	}
	if run.status == runStatusCompleted {
		// Another of the run's jobs was refused first, or the run ended some other way:
		// there is nothing left to cancel or explain, and a cancel would be refused.
		log.Info("refused a fork pull request job whose run has already concluded",
			"headRepository", run.headRepo, "conclusion", run.conclusion)
		return fmt.Errorf("%w: head repository %q, run already %s", ErrForkJobRefused, run.headRepo, run.conclusion)
	}
	if err := p.forceCancelRun(ctx, job.Owner, job.Repository, job.RunID); err != nil {
		return fmt.Errorf("force-cancel the run of a refused fork pull request job: %w", err)
	}
	key := target.Key()
	log.Info("refused a fork pull request job and force-cancelled its run",
		"headRepository", run.headRepo, "repository", job.Owner+"/"+job.Repository)
	target.RecordEvent(corev1.EventTypeWarning, "ForkJobRefused", "ProvisionWorker",
		fmt.Sprintf("refused a job from a fork pull request (head repository %s, run %s/%s#%s) and force-cancelled its run, because spec.forkPullRequests is not Accept",
			head, job.Owner, job.Repository, job.RunID))

	if run.headSHA != "" {
		summary := fmt.Sprintf("RunnerSet `%s/%s` refuses jobs from fork pull requests, so the run was cancelled before any runner started. "+
			"The workflow's `runs-on` named this set's label and the pull request comes from %s. "+
			"A maintainer who wants this set to run fork pull requests sets `spec.forkPullRequests: Accept` on it.",
			key.Namespace, key.Name, head)
		if err := p.createFailedCheckRun(ctx, job.Owner, job.Repository, run.headSHA, summary); err != nil {
			// Best-effort: the refusal has happened, and the App may simply not hold
			// Checks: Read and write.
			log.Info("could not write the fork refusal check run; the cancelled run shows no reason", "error", err)
			target.RecordEvent(corev1.EventTypeNormal, "ForkRefusalCheckRunSkipped", "ProvisionWorker",
				fmt.Sprintf("could not write a check run explaining the fork pull request refusal on run %s, so the contributor sees only a cancelled run; grant the GitHub App Checks: Read and write to enable it: %v", job.RunID, err))
		}
	}
	return fmt.Errorf("%w: head repository %q", ErrForkJobRefused, run.headRepo)
}

// createFailedCheckRun calls POST /repos/{owner}/{repo}/check-runs, writing a completed,
// failed check run named forkRefusalCheckName on headSHA. GitHub answers 201 Created,
// and 403 when the installation lacks Checks: Read and write.
func (p *Provisioner) createFailedCheckRun(ctx context.Context, owner, repo, headSHA, summary string) error {
	if err := p.githubReady(owner, repo, "check-run"); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"name":       forkRefusalCheckName,
		"head_sha":   headSHA,
		"status":     "completed",
		"conclusion": "failure",
		"output": map[string]string{
			"title":   "Fork pull request jobs are refused on this runner set",
			"summary": summary,
		},
	})
	if err != nil {
		return fmt.Errorf("encode check run: %w", err)
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/check-runs",
		p.GitHubAPIURL,
		neturl.PathEscape(owner),
		neturl.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build check-run request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	status, resp, err := p.githubSend(ctx, req)
	if err != nil {
		return fmt.Errorf("check-run API call: %w", err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("check-run API returned %d: %s", status, strings.TrimSpace(string(resp)))
	}
	return nil
}
