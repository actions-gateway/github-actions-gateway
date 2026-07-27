package provisioner

import (
	"context"
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
	quotaCalls     int
	declinedCalls  int
	ceilingCalls   int
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
func (s *rungTarget) RecordEvent(_, _, _, _ string)                         {}
func (s *rungTarget) Resolve(context.Context) (*ResolvedSpec, error)        { return &ResolvedSpec{}, nil }

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
