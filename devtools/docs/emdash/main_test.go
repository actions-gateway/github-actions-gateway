package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// Every exclusion is asserted against a control that moves the same dash into
// prose. A green exclusion case on its own cannot tell "skipped because it is
// code" from "never counted at all".
func TestMeasureExclusions(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		wantDashes int
	}{
		{"prose dash counts", "One thing — and another.\n", 1},
		{"fenced code block", "```sh\ngrep -o '—' file | wc -l\n```\n", 0},
		{"control: the same text as prose", "grep -o '—' file | wc -l\n", 1},
		{"indented code block", "text\n\n    a — b\n", 0},
		{"inline code span", "Run `grep -o '—' f` on it.\n", 0},
		{"control: the same span unfenced", "Run grep -o '—' f on it.\n", 1},
		{"heading separator", "## 2.1. Tier 1 — Gateway Manager Controller\n", 0},
		{"setext heading separator", "Tier 1 — Gateway Manager\n===\n", 0},
		{"link text title citation", "See [Appendix A — Capacity](a.md).\n", 0},
		{"control: the same title outside a link", "See Appendix A — Capacity.\n", 1},
		{"link destination", "See [a](a.md#x—y).\n", 0},
		{"html comment", "<!-- a — b -->\n", 0},
		{"inline raw html", "Text <span title=\"a — b\">x</span> more.\n", 0},
		{"table cell is prose", "| a | b |\n|---|---|\n| one — two | three |\n", 1},
		{"list item is prose", "- one — two\n", 1},
		{"blockquote is prose", "> one — two\n", 1},
		{"image alt text is prose", "![one — two](i.png)\n", 1},
		{"mkdocs admonition body is prose, not indented code", "!!! note\n\n    one — two\n", 1},
		{"mkdocs admonition title is a title, and the dialect keeps it out of the text", "!!! note \"one — two\"\n\n    body\n", 0},
		{"en-dash and hyphen are not the tell", "a – b, c - d\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := measure(markdown.Parse([]byte(tt.src)))
			if got != tt.wantDashes {
				t.Errorf("dashes = %d, want %d", got, tt.wantDashes)
			}
		})
	}
}

func TestMeasureWords(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		words int
	}{
		{"prose words", "One two three four.\n", 4},
		{"bare punctuation is not a word", "one — two\n", 2},
		{"code block words are excluded with its dashes", "```\nalpha beta gamma\n```\n\none two\n", 2},
		{"heading words are excluded with its dashes", "# alpha beta\n\none two\n", 2},
		{"link text words are excluded", "see [alpha beta gamma](a.md) now\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := measure(markdown.Parse([]byte(tt.src)))
			if got != tt.words {
				t.Errorf("words = %d, want %d", got, tt.words)
			}
		})
	}
}

// A file's density is over the rule but its dashes are within the short-doc
// allowance, so it passes: "one or two per page is punctuation".
func TestShortDocAllowance(t *testing.T) {
	dir := t.TempDir()
	short := write(t, dir, "short.md", "One — two — three.\n")
	long := write(t, dir, "long.md", "One — two — three — four.\n")

	if over := check(t, options{limit: 3}, short); over != 0 {
		t.Errorf("two dashes in a short file: over = %d, want 0", over)
	}
	if over := check(t, options{limit: 3}, long); over != 1 {
		t.Errorf("three dashes in a short file: over = %d, want 1", over)
	}
}

func TestBaselineCeiling(t *testing.T) {
	dir := t.TempDir()
	// 6 dashes in ~40 words: over the rule by density, so only a ceiling
	// keeps it green.
	body := strings.Repeat("alpha beta gamma delta epsilon zeta eta theta ", 5)
	doc := write(t, dir, "doc.md", body+strings.Repeat("x — y. ", 6)+"\n")
	dashes, _ := measure(markdown.Parse([]byte(mustRead(t, doc))))
	if dashes != 6 {
		t.Fatalf("fixture has %d dashes, want 6", dashes)
	}

	tests := []struct {
		name     string
		baseline string
		wantOver int
	}{
		{"at its ceiling", "6 " + doc, 0},
		{"under its ceiling", "9 " + doc, 0},
		{"over its ceiling", "5 " + doc, 1},
		{"no entry, held to the rule", "3 other.md", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bl := write(t, dir, strings.ReplaceAll(tt.name, " ", "-")+".txt",
				"# comment\n\n"+tt.baseline+"\n")
			if over := check(t, options{limit: 3, baseline: bl}, doc); over != tt.wantOver {
				t.Errorf("over = %d, want %d", over, tt.wantOver)
			}
		})
	}
}

