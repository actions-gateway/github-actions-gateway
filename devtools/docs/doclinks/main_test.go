package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes files into a throwaway root and returns the arguments run
// needs: the root, the existence-oracle file, and the Markdown files to scan.
// The oracle lists every file written, which is what the caller derives from
// git.
func fixture(t *testing.T, files map[string]string) (root, existFile string, mdFiles []string) {
	t.Helper()
	root = t.TempDir()
	var paths []string
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
		if strings.HasSuffix(name, ".md") {
			mdFiles = append(mdFiles, name)
		}
	}
	existFile = filepath.Join(t.TempDir(), "exist")
	if err := os.WriteFile(existFile, []byte(strings.Join(paths, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, existFile, mdFiles
}

func check(t *testing.T, files map[string]string) (broken int, output string) {
	t.Helper()
	root, existFile, mdFiles := fixture(t, files)
	var out bytes.Buffer
	broken, err := run(root, existFile, mdFiles, &out, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return broken, out.String()
}

func TestBrokenLinks(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string // the finding, or "" for a clean run
	}{{
		// Q612, end to end: the shape that made the gate report green.
		name:  "badge-wrapped link at a missing target",
		files: map[string]string{"README.md": "[![License](https://img/b.svg)](LICENSE)\n"},
		want:  "README.md:1: dead link: LICENSE -> LICENSE",
	}, {
		name: "badge-wrapped link at a present target",
		files: map[string]string{
			"README.md": "[![License](https://img/b.svg)](LICENSE)\n",
			"LICENSE":   "Apache 2.0\n",
		},
	}, {
		name:  "plain link at a missing target",
		files: map[string]string{"README.md": "[license](LICENSE)\n"},
		want:  "README.md:1: dead link: LICENSE -> LICENSE",
	}, {
		name: "relative link out of a subdirectory",
		files: map[string]string{
			"docs/a.md": "[b](../b.md)\n",
			"b.md":      "# B\n",
		},
	}, {
		name:  "link that unwinds to nothing is outside the repo",
		files: map[string]string{"docs/a.md": "[out](../..)\n"},
		want:  "docs/a.md:1: dead link: ../.. -> (outside repo)",
	}, {
		name: "root-absolute link",
		files: map[string]string{
			"docs/a.md": "[b](/docs/b.md)\n",
			"docs/b.md": "# B\n",
		},
	}, {
		name: "directory link resolves through an ancestor",
		files: map[string]string{
			"README.md":        "[the docs](docs/design)\n",
			"docs/design/a.md": "# A\n",
		},
	}, {
		name: "trailing line reference is tolerated",
		files: map[string]string{
			"docs/a.md":    "[the call](../pkg/run.go:42)\n",
			"pkg/run.go":   "package pkg\n",
			"docs/keep.md": "# keep\n",
		},
	}, {
		name:  "external URLs are out of scope",
		files: map[string]string{"a.md": "[x](https://example.com/nope) [y](mailto:a@b.c) [z](tel:+1)\n"},
	}, {
		name: "link inside an admonition body",
		files: map[string]string{
			"a.md": "!!! info \"T\"\n\n    See [appendix](docs/appendix.md).\n",
		},
		want: "a.md:3: dead link: docs/appendix.md -> docs/appendix.md",
	}, {
		name: "link whose text spans a line break",
		files: map[string]string{
			"a.md": "See [the capacity\nappendix](docs/appendix.md).\n",
		},
		want: "a.md:1: dead link: docs/appendix.md -> docs/appendix.md",
	}, {
		name:  "bracket syntax inside inline code is not a link",
		files: map[string]string{"a.md": "Write `[text](missing.md)` to link.\n"},
	}, {
		name:  "bracket syntax inside a fence is not a link",
		files: map[string]string{"a.md": "```\n[text](missing.md)\n```\n"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broken, out := check(t, tc.files)
			if tc.want == "" {
				if broken != 0 {
					t.Errorf("want clean, got %d finding(s):\n%s", broken, out)
				}
				return
			}
			if broken != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("want 1 finding containing %q, got %d:\n%s", tc.want, broken, out)
			}
		})
	}
}

func TestBrokenAnchors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{{
		name:  "same-page anchor with no heading",
		files: map[string]string{"a.md": "# Overview\n\n[jump](#missing)\n"},
		want:  "a.md:3: dead anchor: #missing -> #missing has no matching heading or <a id> in a.md",
	}, {
		name:  "same-page anchor matching a heading slug",
		files: map[string]string{"a.md": "# The `check` gate\n\n[jump](#the-check-gate)\n"},
	}, {
		name:  "same-page anchor matching an explicit HTML anchor",
		files: map[string]string{"a.md": "<a id=\"Q612\"></a>Q612\n\n[jump](#Q612)\n"},
	}, {
		name: "cross-document anchor with no heading",
		files: map[string]string{
			"a.md": "[jump](b.md#missing)\n",
			"b.md": "# Present\n",
		},
		want: "a.md:1: dead anchor: b.md#missing -> #missing has no matching heading or <a id> in b.md",
	}, {
		name: "cross-document anchor matching a heading slug",
		files: map[string]string{
			"a.md": "[jump](b.md#present)\n",
			"b.md": "# Present\n",
		},
	}, {
		name: "anchor on a non-Markdown target is not checked",
		files: map[string]string{
			"a.md":      "[jump](script.sh#L20)\n",
			"script.sh": "#!/bin/sh\n",
		},
	}, {
		name:  "anchor into a duplicate heading",
		files: map[string]string{"a.md": "# Setup\n# Setup\n\n[second](#setup-1)\n"},
	}, {
		// The awk anchored heading matching to the line start, so this anchor
		// did not exist as far as the gate was concerned.
		name:  "anchor into a heading inside a blockquote",
		files: map[string]string{"a.md": "> ### Nested heading\n\n[jump](#nested-heading)\n"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broken, out := check(t, tc.files)
			if tc.want == "" {
				if broken != 0 {
					t.Errorf("want clean, got %d finding(s):\n%s", broken, out)
				}
				return
			}
			if broken != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("want 1 finding containing %q, got %d:\n%s", tc.want, broken, out)
			}
		})
	}
}

