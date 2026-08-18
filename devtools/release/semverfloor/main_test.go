package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo builds a throwaway repository so the git-reading seam is exercised
// against real `git log` output rather than a hand-written fixture. The record
// framing is the part at risk: a body holding blank lines, or a subject holding
// a colon, would shift a field if the separators were newlines.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		//nolint:gosec // G204: this test's own git verbs against its own temp repo.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	// Q820: no detached maintenance racing the next command in a fixture repo.
	run("config", "maintenance.auto", "false")
	commit := func(path, subject, body string) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(subject), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", path)
		msg := subject
		if body != "" {
			msg += "\n\n" + body
		}
		run("commit", "-q", "-m", msg)
	}
	commit("README.md", "chore: seed", "")
	run("tag", "v0.0.0")

	commit("cmd/agc/internal/listener/job.go", "feat(agc): add a gauge", "A body.\n\nWith a blank line in it.")
	commit("scripts/agent/hook.sh", "feat(agent): add a hook", "")
	commit("api/v2beta1/types.go", "fix(api): correct a default", "BREAKING CHANGE: the default moved")
	commit("docs/NOTES.md", "Re-measure the split (Q1)", "")
	return dir
}

func TestReadCommitsAgainstRealGit(t *testing.T) {
	dir := gitRepo(t)
	commits, err := readCommits(dir, "v0.0.0..HEAD")
	if err != nil {
		t.Fatalf("readCommits: %v", err)
	}
	if len(commits) != 4 {
		t.Fatalf("read %d commits, want 4: %+v", len(commits), commits)
	}

	by := map[string]Commit{}
	for _, c := range commits {
		by[c.Subject] = c
	}

	feat, ok := by["feat(agc): add a gauge"]
	if !ok {
		t.Fatal("the shipping feat was not read back")
	}
	if feat.Type != "feat" || len(feat.Scopes) != 1 || feat.Scopes[0] != "agc" {
		t.Errorf("parsed %+v, want feat(agc)", feat)
	}
	if len(feat.Files) != 1 || feat.Files[0] != "cmd/agc/internal/listener/job.go" {
		t.Errorf("files = %v, want the one changed path", feat.Files)
	}

	// A footer-declared break must be read from the body, not the subject.
	brk := by["fix(api): correct a default"]
	if !brk.Breaking {
		t.Error("the BREAKING CHANGE footer was not picked up")
	}

	if by["Re-measure the split (Q1)"].Type != "" {
		t.Error("a non-conventional subject must not be assigned a type")
	}
}

// The end-to-end acceptance case, over real git history: a shipping feature
// raises the floor, an identically-typed tooling commit does not, and a
// breaking marker surfaces as a question.
func TestFloorEndToEnd(t *testing.T) {
	dir := gitRepo(t)
	commits, err := readCommits(dir, "v0.0.0..HEAD")
	if err != nil {
		t.Fatalf("readCommits: %v", err)
	}
	surface := Surface{
		PkgDirs: map[string]bool{
			"cmd/agc/internal/listener": true,
			"api/v2beta1":               true,
		},
	}
	r := Classify(commits, surface, nil)

	if r.Floor != LevelMinor {
		t.Errorf("floor = %v, want minor", r.Floor)
	}
	if len(r.Raising) != 2 {
		t.Errorf("raising = %d, want 2 (the feat and the fix)", len(r.Raising))
	}
	if len(r.Withheld) != 1 || r.Withheld[0].Commit.Subject != "feat(agent): add a hook" {
		t.Errorf("withheld = %+v, want just the tooling feat", r.Withheld)
	}
	if len(r.Unresolved) != 1 {
		t.Errorf("unresolved = %d, want the footer-declared break", len(r.Unresolved))
	}
	if len(r.NonConventional) != 1 {
		t.Errorf("nonConventional = %d, want 1", len(r.NonConventional))
	}
}

// TestShipsBytes pins the split between "on the released surface" and "can change
// a released artifact's bytes". The two are not the same question, and the gap is
// why -ships exists apart from the floor: Surface.Ships matches a Go package
// directory whole, so a _test.go beside shipped code answers yes.
//
// Both directions are asserted. A filter answering no to everything would let a
// real artifact change through the release freeze, and one answering yes to a test
// file would cost a release candidate for added coverage.
func TestShipsBytes(t *testing.T) {
	surface := Surface{
		PkgDirs: map[string]bool{"api/apiconditions": true},
		Trees:   []string{"charts/actions-gateway"},
	}

	for _, tc := range []struct {
		path string
		want bool
		why  string
	}{
		{"api/apiconditions/conditions.go", true, "shipped code"},
		{"api/apiconditions/conditions_test.go", false, "test file in a shipped package"},
		{"api/apiconditions/testdata/x.yaml", false, "testdata is not a package dir"},
		{"charts/actions-gateway/README.md", true, "markdown packaged into the chart tarball"},
		{"charts/actions-gateway/values_test.go", false, "test file inside a packaged tree"},
		{"docs/development/testing.md", false, "documentation off the surface"},
		{"internal/unshipped/thing.go", false, "package nothing released imports"},
	} {
		if got := shipsBytes(surface, tc.path); got != tc.want {
			t.Errorf("shipsBytes(%q) = %v, want %v (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}
