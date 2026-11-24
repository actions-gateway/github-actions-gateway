package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// runJob is one job of jobsAPI's run.
type runJob struct {
	name, runner, conclusion string
	// ranIn is the attempt the job last ran in, 0 for attempt 1.
	ranIn int
}

// jobsAPI fakes one run as GitHub treats it (measured 2026-11-24, the design's M6): each
// attempt lists every job under a new ID, a job it did not re-run keeps its runner and
// times, and a single-job re-run is refused while the run is in progress or for an earlier
// attempt's ID.
type jobsAPI struct {
	mu      sync.Mutex
	jobs    []runJob
	attempt int
	running bool
	// next is the conclusion each re-run job ends with, "success" where unset.
	next map[string]string
	// jobsStatus, where set, answers that many lists with its status.
	jobsStatus, jobsFailures int
	emptyList                bool
	// refuseFor is the runner whose job's re-run GitHub refuses for good.
	refuseFor string
	// stale answers that many job re-runs as GitHub refuses an earlier attempt's ID.
	stale int
	// calls is every accepted re-run: "failed-jobs", or a job's runner.
	calls []string
}

func (a *jobsAPI) jobID(i int) int { return a.attempt*1000 + i }

func (a *jobsAPI) failed(i int) bool {
	return a.jobs[i].conclusion != "success" && a.jobs[i].conclusion != "skipped"
}

// rerun starts the next attempt with the jobs at indexes.
func (a *jobsAPI) rerun(call string, indexes ...int) {
	a.attempt++
	a.running = true
	a.calls = append(a.calls, call)
	for _, i := range indexes {
		a.jobs[i].ranIn = a.attempt
		a.jobs[i].runner = fmt.Sprintf("%s-%d", a.jobs[i].runner, a.attempt)
		a.jobs[i].conclusion = "success"
		if c := a.next[a.jobs[i].name]; c != "" {
			a.jobs[i].conclusion = c
		}
	}
}

// humanReruns re-runs the job at i, as somebody's click does, and ends the attempt.
func (a *jobsAPI) humanReruns(i int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rerun("human", i)
	a.running = false
}

// conclude ends the attempt in progress.
func (a *jobsAPI) conclude() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running = false
}

func (a *jobsAPI) accepted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *jobsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	refuse := func(message string) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/myrepo/actions/")
	switch {
	case path == "runs/4242/rerun-failed-jobs" && r.Method == http.MethodPost:
		if a.running {
			_ = r.Body.Close()
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, alreadyRunningBody)
			return
		}
		var failed []int
		for i := range a.jobs {
			if a.failed(i) {
				failed = append(failed, i)
			}
		}
		a.rerun("failed-jobs", failed...)
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(path, "jobs/") && strings.HasSuffix(path, "/rerun") && r.Method == http.MethodPost:
		id, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "jobs/"), "/rerun"))
		switch {
		case a.running:
			refuse("The workflow run containing this job is already running")
		case a.stale > 0 || id/1000 != a.attempt:
			a.stale--
			refuse("Only jobs from the current attempt can be re-run")
		case a.refuseFor == a.jobs[id%1000].runner:
			refuse("Resource not accessible by integration")
		default:
			a.rerun(a.jobs[id%1000].runner, id%1000)
			w.WriteHeader(http.StatusCreated)
		}
	case path == fmt.Sprintf("runs/4242/attempts/%d/jobs", a.attempt) && r.Method == http.MethodGet:
		if a.jobsFailures > 0 {
			a.jobsFailures--
			w.WriteHeader(a.jobsStatus)
			return
		}
		rows := []map[string]any{}
		for i, j := range a.jobs {
			if a.emptyList {
				break
			}
			at := time.Now().Add(-time.Duration(100-10*max(j.ranIn, 1)) * time.Minute)
			rows = append(rows, map[string]any{"id": a.jobID(i), "name": j.name, "runner_name": j.runner,
				"conclusion": j.conclusion, "run_attempt": a.attempt, "completed_at": at.Format(time.RFC3339)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(rows), "jobs": rows})
	case path == "runs/4242" && r.Method == http.MethodGet:
		status, conclusion := "completed", "success"
		for i := range a.jobs {
			if a.failed(i) {
				conclusion = "failure"
			}
		}
		if a.running {
			status, conclusion = "in_progress", ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "status": status, "conclusion": conclusion, "run_attempt": a.attempt,
			"run_started_at": time.Now().Add(-time.Duration(105-10*a.attempt) * time.Minute).Format(time.RFC3339)})
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

