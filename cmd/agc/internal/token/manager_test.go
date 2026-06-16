package token_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/token"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// fakeClock is an injectable Clock that starts at a fixed epoch.
// Call Stop() before goleak.VerifyNone so all polling goroutines exit.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	waiters  int // live After() waiters; lets a test wait until the loop is parked before advancing
	done     chan struct{}
	stopOnce sync.Once
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t, done: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Stop signals all pending After goroutines to exit.
func (c *fakeClock) Stop() {
	c.stopOnce.Do(func() { close(c.done) })
}

// After returns a channel that fires once the clock is advanced past the
// target time. The polling goroutine exits when Stop() is called.
//
// The target is captured and the waiter is counted synchronously, before the
// polling goroutine starts, so a test that observes Waiters() sees the wait the
// caller has already committed to (computed against the pre-advance clock).
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	target := c.now.Add(d)
	c.waiters++
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.waiters--
			c.mu.Unlock()
		}()
		for {
			select {
			case <-c.done:
				return
			case <-time.After(time.Millisecond):
				now := c.Now()
				if !now.Before(target) {
					select {
					case ch <- now:
					default:
					}
					return
				}
			}
		}
	}()
	return ch
}

// Waiters reports how many After() waiters are currently parked. A test uses it
// to wait until the manager's refresh loop has entered its proactive sleep
// before advancing the clock — advancing earlier would let the loop compute its
// wait against the already-advanced time and park past the target.
func (c *fakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiters
}

// stubProvider records calls and returns preset tokens.
type stubProvider struct {
	mu     sync.Mutex
	calls  int
	tokens []*githubapp.InstallationToken
	err    error // if non-nil, all calls return this error
}

func (p *stubProvider) Token(ctx context.Context) (string, error) {
	tok, err := p.TokenWithExpiry(ctx)
	if err != nil {
		return "", err
	}
	return tok.Token, nil
}

func (p *stubProvider) TokenWithExpiry(_ context.Context) (*githubapp.InstallationToken, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	i := p.calls
	p.calls++
	if i >= len(p.tokens) {
		i = len(p.tokens) - 1
	}
	return p.tokens[i], nil
}

func (p *stubProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *stubProvider) SetErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *stubProvider) ClearErr() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = nil
}

var epoch = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

func TestManager_ProactiveRefresh(t *testing.T) {
	expiry := epoch.Add(time.Hour)
	tok1 := &githubapp.InstallationToken{Token: "tok-1", ExpiresAt: expiry}
	tok2 := &githubapp.InstallationToken{Token: "tok-2", ExpiresAt: expiry.Add(time.Hour)}

	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok1, tok2}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	// Wait for first fetch.
	tok, err := mgr.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tok-1", tok)

	// The loop closes m.ready (unblocking Token above) *before* it parks in
	// clock.After for the ~55min proactive sleep. Wait until that waiter is
	// registered, otherwise advancing the clock here can win the race: the loop
	// would then compute its wait against the already-advanced time, floor it to
	// minRefreshInterval, and park 30s past our target — so the refresh never
	// fires within the assertion window below (the source of a -race flake).
	require.Eventually(t, func() bool { return clk.Waiters() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"refresh loop never parked in clock.After after the first fetch")

	// Advance to T-5min before expiry to trigger proactive refresh.
	clk.Advance(expiry.Sub(epoch) - 5*time.Minute)

	// Give the refresh loop time to fire.
	assert.Eventually(t, func() bool {
		t2, e := mgr.Token(ctx)
		return e == nil && t2 == "tok-2"
	}, 2*time.Second, 10*time.Millisecond, "expected proactive refresh to tok-2")

	cancel()
	clk.Stop()
	time.Sleep(30 * time.Millisecond)
	goleak.VerifyNone(t)
}

