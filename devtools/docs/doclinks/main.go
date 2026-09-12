// Command doclinks checks Markdown relative links and heading anchors. It is
// the checker behind scripts/docs/check-doc-links.sh (Q52), which selects the
// files and the existence oracle and hands both to this program (Q612).
//
// It fails on:
//
//  1. Dead relative file links — a link, image or reference definition whose
//     resolved path is neither a present file nor a present directory.
//  2. Dead anchors — a `#fragment` (same-page or cross-doc) matching no
//     heading slug and no explicit `<a id="…">`/`<a name="…">` in the target.
//  3. Unanchored section references — a bare `.md` file link followed in prose
//     by `§`, which documentation-standards.md forbids because it renders like
//     the anchored form and nothing checks it (Q686). Rule 2 is what makes it
//     worth forbidding: move the section into the destination and every later
//     rename is caught, where the prose form is verbatim the day it is written
//     and silently wrong afterwards.
//  4. Dead links in named YAML frontmatter fields, when `-frontmatter-keys`
//     names them (Q1081). A backlog row's `target:` is a relative path with an
//     optional heading anchor, written where no Markdown parser looks, so a
//     one-character anchor typo passed every gate that runs on a pull request.
//     The keys are a flag rather than a constant so this program stays a link
//     checker: which frontmatter fields hold links is the caller's knowledge,
//     and check-doc-links.sh is where the backlog store is already known about.
//
// Out of scope, deliberately: external URLs (http/https/mailto/tel and every
// other scheme, which is what an autolink always is), links inside fenced or
// inline code, and anchors in non-Markdown or vendored targets. A trailing
// `:NN` / `:NN-MM` line reference on a file link (`provisioner.go:42`) is
// tolerated — only the file part is resolved.
//
// Usage:
//
//	doclinks -root <repo-root> -exist-file <paths> [-frontmatter-keys k1,k2] <file.md>...
//
// Findings print as `file:line: message`, or as GitHub `::error::` annotations
// when GITHUB_ACTIONS is set. Exits 1 if anything is broken.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark/ast"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

func main() {
	root := flag.String("root", ".", "repo root that link paths resolve against")
	existFile := flag.String("exist-file", "", "file listing the paths that exist, one per line")
	fmKeys := flag.String("frontmatter-keys", "", "comma-separated YAML frontmatter keys whose values are links")
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	broken, err := run(*root, *existFile, splitKeys(*fmKeys), flag.Args(), out, os.Getenv("GITHUB_ACTIONS") != "")
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "doclinks: %v\n", err)
		os.Exit(2)
	}
	if broken > 0 {
		os.Exit(1)
	}
}

// run checks every file and reports how many broken links and anchors it
// found. Paths are repo-relative, as they appear in the output; root is where
// they are read from.
// splitKeys parses the -frontmatter-keys flag, dropping empty entries so a
// trailing comma or an unset flag yields no keys rather than one empty one.
func splitKeys(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func run(root, existFile string, fmKeys, files []string, out io.Writer, gha bool) (int, error) {
	if existFile == "" {
		return 0, fmt.Errorf("-exist-file is required")
	}
	exists, err := readExisting(existFile)
	if err != nil {
		return 0, err
	}

	c := &checker{root: root, exists: exists, anchors: map[string]map[string]bool{}, fmKeys: fmKeys}
	for _, f := range files {
		if err := c.scan(f); err != nil {
			return 0, err
		}
	}
	// Anchors of every file must be known before any cross-file anchor can be
	// resolved, so validation is a second pass.
	for _, l := range c.links {
		c.validate(l)
	}

	for _, f := range c.findings {
		if gha {
			_, _ = fmt.Fprintf(out, "::error file=%s,line=%d::%s\n", f.file, f.line, f.msg)
		} else {
			_, _ = fmt.Fprintf(out, "%s:%d: %s\n", f.file, f.line, f.msg)
		}
	}
	if n := len(c.findings); n > 0 {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		_, _ = fmt.Fprintf(out, "check-doc-links: FAILED — %d broken link/anchor%s\n", n, plural)
		return n, nil
	}
	_, _ = fmt.Fprintf(out, "check-doc-links: ok (%d markdown files, %d links/anchors checked)\n", len(files), len(c.links))
	return 0, nil
}

type link struct {
	src    string
	line   int
	target string
}

type finding struct {
	file string
	line int
	msg  string
}

type checker struct {
	root     string
	exists   map[string]bool
	anchors  map[string]map[string]bool
	links    []link
	findings []finding
	// fmKeys are the YAML frontmatter keys whose values are links (Q1081).
	fmKeys []string
}

// scan parses one file, registering its anchors and queuing its links.
func (c *checker) scan(file string) error {
	src, err := os.ReadFile(filepath.Join(c.root, file))
	if err != nil {
		return err
	}
	doc := markdown.Parse(src)

	anchors := map[string]bool{}
	for _, h := range doc.Headings() {
		anchors[h.Slug] = true
	}
	for _, a := range doc.HTMLAnchors() {
		anchors[a.ID] = true
	}
	c.anchors[file] = anchors

	for _, l := range doc.Links() {
		// An autolink is a scheme URL or an email by construction — external,
		// which this gate does not resolve.
		if l.Kind == markdown.KindAutoLink {
			continue
		}
		c.links = append(c.links, link{src: file, line: l.Line, target: l.Destination})
	}
	c.checkSectionRefs(file, doc)
	c.scanFrontmatter(file, src)
	return nil
}

// frontmatterKey matches a top-level `key: value` line inside a frontmatter
// block: no leading space, so a key nested under another one is not read as a
// field of the document.
var frontmatterKey = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):[ \t]*(.*)$`)

// scanFrontmatter records the value of each -frontmatter-keys field as a link.
// The block is the YAML between a leading `---` line and the next one; a file
// that does not open with `---` has none, which is every document but a backlog
// row. Values are recorded verbatim and resolved by the same pass that resolves
// Markdown links, so the anchor rules are identical by construction rather than
// by a second implementation that can drift from the first.
func (c *checker) scanFrontmatter(file string, src []byte) {
	if len(c.fmKeys) == 0 {
		return
	}
	lines := strings.Split(string(src), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return
	}
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "---" {
			return
		}
		m := frontmatterKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !slices.Contains(c.fmKeys, m[1]) {
			continue
		}
		// Quotes are the one YAML scalar decoration a path is plausibly written
		// with; anything richer is not a link and resolving it would be a guess.
		v := strings.Trim(strings.TrimSpace(m[2]), `"'`)
		if v == "" {
			continue
		}
		c.links = append(c.links, link{src: file, line: i + 1, target: v})
	}
}

