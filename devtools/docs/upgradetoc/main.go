// Command upgradetoc holds the hand-kept Table of Contents in
// docs/operations/upgrade.md to the headings it indexes (Q865). It is the
// checker behind scripts/docs/check-upgrade-toc.sh, which resolves the file.
//
// doc-links already resolves every `#anchor` in the tree, but it can only fail
// an anchor that is *written*: a heading the TOC never mentions has no link to
// check, so the page's own index silently stops covering it. Measured
// 2026-08-18 on upgrade.md: 50 indexable headings against 47 entries, with two
// migration notes and one GMC subsection reachable only by scrolling, plus one
// entry listed two places early, which left three of them out of document
// order.
//
// It fails on:
//
//  1. An indexable heading with no TOC entry — the blind spot above.
//  2. A TOC entry naming no indexable heading, or naming one twice.
//  3. An entry sequence that does not follow document order, or whose nesting
//     does not follow heading level.
//
// Indexable means level 2 or 3, excluding the page title and the Table of
// Contents heading itself: the depth documentation-standards.md already asks
// of an operator-facing doc, not a rule invented here. The level-4 steps under
// a migration note are procedure detail the index stops above.
//
// Out of scope, deliberately: an entry's link *text*. The TOC writes some
// heading titles with their code spans and some without, so holding the text
// to the heading would be a rewrite of the page rather than a gate on its
// index — and the text is not what makes a section reachable. A heading
// renamed within one slug therefore leaves the entry's text stale, which no
// gate here reports.
//
// Slugs come from devtools/docs/markdown, the same source doc-links resolves
// anchors against, so the two gates cannot disagree about what an anchor
// points at.
//
// Usage:
//
//	upgradetoc <upgrade.md>
//
// Findings print as `file:line: message`, or as GitHub `::error::` annotations
// when GITHUB_ACTIONS is set. Exits 1 on any finding, and 2 when the page's
// shape drifted far enough that the gate would otherwise pass by checking
// nothing.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
	"github.com/yuin/goldmark/ast"
)

// tocHeading is the section the entries live under, and maxLevel the deepest
// heading it indexes.
const (
	tocHeading = "Table of Contents"
	maxLevel   = 3
)

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: upgradetoc <upgrade.md>")
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	findings, err := run(flag.Arg(0), out, os.Getenv("GITHUB_ACTIONS") != "")
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "upgradetoc: %v\n", err)
		os.Exit(2)
	}
	if findings > 0 {
		os.Exit(1)
	}
}

// run checks one page and reports how many findings it made. The path is
// printed as given, so the caller decides whether findings read repo-relative.
func run(file string, out io.Writer, gha bool) (int, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	doc := markdown.Parse(src)

	start, end, ok := doc.SectionRange(2, tocHeading)
	if !ok {
		return 0, fmt.Errorf("%s has no `## %s` heading, so there is no index to check", file, tocHeading)
	}
	entries := tocEntries(doc, start, end)
	if len(entries) == 0 {
		return 0, fmt.Errorf("%s: the `## %s` section holds no links, so this gate would check nothing", file, tocHeading)
	}
	want := indexable(doc)
	if len(want) == 0 {
		return 0, fmt.Errorf("%s has no level-2 or level-3 headings outside its index, so this gate would check nothing", file)
	}

	findings := compare(want, entries)
	for _, f := range findings {
		if gha {
			_, _ = fmt.Fprintf(out, "::error file=%s,line=%d::%s\n", file, f.line, f.msg)
		} else {
			_, _ = fmt.Fprintf(out, "%s:%d: %s\n", file, f.line, f.msg)
		}
	}
	name := filepath.Base(file)
	if n := len(findings); n > 0 {
		_, _ = fmt.Fprintf(out, "check-upgrade-toc: FAILED — %s's Table of Contents does not match its headings (%d finding%s)\n",
			name, n, plural(n))
		return n, nil
	}
	_, _ = fmt.Fprintf(out, "check-upgrade-toc: ok (%s, %d headings indexed by %d entries)\n", name, len(want), len(entries))
	return 0, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// entry is one Table of Contents bullet: the heading it points at, how deeply
// the bullet is nested, and where to report it.
type entry struct {
	anchor string
	depth  int
	line   int
}

// heading is one indexable heading, with the bullet depth its level calls for.
type heading struct {
	text   string
	anchor string
	depth  int
	line   int
}

type finding struct {
	line int
	msg  string
}

// indexable returns, in document order, the headings the TOC is expected to
// carry. Level 2 sits at the top of the list and each further level nests one
// deeper, which is how the page already writes it.
func indexable(doc *markdown.Document) []heading {
	var out []heading
	for _, h := range doc.Headings() {
		if h.Level < 2 || h.Level > maxLevel || h.Text == tocHeading {
			continue
		}
		out = append(out, heading{text: h.Text, anchor: h.Slug, depth: h.Level - 1, line: h.Line})
	}
	return out
}

// tocEntries returns the same-page links in the TOC section, in source order,
// each with its bullet nesting depth. Only the first link of a bullet counts:
// a heading title that itself contains a link would otherwise index twice.
func tocEntries(doc *markdown.Document, start, end int) []entry {
	var out []entry
	depth := 0
	claimed := map[ast.Node]bool{}

	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if _, ok := n.(*ast.List); ok {
			if entering {
				depth++
			} else {
				depth--
			}
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		line := blockLine(doc, link)
		if line < start || line > end {
			return ast.WalkContinue, nil
		}
		item := enclosingItem(link)
		if item == nil || claimed[item] {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if !strings.HasPrefix(dest, "#") {
			return ast.WalkContinue, nil
		}
		claimed[item] = true
		out = append(out, entry{anchor: dest[1:], depth: depth, line: line})
		return ast.WalkContinue, nil
	})
	return out
}

// blockLine reports the source line of the nearest enclosing block, which is
// the bullet's own text. Inline nodes carry no source lines of their own.
func blockLine(doc *markdown.Document, n ast.Node) int {
	for p := n; p != nil; p = p.Parent() {
		if p.Type() != ast.TypeBlock {
			continue
		}
		if lines := p.Lines(); lines != nil && lines.Len() > 0 {
			return doc.Line(lines.At(0).Start)
		}
	}
	return 1
}

func enclosingItem(n ast.Node) ast.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.ListItem); ok {
			return p
		}
	}
	return nil
}

