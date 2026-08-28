package provisioner

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fakeClock is a manually-advanced clock so the token bucket is driven
// deterministically, with no wall-clock sleeps in the tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestLimiter wires a scaleUpLimiter to a fake clock and a sleep stub that,
// rather than blocking, records the requested delay and advances the clock by it
// (modelling "time passed while we waited"), so the bucket accounting stays
// consistent across calls.
func newTestLimiter(clock *fakeClock, lastDelay *time.Duration) *scaleUpLimiter {
	return &scaleUpLimiter{
		now: clock.now,
		sleep: func(_ context.Context, d time.Duration) error {
			if lastDelay != nil {
				*lastDelay = d
			}
			clock.advance(d)
			return nil
		},
	}
}

func TestScaleUpLimiter_DisabledIsNoOp(t *testing.T) {
	clock := newFakeClock()
	var sleepCalls int
	l := &scaleUpLimiter{
		now: clock.now,
		sleep: func(context.Context, time.Duration) error {
			sleepCalls++
			return nil
		},
	}

	for _, cfg := range []*ScaleUpConfig{
		nil,                          // not opted in
		{MaxPerSecond: 0, Burst: 5},  // zero rate = disabled
		{MaxPerSecond: 10, Burst: 0}, // zero burst = disabled
	} {
		for i := 0; i < 1000; i++ {
			throttled, err := l.wait(context.Background(), "ns/rg", cfg)
			if err != nil {
				t.Fatalf("disabled limiter returned error: %v", err)
			}
			if throttled {
				t.Fatalf("disabled limiter throttled (cfg=%+v)", cfg)
			}
		}
	}
	if sleepCalls != 0 {
		t.Fatalf("disabled limiter slept %d times; want 0", sleepCalls)
	}
	// A disabled config must not even allocate a per-key limiter.
	if l.limiterFor("ns/rg", nil) != nil {
		t.Fatal("limiterFor(nil) returned a non-nil limiter")
	}
}

func TestScaleUpLimiter_BurstThenThrottle(t *testing.T) {
	clock := newFakeClock()
	var lastDelay time.Duration
	l := newTestLimiter(clock, &lastDelay)
	cfg := &ScaleUpConfig{MaxPerSecond: 1, Burst: 3}
	ctx := context.Background()

	// The initial burst of 3 is admitted instantly (clock does not advance).
	for i := 0; i < 3; i++ {
		throttled, err := l.wait(ctx, "ns/rg", cfg)
		if err != nil {
			t.Fatalf("burst call %d errored: %v", i, err)
		}
		if throttled {
			t.Fatalf("burst call %d throttled; the first %d should pass instantly", i, cfg.Burst)
		}
	}

	// The 4th creation in the same instant must wait ~1s (rate = 1/s).
	throttled, err := l.wait(ctx, "ns/rg", cfg)
	if err != nil {
		t.Fatalf("post-burst call errored: %v", err)
	}
	if !throttled {
		t.Fatal("post-burst call was not throttled; the bucket should be empty")
	}
	if lastDelay < 900*time.Millisecond || lastDelay > 1100*time.Millisecond {
		t.Fatalf("post-burst wait was %v; want ~1s (rate 1/s)", lastDelay)
	}
}

func TestScaleUpLimiter_SustainedRate(t *testing.T) {
	clock := newFakeClock()
	var lastDelay time.Duration
	l := newTestLimiter(clock, &lastDelay)
	cfg := &ScaleUpConfig{MaxPerSecond: 5, Burst: 1} // one every 200ms, no burst headroom
	ctx := context.Background()

	// First call drains the single burst token.
	if throttled, _ := l.wait(ctx, "ns/rg", cfg); throttled {
		t.Fatal("first call throttled with burst=1")
	}
	// Each subsequent back-to-back call must wait exactly one refill interval (200ms).
	for i := 0; i < 5; i++ {
		throttled, err := l.wait(ctx, "ns/rg", cfg)
		if err != nil {
			t.Fatalf("sustained call %d errored: %v", i, err)
		}
		if !throttled {
			t.Fatalf("sustained call %d not throttled; burst=1 admits only one instantly", i)
		}
		if lastDelay < 150*time.Millisecond || lastDelay > 250*time.Millisecond {
			t.Fatalf("sustained call %d waited %v; want ~200ms (5/s)", i, lastDelay)
		}
	}
}

