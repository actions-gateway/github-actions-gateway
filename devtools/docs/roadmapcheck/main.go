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
//  9. A `gag-new-badge` chip reads `new in X.Y` and names a release no more
//     than -max-chip-age behind the current one.
//  10. A `gag-tier-badge` badge carries a `<!-- tier:QN -->` annotation naming a
//     live row, and its bullet links an `operations/` page.
//  11. Both of those badges sit on a bullet below the page's first section
//     heading, where rules 9 and 10 can reach them.
//  12. A capped bullet holds one paragraph. A blank line splits it in two, and
//     what a reader sees as a bullet is then measured over both.
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
// Rules 9-11 are the marketing badges, and badges.go carries their reasoning.
// They belong here rather than in a gate of their own because they ask the same
// question rules 2 and 7 ask — does this page's claim still agree with the
// backlog — of a different rendering of it.
//
// Rule 12 exists because rules 5 and 6 measure a Markdown list item, and an
// item swallows more than a reader thinks. Deleting a bullet and leaving its
// indented continuation lines behind attaches them to the bullet above, whose
// cap then breaks on words that are not its own: an accurate finding against an
// innocent bullet, which reads as pre-existing debt rather than as the deletion
// that caused it (Q798). Naming the stray paragraph is what makes it a fixable
// finding, and the caps report the line span they measured for the same reason.
// The two pages this reaches use no multi-paragraph bullets; the grid cards on
// docs/index.md and docs/why-gag.md do, and are checked for badges alone.
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
//	roadmapcheck [-release X.Y] [-max-chip-age N] \
//	    <roadmap.md> <STATUS.md> <features.md> [page.md…]
//
// Trailing pages are scanned for badges only — the other marketing surfaces,
// which carry no roadmap bullets and no capability index. -release is the
// current release, and rule 9 is skipped (loudly) without one, since a fresh
// fork has no tag to be behind.
//
// Exits 1 on any finding, and 2 when either page's format drifted far enough
// that the gate would otherwise pass by checking nothing.
package main

import (
	"bufio"
	"flag"
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

	// How many releases a `new in X.Y` chip may trail the current release. One
	// keeps the chip through the release it names and the one after it, so the
	// pull request that declares it never has to schedule its own removal, and
	// a capability stops being advertised as new two releases on.
	defaultMaxChipAge = 1
)

// config is one invocation: which pages to read and what "the current release"
// means for this run.
type config struct {
	roadmap, status, features string
	// badgeOnly names the marketing surfaces carrying no roadmap bullets and no
	// capability index, so only rules 9-11 apply to them.
	badgeOnly  []string
	release    string
	maxChipAge int
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.release, "release", "", "current release, as X.Y or a vX.Y.Z tag")
	flag.IntVar(&cfg.maxChipAge, "max-chip-age", defaultMaxChipAge, "releases a `new in X.Y` chip may trail the current release")
	flag.Parse()
	args := flag.Args()
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: roadmapcheck [-release X.Y] [-max-chip-age N] <roadmap.md> <STATUS.md> <features.md> [page.md…]")
		os.Exit(2)
	}
	cfg.roadmap, cfg.status, cfg.features, cfg.badgeOnly = args[0], args[1], args[2], args[3:]
	out := bufio.NewWriter(os.Stderr)
	code := run(cfg, out, os.Stdout)
	_ = out.Flush()
	os.Exit(code)
}

func run(cfg config, findings, summary io.Writer) int {
	roadmapPath, statusPath, featuresPath := cfg.roadmap, cfg.status, cfg.features
	docs := map[string]*markdown.Document{}
	paths := append([]string{roadmapPath, statusPath, featuresPath}, cfg.badgeOnly...)
	for _, p := range paths {
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
				"%q is %d words (max %d), counted over %s. Move the detail into the linked doc.",
				b.label, b.words, maxFeatureWords, b.span()))
		}
		c.checkOrphan(featuresName, b)
	}

	current, ok := parseRelease(cfg.release)
	if !ok {
		// Loud, because rule 9 is the one rule that cannot run without an
		// outside fact, and a quiet skip reads exactly like a clean pass.
		_, _ = fmt.Fprintf(summary, "check-roadmap: no current release given, so `new in X.Y` chips are unchecked (rule 9)\n")
	}
	for _, p := range paths {
		if p == statusPath {
			continue
		}
		if ok {
			c.checkBadges(base(p), docs[p], &current, cfg.maxChipAge)
			continue
		}
		c.checkBadges(base(p), docs[p], nil, cfg.maxChipAge)
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

// checkOrphan is rule 12. It is reported at the stray paragraph rather than at
// the bullet, because that is the line the edit goes on and the bullet above it
// is innocent.
func (c *checker) checkOrphan(file string, b bullet) {
	if len(b.paraLines) < 2 {
		return
	}
	for _, line := range b.paraLines[1:] {
		c.report(file, line, fmt.Sprintf(
			"a second paragraph inside the bullet at line %d (%q), so its word count is measured over %s. Deleting a bullet without its indented continuation lines leaves them attached to the bullet above. Delete them too, or unindent them into a paragraph of their own.",
			b.line, b.label, b.span()))
	}
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
			"%q is %d words (max %d), counted over %s. Say what is missing, what changes, and what it waits on — the rest belongs in the linked doc.",
			b.label, b.words, maxRoadmapWords, b.span()))
	}
	c.checkOrphan(file, b)
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

