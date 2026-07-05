package scaleset

import (
	"fmt"
	"time"
)

// UnauthorizedError is returned when the Actions Service or the message queue
// responds 401 Unauthorized or 403 Forbidden. For an admin-JWT call the caller
// should re-mint the admin connection; for a queue call (GetMessage/AcquireJobs)
// it should refresh the session token (RefreshSession) and retry (Q264 plan §2.5).
type UnauthorizedError struct {
	StatusCode int
}

func (e *UnauthorizedError) Error() string {
	return fmt.Sprintf("scaleset: unauthorized (HTTP %d)", e.StatusCode)
}

// SessionConflictError is returned by CreateSession when a session already exists
// for the scale set (409 Conflict) — one active session per scale set is a
// protocol invariant. The caller must delete or wait out the existing session
// before creating another (Q264 plan §2.2).
type SessionConflictError struct {
	StatusCode int
}

func (e *SessionConflictError) Error() string {
	return fmt.Sprintf("scaleset: session already exists (HTTP %d)", e.StatusCode)
}

// NotFoundError is returned when a scale set, session, or message no longer exists
// server-side (404 Not Found or 410 Gone). For a session it signals recovery by
// re-create (the queue replays unacked messages to a fresh session — §2b-3).
type NotFoundError struct {
	StatusCode int
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("scaleset: not found (HTTP %d)", e.StatusCode)
}

// RateLimitError is returned on 429 Too Many Requests. RetryAfter is parsed from
// the Retry-After header when present; -1 signals the caller should apply
// exponential backoff. The live probe observed no rate-limit headers on the queue
// (Q264 plan §2a-5), so -1 is the common case.
type RateLimitError struct {
	RetryAfter time.Duration // -1 if no Retry-After header was present
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter < 0 {
		return "scaleset: rate limited (no Retry-After header)"
	}
	return fmt.Sprintf("scaleset: rate limited, retry after %s", e.RetryAfter)
}
