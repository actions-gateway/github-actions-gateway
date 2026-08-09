// Command roadmapcheck keeps the public roadmap honest against the backlog,
// and keeps the feature index from regrowing into prose. It is the checker
// behind scripts/docs/check-roadmap.sh, which selects the files (Q614).
//
// docs/roadmap.md is adopter-facing narrative; docs/STATUS.md is the terse
// internal backlog. Neither can be generated from the other, so they drift: a
// 2026-07-25 audit found six of seven "In progress / near-term" roadmap items
// had already shipped, some of them release-frozen into published docs.
//
// The signal that makes this mechanical: this repo deletes a Queue row when its
// work ships (git is the archive). So a roadmap bullet naming a Q-ID that no
// longer exists in STATUS.md is an exact, zero-false-negative indicator that
// the item shipped and the bullet belongs under "Available now". Each
// forward-looking bullet therefore carries an invisible annotation naming the
// backlog rows behind it:
//
//   - **Capacity-aware job intake.** <!-- q:Q405,Q406 --> Additional opt-in …
//
// HTML comments render nowhere, on github.com or the MkDocs site. An annotation
// inside a code fence is prose about the format, not an annotation, and does
// not count.
//
// Rules:
//
//  1. Every top-level bullet under "In progress / near-term" and under
//     "Exploring / longer-term" carries a `<!-- q:QN[,QM…] -->` annotation.
//  2. Every annotated ID resolves to a row in STATUS.md. A dangling ID means
//     the work shipped — move the bullet to "Available now", or drop just that
//     ID when only part of a multi-item bullet shipped.
//  3. A near-term bullet names at least one row that is in the Queue (an
//     all-Deferred bullet was parked and belongs under "Exploring").
//  4. An exploring bullet names at least one row that is in Deferred (an ID
//     that moved into the Queue is active work and belongs under "In progress /
//     near-term").
//  5. Every top-level bullet in docs/features.md carries a Markdown link and
//     stays under maxFeatureWords. A capability with no doc to link is a
//     documentation gap to file, not a longer bullet.
//  6. The same for the roadmap's own gated bullets, under maxRoadmapWords —
//     looser, because a roadmap bullet also has to name the gate it waits on.
//  7. Every row labelled `X.Y-gate` is named by a roadmap bullet.
//  8. A bullet that writes a release version into its prose names a row
//     labelled with that gate.
//
// Rules 7 and 8 reconcile the one promise this page makes with a date attached.
// A release gate lives in STATUS.md as an `X.Y-gate` label, meaning the row
// blocks that tag; the roadmap is where an adopter reads it.
//
// Rule 7 is the load-bearing half, and it reads nothing but the `<!-- q:QN -->`
// binding and the label. Both are machine-readable, so it is indifferent to how
// the bullet renders the commitment: prose today, a derived version chip under
// Q770, something else later. A rule that instead matched the sentence "Gating
// the 1.5 release" would go quiet the moment that sentence became a pill, and a
// gate that stops matching fails exactly as silently as one that matches
// everything.
//
// Rule 8 is the narrower guard that a version typed by hand agrees with the
// label it duplicates. Q770 makes the chip derived and never hand-typed, at
// which point this rule finds nothing left to check — which is the intended end
// state rather than a blind spot, because rule 7 never depended on it.
//
// A hand-typed version means a gating verb followed by a numbered release:
// "Gating the 1.5 release", "so it gates the 1.5 release". Naming a version
// without one is context rather than a commitment, and is deliberately not a
// claim: Q273's bullet says `v2.0.0` "is the named release that removes all
// three together", which describes where the removal lands and does not assert
// that Q273 blocks that tag. Its row carries no gate label, and must not.
//
// Usage:
//
//	roadmapcheck <roadmap.md> <STATUS.md> <features.md>
//
// Exits 1 on any finding, and 2 when either page's format drifted far enough
// that the gate would otherwise pass by checking nothing.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

const (
	nearTermHeading  = "In progress / near-term"
	exploringHeading = "Exploring / longer-term"

	// The STATUS.md column carrying the `X.Y-gate` labels rules 7 and 8 read.
	labelsColumn = "Labels"

	// Generous enough that a capability plus one qualifying clause fits; the
	// longest bullet at extraction was 31 words. Tight enough that the 126-word
	// paragraphs this page replaced cannot come back.
	maxFeatureWords = 45

	// Looser than the feature cap: a roadmap bullet carries what is missing,
	// what would change, and the gate it waits on. At extraction the five worst
	// ran 74-123 words by explaining the whole approach inline.
	maxRoadmapWords = 60
)

