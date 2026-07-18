package listener

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

// NonRetriableError wraps an error from Run that indicates a permanent failure
// condition for this goroutine (e.g. version too old, unauthorized). The
// Multiplexer uses this to suppress automatic restart of the permanent baseline.
type NonRetriableError struct {
	Cause error
}

func (e *NonRetriableError) Error() string { return "non-retriable: " + e.Cause.Error() }
func (e *NonRetriableError) Unwrap() error { return e.Cause }

func isUnauthorized(err error) bool {
	var typed *broker.UnauthorizedError
	return errors.As(err, &typed)
}

func isSessionExpired(err error) bool {
	var typed *broker.SessionExpiredError
	return errors.As(err, &typed)
}

// isPollTimeout reports whether a GetMessage error is a client-side timeout
// rather than a broker-reported status. It fires when the broker client's
// ResponseHeaderTimeout (broker.LongPollResponseHeaderTimeout) elapses on a
// black-holed long-poll — the broker accepts the connection but never answers
// (Q108). The poll loop already returns early on parent-context cancellation, so
// the only timeout that reaches here is the per-request response-header (or
// connect) deadline, which is a benign "no message, retry" — not a session-level
// failure that should trip backoff or a heal.
func isPollTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isDecodeEOF reports whether a GetMessage error is a 200 response whose body
// could not be decoded because it was empty or truncated — observed live as
// GitHub's response signature once a session's single-use JIT runner record
// has been deleted (Q114, M4 §12: "decode response: EOF").
func isDecodeEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// isRegistrationNotFound reports whether a broker OAuth token fetch failed with
// the transient "Registration … was not found" response GitHub's token endpoint
// returns for a runner record that exists but whose registration the OAuth
// service has not yet caught up to — the propagation window just after
// generate-jitconfig (Q267). It is deliberately narrower than isTokenRejected:
// only this specific body warrants riding out the lag by retrying the SAME fresh
// credentials, whereas a broad credential rejection means the record is genuinely
// gone and the agent must be recycled (Q114).
func isRegistrationNotFound(err error) bool {
	var typed *githubapp.TokenExchangeError
	if !errors.As(err, &typed) {
		return false
	}
	if typed.StatusCode != http.StatusBadRequest && typed.StatusCode != http.StatusNotFound {
		return false
	}
	body := strings.ToLower(typed.Body)
	return strings.Contains(body, "registration") && strings.Contains(body, "not found")
}

// isTokenRejected reports whether a broker OAuth token fetch failed because
// the token service rejected the client credentials (as opposed to a
// transport or server failure). For a single-use JIT agent this happens once
// GitHub deletes the runner record behind the credential (Q114).
func isTokenRejected(err error) bool {
	var typed *githubapp.TokenExchangeError
	if !errors.As(err, &typed) {
		return false
	}
	switch typed.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// 400 covers OAuth's invalid_client convention (RFC 6749 §5.2), which
		// some token services use instead of 401 for unknown clients.
		return true
	default:
		return false
	}
}
