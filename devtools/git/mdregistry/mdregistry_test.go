package mdregistry

import "testing"

func TestLinkKey(t *testing.T) {
	tests := []struct{ name, line, want string }{
		{"a plain row keys on cell 1's link target", "| [a.md](a.md) | note |", "a.md"},
		{"surrounding whitespace is trimmed", "|   [a.md](docs/a.md)   | note |", "docs/a.md"},
		{"a link in a later cell is not the key", "| plain | [a.md](a.md) |", ""},
		{"a row whose first cell is not a link has no key", "| plain | note |", ""},
		{"a non-row has no key", "not a row", ""},
		{"a row with too few cells has no key", "| [a.md](a.md)", ""},
		// The whole reason the key reads cell 1 alone: an escaped pipe further
		// along must not shift it (Q613/Q614 are the same class elsewhere).
		{"an escaped pipe in a later cell cannot shift the key", `| [a.md](a.md) | uses \| a pipe | x |`, "a.md"},
		{"a badge-wrapped link takes the inner target, as the awk did", "| [![b](img.svg)](a.md) | note |", "img.svg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LinkKey(tc.line); got != tc.want {
				t.Errorf("LinkKey(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestSplitCarvesProseFromRows(t *testing.T) {
	doc, err := Split([]string{
		"# Title",
		"",
		"| Col | Note |",
		"|---|---|",
		"| [a.md](a.md) | one |",
		"| [b.md](b.md) | two |",
		"",
		"Between the tables.",
		"",
		"| Col | Note |",
		"|---|---|",
		"| [c.md](c.md) | three |",
		"",
		"Trailing prose.",
	})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(doc.Tables) != 2 {
		t.Fatalf("tables: want 2, got %d", len(doc.Tables))
	}
	if got := len(doc.Tables[0].Rows); got != 2 {
		t.Errorf("table 1 rows: want 2, got %d", got)
	}
	if got := len(doc.Tables[1].Rows); got != 1 {
		t.Errorf("table 2 rows: want 1, got %d", got)
	}
	// The header and its separator belong to the table's own prose segment, so
	// the rows the merge sees are data rows only.
	pre := doc.Tables[0].Pre
	if len(pre) < 2 || pre[len(pre)-1] != "|---|---|" {
		t.Errorf("table 1 pre must end at the column separator, got %q", pre)
	}
	if len(doc.Post) == 0 || doc.Post[len(doc.Post)-1] != "Trailing prose." {
		t.Errorf("post: want the tail after the last table, got %q", doc.Post)
	}
}

// A stray leading `|` is prose unless the next line is a column separator.
func TestSplitIgnoresAPipeLineThatIsNotAHeader(t *testing.T) {
	_, err := Split([]string{"| this looks like a row", "but this is prose"})
	if err != ErrNoTables {
		t.Errorf("want ErrNoTables, got %v", err)
	}
}

func TestSplitRefusesAPageWithNoTable(t *testing.T) {
	if _, err := Split([]string{"# Title", "", "Prose only."}); err != ErrNoTables {
		t.Errorf("want ErrNoTables, got %v", err)
	}
}

// Reconstruction is byte-exact: prose, header, separator, rows and tail put
// back in order must equal the input, or the driver would rewrite the file it
// was only asked to merge.
func TestSplitRoundTripsEveryLine(t *testing.T) {
	in := []string{
		"# Title", "", "| Col |", "|---|", "| [a.md](a.md) |", "", "Tail.",
	}
	doc, err := Split(in)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	var got []string
	for _, tbl := range doc.Tables {
		got = append(got, tbl.Pre...)
		got = append(got, tbl.Rows...)
	}
	got = append(got, doc.Post...)
	if len(got) != len(in) {
		t.Fatalf("round trip: want %d lines %q, got %d %q", len(in), in, len(got), got)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("line %d: want %q, got %q", i, in[i], got[i])
		}
	}
}