// jobsFixture is a set with a run of jobs, all of them finished in attempt 1, which is
// still running until conclude.
func jobsFixture(t *testing.T, jobs ...runJob) (*Provisioner, *jobsAPI, *stubTarget, *testClock, func() *Provisioner) {
	t.Helper()
	api := &jobsAPI{jobs: jobs, attempt: 1, running: true, next: map[string]string{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}, spec: &ResolvedSpec{MaxEvictionRetries: 2}}
	clock := newTestClock()
	newP := func() *Provisioner {
		p := ledgerProvisioner(c, srv)
		p.now = clock.now
		return p
	}
	return newP(), api, target, clock, newP
}

// parkKill parks the out-of-memory re-run of the worker pod whose runner is runner.
func parkKill(p *Provisioner, target Target, pod, job, runner string) {
	evictScaleSet(p, target, "4242", 2, recoveryCauseOOM, rerunDisruption{key: pod, job: job, workerRunner: runner, park: true})
}

// release ends the attempt in progress and sweeps, requiring want releases.
func release(t *testing.T, p *Provisioner, api *jobsAPI, clock *testClock, want int) {
	t.Helper()
	api.conclude()
	clock.advance(10 * time.Minute)
	sweepPendingAndWait(t, p, context.Background(), want)
}

func readLedger(t *testing.T, p *Provisioner, target Target) *rerunLedger {
	t.Helper()
	l, _, err := p.readRerunLedger(context.Background(), target)
	require.NoError(t, err)
	return l
}

// TestJobRerun_AGenuineFailureIsLeftAlone is Q1319: the class re-runs the job its worker
// was killed under and not the sibling that failed on its own, and the ledger says so.
func TestJobRerun_AGenuineFailureIsLeftAlone(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "build", runner: "r-build", conclusion: "failure"},
		runJob{name: "docs", runner: "r-docs", conclusion: "success"})
	parkKill(p, target, "pod-a", "build", "r-build")
	release(t, p, api, clock, 1)

	assert.Equal(t, []string{"r-build"}, api.accepted(), "only the killed job is re-run")
	l := readLedger(t, p, target)
	assert.Empty(t, l.Pending)
	assert.Equal(t, 1, l.Reruns["4242"].Count, "the release re-uses the slot taken when the class fired")
	a := l.Reruns["4242"].Attempts["1"]
	assert.Equal(t, rerunScopeJob, a.Scope)
	assert.Equal(t, []string{"build"}, a.Jobs)
	assert.Equal(t, []string{"pod-a"}, a.Disruptions)
}

// TestJobRerun_MatchesTheRunnerAndNotTheName: two jobs of one name, and a pod whose
// recorded name is another job's, which is what a worker killed before its JobStarted is
// handled leaves (Q1389).
func TestJobRerun_MatchesTheRunnerAndNotTheName(t *testing.T) {
	for _, tc := range []struct{ name, podJob string }{
		{"the second of two jobs that share a name", "dup"},
		{"the pod names the job it was provisioned for", "lint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t,
				runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
				runJob{name: "dup", runner: "r-one", conclusion: "failure"},
				runJob{name: "dup", runner: "r-two", conclusion: "failure"})
			parkKill(p, target, "pod-a", tc.podJob, "r-two")
			release(t, p, api, clock, 1)
			assert.Equal(t, []string{"r-two"}, api.accepted())
		})
	}
}

