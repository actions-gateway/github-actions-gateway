package provisioner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestHandleEviction_ConcurrentSameRunRespectsBudget is the Q106 regression
// test. handleEviction read-modify-writes a per-run eviction counter; without
// per-run serialization two concurrent evictions of the same run_id can both
// read the same count, both pass the budget check, and both fire a rerun —
// exceeding maxRetries.
//
// It spawns many concurrent evictions of one run_id against a counting fake for
// the rerun API and asserts the invariant: the rerun API is called at most
// maxRetries times. Run under -race (make test-race) — this is the data-race
// class -race exists to catch.
//
// Half the evictions arrive on the classic tier and half on the scale-set tier, which
// is the Q417 half of the invariant: the budget is keyed by run_id alone, so one run's
// worth of concurrent evictions can never collectively exceed maxRetries even when the
// two tiers detect them by different machinery. A budget that were per-tier would
// silently allow 2×maxRetries re-runs for a run whose workers span both.
func TestHandleEviction_ConcurrentSameRunRespectsBudget(t *testing.T) {
	const (
		maxRetries  = 2
		concurrency = 64
	)

	var rerunCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rerunCount.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	m := &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_q106_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_q106_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
	}

	p := &Provisioner{
		Metrics:      m,
		TokenFunc:    func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL: srv.URL,
		HTTPClient:   srv.Client(),
	}
	rg := &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "mygroup", Namespace: "ns"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// All goroutines target the same run_id (and therefore the same lock shard),
	// so the read-modify-write is maximally contended — exactly the interleaving
	// the fix must defend against. retryDelay=0 keeps the test fast.
	// Both tiers AND both disruption causes are interleaved, because the budget is one
	// budget across every combination: a run that is alternately evicted and preempted
	// on either tier must still be capped at maxRetries re-runs in total (Q497).
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		tier := evictionTierClassic
		if i%2 == 1 {
			tier = evictionTierScaleSet
		}
		cause := recoveryCauseEviction
		if i%3 == 1 {
			cause = recoveryCausePreemption
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-p.handleEviction(context.Background(), p.runnerGroupTarget(rg), "owner", "repo", "12345", log, maxRetries, 0, tier, cause)
		}()
	}
	wg.Wait()

	got := rerunCount.Load()
	require.LessOrEqualf(t, got, int64(maxRetries),
		"rerun API must be called at most maxRetries (%d) times across BOTH tiers, got %d", maxRetries, got)
	// With concurrency far above the budget the budget should be fully consumed.
	require.Equal(t, int64(maxRetries), got,
		"budget should be fully used when evictions far exceed it")

	// The EvictionRetries metric is incremented exactly once per reserved slot, so every
	// tier×cause series must SUM to the number of rerun calls — the labels split the
	// reporting without splitting the budget.
	var total float64
	for _, tier := range []string{evictionTierClassic, evictionTierScaleSet} {
		for _, cause := range []string{recoveryCauseEviction, recoveryCausePreemption} {
			total += testutil.ToFloat64(m.EvictionRetries.WithLabelValues("ns", "mygroup", tier, cause))
		}
	}
	assert.Equal(t, float64(got), total)
}

// TestHandleEviction_BudgetIsHardCap verifies that the eviction-retry budget is
// a hard lifetime cap: repeated (sequential) evictions of the same run never
// fire more than maxRetries reruns. This guards the Q106 fix's removal of the
// delete-on-exhaustion that previously reset the budget on the next eviction.
func TestHandleEviction_BudgetIsHardCap(t *testing.T) {
	const (
		maxRetries = 1
		evictions  = 5
	)

	var rerunCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rerunCount.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Provisioner{
		TokenFunc:    func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL: srv.URL,
		HTTPClient:   srv.Client(),
	}
	rg := &v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 0; i < evictions; i++ {
		<-p.handleEviction(context.Background(), p.runnerGroupTarget(rg), "owner", "repo", "999", log, maxRetries, 0, evictionTierClassic, recoveryCauseEviction)
	}

	assert.Equal(t, int64(maxRetries), rerunCount.Load(),
		"budget must not refill across sequential evictions")
}

