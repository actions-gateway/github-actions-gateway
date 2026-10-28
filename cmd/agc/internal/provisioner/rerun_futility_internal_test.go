package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// Q1181: the known-futile checks on the scale-set re-run path. Each case drives one
// disruption through handleEviction against a fake GitHub and asserts both halves of a
// withhold: no re-run call, and no budget slot spent.

// futilityAPI fakes the run GET, the workflow-runs list and the rerun POST.
type futilityAPI struct {
	mu sync.Mutex
	// run is the run GET's body; runAt, where set, replaces it per GET by call number.
	run   map[string]any
	runAt func(call int) map[string]any
	// newest is the list's only run, nil for an empty list; listStatus overrides 200.
	newest     map[string]any
	listStatus int

	gets, lists, reruns atomic.Int64
	lastListQuery       string
}

func (a *futilityAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs"):
			a.reruns.Add(1)
			w.WriteHeader(http.StatusCreated)
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
		"team-a", "gpu", evictionTierScaleSet, cause, reason)))
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
		"team-a", "gpu", evictionTierScaleSet, cause)))
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
// progress when it is detected and is cancelled before GitHub accepts the re-run. Before
// Q1181 only the deletion arm checked, so the eviction arm re-ran a cancelled run.
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
