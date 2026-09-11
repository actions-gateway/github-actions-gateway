// Command cardbullets holds every bullet inside a `.gag-pillars` card to the
// width of the column it renders in (Q711). It is the checker behind
// scripts/docs/check-card-bullets.sh, which resolves the pages.
//
// The grid is a scanning surface: one wrapped bullet reads as prose among
// labels, so the copy is written to the column rather than the column widened
// to the copy. Nothing enforced that, and the invariant drifted — measured
// 2026-09-10 against `make docs-serve` at 1440px, two bullets on
// docs/why-gag.md wrapped to a second line, on a page website.md recorded as
// compliant on 2026-08-06.
//
// The budgets are website.md's, measured in the browser rather than derived:
// 50 characters in the three-across grid, 77 in `.gag-cols-2`. They are a
// proxy, because the body face is proportional — the same column takes 90
// narrow characters and 25 wide ones — so this gate can only catch a bullet
// that is over budget in characters, and a bullet under budget that still
// wraps on its glyphs is left to the render check website.md documents.
// Measured against that render on the two pages as they stand: the character
// rule flags exactly the two bullets that wrap and none of the other 73.
//
// It measures the *rendered* text, the way a reader sees it: a link
// contributes its text and not its destination, a code span its contents
// without the backticks. Counting the Markdown source would bill a bullet for
// a URL nobody reads.
//
// Usage:
//
//	cardbullets <page.md>...
//
// Findings print as `file:line: message`, or as GitHub `::error::`
// annotations when GITHUB_ACTIONS is set. Exits 1 on any finding, and 2 when
// no card bullet was found at all, since the gate would otherwise pass by
// checking nothing.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// The two column widths the grid has, in rendered characters, from
// docs/development/website.md § Card bullets fit on one line.
const (
	budgetThreeAcross = 50
	budgetTwoAcross   = 77
)

// cardClass opens a card grid and colsClass narrows it to two across. The
// attribute is matched rather than the whole tag so the order of `class` and
// `markdown` does not decide the verdict.
var (
	openDivRE  = regexp.MustCompile(`^\s*<div\b[^>]*>`)
	closeDivRE = regexp.MustCompile(`^\s*</div>`)
	classRE    = regexp.MustCompile(`class="([^"]*)"`)
)

const (
	cardClass = "gag-pillars"
	colsClass = "gag-cols-2"
)

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: cardbullets <page.md>...")
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	findings, checked, err := run(flag.Args(), out, os.Getenv("GITHUB_ACTIONS") != "")
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cardbullets: %v\n", err)
		os.Exit(2)
	}
	if checked == 0 {
		fmt.Fprintf(os.Stderr, "cardbullets: no `%s` card bullet found in %d file(s), so this gate would check nothing\n",
			cardClass, flag.NArg())
		os.Exit(2)
	}
	if findings > 0 {
		os.Exit(1)
	}
}

// run checks each page and reports the findings made and the bullets measured.
func run(files []string, out io.Writer, gha bool) (findings, checked int, err error) {
	for _, file := range files {
		src, rerr := os.ReadFile(file)
		if rerr != nil {
			return 0, 0, rerr
		}
		for _, b := range bullets(src) {
			checked++
			n := utf8.RuneCountInString(b.text)
			if n <= b.budget {
				continue
			}
			findings++
			msg := fmt.Sprintf("card bullet renders %d characters against a %d-character column, so it wraps: %q",
				n, b.budget, b.text)
			if gha {
				_, _ = fmt.Fprintf(out, "::error file=%s,line=%d::%s\n", file, b.line, msg)
			} else {
				_, _ = fmt.Fprintf(out, "%s:%d: %s\n", file, b.line, msg)
			}
		}
	}
	if findings > 0 {
		_, _ = fmt.Fprintf(out, "check-card-bullets: FAILED - %d card bullet(s) over their column budget\n", findings)
		return findings, checked, nil
	}
	_, _ = fmt.Fprintf(out, "check-card-bullets: ok (%d card bullet(s) in %d file(s))\n", checked, len(files))
	return 0, checked, nil
}

// bullet is one measured bullet: its rendered text, the column budget in
// force where it sits, and where to report it.
type bullet struct {
	text   string
	budget int
	line   int
}

// bullets returns every bullet inside a card grid, with the budget its grid
// sets. A card's own item is not one of these: the bullets are the list nested
// inside it, which is what `.gag-pillars li li` selects in the browser.
func bullets(src []byte) []bullet {
	doc := markdown.Parse(src)
	spans := cardSpans(strings.Split(string(src), "\n"))
	if len(spans) == 0 {
		return nil
	}
	var out []bullet
	for _, item := range doc.ListItems() {
		if item.Depth < 2 {
			continue
		}
		for _, s := range spans {
			if item.Line < s.start || item.Line > s.end {
				continue
			}
			out = append(out, bullet{
				text:   strings.TrimSpace(item.Text),
				budget: s.budget,
				line:   item.Line,
			})
			break
		}
	}
	return out
}

// span is the 1-based line range one card grid covers, and its budget.
type span struct {
	start, end, budget int
}

// cardSpans finds each card grid and the lines it covers, by counting `<div>`
// nesting from its opening tag. The grid is raw HTML to the Markdown parser —
// MkDocs' md_in_html is what renders the Markdown inside it — so the span has
// to come from the source lines rather than the AST.
func cardSpans(lines []string) []span {
	var out []span
	for i, l := range lines {
		m := classRE.FindStringSubmatch(l)
		if m == nil || !hasClass(m[1], cardClass) || !openDivRE.MatchString(l) {
			continue
		}
		budget := budgetThreeAcross
		if hasClass(m[1], colsClass) {
			budget = budgetTwoAcross
		}
		depth := 0
		end := len(lines)
		for j := i; j < len(lines); j++ {
			if openDivRE.MatchString(lines[j]) {
				depth++
			}
			if closeDivRE.MatchString(lines[j]) {
				depth--
				if depth == 0 {
					end = j + 1
					break
				}
			}
		}
		out = append(out, span{start: i + 1, end: end, budget: budget})
	}
	return out
}

// hasClass reports whether a class attribute carries one class, matching whole
// words so `gag-pillars--problem` is not `gag-pillars`.
func hasClass(attr, want string) bool {
	for _, c := range strings.Fields(attr) {
		if c == want {
			return true
		}
	}
	return false
}
