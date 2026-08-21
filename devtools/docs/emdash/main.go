// Command emdash enforces the em-dash density rule from
// docs/development/documentation-standards.md (Q654): "above roughly 3 per
// 1,000 words, rewrite". It is the counter behind
// scripts/docs/check-em-dash.sh, which selects the files.
//
// The exclusions are the point. A raw `grep -o '—' | wc -l` counts every
// legitimate use alongside the prose it is meant to police, and regular
// expressions cannot tell them apart. Counted over the parsed document
// (Q612's devtools/docs/markdown), these are skipped — dashes and words
// alike, so a doc cannot buy headroom with a long code block:
//
//   - fenced and indented code blocks, and inline code spans — the dash is
//     part of a command or an identifier, not punctuation;
//   - heading text — the title separator, `2.1. Tier 1 — Gateway Manager
//     Controller`, is this docset's section-naming convention;
//   - link text — every one of the 127 in the tree is a title citation
//     (`Appendix A — Capacity Targets & SLOs`), which is the same separator
//     reproduced from a heading the linking file does not own;
//   - raw HTML, block and inline — markup and comments the reader never sees.
//
// Table cells, blockquotes, and list items are prose and are counted.
//
// Reconciled against a raw byte count of the tree on 2026-08-04: 10,999 raw,
// 9,988 prose plus 1,009 excluded, leaving 2 the AST carries in neither place.
// Both under-count, which is the safe direction, and both are known:
//
//   - An MkDocs admonition title (`!!! warning "This changed — …"`) is consumed
//     by the dialect as an attribute rather than as inline text. It is a title,
//     so not counting it agrees with the heading rule anyway.
//   - A GFM table row with an unescaped `|` inside a code span splits into the
//     wrong cells, and one dash lands in a fragment no node keeps. That row is
//     malformed for GitHub too; the fix belongs in the doc.
//
// # The baseline
//
// The tree is at 17 per 1,000 today and 239 of 250 files are over the rule, so
// a gate set at the rule would land red and be turned off. The baseline file
// freezes each of those files at its current count as a ceiling: a listed file
// may not gain em-dashes, and a file with no entry is held to the rule. Q650 is
// the cleanup that lowers the entries; `-write-baseline` re-records them, and
// the diff is the measurement of what it cleared.
//
// # The diff ratchet
//
// A ceiling is a whole-file total, so it is slack wherever a file sits under
// its entry, and two changes can each spend that same slack on their own base
// and merge to a total above it (Q742). The ratchet measures the change rather
// than the total: with -base-dir holding the same files at the base revision, a
// file may gain em-dashes only while it stays inside the density rule. One
// already over the rule may only lose them.
//
// It never fails a reduction, and it holds under the merge queue's candidate
// commit, where the base is main and the head carries every queued change at
// once. That is the only view in which a jointly-red merge is visible before it
// lands. A file with no copy under -base-dir is left to the ceiling check, so a
// base the caller could not resolve degrades the gate rather than blocking it.
//
// Usage:
//
//	emdash -baseline <file> [-base-dir <dir>] [-max-per-1000 <density>] <file.md>...
//	emdash -report <file.md>...
//	emdash -baseline <file> -write-baseline <file.md>...
//
// Findings print as `file: message`, or as GitHub `::error::` annotations
// when GITHUB_ACTIONS is set. Exits 1 if any file is over its ceiling.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// emDash is the character the rule names. The en-dash and the hyphen are not
// the tell and are not counted.
const emDash = '—'

// shortDocAllowance is the "one or two per page is punctuation" half of the
// rule: a file under the density limit by count alone never fails, so a short
// page is not held to a ratio its length cannot express.
const shortDocAllowance = 2

func main() {
	opts := options{}
	flag.Float64Var(&opts.limit, "max-per-1000", 3, "em-dashes allowed per 1,000 prose words in a file with no baseline entry")
	flag.StringVar(&opts.baseline, "baseline", "", "file of `<count> <path>` ceilings for the files the rule has not reached yet")
	flag.BoolVar(&opts.report, "report", false, "print every file's density, worst first, and exit 0")
	flag.BoolVar(&opts.write, "write-baseline", false, "rewrite the baseline from the current counts instead of checking")
	flag.StringVar(&opts.baseDir, "base-dir", "", "`directory` holding the same files at the base revision; enables the diff ratchet")
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	over, err := run(opts, flag.Args(), out, os.Getenv("GITHUB_ACTIONS") != "")
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "emdash: %v\n", err)
		os.Exit(2)
	}
	if over > 0 {
		os.Exit(1)
	}
}

