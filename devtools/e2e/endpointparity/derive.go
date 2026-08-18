package main

// Caller-side derivation (Q871).
//
// The set of GitHub REST endpoints the AGC calls has to come from the source
// that calls them. A hand-kept list is the thing this gate exists to replace:
// Q811 added a run read and nothing re-walked the fake, so the e2e venue 404'd
// an endpoint the AGC had started depending on while the PR stayed green.
//
// Every outbound call is built at an http.NewRequest[WithContext] site, so the
// walk starts there and folds the URL expression into a path template. Four
// shapes carry a path to one of those sites in this tree:
//
//	literal    c.apiBase + "/actions/runner-registration"
//	format     fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s", base, owner, repo, id)
//	callee     r.runnerAPIPrefix() + "/actions/runners/generate-jitconfig"
//	opaque     a value the caller was handed — a param, a field, a method result
//
// Opaque parts fold to a hole rather than failing the fold, because a hole is
// still a probeable path: the AGC's own path segments are what parity is about,
// and the owner/repo/run-id in them are not. A site whose URL folds to holes
// ALONE composes no path — the URL came from config or from a server response,
// which is a URL the fake minted itself and cannot be out of parity with. Those
// are reported rather than dropped: an endpoint that stops being derived stops
// being demanded, so silent under-derivation would leave the gate green with
// the hole it exists to catch.
//
// Resolution runs into callees only, never up into callers. A parameter
// resolved one hop up would pull every caller's literal into the site, which is
// how the Vault signer's endpoint (githubapp/vaultsigner, not GitHub at all)
// would arrive looking like a GitHub path.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// calleeDepth bounds the walk into helper functions that return a path. One hop
// reaches every path in this tree and two is the deepest chain that exists:
// scaleset's registrationTokenPath returns a prefix its own helper built. Going
// deeper cannot invent a segment — a fold only ever concatenates what the source
// wrote — but it does cost a fixed point over mutually recursive helpers.
const calleeDepth = 2

// hole marks a segment the source does not fix: an owner, a repo, a run id, or
// a whole prefix a caller supplies. numHole is the same thing behind an integer
// verb, kept apart so a probe fills it with something that parses as one.
const (
	hole    = "{}"
	numHole = "{d}"
)

// callSite is one outbound HTTP request the source builds, as the template its
// URL expression folds to. Template is a path with holes; it is empty when the
// expression folded to holes alone.
type callSite struct {
	Method   string
	Template string
	File     string
	Line     int
	Func     string
}

// ID names the site the way a pin does: the function that builds the request,
// qualified by the file it lives in. Line numbers are deliberately not part of
// it — a pin must survive the file being edited around it.
func (c callSite) ID() string { return c.File + ":" + c.Func }

// composesPath reports whether the fold found any path the source itself wrote.
// A template with no "/" in a literal position is a URL the caller was handed
// whole, so there is no AGC-composed endpoint in it to hold the fake to.
func (c callSite) composesPath() bool {
	return strings.Contains(stripHoles(c.Template), "/")
}

// stripHoles removes the hole markers so what is left is only what the source
// spelled out.
func stripHoles(t string) string {
	return strings.NewReplacer(hole, "", numHole, "").Replace(t)
}

// srcFile is one parsed non-test Go file, kept with the path the report names.
type srcFile struct {
	path string
	file *ast.File
}

// parseRoots parses every non-test .go file under each root, skipping vendor
// trees. Test files are excluded: a test's own stub URLs are not endpoints the
// AGC calls in production, and folding them in would demand the fake serve
// paths no deployed binary ever asks for.
func parseRoots(fset *token.FileSet, roots []string) ([]srcFile, error) {
	var out []srcFile
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			out = append(out, srcFile{path: filepath.ToSlash(path), file: f})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// resolver folds expressions to templates against the parsed tree. Functions are
// indexed by name only, not by receiver type: a callee whose name is unique in
// this tree resolves, and two same-named callees pool their returns, which can
// only add templates. An added template is a path the gate then demands the fake
// serve — visible as a failure naming the path, never a silent pass.
type resolver struct {
	funcs map[string][]*ast.FuncDecl
}

func newResolver(files []srcFile) *resolver {
	r := &resolver{funcs: map[string][]*ast.FuncDecl{}}
	for _, sf := range files {
		for _, decl := range sf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			r.funcs[fn.Name.Name] = append(r.funcs[fn.Name.Name], fn)
		}
	}
	return r
}

