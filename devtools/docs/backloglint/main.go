// Command backloglint enforces the format rules on a repo-local backlog file
// (docs/STATUS.md). It is the checker behind scripts/docs/lint-backlog.sh,
// which maps the environment interface onto these flags so the gate map stays
// in scripts/ (Q613).
//
// Rows are read from the GFM table AST rather than split on a literal `|` at
// fixed indices: one escaped pipe shifted every field, so `St` read the label
// cell and `Notes` read the size, and the rules then passed on the wrong cells
// (Q613 — Q625 carries a live instance). Cell lengths count runes, not bytes:
// the awk this replaces counted bytes under BWK awk and mawk but runes under
// gawk in a UTF-8 locale, so a row near the cap was enforced differently
// depending on which awk ran it.
//
// The rules, numbered as scripts/docs/lint-backlog.sh and the backlog skill
// number them:
//
//  1. No `**Next ID:** QN` line — IDs come from a server-side ref allocator,
//     and a file-local counter conflicts by construction (Q382).
//  2. IDs are unique across the Queue and Deferred tables, and each row's
//     `<a id="QN"></a>QN` anchor matches its visible ID.
//  3. Queue St is 🔲 or 🚫 only. ✅/▶/💤 are old-format markers.
//  4. Queue Notes and Deferred triggers are at most -max-chars; over
//     -link-chars the cell must link another document.
//  5. A `Blocked by [QN](#QN)` prefix requires St 🚫, and every `(#QN)` link
//     target must resolve to an existing row.
//  6. Deferred triggers open with **Demand:**, **Event:**, or **Decision:**.
//  7. No `Last touched:` line — git holds that fact.
//  8. A `flake`-labelled Queue row may not simply vanish; it moves to
//     Deferred § Flake watch.
//  9. Deleting the last Queue row pointing at a plan flips that plan's
//     Progress verdict to ✅ in the same edit.
//  10. A row the baseline deleted may not reappear.
//  11. Every label a row wears is declared on the `**Labels:**` line.
//  12. A Q-ID this branch ADDS holds a `refs/queue-ids/QN` claim on the remote,
//     so no concurrent session can be handed it (Q656).
//  13. A row spells no more cells than its table header declares, so an
//     unescaped `|` cannot silently truncate it (Q870).
//
// Rules 8, 9, 10 and 12 compare against a git baseline, since a deletion — or
// a newly added row — is invisible from the file alone. That baseline is the
// merge base with origin/main, not its tip (Q684); see baselineRef. They are
// no-ops when no baseline resolves, and rule 12 is additionally skipped when
// the remote is unreachable.
//
// Usage:
//
//	backloglint [-staged] [-max-chars N] [-link-chars N] \
//	    [-allow-flake-delete "Q1 Q2"] [-allow-resurrect "Q1 Q2"] \
//	    [-allow-progress-stale "plan/foo.md"] [-allow-unclaimed-id "Q1 Q2"] \
//	    path/to/STATUS.md
//
// Findings print as `file:line: message`, or as GitHub `::error::` annotations
// when GITHUB_ACTIONS is set. Exits 1 if any rule fires, 2 on a usage error.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
	"github.com/yuin/goldmark/ast"
)

type config struct {
	file              string
	staged            bool
	maxChars          int
	linkChars         int
	allowFlakeDelete  []string
	allowResurrect    []string
	allowProgressStop []string
	allowUnclaimedID  []string
	gha               bool
}

