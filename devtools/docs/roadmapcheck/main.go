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
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

const (
	nearTermHeading  = "In progress / near-term"
	exploringHeading = "Exploring / longer-term"

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

	queue := statusIDs(docs[statusPath], "Queue")
	deferred := statusIDs(docs[statusPath], "Deferred")
	if len(queue) == 0 && len(deferred) == 0 {
		_, _ = fmt.Fprintf(findings, "check-roadmap: parsed no Q-IDs from %s — the table format changed?\n", statusPath)
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
	for _, b := range bullets {
		c.checkRoadmapBullet(roadmapName, b)
	}

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

type checker struct {
	findings        io.Writer
	queue, deferred map[string]bool
	failed          bool
}

func (c *checker) report(file string, line int, msg string) {
	_, _ = fmt.Fprintf(c.findings, "check-roadmap: %s:%d: %s\n", file, line, msg)
	c.failed = true
}

func (c *checker) checkRoadmapBullet(file string, b bullet) {
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
	for _, id := range b.ids {
		switch {
		case !qIDRE.MatchString(id):
			c.report(file, b.line, fmt.Sprintf("%q annotation %q is not a Q-ID.", b.label, id))
		case c.queue[id]:
			inQueue = true
		case c.deferred[id]:
			inDeferred = true
		default:
			c.report(file, b.line, fmt.Sprintf(
				"%q names %s, which no longer exists in STATUS.md — the row was deleted, so the work shipped. Move this bullet to docs/features.md, or drop %s if only part of it shipped.",
				b.label, id, id))
		}
	}

	if b.section == nearTermHeading && !inQueue && inDeferred {
		c.report(file, b.line, fmt.Sprintf(
			"%q names only Deferred rows — it was parked. Move it to %q.", b.label, exploringHeading))
	}
	if b.section == exploringHeading && !inDeferred && inQueue {
		c.report(file, b.line, fmt.Sprintf(
			"%q names only Queue rows — it is active work. Move it to %q.", b.label, nearTermHeading))
	}
}

// statusIDs returns the Q-IDs of the rows in one STATUS.md table. A backlog
// row's ID cell is `<a id="QN"></a>QN`, which renders as the bare ID; a
// Progress-table cell carries a plan link instead, so it never matches.
func statusIDs(doc *markdown.Document, heading string) map[string]bool {
	start, end, _ := doc.SectionRange(2, heading)
	ids := map[string]bool{}
	for _, table := range doc.Tables() {
		for _, row := range table.Rows {
			if row.Line < start || row.Line > end || len(row.Text) == 0 {
				continue
			}
			if qIDRE.MatchString(row.Text[0]) {
				ids[row.Text[0]] = true
			}
		}
	}
	return ids
}

type bullet struct {
	section string
	line    int
	label   string
	words   int
	hasLink bool
	ids     []string
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
	qIDRE   = regexp.MustCompile(`^Q[0-9]+$`)
	annotRE = regexp.MustCompile(`<!--\s*q:([^-]*)-->`)
)

func newBullet(doc *markdown.Document, item markdown.ListItem) bullet {
	b := bullet{
		line:    item.Line,
		label:   item.Lead,
		words:   len(strings.Fields(item.Text)),
		hasLink: item.HasLink,
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
