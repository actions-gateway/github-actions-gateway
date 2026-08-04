package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pinnedSHA = "3d3c42e5aac5ba805825da76410c181273ba90b1"

// write puts a workflow in a temp dir and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// steps wraps step YAML in the minimum workflow around it.
func steps(body string) string {
	return "name: t\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n" + body
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name        string
		uses        string
		wantMsg     string // substring; empty means the reference is acceptable
		wantVersion bool
	}{
		{"sha pin", "actions/checkout@" + pinnedSHA, "", true},
		{"sha pin in a subdirectory", "anchore/sbom-action/download-syft@" + pinnedSHA, "", true},
		{"remote reusable workflow", "o/r/.github/workflows/x.yml@" + pinnedSHA, "", true},
		{"semver tag", "actions/checkout@v7.0.1", "tag or branch", false},
		{"major tag", "actions/checkout@v4", "tag or branch", false},
		{"branch", "actions/checkout@main", "tag or branch", false},
		{"short sha", "actions/checkout@3d3c42e", "tag or branch", false},
		{"uppercase sha", "actions/checkout@" + strings.ToUpper(pinnedSHA), "tag or branch", false},
		{"no ref", "actions/checkout", "no ref", false},
		{"local action", "./.github/actions/setup", "", false},
		{"local reusable workflow", "./.github/workflows/e2e-reusable.yml", "", false},
		{"local with a ref", "./.github/actions/setup@v1", "must not carry a ref", false},
		{"docker digest", "docker://alpine@sha256:" + strings.Repeat("a", 64), "", false},
		{"docker tag", "docker://alpine:3.20", "not digest-pinned", false},
		{"docker latest", "docker://alpine", "not digest-pinned", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, wantVersion := classify(tc.uses)
			if tc.wantMsg == "" && msg != "" {
				t.Fatalf("classify(%q) rejected a legitimate reference: %s", tc.uses, msg)
			}
			if tc.wantMsg != "" && !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("classify(%q) = %q, want it to mention %q", tc.uses, msg, tc.wantMsg)
			}
			if wantVersion != tc.wantVersion {
				t.Errorf("classify(%q) version-comment requirement = %v, want %v", tc.uses, wantVersion, tc.wantVersion)
			}
		})
	}
}

func TestVersionCommentRequired(t *testing.T) {
	tests := []struct {
		name    string
		comment string
		wantOK  bool
	}{
		{"dependabot form", " # v7.0.1", true},
		{"no leading v", " # 4.1.1", true},
		{"tag= form", " # tag=v1.2.3", true},
		{"major only", " # v4", false},
		{"not a version", " # pinned", false},
		{"absent", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, steps("      - uses: actions/checkout@"+pinnedSHA+tc.comment+"\n"))
			n, found, err := checkFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("checked %d references, want 1", n)
			}
			if tc.wantOK && len(found) != 0 {
				t.Fatalf("comment %q rejected: %s", tc.comment, found[0].msg)
			}
			if !tc.wantOK && len(found) != 1 {
				t.Fatalf("comment %q accepted, want a finding", tc.comment)
			}
		})
	}
}

// The word `uses:` appears in this repo's workflows inside comments and could
// appear inside a run: block. Both defeat a regex-based gate; neither is a
// mapping key, so the parser must not see them.
func TestNonKeyOccurrencesAreNotReferences(t *testing.T) {
	body := steps("" +
		"      # a comment about SHA-pinned `uses:` refs, as unit-test.yml has\n" +
		"      - uses: actions/checkout@" + pinnedSHA + " # v7.0.1\n" +
		"      - name: prose\n" +
		"        run: |\n" +
		"          echo \"uses: actions/checkout@v4\"\n" +
		"          grep -n 'uses:' .github/workflows/*.yml\n")
	n, found, err := checkFile(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("checked %d references, want 1 — only the real `uses:` key counts", n)
	}
	if len(found) != 0 {
		t.Fatalf("unexpected finding: %+v", found[0])
	}
}

// A `uses:` on a job rather than a step is a reusable-workflow call and is just
// as exploitable, so the walk must reach it.
func TestJobLevelUsesIsChecked(t *testing.T) {
	body := "name: t\non: push\njobs:\n" +
		"  local:\n    uses: ./.github/workflows/e2e-reusable.yml\n" +
		"  remote:\n    uses: o/r/.github/workflows/x.yml@v1\n"
	n, found, err := checkFile(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("checked %d references, want 2", n)
	}
	if len(found) != 1 || !strings.Contains(found[0].msg, "tag or branch") {
		t.Fatalf("want exactly the remote tag flagged, got %+v", found)
	}
}

func TestCompositeActionStepsAreChecked(t *testing.T) {
	body := "name: a\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v4\n"
	n, found, err := checkFile(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(found) != 1 {
		t.Fatalf("checked %d references with %d findings, want 1 and 1", n, len(found))
	}
}

// A step reached through a YAML alias is still a step. GitHub's own parser does
// not take anchors, so this cannot arrive from a workflow it runs — but a walk
// that returns nil on a node shape it did not expect fails open, which is the
// one way this gate could report green over an unpinned ref.
func TestAliasedStepIsFollowed(t *testing.T) {
	body := "name: t\non: push\n" +
		"x-anchor: &s\n  uses: actions/checkout@v4\n" +
		"jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - *s\n"
	n, found, err := checkFile(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("checked %d references, want 1 — the alias was not followed", n)
	}
	if len(found) != 1 {
		t.Fatalf("want the aliased tag flagged, got %d findings", len(found))
	}
}

// Fail closed: an unparseable workflow is an error, never a clean result.
func TestUnparseableFileIsAnError(t *testing.T) {
	if _, _, err := checkFile(write(t, "jobs:\n  j:\n   - [unbalanced\n")); err == nil {
		t.Fatal("unparseable YAML returned no error, so the gate would skip the file")
	}
	if _, _, err := checkFile(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("missing file returned no error")
	}
}

// A workflow with no steps is legitimately empty; the -min floor, not this, is
// what catches an extractor that stopped matching.
func TestFileWithNoUsesIsClean(t *testing.T) {
	n, found, err := checkFile(write(t, steps("      - run: make check\n")))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(found) != 0 {
		t.Fatalf("checked %d references with %d findings, want 0 and 0", n, len(found))
	}
}

// The findings carry the source line, which is the only thing that makes the
// output actionable in a 24-workflow tree.
func TestFindingReportsSourceLine(t *testing.T) {
	body := steps("" +
		"      - uses: actions/checkout@" + pinnedSHA + " # v7.0.1\n" +
		"      - uses: actions/setup-go@v7.0.0\n")
	_, found, err := checkFile(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("want 1 finding, got %d", len(found))
	}
	if found[0].line != 8 {
		t.Errorf("finding line = %d, want 8", found[0].line)
	}
}