func TestScaleUpLimiter_ReReadsConfig(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, nil)

	// Same key, changing config: the per-key limiter is reused and reconciled to the
	// new rate/burst (Q117: a spec edit takes effect on the next job).
	lim1 := l.limiterFor("ns/rg", &ScaleUpConfig{MaxPerSecond: 1, Burst: 1})
	lim2 := l.limiterFor("ns/rg", &ScaleUpConfig{MaxPerSecond: 5, Burst: 10})
	if lim1 != lim2 {
		t.Fatal("limiterFor allocated a new limiter for a changed config; want the same instance reconciled in place")
	}
	if lim2.Limit() != rate.Limit(5) {
		t.Fatalf("limit not reconciled: got %v, want 5", lim2.Limit())
	}
	if lim2.Burst() != 10 {
		t.Fatalf("burst not reconciled: got %d, want 10", lim2.Burst())
	}

	// Switching the config off mid-life disables limiting again (no leaked limiter use).
	if l.limiterFor("ns/rg", nil) != nil {
		t.Fatal("limiterFor(nil) returned non-nil after the key had been active")
	}
}

func TestScaleUpLimiter_ContextCancelWhileWaiting(t *testing.T) {
	clock := newFakeClock()
	l := &scaleUpLimiter{
		now: clock.now,
		sleep: func(ctx context.Context, _ time.Duration) error {
			return context.Canceled // model a cancellation mid-wait
		},
	}
	cfg := &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}
	ctx := context.Background()

	// Drain the burst token so the next call must wait.
	if throttled, _ := l.wait(ctx, "ns/rg", cfg); throttled {
		t.Fatal("first call throttled with burst=1")
	}
	throttled, err := l.wait(ctx, "ns/rg", cfg)
	if err == nil {
		t.Fatal("cancelled wait returned nil error; want the cancellation propagated")
	}
	if !throttled {
		t.Fatal("cancelled wait reported throttled=false; it did have to wait")
	}
}

func TestScaleUpLimiter_PerKeyIsolation(t *testing.T) {
	clock := newFakeClock()
	var lastDelay time.Duration
	l := newTestLimiter(clock, &lastDelay)
	cfg := &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}
	ctx := context.Background()

	// Draining group A's bucket must not throttle group B — the buckets are per key.
	if throttled, _ := l.wait(ctx, "ns/a", cfg); throttled {
		t.Fatal("group A first call throttled")
	}
	if throttled, _ := l.wait(ctx, "ns/b", cfg); throttled {
		t.Fatal("group B first call throttled by group A's usage; buckets should be isolated")
	}
}

// TestScaleUpLimiter_RefundRestoresOneToken pins the refund at the level the ramp
// is felt: a spent token that bought no pod must buy one later (Q972).
func TestScaleUpLimiter_RefundRestoresOneToken(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, nil)
	cfg := &ScaleUpConfig{MaxPerSecond: 1, Burst: 3}

	for i := 0; i < 3; i++ {
		if !l.allow("ns/rg", cfg) {
			t.Fatalf("burst take %d refused; the first %d must pass instantly", i, cfg.Burst)
		}
	}
	if l.allow("ns/rg", cfg) {
		t.Fatal("a 4th take passed; the bucket should be empty")
	}

	// Two of the three deliveries never created a pod, so their tokens come back
	// and the very next two takes pass with no time on the clock.
	l.refund("ns/rg", cfg)
	l.refund("ns/rg", cfg)
	for i := 0; i < 2; i++ {
		if !l.allow("ns/rg", cfg) {
			t.Fatalf("take %d after two refunds was refused; a refunded token must be spendable", i)
		}
	}
	if l.allow("ns/rg", cfg) {
		t.Fatal("a third take passed after only two refunds; the refund added more than it returned")
	}
}

// TestScaleUpLimiter_RefundCannotExceedBurst pins the other half: a refund into a
// bucket that owes nothing must not raise availability above Burst, or an operator
// who set burst=N could see N+k pods start at once. This is the property the
// AllowN(now, -1) mechanism rests on, so a golang.org/x/time bump that dropped the
// burst cap fails here rather than silently loosening the limit (Q972).
func TestScaleUpLimiter_RefundCannotExceedBurst(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, nil)
	cfg := &ScaleUpConfig{MaxPerSecond: 1, Burst: 2}

	// The bucket is full, so these refunds are owed nothing.
	for i := 0; i < 5; i++ {
		l.refund("ns/rg", cfg)
	}

	takes := 0
	for i := 0; i < 10; i++ {
		if l.allow("ns/rg", cfg) {
			takes++
		}
	}
	if takes != int(cfg.Burst) {
		t.Fatalf("%d instantaneous takes after refunding into a full bucket; want %d (burst)", takes, cfg.Burst)
	}
}

// TestScaleUpLimiter_RefundDisabledIsNoOp keeps the default-off promise: an owner
// that never opted into spec.scaleUp allocates no limiter, refund included.
func TestScaleUpLimiter_RefundDisabledIsNoOp(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, nil)

	l.refund("ns/rg", nil)
	l.refund("ns/rg", &ScaleUpConfig{MaxPerSecond: 0, Burst: 5})
	if l.limiterFor("ns/rg", nil) != nil {
		t.Fatal("refund with a disabled config allocated a limiter")
	}
}