// options is the command line, threaded to run so the tests drive it directly.
type options struct {
	limit    float64
	baseline string
	baseDir  string
	report   bool
	write    bool
}

// count is one file's prose measurement. baseDashes is the same file's count
// at the base revision, carried only when -base-dir held a copy of it.
type count struct {
	file       string
	dashes     int
	words      int
	density    float64
	baseDashes int
	hasBase    bool
}

func run(opts options, files []string, out io.Writer, gha bool) (int, error) {
	counts := make([]count, 0, len(files))
	total := count{file: "(all files)"}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		dashes, words := measure(markdown.Parse(src))
		c := count{file: f, dashes: dashes, words: words, density: density(dashes, words)}
		if opts.baseDir != "" {
			if base, err := os.ReadFile(filepath.Join(opts.baseDir, f)); err == nil {
				c.baseDashes, _ = measure(markdown.Parse(base))
				c.hasBase = true
			} else if !errors.Is(err, fs.ErrNotExist) {
				return 0, err
			}
		}
		counts = append(counts, c)
		total.dashes += dashes
		total.words += words
	}
	total.density = density(total.dashes, total.words)

	if opts.report {
		byDensity(counts)
		for _, c := range append(counts, total) {
			_, _ = fmt.Fprintf(out, "%7.2f  %5d dashes  %7d words  %s\n", c.density, c.dashes, c.words, c.file)
		}
		return 0, nil
	}

	if opts.write {
		if opts.baseline == "" {
			return 0, fmt.Errorf("-write-baseline needs -baseline")
		}
		return 0, writeBaseline(opts.baseline, counts, opts.limit)
	}

	var ceilings map[string]int
	if opts.baseline != "" {
		var err error
		if ceilings, err = readBaseline(opts.baseline); err != nil {
			return 0, err
		}
	}

	var over []count
	for _, c := range counts {
		// The diff ratchet runs first: it is the finding that names what the
		// change did, and it catches a gain the ceiling below still permits.
		if c.hasBase && c.dashes > c.baseDashes && overRule(c, opts.limit) {
			report(out, gha, c.file, fmt.Sprintf(
				"%d em-dashes, up from %d at the base revision, in a file already above %.1f per 1,000 - a file over the rule may only lose them",
				c.dashes, c.baseDashes, opts.limit))
			over = append(over, c)
			continue
		}
		if ceiling, listed := ceilings[c.file]; listed {
			if c.dashes > ceiling {
				report(out, gha, c.file, fmt.Sprintf(
					"%d em-dashes, above this file's baseline ceiling of %d - cut the new ones, or lower the ceiling only after cutting others (make em-dash-baseline)",
					c.dashes, ceiling))
				over = append(over, c)
			}
			continue
		}
		if overRule(c, opts.limit) {
			report(out, gha, c.file, fmt.Sprintf(
				"em-dash density %.1f per 1,000 prose words (%d in %d) is above %.1f - see docs/development/documentation-standards.md",
				c.density, c.dashes, c.words, opts.limit))
			over = append(over, c)
		}
	}
	if n := len(over); n > 0 {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		_, _ = fmt.Fprintf(out, "check-em-dash: FAILED - %d file%s over the em-dash limit\n", n, plural)
		return n, nil
	}
	_, _ = fmt.Fprintf(out, "check-em-dash: ok (%d markdown files, %d em-dashes in %d prose words, %.2f per 1,000; %d files still on the baseline)\n",
		len(files), total.dashes, total.words, total.density, len(ceilings))
	return 0, nil
}

func report(out io.Writer, gha bool, file, msg string) {
	if gha {
		_, _ = fmt.Fprintf(out, "::error file=%s::%s\n", file, msg)
		return
	}
	_, _ = fmt.Fprintf(out, "%s: %s\n", file, msg)
}

