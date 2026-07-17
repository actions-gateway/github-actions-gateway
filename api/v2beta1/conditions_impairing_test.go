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

	// The advisory/transient conditions must never be treated as impairing.
	for _, c := range []string{
		ConditionRateLimited,
		ConditionWorkerQuotaPressure,
		ConditionWorkerQuotaExceeded,
		ConditionEgressUnattributed,
		ConditionPossibleReapBlockingSidecar,
	} {
		for _, imp := range ImpairingConditionTypes() {
			if imp == c {
				t.Errorf("advisory condition %q must not be impairing", c)
			}
		}
	}
}
