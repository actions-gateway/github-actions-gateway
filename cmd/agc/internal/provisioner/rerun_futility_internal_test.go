package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The known-futile checks on the scale-set re-run path. Each case drives one
// disruption through handleEviction against a fake GitHub and asserts both halves of a
// withhold: no re-run call, and no budget slot spent.

// futilityAPI fakes the run GET, the workflow-runs list, an attempt's job list and the
// rerun POST, which it refuses for a successful run as GitHub does.
type futilityAPI struct {
	mu sync.Mutex
	// run is the run GET's body; runAt, where set, replaces it per GET by call number.
	run   map[string]any
	runAt func(call int) map[string]any
	// newest is the list's only run, nil for an empty list, and runsAt replaces it per call
	// by call number and query; listStatus overrides 200.
	newest     map[string]any
	runsAt     func(call int, query neturl.Values) []map[string]any
	listStatus int
	// jobs is the attempt's job list, and jobsAt replaces it per call by call number;
	// jobsStatus overrides 200, and jobsStatusAt replaces it the same way. jobPage, where
	// set, serves the list in pages that size.
	jobs         []map[string]any
	jobsAt       func(call int) []map[string]any
	jobsStatus   int
	jobsStatusAt func(call int) int
	jobPage      int

	gets, lists, jobLists, reruns atomic.Int64
	lastJobsPath                  string
	lastListQuery                 string
}

func (a *futilityAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs"):
			a.reruns.Add(1)
			if a.run["conclusion"] == "success" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			a.lastJobsPath = r.URL.Path
			status := a.jobsStatus
			if a.jobsStatusAt != nil {
				status = a.jobsStatusAt(int(a.jobLists.Load()) + 1)
			}
			a.jobLists.Add(1)
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			jobs := a.jobs
			if a.jobsAt != nil {
				jobs = a.jobsAt(int(a.jobLists.Load()))
			}
			page := jobs
			if a.jobPage > 0 {
				n, _ := strconv.Atoi(r.URL.Query().Get("page"))
				from := min((n-1)*a.jobPage, len(jobs))
				page = jobs[from:min(from+a.jobPage, len(jobs))]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(jobs), "jobs": page})
		case strings.Contains(r.URL.Path, "/actions/workflows/"):
			a.lists.Add(1)
			a.lastListQuery = r.URL.RawQuery
			if a.listStatus != 0 {
				w.WriteHeader(a.listStatus)
				return
			}
			runs := []map[string]any{}
			if a.newest != nil {
				runs = append(runs, a.newest)
			}
			if a.runsAt != nil {
				runs = a.runsAt(int(a.lists.Load()), r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
		default:
			body := a.run
			if a.runAt != nil {
				body = a.runAt(int(a.gets.Load()) + 1)
			}
			a.gets.Add(1)
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failedRun is a concluded, failed attempt-1 push run created an hour ago.
func failedRun(now time.Time, overrides map[string]any) map[string]any {
	run := map[string]any{
		"id": 4242, "status": "completed", "conclusion": "failure", "run_attempt": 1,
		"workflow_id": 7, "created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"event": "push", "head_branch": "feature", "head_sha": "aaaaaaaaaaaaaaaa",
		"head_repository": map[string]any{"full_name": "myorg/myrepo"},
	}
	for k, v := range overrides {
		run[k] = v
	}
	return run
}

func futilityFixture(t *testing.T, api *futilityAPI) (*Provisioner, *stubTarget) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	p := ledgerProvisioner(c, api.server(t))
	return p, &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}
}

// assertWithheld asserts a withhold for reason under cause, and whether a slot was spent.
func assertWithheld(t *testing.T, p *Provisioner, target *stubTarget, api *futilityAPI, cause, reason string, slotSpent bool) {
	t.Helper()
	assert.Equal(t, int64(0), api.reruns.Load(), "a withheld re-run makes no rerun call")
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunWithheld.WithLabelValues(
		"team-a", "gpu", cause, reason)))
	assert.Contains(t, target.events, "EvictionRerunWithheld")
	l, _, err := p.readRerunLedger(context.Background(), target)
	require.NoError(t, err)
	if slotSpent {
		require.Contains(t, l.Reruns, "4242")
		assert.Equal(t, 1, l.Reruns["4242"].Count)
		return
	}
	assert.NotContains(t, l.Reruns, "4242", "a re-run withheld before the reservation spends no slot")
	assert.Equal(t, float64(0), testutil.ToFloat64(p.Metrics.EvictionRetries.WithLabelValues(
		"team-a", "gpu", cause)))
}

