package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Q1375: two jobs of one run can share a display name, and the run's job list tells a
// re-run from the other one.

// runJobsStub answers a run's job list, and counts the reads.
type runJobsStub struct {
	mu     sync.Mutex
	jobs   []map[string]any
	status int
	gets   int
	// slowest is the longest a read was allowed to take.
	slowest time.Duration
	query   []string
}

func (s *runJobsStub) set(jobs []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = jobs
}

// serve points p's GitHub API at s.
func (s *runJobsStub) serve(p *Provisioner) {
	p.GitHubAPIURL, p.HTTPClient = "https://github.test", &http.Client{Transport: s}
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
}

func (s *runJobsStub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if deadline, ok := r.Context().Deadline(); !ok {
		s.slowest = time.Hour
	} else {
		s.slowest = max(s.slowest, time.Until(deadline))
	}
	s.query = append(s.query, r.URL.Path+"?"+r.URL.RawQuery)
	w := httptest.NewRecorder()
	if s.status != 0 {
		w.WriteHeader(s.status)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	from := min((page-1)*size, len(s.jobs))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_count": len(s.jobs), "jobs": s.jobs[from:min(from+size, len(s.jobs))]})
	return w.Result(), nil
}

// listed is a job as GitHub lists it in one attempt of its run, on runner.
func listed(attempt int, name, runner string) map[string]any {
	return map[string]any{"name": name, "runner_name": runner, "run_attempt": attempt, "conclusion": "success"}
}

// The runners of run 45052015868, the measured run whose jobs share a name: the red twin
// and the green one, and the red twin's re-run.
const (
	redTwin      = "gag-ci-scaleset-82a2530d-68c0-562b-a11c-3b77eff9662f"
	greenTwin    = "gag-ci-scaleset-abe6cad3-432c-54ac-b3f9-52bfb41b8159"
	redTwinRerun = "gag-ci-scaleset-ee99fae0-6d7f-5751-98dc-c2ff56d5ad96"
)

// capturedJobs is the measured run's job list as it stood through attempt n.
func capturedJobs(t *testing.T, n int) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/run_45052015868_jobs.json")
	require.NoError(t, err)
	var attempts map[string]struct {
		Jobs []map[string]any `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(raw, &attempts))
	var jobs []map[string]any
	for attempt := 1; attempt <= n; attempt++ {
		jobs = append(jobs, attempts[strconv.Itoa(attempt)].Jobs...)
	}
	require.Len(t, jobs, 5*n)
	return jobs
}

// twinJob is a completion of the measured run's shared-name job on runner.
func twinJob(id, runner string) ScaleSetJob {
	return ScaleSetJob{JobID: id, RunnerName: runner, Owner: "org", Repository: "repo", RunID: "900", JobName: "build", EventName: "push"}
}

func TestReranJob(t *testing.T) {
	job := func(attempt int, name, runner string) runJob {
		return runJob{Name: name, RunnerName: runner, RunAttempt: attempt}
	}
	for _, tc := range []struct {
		name           string
		jobs           []runJob
		runner, failed string
		want           bool
	}{
		{"its re-run", []runJob{job(1, "build", "r1"), job(1, "lint", "r0"), job(2, "build", "r2"), job(2, "lint", "r3")}, "r2", "r1", true},
		{"a job sharing its name, in the attempt it failed in", []runJob{job(1, "build", "r1"), job(1, "build", "r2")}, "r2", "r1", false},
		{"its re-run beside a twin not re-run", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r0"), job(2, "build", "r2")}, "r2", "r1", true},
		{"the twin re-run alone", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r1"), job(2, "build", "r2")}, "r2", "r1", false},
		{"both re-run, the other queued", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", false},
		{"both re-run, the other not listed yet", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2")}, "r2", "r1", false},
		{"both re-run, the other cancelled without a runner", []runJob{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", false},
		{"both re-run, the other cancelled without a runner the first time", []runJob{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2")}, "r2", "r1", false},
		{"a twin skipped the first time, its re-run not listed yet", []runJob{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2")}, "r2", "r1", false},
		{"a twin skipped the first time, its re-run in progress", []runJob{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "r3")}, "r2", "r1", false},
		{"its re-run beside a job first listed with it", []runJob{job(1, "build", "r1"), job(2, "build", "r2"), job(2, "build", "r3")}, "r2", "r1", false},
		{"a twin skipped both times, or the two traded places", []runJob{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", false},
		{"its re-run, listed again by a later attempt", []runJob{job(1, "build", "r1"), job(2, "build", "r2"), job(3, "build", "r2")}, "r2", "r1", true},
		{"its re-run, listed newest attempt first", []runJob{job(3, "build", "r2"), job(2, "build", "r2"), job(1, "build", "r1")}, "r2", "r1", true},
		{"its re-run after an attempt that did not re-run it", []runJob{job(1, "build", "r1"), job(2, "build", "r1"), job(3, "build", "r2")}, "r2", "r1", true},
		{"the re-run of an attempt that is not recorded", []runJob{job(1, "build", "r1"), job(2, "build", "r2"), job(3, "build", "r3")}, "r3", "r1", false},
		{"a twin carried under another job's name", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2"), job(2, "lint", "r0")}, "r2", "r1", false},
		{"its re-run beside a queued job first listed with it", []runJob{job(1, "build", "r1"), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", false},
		{"a completion not listed", []runJob{job(1, "build", "r1")}, "r2", "r1", false},
		{"a completion with no runner", []runJob{job(1, "build", "r1"), job(2, "build", "")}, "", "r1", false},
		{"a record with no runner, its name held by one job", []runJob{job(1, "build", "r1"), job(2, "build", "r2")}, "r2", "", true},
		{"a record with no runner, beside a queued job first listed with its re-run", []runJob{job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "", false},
		{"a record with no runner, its name shared", []runJob{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r0"), job(2, "build", "r2")}, "r2", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reranJob(tc.jobs, tc.runner, tc.failed))
		})
	}
}

// TestFlakyJobs_AJobSharingANameIsNotItsReRun replays run 45052015868, where one of two
// jobs named alike failed and the other succeeded, against GitHub's own job lists: the
// second job pairs with nothing, and the first one's re-run still pairs.
func TestFlakyJobs_AJobSharingANameIsNotItsReRun(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	gh := &runJobsStub{jobs: capturedJobs(t, 1)}
	gh.serve(p)

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, twinJob("red", redTwin), "failed"))
	assert.Zero(t, gh.gets, "a first failure reads nothing")
	recorded := readFailures(t, p, target)["900/build"]
	require.NotNil(t, recorded)

	green := scaleSetWorker(greenTwin, corev1.PodFailed, "Evicted")
	<-p.SnapshotScaleSetWorker(ctx, nil, green, false)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, twinJob("green", greenTwin), jobResultSucceeded))
	assert.Equal(t, 1, gh.gets)
	assert.Equal(t, recorded, readFailures(t, p, target)["900/build"], "the record is left as it was")
	assert.Zero(t, rerunCount(p, rerunTriggerOther), "nobody re-ran anything")
	assert.Zero(t, flaky(p, rerunTriggerOther))
	assert.Empty(t, target.events, "a success is no failed job, whatever its worker shows")

	gh.set(capturedJobs(t, 2))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, twinJob("red-2", redTwinRerun), "failed"))
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther), "the red job's re-run pairs")
	again := readFailures(t, p, target)["900/build"]
	assert.Equal(t, []string{"red-2", redTwinRerun}, []string{again.JobID, again.Runner})
	assert.Equal(t, recorded.FailedAt, again.FailedAt)
	assert.Equal(t, []string{"/repos/org/repo/actions/runs/900/jobs?filter=all&page=1&per_page=100"}, gh.query[:1])
}

// TestFlakyJobs_ASkippedTwinThatRunsIsNotTheReRun: a job skipped beside its failing twin
// runs in the re-run and succeeds first, so it is not the failed one's re-run, which then
// fails.
func TestFlakyJobs_ASkippedTwinThatRunsIsNotTheReRun(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	first := []map[string]any{listed(1, "build", "runner-1"), {"name": "build", "run_attempt": 1, "conclusion": "skipped", "runner_name": nil}}
	gh := &runJobsStub{jobs: first}
	gh.serve(p)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	recorded := readFailures(t, p, target)["900/build"]

	gh.set(append(first, listed(2, "build", "runner-2"), listed(2, "build", "runner-3")))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(3), "failed"))
	assert.Equal(t, recorded, readFailures(t, p, target)["900/build"])
	assert.Zero(t, rerunCount(p, rerunTriggerOther))
	assert.Zero(t, flaky(p, rerunTriggerOther))
	assert.Empty(t, target.events)
}

// TestFlakyJobs_AFailedTwinIsReportedButNotRecorded: the record holds the first failure
// under a name, and the second job's is logged and its fault reported.
func TestFlakyJobs_AFailedTwinIsReportedButNotRecorded(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	gh := &runJobsStub{jobs: []map[string]any{listed(1, "build", "runner-1"), listed(1, "build", "runner-2")}}
	gh.serve(p)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	before := readLedgerData(t, c, target)

	worker := scaleSetWorker("runner-2", corev1.PodRunning, "")
	require.NoError(t, c.Create(ctx, worker))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), "failed"))
	finished := finishWorker(worker, time.Now())
	finished.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated("dind", "OOMKilled", 137, time.Now().Add(-time.Minute))}
	<-p.SnapshotScaleSetWorker(ctx, nil, finished, false)

	assert.Equal(t, before, readLedgerData(t, c, target))
	assert.Zero(t, rerunCount(p, rerunTriggerOther))
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)
	assert.Empty(t, p.workerSnapshots.recorded)
}

// TestFlakyJobs_AnUnreadableRunPairsNothing: an undercount is acceptable and a false
// flake is not, so a match GitHub cannot settle changes nothing.
func TestFlakyJobs_AnUnreadableRunPairsNothing(t *testing.T) {
	for name, prepare := range map[string]func(*runJobsStub, *Provisioner, *ScaleSetJob){
		"an error status": func(gh *runJobsStub, _ *Provisioner, _ *ScaleSetJob) { gh.status = http.StatusBadGateway },
		"more jobs than it reads": func(gh *runJobsStub, _ *Provisioner, _ *ScaleSetJob) {
			for len(gh.jobs) < runJobsPageSize*runJobsMaxPages {
				gh.jobs = append(gh.jobs, listed(1, "other", "r"))
			}
		},
		"no repository": func(_ *runJobsStub, _ *Provisioner, job *ScaleSetJob) { job.Owner, job.Repository = "", "" },
		"no runner":     func(_ *runJobsStub, _ *Provisioner, job *ScaleSetJob) { job.RunnerName = "" },
		"no endpoint":   func(_ *runJobsStub, p *Provisioner, _ *ScaleSetJob) { p.GitHubAPIURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, p, target := flakyFixture(t)
			gh := &runJobsStub{jobs: []map[string]any{listed(1, "build", "runner-1"), listed(2, "build", "runner-2")}}
			gh.serve(p)
			require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
			before := readLedgerData(t, c, target)
			job := buildJob(2)
			prepare(gh, p, &job)
			var log bytes.Buffer
			p.Log = slog.New(slog.NewTextHandler(&log, nil))
			require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, job, jobResultSucceeded))
			assert.Equal(t, before, readLedgerData(t, c, target))
			assert.Zero(t, rerunCount(p, rerunTriggerOther))
			assert.Zero(t, flaky(p, rerunTriggerOther))
			assert.Empty(t, target.events)
			if job.RunnerName == "" {
				assert.Zero(t, gh.gets, "nothing to look for")
			} else {
				assert.Contains(t, log.String(), "could not read the run's jobs")
			}
		})
	}
}

// TestRunJobs_ReadsEveryPage: a re-run past the first page is still found.
func TestRunJobs_ReadsEveryPage(t *testing.T) {
	p := flakyProvisioner(nil)
	gh := &runJobsStub{}
	gh.serve(p)
	for len(gh.jobs) < runJobsPageSize {
		gh.jobs = append(gh.jobs, listed(1, "other", "r"))
	}
	gh.jobs = append(gh.jobs, listed(2, "build", "runner-2"))
	jobs, err := p.runJobs(context.Background(), "org", "repo", "900")
	require.NoError(t, err)
	require.Len(t, jobs, runJobsPageSize+1)
	assert.Equal(t, "runner-2", jobs[runJobsPageSize].RunnerName)
	assert.Equal(t, 2, gh.gets)
	assert.LessOrEqual(t, gh.slowest, runJobsBudget)
}
