package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// past is the jobs of each attempt before the latest, as it ended.
	past map[int][]runJob
	// next is the conclusion each re-run job ends with, "success" where unset.
	next map[string]string
	// jobsStatus, where set, answers that many lists with its status.
	jobsStatus, jobsFailures int
	emptyList                bool
	// refuseFor is the runner whose job's re-run GitHub refuses for good.
	refuseFor string
	// stale answers that many job re-runs as GitHub refuses an earlier attempt's ID.
	stale int
	// busy refuses every re-run as if the run were in progress, whatever its status says.
	busy bool
	// hold, where set, keeps every re-run call waiting until it is closed.
	hold chan struct{}
	// calls is every accepted re-run: "failed-jobs", or a job's runner.
	calls []string
}

func (a *jobsAPI) failed(i int) bool { return a.jobs[i].failed() }

func (j runJob) failed() bool { return j.conclusion != "success" && j.conclusion != "skipped" }

// conclusion is what GitHub concludes attempt. One past the first that holds a cancelled
// job is cancelled, though none it ran was (measured 2026-11-24).
func (a *jobsAPI) conclusion(attempt int, jobs []runJob) string {
	switch {
	case !slices.ContainsFunc(jobs, runJob.failed):
		return "success"
	case attempt > 1 && slices.ContainsFunc(jobs, func(j runJob) bool { return j.conclusion == "cancelled" }):
		return "cancelled"
	}
	return "failure"
}

// rerun starts the next attempt with the jobs at indexes.
func (a *jobsAPI) rerun(call string, indexes ...int) {
	if a.past == nil {
		a.past = map[int][]runJob{}
	}
	a.past[a.attempt] = slices.Clone(a.jobs)
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
	if a.hold != nil && r.Method == http.MethodPost {
		<-a.hold
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	refuse := func(message string) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/myrepo/actions/")
	switch {
	case path == "runs/4242/rerun-failed-jobs" && r.Method == http.MethodPost:
		if a.running || a.busy {
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
		case a.running || a.busy:
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
	case strings.HasPrefix(path, "runs/4242/attempts/") && strings.HasSuffix(path, "/jobs"):
		attempt, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "runs/4242/attempts/"), "/jobs"))
		jobs := a.jobs
		if attempt != a.attempt {
			jobs = a.past[attempt]
		}
		if a.jobsFailures > 0 {
			a.jobsFailures--
			w.WriteHeader(a.jobsStatus)
			return
		}
		rows := []map[string]any{}
		for i, j := range jobs {
			if a.emptyList {
				break
			}
			at := time.Now().Add(-time.Duration(100-10*max(j.ranIn, 1)) * time.Minute)
			rows = append(rows, map[string]any{"id": attempt*1000 + i, "name": j.name, "runner_name": j.runner,
				"conclusion": j.conclusion, "run_attempt": attempt, "completed_at": at.Format(time.RFC3339)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(rows), "jobs": rows})
	case strings.HasPrefix(path, "runs/4242") && r.Method == http.MethodGet:
		attempt, jobs := a.attempt, a.jobs
		if n, err := strconv.Atoi(strings.TrimPrefix(path, "runs/4242/attempts/")); err == nil && n != a.attempt {
			attempt, jobs = n, a.past[n]
		}
		status, conclusion := "completed", a.conclusion(attempt, jobs)
		if a.running && attempt == a.attempt {
			status, conclusion = "in_progress", ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "status": status, "conclusion": conclusion, "run_attempt": attempt,
			"run_started_at": time.Now().Add(-time.Duration(105-10*attempt) * time.Minute).Format(time.RFC3339)})
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

// TestJobRerun_TwoSetsOfOneRunTakeTurns: GitHub takes one re-run of a run at a time, so
// the set that loses the call keeps its worker parked for the attempt the other's starts.
func TestJobRerun_TwoSetsOfOneRunTakeTurns(t *testing.T) {
	p, api, gpu, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	cpu := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "cpu"}, spec: gpu.spec}
	parkKill(p, gpu, "pod-a", "a", "r-a")
	parkKill(p, cpu, "pod-b", "b", "r-b")
	api.conclude()
	api.hold = make(chan struct{})
	clock.advance(10 * time.Minute)
	released := p.sweepPendingReruns(context.Background())
	close(api.hold)
	require.Len(t, released, 2, "both sets read the run before either call is answered")
	for _, done := range released {
		<-done
	}
	require.Len(t, api.accepted(), 1)
	lost, name := cpu, "b"
	if api.accepted()[0] == "r-b" {
		lost, name = gpu, "a"
	}
	require.Len(t, readLedger(t, p, lost).Pending, 1, "the set GitHub refused keeps its worker parked")

	clock.advance(10 * time.Minute)
	sweepPendingAndWait(t, p, context.Background(), 0)
	release(t, p, api, clock, 1)
	assert.ElementsMatch(t, []string{"r-a", "r-b"}, api.accepted())
	for _, target := range []*stubTarget{gpu, cpu} {
		l := readLedger(t, p, target)
		assert.Empty(t, l.Pending)
		assert.Equal(t, 1, l.Reruns["4242"].Count, "the refused call spends no second slot")
		assert.NotContains(t, target.events, "EvictionRerunFailed")
	}
	trigger, cause := pairFailure(readLedger(t, p, lost).Reruns["4242"], 2, name)
	assert.Equal(t, rerunTriggerGAG, trigger, "the re-run is recorded under the attempt it started from")
	assert.Equal(t, recoveryCauseOOM, cause)
}

