package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakePager answers OperatorPages from a table, so the partition can be tested
// without a repository behind it.
type fakePager struct {
	added  map[string][]string
	edited map[string][]string
}

func (f fakePager) OperatorPages(c Commit) (added, edited []string) {
	return f.added[c.SHA], f.edited[c.SHA]
}

// TestEnumerateNotesReproducesTheAllowListDefects is the acceptance case: the
// two commits the release.md scope allow-list got wrong at v1.4.0, in both
// directions, plus the headline feature that ships outside every image and
// chart. Paths must settle all three without any scope being consulted.
func TestEnumerateNotesReproducesTheAllowListDefects(t *testing.T) {
	s := testSurface()

	// Q166 shipped as a scopeless `feat:`, so `^(feat|fix)\(scope\)` never
	// matched it and the notes lost a headline feature.
	q166 := commit("aaaaaaaa1", "feat: enforce cross-namespace EgressProxy sharing (Q166)",
		"api/v2beta1/actionsgateway_types.go")
	// `feat(metrics)` was on the allow-list, but this one is the Claude usage
	// tooling: the scope string is shared, the released surface is not.
	usage := commit("bbbbbbbb2", "feat(metrics): refresh the usage snapshot",
		"claude-usage/snapshot.json")
	// Q554 is a headline feature that no image and no chart carries. Nothing
	// derived from publish.yml can admit it, so the residue has to surface it.
	q554 := commit("cccccccc3", "feat(deploy): ship a curated runner template library (Q554)",
		"deploy/templates/plain/template.yaml", "docs/operations/runner-template-library.md")

	r := Classify([]Commit{q166, usage, q554}, s, nil)
	n := EnumerateNotes(r, fakePager{
		added: map[string][]string{"cccccccc3": {"docs/operations/runner-template-library.md"}},
	})

	if len(n.Ships) != 1 || n.Ships[0].Commit.SHA != q166.SHA {
		t.Fatalf("Ships = %v, want only the scopeless Q166 feat", subjects(n.Ships))
	}
	if len(n.Residue) != 2 {
		t.Fatalf("Residue = %d commits, want 2", len(n.Residue))
	}
	// The residue is ordered so the one an operator receives is read first.
	if n.Residue[0].Commit.SHA != q554.SHA {
		t.Errorf("residue[0] = %q, want Q554 ranked first by its added operator page",
			n.Residue[0].Commit.Subject)
	}
	if !n.Residue[0].Operator() {
		t.Error("Q554 is not flagged operator-facing, so a reader scanning the flags would miss it")
	}
	if n.Residue[1].Operator() {
		t.Errorf("%q is flagged operator-facing; a flag that fires on tooling is no flag at all",
			n.Residue[1].Commit.Subject)
	}
}

// TestEnumerateNotesReconciles holds the property the whole reconciliation rests
// on: every feat/fix/perf commit lands in exactly one of the two lists. A commit
// that leaves Ships without arriving in Residue is the failure the allow-list
// used to produce silently.
func TestEnumerateNotesReconciles(t *testing.T) {
	s := testSurface()
	commits := []Commit{
		commit("1111111a1", "feat(agc): a shipping feature", "cmd/agc/internal/listener/job.go"),
		commit("2222222b2", "fix(agc): a shipping fix", "cmd/agc/internal/listener/job.go"),
		commit("3333333c3", "perf(agc): a shipping perf change", "cmd/agc/internal/listener/job.go"),
		commit("4444444d4", "feat(ci): tooling", "scripts/ci/gate.sh"),
		commit("5555555e5", "fix(docs): a doc fix", "docs/STATUS.md"),
		// Neither of these carries a level, so neither belongs in either list.
		commit("6666666f6", "refactor(agc): moves shipped code", "cmd/agc/internal/listener/job.go"),
		commit("7777777g7", "chore: bump a pin", "scripts/ci/gate.sh"),
		{SHA: "8888888h8", Subject: "Not a conventional subject", Files: []string{"docs/STATUS.md"}},
	}
	r := Classify(commits, s, nil)
	n := EnumerateNotes(r, nil)

	if got := len(n.Ships) + len(n.Residue); got != n.Total {
		t.Errorf("Ships+Residue = %d, Total = %d: a commit is being double-counted or dropped", got, n.Total)
	}
	if n.Total != 5 {
		t.Errorf("Total = %d, want the 5 feat/fix/perf commits; refactor, chore and an unreadable "+
			"subject carry no level and must not be enumerated", n.Total)
	}
	if len(n.Ships) != 3 {
		t.Errorf("Ships = %v, want the three commits touching cmd/agc/internal/listener", subjects(n.Ships))
	}

	seen := map[string]int{}
	for _, v := range n.Ships {
		seen[v.Commit.SHA]++
	}
	for _, item := range n.Residue {
		seen[item.Commit.SHA]++
	}
	for sha, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times across the two lists, want exactly 1", sha, count)
		}
	}
}

