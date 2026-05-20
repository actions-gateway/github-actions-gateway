// Package token provides a thread-safe, proactively refreshed GitHub App
// installation access token manager.
package token

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/karlkfi/github-actions-gateway/githubapp"
)

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                        { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the production Clock implementation.
var RealClock Clock = realClock{}

const refreshLeadTime = 5 * time.Minute

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

	// Namespace and Metrics are optional; set after NewManager if desired.
	Namespace string
	Metrics   MetricsRecorder
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
func (m *Manager) loop(ctx context.Context) {
	const baseBackoff = 5 * time.Second
	const maxBackoff = 60 * time.Second
	backoff := baseBackoff

	for {
		// Attempt to fetch a new token (HTTP call outside the lock).
		tok, err := m.provider.TokenWithExpiry(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if m.Metrics != nil {
				m.Metrics.IncTokenRefreshErrors(m.Namespace)
			}
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
		m.readyOnce.Do(func() { close(m.ready) })
		if m.Metrics != nil {
			m.Metrics.IncTokenRefreshes(m.Namespace)
		}
		backoff = baseBackoff

		// Sleep until T-5min before expiry.
		waitUntil := tok.ExpiresAt.Add(-refreshLeadTime)
		wait := waitUntil.Sub(m.clock.Now())
		if wait <= 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-m.clock.After(wait):
		}
	}
}
