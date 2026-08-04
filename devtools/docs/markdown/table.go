package markdown

import (
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
	}
	return row
}
