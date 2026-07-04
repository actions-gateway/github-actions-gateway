package listener

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
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
	winner := m.claimJob(planID, SiblingDelivery{})
	if !winner.Won {
		t.Fatal("winner should acquire the claim on a fresh planID")
	}

	// A sibling delivered the same planID mid-flight is deduped (in-flight claim).
	if r := m.claimJob(planID, SiblingDelivery{}); r.Won {
		t.Fatal("a concurrent sibling must be deduped while the claim is in-flight")
	}

	// The winner completes: Complete does NOT free the planID immediately — it
	// lingers for ClaimLinger past completion.
	winner.Complete(broker.TaskResultSucceeded)

	// A late redelivery arriving well within the linger window (the winner's pod is
	// still lingering, unreaped) is deduped rather than reclaiming and colliding.
	now = now.Add(1 * time.Minute)
	if r := m.claimJob(planID, SiblingDelivery{}); r.Won {
		t.Fatal("a late redelivery within the pod-linger window must be deduped (Q260 residual)")
	}

	// Once the linger window elapses (the pod has been reaped), a genuine
	// redelivery of the same planID is provisionable again.
	now = now.Add(m.ClaimLinger)
	winner2 := m.claimJob(planID, SiblingDelivery{})
	if !winner2.Won {
		t.Fatal("after the linger window elapses the planID must be reclaimable")
	}
	winner2.Complete(broker.TaskResultSucceeded)
}

// TestMultiplexer_FanoutClaim_TracksSiblingsAndLateRedelivery exercises the Q260
// Option A registry behavior. Siblings deduped while the winner runs are registered
// and returned to the winner by Complete (so the winner can fan completion out to
// each). After the winner concludes, a late redelivery of the same planID arriving
// within the linger window is NOT registered for the now-gone winner — it is handed
// the winner's recorded terminal result so the caller resolves its own delivery.
func TestMultiplexer_FanoutClaim_TracksSiblingsAndLateRedelivery(t *testing.T) {
	const planID = "plan-fan"
	now := time.Unix(0, 0)
	m := NewMultiplexer(func(int) Config { return Config{} }, 1, nil)
	m.now = func() time.Time { return now }
	m.ClaimLinger = 5 * time.Minute

	winner := m.claimJob(planID, SiblingDelivery{RunnerRequestID: "d1"})
	if !winner.Won {
		t.Fatal("winner should acquire the claim")
	}

	// Two siblings deduped while the winner runs: each registered for the winner,
	// neither winning nor carrying a late result.
	for _, id := range []string{"d2", "d3"} {
		r := m.claimJob(planID, SiblingDelivery{RunnerRequestID: id})
		if r.Won || r.LateResult != "" {
			t.Fatalf("sibling %q should be a registered loser, got Won=%v LateResult=%q", id, r.Won, r.LateResult)
		}
	}

	// The winner concludes failed: Complete returns exactly the two registered
	// siblings, each keyed on its own RunnerRequestID.
	siblings := winner.Complete(broker.TaskResultFailed)
	got := map[string]bool{}
	for _, s := range siblings {
		got[s.RunnerRequestID] = true
	}
	if len(siblings) != 2 || !got["d2"] || !got["d3"] {
		t.Fatalf("winner should get its two registered siblings d2/d3, got %+v", siblings)
	}

	// A late redelivery within the linger window gets the winner's recorded result
	// (failed) to resolve its own delivery — it does not win, and is not registered.
	late := m.claimJob(planID, SiblingDelivery{RunnerRequestID: "d4"})
	if late.Won {
		t.Fatal("a late redelivery must not win the concluded claim")
	}
	if late.LateResult != broker.TaskResultFailed {
		t.Fatalf("late redelivery should carry the winner's recorded result, got %q", late.LateResult)
	}
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

	winner := m.claimJob(planID, SiblingDelivery{})
	if !winner.Won {
		t.Fatal("winner should acquire the claim")
	}
	winner.Complete(broker.TaskResultSucceeded)

	if r := m.claimJob(planID, SiblingDelivery{}); !r.Won {
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
		res := m.claimJob(id, SiblingDelivery{})
		if !res.Won {
			t.Fatalf("claim %q should succeed", id)
		}
		res.Complete(broker.TaskResultSucceeded)
	}
	// One more job is still in-flight (never released).
	if r := m.claimJob("live", SiblingDelivery{}); !r.Won {
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
	res := m.claimJob("d", SiblingDelivery{})
	if !res.Won {
		t.Fatal("claim d should succeed")
	}
	res.Complete(broker.TaskResultSucceeded)

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
