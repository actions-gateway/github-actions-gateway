package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// A marketing badge is an inline `<span class="gag-…">label</span>` pill on a
// page an adopter reads. Four exist; two of them make a claim that expires on
// its own, and those two are what this file checks.
//
// `gag-new-badge` says a capability arrived in a named release. It is declared
// by the pull request that ships the capability and never backfilled — absence
// means "not new", which is the only reading that works on a page written after
// 1.0-1.2 shipped. What makes it safe to write is that it comes off by itself:
// the gate expires a chip once it trails the current release by more than
// maxChipAge, so nothing has to remember.
//
// `gag-tier-badge` says a shipped capability reaches only one acquisition tier.
// That is a caveat rather than a property, so it must name the backlog row
// tracking the port — `<!-- tier:QN -->` — and the gate removes it on parity by
// the same signal roadmapcheck's rule 2 uses: a deleted row means the work
// shipped. It must also link an operator doc, because a tier limitation an
// operator cannot read about anywhere else is a marketing claim with nothing
// behind it.
//
// Both live on a bullet, below the page's first section heading. A page's
// lead-in explains its badges and is deliberately out of scope; a badge
// anywhere else — a table cell, a paragraph — is reported rather than skipped,
// since a badge the rules never reach is worse than no badge at all.

var (
	// A badge and the text between its tags. The label matters: the release a
	// `new in X.Y` chip names is written there and nowhere else.
	badgeRE = regexp.MustCompile(`<span class="(gag-new-badge|gag-tier-badge)">([^<]*)</span>`)

	// The tier badge's binding to the row tracking its port, in the shape rule
	// 1's `q:` annotation already established.
	tierAnnotRE = regexp.MustCompile(`<!--\s*tier:([^-]*)-->`)

	// The one label shape a new-in-release chip may take.
	newChipRE = regexp.MustCompile(`^new in ([0-9]+)\.([0-9]+)$`)

	// A link into the operator documentation, from any depth.
	opsLinkRE = regexp.MustCompile(`^(?:\.\./)*operations/`)
)

// release is a `major.minor` release, the granularity a gate label and a chip
// both name. A patch release carries no new capability, so it never appears.
type release struct{ major, minor int }

func (r release) String() string { return fmt.Sprintf("%d.%d", r.major, r.minor) }

// parseRelease reads `1.5`, `1.5.0` or `v1.5.0`. The patch component is
// accepted and discarded so a caller can hand it a tag verbatim.
func parseRelease(s string) (release, bool) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) < 2 {
		return release{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return release{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return release{}, false
	}
	return release{major, minor}, true
}

// expired reports whether a chip naming r should be gone by now.
//
// A chip naming a FUTURE release is how one is written: the pull request
// declaring it lands before the release it names is tagged, so for a whole
// development cycle the chip is ahead of every tag. A chip from a previous
// major goes with the major — the minors of a retired major cannot be counted
// against the current one, and a major bump is exactly when a capability stops
// being the new thing.
func (r release) expired(current release, maxChipAge int) bool {
	if r.major != current.major {
		return r.major < current.major
	}
	return current.minor-r.minor > maxChipAge
}

// checkBadges applies rules 9-11 to one page.
func (c *checker) checkBadges(file string, doc *markdown.Document, current *release, maxChipAge int) {
	claimed := map[int]bool{}
	for _, item := range bulletsBelowFirstSection(doc) {
		for line := item.Line; line <= item.Line+strings.Count(item.Raw, "\n"); line++ {
			claimed[line] = true
		}
		for _, m := range badgeRE.FindAllStringSubmatch(item.Raw, -1) {
			switch class, label := m[1], strings.TrimSpace(m[2]); class {
			case "gag-new-badge":
				c.checkNewChip(file, item, label, current, maxChipAge)
			case "gag-tier-badge":
				c.checkTierBadge(file, item, label)
			}
		}
	}
	c.checkStrayBadges(file, doc, claimed)
}

// checkNewChip is rule 9: the chip names a release, and that release is recent
// enough that "new" is still true.
func (c *checker) checkNewChip(file string, item markdown.ListItem, label string, current *release, maxChipAge int) {
	m := newChipRE.FindStringSubmatch(label)
	if m == nil {
		c.report(file, item.Line, fmt.Sprintf(
			"%q carries a new-in-release chip reading %q. The one form is `new in X.Y`, naming the release the capability shipped in — the gate reads the version out of that label and can expire nothing without it.",
			itemLabel(item), label))
		return
	}
	if current == nil {
		return // no release resolved; run() has already said so.
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	chip := release{major, minor}
	if chip.expired(*current, maxChipAge) {
		c.report(file, item.Line, fmt.Sprintf(
			"%q is still marked new in %s, and the current release is %s. A chip may trail the release by %d; drop it — the capability is not what is new about this project any more.",
			itemLabel(item), chip, current, maxChipAge))
	}
}

// checkTierBadge is rule 10: the badge names the row tracking its port, and the
// bullet links the operator doc recording the limitation.
func (c *checker) checkTierBadge(file string, item markdown.ListItem, label string) {
	var ids []string
	for _, comment := range item.Comments {
		m := tierAnnotRE.FindStringSubmatch(comment)
		if m == nil {
			continue
		}
		for _, id := range strings.Split(strings.ReplaceAll(m[1], " ", ""), ",") {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		c.report(file, item.Line, fmt.Sprintf(
			"%q is badged %q with no <!-- tier:QN --> annotation. Name the backlog row tracking the port, so the badge fails here the day the row ships rather than outliving the gap it describes.",
			itemLabel(item), label))
		return
	}
	for _, id := range ids {
		switch _, _, live := c.row(id); {
		case !qIDRE.MatchString(id):
			c.report(file, item.Line, fmt.Sprintf("%q tier annotation %q is not a Q-ID.", itemLabel(item), id))
		case !live:
			c.report(file, item.Line, fmt.Sprintf(
				"%q is badged %q and names %s, which no longer exists in STATUS.md — the row was deleted, so the port shipped and the tiers are at parity. Drop the badge and the annotation.",
				itemLabel(item), label, id))
		}
	}
	for _, dest := range item.Destinations {
		if opsLinkRE.MatchString(dest) {
			return
		}
	}
	c.report(file, item.Line, fmt.Sprintf(
		"%q is badged %q but links no operations/ page. A tier limitation an operator cannot read about is a claim with nothing behind it; link the doc that records which tier emits what.",
		itemLabel(item), label))
}

// checkStrayBadges is rule 11: an expiring badge outside a bullet is one the
// rules above never reach, and it fails silently rather than loudly. A page's
// lead-in, which explains its badges, sits above the first section heading and
// is not one of these.
func (c *checker) checkStrayBadges(file string, doc *markdown.Document, claimed map[int]bool) {
	first := firstSectionLine(doc)
	line := 0
	for _, text := range strings.Split(string(doc.Source), "\n") {
		line++
		if line <= first || claimed[line] {
			continue
		}
		for _, m := range badgeRE.FindAllStringSubmatch(text, -1) {
			c.report(file, line, fmt.Sprintf(
				"a `%s` badge sits outside a bullet, where no rule reaches it. Expiring badges belong on a capability bullet; a page's lead-in, above the first heading, is where they are explained.",
				m[1]))
		}
	}
}

// itemLabel names a bullet in a finding: its bold lead where it has one, its
// text otherwise.
func itemLabel(item markdown.ListItem) string {
	if item.Lead != "" {
		return item.Lead
	}
	return item.Text
}
