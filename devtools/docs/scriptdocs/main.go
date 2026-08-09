// Command scriptdocs fails when a script under scripts/ has no mention in
// scripts/README.md (Q688). It is the checker behind
// scripts/docs/check-script-docs.sh, which selects the files.
//
// scripts/README.md maps every script to the gate that runs it, which is the
// only place that mapping exists. A script added without an entry is invisible
// to that map, and the drift is silent: sixteen *-test.sh files had accumulated
// with no mention by the time this gate was written, plus check-page-density.sh
// itself. Listing them fixes the day; this fixes the week.
//
// # What counts as a mention
//
// Either a link whose destination resolves to the script, or the script's
// basename appearing in the document's prose. Both routes are needed because
// the README documents scripts two ways: an entry-point script gets its own
// table row, and a *-test.sh is usually named inside its subject's row instead
// ("assertions in `gate-list-test.sh` under `make scripts-test`"), since a test
// inherits its subject's gate.
//
// Prose here is what a Text or String node carries — the same corpus the
// em-dash counter measures, and for the same reason. goldmark keeps a fenced
// code block's content and raw HTML as source segments rather than child text
// nodes, so neither reaches the walk: a filename inside a fenced example is an
// illustration, not documentation, and a `grep` cannot tell the two apart. A
// code span's contents do reach it, which is what the *-test.sh convention
// needs.
//
// A mention must stand on its own filename boundary. Plain substring matching
// makes scripts/dogfood/start.sh look documented by any sentence naming
// e2e-start.sh, which is the false pass a coverage gate can least afford.
//
// Usage:
//
//	scriptdocs <readme.md> <script>...
//
// Script paths are given relative to the same root the README's own links
// resolve against. Findings print as `file: message`, or as GitHub `::error::`
// annotations when GITHUB_ACTIONS is set. Exits 1 when a script is
// undocumented, 2 when the README's format drifted far enough that the gate
// would otherwise pass by checking nothing.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark/ast"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: scriptdocs <readme.md> <script>...")
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	missing, err := run(args[0], args[1:], out, os.Getenv("GITHUB_ACTIONS") != "")
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scriptdocs: %v\n", err)
		os.Exit(2)
	}
	if missing > 0 {
		os.Exit(1)
	}
}

func run(readme string, scripts []string, out io.Writer, gha bool) (int, error) {
	src, err := os.ReadFile(readme)
	if err != nil {
		return 0, err
	}
	doc := markdown.Parse(src)

	// Fail closed on a README that stopped being a table of scripts: with no
	// tables there is nothing to be listed in, and every script would report as
	// undocumented rather than the gate admitting it cannot judge.
	if len(doc.Tables()) == 0 {
		return 0, fmt.Errorf("%s: parsed no tables - the format drifted, and this gate cannot judge it", readme)
	}

	prose := proseText(doc)
	linked := linkedPaths(doc)

	sorted := append([]string(nil), scripts...)
	sort.Strings(sorted)

	var undocumented []string
	for _, script := range sorted {
		rel, err := filepath.Rel(filepath.Dir(readme), script)
		if err != nil {
			return 0, fmt.Errorf("%s: not under %s", script, filepath.Dir(readme))
		}
		rel = filepath.ToSlash(rel)
		if linked[rel] || mentions(prose, path.Base(rel)) {
			continue
		}
		undocumented = append(undocumented, script)
		report(out, gha, script, fmt.Sprintf(
			"no mention in %s - give it a row in the `%s` table, or name it in its subject's row, saying which gate runs it",
			readme, group(rel)))
	}

	if n := len(undocumented); n > 0 {
		_, _ = fmt.Fprintf(out, "check-script-docs: FAILED - %d of %d script(s) undocumented in %s\n", n, len(scripts), readme)
		return n, nil
	}
	_, _ = fmt.Fprintf(out, "check-script-docs: ok (%d script(s), all mentioned in %s)\n", len(scripts), readme)
	return 0, nil
}

func report(out io.Writer, gha bool, file, msg string) {
	if gha {
		_, _ = fmt.Fprintf(out, "::error file=%s::%s\n", file, msg)
		return
	}
	_, _ = fmt.Fprintf(out, "%s: %s\n", file, msg)
}

// group names the README section a script belongs in: the directory holding it,
// which is how the page is organised.
func group(rel string) string {
	if dir := path.Dir(rel); dir != "." {
		return dir + "/"
	}
	return "."
}

// proseText renders the document's prose as the mention corpus, one text node
// per line so the corpus reads the way the source does.
func proseText(doc *markdown.Document) string {
	var b strings.Builder
	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(doc.Source))
			b.WriteByte('\n')
		case *ast.String:
			b.Write(v.Value)
			b.WriteByte('\n')
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// linkedPaths collects the link destinations that resolve to a file, keyed the
// way a script path relative to the README's directory reads. An absolute or
// off-site destination cannot name a script in this tree and is dropped.
func linkedPaths(doc *markdown.Document) map[string]bool {
	linked := map[string]bool{}
	for _, link := range doc.Links() {
		dest := link.Destination
		if i := strings.IndexAny(dest, "#?"); i >= 0 {
			dest = dest[:i]
		}
		if dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "/") {
			continue
		}
		linked[path.Clean(dest)] = true
	}
	return linked
}

// mentions reports whether name appears in the prose on its own filename
// boundary. The characters excluded on either side are the ones a filename is
// built from, so `start.sh` is not found inside `e2e-start.sh`, while a path
// separator, a backtick or ordinary punctuation still delimits a real mention.
func mentions(prose, name string) bool {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(name) + `(?:[^A-Za-z0-9_-]|$)`)
	return re.MatchString(prose)
}
