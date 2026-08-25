package v2beta1

import "testing"

// TestImpairingConditionTypes pins the abnormal-is-True conditions that roll a
// RunnerSet up as impaired for the GMC's RunnerSetsDegraded rollup (Q330): the set
// must contain exactly the four "the set cannot serve jobs" conditions and must
// exclude every advisory/transient signal, so the rollup does not flap on normal load.
func TestImpairingConditionTypes(t *testing.T) {
	got := ImpairingConditionTypes()

	want := map[string]bool{
		ConditionDegraded:              true,
		ConditionCredentialUnavailable: true,
		ConditionRunnerVersionTooOld:   true,
		ConditionWorkersUnschedulable:  true,
	}
	if len(got) != len(want) {
		t.Fatalf("ImpairingConditionTypes() = %v; want the %d impairing conditions", got, len(want))
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected impairing condition %q", c)
		}
		delete(want, c)
	}
	for c := range want {
		t.Errorf("missing impairing condition %q", c)
	}

	// The advisory/transient conditions must never be treated as impairing. The last
	// entry is the load-bearing one for Q405: WorkerCapacityDeclined derives from the
	// same fact as WorkersUnschedulable, which IS impairing, so rolling both up would
	// double-count one stall into the gateway summary (Q304).
	for _, c := range []string{
		ConditionRateLimited,
		ConditionWorkerQuotaPressure,
		ConditionWorkerQuotaExceeded,
		ConditionEgressUnattributed,
		ConditionPossibleReapBlockingSidecar,
		ConditionWorkerCapacityDeclined,
		// Q906: the kubelet's startup verdict reports without deciding, like
		// WorkersUnschedulable, but unlike it does not roll up. WorkerQuotaExceeded
		// above is the precedent — a harder stall, also advisory-only — so
		// WorkersUnschedulable (Q157) is the exception in this family rather than the
		// rule, and adding a second rollup input would change every gateway's
		// RunnerSetsDegraded for a signal an operator can alert on directly.
		ConditionWorkersNotStarting,
	} {
		for _, imp := range ImpairingConditionTypes() {
			if imp == c {
				t.Errorf("advisory condition %q must not be impairing", c)
			}
		}
	}
}
