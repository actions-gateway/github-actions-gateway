package markdown

import (
	"strings"
	"testing"
)

// cells renders the first table's body rows as "line:cell|cell|…", the shape a
// gate reading fixed columns depends on.
func cells(t *testing.T, src string) []string {
	t.Helper()
	tables := Parse([]byte(src)).Tables()
	if len(tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(tables))
	}
	var got []string
	for _, r := range tables[0].Rows {
		got = append(got, strings.Join(r.Cells, "\x1f"))
	}
	return got
}

const header = "| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n"

func TestTableCells(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{{
		// The Q613 defect. Splitting on a literal `|` gives nine fields here,
		// so St reads the label cell and Notes reads the size.
		name: "escaped pipe stays inside its cell",
		src:  header + `| Q1 | Item | ` + "`ci`" + ` | 🔲 | S | ` + "`make check \\| tail`" + ` lies |` + "\n",
		want: []string{"Q1\x1fItem\x1f`ci`\x1f🔲\x1fS\x1f`make check \\| tail` lies"},
	}, {
		name: "control row with no escaped pipe",
		src:  header + "| Q2 | Item | `ci` | 🔲 | S | plain note |\n",
		want: []string{"Q2\x1fItem\x1f`ci`\x1f🔲\x1fS\x1fplain note"},
	}, {
		// An unescaped pipe is an authoring error, and GFM truncates the row at
		// the header's column count. The cells after it hold the wrong thing —
		// which is what makes the rules downstream fail loudly rather than pass
		// on a shifted cell.
		name: "unescaped pipe shifts and truncates, as GFM renders it",
		src:  header + "| Q3 | Item | pipe | `ci` | 🔲 | S | note |\n",
		want: []string{"Q3\x1fItem\x1fpipe\x1f`ci`\x1f🔲\x1fS"},
	}, {
		name: "short row is padded to the header width",
		src:  header + "| Q4 | Item | `ci` |\n",
		want: []string{"Q4\x1fItem\x1f`ci`\x1f\x1f\x1f"},
	}, {
		name: "cells are trimmed of surrounding space",
		src:  header + "|   Q5   | Item |   `ci` | 🔲 | S | note   |\n",
		want: []string{"Q5\x1fItem\x1f`ci`\x1f🔲\x1fS\x1fnote"},
	}, {
		// A link's source form is what the length budget is written against.
		name: "link syntax is preserved verbatim",
		src:  header + "| Q6 | [Item](plan/x.md) | `ci` | 🔲 | S | see [Q7](#Q7) |\n",
		want: []string{"Q6\x1f[Item](plan/x.md)\x1f`ci`\x1f🔲\x1fS\x1fsee [Q7](#Q7)"},
	}, {
		name: "html anchor in a cell survives as source",
		src:  header + `| <a id="Q8"></a>Q8 | Item | ` + "`ci`" + ` | 🔲 | S | note |` + "\n",
		want: []string{`<a id="Q8"></a>Q8` + "\x1fItem\x1f`ci`\x1f🔲\x1fS\x1fnote"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cells(t, tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d: %q", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d:\n got %q\nwant %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestTableHeaderAndLines(t *testing.T) {
	src := "# Doc\n\n## Queue\n\n" + header + "| Q1 | Item | `ci` | 🔲 | S | note |\n"
	tables := Parse([]byte(src)).Tables()
	if len(tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(tables))
	}
	if got, want := strings.Join(tables[0].Header.Cells, ","), "ID,Item,Labels,St,Sz,Notes"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	if got := tables[0].Line; got != 5 {
		t.Errorf("table line = %d, want 5", got)
	}
	if got := tables[0].Rows[0].Line; got != 7 {
		t.Errorf("row line = %d, want 7", got)
	}
}

// A table in a section is located by its line against the headings, which is
// how a gate scopes rules to one `## ` section.
func TestTablesAreOrderedForSectionLookup(t *testing.T) {
	src := "## Progress\n\n| Item | Status |\n|---|---|\n| A | ✅ |\n\n" +
		"## Queue\n\n" + header + "| Q1 | Item | `ci` | 🔲 | S | note |\n"
	doc := Parse([]byte(src))
	tables := doc.Tables()
	if len(tables) != 2 {
		t.Fatalf("want 2 tables, got %d", len(tables))
	}
	headings := doc.Headings()
	section := func(line int) string {
		name := ""
		for _, h := range headings {
			if h.Level == 2 && h.Line < line {
				name = h.Text
			}
		}
		return name
	}
	if got := section(tables[0].Line); got != "Progress" {
		t.Errorf("first table section = %q, want Progress", got)
	}
	if got := section(tables[1].Line); got != "Queue" {
		t.Errorf("second table section = %q, want Queue", got)
	}
}
