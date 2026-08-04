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
	// HasLink reports whether the item contains a link.
	HasLink bool
	// Line is the 1-based source line the item starts on.
	Line int
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
	item := ListItem{Text: d.inlineText(n), Lead: d.leadStrong(n), Line: d.nodeLine(n)}
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
	return item
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
