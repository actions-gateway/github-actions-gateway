package provisioner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rungTarget is a Target stub whose three admission rungs are set independently, so a
// test can pin WHICH rung refused rather than only that some rung did.
type rungTarget struct {
	ceiling        int32
	ceilingBounded bool
	quotaExhausted bool
	declined       bool
	declinedDetail string
	scaleUp        *ScaleUpConfig
	quotaCalls     int
	declinedCalls  int
	ceilingCalls   int
	scaleUpCalls   int
}

func (s *rungTarget) Key() client.ObjectKey             { return client.ObjectKey{Namespace: "ns", Name: "s"} }
func (s *rungTarget) OwnerRef() metav1.OwnerReference   { return metav1.OwnerReference{} }
func (s *rungTarget) PodOwnerLabels() map[string]string { return nil }
func (s *rungTarget) Ceiling(context.Context) (int32, bool) {
	s.ceilingCalls++
	return s.ceiling, s.ceilingBounded
}
func (s *rungTarget) QuotaExhausted(context.Context) (bool, string) {
	s.quotaCalls++
	return s.quotaExhausted, "quota detail"
}
func (s *rungTarget) QuotaCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (s *rungTarget) CapacityDeclined(context.Context) (bool, string) {
	s.declinedCalls++
	return s.declined, s.declinedDetail
}
func (s *rungTarget) DeclinedCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (s *rungTarget) ScaleUpLimit(context.Context) *ScaleUpConfig {
	s.scaleUpCalls++
	return s.scaleUp
}
func (s *rungTarget) RecordEvent(_, _, _, _ string)                  {}
func (s *rungTarget) Resolve(context.Context) (*ResolvedSpec, error) { return &ResolvedSpec{}, nil }

// TestAdmit_CapacityRung pins the Q405 rung's placement in the ladder and its
// rejection reason. The ordering matters beyond tidiness: the capacity rung reserves
// nothing, so it must sit ahead of the ceiling rung — refusing after a reservation
// would leak a slot on every declined job.
func TestAdmit_CapacityRung(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		target     rungTarget
		wantOK     bool
		wantReason string
	}{
		{
			name:   "gate open admits",
			target: rungTarget{ceiling: 5, ceilingBounded: true},
			wantOK: true,
		},
		{
			name:       "a declining gate refuses with reason capacity",
			target:     rungTarget{ceiling: 5, ceilingBounded: true, declined: true, declinedDetail: "no node fits"},
			wantReason: runnercore.AdmitReasonCapacity,
		},
		{
			// Quota is rung 1 and answers a different question (namespace-wide
			// headroom); when both refuse, the operator is told about quota first
			// because that is the one they can fix without touching the cluster.
			name:       "quota outranks capacity when both refuse",
			target:     rungTarget{ceiling: 5, ceilingBounded: true, quotaExhausted: true, declined: true},
			wantReason: runnercore.AdmitReasonQuota,
		},
		{
			// The ceiling rung is last precisely because it is the only one that
			// reserves; a capacity refusal must never consume a slot.
			name:       "capacity outranks the ceiling when both would refuse",
			target:     rungTarget{ceiling: 0, ceilingBounded: true, declined: true},
			wantReason: runnercore.AdmitReasonCapacity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvisioner(nil, nil, nil)
			target := tt.target

			release, ok, reason := p.Admit(&target)(ctx)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantReason, reason)
			if !ok {
				assert.Nil(t, release, "a refused job must carry no release func")
				assert.Zero(t, p.admission.reservedCount(target.Key().String()),
					"a rung that reserves nothing must leave the in-flight count untouched")
			}
		})
	}
}

// TestAdmit_CapacityRungIsSkippedWhenTheGateIsOff is the no-cost-for-the-default
// assertion. The mode lives in the owner's spec, so "off" is the Target answering
// false — the rung is still consulted, and the point is that consulting it changes
// nothing and reserves nothing.
func TestAdmit_CapacityRungIsSkippedWhenTheGateIsOff(t *testing.T) {
	target := &rungTarget{ceiling: 2, ceilingBounded: true}
	p := NewProvisioner(nil, nil, nil)

	release, ok, reason := p.Admit(target)(context.Background())

	require.True(t, ok, "a set with no capacity gate must admit exactly as before Q405")
	assert.Empty(t, reason)
	require.NotNil(t, release)
	release()
}

