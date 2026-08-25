package v2alpha1

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
		// WorkersUnschedulable, but unlike it does not roll up.
		//
		// The settling argument is ConditionWorkerCapacityDeclined one line above:
		// under reason PodsNotStarting that condition is this identical fact, and it
		// is not impairing. Rolling up the ungated twin while the gated one stays out
		// would make a set degraded at the gateway precisely BECAUSE its operator did
		// not opt into spec.capacityGate, and undegraded the moment they did.
		//
		// (The WorkerQuotaExceeded precedent is weaker than it looks and should not be
		// leaned on: read the impairing list and the organizing principle is closer to
		// fault-versus-saturation than to advisory-versus-deciding, which would put an
		// image that will not pull on the same side as RunnerVersionTooOld.)
		ConditionWorkersNotStarting,
	} {
		for _, imp := range ImpairingConditionTypes() {
			if imp == c {
				t.Errorf("advisory condition %q must not be impairing", c)
			}
		}
	}
}
