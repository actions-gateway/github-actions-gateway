// Package token provides a thread-safe, proactively refreshed GitHub App
// installation access token manager.
package token

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/go-logr/logr"
)

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the production Clock implementation.
var RealClock Clock = realClock{}

const refreshLeadTime = 5 * time.Minute

// minRefreshInterval floors the wait between successful token fetches. A GitHub
// installation token normally has a 1-hour TTL, so the proactive refresh fires
// ~55 minutes out. But if a token arrives already inside the refreshLeadTime
// window — a TTL shorter than 5 minutes, or wall-clock skew between us and
// GitHub — the computed wait (ExpiresAt-refreshLeadTime-now) is non-positive.
// Clamping that to 0 would spin the loop fetch→wait(0)→fetch as a retry storm
// against the token endpoint. Flooring to 30s instead still refreshes promptly
// (a genuinely short-TTL token is renewed every 30s, well ahead of a 1-minute
// expiry) while bounding the success-path request rate to at most one fetch per
// 30s. This is distinct from the exponential backoff used for *failed* fetches.
const minRefreshInterval = 30 * time.Second

// MetricsRecorder is implemented by listener.Metrics to record token events.
// nil-safe: all methods must check for nil before calling.
type MetricsRecorder interface {
	IncTokenRefreshes(namespace string)
	IncTokenRefreshErrors(namespace string)
}

// Manager holds a thread-safe, proactively refreshed installation access token.
// The background loop wakes at T-5min before expiry and swaps in a fresh token
// while holding the write lock only for the pointer swap — never during the
// HTTP round-trip — so readers are never blocked waiting for the network.
type Manager struct {
	provider  githubapp.ExpiringTokenProvider
	mu        sync.RWMutex
	current   *githubapp.InstallationToken // nil until first successful fetch
	clock     Clock
	ready     chan struct{} // closed after the first successful fetch
	readyOnce sync.Once

	// Namespace, Metrics, and Logger are optional; set after NewManager if desired.
	// Logger defaults to logr.Discard() so a silent loop never panics — set it to
	// surface fetch attempts, errors, and successes in production. Without a real
	// logger the loop is invisible: a pod stuck on the initial fetch produces
	// zero output, which is exactly what hid an earlier CI hang.
	Namespace string
	Metrics   MetricsRecorder
	Logger    logr.Logger
}

// NewManager creates a Token Manager backed by the given ExpiringTokenProvider.
// If clock is nil, the real wall clock is used.
func NewManager(provider githubapp.ExpiringTokenProvider, clock Clock) *Manager {
	if clock == nil {
		clock = RealClock
	}
	return &Manager{
		provider: provider,
		clock:    clock,
		ready:    make(chan struct{}),
		Logger:   logr.Discard(),
	}
}

// Token returns the current valid token. Blocks only until the initial fetch
// completes (first call after Start). Subsequent calls take the read lock and
// return immediately. Returns an error if the token has expired and refresh has
// not succeeded.
func (m *Manager) Token(ctx context.Context) (string, error) {
	select {
	case <-m.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	m.mu.RLock()
	current := m.current
	m.mu.RUnlock()

	if m.clock.Now().After(current.ExpiresAt) {
		return "", fmt.Errorf("token manager: token expired at %s and refresh has not succeeded", current.ExpiresAt)
	}
	return current.Token, nil
}

// NewManagerWithExpiredToken returns a Manager whose token appears expired so
// Token() returns an error immediately without blocking. The background goroutine
// is not started; do not call Start on the returned manager. Intended for tests.
func NewManagerWithExpiredToken() *Manager {
	m := &Manager{
		clock:   RealClock,
		ready:   make(chan struct{}),
		current: &githubapp.InstallationToken{Token: "", ExpiresAt: time.Unix(0, 0)},
		Logger:  logr.Discard(),
	}
	close(m.ready)
	return m
}

// Start begins the background refresh loop. Refresh fires at T-5 minutes before
// the current token's ExpiresAt. The loop exits when ctx is cancelled.
// Must be called once before Token() is used.
func (m *Manager) Start(ctx context.Context) {
	go m.loop(ctx)
}

// loop is the background refresh goroutine.
//
// Logging policy: every iteration is observable. The previous silent-retry
// implementation made a stuck initial fetch indistinguishable from a healthy
// process — kubectl logs returned nothing for minutes. The Info/Error pair
// here ensures one log line per attempt regardless of outcome.
func (m *Manager) loop(ctx context.Context) {
	const baseBackoff = 5 * time.Second
	const maxBackoff = 60 * time.Second
	backoff := baseBackoff
	attempt := 0

	for {
		attempt++
		m.Logger.Info("fetching installation token", "attempt", attempt)

		// Attempt to fetch a new token (HTTP call outside the lock).
		tok, err := m.provider.TokenWithExpiry(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if m.Metrics != nil {
				m.Metrics.IncTokenRefreshErrors(m.Namespace)
			}
			m.Logger.Error(err, "token fetch failed; retrying after backoff",
				"attempt", attempt, "backoff", backoff)
			// Retry with exponential backoff; do not update ready or current.
			select {
			case <-ctx.Done():
				return
			case <-m.clock.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Swap the token pointer under the write lock (fast — no I/O).
		m.mu.Lock()
		m.current = tok
		m.mu.Unlock()
		firstFetch := false
		m.readyOnce.Do(func() {
			close(m.ready)
			firstFetch = true
		})
		if m.Metrics != nil {
			m.Metrics.IncTokenRefreshes(m.Namespace)
		}
		m.Logger.Info("token ready",
			"firstFetch", firstFetch,
			"expiresAt", tok.ExpiresAt.Format(time.RFC3339),
			"validFor", tok.ExpiresAt.Sub(m.clock.Now()).Round(time.Second).String())
		backoff = baseBackoff

		// Sleep until T-5min before expiry, but never less than
		// minRefreshInterval — see that const for why a non-positive wait must
		// be floored rather than clamped to 0 (success-path storm prevention).
		waitUntil := tok.ExpiresAt.Add(-refreshLeadTime)
		wait := waitUntil.Sub(m.clock.Now())
		if wait < minRefreshInterval {
			wait = minRefreshInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-m.clock.After(wait):
		}
	}
}
