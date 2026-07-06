package provisioner

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// scaleUpLimiter is an in-memory, per-RunnerGroup token-bucket rate limiter over
// worker-pod CREATION (Q223). It smooths cold-start stampedes on a shared,
// rate-sensitive egress path (NAT/SNAT, firewall conntrack, site-to-site VPN) by
// spreading a burst of simultaneously-acquired jobs' pod creations over time. It
// is DEFAULT-OFF: a nil config disables it entirely (immediate provisioning, zero
// added latency), so it never changes behaviour for a RunnerGroup that has not
// opted in.
//
// It complements — does not replace — the admissionGate / ceilingCheck, which cap
// the concurrent pod COUNT: this caps the creation RATE. When the bucket is empty,
// an acquired job waits (holding its Q59 admission slot and its GitHub job lock,
// which the renew loop keeps alive) until a token frees, composing with the
// namespace-quota retry wait rather than adding a new state machine.
//
// The per-key limiter is soft state, lost on AGC restart (fail-safe: a restart
// simply resets the bucket to full burst). The zero value is ready to use.
type scaleUpLimiter struct {
	mu    sync.Mutex
	byKey map[string]*rate.Limiter

	// now returns the current time; nil selects time.Now. Overridden in tests to
	// drive the token bucket deterministically without wall-clock sleeps.
	now func() time.Time
	// sleep blocks for d or until ctx is done, returning ctx.Err() on cancel; nil
	// selects a real context-aware timer. Overridden in tests alongside now so the
	// wait path is fully deterministic.
	sleep func(ctx context.Context, d time.Duration) error
}

func (l *scaleUpLimiter) nowFn() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *scaleUpLimiter) sleepFn(ctx context.Context, d time.Duration) error {
	if l.sleep != nil {
		return l.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// limiterFor returns the rate.Limiter for key under cfg, creating it on first use
// and reconciling its limit/burst to cfg on every call so a spec edit takes effect
// on the next job without an AGC restart (Q117), mirroring the admission gate.
// Returns nil when cfg disables limiting (nil, or a non-positive rate/burst), in
// which case wait is a no-op.
func (l *scaleUpLimiter) limiterFor(key string, cfg *ScaleUpConfig) *rate.Limiter {
	if cfg == nil || cfg.MaxPerSecond <= 0 || cfg.Burst <= 0 {
		return nil
	}
	limit := rate.Limit(cfg.MaxPerSecond)
	burst := int(cfg.Burst)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		l.byKey = make(map[string]*rate.Limiter)
	}
	lim, ok := l.byKey[key]
	if !ok {
		// NewLimiter starts full (burst tokens available), so the first burst of a
		// freshly-observed group is admitted immediately — the ramp engages only once
		// the burst is spent.
		lim = rate.NewLimiter(limit, burst)
		l.byKey[key] = lim
		return lim
	}
	// Reconcile to the current config. SetLimitAt/SetBurstAt refill the bucket
	// relative to now before applying the change, so passing the limiter's own clock
	// keeps the accounting consistent with wait's ReserveN calls.
	if lim.Limit() != limit {
		lim.SetLimitAt(l.nowFn(), limit)
	}
	if lim.Burst() != burst {
		lim.SetBurstAt(l.nowFn(), burst)
	}
	return lim
}

// wait blocks until the limiter for key admits one worker-pod creation under cfg,
// or ctx is cancelled. It returns throttled=true when a token was NOT immediately
// available (the caller had to wait), so the caller can record the throttle
// metric; err is non-nil only when ctx was cancelled while waiting. cfg==nil (or a
// disabled config) is a no-op: it returns (false, nil) immediately, so an
// opted-out RunnerGroup pays nothing.
func (l *scaleUpLimiter) wait(ctx context.Context, key string, cfg *ScaleUpConfig) (throttled bool, err error) {
	lim := l.limiterFor(key, cfg)
	if lim == nil {
		return false, nil
	}
	now := l.nowFn()
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		// Unreachable while burst >= 1 (limiterFor excludes burst < 1): ReserveN of a
		// single event always succeeds. Guard against a deadlock rather than block
		// forever if that invariant ever changes.
		return false, nil
	}
	delay := r.DelayFrom(now)
	if delay <= 0 {
		return false, nil
	}
	if serr := l.sleepFn(ctx, delay); serr != nil {
		// Return the unused reservation so a job cancelled mid-wait does not consume a
		// token it never spent — otherwise the next job would see one fewer token of
		// burst than it should.
		r.CancelAt(l.nowFn())
		return true, serr
	}
	return true, nil
}
