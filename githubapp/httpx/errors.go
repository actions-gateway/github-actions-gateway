package httpx

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Source names the GitHub protocol a typed error came from. It is carried on the
// shared error types purely so their messages keep naming the protocol that
// failed; error *matching* (errors.As) never depends on it.
type Source string

const (
	// SourceBroker labels the classic runner broker / message-queue protocol
	// (the broker package).
	SourceBroker Source = "broker"
	// SourceScaleSet labels the runner-scale-set protocol against the Actions
	// Service (the scaleset package).
	SourceScaleSet Source = "scaleset"
)

// prefix renders "<source>: " for a named Source and "" for the zero value, so
// an error built without one still reads as a plain sentence.
func (s Source) prefix() string {
	if s == "" {
		return ""
	}
	return string(s) + ": "
}

// RateLimitError is returned when GitHub responds with 429 Too Many Requests.
// RetryAfter is the duration the caller should wait before retrying: it is
// parsed from the Retry-After header when present, and is otherwise -1 to signal
// that the caller should apply exponential backoff.
//
// This is the single declaration shared by every GitHub protocol client in the
// repo — broker.RateLimitError and scaleset.RateLimitError are aliases for it
// (Q369). A caller spanning both protocols therefore matches one type, and an
// errors.As against either name matches a rate limit raised by either protocol.
type RateLimitError struct {
	// Source names the protocol that was rate limited. It only prefixes
	// Error(); it never affects matching.
	Source Source
	// RetryAfter is -1 when the response carried no usable Retry-After header.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter < 0 {
		return e.Source.prefix() + "rate limited (no Retry-After header)"
	}
	return fmt.Sprintf("%srate limited, retry after %s", e.Source.prefix(), e.RetryAfter)
}

// UnauthorizedError is returned when GitHub responds with 401 Unauthorized or
// 403 Forbidden. Callers should treat it as a signal to refresh the credential
// behind the call — the bearer token for a broker session, the session token
// (RefreshSession) or admin connection for a scale set — before retrying.
//
// This is the single declaration shared by every GitHub protocol client in the
// repo — broker.UnauthorizedError and scaleset.UnauthorizedError are aliases for
// it (Q369), so an errors.As against either name matches an auth failure raised
// by either protocol.
type UnauthorizedError struct {
	// Source names the protocol that rejected the credential. It only prefixes
	// Error(); it never affects matching.
	Source Source
	// StatusCode is the rejecting status, 401 or 403.
	StatusCode int
}

func (e *UnauthorizedError) Error() string {
	return fmt.Sprintf("%sunauthorized (HTTP %d)", e.Source.prefix(), e.StatusCode)
}

// ParseRateLimitError builds a *RateLimitError attributed to source from a 429
// response's headers, honoring Retry-After (in seconds) when present. A missing
// or unparseable header yields RetryAfter -1, telling the caller to back off
// exponentially rather than trust a server-provided delay.
func ParseRateLimitError(source Source, header http.Header) *RateLimitError {
	ra := header.Get("Retry-After")
	if ra == "" {
		return &RateLimitError{Source: source, RetryAfter: -1}
	}
	secs, err := strconv.ParseFloat(ra, 64)
	if err != nil {
		return &RateLimitError{Source: source, RetryAfter: -1}
	}
	return &RateLimitError{Source: source, RetryAfter: time.Duration(secs * float64(time.Second))}
}
