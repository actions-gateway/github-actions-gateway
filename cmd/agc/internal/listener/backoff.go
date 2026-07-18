package listener

import (
	"math/rand"
	"time"
)

// jitterBackoff returns d with full jitter applied over [d/2, d], so concurrent
// recyclers under a burst do not resynchronize their retries into a thundering
// herd. A non-positive d returns 0.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not crypto
}

// BackoffDelay returns a jittered delay matching the two-tier policy from
// MessageListener.cs: up to 5 errors → [15s,30s); beyond 5 → [30s,60s).
func BackoffDelay(consecutiveErrors int, _ Clock) time.Duration {
	if consecutiveErrors <= 5 {
		return 15*time.Second + time.Duration(rand.Int63n(int64(15*time.Second))) //nolint:gosec // jitter, not crypto
	}
	return 30*time.Second + time.Duration(rand.Int63n(int64(30*time.Second))) //nolint:gosec // jitter, not crypto
}