// TestEnumerateNotesKeepsCommentOnlyVisible checks that a commit narrowed out of
// the floor is still shown. It ships a byte-identical artifact so it belongs in
// no change list, but a commit that vanishes from both lists cannot be checked.
func TestEnumerateNotesKeepsCommentOnlyVisible(t *testing.T) {
	c := commit("9999999i9", "feat(probe): a comment-only change", "cmd/agc/internal/listener/job.go")
	r := Classify([]Commit{c}, testSurface(), nothingSubstantive{})
	n := EnumerateNotes(r, nil)

	if len(n.Ships) != 0 {
		t.Errorf("Ships = %v, want nothing: the artifact is byte-identical", subjects(n.Ships))
	}
	if len(n.Residue) != 1 || !n.Residue[0].CommentOnly {
		t.Fatalf("Residue = %+v, want the commit carried with its comment-only reason", n.Residue)
	}
	var b strings.Builder
	reportNotes(&b, "v1.0.0", "HEAD", []Commit{c}, testSurface(), Sources{}, n, "")
	if !strings.Contains(b.String(), "comment-only") {
		t.Error("the report does not say why the commit is in the residue")
	}
}

// nothingSubstantive narrows every shipped file away, which is what a
// comments-and-whitespace diff produces.
type nothingSubstantive struct{}

func (nothingSubstantive) Substantive(Commit, []string) []string { return nil }

func TestStripPrefix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"feat(agc): add a gauge", "add a gauge"},
		{"feat: enforce sharing (Q166)", "enforce sharing (Q166)"},
		{"fix(agc,gmc)!: reshape a field", "reshape a field"},
		// A subject with no readable prefix is left exactly as it stands.
		{"Re-measure the split (Q657)", "Re-measure the split (Q657)"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := stripPrefix(tc.in); got != tc.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCheckOperatorDocs is the planted failure: the flag reads one hard-coded
// tree, and a tree that moved would silently stop flagging anything. A window
// with nothing to flag and a program that can no longer flag must not look the
// same.
func TestCheckOperatorDocs(t *testing.T) {
	dir := t.TempDir()
	if got := checkOperatorDocs(dir); got == "" {
		t.Error("a tree with no docs/operations/ reported no problem")
	}
	if err := os.MkdirAll(filepath.Join(dir, operatorDocs), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := checkOperatorDocs(dir); got != "" {
		t.Errorf("a tree with docs/operations/ reported %q", got)
	}

	// A file where the directory belongs is the case a bare existence check
	// would pass.
	file := t.TempDir()
	if err := os.MkdirAll(filepath.Join(file, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, operatorDocs), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkOperatorDocs(file); got == "" {
		t.Error("docs/operations as a file reported no problem")
	}
}

// TestGitPagerAgainstRealGit exercises the added/changed split against real
// `git show --name-status` output. The rename is the shape that would break a
// two-field parse: it reports three fields, and the destination is the one that
// exists at the commit.
func TestGitPagerAgainstRealGit(t *testing.T) {
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
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	head := func() string {
		t.Helper()
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-q", "-b", "main")
	write("docs/operations/upgrade.md", "one\n")
	write("scripts/ci/gate.sh", "#!/bin/sh\n")
	run("add", "-A")
	run("commit", "-q", "-m", "chore: seed")

	write("docs/operations/runner-template-library.md", "new page\n")
	write("docs/operations/upgrade.md", "one\ntwo\n")
	run("add", "-A")
	run("commit", "-q", "-m", "feat(deploy): ship a template library")
	added := head()

	run("mv", "docs/operations/upgrade.md", "docs/operations/upgrading.md")
	run("commit", "-q", "-m", "docs(operations): rename")
	renamed := head()

	write("scripts/ci/gate.sh", "#!/bin/sh\ntrue\n")
	run("add", "-A")
	run("commit", "-q", "-m", "fix(ci): tooling only")
	tooling := head()

	p := gitPager{root: dir}

	a, e := p.OperatorPages(Commit{SHA: added, Files: []string{
		"docs/operations/runner-template-library.md", "docs/operations/upgrade.md"}})
	if len(a) != 1 || a[0] != "docs/operations/runner-template-library.md" {
		t.Errorf("added = %v, want the new page alone", a)
	}
	if len(e) != 1 || e[0] != "docs/operations/upgrade.md" {
		t.Errorf("edited = %v, want the modified page alone", e)
	}

	a, e = p.OperatorPages(Commit{SHA: renamed, Files: []string{"docs/operations/upgrading.md"}})
	if len(a)+len(e) != 1 {
		t.Fatalf("a rename yielded added=%v edited=%v, want exactly one page", a, e)
	}
	if got := append(a, e...)[0]; got != "docs/operations/upgrading.md" {
		t.Errorf("a rename reported %q, want the destination path", got)
	}

	// The paths already exclude this one, so no git call should be needed and
	// nothing may come back.
	if a, e := p.OperatorPages(Commit{SHA: tooling, Files: []string{"scripts/ci/gate.sh"}}); len(a)+len(e) != 0 {
		t.Errorf("a tooling commit reported operator pages added=%v edited=%v", a, e)
	}
}

func subjects(vs []Verdict) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Commit.Subject)
	}
	return out
}