// TestJobRerun_WithoutARunnerName falls back to the display name where it names one job,
// and to every failed job where it does not.
func TestJobRerun_WithoutARunnerName(t *testing.T) {
	lint := runJob{name: "lint", runner: "r-lint", conclusion: "failure"}
	build := runJob{name: "build", runner: "r-build", conclusion: "failure"}
	for _, tc := range []struct {
		name string
		jobs []runJob
		want string
	}{
		{"the name is one job's", []runJob{lint, build}, "r-build"},
		{"two jobs share it", []runJob{lint, build, {name: "build", runner: "r-twin", conclusion: "success"}}, "failed-jobs"},
		{"no job has it", []runJob{lint, {name: "caller / build", runner: "r-build", conclusion: "failure"}}, "failed-jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t, tc.jobs...)
			parkKill(p, target, "pod-a", "build", "")
			release(t, p, api, clock, 1)
			assert.Equal(t, []string{tc.want}, api.accepted())
		})
	}
}

// TestJobRerun_NothingToReRun: a runner the run's failed jobs do not name re-runs nothing,
// even where the pod's job name is one of them.
func TestJobRerun_NothingToReRun(t *testing.T) {
	for _, tc := range []struct{ name, runner string }{
		{"the runner took no job of this run", "r-elsewhere"},
		{"the runner's job succeeded", "r-docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t,
				runJob{name: "build", runner: "r-build", conclusion: "failure"},
				runJob{name: "docs", runner: "r-docs", conclusion: "success"})
			parkKill(p, target, "pod-a", "build", tc.runner)
			release(t, p, api, clock, 0)

			assert.Empty(t, api.accepted())
			assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunWithheld.WithLabelValues(
				"team-a", "gpu", recoveryCauseOOM, rerunWithheldReasonJobNotFound)))
			assert.Contains(t, target.events, "EvictionRerunWithheld")
			assert.Empty(t, readLedger(t, p, target).Pending)
		})
	}
}

// TestJobRerun_ASucceededRunIsTheFutilityChecks: a run that succeeded with the killed
// worker's job in it is withheld as any other, not as a job that was not found.
func TestJobRerun_ASucceededRunIsTheFutilityChecks(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t, runJob{name: "build", runner: "r-build", conclusion: "success"})
	parkKill(p, target, "pod-a", "build", "r-build")
	release(t, p, api, clock, 1)

	assert.Empty(t, api.accepted())
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunWithheld.WithLabelValues(
		"team-a", "gpu", recoveryCauseOOM, rerunWithheldReasonRunSucceeded)))
	assert.Empty(t, readLedger(t, p, target).Pending)
}

// TestJobRerun_KilledJobsThatAreAllTheFailuresShareOneReRun: re-running every failed job
// takes nothing the class did not classify, so it costs the one slot it always did.
func TestJobRerun_KilledJobsThatAreAllTheFailuresShareOneReRun(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "cancelled"},
		runJob{name: "c", runner: "r-c", conclusion: "failure"},
		runJob{name: "docs", runner: "r-docs", conclusion: "success"},
		runJob{name: "after", runner: "", conclusion: "skipped"})
	for _, j := range []string{"a", "b", "c"} {
		parkKill(p, target, "pod-"+j, j, "r-"+j)
	}
	release(t, p, api, clock, 1)

	assert.Equal(t, []string{"failed-jobs"}, api.accepted())
	l := readLedger(t, p, target)
	assert.Empty(t, l.Pending)
	assert.Equal(t, 1, l.Reruns["4242"].Count)
	assert.Equal(t, rerunScopeFailedJobs, l.Reruns["4242"].Attempts["1"].Scope)
}