// TestJobRerun_ACarriedCancelDoesNotEndTheTurns: GitHub concludes cancelled the attempt that
// carries a fail-fast matrix's cancelled legs, which nobody cancelled, and the next killed
// leg has its turn all the same.
func TestJobRerun_ACarriedCancelDoesNotEndTheTurns(t *testing.T) {
	p, api, gpu, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"},
		runJob{name: "c", runner: "r-c", conclusion: "cancelled"})
	cpu := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "cpu"}, spec: gpu.spec}
	parkKill(p, gpu, "pod-a", "a", "r-a")
	parkKill(p, cpu, "pod-b", "b", "r-b")
	api.conclude()
	clock.advance(10 * time.Minute)
	for _, done := range p.sweepPendingReruns(context.Background()) {
		<-done
	}
	require.Len(t, api.accepted(), 1)
	release(t, p, api, clock, 1)
	assert.ElementsMatch(t, []string{"r-a", "r-b"}, api.accepted())
}

// TestFutileRerun_WhoCancelledIsReadAgain: an attempt that carries cancelled jobs is no
// cancel on any arm, and a list GitHub fails to serve is asked for again before that is
// decided.
func TestFutileRerun_WhoCancelledIsReadAgain(t *testing.T) {
	p, api, target, _, _ := jobsFixture(t,
		runJob{name: "a", runner: "r-a", conclusion: "failure", ranIn: 2},
		runJob{name: "c", runner: "r-c", conclusion: "cancelled"})
	api.attempt, api.running = 2, false
	api.past = map[int][]runJob{1: {{name: "a", conclusion: "failure"}, {name: "c", conclusion: "cancelled"}}}
	api.jobsStatus, api.jobsFailures = http.StatusBadGateway, 1
	evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a", job: "a", failedAttempt: 2})
	assert.Equal(t, []string{"failed-jobs"}, api.accepted())
}