// deriveSites folds every http.NewRequest / http.NewRequestWithContext site in
// the tree. Locals are resolved within the enclosing function, which is where
// every URL in this tree is assembled before the request is built.
func deriveSites(files []srcFile, fset *token.FileSet) []callSite {
	r := newResolver(files)
	var sites []callSite
	for _, sf := range files {
		for _, decl := range sf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			locals := collectLocals(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isRequestCtor(call.Fun) {
					return true
				}
				method, url := requestArgs(call)
				if url == nil {
					return true
				}
				sites = append(sites, callSite{
					Method:   r.foldMethod(method, locals),
					Template: r.fold(url, locals, calleeDepth),
					File:     sf.path,
					Line:     fset.Position(call.Pos()).Line,
					Func:     fn.Name.Name,
				})
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}

// isRequestCtor reports whether fun names http.NewRequest or
// http.NewRequestWithContext. Matching on the selector rather than the resolved
// package keeps the walk type-free; a local named http shadowing the import
// would be caught by the derivation report, not silently mis-folded, because
// its call would not carry a method constant in the expected position.
func isRequestCtor(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "http" {
		return false
	}
	return sel.Sel.Name == "NewRequest" || sel.Sel.Name == "NewRequestWithContext"
}

// requestArgs picks the method and url arguments out of either constructor.
// NewRequest is (method, url, body); NewRequestWithContext is (ctx, method, url, body).
func requestArgs(call *ast.CallExpr) (method, url ast.Expr) {
	sel := call.Fun.(*ast.SelectorExpr)
	off := 0
	if sel.Sel.Name == "NewRequestWithContext" {
		off = 1
	}
	if len(call.Args) < off+2 {
		return nil, nil
	}
	return call.Args[off], call.Args[off+1]
}

// collectLocals maps each single-assignment local in fn to its value. A name
// assigned more than once maps to nil, which folds to a hole: the walk has no
// flow analysis, so the second assignment is the honest answer to "which one".
func collectLocals(fn *ast.FuncDecl) map[string]ast.Expr {
	locals := map[string]ast.Expr{}
	seen := map[string]int{}
	record := func(lhs, rhs []ast.Expr) {
		switch {
		case len(lhs) == len(rhs):
			for i, l := range lhs {
				bind(locals, seen, l, rhs[i])
			}
		case len(rhs) == 1:
			// `path, err := helper()` — the shape every path-returning helper in
			// this tree is called in. Only the first result can be the path, and
			// the rest bind to nothing, which folds them to holes.
			bind(locals, seen, lhs[0], rhs[0])
			for _, l := range lhs[1:] {
				bind(locals, seen, l, nil)
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			record(s.Lhs, s.Rhs)
		case *ast.ValueSpec:
			var lhs []ast.Expr
			for _, name := range s.Names {
				lhs = append(lhs, name)
			}
			record(lhs, s.Values)
		}
		return true
	})
	for name, n := range seen {
		if n > 1 {
			locals[name] = nil
		}
	}
	return locals
}

// bind records one name's value, or marks it unresolvable when value is nil.
func bind(locals map[string]ast.Expr, seen map[string]int, lhs ast.Expr, value ast.Expr) {
	id, ok := lhs.(*ast.Ident)
	if !ok || id.Name == "_" {
		return
	}
	seen[id.Name]++
	locals[id.Name] = value
}

// foldMethod resolves the method argument to an HTTP verb. http.MethodGet and a
// bare "GET" both resolve; anything else yields "*", which a probe sends as GET
// and reports as unresolved alongside the template.
func (r *resolver) foldMethod(e ast.Expr, locals map[string]ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "http" &&
			strings.HasPrefix(v.Sel.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(v.Sel.Name, "Method"))
		}
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return strings.ToUpper(s)
			}
		}
	case *ast.Ident:
		if inner, ok := locals[v.Name]; ok && inner != nil {
			return r.foldMethod(inner, locals)
		}
	}
	return "*"
}

