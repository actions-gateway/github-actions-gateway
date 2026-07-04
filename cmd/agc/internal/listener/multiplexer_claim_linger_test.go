package listener

import (
	"testing"
	"time"
)

// TestMultiplexer_ClaimLinger_DedupsLateRedelivery exercises the Q260 redelivery
// residual fix at the claim-registry level: after the winning goroutine releases
// a planID claim, the claim must LINGER for ClaimLinger (the owner's
// completedPodTTL) so a late GitHub redelivery of the same planID — arriving while
// the winner's Completed-but-not-yet-reaped worker pod still exists — is deduped
// rather than reclaiming the planID and colliding on `create Pod`.
//
// It drives claimJob directly with an injected clock so the linger expiry is
// deterministic (no sleeps). Against the pre-fix behavior (delete-on-release) the
// second claim after release would succeed, which is the exact collision this
// guards against.
func TestMultiplexer_ClaimLinger_DedupsLateRedelivery(t *testing.T) {
	const planID = "plan-A"
	now := time.Unix(0, 0)
	m := NewMultiplexer(func(int) Config { return Config{} }, 1, nil)
	m.now = func() time.Time { return now }
	m.ClaimLinger = 5 * time.Minute

	// The winner claims the planID.
	release, ok := m.claimJob(planID)
	if !ok {
		t.Fatal("winner should acquire the claim on a fresh planID")
	}

	// A sibling delivered the same planID mid-flight is deduped (in-flight claim).
	if _, ok := m.claimJob(planID); ok {
		t.Fatal("a concurrent sibling must be deduped while the claim is in-flight")
	}

	// The winner completes: release does NOT free the planID immediately — it
	// lingers for ClaimLinger past completion.
	release()

	// A late redelivery arriving well within the linger window (the winner's pod is
	// still lingering, unreaped) is deduped rather than reclaiming and colliding.
	now = now.Add(1 * time.Minute)
	if _, ok := m.claimJob(planID); ok {
		t.Fatal("a late redelivery within the pod-linger window must be deduped (Q260 residual)")
	}

	// Once the linger window elapses (the pod has been reaped), a genuine
	// redelivery of the same planID is provisionable again.
	now = now.Add(m.ClaimLinger)
	release2, ok := m.claimJob(planID)
	if !ok {
		t.Fatal("after the linger window elapses the planID must be reclaimable")
	}
	release2()
}

// TestMultiplexer_ClaimLinger_ZeroFreesImmediately verifies that when ClaimLinger
// is zero — the owner reaps terminal pods synchronously on completion, so none
// linger — release frees the planID at once, preserving the original
// delete-on-completion behavior (no needless dedup of a genuine redelivery).
func TestMultiplexer_ClaimLinger_ZeroFreesImmediately(t *testing.T) {
	const planID = "plan-Z"
	now := time.Unix(0, 0)
	m := NewMultiplexer(func(int) Config { return Config{} }, 1, nil)
	m.now = func() time.Time { return now }
	m.ClaimLinger = 0

	release, ok := m.claimJob(planID)
	if !ok {
		t.Fatal("winner should acquire the claim")
	}
	release()

	if _, ok := m.claimJob(planID); !ok {
		t.Fatal("with ClaimLinger==0 the planID must be free immediately after release")
	}
}

// TestMultiplexer_ClaimLinger_SweepBoundsMap verifies the lazy sweep reclaims
// expired lingering entries so the claim map does not grow unbounded with every
// completed planID. A distinct in-flight planID is never swept.
func TestMultiplexer_ClaimLinger_SweepBoundsMap(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMultiplexer(func(int) Config { return Config{} }, 1, nil)
	m.now = func() time.Time { return now }
	m.ClaimLinger = time.Minute

	// Complete several jobs; each leaves a lingering entry.
	for _, id := range []string{"a", "b", "c"} {
		release, ok := m.claimJob(id)
		if !ok {
			t.Fatalf("claim %q should succeed", id)
		}
		release()
	}
	// One more job is still in-flight (never released).
	if _, ok := m.claimJob("live"); !ok {
		t.Fatal("in-flight claim should succeed")
	}

	m.jobClaimsMu.Lock()
	before := len(m.jobClaims)
	m.jobClaimsMu.Unlock()
	if before != 4 {
		t.Fatalf("expected 4 claim entries (3 lingering + 1 in-flight), got %d", before)
	}

	// Advance past the linger window and trigger a sweep via a fresh claim.
	now = now.Add(2 * time.Minute)
	release, ok := m.claimJob("d")
	if !ok {
		t.Fatal("claim d should succeed")
	}
	release()

	m.jobClaimsMu.Lock()
	defer m.jobClaimsMu.Unlock()
	// The 3 expired lingering entries are swept; the in-flight "live" and the
	// freshly lingering "d" remain.
	if _, live := m.jobClaims["live"]; !live {
		t.Fatal("in-flight claim must never be swept")
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, present := m.jobClaims[id]; present {
			t.Fatalf("expired lingering claim %q should have been swept", id)
		}
	}
}