// TestJobRerun_KilledJobsBesideAGenuineFailureTakeTurns: with a failure nobody classified
// in the run, the killed jobs are re-run one attempt at a time, each on its own slot, until
// the budget is spent.
func TestJobRerun_KilledJobsBesideAGenuineFailureTakeTurns(t *testing.T) {
	p, api, target, clock, newP := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"},
		runJob{name: "c", runner: "r-c", conclusion: "failure"})
	for _, j := range []string{"a", "b", "c"} {
		parkKill(p, target, "pod-"+j, j, "r-"+j)
	}
	release(t, p, api, clock, 1)
	assert.Equal(t, []string{"r-a"}, api.accepted())
	l := readLedger(t, p, target)
	require.Len(t, l.Pending, 1, "the others stay parked")
	for _, e := range l.Pending {
		assert.Equal(t, []string{"pod-b", "pod-c"}, []string{e.Workers[0].Disruption, e.Workers[1].Disruption})
		assert.Equal(t, "pod-b", e.Disruption)
	}

	clock.advance(10 * time.Minute)
	sweepPendingAndWait(t, p, context.Background(), 0)
	assert.Equal(t, []string{"r-a"}, api.accepted(), "the second waits for the first's attempt to conclude")

	after := newP()
	require.NoError(t, after.AdoptPendingReruns(context.Background(), target))
	release(t, after, api, clock, 1)
	assert.Equal(t, []string{"r-a", "r-b"}, api.accepted(), "a restart between turns loses none")
	l = readLedger(t, after, target)
	assert.Equal(t, 2, l.Reruns["4242"].Count, "each turn is a re-run and spends a slot")
	assert.Equal(t, []string{"a"}, l.Reruns["4242"].Attempts["1"].Jobs)
	assert.Equal(t, []string{"b"}, l.Reruns["4242"].Attempts["2"].Jobs)
	assert.Equal(t, rerunScopeJob, l.Reruns["4242"].Attempts["2"].Scope)

	release(t, after, api, clock, 1)
	assert.Equal(t, []string{"r-a", "r-b"}, api.accepted(), "the budget of two is spent")
	assert.Equal(t, float64(1), testutil.ToFloat64(after.Metrics.EvictionRetriesExhausted.WithLabelValues(
		"team-a", "gpu", recoveryCauseOOM)))
	assert.Empty(t, readLedger(t, after, target).Pending)
	assert.Equal(t, "r-lint", api.jobs[0].runner, "the genuine failure was never re-run")
}

// TestJobRerun_AHumansReRunLeavesTheOthersParked: somebody re-ran one of two killed jobs,
// so only the other is.
func TestJobRerun_AHumansReRunLeavesTheOthersParked(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	parkKill(p, target, "pod-a", "a", "r-a")
	parkKill(p, target, "pod-b", "b", "r-b")
	api.humanReruns(1)
	release(t, p, api, clock, 1)

	assert.Equal(t, []string{"human", "r-b"}, api.accepted())
	l := readLedger(t, p, target)
	assert.Empty(t, l.Pending)
	assert.Equal(t, 1, l.Reruns["4242"].Count, "the slot taken for the two pays for the one re-run")
}

// TestJobRerun_FallsBackToFailedJobs covers a list that does not say which jobs were
// killed, and an attempt a disruption arm also re-runs.
func TestJobRerun_FallsBackToFailedJobs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *Provisioner, api *jobsAPI, target Target)
	}{
		{"GitHub refuses the list", func(_ *testing.T, p *Provisioner, api *jobsAPI, target Target) {
			parkKill(p, target, "pod-a", "build", "r-build")
			api.jobsStatus, api.jobsFailures = http.StatusForbidden, 1
		}},
		{"the list is empty", func(_ *testing.T, p *Provisioner, api *jobsAPI, target Target) {
			parkKill(p, target, "pod-a", "build", "r-build")
			api.emptyList = true
		}},
		{"more kills than the entry lists", func(_ *testing.T, p *Provisioner, _ *jobsAPI, target Target) {
			for i := range maxMergedPerAttempt + 1 {
				runner := "r-elsewhere"
				if i == maxMergedPerAttempt {
					runner = "r-build"
				}
				parkKill(p, target, fmt.Sprintf("pod-%d", i), "build", runner)
			}
		}},
		{"an older AGC's entry, which names one worker of its attempt's", func(t *testing.T, p *Provisioner, _ *jobsAPI, target Target) {
			parkKill(p, target, "pod-a", "build", "r-build")
			require.NoError(t, p.updateRerunLedger(context.Background(), target, func(l *rerunLedger, _ time.Time) bool {
				for _, e := range l.Pending {
					e.Workers = nil
				}
				return true
			}))
			parkKill(p, target, "pod-b", "test", "r-test")
			for _, e := range readLedger(t, p, target).Pending {
				require.Empty(t, e.Workers, "a list that would miss pod-a is not started")
			}
		}},
		{"a disruption arm holds the attempt too", func(t *testing.T, p *Provisioner, _ *jobsAPI, target Target) {
			parkKill(p, target, "pod-a", "build", "r-build")
			_, _, err := p.reserveScaleSetRerun(context.Background(), target, "4242",
				&rerunDisruption{key: "evicted", job: "test", failedAttempt: 1}, recoveryCauseEviction, 2, nil)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t,
				runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
				runJob{name: "build", runner: "r-build", conclusion: "failure"},
				runJob{name: "test", runner: "r-test", conclusion: "failure"})
			tc.setup(t, p, api, target)
			release(t, p, api, clock, 1)

			assert.Equal(t, []string{"failed-jobs"}, api.accepted())
			l := readLedger(t, p, target)
			assert.Empty(t, l.Pending)
			assert.Equal(t, 1, l.Reruns["4242"].Count)
			assert.Equal(t, rerunScopeFailedJobs, l.Reruns["4242"].Attempts["1"].Scope)
		})
	}
}

