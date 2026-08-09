package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readme writes a fixture page with the given body under a synthetic scripts/
// root, and returns the README path and the root. Every fixture carries a table,
// since a page without one is the fail-closed case asserted separately.
func readme(t *testing.T, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	page := filepath.Join(root, "README.md")
	src := "# scripts/\n\n| Script | Purpose |\n|---|---|\n" + body + "\n"
	if err := os.WriteFile(page, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return page, root
}

// check runs the gate over one script path relative to the fixture root.
func check(t *testing.T, page, root, script string) (int, string, error) {
	t.Helper()
	var out strings.Builder
	n, err := run(page, []string{filepath.Join(root, filepath.FromSlash(script))}, &out, false)
	return n, out.String(), err
}

func TestDocumentedRoutes(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		script string
	}{
		// The entry-point shape: a row whose first cell links to the script.
		{"own row", "| [go-lint.sh](go/go-lint.sh) | Lint. |", "go/go-lint.sh"},
		// The *-test.sh convention: named in its subject's row as a code span,
		// with no row and no link of its own.
		{"code span in a sibling row", "| [go-lint.sh](go/go-lint.sh) | Scoping asserted by `go-lint-scope-test.sh` under `make scripts-test`. |", "go/go-lint-scope-test.sh"},
		// Bare prose, no code span and no link.
		{"plain prose", "| [go-lint.sh](go/go-lint.sh) | Assertions in go-lint-scope-test.sh under make scripts-test. |", "go/go-lint-scope-test.sh"},
		// A link whose text is not the filename still documents the target.
		{"link text differs from target", "| [the linter](go/go-lint.sh) | Lint. |", "go/go-lint.sh"},
		{"dot-slash destination", "| [go-lint.sh](./go/go-lint.sh) | Lint. |", "go/go-lint.sh"},
		{"destination with a fragment", "| [go-lint.sh](go/go-lint.sh#usage) | Lint. |", "go/go-lint.sh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, root := readme(t, tc.body)
			n, out, err := check(t, page, root, tc.script)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if n != 0 {
				t.Errorf("want documented, got %d finding(s):\n%s", n, out)
			}
		})
	}
}

func TestUndocumentedRoutes(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		script string
	}{
		{"absent entirely", "| [go-lint.sh](go/go-lint.sh) | Lint. |", "go/go-vet-tags.sh"},
		// The case that earns the parser. A filename inside a fenced example is
		// an illustration of how to invoke something, not a documented entry —
		// and it is indistinguishable from a real mention to a line-matching
		// search.
		{"only inside a fenced code block", "| [go-lint.sh](go/go-lint.sh) | Lint. |\n\n```\nscripts/go/go-vet-tags.sh --all\n```", "go/go-vet-tags.sh"},
		// The case that earns the boundary rule: a plain substring search finds
		// start.sh inside every mention of e2e-start.sh, so the one script that
		// most needs an entry reads as having one.
		{"only as a suffix of a longer name", "| [e2e-start.sh](dogfood/e2e-start.sh) | Spin up the e2e tenant. |", "dogfood/start.sh"},
		// The mirror of that: a longer name is not documented by its prefix.
		{"only as a prefix of a longer name", "| [start.sh](dogfood/start.sh) | Bring the cluster online. |", "dogfood/start-test.sh"},
		// A link to the directory is not a link to the script in it.
		{"only the directory is linked", "| [dogfood/](dogfood/) | Tenant tooling. |", "dogfood/start.sh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, root := readme(t, tc.body)
			n, out, err := check(t, page, root, tc.script)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if n != 1 {
				t.Fatalf("want 1 finding, got %d:\n%s", n, out)
			}
			if !strings.Contains(out, "no mention in") {
				t.Errorf("finding does not name the defect:\n%s", out)
			}
			if !strings.Contains(out, "`"+filepath.ToSlash(filepath.Dir(tc.script))+"/`") {
				t.Errorf("finding does not name the group table:\n%s", out)
			}
		})
	}
}

// A page with no table is a page this gate cannot judge. Reporting every script
// as undocumented would be a hundred findings pointing at the wrong defect, and
// silently passing would be worse.
func TestNoTablesIsAHardError(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "README.md")
	if err := os.WriteFile(page, []byte("# scripts/\n\nProse only.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := check(t, page, root, "go/go-lint.sh"); err == nil {
		t.Fatal("want an error, got none")
	}
}

func TestMissingReadmeIsAHardError(t *testing.T) {
	root := t.TempDir()
	if _, _, err := check(t, filepath.Join(root, "nope.md"), root, "go/go-lint.sh"); err == nil {
		t.Fatal("want an error, got none")
	}
}

// Findings are per script, so a page missing several reports each one.
func TestEveryUndocumentedScriptIsReported(t *testing.T) {
	page, root := readme(t, "| [go-lint.sh](go/go-lint.sh) | Lint. |")
	var out strings.Builder
	n, err := run(page, []string{
		filepath.Join(root, "go", "go-lint.sh"),
		filepath.Join(root, "go", "go-vet-tags.sh"),
		filepath.Join(root, "ci", "gate-list.sh"),
	}, &out, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 findings, got %d:\n%s", n, out.String())
	}
	for _, want := range []string{"go-vet-tags.sh", "gate-list.sh"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not name %s:\n%s", want, out.String())
		}
	}
}

func TestGitHubAnnotationFormat(t *testing.T) {
	page, root := readme(t, "| [go-lint.sh](go/go-lint.sh) | Lint. |")
	var out strings.Builder
	if _, err := run(page, []string{filepath.Join(root, "go", "go-vet-tags.sh")}, &out, true); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(out.String(), "::error file=") {
		t.Errorf("want a GitHub annotation, got:\n%s", out.String())
	}
}
