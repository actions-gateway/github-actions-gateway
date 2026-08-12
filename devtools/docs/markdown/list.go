package markdown

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// ListItem is one item of a Markdown list, flattened to what a prose gate
// measures about a bullet.
type ListItem struct {
	// Text is the item's rendered inline text, continuation lines and nested
	// items included: a link contributes its text and not its destination, a
	// code span its contents, an HTML tag or comment nothing.
	Text string
	// Lead is the text of the strong-emphasis run the item opens with, empty
	// when it does not open with one. It is how a bullet titles itself.
	Lead string
	// Comments holds the item's HTML comments verbatim, in source order. A
	// comment inside a code fence is code, so it is not one of these.
	Comments []string
	// Raw is the item's verbatim source, continuation lines included. Inline
	// HTML is the reason it exists: Text drops tags the way a browser does, so
	// a gate reading a `<span class="…">label</span>` badge — which pairs a
	// class with the text between the tags — cannot get both from Text alone.
	// Prefer Text or Comments where they answer the question; a raw slice is
	// the last resort, not the first.
	Raw string
	// Destinations holds the item's link targets in source order, verbatim.
	Destinations []string
	// HasLink reports whether the item contains a link.
	HasLink bool
	// Line is the 1-based source line the item starts on.
	Line int
	// EndLine is the 1-based source line the item ends on. It exceeds Line
	// whenever the item's prose wraps or it carries a second block, which is
	// what a gate measuring an item has to name to be checkable by hand.
	EndLine int
	// ParagraphLines holds the 1-based start line of each block-level
	// paragraph the item contains, in source order. Prose that wraps is one
	// paragraph however many lines it spans, so more than one entry means a
	// blank line separates two blocks inside the item.
	ParagraphLines []int
}

// TopLevelListItems returns, in source order, the items of every list that is a
// direct child of the document. A nested list's items are part of the item that
// contains them rather than entries of their own.
func (d *Document) TopLevelListItems() []ListItem {
	var items []ListItem
	for n := d.Root.FirstChild(); n != nil; n = n.NextSibling() {
		list, ok := n.(*ast.List)
		if !ok {
			continue
		}
		for c := list.FirstChild(); c != nil; c = c.NextSibling() {
			if item, ok := c.(*ast.ListItem); ok {
				items = append(items, d.listItem(item))
			}
		}
	}
	return items
}

func (d *Document) listItem(n *ast.ListItem) ListItem {
	item := ListItem{
		Text:           d.inlineText(n),
		Lead:           d.leadStrong(n),
		Line:           d.nodeLine(n),
		ParagraphLines: d.paragraphLines(n),
	}
	item.EndLine = item.Line
	collect := func(seg text.Segment) {
		raw := strings.TrimSpace(string(seg.Value(d.Source)))
		if strings.HasPrefix(raw, "<!--") {
			item.Comments = append(item.Comments, raw)
		}
	}
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := c.(type) {
		case *ast.Link:
			item.HasLink = true
			item.Destinations = append(item.Destinations, string(v.Destination))
		case *ast.RawHTML:
			for i := 0; i < v.Segments.Len(); i++ {
				collect(v.Segments.At(i))
			}
		case *ast.HTMLBlock:
			for i := 0; i < v.Lines().Len(); i++ {
				collect(v.Lines().At(i))
			}
		}
		return ast.WalkContinue, nil
	})
	if start, stop := d.rawRange(n); start >= 0 {
		item.Raw = string(d.Source[start:stop])
		item.EndLine = d.Line(stop - 1)
	}
	return item
}

// paragraphLines reports where each of an item's block-level paragraphs
// starts. A tight list carries its prose in a TextBlock and a loose one in a
// Paragraph; both are the same block to a caller counting them. Nested lists,
// fences and tables are other kinds of block and are not counted.
func (d *Document) paragraphLines(n *ast.ListItem) []int {
	var lines []int
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case ast.KindParagraph, ast.KindTextBlock:
			lines = append(lines, d.nodeLine(c))
		}
	}
	return lines
}

// rawRange returns the byte range a node's own text occupies in Source, or
// (-1, -1) for a node with no text at all. Inline nodes carry their offsets in
// different fields from block nodes, so both are walked; the outer bounds of
// what is found are the item, since Markdown nests but does not interleave.
func (d *Document) rawRange(n ast.Node) (start, stop int) {
	start, stop = -1, -1
	consider := func(seg text.Segment) {
		if start < 0 || seg.Start < start {
			start = seg.Start
		}
		if seg.Stop > stop {
			stop = seg.Stop
		}
	}
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if c.Type() == ast.TypeBlock {
			lines := c.Lines()
			for i := 0; i < lines.Len(); i++ {
				consider(lines.At(i))
			}
		}
		switch v := c.(type) {
		case *ast.Text:
			consider(v.Segment)
		case *ast.RawHTML:
			for i := 0; i < v.Segments.Len(); i++ {
				consider(v.Segments.At(i))
			}
		}
		return ast.WalkContinue, nil
	})
	return start, stop
}

// leadStrong returns the text of a strong-emphasis run opening the item's first
// paragraph, skipping the raw HTML a bullet may lead with.
func (d *Document) leadStrong(n *ast.ListItem) string {
	first := n.FirstChild()
	if first == nil {
		return ""
	}
	for c := first.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.RawHTML:
			continue
		case *ast.Emphasis:
			if v.Level == 2 {
				return d.inlineText(v)
			}
			return ""
		default:
			return ""
		}
	}
	return ""
}