// TestJobRerun_AnUnreadableListWaits: a list GitHub failed to serve costs a poll rather
// than a re-run of every failed job.
func TestJobRerun_AnUnreadableListWaits(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "build", runner: "r-build", conclusion: "failure"})
	parkKill(p, target, "pod-a", "build", "r-build")
	api.jobsStatus, api.jobsFailures = http.StatusBadGateway, 1
	release(t, p, api, clock, 0)
	assert.Empty(t, api.accepted())
	require.Len(t, readLedger(t, p, target).Pending, 1)

	release(t, p, api, clock, 1)
	assert.Equal(t, []string{"r-build"}, api.accepted())
}

// TestJobRerun_AStaleJobIDIsReadAgain: a re-run that landed between the list and the call
// makes GitHub refuse the ID, and the next pass finds the job in the list again.
func TestJobRerun_AStaleJobIDIsReadAgain(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "build", runner: "r-build", conclusion: "failure"})
	parkKill(p, target, "pod-a", "build", "r-build")
	api.stale = 1
	release(t, p, api, clock, 1)
	assert.Equal(t, []string{"r-build"}, api.accepted())
	assert.Zero(t, testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", recoveryCauseOOM, rerunFailureReasonAPIError)))
}

// TestJobRerun_ARestartBetweenTheSlotAndTheCall: the attempt is narrowed to the job before
// the call, and a process that stops there leaves the entry whole, so its successor resumes
// the same job on the same slot and the others still take their turns.
func TestJobRerun_ARestartBetweenTheSlotAndTheCall(t *testing.T) {
	ctx := context.Background()
	p, api, target, clock, newP := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	parkKill(p, target, "pod-a", "a", "r-a")
	parkKill(p, target, "pod-b", "b", "r-b")
	d := rerunDisruption{key: "pod-a", job: "a", failedAttempt: 1, scope: rerunScopeJob}
	got, _, err := p.reserveScaleSetRerun(ctx, target, "4242", &d, recoveryCauseOOM, 2, nil)
	require.NoError(t, err)
	require.Equal(t, rerunResumed, got)
	require.Equal(t, rerunScopeJob, readLedger(t, p, target).Reruns["4242"].Attempts["1"].Scope)

	after := newP()
	require.NoError(t, after.AdoptPendingReruns(ctx, target))
	release(t, after, api, clock, 1)
	assert.Equal(t, []string{"r-a"}, api.accepted())
	assert.Equal(t, 1, readLedger(t, after, target).Reruns["4242"].Count)
	release(t, after, api, clock, 1)
	assert.Equal(t, []string{"r-a", "r-b"}, api.accepted())
	assert.Equal(t, 2, readLedger(t, after, target).Reruns["4242"].Count)
}