func TestFutileRerun_WithheldBeforeTheReservation(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		run    map[string]any
		newest map[string]any
		reason string
	}{
		{"past GitHub's 50 re-runs", map[string]any{"run_attempt": 51}, nil, rerunWithheldReasonAttemptLimit},
		{"created over 30 days ago", map[string]any{"created_at": now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)}, nil, rerunWithheldReasonRunTooOld},
		{"a newer commit's run on the branch", nil, map[string]any{
			"id": 5000, "head_sha": "bbbbbbbbbbbbbbbb", "created_at": now.Format(time.RFC3339),
			"head_repository": map[string]any{"full_name": "myorg/myrepo"},
		}, rerunWithheldReasonSuperseded},
		{"cancelled by somebody other than GAG", map[string]any{"conclusion": "cancelled"}, nil, rerunWithheldReasonRunCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &futilityAPI{run: failedRun(now, tc.run), newest: tc.newest}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

			assertWithheld(t, p, target, api, recoveryCauseEviction, tc.reason, false)
		})
	}
}

// TestFutileRerun_ReRunsAtEachBoundary is the other side of every check: the last run
// GitHub still accepts is re-run, and nothing is withheld.
func TestFutileRerun_ReRunsAtEachBoundary(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		run        map[string]any
		newest     map[string]any
		listStatus int
		wantLists  int64
	}{
		{"attempt 51 is GitHub's last", map[string]any{"run_attempt": 50}, nil, 0, 1},
		{"created 29 days ago", map[string]any{"created_at": now.Add(-29 * 24 * time.Hour).Format(time.RFC3339)}, nil, 0, 1},
		{"the newest run is this one", nil, map[string]any{
			"id": 4242, "head_sha": "aaaaaaaaaaaaaaaa", "created_at": now.Add(-time.Hour).Format(time.RFC3339),
			"head_repository": map[string]any{"full_name": "myorg/myrepo"},
		}, 0, 1},
		{"a newer run of the same commit", nil, map[string]any{
			"id": 5000, "head_sha": "aaaaaaaaaaaaaaaa", "created_at": now.Format(time.RFC3339),
			"head_repository": map[string]any{"full_name": "myorg/myrepo"},
		}, 0, 1},
		{"a fork's branch of the same name", nil, map[string]any{
			"id": 5000, "head_sha": "bbbbbbbbbbbbbbbb", "created_at": now.Format(time.RFC3339),
			"head_repository": map[string]any{"full_name": "someone/myrepo"},
		}, 0, 1},
		{"an unreadable list does not withhold", nil, nil, http.StatusInternalServerError, 1},
		{"a schedule run is never superseded", map[string]any{"event": "schedule"}, map[string]any{
			"id": 5000, "head_sha": "bbbbbbbbbbbbbbbb", "created_at": now.Format(time.RFC3339),
			"head_repository": map[string]any{"full_name": "myorg/myrepo"},
		}, 0, 0},
		{"a run GitHub sent no fields for", map[string]any{
			"created_at": nil, "event": nil, "workflow_id": nil, "head_branch": nil, "head_sha": nil, "head_repository": nil,
		}, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &futilityAPI{run: failedRun(now, tc.run), newest: tc.newest, listStatus: tc.listStatus}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

			assert.Equal(t, int64(1), api.reruns.Load())
			assert.Equal(t, tc.wantLists, api.lists.Load())
			assert.NotContains(t, target.events, "EvictionRerunWithheld")
		})
	}
}

// secondAttempt is failedRun re-run ten minutes ago, for a job assigned an hour ago.
func secondAttempt(now time.Time, overrides map[string]any) map[string]any {
	run := failedRun(now, map[string]any{"run_attempt": 2, "run_started_at": now.Add(-10 * time.Minute).Format(time.RFC3339)})
	for k, v := range overrides {
		run[k] = v
	}
	return run
}

