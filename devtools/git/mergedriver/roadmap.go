package main

import (
	"os"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/git/keyedrecords"
	"github.com/actions-gateway/github-actions-gateway/devtools/git/mdroadmap"
)

// roadmapDriver merges docs/roadmap.md, whose annotated bullet lists are keyed
// on each bullet's `<!-- q:QN -->` backlog binding — the same binding
// devtools/docs/roadmapcheck parses, so the driver and the gate read a bullet's
// identity the same way.
//
// The lists are the only part this understands. The frontmatter, the headings
// and the prose around them go through git's own merge, and a run holding even
// one unannotated bullet is prose too.
type roadmapDriver struct{}

func (d roadmapDriver) run(in *invocation) {
	sides := map[string]string{"base": in.base, "ours": in.ours, "theirs": in.theirs}
	docs := make(map[string]*mdroadmap.Doc, 3)
	for _, name := range []string{"base", "ours", "theirs"} {
		lines, err := readLines(sides[name])
		if err != nil {
			in.fallback("%s: could not be read", name)
		}
		doc, err := mdroadmap.Split(lines)
		if err != nil {
			in.fallback("%s: %s", name, err)
		}
		docs[name] = doc
	}

	base, ours, theirs := docs["base"], docs["ours"], docs["theirs"]

	// A side that added or dropped a whole list has restructured the page, and
	// the per-list pairing this driver depends on no longer holds.
	if len(ours.Lists) != len(base.Lists) || len(theirs.Lists) != len(base.Lists) {
		in.fallback("the sides disagree on how many annotated lists the page has (base %d, ours %d, theirs %d)",
			len(base.Lists), len(ours.Lists), len(theirs.Lists))
	}

	work, err := os.MkdirTemp("", in.name+"-merge")
	if err != nil {
		in.fallback("no temporary directory")
	}
	defer func() { _ = os.RemoveAll(work) }()

	var result []string
	for i := range base.Lists {
		prose, clean := in.mergeSegment(work, i, base.Lists[i].Pre, ours.Lists[i].Pre, theirs.Lists[i].Pre)
		if !clean {
			in.fallback("the prose before list %d conflicts", i+1)
		}
		merged, err := keyedrecords.Merge(
			base.Lists[i].Records, ours.Lists[i].Records, theirs.Lists[i].Records, mdroadmap.MarkerKey)
		if err != nil {
			in.fallback("list %d: %s", i+1, err)
		}
		bullets, err := mdroadmap.Decode(&base.Lists[i], &ours.Lists[i], &theirs.Lists[i], merged)
		if err != nil {
			in.fallback("list %d: %s", i+1, err)
		}
		result = append(result, prose...)
		result = append(result, bullets...)
	}

	post, clean := in.mergeSegment(work, len(base.Lists), base.Post, ours.Post, theirs.Post)
	if !clean {
		in.fallback("the prose after the last list conflicts")
	}
	result = append(result, post...)

	// Each list merged on its own, so nothing above can see a bullet that ended
	// up in two of them — the shape one branch parking an item under
	// "Exploring" produces while another moves it somewhere else. One binding,
	// one bullet, whole page.
	if dupes := duplicateBindings(result); len(dupes) > 0 {
		in.fallback("the merged page lists a backlog binding more than once: %s", strings.Join(dupes, " "))
	}

	if err := writeLines(in.ours, result); err != nil {
		in.fallback("the merged page could not be written")
	}
	in.note("resolved %s by backlog ID; review the bullet set before committing", in.targetPath)
}

func duplicateBindings(lines []string) []string {
	seen := make(map[string]bool)
	var dupes []string
	for _, line := range lines {
		key := mdroadmap.DupeKey(line)
		if key == "" {
			continue
		}
		if seen[key] {
			dupes = append(dupes, key)
		}
		seen[key] = true
	}
	return dupes
}
