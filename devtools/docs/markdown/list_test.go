package markdown

import (
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