// ranInAttempt is a job secondAttempt ran, and carriedJob one it did not, in the shapes
// GitHub lists them: a carried job has the new attempt's created_at and the old one's
// runner and completed_at (run 45310657002, measured 2026-11-23).
func ranInAttempt(now time.Time, name, runner, conclusion string) map[string]any {
	return map[string]any{"name": name, "runner_name": runner, "conclusion": conclusion, "run_attempt": 2,
		"created_at":   now.Add(-10 * time.Minute).Format(time.RFC3339),
		"completed_at": now.Add(-5 * time.Minute).Format(time.RFC3339)}
}

func carriedJob(now time.Time, name, runner, conclusion string) map[string]any {
	return map[string]any{"name": name, "runner_name": runner, "conclusion": conclusion, "run_attempt": 2,
		"created_at":   now.Add(-10 * time.Minute).Format(time.RFC3339),
		"completed_at": now.Add(-50 * time.Minute).Format(time.RFC3339)}
}

// TestFutileRerun_AlreadyRerun covers a run that moved past the attempt the job failed in:
// the re-run is withheld only where the latest attempt's jobs show the job has one after
// it, whatever the run's attempt says.
func TestFutileRerun_AlreadyRerun(t *testing.T) {
	now := time.Now()
	lost := rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name string
		d    rerunDisruption
		jobs []map[string]any
	}{
		{"the job ran again on another runner", lost, []map[string]any{
			ranInAttempt(now, "build", "gpu-new", "failure"), carriedJob(now, "lint", "gpu-x", "success")}},
		{"the job is queued again, named differently by the jobs API", lost, []map[string]any{
			{"name": "caller / build", "runner_name": nil, "conclusion": nil, "created_at": now.Format(time.RFC3339), "completed_at": nil},
			carriedJob(now, "lint", "gpu-x", "failure")}},
		{"the vanished arm, which knows no name or runner, and every failure was re-run",
			rerunDisruption{key: "pod-a", assignedAt: now.Add(-time.Hour)},
			[]map[string]any{ranInAttempt(now, "build", "gpu-new", "failure"), carriedJob(now, "lint", "gpu-x", "success")}},
		{"a known attempt, and the job ran again while another's failure was carried",
			rerunDisruption{key: "pod-a", job: "build", failedAttempt: 1},
			[]map[string]any{ranInAttempt(now, "build", "gpu-new", "failure"), carriedJob(now, "lint", "gpu-x", "failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &futilityAPI{run: secondAttempt(now, nil), jobs: tc.jobs}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseVanished, tc.d)

			assertWithheld(t, p, target, api, recoveryCauseVanished, rerunWithheldReasonAlreadyRerun, false)
			assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/attempts/2/jobs", api.lastJobsPath)
			assert.Contains(t, strings.Join(target.messages, "\n"), "up to attempt 2, and that re-run covers the lost job")
		})
	}
}