func byDensity(counts []count) {
	sort.SliceStable(counts, func(i, j int) bool { return counts[i].density > counts[j].density })
}

// readBaseline parses `<count> <path>` lines, ignoring blanks and `#` comments.
func readBaseline(name string) (map[string]int, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	ceilings := map[string]int{}
	s := bufio.NewScanner(f)
	for line := 1; s.Scan(); line++ {
		txt := strings.TrimSpace(s.Text())
		if txt == "" || strings.HasPrefix(txt, "#") {
			continue
		}
		fields := strings.Fields(txt)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want `<count> <path>`, got %q", name, line, txt)
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", name, line, err)
		}
		ceilings[fields[1]] = n
	}
	return ceilings, s.Err()
}

// writeBaseline records every file the rule has not reached yet, at its current
// count. Files already within the rule are dropped, so the file shrinks as the
// cleanup lands and an empty one means the ratchet is done.
func writeBaseline(name string, counts []count, limit float64) error {
	var b strings.Builder
	b.WriteString(baselineHeader)
	sort.Slice(counts, func(i, j int) bool { return counts[i].file < counts[j].file })
	kept := 0
	for _, c := range counts {
		if !overRule(c, limit) {
			continue
		}
		fmt.Fprintf(&b, "%d %s\n", c.dashes, c.file)
		kept++
	}
	fmt.Fprintf(os.Stderr, "em-dash baseline: %d files recorded, %d already within %.1f per 1,000\n",
		kept, len(counts)-kept, limit)
	return os.WriteFile(name, []byte(b.String()), 0o600)
}

const baselineHeader = `# Per-file em-dash ceilings for check-em-dash.sh (Q654).
#
# Every file here is above the density rule in
# docs/development/documentation-standards.md and may not gain more em-dashes.
# A file with no entry is held to the rule itself. Q650 is the cleanup: as it
# lands, regenerate with ` + "`make em-dash-baseline`" + ` and the diff is what it cleared.
#
# <em-dashes> <path>
`

// overRule reports whether a file breaches the density rule on its own, which
// is both the verdict for an unlisted file and the condition the diff ratchet
// gates on. A page under the allowance by count alone is never over it.
func overRule(c count, limit float64) bool {
	return c.dashes > shortDocAllowance && c.density > limit
}

func density(dashes, words int) float64 {
	if words == 0 {
		return 0
	}
	return float64(dashes) * 1000 / float64(words)
}

// measure walks the prose of a document, returning its em-dashes and its word
// count. Both come from the same text, so the ratio is over what the rule
// actually governs.
func measure(doc *markdown.Document) (dashes, words int) {
	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if skip(n) {
			return ast.WalkSkipChildren, nil
		}
		var txt []byte
		switch v := n.(type) {
		case *ast.Text:
			seg := v.Segment
			txt = seg.Value(doc.Source)
		case *ast.String:
			txt = v.Value
		default:
			return ast.WalkContinue, nil
		}
		dashes += strings.Count(string(txt), string(emDash))
		words += countWords(txt)
		return ast.WalkContinue, nil
	})
	return dashes, words
}

// skip reports whether a node and everything under it is outside the prose the
// rule governs. The package comment records why each shape is here.
//
// Code blocks and HTML — fenced, indented, block or inline — need no entry:
// goldmark v1.8.5 carries their content as source segments rather than as
// child text nodes, so measure never reaches it. The package's tests assert
// that outcome directly, which is what keeps it true.
func skip(n ast.Node) bool {
	switch n.(type) {
	case *ast.CodeSpan, *ast.Heading, *ast.Link:
		return true
	}
	return false
}

// countWords counts whitespace-separated runs holding at least one letter or
// digit, so bare punctuation and list bullets do not inflate the denominator.
func countWords(txt []byte) int {
	n := 0
	inWord, alnum := false, false
	for _, r := range string(txt) {
		switch {
		case unicode.IsSpace(r):
			if inWord && alnum {
				n++
			}
			inWord, alnum = false, false
		default:
			inWord = true
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum = true
			}
		}
	}
	if inWord && alnum {
		n++
	}
	return n
}