// sectionMark is the character the convention uses for a section citation. A
// bare `[file.md](file.md) § Name` renders like the anchored form and rule 2
// never sees the section, so it is the one cross-file reference that rots
// silently.
const sectionMark = "§"

// checkSectionRefs reports a file link followed by a section citation in prose.
//
// Walked here rather than read off Document.Links: the rule is about what
// *follows* a link, which is a relation between siblings that the package's
// flat link list cannot express, and its doc comment names this as the case for
// walking the AST directly.
//
// A code span carries its content as a source segment rather than as child
// nodes, so `[testing.md](testing.md) § X` written as an example is not a Link
// at all and never reaches here. That is what lets documentation-standards.md
// state the rule using the very form it forbids, and the tests assert it.
func (c *checker) checkSectionRefs(file string, doc *markdown.Document) {
	_ = ast.Walk(doc.Root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		l, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		dest := string(l.Destination)
		// Only a cross-file Markdown link can rot this way. One that already
		// carries a fragment is the anchored form the convention asks for, and
		// an external target is not this gate's to resolve.
		if !strings.HasSuffix(dest, ".md") || strings.Contains(dest, "#") ||
			schemeRE.MatchString(dest) {
			return ast.WalkContinue, nil
		}
		next, ok := n.NextSibling().(*ast.Text)
		if !ok {
			return ast.WalkContinue, nil
		}
		if !strings.HasPrefix(strings.TrimLeft(string(next.Segment.Value(doc.Source)), " \t"), sectionMark) {
			return ast.WalkContinue, nil
		}
		// The citation's own offset, which is on the link's line in every real
		// instance and is reachable through the package's exported Line.
		c.report(file, doc.Line(next.Segment.Start), "unanchored section reference: "+dest+
			" followed by "+sectionMark+
			" - cite the section in the destination, `[page.md "+sectionMark+
			" Heading](page.md#heading)`, so a rename is caught"+
			" (docs/development/documentation-standards.md#conventions)")
		return ast.WalkContinue, nil
	})
}

var (
	schemeRE  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	lineRefRE = regexp.MustCompile(`:[0-9]+(-[0-9]+)?$`)
)

func (c *checker) validate(l link) {
	t := l.target
	switch {
	case t == "":
		return
	case schemeRE.MatchString(t), strings.HasPrefix(t, "mailto:"), strings.HasPrefix(t, "tel:"):
		return
	case strings.HasPrefix(t, "#"):
		c.checkAnchor(l, l.src, t[1:])
		return
	}

	p, anchor := t, ""
	if i := strings.Index(t, "#"); i >= 0 {
		p, anchor = t[:i], t[i+1:]
	}
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	p = lineRefRE.ReplaceAllString(p, "")
	if p == "" {
		c.checkAnchor(l, l.src, anchor)
		return
	}

	resolved := resolve(l.src, p)
	bare := strings.TrimSuffix(resolved, "/")
	if !c.exists[bare] && !c.exists[resolved] {
		where := resolved
		if where == "" {
			where = "(outside repo)"
		}
		c.report(l.src, l.line, "dead link: "+t+" -> "+where)
		return
	}
	if anchor != "" && strings.HasSuffix(bare, ".md") {
		if _, scanned := c.anchors[bare]; scanned {
			c.checkAnchor(l, bare, anchor)
		}
	}
}

func (c *checker) checkAnchor(l link, target, anchor string) {
	if anchor == "" || c.anchors[target][anchor] {
		return
	}
	c.report(l.src, l.line, fmt.Sprintf(
		"dead anchor: %s -> #%s has no matching heading or <a id> in %s", l.target, anchor, target))
}

func (c *checker) report(file string, line int, msg string) {
	c.findings = append(c.findings, finding{file: file, line: line, msg: msg})
}

// resolve turns a link path into a repo-relative path: a leading `/` means the
// repo root, anything else is relative to the linking file's directory.
func resolve(srcFile, p string) string {
	if strings.HasPrefix(p, "/") {
		return normalize(p[1:])
	}
	// Concatenated then normalized, not path.Join'd: Join resolves `..`
	// against the directory before normalize sees it, which differs at the
	// root boundary.
	return normalize(path.Dir(srcFile) + "/" + p)
}

// normalize resolves `.`, `..` and empty segments without touching the disk.
// A `..` that climbs past the root leaves an empty path, which the caller
// reports as outside the repo.
func normalize(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// readExisting reads the path list the caller derived from git, and derives
// every ancestor directory from it so a link to a directory resolves too.
func readExisting(name string) (map[string]bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	exists := map[string]bool{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		p := s.Text()
		if p == "" {
			continue
		}
		exists[p] = true
		for i, ch := range p {
			if ch == '/' {
				exists[p[:i]] = true
			}
		}
	}
	return exists, s.Err()
}
