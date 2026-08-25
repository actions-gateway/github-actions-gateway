package controller

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The capacity accounting on RunnerSet status (Q721): the same numbers the scale-set
// tier publishes to Prometheus, reachable from `kubectl describe` by a tenant who
// cannot read metrics. These tests pin the two properties that make the field usable —
// "not yet advertised" is distinguishable from "advertising zero", and the withheld
// list does not churn between reconciles — plus the clearing that keeps it from
// describing intake that no longer exists.

// TestApplyAdvertisedCapacity_BeforeFirstPoll pins the reason advertisedCapacity is a
// pointer. A listener that has started but not yet polled has made no advertisement,
// and writing 0 there would read as "intake fully withheld", which is the opposite
// claim.
func TestApplyAdvertisedCapacity_BeforeFirstPoll(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}

	applyAdvertisedCapacity(rs, &capacityRecord{})

	assert.Nil(t, rs.Status.AdvertisedCapacity, "no poll has happened, so nothing has been advertised")
	assert.Empty(t, rs.Status.WithheldCapacity)
}

// TestApplyAdvertisedCapacity_PublishesTheAccounting checks the arithmetic a tenant
// reads: the advertised total, and every evaluated rung's contribution — including the
// explicit zeros, which are what let a reader tell "this rung is not withholding" from
// "this rung was never evaluated".
func TestApplyAdvertisedCapacity_PublishesTheAccounting(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}
	rec := &capacityRecord{}
	rec.record(provisioner.CapacityAdvertisement{
		Total:   3,
		Ceiling: 8,
		Withheld: map[string]int32{
			runnercore.AdmitReasonQuota:   5,
			runnercore.AdmitReasonScaleUp: 0,
		},
	})

	applyAdvertisedCapacity(rs, rec)

	require.NotNil(t, rs.Status.AdvertisedCapacity)
	assert.Equal(t, int32(3), *rs.Status.AdvertisedCapacity)
	assert.Equal(t, []v2alpha1.WithheldCapacity{
		{Reason: runnercore.AdmitReasonQuota, Slots: 5},
		{Reason: runnercore.AdmitReasonScaleUp, Slots: 0},
	}, rs.Status.WithheldCapacity)
}

// TestApplyAdvertisedCapacity_ZeroIsAnAdvertisement is the other half of the pointer:
// a set withholding everything advertises 0, and that has to survive as a published
// value rather than reading as "nothing published yet".
func TestApplyAdvertisedCapacity_ZeroIsAnAdvertisement(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}
	rec := &capacityRecord{}
	rec.record(provisioner.CapacityAdvertisement{
		Total:    0,
		Ceiling:  8,
		Withheld: map[string]int32{runnercore.AdmitReasonQuota: 8},
	})

	applyAdvertisedCapacity(rs, rec)

	require.NotNil(t, rs.Status.AdvertisedCapacity, "advertising zero is a statement, not an absence")
	assert.Equal(t, int32(0), *rs.Status.AdvertisedCapacity)
}

// TestApplyAdvertisedCapacity_OrderIsStable pins the sort. The advertisement carries
// its rungs in a map, whose iteration order Go deliberately randomises, so an unsorted
// write would reorder the list on some fraction of reconciles — producing a status
// update, and a watch event, whenever the map iterated differently rather than when
// the capacity changed.
func TestApplyAdvertisedCapacity_OrderIsStable(t *testing.T) {
	adv := provisioner.CapacityAdvertisement{
		Total:   1,
		Ceiling: 9,
		Withheld: map[string]int32{
			runnercore.AdmitReasonScaleUp:  3,
			runnercore.AdmitReasonQuota:    4,
			runnercore.AdmitReasonCapacity: 1,
		},
	}

	var first []v2alpha1.WithheldCapacity
	for i := range 50 {
		rs := &v2alpha1.RunnerSet{}
		rec := &capacityRecord{}
		rec.record(adv)
		applyAdvertisedCapacity(rs, rec)
		if i == 0 {
			first = rs.Status.WithheldCapacity
			continue
		}
		require.Equal(t, first, rs.Status.WithheldCapacity, "reconcile %d reordered the list", i)
	}
	assert.Equal(t, []string{
		runnercore.AdmitReasonCapacity,
		runnercore.AdmitReasonQuota,
		runnercore.AdmitReasonScaleUp,
	}, []string{first[0].Reason, first[1].Reason, first[2].Reason})
}

// TestClearAdvertisedCapacity drops both fields together. A set with no listener
// advertises nothing, so leaving the last advertisement behind would report withheld
// slots against intake that no longer exists — the same reasoning that zeroes
// activeSessions on those paths.
func TestClearAdvertisedCapacity(t *testing.T) {
	total := int32(4)
	rs := &v2alpha1.RunnerSet{}
	rs.Status.AdvertisedCapacity = &total
	rs.Status.WithheldCapacity = []v2alpha1.WithheldCapacity{{Reason: runnercore.AdmitReasonQuota, Slots: 4}}

	clearAdvertisedCapacity(rs)

	assert.Nil(t, rs.Status.AdvertisedCapacity)
	assert.Nil(t, rs.Status.WithheldCapacity)
}

// TestCapacityRecord_KeepsOnlyTheLatest pins the handoff's contract: status reports
// the set's current intake, not a history, so a second poll replaces the first.
func TestCapacityRecord_KeepsOnlyTheLatest(t *testing.T) {
	rec := &capacityRecord{}
	rec.record(provisioner.CapacityAdvertisement{Total: 1, Ceiling: 8})
	rec.record(provisioner.CapacityAdvertisement{Total: 6, Ceiling: 8})

	adv, ok := rec.last()

	require.True(t, ok)
	assert.Equal(t, int32(6), adv.Total)
}

// TestCapacityRecord_NilReceiver keeps a handle built without a record (a hand-built
// one in a test, or a stale handle) reading as "not yet advertised" rather than
// panicking on the reconcile path.
func TestCapacityRecord_NilReceiver(t *testing.T) {
	var rec *capacityRecord

	_, ok := rec.last()

	assert.False(t, ok)
}