// A reference-style link is reported at both the use and the definition: the
// awk saw only the definition, so a dead target was flagged on a line far from
// the prose that reads as broken.
func TestDeadReferenceIsReportedAtUseAndDefinition(t *testing.T) {
	broken, out := check(t, map[string]string{
		"a.md": "Read [the design][d].\n\n[d]: docs/design.md\n",
	})
	if broken != 2 {
		t.Fatalf("broken = %d, want 2:\n%s", broken, out)
	}
	for _, want := range []string{
		"a.md:1: dead link: docs/design.md",
		"a.md:3: dead link: docs/design.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOutputShape(t *testing.T) {
	files := map[string]string{"a.md": "[x](missing.md)\n[y](also-missing.md)\n"}
	root, existFile, mdFiles := fixture(t, files)

	var plain bytes.Buffer
	broken, err := run(root, existFile, mdFiles, &plain, false)
	if err != nil {
		t.Fatal(err)
	}
	if broken != 2 {
		t.Fatalf("broken = %d, want 2", broken)
	}
	if !strings.Contains(plain.String(), "check-doc-links: FAILED — 2 broken link/anchors") {
		t.Errorf("missing plural summary:\n%s", plain.String())
	}

	var gha bytes.Buffer
	if _, err := run(root, existFile, mdFiles, &gha, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gha.String(), "::error file=a.md,line=1::dead link: missing.md") {
		t.Errorf("missing GitHub annotation:\n%s", gha.String())
	}
}

func TestCleanRunReportsCounts(t *testing.T) {
	files := map[string]string{
		"a.md": "[b](b.md) and [ext](https://example.com)\n",
		"b.md": "# B\n",
	}
	broken, out := check(t, files)
	if broken != 0 {
		t.Fatalf("broken = %d, want 0:\n%s", broken, out)
	}
	if !strings.Contains(out, "check-doc-links: ok (2 markdown files, 2 links/anchors checked)") {
		t.Errorf("unexpected summary: %s", out)
	}
}