// TestSweepEvictionCounts_ReclaimsExpiredKeepsFresh verifies the Q141 cleanup:
// the background sweep deletes per-run counters older than the TTL while
// retaining entries still inside the window. Bounding map growth this way is the
// whole point — Q106 made the counter a hard lifetime cap with no delete on
// exhaustion, so without the sweep one entry leaks per distinct evicted run_id.
func TestSweepEvictionCounts_ReclaimsExpiredKeepsFresh(t *testing.T) {
	const ttl = 24 * time.Hour
	base := time.Unix(1_700_000_000, 0)
	clock := base
	p := &Provisioner{now: func() time.Time { return clock }}

	// Reserve a slot for an "old" run at base.
	_, ok := p.reserveEvictionRetry("old", 2)
	require.True(t, ok)

	// Advance past the TTL, then touch a "fresh" run at the new now.
	clock = base.Add(ttl + time.Hour)
	_, ok = p.reserveEvictionRetry("fresh", 2)
	require.True(t, ok)

	n := p.sweepEvictionCounts(ttl)
	assert.Equal(t, 1, n, "exactly the expired entry is reclaimed")

	_, oldPresent := p.evictionCounts.Load("old")
	assert.False(t, oldPresent, "expired entry deleted")
	_, freshPresent := p.evictionCounts.Load("fresh")
	assert.True(t, freshPresent, "in-TTL entry retained")
}

// TestSweepEvictionCounts_RefreshKeepsLiveRunPinned is the core Q141/Q106
// invariant test. A still-evicting run is provably live, so the sweep must never
// reclaim its counter — reclaiming would let the next eviction refill the retry
// budget to zero (the Q106 over-budget bug). reserveEvictionRetry refreshes
// lastUpdate on every eviction, including the exhausted/rejected path, so the
// entry stays out of the sweep as long as evictions keep arriving within the TTL.
func TestSweepEvictionCounts_RefreshKeepsLiveRunPinned(t *testing.T) {
	const (
		ttl        = 24 * time.Hour
		maxRetries = 1
	)
	base := time.Unix(1_700_000_000, 0)
	clock := base
	p := &Provisioner{now: func() time.Time { return clock }}

	// Exhaust the budget at base.
	_, ok := p.reserveEvictionRetry("live", maxRetries)
	require.True(t, ok)
	_, ok = p.reserveEvictionRetry("live", maxRetries)
	require.False(t, ok, "budget exhausted after maxRetries reservations")

	// 18h later (within the TTL) the run is evicted again: rejected, but
	// lastUpdate is refreshed to the current now.
	clock = base.Add(18 * time.Hour)
	_, ok = p.reserveEvictionRetry("live", maxRetries)
	require.False(t, ok)

	// 36h after base — but only 18h after the refresh — the entry must survive
	// the sweep, proving lastUpdate moved on the exhausted path.
	clock = base.Add(36 * time.Hour)
	n := p.sweepEvictionCounts(ttl)
	assert.Equal(t, 0, n, "still-evicting run must not be reclaimed")

	v, present := p.evictionCounts.Load("live")
	require.True(t, present, "live run's counter retained")
	assert.Equal(t, maxRetries, v.(evictionEntry).count, "count stays pinned at the cap")

	// The budget must still be exhausted — the surviving entry never refilled.
	_, ok = p.reserveEvictionRetry("live", maxRetries)
	assert.False(t, ok, "budget must not refill while the run is live")
}

// rerunLoopMetrics builds the counters the Q503 re-run loop records into.
func rerunLoopMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q503_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRerunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q503_eviction_rerun_failures_total",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),
		EvictionRerunWithheld: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q811_eviction_rerun_withheld_total",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),
	}
}