func main() {
	args := os.Args[1:]
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: roadmapcheck <roadmap.md> <STATUS.md> <features.md>")
		os.Exit(2)
	}
	out := bufio.NewWriter(os.Stderr)
	code := run(args[0], args[1], args[2], out, os.Stdout)
	_ = out.Flush()
	os.Exit(code)
}

func run(roadmapPath, statusPath, featuresPath string, findings, summary io.Writer) int {
	docs := map[string]*markdown.Document{}
	for _, p := range []string{roadmapPath, statusPath, featuresPath} {
		src, err := os.ReadFile(p)
		if err != nil {
			_, _ = fmt.Fprintf(findings, "check-roadmap: file not found: %s\n", p)
			return 2
		}
		docs[p] = markdown.Parse(src)
	}

	queue, queueLabelled := statusRows(docs[statusPath], "Queue")
	deferred, deferredLabelled := statusRows(docs[statusPath], "Deferred")
	if len(queue) == 0 && len(deferred) == 0 {
		_, _ = fmt.Fprintf(findings, "check-roadmap: parsed no Q-IDs from %s — the table format changed?\n", statusPath)
		return 2
	}
	if !queueLabelled && !deferredLabelled {
		_, _ = fmt.Fprintf(findings, "check-roadmap: found rows but no %q column in %s — the table format changed?\n", labelsColumn, statusPath)
		return 2
	}

	c := &checker{findings: findings, queue: queue, deferred: deferred}

	roadmapName := base(roadmapPath)
	bullets := roadmapBullets(docs[roadmapPath])
	if len(bullets) == 0 {
		_, _ = fmt.Fprintf(findings, "check-roadmap: found no bullets under %q or %q in %s — the headings changed?\n",
			nearTermHeading, exploringHeading, roadmapPath)
		return 2
	}
	bound := map[string]bool{}
	for _, b := range bullets {
		c.checkRoadmapBullet(roadmapName, b, bound)
	}
	c.checkGateCoverage(base(statusPath), roadmapName, bound)

	features := featureBullets(docs[featuresPath])
	if len(features) == 0 {
		_, _ = fmt.Fprintf(findings, "check-roadmap: found no capability bullets in %s — the page format changed?\n", featuresPath)
		return 2
	}
	featuresName := base(featuresPath)
	for _, b := range features {
		if !b.hasLink {
			c.report(featuresName, b.line, fmt.Sprintf(
				"%q has no link. Every capability points at the doc that explains it; if none exists, that is a docs gap to file.", b.label))
		}
		if b.words > maxFeatureWords {
			c.report(featuresName, b.line, fmt.Sprintf(
				"%q is %d words (max %d). Move the detail into the linked doc.", b.label, b.words, maxFeatureWords))
		}
	}

	if c.failed {
		_, _ = fmt.Fprintln(findings, "check-roadmap: roadmap and backlog disagree, or the feature index drifted (see above). Reconcile per docs/development/doc-update-matrix.md.")
		return 1
	}
	_, _ = fmt.Fprintf(summary, "check-roadmap: ok (%d forward-looking bullet(s) backed by live STATUS.md rows; %d feature(s) linked)\n",
		len(bullets), len(features))
	return 0
}

// checker holds the two backlog tables by Q-ID, which is both the membership
// test rules 2-4 need and the label side of rules 7 and 8.
type checker struct {
	findings        io.Writer
	queue, deferred map[string]statusRow
	failed          bool
}

func (c *checker) report(file string, line int, msg string) {
	_, _ = fmt.Fprintf(c.findings, "check-roadmap: %s:%d: %s\n", file, line, msg)
	c.failed = true
}