// TestFutileRerun_ReRunsAJobTheLaterAttemptDidNotRun is the other side: somebody re-ran
// an unrelated job, or nothing says the job was re-run, and the re-run goes ahead.
func TestFutileRerun_ReRunsAJobTheLaterAttemptDidNotRun(t *testing.T) {
	now := time.Now()
	lost := rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-time.Hour)}
	unrelated := []map[string]any{ranInAttempt(now, "lint", "gpu-new", "success"), carriedJob(now, "build", "gpu-old", "failure")}
	for _, tc := range []struct {
		name       string
		d          rerunDisruption
		run        map[string]any
		jobs       []map[string]any
		jobsStatus int
		jobPage    int
		wantLists  int64
	}{
		{name: "an unrelated job was re-run", d: lost, jobs: unrelated, wantLists: 1},
		{name: "an unrelated job was re-run, found on the second page", d: lost, jobs: unrelated, jobPage: 1, wantLists: 2},
		{name: "an unrelated job was re-run, and the attempt is known", d: rerunDisruption{key: "pod-a", job: "build", failedAttempt: 1},
			jobs: unrelated, wantLists: 1},
		{name: "an idle runner took the job in the latest attempt", d: lost,
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-old", "failure")}, wantLists: 1},
		{name: "two jobs share the name, and one failure was carried", d: rerunDisruption{key: "pod-a", job: "build", assignedAt: now.Add(-time.Hour)},
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-new", "success"), carriedJob(now, "build", "gpu-old", "failure")}, wantLists: 1},
		{name: "the vanished arm, with a failure carried", d: rerunDisruption{key: "pod-a", assignedAt: now.Add(-time.Hour)},
			jobs: unrelated, wantLists: 1},
		{name: "an unreadable list does not withhold", d: lost, jobsStatus: http.StatusInternalServerError, wantLists: 1},
		{name: "a forbidden list does not withhold", d: lost, jobsStatus: http.StatusForbidden, wantLists: 1},
		{name: "an empty list does not withhold", d: lost, wantLists: 1},
		{name: "the attempt did not move, so no list is read", d: lost, run: failedRun(now, nil),
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-new", "failure")}},
		{name: "the attempt started before the job was assigned", d: rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-9 * time.Minute)},
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-new", "failure")}},
		{name: "the attempt started within the clock skew of the assignment", d: rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-10*time.Minute - attemptStartSkew + 2*time.Second)},
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-new", "failure")}},
		{name: "GitHub sent no run_started_at", d: lost, run: secondAttempt(now, map[string]any{"run_started_at": nil}),
			jobs: []map[string]any{ranInAttempt(now, "build", "gpu-new", "failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			if run == nil {
				run = secondAttempt(now, nil)
			}
			api := &futilityAPI{run: run, jobs: tc.jobs, jobsStatus: tc.jobsStatus, jobPage: tc.jobPage}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, tc.d)

			assert.Equal(t, int64(1), api.reruns.Load())
			assert.Equal(t, tc.wantLists, api.jobLists.Load())
			assert.NotContains(t, target.events, "EvictionRerunWithheld")
			l, _, err := p.readRerunLedger(context.Background(), target)
			require.NoError(t, err)
			require.Contains(t, l.Reruns, "4242")
			assert.Contains(t, l.Reruns["4242"].Attempts, strconv.Itoa(run["run_attempt"].(int)),
				"the slot is keyed by the attempt the re-run starts from")
		})
	}
}

// TestFutileRerun_AYoungAttemptsListIsReadAgain: GitHub fills a new attempt's list over
// several seconds, so a job missing from it says nothing until it has settled.
func TestFutileRerun_AYoungAttemptsListIsReadAgain(t *testing.T) {
	now := time.Now()
	lint, lost := ranInAttempt(now, "lint", "gpu-new", "success"), carriedJob(now, "build", "gpu-old", "failure")
	otherLost := carriedJob(now, "test", "gpu-x", "failure")
	again := ranInAttempt(now, "build", "gpu-new", "failure")
	young := secondAttempt(now, map[string]any{"status": "in_progress", "conclusion": nil,
		"run_started_at": now.Add(-jobListSettle + 10*time.Second).Format(time.RFC3339)})
	for _, tc := range []struct {
		name         string
		d            rerunDisruption
		first, later []map[string]any
		wantReruns   int64
	}{
		{"its runner's row is not listed yet", rerunDisruption{job: "build", runner: "gpu-old"}, []map[string]any{lint, otherLost}, []map[string]any{lint, otherLost, lost}, 1},
		{"nothing is listed yet", rerunDisruption{job: "build", runner: "gpu-old"}, nil, []map[string]any{lint, lost}, 1},
		{"no carried failure is listed yet, and no runner is known", rerunDisruption{job: "build"}, []map[string]any{lint}, []map[string]any{lint, lost}, 1},
		{"one of two jobs sharing the name is listed", rerunDisruption{job: "build"}, []map[string]any{again, otherLost}, []map[string]any{again, otherLost, lost}, 1},
		{"the settled list shows it was re-run", rerunDisruption{job: "build", runner: "gpu-old"}, []map[string]any{lint}, []map[string]any{lint, again}, 0},
	} {
		// Before the reservation the read that finds the moved attempt is the first; in the
		// loop it is the second.
		for where, movedAt := range map[string]int{"before the reservation": 1, "in the loop": 2} {
			t.Run(tc.name+", "+where, func(t *testing.T) {
				api := &futilityAPI{
					runAt: func(call int) map[string]any {
						switch {
						case call < movedAt:
							return failedRun(now, nil)
						case call == movedAt:
							return young
						}
						return secondAttempt(now, nil)
					},
					jobsAt: func(call int) []map[string]any {
						if call == 1 {
							return tc.first
						}
						return tc.later
					},
				}
				p, target := futilityFixture(t, api)
				d := tc.d
				d.key, d.assignedAt = "pod-a", now.Add(-time.Hour)

				evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, d)

				assert.Equal(t, tc.wantReruns, api.reruns.Load())
				if tc.wantReruns == 0 && movedAt == 1 {
					assertWithheld(t, p, target, api, recoveryCauseEviction, rerunWithheldReasonAlreadyRerun, false)
					return
				}
				assert.NotContains(t, target.events, "EvictionRerunWithheld")
			})
		}
	}
}