func main() {
	staged := flag.Bool("staged", false, "pre-commit mode: skip when the file is not staged, and require it to be staged alone")
	maxChars := flag.Int("max-chars", 250, "hard cap on a Notes or trigger cell, in characters")
	linkChars := flag.Int("link-chars", 200, "length above which a Notes or trigger cell must link a document")
	allowFlake := flag.String("allow-flake-delete", "", "space-separated IDs whose flake row may be retired (rule 8)")
	allowResurrect := flag.String("allow-resurrect", "", "space-separated IDs that may be deliberately re-opened (rule 10)")
	allowStale := flag.String("allow-progress-stale", "", "space-separated plan paths whose Progress row may stay ⚠️ (rule 9)")
	allowUnclaimed := flag.String("allow-unclaimed-id", "", "space-separated IDs claimed from another clone or session (rule 12)")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: backloglint [flags] path/to/STATUS.md")
		os.Exit(2)
	}
	cfg := config{
		file:              flag.Arg(0),
		staged:            *staged,
		maxChars:          *maxChars,
		linkChars:         *linkChars,
		allowFlakeDelete:  strings.Fields(*allowFlake),
		allowResurrect:    strings.Fields(*allowResurrect),
		allowProgressStop: strings.Fields(*allowStale),
		allowUnclaimedID:  strings.Fields(*allowUnclaimed),
		gha:               os.Getenv("GITHUB_ACTIONS") != "",
	}

	out := bufio.NewWriter(os.Stdout)
	errOut := bufio.NewWriter(os.Stderr)
	n, err := run(cfg, out, errOut)
	_ = out.Flush()
	_ = errOut.Flush()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint-backlog: %v\n", err)
		os.Exit(2)
	}
	if n > 0 {
		os.Exit(1)
	}
}

// finding is one rule failure. line 0 means the finding is about the file as a
// whole rather than a row.
type finding struct {
	line int
	msg  string
	// detail is the multi-line remediation the git-baseline rules print in a
	// terminal; CI gets the one-line annotation instead.
	detail string
}

type linter struct {
	cfg      config
	rel      string // repo-relative path of cfg.file, "" outside a repo
	git      *repo
	findings []finding
}

func (l *linter) fail(line int, msg string) { l.failWith(line, msg, "") }

func (l *linter) failWith(line int, msg, detail string) {
	l.findings = append(l.findings, finding{line: line, msg: msg, detail: detail})
}

// run checks the file and reports how many rules fired. Findings go to errOut
// in a terminal and to out as `::error::` annotations under CI, which is where
// GitHub reads workflow commands from.
func run(cfg config, out, errOut io.Writer) (int, error) {
	l := &linter{cfg: cfg, git: newRepo(filepath.Dir(cfg.file))}
	l.rel = l.git.relpath(cfg.file)

	if cfg.staged {
		staged, err := l.git.stagedFiles()
		if err != nil {
			return 0, err
		}
		if !contains(staged, l.rel) {
			return 0, nil
		}
		if others := without(staged, l.rel); len(others) > 0 {
			_, _ = fmt.Fprintf(errOut, "lint-backlog: %s must be committed in isolation, but these files are staged with it:\n", l.rel)
			for _, f := range others {
				_, _ = fmt.Fprintf(errOut, "  %s\n", f)
			}
			_, _ = fmt.Fprintln(errOut, "commit the backlog edit separately (git reset <files>, or commit them first)")
			return 1, nil
		}
	}

	src, err := os.ReadFile(cfg.file)
	if err != nil {
		return 0, err
	}
	doc := parseBacklog(src)

	l.checkBaseline(doc)
	l.checkContent(doc)

	sort.SliceStable(l.findings, func(i, j int) bool { return l.findings[i].line < l.findings[j].line })
	for _, f := range l.findings {
		if cfg.gha {
			if f.line > 0 {
				_, _ = fmt.Fprintf(out, "::error file=%s,line=%d::%s\n", cfg.file, f.line, f.msg)
			} else {
				_, _ = fmt.Fprintf(out, "::error file=%s::%s\n", cfg.file, f.msg)
			}
			continue
		}
		if f.line > 0 {
			_, _ = fmt.Fprintf(errOut, "lint-backlog: %s:%d: %s\n", cfg.file, f.line, f.msg)
		} else {
			_, _ = fmt.Fprintf(errOut, "lint-backlog: %s: %s\n", cfg.file, f.msg)
		}
		if f.detail != "" {
			_, _ = fmt.Fprint(errOut, f.detail)
		}
	}
	if n := len(l.findings); n > 0 {
		return n, nil
	}
	_, _ = fmt.Fprintf(out, "lint-backlog: ok (%s)\n", cfg.file)
	return 0, nil
}

// --- the parsed backlog -----------------------------------------------------

// row is one item row of the Queue or Deferred table.
type row struct {
	id     string
	cells  []string
	line   int
	anchor string // the id spelled by the row's <a id="…"> anchor, "" if absent
	// srcCells is how many cells the source line spells and hdrCells how many
	// the table header declares. cells is already truncated to hdrCells, so
	// rule 13 needs the untruncated count; see sourceCells.
	srcCells int
	hdrCells int
}

