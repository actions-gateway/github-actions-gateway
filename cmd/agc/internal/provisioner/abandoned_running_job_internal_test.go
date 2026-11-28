package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// A force-cancel ends every unfinished job of a run, so an abandoned job's run is left
// alone while another job of it is running.

// runningRunAPI fakes the run GET, the attempt's jobs list and the force-cancel POST.
type runningRunAPI struct {
	mu          sync.Mutex
	jobs        string
	jobsStatus  int
	forceCancel []string
}

func (a *runningRunAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/force-cancel"):
		a.forceCancel = append(a.forceCancel, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	case strings.HasSuffix(r.URL.Path, "/attempts/1/jobs"):
		w.WriteHeader(a.jobsStatus)
		_, _ = io.WriteString(w, a.jobs)
	default:
		_, _ = io.WriteString(w, `{"status":"in_progress","conclusion":null,"run_attempt":1}`)
	}
}

func (a *runningRunAPI) cancels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.forceCancel...)
}

// reapedWorkerOf is the worker minted for jobID of run 111, reaped while Pending.
func reapedWorkerOf(jobID string) *corev1.Pod {
	pod := mintedWorker(jobID, "myorg/repo-a", "111")
	pod.Annotations[AnnotationDeletionReason] = "pending_deadline"
	pod.Status.Phase = corev1.PodPending
	return pod
}

func abandonedFixtureOn(t *testing.T, api *runningRunAPI, pods ...*corev1.Pod) *abandonedScaleSetFixture {
	t.Helper()
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted, pods...)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	f.p.GitHubAPIURL, f.p.HTTPClient = srv.URL, srv.Client()
	return f
}

const (
	jobsStuckOnly     = `{"total_count":2,"jobs":[{"id":1,"name":"stuck","status":"queued","runner_name":""},{"id":2,"name":"quick","status":"completed","conclusion":"success","runner_name":"GitHub Actions 7"}]}`
	jobsHostedRunning = `{"total_count":2,"jobs":[{"id":1,"name":"stuck","status":"queued","runner_name":""},{"id":3,"name":"hosted","status":"in_progress","runner_name":"GitHub Actions 8"}]}`
)

// TestAbandoned_AJobRunningOnAnotherRunnerSparesTheRun: GitHub lists a job of the run in
// progress on a runner no worker of this set holds, so only the jobs list can show it.
func TestAbandoned_AJobRunningOnAnotherRunnerSparesTheRun(t *testing.T) {
	api := &runningRunAPI{jobs: jobsHostedRunning, jobsStatus: http.StatusOK}
	f := abandonedFixtureOn(t, api)

	<-f.p.RecoverAbandonedScaleSetWorker(context.Background(), f.target, reapedWorkerOf("job-a"))

	assert.Empty(t, api.cancels(), "the force-cancel would end the hosted job")
	_, waiting := f.p.peekAbandonedRerun(abandonedRerunKey{owner: f.target.Key(), runID: "111"})
	assert.False(t, waiting, "nothing was cancelled, so nothing waits to be re-run")
	require.Equal(t, []string{"EvictionRerunWithheld"}, f.target.events)
	assert.Contains(t, f.target.messages[0], `job "hosted" of the run is running`)
	assert.Zero(t, f.p.selfCancelledAttempt(context.Background(), f.target, "111"),
		"no cancel of GAG's is on record, so a user's later cancel is not taken for one")
}

// TestAbandoned_AJobRunningOnThisSetSparesTheRun: the set's own workers show the running
// job, and GitHub is not asked.
func TestAbandoned_AJobRunningOnThisSetSparesTheRun(t *testing.T) {
	api := &runningRunAPI{jobs: jobsStuckOnly, jobsStatus: http.StatusOK}
	f := abandonedFixtureOn(t, api, startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-b"))
	target := &inFlightTarget{stubTarget: f.target, jobs: []OrphanedWorker{
		inFlight("job-a", "myorg/repo-a", "111"),
		inFlight("job-b", "myorg/repo-a", "111"),
	}}

	<-f.p.RecoverAbandonedScaleSetWorker(context.Background(), target, reapedWorkerOf("job-a"))

	assert.Empty(t, api.cancels(), "the force-cancel would end job-b")
	require.Equal(t, []string{"EvictionRerunWithheld"}, f.target.events)
	assert.Contains(t, f.target.messages[0], "another job of the run is running")
}

// TestAbandoned_NoJobRunningStillForceCancels is the control, and the unread list beside
// it: neither withholds the cancel.
func TestAbandoned_NoJobRunningStillForceCancels(t *testing.T) {
	for name, api := range map[string]*runningRunAPI{
		"only the abandoned job is unfinished": {jobs: jobsStuckOnly, jobsStatus: http.StatusOK},
		"the jobs list cannot be read":         {jobs: `{}`, jobsStatus: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			f := abandonedFixtureOn(t, api)

			<-f.p.RecoverAbandonedScaleSetWorker(context.Background(), f.target, reapedWorkerOf("job-a"))

			assert.Equal(t, []string{"/repos/myorg/repo-a/actions/runs/111/force-cancel"}, api.cancels())
			assert.Empty(t, f.target.events)
		})
	}
}
