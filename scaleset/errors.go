package scaleset

import (
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

// errSource labels this package's shared typed errors, keeping their messages
// prefixed "scaleset: " now that the types themselves are shared with the
// classic broker protocol client (Q369).
const errSource = httpx.SourceScaleSet

// UnauthorizedError is returned when the Actions Service or the message queue
// responds 401 Unauthorized or 403 Forbidden. For an admin-JWT call the caller
// should re-mint the admin connection; for a queue call (GetMessage/AcquireJobs)
// it should refresh the session token (RefreshSession) and retry (Q264 plan §2.5).
//
// It is an alias for httpx.UnauthorizedError, the one declaration shared with the
// classic broker protocol client (Q369): a caller handling both protocols matches
// a single type, and errors.As against this name also matches a broker auth
// failure.
type UnauthorizedError = httpx.UnauthorizedError

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

// RunnerBusyError is returned by DeregisterRunnerByName when the runner record it
// tried to delete is currently running a job (422 Unprocessable Entity, "is still
// running a job"). The record is held by a live runner, so deleting it would be
// wrong — the caller must leave it in place (a reaped never-started worker's record
// is instead offline, so this never blocks the Q334 cleanup path).
type RunnerBusyError struct {
	Name string
}

func (e *RunnerBusyError) Error() string {
	return fmt.Sprintf("scaleset: runner %q is currently running a job and cannot be deleted", e.Name)
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
//
// It is an alias for httpx.RateLimitError, the one declaration shared with the
// classic broker protocol client (Q369).
type RateLimitError = httpx.RateLimitError