// TestCancelledByOther covers who cancelled a run concluded cancelled.
func TestCancelledByOther(t *testing.T) {
	type attempt struct {
		conclusion string
		jobs       []runJob
	}
	ran := func(in int, conclusion string) runJob {
		return runJob{name: conclusion, conclusion: conclusion, ranIn: in}
	}
	carried := attempt{"cancelled", []runJob{ran(1, "cancelled"), ran(2, "failure")}}
	for _, tc := range []struct {
		name     string
		attempts []attempt
		self     int
		// jobs and run answer every jobs list, and every earlier attempt's read, with a status.
		jobs, run int
		want      bool
		retry     bool
		// other serves an earlier attempt's read as another attempt's.
		other bool
	}{
		{"not cancelled", []attempt{{"failure", []runJob{ran(1, "cancelled")}}}, 0, 0, 0, false, false, false},
		{"the first attempt", []attempt{{"cancelled", []runJob{ran(1, "cancelled")}}}, 0, 0, 0, true, false, false},
		{"a first attempt that lists no cancelled job", []attempt{{"cancelled", []runJob{ran(1, "failure")}}}, 0, 0, 0, true, false, false},
		{"another attempt read for the one before", []attempt{{"failure", nil}, carried}, 0, 0, 0, true, false, true},
		{"a job of a later attempt", []attempt{{"failure", []runJob{ran(1, "failure")}}, {"cancelled", []runJob{ran(2, "cancelled")}}}, 0, 0, 0, true, false, false},
		{"carried from a failed attempt", []attempt{{"failure", []runJob{ran(1, "cancelled")}}, carried}, 0, 0, 0, false, false, false},
		{"carried from a cancelled first attempt", []attempt{{"cancelled", []runJob{ran(1, "cancelled")}}, carried}, 0, 0, 0, true, false, false},
		{"carried twice", []attempt{{"failure", []runJob{ran(1, "cancelled")}}, carried, {"cancelled", []runJob{ran(1, "cancelled"), ran(3, "failure")}}}, 0, 0, 0, false, false, false},
		{"carried from one somebody cancelled", []attempt{{"failure", []runJob{ran(1, "failure")}}, {"cancelled", []runJob{ran(2, "cancelled")}}, {"cancelled", []runJob{ran(2, "cancelled"), ran(3, "failure")}}}, 0, 0, 0, true, false, false},
		{"GAG's own", []attempt{{"cancelled", []runJob{ran(1, "cancelled")}}}, 1, 0, 0, false, false, false},
		{"carried from GAG's own", []attempt{{"cancelled", []runJob{ran(1, "cancelled")}}, carried}, 1, 0, 0, false, false, false},
		{"a list GitHub refuses", []attempt{{"failure", nil}, carried}, 0, http.StatusForbidden, 0, true, false, false},
		{"a list GitHub fails to serve", []attempt{{"failure", nil}, carried}, 0, http.StatusBadGateway, 0, true, true, false},
		{"an empty list", []attempt{{"failure", nil}, {"cancelled", nil}}, 0, 0, 0, true, false, false},
		{"an earlier attempt GitHub refuses", []attempt{{"failure", nil}, carried}, 0, 0, http.StatusForbidden, true, false, false},
		{"an earlier attempt GitHub fails to serve", []attempt{{"failure", nil}, carried}, 0, 0, http.StatusBadGateway, true, true, false},
		{"an earlier attempt without a conclusion", []attempt{{"", nil}, carried}, 0, 0, 0, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now().Add(-time.Hour)
			at := func(attempt int) time.Time { return start.Add(time.Duration(attempt) * time.Minute) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, jobs := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/myorg/myrepo/actions/runs/4242/attempts/"), "/jobs")
				n, _ := strconv.Atoi(path)
				switch status := map[bool]int{true: tc.jobs, false: tc.run}[jobs]; {
				case status != 0:
					w.WriteHeader(status)
				case jobs:
					rows := []map[string]any{}
					for _, j := range tc.attempts[n-1].jobs {
						rows = append(rows, map[string]any{"name": j.name, "conclusion": j.conclusion, "completed_at": at(j.ranIn).Add(time.Second)})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(rows), "jobs": rows})
				default:
					served := n
					if tc.other {
						served++
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "conclusion": tc.attempts[n-1].conclusion, "run_attempt": served, "run_started_at": at(n)})
				}
			}))
			t.Cleanup(srv.Close)
			p := ledgerProvisioner(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build(), srv)
			target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}, spec: &ResolvedSpec{}}
			if tc.self > 0 {
				require.NoError(t, p.recordSelfCancel(context.Background(), target, "4242", tc.self))
			}
			n := len(tc.attempts)
			got, err := p.cancelledByOther(context.Background(), target, "myorg", "myrepo", "4242", runState{
				status: "completed", conclusion: tc.attempts[n-1].conclusion, attempt: n, startedAt: at(n)})
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.retry, errors.Is(err, errRunConclusionUnreadable))
		})
	}
}

// TestJobRerun_ARefusedReleaseEndsWithTheWait: a run that reads concluded and is refused as
// running waits like one in progress, and ends the same way.
func TestJobRerun_ARefusedReleaseEndsWithTheWait(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "build", runner: "r-build", conclusion: "failure"})
	p.EvictionRerunWindow = 20 * time.Millisecond
	neverConcluded := p.Metrics.EvictionRerunFailures.WithLabelValues("team-a", "gpu", recoveryCauseOOM, rerunFailureReasonNeverConcluded)
	parkKill(p, target, "pod-a", "build", "r-build")
	api.busy = true
	release(t, p, api, clock, 1)
	require.Len(t, readLedger(t, p, target).Pending, 1)
	assert.Zero(t, testutil.ToFloat64(neverConcluded))
	sweepPendingAndWait(t, p, context.Background(), 0)

	clock.advance(pendingRerunMaxWait)
	sweepPendingAndWait(t, p, context.Background(), 1)
	assert.Empty(t, readLedger(t, p, target).Pending)
	assert.Equal(t, float64(1), testutil.ToFloat64(neverConcluded))
	assert.Empty(t, api.accepted())
}

