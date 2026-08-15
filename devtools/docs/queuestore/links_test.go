package main

import (
	"strings"
	"testing"
)

func TestLinkRebasingIsInvertible(t *testing.T) {
	for _, table := range []string{
		"development/maintaining-backlog.md",
		"../devtools/docs/metrictiers/main.go",
		"design/04-operational-flows.md#which-disruptions-are-recovered",
		"#Q408",
		"https://github.com/actions-gateway/github-actions-gateway/issues/1",
		"",
	} {
		item := toItemLink(table)
		if back := toTableLink(item); back != table {
			t.Errorf("%q -> %q -> %q, want the original back", table, item, back)
		}
	}
}

func TestLinkRebasingMovesTheDestination(t *testing.T) {
	for _, c := range []struct{ table, want string }{
		{"design/foo.md", "../design/foo.md"},
		{"../scripts/x.sh", "../../scripts/x.sh"},
		{"design/a.md#frag", "../design/a.md#frag"},
		{"#Q408", "Q408.md"},
		{"https://example.invalid/x", "https://example.invalid/x"},
	} {
		if got := toItemLink(c.table); got != c.want {
			t.Errorf("toItemLink(%q) = %q, want %q", c.table, got, c.want)
		}
	}
}

func TestRebaseLinksRewritesEveryDestinationInProse(t *testing.T) {
	notes := "See [the plan](design/x.md) and [the script](../scripts/y.sh), plus [Q408](#Q408)."
	got := rebaseLinks(notes, toItemLink)
	want := "See [the plan](../design/x.md) and [the script](../../scripts/y.sh), plus [Q408](Q408.md)."
	if got != want {
		t.Errorf("rebaseLinks:\n  got  %s\n  want %s", got, want)
	}
	if back := rebaseLinks(got, toTableLink); back != notes {
		t.Errorf("rebaseLinks does not invert:\n  got  %s\n  want %s", back, notes)
	}
}

// The store round-trip is symmetric, so it would pass just as happily if the
// re-basing did nothing at all. This asserts the written file actually carries
// the item-relative form, which is the half a reader on github.com sees.
func TestWrittenFileCarriesItemRelativeLinks(t *testing.T) {
	it := Item{
		ID:     "Q1",
		Rank:   "a0",
		Status: StatusReady,
		Size:   "S",
		Target: "design/x.md",
		Title:  "A title",
		Notes:  "Body links [a doc](development/testing.md) and [Q408](#Q408).",
	}
	body, err := it.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	src := string(body)

	for _, want := range []string{
		"target: ../design/x.md",
		"](../development/testing.md)",
		"](Q408.md)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("written file does not carry %q:\n%s", want, src)
		}
	}
	for _, unwanted := range []string{
		"target: design/x.md",
		"](development/testing.md)",
		"](#Q408)",
	} {
		if strings.Contains(src, unwanted) {
			t.Errorf("written file still carries the table form %q:\n%s", unwanted, src)
		}
	}

	back, err := UnmarshalItem(body)
	if err != nil {
		t.Fatalf("UnmarshalItem: %v", err)
	}
	if back.Target != it.Target || back.Notes != it.Notes {
		t.Errorf("reading back did not restore the table form:\n  target %q\n  notes  %q", back.Target, back.Notes)
	}
}
