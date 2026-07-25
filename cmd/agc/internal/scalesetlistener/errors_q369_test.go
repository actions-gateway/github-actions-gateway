package scalesetlistener

import (
	"context"
	"errors"
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

// This file is the regression guard for the unified error taxonomy (Q369).
// RateLimitError and UnauthorizedError are declared once in githubapp/httpx and
// aliased by both broker and scaleset, so this listener's matchers — written
// against the scaleset names — match an error raised by *either* protocol client.
//
// That property is invisible to the compiler: errors.As against a type that is no
// longer the one being raised silently returns false, turning a rate-limit backoff
// or a token refresh into a generic retry. So the tests drive real clients against
// real 429/401 responses on both paths and assert the matchers still fire.

// tokenProvider is a githubapp.TokenProvider returning a fixed placeholder.
type tokenProvider struct{}

func (tokenProvider) Token(context.Context) (string, error) { return "test-token", nil }

// statusServer returns a server answering every request with status, optionally
// setting Retry-After.
func statusServer(t *testing.T, status int, retryAfter string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scalesetPollError performs a real scaleset GetMessage against srv and returns the
// error it yields. The session is supplied directly, so no bootstrap is needed.
func scalesetPollError(t *testing.T, srv *httptest.Server) error {
	t.Helper()
	c, err := scaleset.New(scaleset.Config{
		TokenProvider: tokenProvider{},
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.Client(),
		PollClient:    srv.Client(),
	})
	require.NoError(t, err)

	sess := &scaleset.RunnerScaleSetSession{
		SessionID:               "sess-1",
		MessageQueueURL:         srv.URL + "/queue",
		MessageQueueAccessToken: "queue-token",
	}
	_, pollErr := c.GetMessage(context.Background(), sess, 1, 0)
	require.Error(t, pollErr)
	return pollErr
}

// brokerPollError performs a real classic-broker GetMessage against srv and returns
// the error it yields.
func brokerPollError(t *testing.T, srv *httptest.Server) error {
	t.Helper()
	c := &broker.Client{
		BrokerURL:  srv.URL,
		PoolID:     1,
		HTTPClient: srv.Client(),
		Token:      "test-token",
	}
	_, pollErr := c.GetMessage(context.Background(), "sess-1")
	require.Error(t, pollErr)
	return pollErr
}

func TestIsRateLimited_MatchesEitherProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll func(*testing.T, *httptest.Server) error
		want httpx.Source
	}{
		{"scaleset", scalesetPollError, httpx.SourceScaleSet},
		{"broker", brokerPollError, httpx.SourceBroker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.poll(t, statusServer(t, http.StatusTooManyRequests, "30"))

			assert.True(t, isRateLimited(err), "isRateLimited must match a %s 429", tc.name)
			assert.False(t, isUnauthorized(err), "a 429 must not read as an auth failure")
			assert.False(t, isNotFound(err), "a 429 must not read as a missing session")

			// The listener reads RetryAfter off the matched error to size its backoff
			// (poll-error handling in listener.go); the value must survive the match.
			var rl *scaleset.RateLimitError
			require.ErrorAs(t, err, &rl)
			assert.Equal(t, tc.want, rl.Source, "the error must still name its protocol")
			assert.Equal(t, 30*time.Second, rl.RetryAfter)
		})
	}
}

func TestIsUnauthorized_MatchesEitherProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll func(*testing.T, *httptest.Server) error
		want httpx.Source
	}{
		{"scaleset", scalesetPollError, httpx.SourceScaleSet},
		{"broker", brokerPollError, httpx.SourceBroker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
				err := tc.poll(t, statusServer(t, status, ""))

				assert.True(t, isUnauthorized(err), "isUnauthorized must match a %s HTTP %d", tc.name, status)
				assert.False(t, isRateLimited(err), "an auth failure must not read as a rate limit")

				var ue *scaleset.UnauthorizedError
				require.ErrorAs(t, err, &ue)
				assert.Equal(t, tc.want, ue.Source)
				assert.Equal(t, status, ue.StatusCode)
			}
		})
	}
}

// TestSharedTaxonomy_AliasesAreOneType is the compile-time half of the guard: the
// broker and scaleset names must resolve to the same declaration, so a single
// errors.As target serves both protocols. Re-splitting them into distinct types
// fails here rather than silently degrading a listener's error handling.
// These parameter types are the assertion: Go assignability between two *named*
// struct types requires them to be the same type, so each call below stops
// compiling the moment either package re-declares its own struct.
func acceptBrokerRateLimit(*broker.RateLimitError)           {}
func acceptScaleSetRateLimit(*scaleset.RateLimitError)       {}
func acceptBrokerUnauthorized(*broker.UnauthorizedError)     {}
func acceptScaleSetUnauthorized(*scaleset.UnauthorizedError) {}

func TestSharedTaxonomy_AliasesAreOneType(t *testing.T) {
	brokerRL := &broker.RateLimitError{Source: httpx.SourceBroker, RetryAfter: -1}
	scaleSetRL := &scaleset.RateLimitError{Source: httpx.SourceScaleSet, RetryAfter: -1}
	acceptBrokerRateLimit(scaleSetRL)
	acceptScaleSetRateLimit(brokerRL)

	brokerUE := &broker.UnauthorizedError{Source: httpx.SourceBroker, StatusCode: http.StatusUnauthorized}
	scaleSetUE := &scaleset.UnauthorizedError{Source: httpx.SourceScaleSet, StatusCode: http.StatusForbidden}
	acceptBrokerUnauthorized(scaleSetUE)
	acceptScaleSetUnauthorized(brokerUE)

	// The runtime consequence: one target matches either protocol's error.
	var rl *httpx.RateLimitError
	assert.True(t, errors.As(error(brokerRL), &rl))
	assert.True(t, errors.As(error(scaleSetRL), &rl))
	var ue *httpx.UnauthorizedError
	assert.True(t, errors.As(error(brokerUE), &ue))
	assert.True(t, errors.As(error(scaleSetUE), &ue))
}
