package markdown

import (
	"fmt"
	"strings"
	"testing"
)

// dests renders the links of a source as "line:destination" strings, which is
// what the gates consume: a destination the parser never yields is a link the
// gate never checks.
func dests(t *testing.T, src string) []string {
	t.Helper()
	var got []string
	for _, l := range Parse([]byte(src)).Links() {
		got = append(got, fmt.Sprintf("%d:%s", l.Line, l.Destination))
	}
	return got
}

func TestLinks(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{{
		// The Q612 defect: the collect regex the awk used matched the inner
		// image, so the outer destination was never seen.
		name: "badge-wrapped link yields both the image and the outer target",
		src:  "[![License](img/badge.svg)](LICENSE)\n",
		want: []string{"1:LICENSE", "1:img/badge.svg"},
	}, {
		name: "link text spanning a line break",
		src:  "See [the capacity\nappendix](docs/appendix-a.md) for limits.\n",
		want: []string{"1:docs/appendix-a.md"},
	}, {
		name: "reference-style link yields the definition and the use",
		src:  "Read [the design][d] first.\n\n[d]: docs/design.md\n",
		want: []string{"1:docs/design.md", "3:docs/design.md"},
	}, {
		name: "shortcut reference link resolves to its definition",
		src:  "Read [design] first.\n\n[design]: docs/design.md\n",
		want: []string{"1:docs/design.md", "3:docs/design.md"},
	}, {
		name: "unused reference definition is still collected",
		src:  "Nothing uses it.\n\n[unused]: docs/orphan.md\n",
		want: []string{"3:docs/orphan.md"},
	}, {
		name: "angle-bracketed destination is unwrapped",
		src:  "[a file](<docs/a file.md>)\n",
		want: []string{"1:docs/a file.md"},
	}, {
		name: "parentheses inside a destination are not truncated",
		src:  "[wiki](docs/a(x).md)\n",
		want: []string{"1:docs/a(x).md"},
	}, {
		name: "brackets inside link text do not drop the link",
		src:  "[see [inner]](docs/target.md)\n",
		want: []string{"1:docs/target.md"},
	}, {
		name: "title after the destination is not part of it",
		src:  "[a](docs/a.md \"the title\")\n",
		want: []string{"1:docs/a.md"},
	}, {
		name: "inline code containing bracket syntax is not a link",
		src:  "Write `[text](target.md)` to link, or ``[a](b)`` inline.\n",
		want: nil,
	}, {
		name: "fenced code containing bracket syntax is not a link",
		src:  "```\n[text](never-checked.md)\n```\n",
		want: nil,
	}, {
		name: "indented code containing bracket syntax is not a link",
		src:  "Example:\n\n    [text](never-checked.md)\n",
		want: nil,
	}, {
		name: "autolink is reported as an autolink",
		src:  "<https://example.com/x>\n",
		want: []string{"1:https://example.com/x"},
	}, {
		name: "image alongside a link on one line",
		src:  "![alt](img/a.png) and [text](docs/b.md)\n",
		want: []string{"1:img/a.png", "1:docs/b.md"},
	}, {
		name: "links inside a table cell",
		src:  "| a | b |\n|---|---|\n| [x](docs/x.md) | [y](docs/y.md) |\n",
		want: []string{"3:docs/x.md", "3:docs/y.md"},
	}, {
		name: "link inside a blockquote",
		src:  "> See [the doc](docs/x.md).\n",
		want: []string{"1:docs/x.md"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dests(t, tc.src)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("links = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinksInMkDocsDialect(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{{
		// Without the admonition parser the body is an indented code block and
		// every link in it goes unchecked — 15 live ones when this was measured.
		name: "admonition body is parsed as block content",
		src: "!!! info \"The numbers behind these claims\"\n\n" +
			"    For limits, see [Appendix A](design/appendix-a.md); for cost,\n" +
			"    [Appendix F](design/appendix-f.md).\n",
		want: []string{"3:design/appendix-a.md", "4:design/appendix-f.md"},
	}, {
		name: "collapsible admonition body is parsed too",
		src:  "???+ note \"Details\"\n\n    See [the doc](docs/x.md).\n",
		want: []string{"3:docs/x.md"},
	}, {
		name: "content after an admonition is not swallowed by it",
		src:  "!!! note \"T\"\n\n    Inside [a](docs/a.md).\n\nOutside [b](docs/b.md).\n",
		want: []string{"3:docs/a.md", "5:docs/b.md"},
	}, {
		name: "an admonition marker inside a fence stays code",
		src:  "```\n!!! note \"T\"\n\n    [a](docs/a.md)\n```\n",
		want: nil,
	}, {
		name: "markdown=span content is parsed",
		src:  "<p class=\"caption\" markdown=\"span\">Read the [overview](design/x.md).</p>\n",
		want: []string{"1:design/x.md"},
	}, {
		name: "markdown=1 block content is parsed",
		src:  "<div markdown=\"1\">\n[a](docs/a.md)\n</div>\n",
		want: []string{"2:docs/a.md"},
	}, {
		// markdown="0" means "do not parse". Without the attribute matching,
		// the content is an ordinary HTML block, which is raw.
		name: "markdown=0 content stays raw HTML",
		src:  "<div class=\"stats\" markdown=\"0\">\n[a](docs/a.md)\n</div>\n",
		want: nil,
	}, {
		name: "plain HTML block content stays raw HTML",
		src:  "<div class=\"stats\">\n[a](docs/a.md)\n</div>\n",
		want: nil,
	}, {
		// Not the extension's doing: a blank line ends a CommonMark HTML
		// block, so what follows is an ordinary paragraph.
		name: "content after a blank line inside an HTML block is markdown",
		src:  "<div class=\"stats\">\n\n[a](docs/a.md)\n\n</div>\n",
		want: []string{"3:docs/a.md"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dests(t, tc.src)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("links = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHeadings(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string // "line:slug"
	}{{
		name: "atx heading",
		src:  "## The `check` gate\n",
		want: []string{"1:the-check-gate"},
	}, {
		name: "setext heading",
		src:  "The check gate\n==============\n",
		want: []string{"1:the-check-gate"},
	}, {
		// The awk anchored its heading match to the start of the line, so a
		// heading inside a blockquote published no anchor and every link to it
		// read as dead.
		name: "heading inside a blockquote",
		src:  "> ### Kata does not close the path\n",
		want: []string{"1:kata-does-not-close-the-path"},
	}, {
		name: "duplicate headings get github-slugger suffixes",
		src:  "# Setup\n# Setup\n# Setup\n",
		want: []string{"1:setup", "2:setup-1", "3:setup-2"},
	}, {
		name: "punctuation and emoji are dropped, hyphen runs are not collapsed",
		src:  "## C. Go 100% — done 🎉\n",
		want: []string{"1:c-go-100--done-"},
	}, {
		name: "link markup in a heading slugs its text",
		src:  "## See [the design](docs/design.md)\n",
		want: []string{"1:see-the-design"},
	}, {
		// GitHub slugs the rendered text, so the tags contribute nothing.
		name: "inline HTML in a heading contributes no slug characters",
		src:  "# <span class=\"hero\">Self-hosted</span> with zero idle\n",
		want: []string{"1:self-hosted-with-zero-idle"},
	}, {
		name: "a heading inside a fence is not a heading",
		src:  "```\n# Not a heading\n```\n",
		want: nil,
	}, {
		name: "closing hashes are not part of the slug",
		src:  "## Overview ##\n",
		want: []string{"1:overview"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, h := range Parse([]byte(tc.src)).Headings() {
				got = append(got, fmt.Sprintf("%d:%s", h.Line, h.Slug))
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("headings = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTMLAnchors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string // "line:id"
	}{{
		name: "anchor in a table cell",
		src:  "| ID | Item |\n|---|---|\n| <a id=\"Q612\"></a>Q612 | a row |\n",
		want: []string{"3:Q612"},
	}, {
		name: "name attribute counts as an anchor",
		src:  "<a name=\"legacy\"></a>\n",
		want: []string{"1:legacy"},
	}, {
		name: "id after another attribute is still found",
		src:  "<a class=\"x\" id=\"deep\"></a>\n",
		want: []string{"1:deep"},
	}, {
		// Prose *about* an anchor publishes no anchor: GitHub renders the code
		// span literally. The awk read raw lines and counted all three.
		name: "anchor inside an inline code span is not an anchor",
		src:  "Renumbering means the row, its `<a id=\"QN\"></a>` anchor, and more.\n",
		want: nil,
	}, {
		name: "anchor inside a fence is not an anchor",
		src:  "```html\n<a id=\"example\"></a>\n```\n",
		want: nil,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, a := range Parse([]byte(tc.src)).HTMLAnchors() {
				got = append(got, fmt.Sprintf("%d:%s", a.Line, a.ID))
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("anchors = %q, want %q", got, tc.want)
			}
		})
	}
}

// The gates report `file:line:`, and goldmark nodes carry byte offsets, so the
// offset→line index is load-bearing for every finding's usefulness.
func TestLine(t *testing.T) {
	doc := Parse([]byte("one\ntwo\n\nfour\n"))
	for offset, want := range map[int]int{0: 1, 3: 1, 4: 2, 8: 3, 9: 4, 13: 4} {
		if got := doc.Line(offset); got != want {
			t.Errorf("Line(%d) = %d, want %d", offset, got, want)
		}
	}
}

// A word count that moves when a paragraph is rewrapped is not a word count.
// roadmapcheck enforces a 60-word cap on ListItem.Text, so a segment boundary
// that swallowed its line break under-counted every wrapped bullet by one word
// per wrap, and the cap passed on prose that exceeded it.
func TestListItemTextSurvivesRewrapping(t *testing.T) {
	const wrapped = "- one two three\n  four five six\n  seven eight\n"
	const flat = "- one two three four five six seven eight\n"

	words := func(src string) []string {
		items := Parse([]byte(src)).TopLevelListItems()
		if len(items) != 1 {
			t.Fatalf("parsed %d list items from %q, want 1", len(items), src)
		}
		return strings.Fields(items[0].Text)
	}

	got, want := words(wrapped), words(flat)
	if len(want) != 8 {
		t.Fatalf("flat item has %d words (%v), want 8", len(want), want)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("wrapped item = %v (%d words), flat = %v (%d words)", got, len(got), want, len(want))
	}
}

func TestSluggerDedupesAcrossDocument(t *testing.T) {
	s := NewSlugger()
	want := []string{"setup", "setup-1", "other", "setup-2"}
	for i, text := range []string{"Setup", "Setup", "Other", "Setup"} {
		if got := s.Slug(text); got != want[i] {
			t.Errorf("Slug(%q) #%d = %q, want %q", text, i, got, want[i])
		}
	}
}
