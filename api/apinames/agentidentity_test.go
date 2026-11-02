package apinames

import "testing"

// TestAgentStemNonInjective pins the collision Q1011 guards against: the "rs-"
// discriminator Q466 uses to keep a v1alpha1 RunnerGroup and a v2 RunnerSet of one
// name apart is a plain prefix, so a RunnerGroup named "rs-<x>" derives exactly the
// stem a RunnerSet named "<x>" derives. This test asserts the collision EXISTS — it
// is the premise the GMC admission guard is built on, and a change here that made the
// derivation injective would retire that guard rather than break it.
func TestAgentStemNonInjective(t *testing.T) {
	if got, want := RunnerGroupAgentStem("rs-build"), RunnerSetAgentStem("build"); got != want {
		t.Fatalf("RunnerGroup %q and RunnerSet %q must derive one stem; got %q and %q",
			"rs-build", "build", got, want)
	}
	if RunnerGroupAgentStem("build") == RunnerSetAgentStem("build") {
		t.Error("a RunnerGroup and a RunnerSet of the SAME name must derive distinct stems (Q466)")
	}
}

func TestAgentStems(t *testing.T) {
	if got, want := RunnerGroupAgentStem("build"), "build"; got != want {
		t.Errorf("RunnerGroupAgentStem = %q, want %q", got, want)
	}
	if got, want := RunnerSetAgentStem("build"), "rs-build"; got != want {
		t.Errorf("RunnerSetAgentStem = %q, want %q", got, want)
	}
	if got, want := RunnerSetAgentStem("build"), RunnerSetStemPrefix+"build"; got != want {
		t.Errorf("RunnerSetAgentStem must use RunnerSetStemPrefix: %q != %q", got, want)
	}
}