func (r row) cell(i int) string {
	if i < len(r.cells) {
		return r.cells[i]
	}
	return ""
}

// backlog is the file's structure: the rows of each named section, the
// paragraph lines the header rules read, and which sections exist.
type backlog struct {
	progress []row
	queue    []row
	deferred []row
	// paras holds every paragraph line with its 1-based source line, so the
	// header rules never match text inside a code block.
	paras    []textLine
	hasQueue bool
}

type textLine struct {
	text string
	line int
}

func parseBacklog(src []byte) *backlog {
	doc := markdown.Parse(src)
	b := &backlog{}
	srcLines := strings.Split(string(src), "\n")

	headings := doc.Headings()
	// section names the `## ` heading a line sits under; a `### ` sub-heading
	// (Deferred's Flake watch) does not open a new section.
	section := func(line int) string {
		name := ""
		for _, h := range headings {
			if h.Level != 2 || h.Line >= line {
				continue
			}
			name = h.Text
		}
		return name
	}
	for _, h := range headings {
		if h.Level == 2 && strings.HasPrefix(h.Text, "Queue") {
			b.hasQueue = true
		}
	}

	for _, t := range doc.Tables() {
		var dst *[]row
		switch name := section(t.Line); {
		case strings.HasPrefix(name, "Progress"):
			dst = &b.progress
		case strings.HasPrefix(name, "Queue"):
			dst = &b.queue
		case strings.HasPrefix(name, "Deferred"):
			dst = &b.deferred
		default:
			continue
		}
		for _, r := range t.Rows {
			nr := newRow(r)
			nr.hdrCells = len(t.Header.Cells)
			nr.srcCells = sourceCells(srcLines, r.Line, nr.hdrCells)
			*dst = append(*dst, nr)
		}
	}

	b.paras = paragraphLines(doc)
	return b
}

// paragraphLines returns every paragraph line with its source line. The header
// rules are line-anchored, and reading them off the AST keeps a line inside a
// code block — which is prose about the format, not the format — from firing
// them. markdown exports Root and Line for exactly this.
func paragraphLines(doc *markdown.Document) []textLine {
	var out []textLine
	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		p, ok := n.(*ast.Paragraph)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		lines := p.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			out = append(out, textLine{
				text: strings.TrimRight(string(seg.Value(doc.Source)), "\r\n"),
				line: doc.Line(seg.Start),
			})
		}
		return ast.WalkSkipChildren, nil
	})
	return out
}

var (
	anchorRE   = regexp.MustCompile(`<a id="(Q[0-9]+)">`)
	tagRE      = regexp.MustCompile(`<[^>]*>`)
	linkSynRE  = regexp.MustCompile(`\[|\]\([^)]*\)`)
	visibleRE  = regexp.MustCompile(`^Q[0-9]+$`)
	labelRE    = regexp.MustCompile("`[^`]+`")
	declLinkRE = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	docLinkRE  = regexp.MustCompile(`\]\([^#)]`)
	refRE      = regexp.MustCompile(`\(#(Q[0-9]+)\)`)
	planRE     = regexp.MustCompile(`\(plan/[A-Za-z0-9._-]+\.md`)
	blockedRE  = regexp.MustCompile(`^Blocked by \[Q[0-9]+\]`)
	triggerRE  = regexp.MustCompile(`^\*\*(Demand|Event|Decision):\*\*`)
	nextIDRE   = regexp.MustCompile(`^\*\*Next ID:\*\*`)
	touchedRE  = regexp.MustCompile(`^Last touched:`)
	labelsRE   = regexp.MustCompile(`^\*\*Labels:\*\*`)
)

// newRow reads a table row's ID cell. A row whose first cell is not a bare
// `QN` — a Progress row, or a stray line — carries no ID and only the label
// rule applies to it.
func newRow(r markdown.Row) row {
	out := row{cells: r.Cells, line: r.Line}
	if len(r.Cells) == 0 {
		return out
	}
	if m := anchorRE.FindStringSubmatch(r.Cells[0]); m != nil {
		out.anchor = m[1]
	}
	visible := strings.TrimSpace(tagRE.ReplaceAllString(r.Cells[0], ""))
	if visibleRE.MatchString(visible) {
		out.id = visible
	}
	return out
}

