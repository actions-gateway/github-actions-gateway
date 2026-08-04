// Package markdown reads Markdown the way the docs gates need it: links,
// headings with their GitHub anchor slugs, and explicit HTML anchors, each
// carrying the source line the gate reports.
//
// It exists because regular expressions cannot count brackets. The awk it
// replaces collected links with `\[[^]]*\]\([^)]*\)`, which matches the inner
// image of `[![badge](img)](target)` and so never sees the outer destination —
// a shape the README uses today (Q612). Parsing is delegated to goldmark with
// the GitHub Flavored Markdown extensions enabled, so what the gates read is
// what GitHub renders.
//
// Root and Line are exported so a gate needing a construct this package does
// not model (GFM table rows, for one) can walk the AST itself and still report
// `file:line:`.
package markdown

import (
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// LinkKind distinguishes the constructs a destination can come from.
type LinkKind int

const (
	// KindInline is a `[text](target)` link, including one produced by a
	// resolved `[text][label]` reference.
	KindInline LinkKind = iota
	// KindImage is the source of an `![alt](target)` image.
	KindImage
	// KindAutoLink is a `<https://example.com>` or GFM-linkified destination.
	KindAutoLink
	// KindReferenceDefinition is the target of a `[label]: target` definition.
	KindReferenceDefinition
)

// Link is one destination found in a document.
type Link struct {
	// Destination is the target as written, with the parser's escapes and
	// angle-bracket wrapping already resolved.
	Destination string
	// Line is the 1-based source line the gate reports the link on.
	Line int
	Kind LinkKind
}

// Heading is one heading and the GitHub anchor it publishes.
type Heading struct {
	Text  string
	Slug  string
	Level int
	Line  int
}

// HTMLAnchor is an explicit `<a id="…">` / `<a name="…">` anchor.
type HTMLAnchor struct {
	ID   string
	Line int
}

// Document is a parsed Markdown source.
type Document struct {
	// Source is the input the offsets in Root index into.
	Source []byte
	// Root is the goldmark AST root.
	Root ast.Node

	ctx        parser.Context
	lineStarts []int
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM, MkDocsDialect))

// Parse builds a Document from Markdown source. goldmark reports no parse
// errors — any byte sequence is valid Markdown — so there is no error return.
func Parse(src []byte) *Document {
	ctx := parser.NewContext()
	root := md.Parser().Parse(text.NewReader(src), parser.WithContext(ctx))
	return &Document{Source: src, Root: root, ctx: ctx, lineStarts: lineStarts(src)}
}

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// Line maps a byte offset into Source to its 1-based line number.
func (d *Document) Line(offset int) int {
	return sort.SearchInts(d.lineStarts, offset+1)
}

// Links returns every destination in the document, in source order: inline
// links and images, autolinks, and reference definitions. Nesting is not
// flattening — `[![badge](img)](target)` yields both the image and the outer
// link.
func (d *Document) Links() []Link {
	var links []Link
	ast.Walk(d.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			links = append(links, Link{string(v.Destination), d.nodeLine(n), KindInline})
		case *ast.Image:
			links = append(links, Link{string(v.Destination), d.nodeLine(n), KindImage})
		case *ast.AutoLink:
			links = append(links, Link{string(v.URL(d.Source)), d.nodeLine(n), KindAutoLink})
		}
		return ast.WalkContinue, nil
	})
	links = append(links, d.referenceDefinitions()...)
	sort.SliceStable(links, func(i, j int) bool { return links[i].Line < links[j].Line })
	return links
}

// referenceDefinitions returns the `[label]: target` definitions. goldmark
// consumes them into the parse context rather than the AST, so a definition
// whose label is never used has no node to take a line from — it is located by
// matching its label against the source.
func (d *Document) referenceDefinitions() []Link {
	refs := d.ctx.References()
	if len(refs) == 0 {
		return nil
	}
	lines := make(map[string]int, len(refs))
	for i, start := range d.lineStarts {
		end := len(d.Source)
		if i+1 < len(d.lineStarts) {
			end = d.lineStarts[i+1]
		}
		if m := refDefRE.FindSubmatch(d.Source[start:end]); m != nil {
			label := normalizeRefLabel(string(m[1]))
			if _, seen := lines[label]; !seen {
				lines[label] = i + 1
			}
		}
	}
	defs := make([]Link, 0, len(refs))
	for _, ref := range refs {
		defs = append(defs, Link{
			Destination: string(ref.Destination()),
			Line:        lines[normalizeRefLabel(string(ref.Label()))],
			Kind:        KindReferenceDefinition,
		})
	}
	return defs
}