// alreadyRunningBody is GitHub's rerun-failed-jobs refusal while the original run has
// not concluded, verbatim from the live measurement (Q396/Q503).
const alreadyRunningBody = `{"message":"This workflow is already running","documentation_url":"https://docs.github.com/rest"}`

// TestHandleEviction_RetriesUntilTheRunConcludes is the Q503 regression test. GitHub
// refuses rerun-failed-jobs with 403 "This workflow is already running" until it has
// concluded the original run — measured at 9m36s after an ungraceful kill — while the
// AGC used to fire exactly once, ~5s after the eviction. The refusal spent the retry
// budget and the job was never re-run.
//
// The invariant now: a still-running refusal is "not yet", so recovery keeps calling
// until GitHub accepts, and the whole wait costs ONE budget slot.
func TestHandleEviction_RetriesUntilTheRunConcludes(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Two refusals before the run "concludes": the AGC must outlast both.
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(alreadyRunningBody))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	m := rerunLoopMetrics()
	p := &Provisioner{
		Metrics:                    m,
		TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:               srv.URL,
		HTTPClient:                 srv.Client(),
		EvictionRerunRetryInterval: time.Millisecond,
	}
	target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	<-p.handleEviction(context.Background(), target, "owner", "repo", "12345", log, 2, 0, evictionTierClassic, recoveryCauseEviction)

	assert.Equal(t, int64(3), calls.Load(), "two refusals then the accepted call")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRetries.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseEviction)),
		"the whole refusal-spanning recovery must cost one budget slot, not one per call")
	assert.Empty(t, target.events, "an eventually-accepted re-run is not an operator incident")

	// The budget must reflect one spent slot, so a SECOND eviction of the run still
	// has its slot — the property the old one-shot behaviour destroyed.
	_, ok := p.reserveEvictionRetry("12345", 2)
	assert.True(t, ok, "one recovery must consume exactly one of the two budget slots")
}

// TestHandleEviction_RunNeverConcludingIsSurfaced bounds the Q503 retry loop: if the
// re-run window closes with GitHub still refusing, recovery gives up loudly — the
// failure counter and an owner Event — rather than retrying forever or, worse,
// pretending the spent budget slot recovered anything.
func TestHandleEviction_RunNeverConcludingIsSurfaced(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(alreadyRunningBody))
	}))
	defer srv.Close()

	m := rerunLoopMetrics()
	p := &Provisioner{
		Metrics:                    m,
		TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:               srv.URL,
		HTTPClient:                 srv.Client(),
		EvictionRerunWindow:        50 * time.Millisecond,
		EvictionRerunRetryInterval: 5 * time.Millisecond,
	}
	target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	<-p.handleEviction(context.Background(), target, "owner", "repo", "777", log, 2, 0, evictionTierScaleSet, recoveryCauseEviction)

	assert.Greater(t, calls.Load(), int64(1), "the window must span several refused attempts")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("ns", "g", evictionTierScaleSet, recoveryCauseEviction, rerunFailureReasonNeverConcluded)))
	assert.Contains(t, target.events, "EvictionRerunFailed",
		"a re-run that never landed needs an owner-visible Event, not just a log line")
}