// sourceCells reports how many cells the row's source line spells. A row wider
// than its header is truncated in the AST, as GFM renders it, so Row.Cells can
// never exceed the header and rule 13 has to re-read the line on its own, where
// ParseRow finds its natural width. Falls back to want, leaving the rule
// silent, when the line does not read as a row on its own.
func sourceCells(srcLines []string, line, want int) int {
	if line < 1 || line > len(srcLines) {
		return want
	}
	r, ok := markdown.ParseRow(srcLines[line-1])
	if !ok {
		return want
	}
	return len(r.Cells)
}

// plain strips HTML tags and Markdown link syntax so a cell reads plainly in a
// message.
func plain(cell string) string {
	return strings.TrimSpace(linkSynRE.ReplaceAllString(tagRE.ReplaceAllString(cell, ""), ""))
}

// --- content rules ----------------------------------------------------------

// checkWidths enforces rule 13. GFM reads a `|` as a column separator even
// inside a code span, so one written raw splits the row and everything past the
// header's last column is dropped from the rendered table, on github.com and on
// the site both. Q866 lost two thirds of its Notes that way, and the truncation
// hid an over-cap cell from rule 4 on top.
func (l *linter) checkWidths(b *backlog) {
	for _, section := range [][]row{b.progress, b.queue, b.deferred} {
		for _, r := range section {
			if r.srcCells <= r.hdrCells {
				continue
			}
			l.fail(r.line, fmt.Sprintf(
				"%s has %d cells but the header declares %d; an unescaped | splits a row even inside a code span, and GFM drops the overflow. Write it as \\|",
				rowName(r), r.srcCells, r.hdrCells))
		}
	}
}

// rowName names a row in a message: its ID, or its first cell for a Progress
// row, which carries none.
func rowName(r row) string {
	if r.id != "" {
		return r.id
	}
	return truncate(plain(r.cell(0)), 40)
}

func (l *linter) checkContent(b *backlog) {
	l.checkWidths(b)
	declared, declaredList, seenDecl := declaredLabels(b)
	warnedNoDecl := false
	checkLabels := func(line int, who, cell string) {
		for _, m := range labelRE.FindAllString(cell, -1) {
			tok := strings.Trim(m, "`")
			if !seenDecl {
				if !warnedNoDecl {
					warnedNoDecl = true
					l.fail(line, "rows carry labels but no **Labels:** line declares the vocabulary")
				}
				continue
			}
			if !declared[tok] {
				l.fail(line, fmt.Sprintf("%s uses undeclared label `%s`; declared: %s", who, tok, declaredList))
			}
		}
	}

	for _, p := range b.paras {
		if nextIDRE.MatchString(p.text) {
			l.fail(p.line, "old format: drop the Next ID counter; allocate with scripts/docs/alloc-queue-id.sh (a file-local counter conflicts by construction under concurrent sessions)")
		}
		if touchedRE.MatchString(p.text) {
			l.fail(p.line, "old format: drop the Last touched line; use git log -1 --format=%as -- "+l.cfg.file)
		}
	}

	// Progress rows carry no ID, and their Labels cell is one column earlier.
	for _, r := range b.progress {
		checkLabels(r.line, plain(r.cell(0)), r.cell(1))
	}

	ids := map[string]string{}
	registerID := func(r row, section string) {
		if where, dup := ids[r.id]; dup {
			l.fail(r.line, fmt.Sprintf("duplicate ID %s (in %s and %s)", r.id, where, section))
			return
		}
		ids[r.id] = section
	}
	type ref struct {
		from   string
		target string
		line   int
	}
	var refs []ref
	collectRefs := func(r row, cells ...string) {
		for _, m := range refRE.FindAllStringSubmatch(strings.Join(cells, "|"), -1) {
			refs = append(refs, ref{from: r.id, target: m[1], line: r.line})
		}
	}

	for _, r := range b.queue {
		if !l.checkID(r) {
			continue
		}
		registerID(r, "Queue")
		checkLabels(r.line, r.id, r.cell(2))
		item, st, notes := r.cell(1), strings.TrimSpace(r.cell(3)), strings.TrimSpace(r.cell(5))
		switch st {
		case "💤":
			l.fail(r.line, r.id+" is 💤 in the Queue; deferred rows move to the ## Deferred table (old format)")
		case "✅", "▶":
			l.fail(r.line, r.id+" St is "+st+"; done rows are deleted and started is signaled by the open PR (old format)")
		case "🔲", "🚫":
		default:
			l.fail(r.line, r.id+" St must be 🔲 or 🚫; got: "+st)
		}
		l.checkCaps(r, "Notes", item, notes)
		if blockedRE.MatchString(notes) && st != "🚫" {
			l.fail(r.line, r.id+" Notes say Blocked by but St is not 🚫")
		}
		collectRefs(r, item, notes)
	}

	for _, r := range b.deferred {
		if !l.checkID(r) {
			continue
		}
		registerID(r, "Deferred")
		checkLabels(r.line, r.id, r.cell(2))
		item, trigger := r.cell(1), strings.TrimSpace(r.cell(4))
		if !triggerRE.MatchString(trigger) {
			l.fail(r.line, fmt.Sprintf("%s Deferred trigger must open with **Demand:**, **Event:**, or **Decision:**; got: %s",
				r.id, truncate(trigger, 40)))
		}
		l.checkCaps(r, "trigger cell", item, trigger)
		collectRefs(r, item, trigger)
	}

	if !b.hasQueue {
		l.fail(0, "no ## Queue section found")
	}
	for _, rf := range refs {
		if _, ok := ids[rf.target]; !ok {
			l.fail(rf.line, fmt.Sprintf("%s links (#%s) but no row %s exists", rf.from, rf.target, rf.target))
		}
	}
}