// fold reduces a URL expression to a path template. Anything it cannot spell
// out becomes a hole, so the result is always probeable — the question a hole
// leaves open is whose value fills it, never whether the surrounding path is
// real.
func (r *resolver) fold(e ast.Expr, locals map[string]ast.Expr, depth int) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s
			}
		}
		return hole

	case *ast.ParenExpr:
		return r.fold(v.X, locals, depth)

	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return hole
		}
		return r.fold(v.X, locals, depth) + r.fold(v.Y, locals, depth)

	case *ast.Ident:
		if inner, ok := locals[v.Name]; ok && inner != nil {
			return r.fold(inner, locals, depth)
		}
		return hole

	case *ast.CallExpr:
		return r.foldCall(v, locals, depth)
	}
	return hole
}

// foldCall handles the two call shapes that carry a path: fmt.Sprintf, whose
// format literal IS the template, and a callee in this tree that returns one.
func (r *resolver) foldCall(call *ast.CallExpr, locals map[string]ast.Expr, depth int) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" &&
			sel.Sel.Name == "Sprintf" && len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if format, err := strconv.Unquote(lit.Value); err == nil {
					return templateFromFormat(format)
				}
			}
			return hole
		}
	}
	if depth <= 0 {
		return hole
	}
	name := calleeName(call.Fun)
	if name == "" {
		return hole
	}
	decls := r.funcs[name]
	if len(decls) == 0 {
		return hole
	}
	// Pool the returns of every declaration with this name. Distinct paths
	// alternate, which the probe expands: a helper branching on org vs repo
	// scope yields both, and the fake must serve whichever the venue reaches.
	var alts []string
	for _, fn := range decls {
		inner := collectLocals(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) == 0 {
				return true
			}
			t := r.fold(ret.Results[0], inner, depth-1)
			if t != hole && t != "" && !contains(alts, t) {
				alts = append(alts, t)
			}
			return true
		})
	}
	switch len(alts) {
	case 0:
		return hole
	case 1:
		return alts[0]
	default:
		sort.Strings(alts)
		return "(" + strings.Join(alts, "|") + ")"
	}
}

// calleeName reports the identifier a call names, for a plain function or a
// method on any receiver.
func calleeName(fun ast.Expr) string {
	switch v := fun.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// templateFromFormat rewrites a Printf format into a template, turning each verb
// into a hole. Integer verbs keep their own marker so a probe fills them with
// something the receiving handler can parse.
func templateFromFormat(format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			b.WriteByte('%')
			i++
			continue
		}
		j := i + 1
		for j < len(format) && strings.ContainsRune("+-# 0123456789.*", rune(format[j])) {
			j++
		}
		if j >= len(format) {
			b.WriteString(hole)
			break
		}
		if strings.ContainsRune("dboxXcU", rune(format[j])) {
			b.WriteString(numHole)
		} else {
			b.WriteString(hole)
		}
		i = j
	}
	return b.String()
}

// expandAlternatives turns a template carrying "(a|b)" groups into one template
// per branch, so each is probed on its own.
func expandAlternatives(t string) []string {
	open := strings.Index(t, "(")
	if open < 0 {
		return []string{t}
	}
	shut := strings.Index(t[open:], ")")
	if shut < 0 {
		return []string{t}
	}
	shut += open
	var out []string
	for _, alt := range strings.Split(t[open+1:shut], "|") {
		out = append(out, expandAlternatives(t[:open]+alt+t[shut+1:])...)
	}
	return out
}

// probePath turns a template into a concrete request path: the leading hole is
// the API base the venue configures, and every other hole becomes a segment
// that carries no meaning to the fake's dispatch.
func probePath(template string) string {
	p := strings.TrimPrefix(template, hole)
	p = strings.ReplaceAll(p, numHole, "1")
	p = strings.ReplaceAll(p, hole, "x")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
