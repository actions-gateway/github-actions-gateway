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

// ConflictError is the generic 409 Conflict the Actions Service returns. Its meaning
// is endpoint-specific, so the shared status-to-error mapping (statusError) yields
// this neutral type and each call translates it into a precise one: CreateSession
// into a SessionConflictError (one active session per scale set), GenerateJITConfig
// into a RunnerNameConflictError (the runner name is already registered). An
// untranslated ConflictError surfaces a 409 from a call that assigns it no specific
// meaning. Distinguishing the two 409 causes is what stops a generatejitconfig
// runner-name conflict from being mislabeled a session conflict (Q270).
type ConflictError struct {
	StatusCode int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("scaleset: conflict (HTTP %d)", e.StatusCode)
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

// RunnerNameConflictError is returned by GenerateJITConfig when the runner name it
// tried to pre-register is already taken (409 Conflict) — distinct from a
// SessionConflictError, which a session-create 409 yields. The name collided (a stale
// runner record, or a replay racing an earlier attempt), so the caller must retry
// under a *fresh* runner name rather than replay the same request, which would
// conflict indefinitely and wedge the queue cursor behind it (Q270).
type RunnerNameConflictError struct {
	StatusCode int
}

func (e *RunnerNameConflictError) Error() string {
	return fmt.Sprintf("scaleset: runner name already registered (HTTP %d)", e.StatusCode)
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
