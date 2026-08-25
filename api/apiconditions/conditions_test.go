package apiconditions_test

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
)

// TestImpairingConditionTypesWireValues pins the impairing set by its literal wire
// values, not by the constants. The per-version conditions_impairing_test.go files
// compare against the re-exported constants, so they would pass even if a constant's
// value were changed on both sides; these strings are what lands in a
// .status.conditions[].type and what the GMC's RunnerSetsDegraded rollup (Q304, Q330)
// matches on, so renaming one is an API-visible change that must fail here first.
func TestImpairingConditionTypesWireValues(t *testing.T) {
	got := apiconditions.ImpairingConditionTypes()

	want := []string{
		"Degraded",
		"CredentialUnavailable",
		"RunnerVersionTooOld",
		"WorkersUnschedulable",
	}
	if len(got) != len(want) {
		t.Fatalf("ImpairingConditionTypes() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ImpairingConditionTypes() = %v; want %v", got, want)
		}
	}
}

// TestImpairingConditionTypesIsACopy guards the exported slice against mutation by a
// caller: the rollup iterates it per reconcile, so a caller that sorted or appended
// to a shared backing array would corrupt every later reconcile's view.
func TestImpairingConditionTypesIsACopy(t *testing.T) {
	first := apiconditions.ImpairingConditionTypes()
	first[0] = "mutated"

	if second := apiconditions.ImpairingConditionTypes(); second[0] == "mutated" {
		t.Fatal("ImpairingConditionTypes() returns a shared slice; a caller's mutation leaked into the next call")
	}
}

// TestVocabularyIsDistinct catches a copy-paste bind of two names to one value — the
// failure the re-export files make possible (v2alpha1.ConditionDegraded pointing at
// apiconditions.ConditionReady would compile fine). Two condition types sharing a
// value would collapse into one entry of the listType=map conditions slice, silently
// dropping a signal.
func TestVocabularyIsDistinct(t *testing.T) {
	types := map[string]string{
		"ConditionReady":                       apiconditions.ConditionReady,
		"ConditionAGCAvailable":                apiconditions.ConditionAGCAvailable,
		"ConditionCredentialUnavailable":       apiconditions.ConditionCredentialUnavailable,
		"ConditionDegraded":                    apiconditions.ConditionDegraded,
		"ConditionEgressUnattributed":          apiconditions.ConditionEgressUnattributed,
		"ConditionPossibleReapBlockingSidecar": apiconditions.ConditionPossibleReapBlockingSidecar,
		"ConditionWorkerQuotaPressure":         apiconditions.ConditionWorkerQuotaPressure,
		"ConditionWorkerQuotaExceeded":         apiconditions.ConditionWorkerQuotaExceeded,
		"ConditionWorkersUnschedulable":        apiconditions.ConditionWorkersUnschedulable,
		"ConditionWorkersNotStarting":          apiconditions.ConditionWorkersNotStarting,
		"ConditionWorkerCapacityDeclined":      apiconditions.ConditionWorkerCapacityDeclined,
		"ConditionRunnerSetsDegraded":          apiconditions.ConditionRunnerSetsDegraded,
		"ConditionProxyQuotaPressure":          apiconditions.ConditionProxyQuotaPressure,
		"ConditionProxyQuotaExceeded":          apiconditions.ConditionProxyQuotaExceeded,
		"ConditionEgressRulesStale":            apiconditions.ConditionEgressRulesStale,
		"ConditionGitHubEgressIncomplete":      apiconditions.ConditionGitHubEgressIncomplete,
		"ConditionRateLimited":                 apiconditions.ConditionRateLimited,
		"ConditionRunnerVersionTooOld":         apiconditions.ConditionRunnerVersionTooOld,
		"ConditionSizingDrift":                 apiconditions.ConditionSizingDrift,
		"ConditionJobProvisionStalled":         apiconditions.ConditionJobProvisionStalled,
		"ConditionScaleSetNameCollision":       apiconditions.ConditionScaleSetNameCollision,
	}

	seen := make(map[string]string, len(types))
	for name, value := range types {
		if value == "" {
			t.Errorf("%s has an empty value", name)
			continue
		}
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s both resolve to %q; condition types are the listType=map key and must be distinct", name, other, value)
			continue
		}
		seen[value] = name
	}
}