// TestHandleEviction_TerminalFailuresDoNotRetry pins the discrimination: only the
// still-running refusal means "again later". A 403 with any other message (a
// permissions problem) and a 5xx are terminal — retrying either would hammer an
// endpoint that is not going to change its mind within the window.
func TestHandleEviction_TerminalFailuresDoNotRetry(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "403 without the still-running message", status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`},
		{name: "500", status: http.StatusInternalServerError, body: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			m := rerunLoopMetrics()
			p := &Provisioner{
				Metrics:                    m,
				TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
				GitHubAPIURL:               srv.URL,
				HTTPClient:                 srv.Client(),
				EvictionRerunRetryInterval: time.Millisecond,
			}
			target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			<-p.handleEviction(context.Background(), target, "owner", "repo", "888", log, 2, 0, evictionTierClassic, recoveryCauseEviction)

			assert.Equal(t, int64(1), calls.Load(), "a terminal failure must not be retried")
			assert.Equal(t, float64(1),
				testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseEviction, rerunFailureReasonAPIError)))
			assert.Contains(t, target.events, "EvictionRerunFailed")
		})
	}
}

// runConcludedFailure is the shape a drained or evicted run concludes on, measured at
// live GitHub (Q459). It is what a fake must answer the Q811 conclusion check with for a
// recovery to proceed to its re-run.
const runConcludedFailure = `{"status":"completed","conclusion":"failure"}`

// answeredRunConclusion answers the run GET the Q811 conclusion check makes, reporting
// whether it did. Every fake a disruption recovery can reach owes this arm: the deletion
// arm makes two differently-shaped calls where there used to be one, and a fake that
// answers both alike both miscounts its re-runs and, when its body will not decode,
// leaves the recovery re-asking for the whole 15-minute window.
func answeredRunConclusion(w http.ResponseWriter, r *http.Request, body string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
	return true
}

// runAPIStub is a fake of the two run endpoints the Q811 conclusion gate touches: the
// run GET, answered from states in order (the last one repeats), and the rerun POST,
// answered 201. It counts both so a test can assert what was NOT called.
type runAPIStub struct {
	gets, reruns atomic.Int64
	// states are the {status, conclusion} pairs the GET returns, one per call, holding
	// the last for every call past the end — a run winding down and then concluding.
	states [][2]string
}

func (s *runAPIStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			i := int(s.gets.Add(1)) - 1
			if i >= len(s.states) {
				i = len(s.states) - 1
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":%q,"conclusion":%q}`, s.states[i][0], s.states[i][1])
			return
		}
		s.reruns.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
}

// TestHandleEviction_CancelledRunIsNotReRun is the Q811 regression test, and the whole
// point of the conclusion gate.
//
// The graceful-deletion arm keys on a deletion mark no AGC delete stamped, and the
// cancel runbook's own remedy for a worker that will not stop is to delete its pod —
// which supplies that mark by hand. GitHub accepts rerun-failed-jobs for a `cancelled`
// conclusion (measured 2026-08-05, Q683), so recovery used to re-queue the job the
// operator had just stopped. It must now stand down without calling at all.
func TestHandleEviction_CancelledRunIsNotReRun(t *testing.T) {
	stub := &runAPIStub{states: [][2]string{{"completed", "cancelled"}}}
	srv := stub.server()
	defer srv.Close()

	m := rerunLoopMetrics()
	p := &Provisioner{
		Metrics:                    m,
		TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:               srv.URL,
		HTTPClient:                 srv.Client(),
		EvictionRerunRetryInterval: time.Millisecond,
	}
	target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	<-p.handleEviction(context.Background(), target, "owner", "repo", "811", log, 2, 0, evictionTierClassic, recoveryCauseDeletion)

	assert.Equal(t, int64(0), stub.reruns.Load(), "a cancelled run must never be asked to re-run")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRerunWithheld.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseDeletion, rerunWithheldReasonRunCancelled)),
		"a withheld re-run is counted, so it is not indistinguishable from a recovery that never armed")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseDeletion, rerunFailureReasonAPIError)),
		"honouring a cancel is the correct outcome, not a failure")
	assert.Contains(t, target.events, "EvictionRerunWithheld",
		"the operator sees why no re-run happened, rather than a silent no-op")
	assert.NotContains(t, target.events, "EvictionRerunFailed")
}

