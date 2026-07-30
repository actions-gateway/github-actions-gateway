package provisioner

import (
	"context"
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
			p.handleEviction(context.Background(), p.runnerGroupTarget(rg), "owner", "repo", "12345", log, maxRetries, 0, tier, cause)
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
		p.handleEviction(context.Background(), p.runnerGroupTarget(rg), "owner", "repo", "999", log, maxRetries, 0, evictionTierClassic, recoveryCauseEviction)
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

// TestRerunFailedJobs_RequiresAnExplicitBaseURL is the Q504 regression test.
//
// The bug was not a wrong URL — it was an UNSET field with a silent default. Nothing
// in cmd/agc ever assigned Provisioner.GitHubAPIURL, so rerunFailedJobs quietly used
// api.github.com no matter what GITHUB_API_BASE_URL said. On a GHES deployment that
// meant posting an installation token to a host that had never issued it, and the
// only symptom was a 401 naming a server the operator had not configured.
//
// A test that merely asserted "the configured URL is used" would have passed
// throughout, because the tests all configure it. What was missing is this: an
// unconfigured provisioner must REFUSE rather than guess, so the misconfiguration
// surfaces at the call instead of as someone else's authentication error.
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