// TestFutileRerun_AConcludedAttemptsListIsSettled: an attempt that concluded has every job
// listed, however recently it started.
func TestFutileRerun_AConcludedAttemptsListIsSettled(t *testing.T) {
	now := time.Now()
	started := now.Add(-10 * time.Second).Format(time.RFC3339)
	api := &futilityAPI{run: secondAttempt(now, map[string]any{"run_started_at": started}), jobs: []map[string]any{
		{"name": "build", "runner_name": "gpu-new", "conclusion": "failure", "created_at": started, "completed_at": now.Format(time.RFC3339)}}}
	p, target := futilityFixture(t, api)

	evictScaleSet(p, target, "4242", 2, recoveryCauseEviction,
		rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-time.Hour)})

	assertWithheld(t, p, target, api, recoveryCauseEviction, rerunWithheldReasonAlreadyRerun, false)
}

// TestFutileRerun_AJobThatNeverRanIsNotCarried: GitHub lists a job that never got a runner
// as completed a second before it was created, in the attempt that cancelled it
// (apache/airflow run 45322354372, measured 2026-11-23).
func TestFutileRerun_AJobThatNeverRanIsNotCarried(t *testing.T) {
	now := time.Now()
	api := &futilityAPI{run: secondAttempt(now, nil), jobs: []map[string]any{
		ranInAttempt(now, "build", "gpu-new", "failure"),
		{"name": "notify", "runner_name": "", "conclusion": "cancelled",
			"created_at":   now.Add(-4 * time.Minute).Format(time.RFC3339),
			"completed_at": now.Add(-4*time.Minute - time.Second).Format(time.RFC3339)},
	}}
	p, target := futilityFixture(t, api)

	evictScaleSet(p, target, "4242", 2, recoveryCauseVanished, rerunDisruption{key: "pod-a", assignedAt: now.Add(-time.Hour)})

	assertWithheld(t, p, target, api, recoveryCauseVanished, rerunWithheldReasonAlreadyRerun, false)
}

// TestFutileRerun_AnUndecidedMoveDoesNotReRunAKnownAttempt: the pre-reservation check
// decides nothing on a list it cannot read, but an arm that knows its attempt still
// stands down in the loop rather than risk a second re-run of it.
func TestFutileRerun_AnUndecidedMoveDoesNotReRunAKnownAttempt(t *testing.T) {
	now := time.Now()
	for name, api := range map[string]*futilityAPI{
		"the list is refused": {run: secondAttempt(now, nil), jobsStatus: http.StatusForbidden},
		"GitHub sent no run_started_at": {run: secondAttempt(now, map[string]any{"run_started_at": nil}),
			jobs: []map[string]any{carriedJob(now, "build", "gpu-old", "failure")}},
	} {
		t.Run(name, func(t *testing.T) {
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseAbandoned, rerunDisruption{key: "pod-a", job: "build", failedAttempt: 1})

			assert.Equal(t, int64(0), api.reruns.Load())
			assert.NotContains(t, target.events, "EvictionRerunWithheld")
			assert.NotContains(t, target.events, "EvictionRerunFailed")
		})
	}
}

