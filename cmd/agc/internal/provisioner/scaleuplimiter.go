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
// the concurrent pod COUNT: this caps the creation RATE.
//
// Both acquisition tiers charge the bucket before the job is committed to, so an
// empty bucket refuses intake rather than blocking a job that already holds a GitHub
// lock (Q717). The classic tier takes its token in Admit's rate rung, before the
// claim, via allow; the scale-set tier has no per-job decision point, so it reads the
// bucket per long-poll via tokens and folds the answer into the advertised capacity,
// leaving wait as the pod-creation backstop behind it.
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

// allow takes one token for key under cfg without blocking, reporting whether a
// worker pod may be created now. It is the pre-claim form of wait: the classic
// tier's admission ladder calls it BEFORE the job is claimed from GitHub, so a job
// the bucket cannot admit is left queued for redelivery instead of claimed and then
// slept on while its GitHub lock runs down (Q717).
//
// It takes the token rather than observing one, because an observation cannot bind
// on the case the bucket exists for: every listener in a simultaneously-delivered
// burst would see the same free token, admit, and claim. AllowN decides under the
// limiter's own lock, which is the rate-limit counterpart of the admission gate's
// reservation counter.
//
// A token taken here is spent until the caller says how the admitted job ended: a
// delivery that reaches worker-pod creation keeps it, and one that returns first
// gets it back via refund (Q972). Only the caller can tell those apart, which is
// why runnercore.AdmitOutcome exists — the reservation release alone cannot, and
// refunding on COMPLETION would turn the rate limit into a second concurrency
// ceiling.
//
// cfg==nil (or a disabled config) is a no-op returning true, so an opted-out owner
// pays nothing.
func (l *scaleUpLimiter) allow(key string, cfg *ScaleUpConfig) bool {
	lim := l.limiterFor(key, cfg)
	if lim == nil {
		return true
	}
	return lim.AllowN(l.nowFn(), 1)
}

// refund returns one token to key's bucket for a delivery that took one in allow
// and then returned without any worker pod being created (Q972). Without it a
// fanned-out job charges one token per sibling delivery for the single pod its
// dedup winner creates, so N-way fan-out cuts the effective ramp to MaxPerSecond/N
// under exactly the burst spec.scaleUp exists to smooth.
//
// Refunding is correct only BEFORE a pod is asked for. On completion it would make
// the bucket a concurrency ceiling rather than a rate limit, which is why the
// decision belongs to the caller and not to the reservation release.
//
// AllowN with a negative n is how the refund reaches the bucket: x/time/rate
// exposes no token setter, and Reservation.CancelAt restores nothing once its
// timeToAct has passed, which is every deferred refund. Measured on the vendored
// golang.org/x/time v0.15.0: AllowN(now, -1) adds exactly one token, and the burst
// cap is applied on every read and take, so a refund into a full bucket cannot
// raise availability above Burst. scaleuplimiter_internal_test.go pins both, so a
// dependency bump that changed either goes red here rather than silently loosening
// the limit.
//
// cfg==nil (or a disabled config) is a no-op, so an opted-out owner pays nothing.
func (l *scaleUpLimiter) refund(key string, cfg *ScaleUpConfig) {
	lim := l.limiterFor(key, cfg)
	if lim == nil {
		return
	}
	lim.AllowN(l.nowFn(), -1)
}

// tokens reports the whole tokens free in key's bucket under cfg, and whether a
// bucket applies at all. It observes without taking: the scale-set tier states a
// capacity per long-poll rather than deciding per job, so the advertisement reads
// the bucket and the token is taken later, at pod creation, by wait.
//
// A fractional token cannot create a pod, so the count truncates — the advertisement
// is bias-low by construction, like the quota and placeability rungs, because
// under-advertising only delays jobs while over-advertising reproduces the
// claim-and-stall the rung exists to stop.
//
// limited=false means the owner declared no rate limit, and the caller keeps
// whatever the earlier rungs left.
func (l *scaleUpLimiter) tokens(key string, cfg *ScaleUpConfig) (free int32, limited bool) {
	lim := l.limiterFor(key, cfg)
	if lim == nil {
		return 0, false
	}
	t := lim.TokensAt(l.nowFn())
	if t <= 0 {
		return 0, true
	}
	return int32(t), true
}
