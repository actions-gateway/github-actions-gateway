package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/actions-gateway/github-actions-gateway/devtools/git/keyedrecords"
	"github.com/actions-gateway/github-actions-gateway/devtools/git/mklists"
)

// managedVars is the set of lists the gate-lists driver owns. Every one is a
// whitespace-separated, order-insensitive set that PRs append to, and every one
// is reconciled by gate-lists-check. A variable not named here is merged by git
// alone.
//
// This slice and the assignments in mk/gate-lists.mk must name the same set.
// mklists.Lift hard-fails on a name it cannot find, so a list renamed or
// dropped there without this slice following refuses every merge of that file;
// a list added there without this slice following is merged by git alone, which
// is the line-position conflict the driver exists to remove.
// git-merge-gate-lists-test.sh reconciles the two, in both directions, through
// --managed-vars below.
var managedVars = []string{
	"CHECK_FAST_GATES",
	"CHECK_HEAVY_GATES",
	"QUEUE_GATES",
	"DOCS_GATES",
	"SCRIPTS_TESTS",
}

// gateListsDriver merges mk/gate-lists.mk. Only the variables it manages are
// treated specially: each side's assignment is lifted out behind a sentinel,
// the rest of the Makefile is merged exactly as git would have merged it, and
// each lifted list is merged as a set of entries.
//
// The order carries nothing here — these are sets make expands — so the shared
// core runs under keyedrecords.BaseThenAdditions. Inferring a reorder, which
// the Markdown registries need, would refuse a merge over a difference that
// means nothing in a Makefile.
type gateListsDriver struct {
	vars []string
}

// flags answers the non-merge invocations, before git's placeholders are
// parsed. --managed-vars prints the list the driver actually runs on, so the
// suite reconciles that value against mk/gate-lists.mk rather than re-deriving
// it from source and asserting a shape nobody runs on.
func (d gateListsDriver) flags(args []string) bool {
	if len(args) == 0 || args[0] != "--managed-vars" {
		return false
	}
	for _, v := range d.vars {
		fmt.Println(v)
	}
	return true
}

func (d gateListsDriver) run(in *invocation) {
	sides := map[string]string{"base": in.base, "ours": in.ours, "theirs": in.theirs}
	docs := make(map[string]*mklists.Doc, 3)
	for _, side := range []string{"base", "ours", "theirs"} {
		lines, err := readLines(sides[side])
		if err != nil {
			in.fallback("%s: the gate lists could not be located", side)
		}
		doc, err := mklists.Lift(lines, d.vars)
		if err != nil {
			in.fallback("%s: %s", side, err)
		}
		docs[side] = doc
	}
	base, ours, theirs := docs["base"], docs["ours"], docs["theirs"]

	work, err := os.MkdirTemp("", in.name+"-merge")
	if err != nil {
		in.fallback("no temporary directory")
	}
	defer func() { _ = os.RemoveAll(work) }()

	// The Makefile minus the managed assignments, merged by git alone. A
	// conflict here is an ordinary Makefile conflict and is none of this
	// driver's business.
	body, clean := in.mergeSegment(work, 0, base.Body, ours.Body, theirs.Body)
	if !clean {
		in.fallback("the Makefile conflicts outside the gate lists")
	}

	blocks := make(map[string][]string, len(d.vars))
	for _, v := range d.vars {
		merged, err := keyedrecords.MergeOrdered(
			base.Blocks[v].Entries, ours.Blocks[v].Entries, theirs.Blocks[v].Entries,
			identityKey, keyedrecords.BaseThenAdditions)
		if err != nil {
			in.fallback("%s: %s", v, err)
		}

		adds := missing(merged, ours.Blocks[v].Entries)
		dels := missing(ours.Blocks[v].Entries, merged)
		style := mklists.StyleOf(ours.Blocks[v])
		switch {
		case len(adds) == 0 && len(dels) == 0:
			// Nothing changed for this list. Reuse ours byte for byte, so an
			// untouched variable contributes no diff at all.
			blocks[v] = ours.Blocks[v].Lines
		case len(dels) == 0:
			blocks[v] = mklists.Append(ours.Blocks[v].Lines, adds, style)
		default:
			// A removal has to rewrite the wrapped lines, so this is the one
			// case that re-renders the whole block.
			blocks[v] = mklists.Render(v, style, merged)
		}

		// The render is the one step that could silently corrupt a list, so
		// read it back and require it to be the same assignment, in two
		// respects that fail independently. Membership: the entry set it
		// produces must equal the set that went in — order is not compared.
		// Shape: it must still be one continued assignment, which membership
		// cannot see, because a block that lost a backslash reads back with
		// every entry present and assigns only its first line.
		if !sameSet(merged, mklists.BlockEntries(blocks[v])) {
			in.fallback("the rebuilt %s did not round-trip to the entries it was given", v)
		}
		if err := mklists.Continued(blocks[v]); err != nil {
			in.fallback("the rebuilt %s is not one assignment: %s", v, err)
		}
	}

	result, err := mklists.Substitute(body, d.vars, blocks)
	if err != nil {
		in.fallback("%s", err)
	}
	if err := writeLines(in.ours, result); err != nil {
		in.fallback("the merged Makefile could not be written")
	}
	in.note("resolved %s gate lists entry by entry; review the list contents before committing", in.targetPath)
}

// identityKey is the key reader for a bare word: the word is its own key.
func identityKey(entry string) string { return entry }

// missing lists the entries of want that have absent does not hold, sorted.
//
// Sorted so that an append of several entries at once lands in a stable order
// rather than one that depends on which side introduced them.
func missing(want, absent []string) []string {
	have := make(map[string]bool, len(absent))
	for _, e := range absent {
		have[e] = true
	}
	var out []string
	for _, e := range want {
		if !have[e] {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	seen := make(map[string]int, len(a))
	for _, e := range a {
		seen[e]++
	}
	for _, e := range b {
		seen[e]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