// TestFutileRerun_TheLoopAsksAboutTheJobToo covers a run re-run while the recovery waits
// for GitHub to accept its own: an unrelated job's re-run does not cover this one, and a
// list GitHub fails to serve is asked for again.
func TestFutileRerun_TheLoopAsksAboutTheJobToo(t *testing.T) {
	now := time.Now()
	lost := rerunDisruption{key: "pod-a", job: "build", runner: "gpu-old", assignedAt: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name       string
		jobs       []map[string]any
		wantReruns int64
	}{
		{"an unrelated job was re-run", []map[string]any{ranInAttempt(now, "lint", "gpu-new", "success"), carriedJob(now, "build", "gpu-old", "failure")}, 1},
		{"the job was re-run", []map[string]any{ranInAttempt(now, "build", "gpu-new", "failure")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &futilityAPI{
				runAt: func(call int) map[string]any {
					if call == 1 {
						return failedRun(now, nil)
					}
					return secondAttempt(now, nil)
				},
				jobs: tc.jobs,
				jobsStatusAt: func(call int) int {
					if call == 1 {
						return http.StatusBadGateway
					}
					return 0
				},
			}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, lost)

			assert.Equal(t, tc.wantReruns, api.reruns.Load())
			assert.Equal(t, int64(2), api.jobLists.Load())
			assert.NotContains(t, target.events, "EvictionRerunFailed")
		})
	}
}

// TestFutileRerun_RunSucceeded covers both reads: a run already successful when it is
// detected spends no slot, and one that succeeds while the recovery waits makes no call.
func TestFutileRerun_RunSucceeded(t *testing.T) {
	now := time.Now()
	succeeded := failedRun(now, map[string]any{"conclusion": "success"})

	t.Run("at detection", func(t *testing.T) {
		api := &futilityAPI{run: succeeded}
		p, target := futilityFixture(t, api)
		evictScaleSet(p, target, "4242", 2, recoveryCauseVanished, rerunDisruption{key: "pod-a"})
		assertWithheld(t, p, target, api, recoveryCauseVanished, rerunWithheldReasonRunSucceeded, false)
		assert.NotContains(t, target.events, "EvictionRerunFailed")
	})
	t.Run("while waiting", func(t *testing.T) {
		api := &futilityAPI{run: succeeded, runAt: func(call int) map[string]any {
			if call == 1 {
				return failedRun(now, map[string]any{"status": "in_progress", "conclusion": nil})
			}
			return succeeded
		}}
		p, target := futilityFixture(t, api)
		evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})
		assertWithheld(t, p, target, api, recoveryCauseEviction, rerunWithheldReasonRunSucceeded, true)
		assert.NotContains(t, target.events, "EvictionRerunFailed")
	})
}

// startupTarget is a stubTarget whose set answers the startup verdict.
type startupTarget struct {
	*stubTarget
	failing bool
}

func (s startupTarget) WorkersFailingToStart(context.Context) (string, bool) {
	return "1 worker pod(s) were placed on a node but could not be started", s.failing
}

func TestFutileRerun_WorkerUnstartable(t *testing.T) {
	for _, failing := range []bool{true, false} {
		t.Run("failing="+strconv.FormatBool(failing), func(t *testing.T) {
			api := &futilityAPI{run: failedRun(time.Now(), map[string]any{"conclusion": "cancelled", "run_attempt": 3})}
			p, target := futilityFixture(t, api)
			require.NoError(t, p.recordSelfCancel(context.Background(), target, "4242", 3))

			evictScaleSet(p, startupTarget{target, failing}, "4242", 2, recoveryCauseAbandoned, rerunDisruption{key: "pod-a", failedAttempt: 3})

			if !failing {
				assert.Equal(t, int64(1), api.reruns.Load())
				assert.NotContains(t, target.events, "EvictionRerunWithheld")
				return
			}
			assert.Equal(t, int64(0), api.reruns.Load())
			assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunWithheld.WithLabelValues(
				"team-a", "gpu", recoveryCauseAbandoned, rerunWithheldReasonWorkerUnstartable)))
			assert.Contains(t, target.events, "EvictionRerunWithheld")
			assert.Equal(t, float64(0), testutil.ToFloat64(p.Metrics.EvictionRetries.WithLabelValues(
				"team-a", "gpu", recoveryCauseAbandoned)), "no slot is spent")
			assert.Contains(t, strings.Join(target.messages, "\n"), "fix the image or its pull credentials, then re-run it by hand")
		})
	}
}

const groupBase = "cccccccccccccccccccccccccccccccccccccccc"

