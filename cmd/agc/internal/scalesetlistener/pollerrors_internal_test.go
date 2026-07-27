package scalesetlistener

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The end-to-end reason labels are covered against the fake in pollerrors_test.go.
// These drive handlePollError directly, for the two classes the fake cannot produce:
// a black-holed long poll (a client-side timeout) and a bare transport failure.

// recordingPollErrors is a PollErrorRecorder that tallies calls by reason.
type recordingPollErrors struct {
	mu       sync.Mutex
	byReason map[string]int
}

func (r *recordingPollErrors) IncPollError(reason string) {
	r.mu.Lock()
	if r.byReason == nil {
		r.byReason = map[string]int{}
	}
	r.byReason[reason]++
	r.mu.Unlock()
}

func (r *recordingPollErrors) count(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byReason[reason]
}

// timeoutError is a net.Error reporting a timeout — the shape the poll client's
// response-header deadline produces on a connection the server accepted but never
// answered.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

// Temporary is deprecated but still part of net.Error.
func (timeoutError) Temporary() bool { return false }

// TestHandlePollError_ReasonLabels pins the classic tier's reason vocabulary onto the
// scale-set tier's poll failures (Q446): a 429 is "rate_limited", a client-side
// timeout is "timeout" (the black-holed long poll), and everything else is "other".
func TestHandlePollError_ReasonLabels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"rate limited", &scaleset.RateLimitError{RetryAfter: -1}, "rate_limited"},
		{"black-holed long poll", fmt.Errorf("scaleset: GetMessage: %w", timeoutError{}), "timeout"},
		{"transport failure", errors.New("connection reset by peer"), "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingPollErrors{}
			l, err := New(Config{
				Client:       &scaleset.Client{},
				ScaleSetName: "linux",
				Provision:    func(context.Context, Job) error { return nil },
				Capacity:     func(context.Context) int { return 1 },
				PollErrors:   rec,
				PollBackoff:  time.Millisecond,
			})
			require.NoError(t, err)

			assert.True(t, l.handlePollError(context.Background(), 1, &scaleset.RunnerScaleSetSession{}, tc.err),
				"a transient poll error must keep the loop running")
			assert.Equal(t, 1, rec.count(tc.want), "reason %q must be counted once", tc.want)
		})
	}
}

// TestHandlePollError_NoRecorderIsSafe confirms an unwired PollErrors — the shape a
// v1-only AGC or a test produces — leaves the error path working rather than
// panicking mid-poll.
func TestHandlePollError_NoRecorderIsSafe(t *testing.T) {
	l, err := New(Config{
		Client:       &scaleset.Client{},
		ScaleSetName: "linux",
		Provision:    func(context.Context, Job) error { return nil },
		Capacity:     func(context.Context) int { return 1 },
		PollBackoff:  time.Millisecond,
	})
	require.NoError(t, err)

	assert.True(t, l.handlePollError(context.Background(), 1, &scaleset.RunnerScaleSetSession{},
		errors.New("connection reset by peer")))
}