var (
	refDefRE  = regexp.MustCompile(`^ {0,3}\[([^\]^][^\]]*)\]:`)
	htmlIDRE  = regexp.MustCompile(`(?is)<a\s[^>]*\b(?:id|name)\s*=\s*("[^"]*"|'[^']*')`)
	spaceRunR = regexp.MustCompile(`\s+`)
)

// normalizeRefLabel applies CommonMark label matching: case-folded, with
// internal whitespace runs collapsed to a single space.
func normalizeRefLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(spaceRunR.ReplaceAllString(label, " ")))
}

// Headings returns every heading with its GitHub anchor slug, de-duplicated
// across the document the way github-slugger does.
func (d *Document) Headings() []Heading {
	slugger := NewSlugger()
	var headings []Heading
	ast.Walk(d.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		txt := d.inlineText(h)
		headings = append(headings, Heading{
			Text:  txt,
			Slug:  slugger.Slug(txt),
			Level: h.Level,
			Line:  d.nodeLine(h),
		})
		return ast.WalkSkipChildren, nil
	})
	return headings
}

// HTMLAnchors returns the `<a id="…">` anchors in the document's raw HTML.
// Markup inside a code block is a code block to the parser, so it contributes
// no anchor.
func (d *Document) HTMLAnchors() []HTMLAnchor {
	var anchors []HTMLAnchor
	collect := func(seg text.Segment) {
		raw := seg.Value(d.Source)
		for _, m := range htmlIDRE.FindAllSubmatchIndex(raw, -1) {
			id := string(raw[m[2]+1 : m[3]-1]) // strip the quote pair
			anchors = append(anchors, HTMLAnchor{ID: id, Line: d.Line(seg.Start + m[0])})
		}
	}
	ast.Walk(d.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.RawHTML:
			for i := 0; i < v.Segments.Len(); i++ {
				collect(v.Segments.At(i))
			}
		case *ast.HTMLBlock:
			for i := 0; i < v.Lines().Len(); i++ {
				collect(v.Lines().At(i))
			}
			if v.HasClosure() {
				collect(v.ClosureLine)
			}
		}
		return ast.WalkContinue, nil
	})
	return anchors
}

// inlineText renders a node's inline children the way a heading slug sees
// them: literal text, code-span contents and autolink URLs, with tags and
// attributes of embedded HTML dropped as a browser drops them.
func (d *Document) inlineText(n ast.Node) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(d.Source))
		case *ast.String:
			b.Write(v.Value)
		case *ast.AutoLink:
			b.Write(v.URL(d.Source))
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// nodeLine reports the line a node starts on. Inline nodes carry no offset of
// their own, so the first text segment at or after the node is used, falling
// back to the enclosing block's first line for a node with no text at all
// (`[](target)`).
func (d *Document) nodeLine(n ast.Node) int {
	if seg, ok := firstSegment(n); ok {
		return d.Line(seg.Start)
	}
	for p := n; p != nil; p = p.Parent() {
		for s := p.NextSibling(); s != nil; s = s.NextSibling() {
			if seg, ok := firstSegment(s); ok {
				return d.Line(seg.Start)
			}
		}
		// Only a block node carries source lines; asking an inline node panics.
		if p.Type() == ast.TypeBlock {
			if lines := p.Lines(); lines != nil && lines.Len() > 0 {
				return d.Line(lines.At(0).Start)
			}
		}
	}
	return 1
}

// firstSegment finds the first source segment at or below n.
func firstSegment(n ast.Node) (text.Segment, bool) {
	var found text.Segment
	ok := false
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || ok {
			return ast.WalkContinue, nil
		}
		switch v := c.(type) {
		case *ast.Text:
			found, ok = v.Segment, true
		case *ast.RawHTML:
			if v.Segments.Len() > 0 {
				found, ok = v.Segments.At(0), true
			}
		}
		if ok {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found, ok
}
