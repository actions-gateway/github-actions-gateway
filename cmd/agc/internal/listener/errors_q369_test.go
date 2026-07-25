package listener

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// This file is the classic-path regression guard for the unified error taxonomy
// (Q369). RateLimitError and UnauthorizedError are declared once in githubapp/httpx
// and aliased by both broker and scaleset, so this package's matchers — written
// against the broker names — also match an error raised by the scaleset client.
//
// The failure mode being guarded is invisible to the compiler: an errors.As against
// a type that is no longer the one raised silently returns false, downgrading a
// token refresh or a rate-limit backoff to a generic retry.

// brokerStatusError performs a real broker GetMessage against a server answering
// status and returns the error it yields.
func brokerStatusError(t *testing.T, status int, retryAfter string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	c := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
	_, err := c.GetMessage(context.Background(), "sess-1")
	require.Error(t, err)
	return err
}

func TestIsUnauthorized_MatchesScaleSetErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("HTTP%d", status), func(t *testing.T) {
			// The classic path, end to end: a real 401/403 from the broker.
			brokerErr := brokerStatusError(t, status, "")
			assert.True(t, isUnauthorized(brokerErr), "isUnauthorized must match a broker HTTP %d", status)
			assert.False(t, isSessionExpired(brokerErr), "an auth failure must not read as an expired session")

			// The cross-protocol half: the scaleset client raises the same type, so
			// this matcher fires for it too. (The scaleset client's own production of
			// the error is covered in scaleset and in scalesetlistener.)
			scaleSetErr := fmt.Errorf("scaleset: GetMessage: %w",
				&scaleset.UnauthorizedError{Source: httpx.SourceScaleSet, StatusCode: status})
			assert.True(t, isUnauthorized(scaleSetErr), "isUnauthorized must match a scaleset HTTP %d", status)
		})
	}
}

// TestRateLimitMatch_SpansBothProtocols pins the errors.As form the poll loop uses
// inline (goroutine.go's 429 branch): the broker-named target must match a rate
// limit from either protocol, and must keep carrying the Retry-After the loop
// waits on.
func TestRateLimitMatch_SpansBothProtocols(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want httpx.Source
	}{
		{"broker", brokerStatusError(t, http.StatusTooManyRequests, "30"), httpx.SourceBroker},
		{"scaleset", fmt.Errorf("scaleset: GetMessage: %w",
			&scaleset.RateLimitError{Source: httpx.SourceScaleSet, RetryAfter: 30 * time.Second}), httpx.SourceScaleSet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rlErr *broker.RateLimitError
			require.True(t, errors.As(tc.err, &rlErr), "a %s 429 must match *broker.RateLimitError", tc.name)
			assert.Equal(t, tc.want, rlErr.Source, "the error must still name its protocol")
			assert.Equal(t, 30*time.Second, rlErr.RetryAfter)

			// A rate limit must not be mistaken for the auth failure that triggers a
			// token refresh.
			assert.False(t, isUnauthorized(tc.err))
		})
	}
}
