package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shippedLadder is the ladder as Admit evaluates it. Every fixture below is a
// mutation of this or of docLadder, so a case that stops discriminating shows up
// as two fixtures that are equal.
const shippedLadder = `package provisioner

func (p *Provisioner) Admit(target Target) runnercore.AdmitFunc {
	return func(ctx context.Context) (release func(runnercore.AdmitOutcome), ok bool, reason string) {
		if quotaExhausted {
			return nil, false, runnercore.AdmitReasonQuota
		}
		if declined {
			return nil, false, runnercore.AdmitReasonCapacity
		}
		if !admitted {
			return nil, false, runnercore.AdmitReasonCeiling
		}
		if !allowed {
			return nil, false, runnercore.AdmitReasonScaleUp
		}
		return release, true, ""
	}
}
`

const docLadder = `# Flows

   **Admit (Q59, #784, Q405, Q406, Q717):** Before claiming the job, the gate asks four questions.
   **Quota:** can the namespace ResourceQuota admit one more worker pod?
   **Capacity:** can the cluster place one more worker pod of this shape?
   **Ceiling:** is the reservation counter below the worker ceiling?
   **Rate:** does the token bucket have a token for another pod creation?
   A no from any of them skips acquirejob.
`

func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestReadCodeOrder_ReadsEvaluationOrder pins the code side, including that the
// walk reaches into the closure Admit returns — every rung lives there, so a
// top-level-only walk would report zero and be caught by the vacuity guard
// rather than by this.
func TestReadCodeOrder_ReadsEvaluationOrder(t *testing.T) {
	got, err := readCodeOrder(write(t, "admission.go", shippedLadder))
	if err != nil {
		t.Fatalf("readCodeOrder: %v", err)
	}
	want := []string{"AdmitReasonQuota", "AdmitReasonCapacity", "AdmitReasonCeiling", "AdmitReasonScaleUp"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v; want %v", got, want)
	}
}

// TestReadCodeOrder_RefusesWhenAdmitIsAbsent is the vacuity guard: a renamed or
// moved Admit must be reported, never treated as a ladder that agrees. A check
// whose subject is missing has verified nothing, and the failure mode this
// prevents is a permanently green gate.
func TestReadCodeOrder_RefusesWhenAdmitIsAbsent(t *testing.T) {
	src := strings.Replace(shippedLadder, "func (p *Provisioner) Admit(", "func (p *Provisioner) Renamed(", 1)
	if src == shippedLadder {
		t.Fatal("fixture mutation did not apply; this case would pass vacuously")
	}
	_, err := readCodeOrder(write(t, "admission.go", src))
	if err == nil {
		t.Fatal("readCodeOrder accepted a file declaring no Admit; a missing subject must be a read failure")
	}
	if !strings.Contains(err.Error(), "no method Admit") {
		t.Fatalf("error does not name the missing subject: %v", err)
	}
}

// TestReadCodeOrder_RefusesAPartialLadder covers the other vacuity direction: a
// body holding one rung is a read that went wrong, not a one-rung ladder.
func TestReadCodeOrder_RefusesAPartialLadder(t *testing.T) {
	src := `package provisioner

func (p *Provisioner) Admit(target Target) runnercore.AdmitFunc {
	return func() { return nil, false, runnercore.AdmitReasonQuota }
}
`
	if _, err := readCodeOrder(write(t, "admission.go", src)); err == nil {
		t.Fatal("readCodeOrder accepted a single-rung ladder; expected a read failure")
	}
}

func TestReadDocOrder_ReadsListedOrder(t *testing.T) {
	got, err := readDocOrder(write(t, "flows.md", docLadder))
	if err != nil {
		t.Fatalf("readDocOrder: %v", err)
	}
	want := []string{"Quota", "Capacity", "Ceiling", "Rate"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v; want %v", got, want)
	}
}

// TestReadDocOrder_StopsAtTheEndOfTheBlock guards against the scan running on
// into later bold prose, which would report rungs that are not rungs. The
// paragraph after the ladder opens with bold text for exactly this reason.
func TestReadDocOrder_StopsAtTheEndOfTheBlock(t *testing.T) {
	doc := docLadder + "\n   **Why the rate limit is a rung rather than a wait (Q717).** Prose.\n"
	got, err := readDocOrder(write(t, "flows.md", doc))
	if err != nil {
		t.Fatalf("readDocOrder: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rungs (%v); the scan ran past the block", len(got), got)
	}
}

// TestReadDocOrder_RefusesWhenTheBlockIsAbsent is the doc-side vacuity guard.
func TestReadDocOrder_RefusesWhenTheBlockIsAbsent(t *testing.T) {
	doc := strings.Replace(docLadder, "**Admit (", "**Reworded (", 1)
	if doc == docLadder {
		t.Fatal("fixture mutation did not apply; this case would pass vacuously")
	}
	_, err := readDocOrder(write(t, "flows.md", doc))
	if err == nil {
		t.Fatal("readDocOrder accepted a file with no ladder block; a missing subject must be a read failure")
	}
	if !strings.Contains(err.Error(), "no ladder block") {
		t.Fatalf("error does not name the missing subject: %v", err)
	}
}

func TestCompare(t *testing.T) {
	shipped := []string{"AdmitReasonQuota", "AdmitReasonCapacity", "AdmitReasonCeiling", "AdmitReasonScaleUp"}

	t.Run("matching orders agree", func(t *testing.T) {
		if f := compare("a.go", "d.md", shipped, []string{"Quota", "Capacity", "Ceiling", "Rate"}); len(f) != 0 {
			t.Fatalf("findings on a matching pair: %v", f)
		}
	})

	// The regression this gate exists for: the order 04-operational-flows.md
	// carried from Q717 until Q972.
	t.Run("the Q972 drift is caught", func(t *testing.T) {
		f := compare("a.go", "d.md", shipped, []string{"Quota", "Capacity", "Rate", "Ceiling"})
		if len(f) != 1 {
			t.Fatalf("want 1 finding for the shipped drift, got %d: %v", len(f), f)
		}
		if !strings.Contains(f[0], "rung 3") {
			t.Fatalf("finding does not locate the drift: %v", f[0])
		}
	})

	t.Run("a rung the table cannot place is a finding", func(t *testing.T) {
		f := compare("a.go", "d.md", shipped, []string{"Quota", "Capacity", "Ceiling", "Throttle"})
		if len(f) == 0 {
			t.Fatal("an unpairable rung passed; the checker must refuse what it cannot place")
		}
		if !strings.Contains(f[0], "Throttle") {
			t.Fatalf("finding does not name the unpairable rung: %v", f[0])
		}
	})

	t.Run("a constant no rung documents is a finding", func(t *testing.T) {
		withNew := append(append([]string{}, shipped...), "AdmitReasonNewRung")
		f := compare("a.go", "d.md", withNew, []string{"Quota", "Capacity", "Ceiling", "Rate"})
		if len(f) == 0 {
			t.Fatal("an undocumented rung passed; a rung added to only one half is the drift this catches")
		}
		if !strings.Contains(f[0], "AdmitReasonNewRung") {
			t.Fatalf("finding does not name the undocumented constant: %v", f[0])
		}
	})

	t.Run("a dropped rung is a finding", func(t *testing.T) {
		if f := compare("a.go", "d.md", shipped, []string{"Quota", "Capacity", "Ceiling"}); len(f) == 0 {
			t.Fatal("a doc missing a rung passed")
		}
	})
}