// The baseline records only what the rule has not reached, so it empties as the
// cleanup lands.
func TestWriteBaseline(t *testing.T) {
	dir := t.TempDir()
	dense := write(t, dir, "dense.md", strings.Repeat("x — y. ", 6)+"\n")
	clean := write(t, dir, "clean.md", strings.Repeat("alpha beta gamma delta. ", 50)+"\n")
	bl := filepath.Join(dir, "baseline.txt")

	if _, err := run(options{limit: 3, baseline: bl, write: true}, []string{dense, clean}, discard(t), false); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, bl)
	if !strings.Contains(got, "6 "+dense) {
		t.Errorf("baseline missing the dense file:\n%s", got)
	}
	if strings.Contains(got, clean) {
		t.Errorf("baseline records a file already within the rule:\n%s", got)
	}
}

// The diff ratchet (Q742). A ceiling is a whole-file total, so it is slack
// wherever the file sits under it, and two changes can each spend that slack on
// the same base. Measuring the change instead: a file already over the rule may
// only lose em-dashes, a file inside it may still gain, and a file with no copy
// under -base-dir is left to its ceiling.
//
// The no-base row is the control on the first: that tree sits inside its
// ceiling, so its red comes from the ratchet or from nothing. The last pair is
// a file with no entry at all, where the density check reaches the same verdict
// — they assert the ratchet does not suppress it.
func TestDiffRatchet(t *testing.T) {
	short := strings.Repeat("alpha beta gamma delta epsilon zeta eta theta ", 5)
	long := strings.Repeat("alpha beta gamma delta. ", 200)
	dashes := func(prose string, n int) string { return prose + strings.Repeat("x — y. ", n) + "\n" }

	tests := []struct {
		name       string
		base, head string
		ceiling    string
		wantOver   int
	}{
		{"a gain inside a stale ceiling is caught", dashes(short, 5), dashes(short, 6), "9 ", 1},
		{"control: no base copy leaves the same tree to its ceiling", "", dashes(short, 6), "9 ", 0},
		{"a reduction is not a gain", dashes(short, 6), dashes(short, 5), "9 ", 0},
		{"an unchanged file is not a gain", dashes(short, 6), dashes(short, 6), "9 ", 0},
		{"a gain that stays inside the rule is allowed", dashes(long, 0), dashes(long, 2), "", 0},
		{"a gain that crosses the rule is still caught", dashes(long, 0), dashes(long, 3), "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := write(t, dir, "doc.md", tt.head)
			baseDir := filepath.Join(dir, "base")
			if tt.base != "" {
				p := filepath.Join(baseDir, doc)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(tt.base), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts := options{limit: 3, baseDir: baseDir}
			if tt.ceiling != "" {
				opts.baseline = write(t, dir, "baseline.txt", tt.ceiling+doc+"\n")
			}
			if over := check(t, opts, doc); over != tt.wantOver {
				t.Errorf("over = %d, want %d", over, tt.wantOver)
			}
		})
	}
}

// The ratchet's finding names the base count, so a reader can tell it from the
// ceiling's.
func TestDiffRatchetFinding(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("alpha beta gamma delta epsilon zeta eta theta ", 5)
	doc := write(t, dir, "doc.md", body+strings.Repeat("x — y. ", 6)+"\n")
	baseDir := filepath.Join(dir, "base")
	p := filepath.Join(baseDir, doc)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body+strings.Repeat("x — y. ", 5)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bl := write(t, dir, "baseline.txt", "9 "+doc+"\n")

	var out bytes.Buffer
	if _, err := run(options{limit: 3, baseline: bl, baseDir: baseDir}, []string{doc}, &out, false); err != nil {
		t.Fatal(err)
	}
	if want := "6 em-dashes, up from 5 at the base revision"; !strings.Contains(out.String(), want) {
		t.Errorf("want a finding containing %q, got:\n%s", want, out.String())
	}
}

func TestGitHubAnnotation(t *testing.T) {
	dir := t.TempDir()
	doc := write(t, dir, "doc.md", strings.Repeat("x — y. ", 6)+"\n")
	var out bytes.Buffer
	if _, err := run(options{limit: 3}, []string{doc}, &out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "::error file="+doc+"::") {
		t.Errorf("want a GitHub error annotation, got:\n%s", out.String())
	}
}

func check(t *testing.T, opts options, files ...string) int {
	t.Helper()
	over, err := run(opts, files, discard(t), false)
	if err != nil {
		t.Fatal(err)
	}
	return over
}

func discard(t *testing.T) *bytes.Buffer {
	t.Helper()
	return &bytes.Buffer{}
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
