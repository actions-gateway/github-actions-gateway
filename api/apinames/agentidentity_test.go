package apinames

import (
	"strconv"
	"testing"
)

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

// TestRunnerGroupName pins the derivation against the expression the GMC controller
// and gag-migrate each held their own copy of before this helper existed, so
// collapsing those copies cannot have moved any existing tenant's name.
func TestRunnerGroupName(t *testing.T) {
	// former returns what both copies computed inline.
	former := func(gateway string, labels []string, i int) string {
		if len(labels) > 0 {
			return Join(MaxLabelValue, gateway, Segment(labels[0], "label"))
		}
		return Join(MaxLabelValue, gateway, strconv.Itoa(i))
	}

	cases := []struct {
		name    string
		gateway string
		labels  []string
		index   int
	}{
		{"first label wins", "gw", []string{"linux", "x64"}, 0},
		{"unlabeled falls back to the index", "gw", nil, 3},
		{"empty slice is unlabeled", "gw", []string{}, 7},
		{"an empty first label is still a label", "gw", []string{""}, 2},
		{"label needing sanitisation", "gw", []string{"self hosted/linux"}, 0},
		{"long gateway and long label are budgeted", "a-very-long-gateway-name-here", []string{
			"an-extremely-long-runner-label-that-will-not-fit-in-the-budget"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RunnerGroupName(tc.gateway, tc.labels, tc.index)
			if want := former(tc.gateway, tc.labels, tc.index); got != want {
				t.Errorf("RunnerGroupName = %q, want %q (the pre-collapse derivation)", got, want)
			}
			if len(got) > MaxLabelValue {
				t.Errorf("RunnerGroupName = %q: %d chars exceeds the %d-char label-value budget",
					got, len(got), MaxLabelValue)
			}
		})
	}
}
