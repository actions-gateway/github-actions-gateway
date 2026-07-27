package scalesetlistener_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the scale-set tier's half of the shared poll-error counter (Q446). The
// counter is the classic tier's actions_gateway_message_poll_errors_total series, so
// these assert on the reason vocabulary the classic listener uses — and on the two
// heal branches deliberately counting nothing, which is what keeps an operator's
// existing alert meaning the same thing after the classic machinery is removed.

// pollErrorCounter is a PollErrorRecorder that tallies calls by reason.
type pollErrorCounter struct {
	mu       sync.Mutex
	byReason map[string]int
}

func newPollErrorCounter() *pollErrorCounter {
	return &pollErrorCounter{byReason: map[string]int{}}
}

func (c *pollErrorCounter) IncPollError(reason string) {
	c.mu.Lock()
	c.byReason[reason]++
	c.mu.Unlock()
}

func (c *pollErrorCounter) count(reason string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byReason[reason]
}

// total counts every recorded poll error, whatever the reason.
func (c *pollErrorCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.byReason {
		n += v
	}
	return n
}

// withPollErrors returns a Config option wiring the given recorder.
func withPollErrors(c *pollErrorCounter) func(*scalesetlistener.Config) {
	return func(cfg *scalesetlistener.Config) { cfg.PollErrors = c }
}

// TestListener_RateLimitedPollCountsRateLimited proves a 429 on the message queue
// increments the shared counter under the classic tier's "rate_limited" reason —
// the rate-able signal the RateLimited condition cannot carry, since that condition
// only trips once an episode outlasts the sustained-rate-limit window.
func TestListener_RateLimitedPollCountsRateLimited(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	counter := newPollErrorCounter()
	startListenerFunc(t, srv, fixedCapacity(1),
		func(context.Context, scalesetlistener.Job) error { return nil },
		nil, withPollErrors(counter))

	srv.SetRateLimitPolls(true)
	require.Eventually(t, func() bool { return counter.count("rate_limited") >= 1 },
		5*time.Second, 5*time.Millisecond,
		"a 429 poll must increment message_poll_errors_total{reason=rate_limited}")

	assert.Zero(t, counter.count("other"), "a 429 must not also be counted as other")
	assert.Zero(t, counter.count("timeout"), "a 429 must not also be counted as timeout")
}

// TestListener_HealPathsDoNotCountPollErrors pins the parity boundary: an expired
// queue token (401 → refresh) and a dropped session (404 → re-create) are heal
// triggers, not poll failures, and the classic listener counts neither. Counting them
// here would make the scale-set tier's series read higher than classic's for the same
// routine credential churn, breaking any threshold an operator already tuned.
func TestListener_HealPathsDoNotCountPollErrors(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	counter := newPollErrorCounter()
	_, ssID := startListenerFunc(t, srv, fixedCapacity(1),
		func(context.Context, scalesetlistener.Job) error { return nil },
		nil, withPollErrors(counter))

	// 401 on the poll: the listener refreshes the queue token and carries on.
	srv.ExpireQueueToken(ssID)
	require.Eventually(t, func() bool { return srv.RefreshSessionCalls() >= 1 },
		5*time.Second, 5*time.Millisecond, "an expired queue token must drive a session refresh")

	// 404 on the poll: the listener re-creates the session and replays from the head.
	srv.DropSession(ssID)
	require.Eventually(t, func() bool { return srv.HasActiveSession(ssID) },
		5*time.Second, 5*time.Millisecond, "a dropped session must be re-created")

	assert.Zero(t, counter.total(),
		"neither heal path may increment the poll-error counter (classic parity)")
}
