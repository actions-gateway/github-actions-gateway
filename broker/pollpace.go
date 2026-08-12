package broker

import (
	"context"
	"time"
)

// MinPollInterval is the floor on the time between two GetMessage polls that
// deliver nothing (a 202). A poll loop otherwise re-polls an empty answer with
// no pause at all: a server that answers "nothing to deliver" promptly — a GHES
// tenant with a short poll window, an intermediary that terminates the long
// poll, a test stub that never holds one — spins the caller into a request storm
// against GitHub, and the rate limiter answers for us.
//
// It is a floor on the *interval*, not a sleep per poll, so a server that really
// does hold the poll (the real broker blocks ~50s) never waits on it: only a 202
// that came back faster than the floor is padded out to it. The cost on such a
// server is exactly zero, and the delay it can add to a job assignment is
// bounded by MinPollInterval. It matches the ScaleSet queue client's own floor
// (Q287).
const MinPollInterval = 100 * time.Millisecond

// PaceEmptyPoll waits out whatever is left of MinPollInterval after an empty
// poll that took elapsed, and reports whether the caller may poll again — false
// once ctx is cancelled. A poll that already blocked at least MinPollInterval
// returns immediately.
func PaceEmptyPoll(ctx context.Context, elapsed time.Duration) bool {
	remaining := MinPollInterval - elapsed
	if remaining <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(remaining)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
