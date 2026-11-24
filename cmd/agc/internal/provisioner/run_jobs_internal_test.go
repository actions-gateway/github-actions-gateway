package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q1375: two jobs of one run can share a display name, and the run's job list tells a
// re-run from the other one.

// runJobsStub answers a run's job list, and counts the reads.
type runJobsStub struct {
	mu     sync.Mutex
	jobs   []map[string]any
	status int
	gets   int
	// unlisted is how many more jobs the run has than the pages serve.
	unlisted int
	// hang: a read is never answered.
	hang bool
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
	if s.hang {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	w := httptest.NewRecorder()
	if s.status != 0 {
		w.WriteHeader(s.status)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	from := min((page-1)*size, len(s.jobs))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_count": len(s.jobs) + s.unlisted, "jobs": s.jobs[from:min(from+size, len(s.jobs))]})
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

func TestReranIn(t *testing.T) {
	job := func(attempt int, name, runner string) workflowJobSummary {
		return workflowJobSummary{Name: name, RunnerName: runner, RunAttempt: attempt}
	}
	for _, tc := range []struct {
		name           string
		jobs           []workflowJobSummary
		runner, failed string
		want           int
	}{
		{"its re-run", []workflowJobSummary{job(1, "build", "r1"), job(1, "lint", "r0"), job(2, "build", "r2"), job(2, "lint", "r3")}, "r2", "r1", 2},
		{"a job sharing its name, in the attempt it failed in", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r2")}, "r2", "r1", 0},
		{"its re-run beside a twin not re-run", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r0"), job(2, "build", "r2")}, "r2", "r1", 2},
		{"the twin re-run alone", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r1"), job(2, "build", "r2")}, "r2", "r1", 0},
		{"both re-run, the other queued", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", 0},
		{"both re-run, the other not listed yet", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2")}, "r2", "r1", 0},
		{"both re-run, the other cancelled without a runner", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", 0},
		{"both re-run, the other cancelled without a runner the first time", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2")}, "r2", "r1", 0},
		{"a twin skipped the first time, its re-run not listed yet", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2")}, "r2", "r1", 0},
		{"a twin skipped the first time, its re-run in progress", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "r3")}, "r2", "r1", 0},
		{"its re-run beside a job first listed with it", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r2"), job(2, "build", "r3")}, "r2", "r1", 0},
		{"a twin skipped both times, or the two traded places", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", 0},
		{"its re-run, listed again by a later attempt", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r2"), job(3, "build", "r2")}, "r2", "r1", 2},
		{"its re-run, listed newest attempt first", []workflowJobSummary{job(3, "build", "r2"), job(2, "build", "r2"), job(1, "build", "r1")}, "r2", "r1", 2},
		{"its re-run after an attempt that did not re-run it", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r1"), job(3, "build", "r2")}, "r2", "r1", 3},
		{"the re-run of an attempt that is not recorded", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r2"), job(3, "build", "r3")}, "r3", "r1", 0},
		{"a twin carried under another job's name", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r2"), job(2, "lint", "r0")}, "r2", "r1", 0},
		{"its re-run beside a queued job first listed with it", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r2"), job(2, "build", "")}, "r2", "r1", 0},
		{"a completion not listed", []workflowJobSummary{job(1, "build", "r1")}, "r2", "r1", 0},
		{"a completion with no runner", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "")}, "", "r1", 0},
		{"a record with no runner, its name held by one job", []workflowJobSummary{job(1, "build", "r1"), job(2, "build", "r2")}, "r2", "", 2},
		{"a record with no runner, beside a queued job first listed with its re-run", []workflowJobSummary{job(1, "build", ""), job(2, "build", "r2"), job(2, "build", "")}, "r2", "", 0},
		{"a record with no runner, its name shared", []workflowJobSummary{job(1, "build", "r1"), job(1, "build", "r0"), job(2, "build", "r0"), job(2, "build", "r2")}, "r2", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reranIn(tc.jobs, tc.runner, tc.failed))
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

	completeJob(ctx, t, p, target, twinJob("red", redTwin), "failed")
	assert.Zero(t, gh.gets, "a first failure reads nothing")
	recorded := readFailures(t, p, target)["900/build"]
	require.NotNil(t, recorded)

	green := scaleSetWorker(greenTwin, corev1.PodFailed, "Evicted")
	<-p.SnapshotScaleSetWorker(ctx, nil, green, false)
	completeJob(ctx, t, p, target, twinJob("green", greenTwin), jobResultSucceeded)
	assert.Equal(t, 1, gh.gets)
	assert.Equal(t, recorded, readFailures(t, p, target)["900/build"], "the record is left as it was")
	assert.Zero(t, rerunCount(p, rerunTriggerOther), "nobody re-ran anything")
	assert.Zero(t, flaky(p, rerunTriggerOther))
	assert.Empty(t, target.events, "a success is no failed job, whatever its worker shows")

	gh.set(capturedJobs(t, 2))
	completeJob(ctx, t, p, target, twinJob("red-2", redTwinRerun), "failed")
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
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	recorded := readFailures(t, p, target)["900/build"]

	gh.set(append(first, listed(2, "build", "runner-2"), listed(2, "build", "runner-3")))
	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	completeJob(ctx, t, p, target, buildJob(3), "failed")
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
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	before := readLedgerData(t, c, target)

	worker := scaleSetWorker("runner-2", corev1.PodRunning, "")
	require.NoError(t, c.Create(ctx, worker))
	completeJob(ctx, t, p, target, buildJob(2), "failed")
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
		"an error status":        func(gh *runJobsStub, _ *Provisioner, _ *ScaleSetJob) { gh.status = http.StatusBadGateway },
		"a list that ends early": func(gh *runJobsStub, _ *Provisioner, _ *ScaleSetJob) { gh.unlisted = 1 },
		"more jobs than it reads": func(gh *runJobsStub, _ *Provisioner, _ *ScaleSetJob) {
			for len(gh.jobs) <= jobsPerPage*maxJobPages {
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
			completeJob(ctx, t, p, target, buildJob(1), "failed")
			before := readLedgerData(t, c, target)
			job := buildJob(2)
			prepare(gh, p, &job)
			var log bytes.Buffer
			p.Log = slog.New(slog.NewTextHandler(&log, nil))
			completeJob(ctx, t, p, target, job, jobResultSucceeded)
			assert.Equal(t, before, readLedgerData(t, c, target))
			assert.Zero(t, rerunCount(p, rerunTriggerOther))
			assert.Zero(t, flaky(p, rerunTriggerOther))
			assert.Empty(t, target.events)
			assert.Zero(t, p.pairings.waiting)
			assert.Empty(t, p.pairings.byRun)
			switch {
			case job.RunnerName == "":
				assert.Zero(t, gh.gets, "nothing to look for")
			case name == "more jobs than it reads":
				assert.Equal(t, 1, gh.gets, "the first page says so")
				fallthrough
			default:
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
	for len(gh.jobs) < jobsPerPage {
		gh.jobs = append(gh.jobs, listed(1, "other", "r"))
	}
	gh.jobs = append(gh.jobs, listed(2, "build", "runner-2"))
	jobs, err := p.runJobs(context.Background(), "org", "repo", "900")
	require.NoError(t, err)
	require.Len(t, jobs, jobsPerPage+1)
	assert.Equal(t, "runner-2", jobs[jobsPerPage].RunnerName)
	assert.Equal(t, 2, gh.gets)
	assert.LessOrEqual(t, gh.slowest, runJobsBudget)
}

// TestPairing_OneListPerRunAndDelay: re-running 100 of a run's 200 legs costs one list of
// its 400 rows per delay and one ledger write, and none of it holds the listener.
func TestPairing_OneListPerRunAndDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const legs, rerun = 200, 100
		ctx := context.Background()
		c, p, target := flakyFixture(t)
		p.pairingDelay = NewProvisioner(c, nil, nil).pairingDelay
		gh := &runJobsStub{}
		gh.serve(p)
		leg := func(attempt, n int) ScaleSetJob {
			job := buildJob(1)
			job.JobName, job.RunnerName = fmt.Sprintf("build (%d)", n), fmt.Sprintf("runner-%d-%d", attempt, n)
			job.JobID = job.RunnerName
			return job
		}
		for n := range legs {
			job := leg(1, n)
			gh.jobs = append(gh.jobs, listed(1, job.JobName, job.RunnerName))
			if n < rerun {
				completeJob(ctx, t, p, target, job, "failed")
			}
		}
		for n := range legs {
			job := leg(1, n)
			if n < rerun {
				job = leg(2, n)
			}
			gh.jobs = append(gh.jobs, listed(2, job.JobName, job.RunnerName))
		}
		before := readLedgerData(t, c, target)
		ledgerVersion := func() int {
			var cm corev1.ConfigMap
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetRerunLedgerConfigMapName("gpu")}, &cm))
			v, err := strconv.Atoi(cm.ResourceVersion)
			require.NoError(t, err)
			return v
		}
		version := ledgerVersion()

		start := time.Now()
		var waits []<-chan struct{}
		for n := range rerun {
			done, err := p.RecordScaleSetJobCompleted(ctx, target, leg(2, n), jobResultSucceeded)
			require.NoError(t, err)
			waits = append(waits, done)
		}
		_, err := p.RecordScaleSetJobCompleted(ctx, target, leg(2, 0), jobResultSucceeded)
		require.NoError(t, err)
		assert.Zero(t, time.Since(start), "the listener waits for no list")
		assert.Zero(t, gh.gets)
		assert.Equal(t, before, readLedgerData(t, c, target))

		for _, done := range waits {
			<-done
		}
		assert.Equal(t, defaultPairingDelay, time.Since(start))
		assert.Equal(t, 4, gh.gets)
		assert.Equal(t, float64(rerun), flaky(p, rerunTriggerOther))
		assert.Empty(t, readFailures(t, p, target))
		assert.Equal(t, 1, ledgerVersion()-version, "one write")
		assert.Zero(t, p.pairings.waiting)
	})
}

// TestPairing_AHungReadHoldsNoListener: what a list GitHub does not answer costs, which is
// its budget off the listener.
func TestPairing_AHungReadHoldsNoListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_, p, target := flakyFixture(t)
		gh := &runJobsStub{hang: true}
		gh.serve(p)
		completeJob(ctx, t, p, target, buildJob(1), "failed")
		start := time.Now()
		done, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
		require.NoError(t, err)
		cancel()
		assert.Zero(t, time.Since(start))
		<-done
		assert.Equal(t, runJobsBudget, time.Since(start), "a listener that stopped does not end the read")
	})
}

// TestPairing_WaitingCompletionsAreBounded: past the bound a completion is dropped and
// said so, and a run's batch frees its place.
func TestPairing_WaitingCompletionsAreBounded(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	var log bytes.Buffer
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	p.pairings.waiting = maxPendingPairings
	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	assert.Contains(t, log.String(), "too many completions await")
	assert.Zero(t, rerunCount(p, rerunTriggerOther))
	p.pairings.waiting--
	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther))
	assert.Equal(t, maxPendingPairings-1, p.pairings.waiting)
}

// TestPairing_ABatchPairsInArrivalOrder: a job's second and third attempts in one batch
// pair each with the one before, and neither two runs nor two sets share a batch.
func TestPairing_ABatchPairsInArrivalOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, p, target := flakyFixture(t)
		p.pairingDelay = defaultPairingDelay
		other := func(n int) ScaleSetJob {
			job := buildJob(n)
			job.RunID, job.JobID = "901", job.JobID+"-901"
			return job
		}
		for _, job := range []ScaleSetJob{buildJob(1), other(1)} {
			completeJob(ctx, t, p, target, job, "failed")
		}
		otherSet := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "cpu"}}
		completeJob(ctx, t, p, otherSet, buildJob(1), "failed")
		var waits []<-chan struct{}
		for _, c := range []struct {
			target *stubTarget
			job    ScaleSetJob
			result string
		}{{target, buildJob(2), "failed"}, {target, other(2), "failed"}, {target, buildJob(3), jobResultSucceeded},
			{target, buildJob(4), "failed"}, {otherSet, buildJob(2), "failed"}} {
			done, err := p.RecordScaleSetJobCompleted(ctx, c.target, c.job, c.result)
			require.NoError(t, err)
			waits = append(waits, done)
		}
		assert.Equal(t, []bool{false, true, true, false},
			[]bool{waits[0] == waits[1], waits[0] == waits[2], waits[0] == waits[3], waits[0] == waits[4]})
		for _, done := range waits {
			<-done
		}
		assert.Equal(t, 3.0, rerunCount(p, rerunTriggerOther))
		assert.Equal(t, 1.0, flaky(p, rerunTriggerOther))
		f := readFailures(t, p, target)
		assert.Equal(t, "job-2", readFailures(t, p, otherSet)["900/build"].JobID)
		require.Contains(t, f, "900/build")
		assert.Equal(t, "job-4", f["900/build"].JobID, "a failure after the one that passed is a first one")
		assert.Equal(t, "job-2-901", f["901/build"].JobID)
	})
}

// TestAttemptJobs_WhatALaterReadCouldAnswer: GitHub unreachable or failing is worth a
// later read, and a refusal is not.
func TestAttemptJobs_WhatALaterReadCouldAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		gh   *runJobsStub
		want bool
	}{
		"unreachable":    {gh: &runJobsStub{hang: true}, want: true},
		"a server error": {gh: &runJobsStub{status: http.StatusBadGateway}, want: true},
		"a refusal":      {gh: &runJobsStub{status: http.StatusForbidden}},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := flakyProvisioner(nil)
				tc.gh.serve(p)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := p.attemptJobs(ctx, "org", "repo", "900", 2)
				require.Error(t, err)
				assert.Equal(t, tc.want, errors.Is(err, errRunConclusionUnreadable))
			})
		})
	}
}