// checkRoadmapBullet applies rules 1-6 and 8 to one bullet, recording in bound
// every live Q-ID it names so rule 7 can find the gated rows nothing names.
func (c *checker) checkRoadmapBullet(file string, b bullet, bound map[string]bool) {
	if !b.hasLink {
		c.report(file, b.line, fmt.Sprintf(
			"%q has no link. Point at the plan doc or Appendix G section carrying the detail.", b.label))
	}
	if b.words > maxRoadmapWords {
		c.report(file, b.line, fmt.Sprintf(
			"%q is %d words (max %d). Say what is missing, what changes, and what it waits on — the rest belongs in the linked doc.",
			b.label, b.words, maxRoadmapWords))
	}
	if len(b.ids) == 0 {
		c.report(file, b.line, fmt.Sprintf(
			"%q has no <!-- q:QN --> annotation. Name the backlog row(s) behind it so this bullet fails when they ship.", b.label))
		return
	}

	inQueue, inDeferred := false, false
	var gates []string
	for _, id := range b.ids {
		row, queued, live := c.row(id)
		switch {
		case !qIDRE.MatchString(id):
			c.report(file, b.line, fmt.Sprintf("%q annotation %q is not a Q-ID.", b.label, id))
			continue
		case !live:
			c.report(file, b.line, fmt.Sprintf(
				"%q names %s, which no longer exists in STATUS.md — the row was deleted, so the work shipped. Move this bullet to docs/features.md, or drop %s if only part of it shipped.",
				b.label, id, id))
			continue
		case queued:
			inQueue = true
		default:
			inDeferred = true
		}
		bound[id] = true
		gates = union(gates, row.gates)
	}

	if b.section == nearTermHeading && !inQueue && inDeferred {
		c.report(file, b.line, fmt.Sprintf(
			"%q names only Deferred rows — it was parked. Move it to %q.", b.label, exploringHeading))
	}
	if b.section == exploringHeading && !inDeferred && inQueue {
		c.report(file, b.line, fmt.Sprintf(
			"%q names only Queue rows — it is active work. Move it to %q.", b.label, nearTermHeading))
	}

	c.checkHandTypedRelease(file, b, gates)
}

// checkHandTypedRelease is rule 8: a version written into the bullet agrees
// with the gate label it duplicates. Only rows that still exist contribute a
// gate — a dangling ID is rule 2's finding, and re-reporting it here would
// bury it.
func (c *checker) checkHandTypedRelease(file string, b bullet, gates []string) {
	for _, claim := range b.claims {
		if !contains(gates, claim) {
			c.report(file, b.line, fmt.Sprintf(
				"%q writes the %s release into its prose, but no row it names carries `%s-gate` — the label was dropped or moved, so this page promises a release the backlog does not. Re-label the row, or drop the version and let the chip derive it.",
				b.label, claim, claim))
		}
	}
}

// checkGateCoverage is rule 7: every gated row is bound to a bullet by its
// `<!-- q:QN -->` annotation. A gate label with nowhere to render is a release
// commitment published nowhere, and it is the one direction here that reads
// only machine-readable inputs, so it cannot go quiet when the rendering
// changes.
func (c *checker) checkGateCoverage(statusFile, roadmapFile string, bound map[string]bool) {
	var uncovered []struct {
		id  string
		row statusRow
	}
	for _, table := range []map[string]statusRow{c.queue, c.deferred} {
		for id, row := range table {
			if len(row.gates) == 0 || bound[id] {
				continue
			}
			uncovered = append(uncovered, struct {
				id  string
				row statusRow
			}{id, row})
		}
	}
	// Map iteration is unordered; findings are read as a list, so sort them.
	sort.Slice(uncovered, func(i, j int) bool { return uncovered[i].row.line < uncovered[j].row.line })
	for _, u := range uncovered {
		c.report(statusFile, u.row.line, fmt.Sprintf(
			"%s is labelled %s but no %s bullet names it, so the release it blocks is committed nowhere an adopter reads. Add a bullet carrying <!-- q:%s -->, or drop the label if it no longer blocks the tag.",
			u.id, gateLabels(u.row.gates), roadmapFile, u.id))
	}
}

// gateLabels renders a row's gates the way STATUS.md writes them, for a
// finding that has to be actionable without opening the file.
func gateLabels(gates []string) string {
	out := make([]string, 0, len(gates))
	for _, g := range gates {
		out = append(out, "`"+g+"-gate`")
	}
	return strings.Join(out, " ")
}

// row returns a Q-ID's row, which table it is in, and whether it exists at
// all. A row in neither table has shipped, since done rows are deleted.
func (c *checker) row(id string) (r statusRow, queued, live bool) {
	if r, ok := c.queue[id]; ok {
		return r, true, true
	}
	if r, ok := c.deferred[id]; ok {
		return r, false, true
	}
	return statusRow{}, false, false
}

// statusRow is one backlog row, reduced to what the roadmap rules read.
type statusRow struct {
	// gates holds the `major.minor` of each `X.Y-gate` label on the row.
	gates []string
	line  int
}

// statusRows returns the rows of one STATUS.md table by Q-ID. A backlog row's
// ID cell is `<a id="QN"></a>QN`, which renders as the bare ID; a
// Progress-table cell carries a plan link instead, so it never matches.
//
// labelled reports whether the label column was found, which is what separates
// "no row is gated" from "the column moved and rules 7-8 now check nothing".
func statusRows(doc *markdown.Document, heading string) (rows map[string]statusRow, labelled bool) {
	start, end, _ := doc.SectionRange(2, heading)
	rows = map[string]statusRow{}
	for _, table := range doc.Tables() {
		labels := columnIndex(table.Header, labelsColumn)
		for _, row := range table.Rows {
			if row.Line < start || row.Line > end || len(row.Text) == 0 {
				continue
			}
			if !qIDRE.MatchString(row.Text[0]) {
				continue
			}
			if labels < 0 || labels >= len(row.Cells) {
				rows[row.Text[0]] = statusRow{line: row.Line}
				continue
			}
			labelled = true
			rows[row.Text[0]] = statusRow{gates: gateVersions(row.Cells[labels]), line: row.Line}
		}
	}
	return rows, labelled
}

