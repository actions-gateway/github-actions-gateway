package token_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/internal/token"
	"github.com/karlkfi/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// fakeClock is an injectable Clock that starts at a fixed epoch.
// Call Stop() before goleak.VerifyNone so all polling goroutines exit.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
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
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	target := c.Now().Add(d)
	go func() {
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