// checkID reports whether the row is an item row, failing rule 2's anchor
// half on the way.
func (l *linter) checkID(r row) bool {
	if r.id == "" {
		return false
	}
	switch {
	case r.anchor == "":
		l.fail(r.line, fmt.Sprintf("%s has no <a id=%q></a> anchor; cross-references cannot resolve", r.id, r.id))
	case r.anchor != r.id:
		l.fail(r.line, fmt.Sprintf("anchor id=%q does not match visible ID %s", r.anchor, r.id))
	}
	return true
}

// checkCaps enforces rule 4 on a Notes or trigger cell. Length is in runes,
// counted over the cell's source form: a link costs what it is written as, and
// an escaped pipe costs the two characters it takes to write.
func (l *linter) checkCaps(r row, what, item, cell string) {
	n := utf8.RuneCountInString(cell)
	switch {
	case n > l.cfg.maxChars:
		l.fail(r.line, fmt.Sprintf("%s %s is %d chars (max %d); move detail to the linked plan doc",
			r.id, what, n, l.cfg.maxChars))
	case n > l.cfg.linkChars && !hasDocLink(item, cell):
		suffix := " but links no document"
		if what == "Notes" {
			suffix = " but links no document from its Item or Notes cell (a #QN sibling anchor does not count)"
		}
		l.fail(r.line, fmt.Sprintf("%s %s is %d chars (> %d)%s", r.id, what, n, l.cfg.linkChars, suffix))
	}
}

// hasDocLink reports whether either cell links another document. A `#QN`
// sibling anchor does not count — sibling rows are capped too, so they cannot
// hold the context this row dropped.
func hasDocLink(item, cell string) bool {
	return docLinkRE.MatchString(item) || docLinkRE.MatchString(cell)
}

// declaredLabels reads the vocabulary off the `**Labels:**` line. A -gate
// label's parenthetical gloss carries its own backticked link text, which
// names a release rather than a label, so link constructs are dropped first.
func declaredLabels(b *backlog) (map[string]bool, string, bool) {
	declared := map[string]bool{}
	var order []string
	seen := false
	for _, p := range b.paras {
		if !labelsRE.MatchString(p.text) {
			continue
		}
		seen = true
		for _, m := range labelRE.FindAllString(declLinkRE.ReplaceAllString(p.text, ""), -1) {
			tok := strings.Trim(m, "`")
			if !declared[tok] {
				declared[tok] = true
				order = append(order, tok)
			}
		}
	}
	return declared, strings.Join(order, " "), seen
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func without(list []string, drop string) []string {
	var out []string
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
