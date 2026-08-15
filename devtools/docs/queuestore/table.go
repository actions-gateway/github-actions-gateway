package main

import (
	"fmt"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// Column counts, which are how a row is told apart from a malformed one. The
// Queue carries St and the Deferred table does not, which is what deferred
// means.
const (
	queueColumns    = 6 // ID | Item | Labels | St | Sz | Notes
	deferredColumns = 5 // ID | Item | Labels | Sz | Trigger to revive
)

// ImportStatus reads the Queue and Deferred tables out of a backlog file and
// returns their items in table order, ranked by that order.
//
// Scoped to the two `## ` sections rather than to every `<a id=` in the file:
// the Progress table carries a row anchor too (Q248), and walking anchors alone
// would import it as a backlog item.
//
// Cells come off the GFM table AST, never off a split on `|`. A Queue row
// splits into six, seven or eight pipe-delimited fields depending on what its
// cells contain, which is the Q613 finding that put the other Markdown gates on
// this same parse layer.
func ImportStatus(src []byte) ([]Item, error) {
	doc := markdown.Parse(src)

	headings := doc.Headings()
	section := func(line int) string {
		name := ""
		for _, h := range headings {
			if h.Level != 2 || h.Line >= line {
				continue
			}
			name = h.Text
		}
		return name
	}

	var items []Item
	for _, t := range doc.Tables() {
		var deferred bool
		switch name := section(t.Line); {
		case strings.HasPrefix(name, "Queue"):
			deferred = false
		case strings.HasPrefix(name, "Deferred"):
			deferred = true
		default:
			continue
		}
		for _, r := range t.Rows {
			it, err := itemFromRow(r, deferred)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", r.Line, err)
			}
			items = append(items, it)
		}
	}
	if err := AssignRanks(items); err != nil {
		return nil, err
	}
	return items, nil
}

// itemFromRow reads one table row into an item.
func itemFromRow(r markdown.Row, deferred bool) (Item, error) {
	want := queueColumns
	if deferred {
		want = deferredColumns
	}
	if len(r.Cells) != want {
		return Item{}, fmt.Errorf("row has %d cells, want %d", len(r.Cells), want)
	}

	cell := func(i int) string { return strings.TrimSpace(r.Cells[i]) }

	it := Item{
		// Text resolves `<a id="Q1"></a>Q1` to `Q1`, which is the ID value
		// rather than its markup.
		ID:     strings.TrimSpace(r.Text[0]),
		Labels: parseLabels(cell(2)),
	}
	it.Title, it.Target = splitItemCell(cell(1))

	if deferred {
		it.Status = StatusDeferred
		it.Size = cell(3)
		it.Notes = cell(4)
		return it, nil
	}

	status, ok := glyphs[cell(3)]
	if !ok {
		return Item{}, fmt.Errorf("%s: St cell %q is not a Queue status glyph", it.ID, cell(3))
	}
	it.Status = status
	it.Size = cell(4)
	it.Notes = cell(5)
	return it, nil
}

// splitItemCell decomposes an Item cell into its title and link target. A cell
// that is not exactly one link keeps its whole text as the title and no target,
// which two of the live Deferred rows need.
func splitItemCell(cell string) (title, target string) {
	if !strings.HasPrefix(cell, "[") || !strings.HasSuffix(cell, ")") {
		return cell, ""
	}
	depth := 0
	for i := 0; i < len(cell); i++ {
		switch cell[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth != 0 {
				continue
			}
			// The link's text ends here; `](` has to follow for the rest of the
			// cell to be its destination.
			if i+1 >= len(cell) || cell[i+1] != '(' {
				return cell, ""
			}
			return cell[1:i], cell[i+2 : len(cell)-1]
		}
	}
	return cell, ""
}

// parseLabels reads the backtick-delimited label list.
func parseLabels(cell string) []string {
	var out []string
	for _, f := range strings.Fields(cell) {
		out = append(out, strings.Trim(f, "`"))
	}
	return out
}

// RenderRows renders the items belonging to one table, in rank order.
func RenderRows(items []Item, deferred bool) []string {
	var picked []Item
	for _, it := range items {
		if it.Deferred() == deferred {
			picked = append(picked, it)
		}
	}
	SortItems(picked)
	rows := make([]string, 0, len(picked))
	for _, it := range picked {
		rows = append(rows, it.Row())
	}
	return rows
}