// TestAdmit_CapacityRungFailsOpen covers the contract that matters most for this rung:
// it may under-gate freely, but over-gating starves a tenant. A Target that cannot
// answer reports not-declined, and the job is claimed exactly as it is today.
func TestAdmit_CapacityRungFailsOpen(t *testing.T) {
	// declined=false is what every unreadable path in the adapter returns — an
	// unreadable RunnerSet, an absent condition, a set that never opted in.
	target := &rungTarget{ceiling: 3, ceilingBounded: true, declined: false}
	p := NewProvisioner(nil, nil, nil)

	_, ok, reason := p.Admit(target)(context.Background())

	assert.True(t, ok, "an unanswerable capacity signal must never gate intake")
	assert.Empty(t, reason)
}

// TestAdmit_CapacityRungRereadsEveryDelivery pins the Q117 property for this rung: the
// AdmitFunc is built once when the listener starts, so a gate that closes (or opens)
// must take effect on the next delivered job without an AGC restart.
//
// It is also the trickle property in miniature — the cycle that keeps a Phase 1 gate
// non-suppressing. The gate closes on a stuck pod, the reaper deletes that pod at
// pendingPodDeadline, the condition clears, and exactly one more job is admitted; if
// capacity is still absent the new pod trips the gate again. Without the re-read, the
// first close would be permanent and the tenant would be starved rather than throttled.
func TestAdmit_CapacityRungRereadsEveryDelivery(t *testing.T) {
	ctx := context.Background()
	target := &rungTarget{ceiling: 10, ceilingBounded: true}
	p := NewProvisioner(nil, nil, nil)
	admit := p.Admit(target)

	// A worker pod goes unschedulable: the gate closes and intake stops.
	target.declined = true
	_, ok, reason := admit(ctx)
	require.False(t, ok)
	require.Equal(t, runnercore.AdmitReasonCapacity, reason)

	// The reaper deletes the stuck pod at the deadline, so the condition clears.
	target.declined = false
	release, ok, _ := admit(ctx)
	require.True(t, ok, "a cleared gate must readmit on the next delivery, with no restart")
	require.NotNil(t, release)
	release()

	// That job's pod is unschedulable too, so the gate closes again — one claim per
	// deadline window rather than one per delivered job.
	target.declined = true
	_, ok, _ = admit(ctx)
	assert.False(t, ok, "the gate must re-close on the next stuck pod")

	assert.Equal(t, 3, target.declinedCalls, "the rung must be re-read on every delivery, never cached")
}

// TestAdmit_ScaleUpRung pins the Q717 rung: the classic tier charges the scale-up
// token bucket BEFORE it claims the job, so a burst that outruns the bucket is left
// queued at GitHub rather than claimed and then slept on with its job lock held.
//
// The rung takes its token rather than reading the bucket, which is what makes it
// bind on the case spec.scaleUp exists for. An observing rung would let every
// listener in a simultaneously-delivered burst see the same free token and claim
// anyway — the second admit below is precisely that job, and it must be refused.
func TestAdmit_ScaleUpRung(t *testing.T) {
	ctx := context.Background()
	p := NewProvisioner(nil, nil, nil)
	p.scaleUp = scaleUpLimiter{now: newFakeClock().now}

	target := &rungTarget{ceiling: 10, ceilingBounded: true, scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}}
	key := target.Key().String()
	admit := p.Admit(target)

	release, ok, reason := admit(ctx)
	require.True(t, ok, "the first job is within the burst")
	assert.Empty(t, reason)
	require.NotNil(t, release)
	require.Equal(t, int32(1), p.admission.reservedCount(key))

	_, ok, reason = admit(ctx)
	assert.False(t, ok, "the second job in the same instant has no token and must not be claimed")
	assert.Equal(t, runnercore.AdmitReasonScaleUp, reason)

	// The rate rung runs BEHIND the ceiling rung and hands its reservation straight
	// back, so a throttled job costs no slot once the call returns. Ordering it ahead
	// instead would spend a token on every ceiling refusal, which is the defect
	// TestAdmit_CeilingRefusalDoesNotSpendAToken pins.
	assert.Equal(t, int32(1), p.admission.reservedCount(key),
		"a job refused by the rate rung must hand its ceiling reservation straight back")
}

