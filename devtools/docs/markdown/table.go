package markdown

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

// Row is one GFM table row.
type Row struct {
	// Cells holds each cell's source text, trimmed of surrounding space and
	// with `\|` left as written — an escape is two source characters, which is
	// what a length budget counted against the source has to see.
	//
	// A row with fewer cells than the header is padded with empty ones and a
	// row with more is truncated, as GFM renders it. An unescaped `|` therefore
	// shifts the cells after it and drops the tail, which is loud: the rules
	// downstream read a cell holding something else entirely.
	Cells []string
	// Text holds each cell's rendered inline text, parallel to Cells: a link
	// contributes its text and not its destination, a code span its contents,
	// an HTML tag nothing. A backlog row's `<a id="Q1"></a>Q1` reads as `Q1`,
	// which is how a rule about the ID *value* asks for it.
	Text []string
	// Line is the 1-based source line the row starts on.
	Line int
}

// Table is one GFM table. A table has a header by construction — GFM requires
// the delimiter row — so Header is always populated.
type Table struct {
	Header Row
	Rows   []Row
	// Line is the 1-based source line the header row starts on.
	Line int
}

// Tables returns every GFM table in the document, in source order. Pair a
// table's Line with Headings to find the section it sits under.
func (d *Document) Tables() []Table {
	var tables []Table
	_ = ast.Walk(d.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		t, ok := n.(*east.Table)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		var tbl Table
		for c := t.FirstChild(); c != nil; c = c.NextSibling() {
			switch v := c.(type) {
			case *east.TableHeader:
				tbl.Header = d.tableRow(v)
				tbl.Line = tbl.Header.Line
			case *east.TableRow:
				tbl.Rows = append(tbl.Rows, d.tableRow(v))
			}
		}
		tables = append(tables, tbl)
		return ast.WalkSkipChildren, nil
	})
	return tables
}

// tableRow reads one header or body row's cells.
func (d *Document) tableRow(n ast.Node) Row {
	row := Row{Line: d.Line(n.Pos())}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		cell, ok := c.(*east.TableCell)
		if !ok {
			continue
		}
		text := ""
		// A padded cell carries no source segment.
		if lines := cell.Lines(); lines.Len() > 0 {
			seg := lines.At(0)
			text = string(seg.Value(d.Source))
		}
		row.Cells = append(row.Cells, text)
		row.Text = append(row.Text, strings.TrimSpace(d.inlineText(cell)))
	}
	return row
}

// ParseRow reads a single GFM table row — a line like `| a | b |` — that has no
// table around it. It exists for a caller holding one out of context: a diff
// hunk's `+`/`-` line, where the marker is the caller's to strip. Reports false
// when the line is not a table row.
//
// A lone row is an ordinary paragraph to a Markdown parser, so it is parsed as
// the header of a synthesized table. GFM recognizes that table only when the
// delimiter is at least as wide as the row (a short header is padded, a long
// one rejected), which makes acceptance monotone in the width and the narrowest
// accepted width the row's own cell count. Binary-searching that width leaves
// every escaping rule to goldmark instead of restating it here.
func ParseRow(line string) (Row, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.Contains(line, "|") {
		return Row{}, false
	}
	// Unescaped pipes are a subset of all pipes, so this width always parses if
	// any does.
	lo, hi := 1, strings.Count(line, "|")+1
	if _, ok := rowAtWidth(line, hi); !ok {
		return Row{}, false
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if _, ok := rowAtWidth(line, mid); ok {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return rowAtWidth(line, lo)
}

// rowAtWidth parses line as the header of a table whose delimiter has width
// columns, reporting false when GFM does not recognize the pair as a table.
func rowAtWidth(line string, width int) (Row, bool) {
	src := line + "\n|" + strings.Repeat("---|", width) + "\n"
	tables := Parse([]byte(src)).Tables()
	if len(tables) != 1 {
		return Row{}, false
	}
	return tables[0].Header, true
}
