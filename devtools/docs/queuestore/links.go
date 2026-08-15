package main

import (
	"regexp"
	"strings"
)

// An item's links are written relative to docs/ in the table and relative to
// the item file in the store, because the store's files are published pages and
// have to resolve unrendered on github.com as well as on the site. An item file
// sits one directory below the table, so the whole difference is one `../`.
//
// Item fields hold the table-relative form throughout. Marshal converts on the
// way out and UnmarshalItem converts back on the way in, so rendering a row
// never has to know where the file lives. The round-trip test is what holds the
// two directions honest.
//
// Measured against the live rows on 2026-08-15: 206 link destinations, 135
// relative to docs/, 52 already escaping it, and 19 `#QNNN` anchors pointing at
// sibling rows. Of the relative ones, 82 carry a `#fragment`, which is why this
// prefixes the destination rather than parsing it.

var (
	// linkRE matches a Markdown link's destination. Angle-bracket and titled
	// destinations do not appear in the tables; the round-trip fails loudly if
	// one ever does.
	linkRE = regexp.MustCompile(`\]\(([^)]*)\)`)
	// siblingRE matches a link to another item's page.
	siblingRE = regexp.MustCompile(`^Q\d+\.md$`)
	// anchorRE matches a table row's link to a sibling row.
	anchorRE = regexp.MustCompile(`^#Q\d+$`)
)

// external reports whether a destination addresses something outside the repo,
// which neither direction rewrites.
func external(dest string) bool {
	return strings.HasPrefix(dest, "http://") ||
		strings.HasPrefix(dest, "https://") ||
		strings.HasPrefix(dest, "mailto:")
}

// toItemLink converts a table-relative destination to one relative to an item
// file in docs/queue/.
func toItemLink(dest string) string {
	switch {
	case dest == "" || external(dest):
		return dest
	case anchorRE.MatchString(dest):
		// A sibling row becomes a sibling page, which is an address that
		// resolves on both surfaces instead of an anchor only the table has.
		return dest[1:] + ".md"
	default:
		return "../" + dest
	}
}

// toTableLink is the inverse of toItemLink.
func toTableLink(dest string) string {
	switch {
	case dest == "" || external(dest):
		return dest
	case siblingRE.MatchString(dest):
		return "#" + strings.TrimSuffix(dest, ".md")
	default:
		return strings.TrimPrefix(dest, "../")
	}
}

// rebaseLinks applies f to every Markdown link destination in text.
func rebaseLinks(text string, f func(string) string) string {
	return linkRE.ReplaceAllStringFunc(text, func(m string) string {
		dest := linkRE.FindStringSubmatch(m)[1]
		return "](" + f(dest) + ")"
	})
}

// toItemForm returns a copy of the item with every link relative to its file.
func (it Item) toItemForm() Item {
	it.Target = toItemLink(it.Target)
	it.Title = rebaseLinks(it.Title, toItemLink)
	it.Notes = rebaseLinks(it.Notes, toItemLink)
	return it
}

// toTableForm is the inverse of toItemForm.
func (it Item) toTableForm() Item {
	it.Target = toTableLink(it.Target)
	it.Title = rebaseLinks(it.Title, toTableLink)
	it.Notes = rebaseLinks(it.Notes, toTableLink)
	return it
}
