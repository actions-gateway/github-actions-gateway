package markdown

import (
	"bytes"
	"regexp"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// The docs are rendered by MkDocs Material, which enables two Python-Markdown
// extensions that CommonMark has no equivalent for. Both hide links from a
// stock parser, and both were checked by the awk this package replaces —
// measured on this repo: 19 links across four pages.
//
//   - `admonition`: a `!!! note "Title"` line plus a four-space-indented body,
//     which CommonMark reads as an indented code block.
//   - `md_in_html`: an HTML element carrying `markdown="1"`/`"span"`/`"block"`
//     whose content is Markdown, which CommonMark reads as raw HTML.
//
// MkDocsDialect parses both the way the site renders them. `markdown="0"` is
// deliberately not matched: it means "do not parse", which is what an ordinary
// HTML block already does.
var MkDocsDialect goldmark.Extender = &mkdocsDialect{}

type mkdocsDialect struct{}

func (e *mkdocsDialect) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithBlockParsers(
		// Admonitions sit below the indented-code parser (500) and above
		// blockquote (800); md-in-html above the HTML block parser (900),
		// which would otherwise swallow the element whole.
		util.Prioritized(&admonitionParser{}, 799),
		util.Prioritized(&mdInHTMLParser{}, 890),
	))
}

// --- admonitions -----------------------------------------------------------

// `!!!` is the always-open form, `???`/`???+` the collapsible ones. The type
// word and the optional quoted title carry no links, so the marker line is
// consumed whole.
var admonitionMarker = regexp.MustCompile(`^(?:!!!|\?\?\?\+?)[ \t]+[A-Za-z][\w-]*`)

var admonitionKind = ast.NewNodeKind("Admonition")

type admonitionNode struct {
	ast.BaseBlock
}

func (n *admonitionNode) Kind() ast.NodeKind { return admonitionKind }

func (n *admonitionNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

type admonitionParser struct{}

func (p *admonitionParser) Trigger() []byte { return []byte{'!', '?'} }

func (p *admonitionParser) Open(_ ast.Node, reader text.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	if w > 3 || !admonitionMarker.Match(line[pos:]) {
		return nil, parser.NoChildren
	}
	reader.Advance(len(line) - 1) // consume the marker line, not its newline
	return &admonitionNode{}, parser.HasChildren
}

func (p *admonitionParser) Continue(_ ast.Node, reader text.Reader, _ parser.Context) parser.State {
	line, _ := reader.PeekLine()
	if util.IsBlank(line) {
		return parser.Continue | parser.HasChildren
	}
	if w, _ := util.IndentWidth(line, reader.LineOffset()); w < 4 {
		return parser.Close
	}
	// Strip one indent level so the body parses as top-level block content.
	pos, padding := util.IndentPosition(line, reader.LineOffset(), 4)
	reader.AdvanceAndSetPadding(pos, padding)
	return parser.Continue | parser.HasChildren
}

func (p *admonitionParser) Close(_ ast.Node, _ text.Reader, _ parser.Context) {}

func (p *admonitionParser) CanInterruptParagraph() bool { return true }

// CanAcceptIndentedLine is false so a `!!! note` shown inside an indented code
// block stays code.
func (p *admonitionParser) CanAcceptIndentedLine() bool { return false }

// --- markdown="…" in HTML --------------------------------------------------

var mdInHTMLOpen = regexp.MustCompile(`^<([a-zA-Z][\w-]*)(?:\s[^>]*)?\smarkdown\s*=\s*"(?:1|span|block)"[^>]*>`)

var mdInHTMLKind = ast.NewNodeKind("MdInHTML")

type mdInHTMLNode struct {
	ast.BaseBlock
	// closing is the element's end tag; the container ends at the line that
	// starts with it, or immediately when the opening line already carried it.
	closing []byte
	oneLine bool
}

func (n *mdInHTMLNode) Kind() ast.NodeKind { return mdInHTMLKind }

func (n *mdInHTMLNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

type mdInHTMLParser struct{}

func (p *mdInHTMLParser) Trigger() []byte { return []byte{'<'} }

func (p *mdInHTMLParser) Open(_ ast.Node, reader text.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	if w > 3 {
		return nil, parser.NoChildren
	}
	m := mdInHTMLOpen.FindSubmatch(line[pos:])
	if m == nil {
		return nil, parser.NoChildren
	}
	closing := append([]byte("</"), append(m[1], '>')...)
	rest := line[pos+len(m[0]):]
	// Consume the opening tag only; whatever follows it on the line is
	// Markdown and belongs to the children.
	reader.Advance(pos + len(m[0]))
	return &mdInHTMLNode{closing: closing, oneLine: bytes.Contains(rest, closing)}, parser.HasChildren
}

func (p *mdInHTMLParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	n := node.(*mdInHTMLNode)
	if n.oneLine {
		return parser.Close
	}
	line, _ := reader.PeekLine()
	if _, pos := util.IndentWidth(line, reader.LineOffset()); bytes.HasPrefix(line[pos:], n.closing) {
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (p *mdInHTMLParser) Close(_ ast.Node, _ text.Reader, _ parser.Context) {}

func (p *mdInHTMLParser) CanInterruptParagraph() bool { return true }

func (p *mdInHTMLParser) CanAcceptIndentedLine() bool { return false }
