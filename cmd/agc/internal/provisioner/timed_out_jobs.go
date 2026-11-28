package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

// GitHub completes a job that reached its timeout-minutes as canceled, as it does a user's
// cancel, and tells the two apart only in an annotation on the job's check run. A cancelled
// job is held in memory, and its annotations are read only once a later attempt of it
// succeeds (docs/design/04-operational-flows.md §4.2).

const (
	// maxCancelHolds bounds the cancelled jobs held, over every set of this process.
	maxCancelHolds = 4096
	// annotationsBudget bounds one batch's annotation reads.
	annotationsBudget = 30 * time.Second
	// conclusionCancelled is the jobs list's spelling of jobResultCanceled.
	conclusionCancelled = "cancelled"
	// annotationsDenied is GitHub's message to an App that lacks the permission, from
	// recall rather than measured here.
	annotationsDenied = "Resource not accessible by integration"
)

// timedOutMessage is how GitHub annotates a job that reached its limit. It is anchored
// because a job writes annotations of its own. The first form was read on a scale set's
// jobs (run 45052015868) and the second, older one is quoted in public issues.
var timedOutMessage = regexp.MustCompile(`^The job (running on runner .+ )?has exceeded the maximum execution time of `)

var (
	// errAnnotationsDenied: the App lacks Checks: Read, so no read would be answered.
	errAnnotationsDenied = errors.New("the GitHub App may not read check runs")
	// errAnnotationsLimited: GitHub refuses this read for a reason the next would meet too.
	errAnnotationsLimited = errors.New("GitHub refused to list annotations")
)

// cancelHold is one cancelled job that a runner had started.
type cancelHold struct {
	jobID, runner, event string
	at                   time.Time
	// settled: its annotations were read, so a redelivered completion changes nothing.
	settled bool
}

// cancelHolds is the latest cancelled job of each set, run and job name.
type cancelHolds struct {
	mu    sync.Mutex
	byJob map[string]*cancelHold
}

func cancelHoldKey(set types.NamespacedName, fkey string) string {
	return set.String() + "/" + fkey
}

// put holds h, in place of an earlier attempt's. A redelivered cancel keeps the hold it
// made.
func (x *cancelHolds) put(set types.NamespacedName, fkey string, h *cancelHold) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key := cancelHoldKey(set, fkey)
	if held := x.byJob[key]; held != nil && held.jobID == h.jobID {
		return
	}
	if x.byJob == nil {
		x.byJob = map[string]*cancelHold{}
	}
	putBounded(x.byJob, key, h, maxCancelHolds, func(h *cancelHold) time.Time { return h.at })
}

// find returns the unsettled hold of fkey on set, nil where there is none as recent as a
// failure record may be.
func (x *cancelHolds) find(set types.NamespacedName, fkey string, now time.Time) *cancelHold {
	x.mu.Lock()
	defer x.mu.Unlock()
	h := x.byJob[cancelHoldKey(set, fkey)]
	if h == nil || h.settled || now.Sub(h.at) > failureRecordTTL {
		return nil
	}
	return h
}

// settle reports whether h was not settled yet, and settles it.
func (x *cancelHolds) settle(h *cancelHold) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	first := !h.settled
	h.settled = true
	return first
}

// heldRerun is a succeeded completion that re-ran a held cancel.
type heldRerun struct {
	c        *jobCompletion
	checkRun int64
	o        completionPairing
}