// groupRun is a merge-group run on branch, at sha, created age before now.
func groupRun(now time.Time, branch, sha string, age time.Duration) map[string]any {
	return map[string]any{"id": 5000, "event": "merge_group", "head_branch": branch, "head_sha": sha,
		"created_at": now.Add(-age).Format(time.RFC3339), "head_repository": map[string]any{"full_name": "myorg/myrepo"}}
}

// failedGroupRun is failedRun in the merge queue, for pull request 217.
func failedGroupRun(now time.Time, overrides map[string]any) map[string]any {
	run := failedRun(now, map[string]any{"event": "merge_group", "head_branch": "gh-readonly-queue/main/pr-217-" + groupBase})
	for k, v := range overrides {
		run[k] = v
	}
	return run
}

// TestFutileRerun_ADisplacedMergeGroup covers the queue abandoning a group: it builds
// another for the same pull request, on the same base or another, and leaves this one's
// runs going. Found at detection no slot is spent; found in the loop one already is.
func TestFutileRerun_ADisplacedMergeGroup(t *testing.T) {
	now := time.Now()
	for name, newer := range map[string]map[string]any{
		"rebuilt on another base":  groupRun(now, "gh-readonly-queue/main/pr-217-dddddddddddddddddddddddddddddddddddddddd", "bbbbbbbbbbbbbbbb", time.Minute),
		"requeued on the same one": groupRun(now, "gh-readonly-queue/main/pr-217-"+groupBase, "bbbbbbbbbbbbbbbb", time.Minute),
	} {
		for _, inLoop := range []bool{false, true} {
			t.Run(name+", in the loop="+strconv.FormatBool(inLoop), func(t *testing.T) {
				api := &futilityAPI{
					run: failedGroupRun(now, nil),
					runsAt: func(call int, _ neturl.Values) []map[string]any {
						if inLoop && call == 1 {
							return nil
						}
						return []map[string]any{newer}
					},
				}
				p, target := futilityFixture(t, api)

				evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

				assertWithheld(t, p, target, api, recoveryCauseEviction, rerunWithheldReasonSuperseded, inLoop)
				assert.Contains(t, strings.Join(target.messages, "\n"), "another group for the pull request (run 5000, commit bbbbbbb)")
				q, err := neturl.ParseQuery(api.lastListQuery)
				require.NoError(t, err)
				assert.Equal(t, neturl.Values{"event": {"merge_group"}, "per_page": {"30"}, "page": {"1"},
					"created": {">=" + now.Add(-time.Hour).UTC().Format(time.RFC3339)}}, q)
			})
		}
	}
}

// TestFutileRerun_AMergeGroupNothingDisplacedIsReRun is the other side: no newer group of
// this pull request exists, so the queue still waits on this one or has merged it.
func TestFutileRerun_AMergeGroupNothingDisplacedIsReRun(t *testing.T) {
	now := time.Now()
	const other = "bbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		name       string
		run        map[string]any
		newer      map[string]any
		listStatus int
		wantLists  int64
	}{
		{"no newer run", nil, nil, 0, 2},
		{"another pull request's group", nil, groupRun(now, "gh-readonly-queue/main/pr-21-"+groupBase, other, time.Minute), 0, 2},
		{"a pull request of that number in another queue", nil, groupRun(now, "gh-readonly-queue/release/pr-217-"+groupBase, other, time.Minute), 0, 2},
		{"a group at the same commit", nil, groupRun(now, "gh-readonly-queue/main/pr-217-"+groupBase, "aaaaaaaaaaaaaaaa", time.Minute), 0, 2},
		{"an older group", nil, groupRun(now, "gh-readonly-queue/main/pr-217-"+groupBase, other, 2*time.Hour), 0, 2},
		{"a branch that is not a queue's", nil, groupRun(now, "pr-217-"+groupBase, other, time.Minute), 0, 2},
		{"an unreadable list does not withhold", nil, nil, http.StatusInternalServerError, 2},
		{"a run still going is asked about once", map[string]any{"status": "in_progress", "conclusion": nil}, nil, 0, 1},
		{"its own branch is not a queue's", map[string]any{"head_branch": "pr-217-" + groupBase},
			groupRun(now, "pr-217-"+groupBase, other, time.Minute), 0, 0},
		{"GitHub sent no created_at", map[string]any{"created_at": nil},
			groupRun(now, "gh-readonly-queue/main/pr-217-"+groupBase, other, time.Minute), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &futilityAPI{run: failedGroupRun(now, tc.run), newest: tc.newer, listStatus: tc.listStatus}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

			assert.Equal(t, int64(1), api.reruns.Load())
			assert.Equal(t, tc.wantLists, api.lists.Load(), "once at detection, once before a concluded run's re-run call")
			assert.NotContains(t, target.events, "EvictionRerunWithheld")
		})
	}
}