// compare reports every way the entry sequence departs from the heading
// sequence. Absent and unknown entries are reported first and in full, because
// each is a section a reader cannot reach from the index; order and nesting are
// then reported against what remains, at the first bullet that departs, since
// one moved entry displaces every entry after it.
func compare(want []heading, got []entry) []finding {
	seen := map[string]int{}
	for _, e := range got {
		seen[e.anchor]++
	}
	known := map[string]heading{}
	for _, h := range want {
		known[h.anchor] = h
	}

	var findings []finding
	for _, h := range want {
		if seen[h.anchor] == 0 {
			findings = append(findings, finding{h.line, fmt.Sprintf(
				"heading %q is absent from the Table of Contents, so it is reachable only by scrolling — add `%s[%s](#%s)`",
				h.text, strings.Repeat("  ", h.depth-1)+"- ", h.text, h.anchor)})
		}
	}
	counted := map[string]int{}
	for _, e := range got {
		counted[e.anchor]++
		switch {
		case known[e.anchor].anchor == "":
			findings = append(findings, finding{e.line, fmt.Sprintf(
				"Table of Contents entry #%s names no level-2 or level-3 heading in this page", e.anchor)})
		case counted[e.anchor] > 1:
			findings = append(findings, finding{e.line, fmt.Sprintf(
				"Table of Contents lists #%s more than once", e.anchor)})
		}
	}

	// Order and nesting are only answerable over the entries that name a
	// heading exactly once; anything else is already reported above, and
	// comparing against it would report the same defect a second time.
	var haveSeq []entry
	for _, e := range got {
		if known[e.anchor].anchor != "" && seen[e.anchor] == 1 {
			haveSeq = append(haveSeq, e)
		}
	}
	var wantSeq []heading
	for _, h := range want {
		if seen[h.anchor] == 1 {
			wantSeq = append(wantSeq, h)
		}
	}
	for i, e := range haveSeq {
		if i >= len(wantSeq) {
			break
		}
		if w := wantSeq[i]; w.anchor != e.anchor {
			findings = append(findings, finding{e.line, fmt.Sprintf(
				"Table of Contents is out of document order: entry %d is #%s, but the next heading is %q (#%s)",
				i+1, e.anchor, w.text, w.anchor)})
			break
		}
	}
	for i, e := range haveSeq {
		if i >= len(wantSeq) || wantSeq[i].anchor != e.anchor {
			break
		}
		if w := wantSeq[i]; w.depth != e.depth {
			findings = append(findings, finding{e.line, fmt.Sprintf(
				"Table of Contents entry #%s is nested %d level(s) deep but its heading is level %d, so it wants %d",
				e.anchor, e.depth, w.depth+1, w.depth)})
			break
		}
	}
	return findings
}
