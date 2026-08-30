// Package mdregistry carves a Markdown registry page into the prose and
// table-row segments its merge driver merges separately, and reads the keys
// those rows are merged by.
//
// It is the Markdown half of the drivers behind scripts/docs/git-merge-script-index.sh
// and scripts/docs/git-merge-plan-index.sh; the set merge itself is in
// devtools/git/keyedrecords.
package mdregistry

import (
	"regexp"
	"strings"
)

// linkRE matches a Markdown inline link. The target class excludes `)` so a
// link is never run together with what follows it.
var linkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// LinkKey reads the first cell's Markdown link target, which is what keys
// docs/plan/README.md and scripts/README.md: one row per file, and the path is
// the identity the rest of the tooling already uses (check-plan-index.sh and
// check-script-docs.sh read the same cell). A row whose first cell is not a
// link has no key.
//
// The key comes from cell 1 alone, so an escaped `\|` in a later cell cannot
// shift it.
func LinkKey(line string) string {
	if !strings.HasPrefix(line, "|") {
		return ""
	}
	cells := strings.Split(line, "|")
	if len(cells) < 3 {
		return ""
	}
	m := linkRE.FindStringSubmatch(cells[1])
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], " \t")
}
