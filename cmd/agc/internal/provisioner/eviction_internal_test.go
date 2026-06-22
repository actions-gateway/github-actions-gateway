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
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
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

	m := &listener.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_q106_eviction_retries_total",
		}, []string{"namespace", "runner_group"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_q106_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group"}),
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
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.handleEviction(context.Background(), client.ObjectKeyFromObject(rg), "owner", "repo", "12345", log, maxRetries, 0)
		}()
	}
	wg.Wait()

	got := rerunCount.Load()
	require.LessOrEqualf(t, got, int64(maxRetries),
		"rerun API must be called at most maxRetries (%d) times, got %d", maxRetries, got)
	// With concurrency far above the budget the budget should be fully consumed.
	require.Equal(t, int64(maxRetries), got,
		"budget should be fully used when evictions far exceed it")

	// The EvictionRetries metric is incremented exactly once per reserved slot,
	// so it must match the number of rerun calls.
	assert.Equal(t, float64(got), testutil.ToFloat64(m.EvictionRetries.WithLabelValues("ns", "mygroup")))
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
		p.handleEviction(context.Background(), client.ObjectKeyFromObject(rg), "owner", "repo", "999", log, maxRetries, 0)
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
