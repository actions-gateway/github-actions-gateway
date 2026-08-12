package markdown

import (
	"slices"
	"strings"
	"testing"
)

// The shape a marketing badge takes on docs/features.md: a bold lead, inline
// HTML pairing a class with the text between its tags, an annotation comment,
// and prose that wraps.
const badgeBullet = "# Features\n\n## Section\n\n" +
	"- **[A capability](operations/runbook.md)** <span class=\"gag-tier-badge\">partly classic-only</span> <!-- tier:Q713 -->:\n" +
	"  one clause, then [more detail](roadmap.md).\n"

func TestListItemRawKeepsWhatTextDrops(t *testing.T) {
	items := Parse([]byte(badgeBullet)).TopLevelListItems()
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	raw := items[0].Raw

	// Text renders the way a browser does, so the class the gate keys on is
	// gone from it and present in Raw. That difference is why Raw exists.
	if strings.Contains(items[0].Text, "gag-tier-badge") {
		t.Errorf("Text kept the tag: %q", items[0].Text)
	}
	for _, want := range []string{
		`<span class="gag-tier-badge">partly classic-only</span>`,
		"<!-- tier:Q713 -->",
		"one clause",                // the continuation line
		"[more detail](roadmap.md)", // a link, destination and all
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("Raw missing %q\ngot: %q", want, raw)
		}
	}
}

func TestListItemDestinations(t *testing.T) {
	items := Parse([]byte(badgeBullet)).TopLevelListItems()
	want := []string{"operations/runbook.md", "roadmap.md"}
	got := items[0].Destinations
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("destination %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

// Deleting a bullet and leaving its indented continuation behind attaches the
// continuation to the bullet above as a second paragraph, silently widening
// what any gate measuring that item reads. ParagraphLines and EndLine are what
// let a gate say so instead of reporting an inflated count against an innocent
// bullet (Q798).
func TestListItemSpanAndParagraphs(t *testing.T) {
	const src = "# Roadmap\n\n## Section\n\n" +
		"- **A bullet.** Its prose wraps\n" +
		"  onto a second line.\n" +
		"\n" +
		"  An orphaned continuation, left by the deleted bullet below.\n" +
		"\n" +
		"- **Another bullet.** One line.\n"

	items := Parse([]byte(src)).TopLevelListItems()
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}

	if got, want := items[0].ParagraphLines, []int{5, 8}; !slices.Equal(got, want) {
		t.Errorf("first item ParagraphLines = %v, want %v", got, want)
	}
	if items[0].Line != 5 || items[0].EndLine != 8 {
		t.Errorf("first item spans %d-%d, want 5-8", items[0].Line, items[0].EndLine)
	}

	// The control: a bullet holding nothing but wrapped prose is one paragraph,
	// so the orphan signal is the blank line and not the line count.
	if got, want := items[1].ParagraphLines, []int{10}; !slices.Equal(got, want) {
		t.Errorf("second item ParagraphLines = %v, want %v", got, want)
	}
	if items[1].Line != 10 || items[1].EndLine != 10 {
		t.Errorf("second item spans %d-%d, want 10-10", items[1].Line, items[1].EndLine)
	}
}

// A tight list holds its prose in a TextBlock rather than a Paragraph, and a
// caller counting an item's blocks must not read that as zero.
func TestListItemParagraphsInTightList(t *testing.T) {
	items := Parse([]byte("- One.\n- Two.\n")).TopLevelListItems()
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	for i, item := range items {
		if len(item.ParagraphLines) != 1 {
			t.Errorf("item %d: ParagraphLines = %v, want one entry", i, item.ParagraphLines)
		}
	}
}

// A bullet whose only content is inline HTML still has a range: rawRange reads
// block lines and inline segments both, and a lead-in span carries neither a
// paragraph line nor a text node of its own on some shapes.
func TestListItemRawOnHTMLOnlyBullet(t *testing.T) {
	items := Parse([]byte("- <span class=\"gag-new-badge\">new in 1.5</span>\n")).TopLevelListItems()
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if !strings.Contains(items[0].Raw, "gag-new-badge") {
		t.Errorf("Raw missing the span: %q", items[0].Raw)
	}
}
