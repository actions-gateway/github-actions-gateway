package main

import "testing"

// testSurface stands in for the derived release surface: two shipped package
// directories and one chart tree.
func testSurface() Surface {
	return Surface{
		PkgDirs: map[string]bool{
			"cmd/agc/internal/listener": true,
			"api/v2beta1":               true,
		},
		Trees: []string{"charts/actions-gateway"},
	}
}

func TestSurfaceShips(t *testing.T) {
	s := testSurface()
	tests := []struct {
		file string
		want bool
	}{
		{"cmd/agc/internal/listener/job.go", true},
		{"api/v2beta1/runnerset_types.go", true},
		{"charts/actions-gateway/values.yaml", true},
		{"charts/actions-gateway/templates/crds/x.yaml", true},

		// A sibling directory nothing imports is not on the surface, which is
		// the point of deriving from `go list -deps` rather than from a prefix.
		{"cmd/agc/internal/notimported/x.go", false},
		{"cmd/probe/main.go", false},
		{"devtools/docs/emdash/main.go", false},
		{"docs/STATUS.md", false},
		{"scripts/agent/hook.sh", false},

		// A path that merely starts with a tree's name is not inside it.
		{"charts/actions-gateway-crds-v2/Chart.yaml", false},
	}
	for _, tc := range tests {
		if got := s.Ships(tc.file); got != tc.want {
			t.Errorf("Ships(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

func commit(sha, subject string, files ...string) Commit {
	c := Commit{SHA: sha, Subject: subject, Files: files}
	c.Type, c.Scopes, c.Breaking, _ = parseSubject(subject)
	return c
}

// TestClassifyInjectedDefects is the acceptance case: a shipping feature must
// raise the floor, an identically-typed tooling commit must not, and a breaking
// marker must surface as an unresolved major rather than silently reading as a
// minor.
func TestClassifyInjectedDefects(t *testing.T) {
	s := testSurface()

	t.Run("shipping feat raises the floor", func(t *testing.T) {
		r := Classify([]Commit{
			commit("aaaaaaaa", "feat(agc): add a gauge", "cmd/agc/internal/listener/job.go"),
		}, s)
		if r.Floor != LevelMinor {
			t.Fatalf("floor = %v, want minor", r.Floor)
		}
		if len(r.Raising) != 1 {
			t.Fatalf("raising = %d, want 1", len(r.Raising))
		}
		if len(r.Raising[0].Shipped) != 1 {
			t.Errorf("evidence = %v, want the shipped file named", r.Raising[0].Shipped)
		}
	})

	t.Run("tooling feat does not", func(t *testing.T) {
		r := Classify([]Commit{
			commit("bbbbbbbb", "feat(agent): add a hook", "scripts/agent/hook.sh"),
		}, s)
		if r.Floor != LevelNone {
			t.Fatalf("floor = %v, want none", r.Floor)
		}
		if len(r.Withheld) != 1 {
			t.Fatalf("withheld = %d, want 1 — a dropped commit must stay visible", len(r.Withheld))
		}
	})

	t.Run("shipping breaking marker is unresolved, not major", func(t *testing.T) {
		r := Classify([]Commit{
			commit("cccccccc", "feat(agc)!: drop a field", "cmd/agc/internal/listener/job.go"),
		}, s)
		if len(r.Unresolved) != 1 {
			t.Fatalf("unresolved = %d, want 1", len(r.Unresolved))
		}
		// The floor still carries the feat's own weight; the marker adds a
		// question on top of it rather than replacing it.
		if r.Floor != LevelMinor {
			t.Errorf("floor = %v, want minor", r.Floor)
		}
	})

	t.Run("a breaking refactor carries no floor but is still asked about", func(t *testing.T) {
		r := Classify([]Commit{
			commit("dddddddd", "refactor(api)!: split the axes", "api/v2beta1/runnerset_types.go"),
		}, s)
		if r.Floor != LevelNone {
			t.Errorf("floor = %v, want none — a refactor is defined as unobservable", r.Floor)
		}
		if len(r.Unresolved) != 1 {
			t.Fatalf("unresolved = %d, want 1 — a refactor! must not slip past", len(r.Unresolved))
		}
	})

	t.Run("a breaking marker that ships nothing raises nothing", func(t *testing.T) {
		r := Classify([]Commit{
			commit("eeeeeeee", "feat(agent)!: change a hook contract", "scripts/agent/hook.sh"),
		}, s)
		if len(r.Unresolved) != 0 {
			t.Errorf("unresolved = %d, want 0", len(r.Unresolved))
		}
	})
}

func TestClassifyFloorIsTheMaximum(t *testing.T) {
	s := testSurface()
	r := Classify([]Commit{
		commit("11111111", "fix(agc): bound the retry", "cmd/agc/internal/listener/job.go"),
		commit("22222222", "feat(agc): add a gauge", "api/v2beta1/runnerset_types.go"),
		commit("33333333", "docs: tidy", "docs/STATUS.md"),
		commit("44444444", "chore(agc): bump", "cmd/agc/internal/listener/job.go"),
	}, s)
	if r.Floor != LevelMinor {
		t.Errorf("floor = %v, want minor", r.Floor)
	}
	if len(r.Raising) != 2 {
		t.Errorf("raising = %d, want 2 (the docs and chore commits carry no weight)", len(r.Raising))
	}
	if r.Raising[0].Level != LevelMinor {
		t.Errorf("raising is not floor-first: %v", r.Raising[0].Level)
	}
}

func TestClassifyPatchOnly(t *testing.T) {
	r := Classify([]Commit{
		commit("11111111", "fix(agc): bound the retry", "cmd/agc/internal/listener/job.go"),
		commit("22222222", "perf(agc): fewer allocations", "cmd/agc/internal/listener/job.go"),
	}, testSurface())
	if r.Floor != LevelPatch {
		t.Errorf("floor = %v, want patch", r.Floor)
	}
}

func TestClassifyRecordsUnreadableSubjects(t *testing.T) {
	r := Classify([]Commit{
		commit("11111111", "Re-measure the eviction split", "cmd/agc/internal/listener/job.go"),
	}, testSurface())
	if len(r.NonConventional) != 1 {
		t.Fatalf("nonConventional = %d, want 1", len(r.NonConventional))
	}
	if r.Floor != LevelNone {
		t.Errorf("floor = %v, want none — an unreadable subject is not assessed", r.Floor)
	}
}

func TestDivergentScopes(t *testing.T) {
	s := testSurface()
	// `metrics` is a scope carried by a shipping commit AND by a tooling one:
	// the v1.3.0 trap, where a `feat(metrics)` touching only the usage tooling
	// was listed as a product feature.
	r := Classify([]Commit{
		commit("11111111", "feat(metrics): add a product gauge", "cmd/agc/internal/listener/job.go"),
		commit("22222222", "feat(metrics): refresh the usage snapshot", "claude-usage/main.go"),
		commit("33333333", "feat(agent): a hook", "scripts/agent/hook.sh"),
	}, s)
	div := DivergentScopes(r, ShippingScopes(r))
	if len(div) != 1 {
		t.Fatalf("divergent = %d, want 1", len(div))
	}
	if div[0].Commit.SHA != "22222222" {
		t.Errorf("divergent commit = %s, want the tooling one", div[0].Commit.SHA)
	}
}