// TestAdmit_ScaleUpRungDefaultOff pins the default-off contract on the classic tier:
// an owner with no spec.scaleUp is never refused for rate, however many jobs arrive
// at once.
func TestAdmit_ScaleUpRungDefaultOff(t *testing.T) {
	ctx := context.Background()
	p := NewProvisioner(nil, nil, nil)
	target := &rungTarget{} // scaleUp nil
	admit := p.Admit(target)

	for i := range 50 {
		_, ok, reason := admit(ctx)
		require.True(t, ok, "job %d must be admitted with no rate limit configured", i)
		require.Empty(t, reason)
	}
	assert.Equal(t, 50, target.scaleUpCalls, "the rung is still re-read per job (Q117)")
}

// TestAdmit_ScaleUpRungOrder pins the rung's placement. The two observed rungs are
// the ones that do not clear on their own, so they are reported in preference to a
// rate refusal that the bucket will resolve within 1/maxPerSecond — and, more
// sharply, a job refused for quota or capacity must not be charged a token it will
// never spend.
func TestAdmit_ScaleUpRungOrder(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name       string
		target     rungTarget
		wantReason string
	}{
		{
			name:       "quota outranks an empty bucket",
			target:     rungTarget{quotaExhausted: true, scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}},
			wantReason: runnercore.AdmitReasonQuota,
		},
		{
			name:       "capacity outranks an empty bucket",
			target:     rungTarget{declined: true, declinedDetail: "d", scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 1}},
			wantReason: runnercore.AdmitReasonCapacity,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvisioner(nil, nil, nil)
			p.scaleUp = scaleUpLimiter{now: newFakeClock().now}
			target := tt.target

			// Drain the single burst token so the rate rung would refuse if reached.
			require.True(t, p.scaleUp.allow(target.Key().String(), target.scaleUp))

			_, ok, reason := p.Admit(&target)(ctx)
			assert.False(t, ok)
			assert.Equal(t, tt.wantReason, reason)
			assert.Zero(t, target.scaleUpCalls, "a rung that refuses earlier must not charge a token")
			assert.Zero(t, target.ceilingCalls, "nor take a reservation")
		})
	}
}

// TestAdmit_CeilingRefusalDoesNotSpendAToken pins the rung ORDER, which is the whole
// reason the rate rung sits last. Its token is the only thing this ladder spends that
// it cannot hand back: a reservation is refundable and a token taken is gone.
//
// Run the other way round — rate ahead of ceiling — a set sitting at its ceiling under
// continued delivery spends a token per refusal. The bucket pins at zero, so a slot
// freed by a finishing worker cannot be filled until a refill beats the refusal churn,
// and every refusal past the burst reports reason="scaleup" for a set whose actual
// limit is maxWorkers. That misattribution is the expensive half: the ledger says the
// reason label names what to raise, so it sends an operator to raise maxPerSecond,
// which feeds the churn faster.
//
// Found in review of the change that added the rung, with this exact shape.
func TestAdmit_CeilingRefusalDoesNotSpendAToken(t *testing.T) {
	ctx := context.Background()
	p := NewProvisioner(nil, nil, nil)
	p.scaleUp = scaleUpLimiter{now: newFakeClock().now}

	// One slot, five tokens: the ceiling is what binds, and it binds first.
	target := &rungTarget{ceiling: 1, ceilingBounded: true, scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: 5}}
	admit := p.Admit(target)

	release, ok, _ := admit(ctx)
	require.True(t, ok, "the first job takes the only slot")

	reasons := make([]string, 0, 10)
	for range 10 {
		_, _, reason := admit(ctx)
		reasons = append(reasons, reason)
	}
	for i, reason := range reasons {
		assert.Equal(t, runnercore.AdmitReasonCeiling, reason,
			"delivery %d is refused by the ceiling, so it must be REPORTED as the ceiling", i)
	}

	free, limited := p.scaleUp.tokens(target.Key().String(), target.scaleUp)
	require.True(t, limited)
	assert.Equal(t, int32(4), free,
		"only the admitted job spent a token: ceiling refusals must not drain the bucket")

	// The property the drained bucket would have broken.
	release()
	_, ok, reason := admit(ctx)
	assert.True(t, ok, "a freed ceiling slot must be immediately fillable; refused for %q", reason)
}