// TestManager_NearExpiryTokenDoesNotHotLoop drives the manager with a token
// whose TTL is shorter than the 5-minute refresh lead time, so the computed
// post-success wait (ExpiresAt-5min-now) is negative. The pre-fix code clamped
// that to 0 and spun fetch→wait(0)→fetch as a retry storm. With the
// minRefreshInterval floor, the loop must park for 30s between fetches instead,
// so the fetch count stays bounded across a simulated interval (Q107).
func TestManager_NearExpiryTokenDoesNotHotLoop(t *testing.T) {
	// 1-minute TTL < 5-minute lead time → negative computed wait.
	shortTTL := epoch.Add(time.Minute)
	tok1 := &githubapp.InstallationToken{Token: "tok-1", ExpiresAt: shortTTL}
	tok2 := &githubapp.InstallationToken{Token: "tok-2", ExpiresAt: shortTTL.Add(time.Minute)}
	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok1, tok2}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	got, err := mgr.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tok-1", got)

	// Without advancing the clock, the floored 30s wait keeps the loop parked.
	// A hot loop (wait clamped to 0) would issue many fetches in this window,
	// since fakeClock.After(0) fires immediately. The floored wait fires only
	// once the clock is advanced past 30s, which we never do here.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 1, provider.Calls(), "near-expiry token must not trigger a fetch storm")

	// Advancing past the floor (minRefreshInterval = 30s) refreshes promptly —
	// the short-TTL token is kept fresh, just throttled rather than hot-looped.
	clk.Advance(30 * time.Second)
	assert.Eventually(t, func() bool {
		t2, e := mgr.Token(ctx)
		return e == nil && t2 == "tok-2"
	}, 2*time.Second, 10*time.Millisecond, "floored wait should still refresh once it elapses")

	cancel()
	clk.Stop()
	time.Sleep(30 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestManager_ReadersDuringRefresh(t *testing.T) {
	expiry := epoch.Add(time.Hour)
	tok := &githubapp.InstallationToken{Token: "valid-tok", ExpiresAt: expiry}
	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	_, err := mgr.Token(ctx)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := mgr.Token(ctx)
			assert.NoError(t, e)
			assert.NotEmpty(t, got, "Token() must never return empty string")
		}()
	}
	wg.Wait()

	cancel()
	clk.Stop()
	time.Sleep(30 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestManager_RefreshFailureFallback(t *testing.T) {
	expiry := epoch.Add(time.Hour)
	tok1 := &githubapp.InstallationToken{Token: "tok-1", ExpiresAt: expiry}

	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok1}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	got, err := mgr.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, "tok-1", got)

	provider.SetErr(fmt.Errorf("provider down"))
	clk.Advance(expiry.Sub(epoch) - 4*time.Minute)
	time.Sleep(50 * time.Millisecond)

	got2, err2 := mgr.Token(ctx)
	require.NoError(t, err2)
	assert.Equal(t, "tok-1", got2, "should fall back to old token while not yet expired")

	cancel()
	clk.Stop()
	time.Sleep(30 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestManager_TokenExpiredAfterFailure(t *testing.T) {
	expiry := epoch.Add(time.Hour)
	tok1 := &githubapp.InstallationToken{Token: "tok-1", ExpiresAt: expiry}

	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok1}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	_, err := mgr.Token(ctx)
	require.NoError(t, err)

	provider.SetErr(fmt.Errorf("provider unavailable"))
	clk.Advance(expiry.Sub(epoch) + time.Minute)
	time.Sleep(50 * time.Millisecond)

	_, err2 := mgr.Token(ctx)
	assert.Error(t, err2, "Token() must return error when token expired and refresh failed")
	assert.Contains(t, err2.Error(), "expired")

	cancel()
	clk.Stop()
	time.Sleep(30 * time.Millisecond)
	goleak.VerifyNone(t)
}

func TestManager_TokenCancelledBeforeReady(t *testing.T) {
	release := make(chan struct{})
	bp := &blockingTokenProvider{releaseCh: release}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(bp, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	// Cancel before the first fetch completes (provider blocks on release).
	cancel()

	// Token() must return a non-nil error, not block.
	_, err := mgr.Token(ctx)
	assert.Error(t, err, "Token() should return error when context cancelled before ready")

	close(release) // unblock the provider goroutine so it can exit
	clk.Stop()
	time.Sleep(50 * time.Millisecond)
	goleak.VerifyNone(t)
}

// blockingTokenProvider blocks TokenWithExpiry until releaseCh is closed or ctx is cancelled.
type blockingTokenProvider struct {
	releaseCh chan struct{}
}

func (p *blockingTokenProvider) Token(ctx context.Context) (string, error) {
	tok, err := p.TokenWithExpiry(ctx)
	if err != nil {
		return "", err
	}
	return tok.Token, nil
}

func (p *blockingTokenProvider) TokenWithExpiry(ctx context.Context) (*githubapp.InstallationToken, error) {
	select {
	case <-p.releaseCh:
		return &githubapp.InstallationToken{Token: "tok", ExpiresAt: epoch.Add(time.Hour)}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestManager_NoLeakOnCancel(t *testing.T) {
	expiry := epoch.Add(time.Hour)
	tok := &githubapp.InstallationToken{Token: "tok", ExpiresAt: expiry}

	provider := &stubProvider{tokens: []*githubapp.InstallationToken{tok}}
	clk := newFakeClock(epoch)

	mgr := token.NewManager(provider, clk)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	mgr.Start(ctx)

	_, err := mgr.Token(ctx)
	require.NoError(t, err)

	cancel()
	clk.Stop()
	time.Sleep(50 * time.Millisecond)
	goleak.VerifyNone(t)
}