// TestJobRerun_AFailedCallKeepsTheOthersTurn: a single-job re-run GitHub refuses for good
// ends that job's recovery and no other's.
func TestJobRerun_AFailedCallKeepsTheOthersTurn(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	parkKill(p, target, "pod-a", "a", "r-a")
	parkKill(p, target, "pod-b", "b", "r-b")
	api.refuseFor = "r-a"
	release(t, p, api, clock, 1)
	assert.Empty(t, api.accepted())
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", recoveryCauseOOM, rerunFailureReasonAPIError)))

	release(t, p, api, clock, 1)
	assert.Equal(t, []string{"r-b"}, api.accepted())
	a := readLedger(t, p, target).Reruns["4242"].Attempts["1"]
	assert.Equal(t, rerunScopeJob, a.Scope)
	assert.Equal(t, []string{"pod-a", "pod-b"}, a.Disruptions, "the first keeps the slot it spent")
	assert.Equal(t, 2, readLedger(t, p, target).Reruns["4242"].Count)
}

// TestJobRerun_AKillFoundLateDoesNotWidenASingleJobReRun: a worker killed in the attempt
// and found after its single-job re-run was recorded, as a scan after a restart finds it,
// parks on a slot of its own and is re-run alone.
func TestJobRerun_AKillFoundLateDoesNotWidenASingleJobReRun(t *testing.T) {
	ctx := context.Background()
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	first := rerunDisruption{key: "pod-a", job: "a", failedAttempt: 1, scope: rerunScopeJob}
	_, _, err := p.reserveScaleSetRerun(ctx, target, "4242", &first, recoveryCauseOOM, 2, nil)
	require.NoError(t, err)

	parkKill(p, target, "pod-b", "b", "r-b")
	release(t, p, api, clock, 1)
	assert.Equal(t, []string{"r-b"}, api.accepted())
	assert.Equal(t, 2, readLedger(t, p, target).Reruns["4242"].Count)
}

// TestPendingSets_OneReleasePerRun: GitHub takes one re-run of a run at a time, so a
// second entry of the run waits for the first's release rather than losing to it.
func TestPendingSets_OneReleasePerRun(t *testing.T) {
	var s pendingSets
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	require.True(t, s.due("set|4242/a", "set|4242", 0, now, time.Time{}))
	require.True(t, s.claimRelease("set|4242"))
	assert.False(t, s.due("set|4242/b", "set|4242", 0, now, time.Time{}))
	assert.False(t, s.claimRelease("set|4242"))
	assert.True(t, s.due("set|77/a", "set|77", 0, now, time.Time{}), "another run is unaffected")

	s.settle("set|4242/a", "set|4242", true)
	assert.True(t, s.due("set|4242/b", "set|4242", 0, now, time.Time{}))
}

func TestPlanJobRerun(t *testing.T) {
	row := func(id int64, name, runner, conclusion string) workflowJobSummary {
		return workflowJobSummary{ID: id, Name: name, RunnerName: runner, Conclusion: conclusion}
	}
	a, b := killedWorker{Job: "a", Runner: "r-a", Disruption: "pod-a"}, killedWorker{Job: "b", Runner: "r-b", Disruption: "pod-b"}
	jobs := []workflowJobSummary{row(1, "a", "r-a", "failure"), row(2, "b", "r-b", "failure"), row(3, "docs", "r-d", "success")}
	for _, tc := range []struct {
		name      string
		workers   []killedWorker
		jobs      []workflowJobSummary
		wantScope string
		want      []killedWorker
	}{
		{"one of two failures", []killedWorker{a}, jobs, rerunScopeJob, []killedWorker{a}},
		{"every failure", []killedWorker{a, b}, jobs, rerunScopeFailedJobs, []killedWorker{a, b}},
		{"one whose runner is not listed beside one that is", []killedWorker{{Job: "b", Runner: "gone"}, a}, jobs, rerunScopeJob, []killedWorker{a}},
		{"none listed", []killedWorker{{Job: "a", Runner: "gone"}}, jobs, rerunScopeJob, nil},
		{"no list", []killedWorker{a}, nil, rerunScopeFailedJobs, []killedWorker{a}},
		{"no workers", nil, jobs, rerunScopeFailedJobs, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, standing := planJobRerun(tc.workers, tc.jobs)
			assert.Equal(t, tc.wantScope, scope)
			assert.Equal(t, tc.want, standing)
		})
	}
}