// TestJobRerun_ACancelledSiblingIsReRunWithTheKilledLeg: a fail-fast matrix cancels the
// killed leg's siblings, and one call takes them all where nothing failed on its own.
func TestJobRerun_ACancelledSiblingIsReRunWithTheKilledLeg(t *testing.T) {
	for _, tc := range []struct {
		name, lint string
		want       []string
		sibling    string
	}{
		{"nothing else failed", "success", []string{"failed-jobs"}, "success"},
		{"beside a genuine failure", "failure", []string{"r-a"}, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t,
				runJob{name: "lint", runner: "r-lint", conclusion: tc.lint},
				runJob{name: "a", runner: "r-a", conclusion: "failure"},
				runJob{name: "b", runner: "r-b", conclusion: "cancelled"})
			parkKill(p, target, "pod-a", "a", "r-a")
			release(t, p, api, clock, 1)
			assert.Equal(t, tc.want, api.accepted())
			assert.Equal(t, tc.sibling, api.jobs[2].conclusion)
			l := readLedger(t, p, target)
			assert.Empty(t, l.Pending)
			assert.Equal(t, 1, l.Reruns["4242"].Count)
		})
	}
}

// TestJobRerun_AReleaseThatWidensRecordsEveryJobItTakes: the killed jobs a re-run of every
// failed job takes are paired with it, where a single-job release GitHub refused had
// narrowed the attempt's record to the first of them.
func TestJobRerun_AReleaseThatWidensRecordsEveryJobItTakes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// human is the jobs somebody re-runs after the refusal, in turn.
		human []int
		// noList is a jobs list that names no job by the next release.
		noList bool
		want   []string
	}{
		{"the first killed job stands", []int{0}, false, []string{"a", "b", "c"}},
		{"somebody re-ran the first killed job too", []int{0, 1}, false, []string{"b", "c"}},
		{"the list no longer says which jobs were killed", nil, true, []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api, target, clock, _ := jobsFixture(t,
				runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
				runJob{name: "a", runner: "r-a", conclusion: "failure"},
				runJob{name: "b", runner: "r-b", conclusion: "failure"},
				runJob{name: "c", runner: "r-c", conclusion: "failure"})
			for _, j := range []string{"a", "b", "c"} {
				parkKill(p, target, "pod-"+j, j, "r-"+j)
			}
			api.busy = true
			release(t, p, api, clock, 1)
			require.Equal(t, []string{"a"}, readLedger(t, p, target).Reruns["4242"].Attempts["1"].Jobs)
			api.busy, api.emptyList = false, tc.noList
			for _, i := range tc.human {
				api.humanReruns(i)
			}
			release(t, p, api, clock, 1)

			calls := api.accepted()
			require.Equal(t, "failed-jobs", calls[len(calls)-1])
			r := readLedger(t, p, target).Reruns["4242"]
			a := r.Attempts[strconv.Itoa(len(calls))]
			require.NotNil(t, a, "the re-run is recorded under the attempt it started from")
			assert.Equal(t, rerunScopeFailedJobs, a.Scope)
			assert.Equal(t, tc.want, a.Jobs)
			for _, job := range tc.want {
				trigger, _ := pairFailure(r, len(calls), job)
				assert.Equal(t, rerunTriggerGAG, trigger, job)
			}
		})
	}
}

// TestReserveScaleSetRerun_WidensOnlyTheAttemptItReRunsFrom: a narrowed record that cannot
// move to the attempt the release re-runs from, which holds one already, stays as it is.
func TestReserveScaleSetRerun_WidensOnlyTheAttemptItReRunsFrom(t *testing.T) {
	p, _, target, _, _ := jobsFixture(t)
	for _, d := range []rerunDisruption{
		{key: "pod-a", job: "a", failedAttempt: 1, scope: rerunScopeJob},
		{key: "pod-x", job: "x", failedAttempt: 2, scope: rerunScopeJob},
		{key: "pod-a", job: "a", failedAttempt: 2, scope: rerunScopeFailedJobs, with: []killedWorker{{Job: "b", Disruption: "pod-b"}}},
	} {
		_, _, err := p.reserveScaleSetRerun(context.Background(), target, "4242", &d, recoveryCauseOOM, 2, nil)
		require.NoError(t, err)
	}
	a := readLedger(t, p, target).Reruns["4242"].Attempts["1"]
	assert.Equal(t, rerunScopeJob, a.Scope)
	assert.Equal(t, []string{"a"}, a.Jobs)
}