// columnIndex finds a table column by its header text, reporting -1 when the
// table has no such column.
func columnIndex(header markdown.Row, name string) int {
	for i, cell := range header.Text {
		if strings.EqualFold(strings.TrimSpace(cell), name) {
			return i
		}
	}
	return -1
}

// gateVersions extracts the release gates a label cell declares, as `major.minor`.
// The backticks are required: they are what separates the label from a mention
// of one in prose.
func gateVersions(cell string) []string {
	var out []string
	for _, m := range gateLabelRE.FindAllStringSubmatch(cell, -1) {
		out = union(out, []string{m[1]})
	}
	return out
}

// releaseClaims extracts the releases a bullet's prose says it gates, as
// `major.minor`. The whitespace around the version is optional because a
// bullet wraps: a soft line break between "the" and "1.5 release" leaves the
// two fused in the rendered text this reads.
func releaseClaims(text string) []string {
	var out []string
	for _, m := range releaseClaimRE.FindAllStringSubmatch(text, -1) {
		version := m[1]
		if version == "" {
			version = m[2]
		}
		out = union(out, []string{version})
	}
	return out
}

func union(dst, src []string) []string {
	for _, s := range src {
		if !contains(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

type bullet struct {
	section string
	line    int
	label   string
	words   int
	hasLink bool
	ids     []string
	claims  []string
}

// roadmapBullets returns the top-level bullets of the two gated sections.
func roadmapBullets(doc *markdown.Document) []bullet {
	var out []bullet
	for _, section := range []string{nearTermHeading, exploringHeading} {
		start, end, ok := doc.SectionRange(2, section)
		if !ok {
			continue
		}
		for _, item := range doc.TopLevelListItems() {
			if item.Line < start || item.Line > end {
				continue
			}
			b := newBullet(doc, item)
			b.section = section
			out = append(out, b)
		}
	}
	return out
}

// featureBullets returns the top-level bullets of docs/features.md, which are
// the capability index itself: everything from the first section heading on.
// The page's lead-in prose and its version-selector tip sit above that.
func featureBullets(doc *markdown.Document) []bullet {
	first := 1 << 30
	for _, h := range doc.Headings() {
		if h.Level == 2 {
			first = h.Line
			break
		}
	}
	var out []bullet
	for _, item := range doc.TopLevelListItems() {
		if item.Line > first {
			out = append(out, newBullet(doc, item))
		}
	}
	return out
}

var (
	qIDRE       = regexp.MustCompile(`^Q[0-9]+$`)
	annotRE     = regexp.MustCompile(`<!--\s*q:([^-]*)-->`)
	gateLabelRE = regexp.MustCompile("`([0-9]+\\.[0-9]+)-gate`")

	// A gating verb, then a numbered release close enough to be its object.
	// The `[^.;]` window stops at a sentence or clause boundary so a verb
	// cannot reach a version in the next thought, and the verb alternation is
	// word-anchored because this page says "Gateway" constantly. A patch
	// component is matched and discarded: `v2.0.0` is gated by `2.0-gate`.
	releaseClaimRE = regexp.MustCompile(
		`(?i)\b(?:gat(?:e|es|ed|ing)|block(?:s|ed|ing)?)\b[^.;]{0,32}?` +
			`(?:v?([0-9]+\.[0-9]+)(?:\.[0-9]+)?\s*release|release\s*v?([0-9]+\.[0-9]+)(?:\.[0-9]+)?)`)
)

func newBullet(doc *markdown.Document, item markdown.ListItem) bullet {
	b := bullet{
		line:    item.Line,
		label:   item.Lead,
		words:   len(strings.Fields(item.Text)),
		hasLink: item.HasLink,
		claims:  releaseClaims(item.Text),
	}
	if b.label == "" {
		b.label = item.Text
	}
	for _, comment := range item.Comments {
		m := annotRE.FindStringSubmatch(comment)
		if m == nil {
			continue
		}
		for _, id := range strings.Split(strings.ReplaceAll(m[1], " ", ""), ",") {
			if id != "" {
				b.ids = append(b.ids, id)
			}
		}
	}
	return b
}

func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