// TestFutileRerun_TheMergeGroupSearchIsPagedAndBounded: a busy queue's newer runs fill
// several pages, newest first, and the search reads only so many.
func TestFutileRerun_TheMergeGroupSearchIsPagedAndBounded(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		onPage     int
		wantReruns int64
	}{{maxMergeGroupRunPages, 0}, {maxMergeGroupRunPages + 1, 1}} {
		t.Run("on page "+strconv.Itoa(tc.onPage), func(t *testing.T) {
			api := &futilityAPI{run: failedGroupRun(now, nil), runsAt: func(_ int, q neturl.Values) []map[string]any {
				runs := make([]map[string]any, mergeGroupRunsPerPage)
				for i := range runs {
					runs[i] = groupRun(now, "gh-readonly-queue/main/pr-300-"+groupBase, "eeeeeeeeeeeeeeee", time.Minute)
				}
				if q.Get("page") == strconv.Itoa(tc.onPage) {
					runs[7] = groupRun(now, "gh-readonly-queue/main/pr-217-"+groupBase, "bbbbbbbbbbbbbbbb", time.Minute)
				}
				return runs
			}}
			p, target := futilityFixture(t, api)

			evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

			assert.Equal(t, tc.wantReruns, api.reruns.Load())
			assert.LessOrEqual(t, api.lists.Load(), int64(2*maxMergeGroupRunPages))
		})
	}
}

// TestFutileRerun_SupersededListIsFilteredByBranchAndEvent pins the list query: without
// the filters the newest run of the workflow is any branch's.
func TestFutileRerun_SupersededListIsFilteredByBranchAndEvent(t *testing.T) {
	api := &futilityAPI{run: failedRun(time.Now(), map[string]any{"event": "pull_request", "head_branch": "fix/a b"})}
	p, target := futilityFixture(t, api)

	evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

	assert.Equal(t, "branch=fix%2Fa+b&event=pull_request&per_page=1", api.lastListQuery)
}

// TestFutileRerun_GAGsOwnCancelIsNotWithheld is the abandoned arm's case: the run is
// cancelled because GAG force-cancelled that attempt, so its re-run goes ahead on every
// arm with no exemption.
func TestFutileRerun_GAGsOwnCancelIsNotWithheld(t *testing.T) {
	api := &futilityAPI{run: failedRun(time.Now(), map[string]any{"conclusion": "cancelled", "run_attempt": 3})}
	p, target := futilityFixture(t, api)
	require.NoError(t, p.recordSelfCancel(context.Background(), target, "4242", 3))

	evictScaleSet(p, target, "4242", 2, recoveryCauseAbandoned, rerunDisruption{key: "pod-a", failedAttempt: 3})

	assert.Equal(t, int64(1), api.reruns.Load())
	assert.NotContains(t, target.events, "EvictionRerunWithheld")
}

// TestFutileRerun_ACancelWhileWaitingIsHonouredOnEveryArm covers the cancel that lands
// after detection, which only the re-run loop's read sees: an eviction's run is still in
// progress when it is detected and is cancelled before GitHub accepts the re-run. A check on
// the deletion arm alone would let the eviction arm re-run a cancelled run.
func TestFutileRerun_ACancelWhileWaitingIsHonouredOnEveryArm(t *testing.T) {
	now := time.Now()
	api := &futilityAPI{runAt: func(call int) map[string]any {
		if call == 1 {
			return failedRun(now, map[string]any{"status": "in_progress", "conclusion": nil})
		}
		return failedRun(now, map[string]any{"conclusion": "cancelled"})
	}}
	p, target := futilityFixture(t, api)

	evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

	// The slot was reserved at detection, minutes before the run concluded.
	assertWithheld(t, p, target, api, recoveryCauseEviction, rerunWithheldReasonRunCancelled, true)
}