// TestHandleEviction_DrainedRunStillReRuns is the negative control for the gate above:
// the deletion arm's whole purpose is that a drained worker's job comes back. Its run
// concludes `failure` (measured, Q459), and the conclusion check must let it through —
// including across the wait, since at detection the run has not concluded at all and its
// conclusion is null.
func TestHandleEviction_DrainedRunStillReRuns(t *testing.T) {
	stub := &runAPIStub{states: [][2]string{
		{"in_progress", ""},
		{"completed", "failure"},
	}}
	srv := stub.server()
	defer srv.Close()

	m := rerunLoopMetrics()
	p := &Provisioner{
		Metrics:                    m,
		TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:               srv.URL,
		HTTPClient:                 srv.Client(),
		EvictionRerunRetryInterval: time.Millisecond,
	}
	target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	<-p.handleEviction(context.Background(), target, "owner", "repo", "459", log, 2, 0, evictionTierScaleSet, recoveryCauseDeletion)

	assert.GreaterOrEqual(t, stub.reruns.Load(), int64(1), "a drained worker's job must still be re-run")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(m.EvictionRerunWithheld.WithLabelValues("ns", "g", evictionTierScaleSet, recoveryCauseDeletion, rerunWithheldReasonRunCancelled)))
	assert.Empty(t, target.events, "an accepted re-run is not an operator incident")
}

// TestHandleEviction_UngatedCausesDoNotReadTheConclusion pins the gate's scope. An
// eviction, a preemption and a vanished worker are signals only the cluster writes, so
// no operator action produces them and the check would be a GitHub call per attempt
// spent discriminating a case that cannot arise. A cancelled conclusion must not stop
// those recoveries either — the run is concluded, which is exactly when the re-run lands.
func TestHandleEviction_UngatedCausesDoNotReadTheConclusion(t *testing.T) {
	for _, cause := range []string{recoveryCauseEviction, recoveryCausePreemption, recoveryCauseVanished, recoveryCauseAbandoned} {
		t.Run(cause, func(t *testing.T) {
			stub := &runAPIStub{states: [][2]string{{"completed", "cancelled"}}}
			srv := stub.server()
			defer srv.Close()

			m := rerunLoopMetrics()
			p := &Provisioner{
				Metrics:                    m,
				TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
				GitHubAPIURL:               srv.URL,
				HTTPClient:                 srv.Client(),
				EvictionRerunRetryInterval: time.Millisecond,
			}
			target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			<-p.handleEviction(context.Background(), target, "owner", "repo", "497", log, 2, 0, evictionTierClassic, cause)

			assert.Equal(t, int64(0), stub.gets.Load(), "only the deletion arm reads the run's conclusion")
			assert.Equal(t, int64(1), stub.reruns.Load(), "the re-run must still fire")
		})
	}
}

// TestHandleEviction_UnreadableConclusionWithholdsThenSurfaces pins the gate's failure
// direction. A conclusion the AGC cannot read says nothing about whether a re-run would
// undo a cancel, so it must not be read as "not cancelled" — the call is retried inside
// the existing window and, if it never becomes readable, the recovery ends as a failure
// the operator can act on rather than as a re-run fired blind.
func TestHandleEviction_UnreadableConclusionWithholdsThenSurfaces(t *testing.T) {
	var gets, reruns atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reruns.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	m := rerunLoopMetrics()
	p := &Provisioner{
		Metrics:                    m,
		TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL:               srv.URL,
		HTTPClient:                 srv.Client(),
		EvictionRerunWindow:        50 * time.Millisecond,
		EvictionRerunRetryInterval: 5 * time.Millisecond,
	}
	target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	<-p.handleEviction(context.Background(), target, "owner", "repo", "812", log, 2, 0, evictionTierClassic, recoveryCauseDeletion)

	assert.Equal(t, int64(0), reruns.Load(), "an unreadable conclusion must not be read as 'not cancelled'")
	assert.Greater(t, gets.Load(), int64(1), "the window must span several re-reads, not give up on the first error")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseDeletion, rerunFailureReasonConclusionUnknown)),
		"a recovery that never re-ran needs its own reason, not the still-running one")
	assert.Contains(t, target.events, "EvictionRerunFailed")
}