// heldRerunOf returns c as the re-run of the cancel it holds, given its run's jobs and
// GAG's re-runs of the run, or nil where it is not known to be one. The cancelled job is
// read where it ran: a later attempt lists a job it did not re-run again, under another id.
func heldRerunOf(c *jobCompletion, jobs []workflowJobSummary, r *rerunRecord) *heldRerun {
	if c.hold == nil {
		return nil
	}
	attempt := reranIn(jobs, c.job.RunnerName, c.hold.runner)
	if attempt == 0 {
		return nil
	}
	var cancelled workflowJobSummary
	for _, j := range jobs {
		if j.RunnerName == c.hold.runner && (cancelled.RunAttempt == 0 || j.RunAttempt < cancelled.RunAttempt) {
			cancelled = j
		}
	}
	if cancelled.Conclusion != conclusionCancelled {
		return nil
	}
	h := &heldRerun{c: c, checkRun: cancelled.ID, o: completionPairing{paired: true, timedOut: true, event: c.hold.event}}
	h.o.trigger, h.o.cause = pairFailure(r, attempt-1, c.jobName)
	h.o.flaky = h.o.trigger != rerunTriggerGAG
	return h
}

// pairTimedOut counts the re-runs of held cancels that GitHub says had timed out, one read
// each and all of them within annotationsBudget.
func (p *Provisioner) pairTimedOut(ctx context.Context, reruns []*heldRerun, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, annotationsBudget)
	defer cancel()
	for i, h := range reruns {
		if ctx.Err() != nil {
			log.Warn("ran out of time to read whether a run's cancelled jobs had timed out; their re-runs go uncounted", "unread", len(reruns)-i)
			return
		}
		c := h.c
		timedOut, err := p.jobTimedOut(ctx, c.job.Owner, c.job.Repository, h.checkRun)
		if err != nil {
			p.completionLog(c).Warn("could not read whether a cancelled job that was re-run had timed out; its re-run goes uncounted",
				"checkRun", h.checkRun, "error", err)
		}
		switch {
		case errors.Is(err, errAnnotationsDenied):
			c.target.RecordEvent(corev1.EventTypeNormal, "CancelledJobUnread", "DetectFlakyJob",
				fmt.Sprintf("Job %q of run %s was cancelled and then succeeded when re-run, and GitHub would not say whether it had timed out, so it is not counted; grant the GitHub App Checks: Read to have such jobs counted.",
					c.jobName, c.runID))
			return
		case errors.Is(err, errAnnotationsLimited):
			return
		case err != nil:
		case p.cancelHolds.settle(c.hold) && timedOut:
			p.reportRerunJob(c.target, c.runID, c.jobName, c.result, h.o, p.completionLog(c))
		}
	}
}

// jobTimedOut calls GET /repos/{owner}/{repo}/check-runs/{check_run_id}/annotations, one
// page of which holds every annotation a job may have, and reports whether GitHub said the
// job had exceeded its limit. The annotations are the job's own output too, so an error
// never carries a body that was answered.
func (p *Provisioner) jobTimedOut(ctx context.Context, owner, repo string, checkRun int64) (bool, error) {
	if err := p.githubReady(owner, repo, "check-run annotations"); err != nil {
		return false, err
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d/annotations",
		p.GitHubAPIURL, neturl.PathEscape(owner), neturl.PathEscape(repo), checkRun)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"?per_page=100", nil)
	if err != nil {
		return false, fmt.Errorf("build check-run annotations request: %w", err)
	}
	status, body, err := p.githubSend(ctx, req)
	if err != nil {
		return false, fmt.Errorf("check-run annotations API call: %w", err)
	}
	if status < 200 || status > 299 {
		err := fmt.Errorf("check-run annotations API returned %d: %s", status, githubapp.SanitizeBody(bytes.TrimSpace(body), 512))
		switch {
		case status == http.StatusForbidden && strings.Contains(string(body), annotationsDenied):
			err = fmt.Errorf("%w: %w", errAnnotationsDenied, err)
		case status == http.StatusForbidden || status == http.StatusTooManyRequests:
			err = fmt.Errorf("%w: %w", errAnnotationsLimited, err)
		}
		return false, err
	}
	var annotations []struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &annotations); err != nil {
		return false, errors.New("decode check-run annotations: not a list of annotations")
	}
	for _, a := range annotations {
		if timedOutMessage.MatchString(a.Message) {
			return true, nil
		}
	}
	return false, nil
}
