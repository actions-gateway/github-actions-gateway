// Command mdreflowcoverage reports how much of the docset's prose sits at a
// sentence boundary. It is the measurement behind
// docs/development/documentation-standards.md § Measuring the residue, which
// carried a hand-derived figure nothing could reproduce until Q833.
//
// The denominator is interior line breaks inside Markdown paragraphs, as
// goldmark blocks them — the same parser mdreflow reflows against. A paragraph
// of N source lines contributes N-1 interior breaks; a one-line paragraph
// contributes none, because it has no break to place. Headings, tables, code
// fences, front matter and list markers are not paragraphs and contribute
// nothing.
//
// Blocking with a parser rather than a line classifier is the whole point. A
// regex-based reconstruction of this metric read 94.07% on a tree that was
// above 99%, because it counted every list continuation and bullet as a prose
// line that failed to end a sentence. The published figure and the
// reconstruction disagreed by six points, and neither could be checked against
// the other.
//
// A line ends at a sentence boundary when its last non-space character is
// terminal punctuation (. ? ! :), optionally followed by closing delimiters
// (" ' ) ] ` * _). The colon is deliberate: this docset ends lead-ins with one
// before a code block, and mdreflow treats those as sentence ends too.
//
// Residue is interior breaks that do not sit at a boundary. Those come from
// paragraphs mdreflow declines to touch (`make md-reflow-explain` names each
// one and why). A residue line in a paragraph mdreflow would reflow means the
// tree is simply unformatted: run `make md-reflow`.
//
// Usage:
//
//	mdreflowcoverage [-v] file.md [file.md ...]
//
// Callers select the files, so the exclusion list stays in scripts/ with the
// rest of the gate map. Exits 0 whatever it finds: a declined paragraph is a
// correct outcome, not a failure.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// closers may follow terminal punctuation and still leave the line at a
// sentence boundary: a quoted sentence, a parenthetical, a trailing emphasis
// or code span.
const closers = `"')]` + "`" + `*_`

// residueLine is one interior break that does not sit at a sentence boundary.
type residueLine struct {
	Path string
	Line int
	Text string
}

// endsSentence reports whether a source line ends at a sentence boundary.
func endsSentence(line string) bool {
	s := strings.TrimRight(line, " \t\r\n")
	s = strings.TrimRight(s, closers)
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '.', '?', '!', ':':
		return true
	}
	return false
}

// measure walks src's paragraphs and returns its interior break count and the
// breaks that do not sit at a sentence boundary.
func measure(path string, src []byte) (breaks int, residue []residueLine) {
	doc := markdown.Parse(src)
	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		// A tight list item holds its text in a TextBlock rather than a
		// Paragraph, and mdreflow reflows both. Missing TextBlock would drop
		// every bullet in the docset from the denominator.
		var lines *gmtext.Segments
		switch b := n.(type) {
		case *ast.Paragraph:
			lines = b.Lines()
		case *ast.TextBlock:
			lines = b.Lines()
		default:
			return ast.WalkContinue, nil
		}
		// The final line of a paragraph ends it, so only the breaks before it
		// are ones mdreflow had a choice about.
		for i := 0; i < lines.Len()-1; i++ {
			breaks++
			seg := lines.At(i)
			text := string(seg.Value(src))
			if endsSentence(text) {
				continue
			}
			residue = append(residue, residueLine{
				Path: path,
				Line: doc.Line(seg.Start),
				Text: strings.TrimSpace(text),
			})
		}
		return ast.WalkSkipChildren, nil
	})
	return breaks, residue
}

func main() {
	verbose := flag.Bool("v", false, "list every residue line")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mdreflowcoverage [-v] file.md [file.md ...]")
		os.Exit(2)
	}

	var breaks int
	var residue []residueLine
	for _, path := range flag.Args() {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mdreflowcoverage: %v\n", err)
			os.Exit(2)
		}
		b, r := measure(path, src)
		breaks += b
		residue = append(residue, r...)
	}

	if *verbose {
		for _, r := range residue {
			fmt.Printf("%s:%d: %s\n", r.Path, r.Line, r.Text)
		}
	}

	byFile := map[string]int{}
	for _, r := range residue {
		byFile[r.Path]++
	}
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		if byFile[paths[i]] != byFile[paths[j]] {
			return byFile[paths[i]] > byFile[paths[j]]
		}
		return paths[i] < paths[j]
	})
	for _, p := range paths {
		fmt.Printf("  %4d  %s\n", byFile[p], p)
	}

	if breaks == 0 {
		fmt.Printf("md-reflow coverage: no paragraph in %d file(s) has an interior break\n", flag.NArg())
		return
	}
	pct := 100 * float64(breaks-len(residue)) / float64(breaks)
	fmt.Printf("md-reflow coverage: %.2f%% (%d of %d interior breaks at a sentence boundary; %d residue across %d file(s), %d scanned)\n",
		pct, breaks-len(residue), breaks, len(residue), len(byFile), flag.NArg())
}
