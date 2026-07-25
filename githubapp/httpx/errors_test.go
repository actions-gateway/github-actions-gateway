package httpx_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

func TestParseRateLimitError_RetryAfter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		retryAfter string // "" = header absent
		want       time.Duration
	}{
		{"absent", "", -1},
		{"seconds", "5", 5 * time.Second},
		{"fractional", "0.5", 500 * time.Millisecond},
		{"unparseable", "not-a-number", -1},
		// GitHub sends an HTTP-date form of Retry-After on some endpoints; it is not
		// a float, so it degrades to the exponential-backoff sentinel rather than
		// being silently read as a duration.
		{"http-date", "Wed, 21 Oct 2026 07:28:00 GMT", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{}
			if tc.retryAfter != "" {
				header.Set("Retry-After", tc.retryAfter)
			}
			got := httpx.ParseRateLimitError(httpx.SourceScaleSet, header)
			assert.Equal(t, tc.want, got.RetryAfter)
			assert.Equal(t, httpx.SourceScaleSet, got.Source, "Source must be attributed to the caller's protocol")
		})
	}
}

// TestRateLimitError_Message pins the exact strings the broker and scaleset
// packages produced before their taxonomies were unified (Q369) — the Source
// prefix is what preserves them.
func TestRateLimitError_Message(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  httpx.RateLimitError
		want string
	}{
		{"broker with delay", httpx.RateLimitError{Source: httpx.SourceBroker, RetryAfter: 30 * time.Second},
			"broker: rate limited, retry after 30s"},
		{"broker no header", httpx.RateLimitError{Source: httpx.SourceBroker, RetryAfter: -1},
			"broker: rate limited (no Retry-After header)"},
		{"scaleset with delay", httpx.RateLimitError{Source: httpx.SourceScaleSet, RetryAfter: 5 * time.Second},
			"scaleset: rate limited, retry after 5s"},
		{"scaleset no header", httpx.RateLimitError{Source: httpx.SourceScaleSet, RetryAfter: -1},
			"scaleset: rate limited (no Retry-After header)"},
		// A zero Source still reads as a sentence rather than ": rate limited".
		{"unattributed", httpx.RateLimitError{RetryAfter: -1}, "rate limited (no Retry-After header)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

func TestUnauthorizedError_Message(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  httpx.UnauthorizedError
		want string
	}{
		{"broker 401", httpx.UnauthorizedError{Source: httpx.SourceBroker, StatusCode: http.StatusUnauthorized},
			"broker: unauthorized (HTTP 401)"},
		{"scaleset 403", httpx.UnauthorizedError{Source: httpx.SourceScaleSet, StatusCode: http.StatusForbidden},
			"scaleset: unauthorized (HTTP 403)"},
		{"unattributed", httpx.UnauthorizedError{StatusCode: http.StatusUnauthorized}, "unauthorized (HTTP 401)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

// TestSharedErrors_MatchRegardlessOfSource is the property the unification exists
// for: one errors.As target matches an error raised by either protocol, wrapped or
// not. Source labels the message; it must never gate matching.
func TestSharedErrors_MatchRegardlessOfSource(t *testing.T) {
	t.Parallel()
	for _, source := range []httpx.Source{httpx.SourceBroker, httpx.SourceScaleSet, ""} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()

			wrappedRL := fmt.Errorf("GetMessage: %w", &httpx.RateLimitError{Source: source, RetryAfter: 7 * time.Second})
			var rl *httpx.RateLimitError
			require.ErrorAs(t, wrappedRL, &rl)
			assert.Equal(t, 7*time.Second, rl.RetryAfter)

			wrappedUnauth := fmt.Errorf("CreateSession: %w", &httpx.UnauthorizedError{Source: source, StatusCode: 403})
			var unauth *httpx.UnauthorizedError
			require.ErrorAs(t, wrappedUnauth, &unauth)
			assert.Equal(t, 403, unauth.StatusCode)

			// The two types stay distinguishable from each other.
			assert.False(t, errors.As(wrappedRL, &unauth), "a rate limit must not match UnauthorizedError")
			assert.False(t, errors.As(wrappedUnauth, &rl), "an auth failure must not match RateLimitError")
		})
	}
}