// TestHandleEviction_UnanswerableConclusionIsTerminal is the other half of the split.
// A 4xx and a 2xx carrying something that is not a run cannot become a verdict inside
// the re-run window — the endpoint is not the API, or the run is not there — so
// re-asking thirty times only delays the Event that tells an operator so. Neither fires
// a re-run: not knowing is still not a licence to undo a cancel.
func TestHandleEviction_UnanswerableConclusionIsTerminal(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "404 the run is not there", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		{name: "200 carrying something that is not a run", status: http.StatusOK, body: `<html>proxy error</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gets, reruns atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				reruns.Add(1)
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			m := rerunLoopMetrics()
			p := &Provisioner{
				Metrics:                    m,
				TokenFunc:                  func(context.Context) (string, error) { return "tok", nil },
				GitHubAPIURL:               srv.URL,
				HTTPClient:                 srv.Client(),
				EvictionRerunRetryInterval: time.Millisecond,
			}
			target := &stubTarget{key: client.ObjectKey{Namespace: "ns", Name: "g"}}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			<-p.handleEviction(context.Background(), target, "owner", "repo", "813", log, 2, 0, evictionTierClassic, recoveryCauseDeletion)

			assert.Equal(t, int64(1), gets.Load(), "a verdict that cannot change must not be re-asked")
			assert.Equal(t, int64(0), reruns.Load(), "an unanswerable conclusion is not a licence to re-run")
			assert.Equal(t, float64(1),
				testutil.ToFloat64(m.EvictionRerunFailures.WithLabelValues("ns", "g", evictionTierClassic, recoveryCauseDeletion, rerunFailureReasonAPIError)))
			assert.Contains(t, target.events, "EvictionRerunFailed")
		})
	}
}

// TestRerunFailedJobs_RequiresAnExplicitBaseURL is the Q504 regression test.
//
// The bug was not a wrong URL — it was an UNSET field with a silent default:
// nothing in cmd/agc assigned Provisioner.GitHubAPIURL, so rerunFailedJobs
// quietly used api.github.com no matter what GITHUB_API_BASE_URL said, posting
// an installation token to a host that had never issued it (a bare 401 on
// GHES). An unconfigured provisioner must REFUSE rather than guess.
func TestRerunFailedJobs_RequiresAnExplicitBaseURL(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Provisioner{
		TokenFunc:  func(context.Context) (string, error) { return "tok", nil },
		HTTPClient: srv.Client(),
		// GitHubAPIURL deliberately left unset — the production shape of the bug.
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := p.rerunFailedJobs(context.Background(), "owner", "repo", "123", log)
	require.Error(t, err, "an unset base URL must be an error, not a silent fall back to api.github.com")
	assert.Contains(t, err.Error(), "GitHubAPIURL is not configured")
	assert.False(t, called.Load(), "no request may be issued when the endpoint is unknown")
}

// TestRerunFailedJobs_UsesTheConfiguredBaseURL is the other half: a configured
// endpoint is addressed verbatim, so a GHES base URL reaches GHES rather than being
// replaced by the public API.
func TestRerunFailedJobs_UsesTheConfiguredBaseURL(t *testing.T) {
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Provisioner{
		TokenFunc:    func(context.Context) (string, error) { return "tok", nil },
		GitHubAPIURL: srv.URL + "/api/v3",
		HTTPClient:   srv.Client(),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	require.NoError(t, p.rerunFailedJobs(context.Background(), "myorg", "myrepo", "77", log))
	select {
	case path := <-gotPath:
		assert.Equal(t, "/api/v3/repos/myorg/myrepo/actions/runs/77/rerun-failed-jobs", path,
			"the configured base path must be preserved; a GHES endpoint carries a /api/v3 prefix")
	default:
		t.Fatal("the rerun call never reached the configured endpoint")
	}
}