// TestReserveScaleSetRerun_AMergeRecordsEveryJobItTakes: a release that merges into a
// disruption arm's record of the attempt records its other jobs there too.
func TestReserveScaleSetRerun_AMergeRecordsEveryJobItTakes(t *testing.T) {
	p, _, target, _, _ := jobsFixture(t)
	for _, d := range []rerunDisruption{
		{key: "pod-x", job: "x", failedAttempt: 1},
		{key: "pod-b", job: "b", failedAttempt: 1, with: []killedWorker{{Job: "c", Disruption: "pod-c"}}},
	} {
		_, _, err := p.reserveScaleSetRerun(context.Background(), target, "4242", &d, recoveryCauseOOM, 2, nil)
		require.NoError(t, err)
	}
	r := readLedger(t, p, target).Reruns["4242"]
	assert.Equal(t, []string{"x", "b", "c"}, r.Attempts["1"].Jobs)
	assert.Equal(t, 1, r.Count)
}

// TestJobRerun_AReplayedParkDoesNotWiden: parking a kill again, as a restart does, leaves
// the single-job re-run its release recorded.
func TestJobRerun_AReplayedParkDoesNotWiden(t *testing.T) {
	p, api, target, clock, _ := jobsFixture(t,
		runJob{name: "lint", runner: "r-lint", conclusion: "failure"},
		runJob{name: "a", runner: "r-a", conclusion: "failure"},
		runJob{name: "b", runner: "r-b", conclusion: "failure"})
	parkKill(p, target, "pod-a", "a", "r-a")
	parkKill(p, target, "pod-b", "b", "r-b")
	api.busy = true
	release(t, p, api, clock, 1)
	parkKill(p, target, "pod-a", "a", "r-a")
	a := readLedger(t, p, target).Reruns["4242"].Attempts["1"]
	assert.Equal(t, rerunScopeJob, a.Scope)
	assert.Equal(t, []string{"a"}, a.Jobs)
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

func TestRerunRecord_Advance(t *testing.T) {
	one := func(scope string, disruptions ...string) *rerunAttempt {
		return &rerunAttempt{Scope: scope, Disruptions: disruptions}
	}
	for _, tc := range []struct {
		name     string
		attempts map[string]*rerunAttempt
		to       int
		want     []string
	}{
		{"a later attempt", map[string]*rerunAttempt{"1": one(rerunScopeJob, "pod-a")}, 3, []string{"3"}},
		{"the same attempt", map[string]*rerunAttempt{"2": one(rerunScopeJob, "pod-a")}, 2, []string{"2"}},
		{"an earlier attempt", map[string]*rerunAttempt{"2": one(rerunScopeJob, "pod-a")}, 1, []string{"2"}},
		{"a re-run of every failed job", map[string]*rerunAttempt{"1": one(rerunScopeFailedJobs, "pod-a")}, 2, []string{"1"}},
		{"a re-run another disruption shares", map[string]*rerunAttempt{"1": one(rerunScopeJob, "pod-a", "pod-b")}, 2, []string{"1"}},
		{"the later attempt holds a re-run", map[string]*rerunAttempt{"1": one(rerunScopeJob, "pod-a"), "2": one(rerunScopeJob, "pod-b")}, 2, []string{"1", "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rerunRecord{Attempts: tc.attempts}
			from := "1"
			if tc.attempts[from] == nil {
				from = "2"
			}
			a := r.Attempts[from]
			moved := r.advance(from, &rerunDisruption{key: "pod-a", failedAttempt: tc.to})
			assert.ElementsMatch(t, tc.want, slices.Collect(maps.Keys(r.Attempts)))
			assert.Equal(t, len(tc.want) == 1 && tc.want[0] != from, moved)
			if moved {
				assert.Same(t, a, r.Attempts[tc.want[0]])
			}
		})
	}
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
		{"a sibling its matrix or its timeout cancelled", []killedWorker{a}, []workflowJobSummary{row(1, "a", "r-a", "failure"), row(2, "b", "r-b", "cancelled")}, rerunScopeFailedJobs, []killedWorker{a}},
		{"a cancelled sibling beside a failure", []killedWorker{a}, append(jobs[:2:2], row(4, "c", "r-c", "cancelled")), rerunScopeJob, []killedWorker{a}},
		{"a killed job GitHub cancelled", []killedWorker{a}, []workflowJobSummary{row(1, "a", "r-a", "cancelled")}, rerunScopeFailedJobs, []killedWorker{a}},
		{"nothing but a cancelled sibling", []killedWorker{{Job: "a", Runner: "gone"}}, []workflowJobSummary{row(2, "b", "r-b", "cancelled")}, rerunScopeFailedJobs, nil},
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