// concurrentTarget is a Target safe to drive from many goroutines at once. rungTarget
// counts its calls in plain ints, which is a data race under concurrency and not worth
// fixing there — no shipped test drives it that way. This one answers the same rungs
// and counts nothing.
type concurrentTarget struct {
	ceiling int32
	scaleUp *ScaleUpConfig
}

func (c *concurrentTarget) Key() client.ObjectKey {
	return client.ObjectKey{Namespace: "ns", Name: "s"}
}
func (c *concurrentTarget) OwnerRef() metav1.OwnerReference   { return metav1.OwnerReference{} }
func (c *concurrentTarget) PodOwnerLabels() map[string]string { return nil }
func (c *concurrentTarget) Ceiling(context.Context) (int32, bool) {
	return c.ceiling, true
}
func (c *concurrentTarget) QuotaExhausted(context.Context) (bool, string)      { return false, "" }
func (c *concurrentTarget) QuotaCapacity(context.Context, int32) (int32, bool) { return 0, false }
func (c *concurrentTarget) CapacityDeclined(context.Context) (bool, string)    { return false, "" }
func (c *concurrentTarget) DeclinedCapacity(context.Context, int32) (int32, bool) {
	return 0, false
}
func (c *concurrentTarget) ScaleUpLimit(context.Context) *ScaleUpConfig { return c.scaleUp }
func (c *concurrentTarget) RecordEvent(_, _, _, _ string)               {}
func (c *concurrentTarget) Resolve(context.Context) (*ResolvedSpec, error) {
	return &ResolvedSpec{}, nil
}

// TestAdmit_RateRefusalRefundsUnderConcurrency is the one the sequential tests cannot
// see. The rate rung takes a ceiling reservation and hands it back when the bucket is
// empty, and the two rungs are reached by every listener at once during exactly the
// burst spec.scaleUp exists to smooth — so the question is whether the reservation
// count is still exact after a wave of admits and refusals interleave.
//
// The invariant is arithmetic rather than timing: however the goroutines interleave,
// the gate must hold exactly one reservation per goroutine that was ADMITTED. A
// refusal that forgot its refund leaks a slot and the count runs high; a double
// refund would run it low.
func TestAdmit_RateRefusalRefundsUnderConcurrency(t *testing.T) {
	const goroutines = 200
	const burst = 5

	p := NewProvisioner(nil, nil, nil)
	p.scaleUp = scaleUpLimiter{now: newFakeClock().now}
	target := &concurrentTarget{ceiling: goroutines, scaleUp: &ScaleUpConfig{MaxPerSecond: 1, Burst: burst}}
	admit := p.Admit(target)

	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		admitted atomic.Int32
	)
	start.Add(1)
	for range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if _, ok, _ := admit(context.Background()); ok {
				admitted.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	// The bucket is the binding rung: the ceiling is wide enough for everyone.
	require.Equal(t, int32(burst), admitted.Load(),
		"the frozen clock refills nothing, so exactly the burst may be admitted")
	assert.Equal(t, admitted.Load(), p.admission.reservedCount(target.Key().String()),
		"the gate must hold exactly one reservation per admitted job: a refusal that "+
			"skipped its refund leaks a slot, and a double refund loses one")

	// A post-hoc check that the ceiling invariant held: this reads the SETTLED count,
	// after done.Wait(), so it cannot observe a mid-flight excursion and nothing here
	// would fail from one. The invariant it corresponds to — admit refuses at
	// reserved >= limit, so the reserve-then-release churn inflates the count only
	// WITHIN the ceiling, which is what makes Q977 a labelling artifact rather than an
	// over-admission bug — is a property of the code, not something this assertion
	// establishes.
	assert.LessOrEqual(t, p.admission.reservedCount(target.Key().String()), target.ceiling,
		"the settled reservation count must not exceed the declared ceiling")
}