// checkGateCoverage is rule 7: every gated row that an adopter would read
// about is bound to a bullet by its `<!-- q:QN -->` annotation. A gate label
// with nowhere to render is a release commitment published nowhere, and it is
// the one direction here that reads only machine-readable inputs, so it cannot
// go quiet when the rendering changes.
//
// Scoped to rows carrying `feature` or `security`, because a gate label answers
// two different questions at once. "Blocks the tag" covers the CI, test, docs
// and dogfood work a release also waits on; "an adopter would upgrade for
// this" does not. Requiring a bullet for both put our own release harness on
// the page people read to evaluate the product.
func (c *checker) checkGateCoverage(statusFile, roadmapFile string, bound map[string]bool) {
	var uncovered []struct {
		id  string
		row statusRow
	}
	for _, table := range []map[string]statusRow{c.queue, c.deferred} {
		for id, row := range table {
			if len(row.gates) == 0 || bound[id] || !row.adopterFacing {
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
			"%s is labelled %s and carries `feature` or `security`, but no %s bullet names it, so the release it blocks is committed nowhere an adopter reads. Add a bullet carrying <!-- q:%s -->, drop the label if it no longer blocks the tag, or drop `feature`/`security` if the work is release process rather than something to upgrade for.",
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
	// adopterFacing is whether the row carries `feature` or `security`, which
	// is what decides whether a gate label also obliges a roadmap bullet.
	adopterFacing bool
	line          int
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
			rows[row.Text[0]] = statusRow{
				gates:         gateVersions(row.Cells[labels]),
				adopterFacing: adopterFacingLabelRE.MatchString(row.Cells[labels]),
				line:          row.Line,
			}
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
	endLine int
	label   string
	words   int
	hasLink bool
	ids     []string
	claims  []string
	// paraLines is where each of the bullet's block-level paragraphs starts.
	// More than one is rule 12's finding.
	paraLines []int
}

// span renders the source lines a bullet's word count was taken over, so a
// count that disagrees with the bullet a reader sees can be reconciled without
// re-deriving which lines it read.
func (b bullet) span() string {
	if b.endLine <= b.line {
		return fmt.Sprintf("line %d", b.line)
	}
	return fmt.Sprintf("lines %d-%d", b.line, b.endLine)
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
// the capability index itself.
func featureBullets(doc *markdown.Document) []bullet {
	var out []bullet
	for _, item := range bulletsBelowFirstSection(doc) {
		out = append(out, newBullet(doc, item))
	}
	return out
}

// bulletsBelowFirstSection returns a page's top-level list items from its first
// section heading on. A page's lead-in prose sits above that — on features.md
// the version-selector tip and the badge legend, which names badges rather than
// applying them.
func bulletsBelowFirstSection(doc *markdown.Document) []markdown.ListItem {
	first := firstSectionLine(doc)
	var out []markdown.ListItem
	for _, item := range doc.TopLevelListItems() {
		if item.Line > first {
			out = append(out, item)
		}
	}
	return out
}

// firstSectionLine reports the line of a page's first level-2 heading, or a
// line past the end when it has none.
func firstSectionLine(doc *markdown.Document) int {
	for _, h := range doc.Headings() {
		if h.Level == 2 {
			return h.Line
		}
	}
	return 1 << 30
}

var (
	qIDRE       = regexp.MustCompile(`^Q[0-9]+$`)
	annotRE     = regexp.MustCompile(`<!--\s*q:([^-]*)-->`)
	gateLabelRE = regexp.MustCompile("`([0-9]+\\.[0-9]+)-gate`")

	// The labels that make a gated row adopter-facing, and so oblige it to
	// appear on the roadmap. Release scope is not all one kind: a capability or
	// a security fix is what someone upgrades FOR, while the CI, test, docs and
	// dogfood work that also blocks a tag is process. Publishing the latter on
	// a page adopters read describes our harness to people evaluating the
	// product.
	adopterFacingLabelRE = regexp.MustCompile("`(feature|security)`")

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
		line:      item.Line,
		endLine:   item.EndLine,
		label:     item.Lead,
		words:     len(strings.Fields(item.Text)),
		hasLink:   item.HasLink,
		claims:    releaseClaims(item.Text),
		paraLines: item.ParagraphLines,
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
