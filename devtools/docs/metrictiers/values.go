package main

// Label-value derivation (Q851).
//
// The series-level checks answer "which tier emits this metric". They cannot
// see a series that populates on both tiers while one of its label values
// reaches only one — eviction_retries_total is Both, and cause="vanished" is
// emitted from a scale-set-only file.
//
// Deriving the value set is what keeps the answer off a hand-kept list. Three
// shapes carry a label value to a Prometheus call in this tree:
//
//	direct     WithLabelValues(…, reapReasonJobAbandoned)
//	local      a variable the enclosing function assigns from a literal or const
//	parameter  a value the caller supplies, resolved one hop up
//
// Anything else is left underived, and an underived label simply carries no
// values — the same one-sidedness the contradiction check has. A value the
// derivation misses is invisible here; a value it finds is one the source
// demonstrably emits, which is what the checks are entitled to assert on.

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

// resolveDepth bounds the walk away from the Prometheus call. Three steps reach
// every value in this tree, and the poll-error seam is what sets the bound: the
// counter is written in runnercore's IncPollError, the scale-set listener wraps
// that in metricsIncPollError, and the wrapper is handed a classifier's return
// value (pollErrorReason). Stopping short leaves the scale-set arm underived and
// every poll reason reading classic-only.
//
// Walking further is safe in the direction that matters — a wrongly pooled
// same-named function can only add origins, and extra origins make a value look
// less tier-exclusive, never more.
const resolveDepth = 3

// tierLabel is the acquisition tier's own axis. Its values are the tier names,
// so asking which tier emits one is tautological and the ledger's tier column
// has already answered it.
const tierLabel = "tier"

// openLabels carry tenant identity rather than a code vocabulary: their values
// come from a CR name or a container name, so there is no closed set to publish
// in Help and no tier to attribute. Nothing in the AGC's own source passes a
// literal in one of these positions, so this set changes no finding today — it
// keeps a future one that does from being read as a vocabulary.
//
// It is the one hand-kept input here, and it can only ever silence a check,
// never manufacture one.
var openLabels = map[string]bool{
	"namespace":    true,
	"runner_group": true,
	"runner_set":   true,
	"container":    true,
	"name":         true,
	"controller":   true,
}

// labelValue is one value of one label, with the files that name it. The
// origins are what the tier classification reads: the file holding the literal,
// not the file holding the Prometheus call.
type labelValue struct {
	value   string
	origins []string
}

// valueSet indexes the derived values by metric name, then label name.
type valueSet map[string]map[string][]labelValue

// deriveValues resolves the label values the source emits for each series.
func deriveValues(files []srcFile, defs []metric) valueSet {
	r := newResolver(files)

	labels := map[string][]string{} // field -> label names
	fieldToNames := map[string][]string{}
	for _, d := range defs {
		if d.field == "" {
			continue
		}
		if len(d.labels) > 0 {
			labels[d.field] = d.labels
		}
		fieldToNames[d.field] = append(fieldToNames[d.field], d.name)
	}

	out := valueSet{}
	for _, c := range r.emitCalls {
		names := labels[c.field]
		if len(names) == 0 {
			continue
		}
		for i, arg := range c.args {
			if i >= len(names) || names[i] == tierLabel || openLabels[names[i]] {
				continue
			}
			for _, v := range r.resolve(arg, c.file, c.fn, 0) {
				for _, series := range fieldToNames[c.field] {
					out.add(series, names[i], v)
				}
			}
		}
	}
	return out
}

// add folds a value into the set, merging origins for a value seen more than
// once. Merging matters: a value emitted from a shared file and a tier-exclusive
// one is not tier-exclusive, and only the union shows that.
func (vs valueSet) add(series, label string, v labelValue) {
	byLabel, ok := vs[series]
	if !ok {
		byLabel = map[string][]labelValue{}
		vs[series] = byLabel
	}
	for i, existing := range byLabel[label] {
		if existing.value == v.value {
			for _, o := range v.origins {
				if !slices.Contains(existing.origins, o) {
					byLabel[label][i].origins = append(byLabel[label][i].origins, o)
				}
			}
			return
		}
	}
	byLabel[label] = append(byLabel[label], labelValue{value: v.value, origins: slices.Clone(v.origins)})
}

// emitCall is one Prometheus call that writes a sample, kept with the scope
// needed to resolve its arguments: the file for package-level lookups and the
// enclosing function for locals and parameters.
type emitCall struct {
	field string
	args  []ast.Expr
	file  srcFile
	fn    *ast.FuncDecl
}

// callSite is one call to a named function, kept so a parameter can be resolved
// against what its callers pass.
type callSite struct {
	args []ast.Expr
	file srcFile
	fn   *ast.FuncDecl
}

// decl is a function and the file declaring it, so a return expression can be
// resolved in its own scope.
type decl struct {
	fn   *ast.FuncDecl
	file srcFile
}

// resolver holds the symbol tables the resolution reads. Constants are scoped by
// package directory, because that is where Go resolves an unqualified
// identifier. Functions and their call sites are keyed by bare name across the
// whole tree, because the seams that carry a label value cross packages: the
// scale-set listener reaches the shared counter through runnercore's
// PollErrorRecorder. Pooling two same-named functions can only add origins to a
// value, and extra origins make a value look *less* tier-exclusive — the safe
// direction for a check that only ever refutes.
type resolver struct {
	consts    map[string]map[string]string // dir -> ident -> string value
	funcs     map[string]decl
	callers   map[string][]callSite
	emitCalls []emitCall
}

func newResolver(files []srcFile) *resolver {
	r := &resolver{
		consts:  map[string]map[string]string{},
		funcs:   map[string]decl{},
		callers: map[string][]callSite{},
	}
	for _, f := range files {
		r.collectConsts(f)
		r.collectFuncs(f)
	}
	for _, f := range files {
		r.collectCalls(f)
	}
	return r
}

func (r *resolver) collectConsts(f srcFile) {
	consts := r.consts[f.dir]
	if consts == nil {
		consts = map[string]string{}
		r.consts[f.dir] = consts
	}
	for _, d := range f.file.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				if s, ok := stringLit(vs.Values[i]); ok {
					consts[name.Name] = s
				}
			}
		}
	}
}

func (r *resolver) collectFuncs(f srcFile) {
	for _, d := range f.file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name != nil {
			r.funcs[fn.Name.Name] = decl{fn: fn, file: f}
		}
	}
}

// collectCalls records both the sample-writing calls and every named call, the
// latter so a parameter can be resolved against its callers.
func (r *resolver) collectCalls(f srcFile) {
	record := func(fn *ast.FuncDecl, body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := trailingName(call.Fun); name != "" {
				r.callers[name] = append(r.callers[name], callSite{args: call.Args, file: f, fn: fn})
			}
			if field, args := labelArgs(call); field != "" {
				r.emitCalls = append(r.emitCalls, emitCall{field: field, args: args, file: f, fn: fn})
			}
			return true
		})
	}

	for _, d := range f.file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			if fn.Body != nil {
				record(fn, fn.Body)
			}
			continue
		}
		record(nil, d)
	}
}

// labelArgs reports the field a call writes through and the arguments sitting in
// label positions, or "" for a call that carries none. WithLabelValues takes
// them from the first argument; the const-metric constructors take a Desc, a
// type and a value first.
func labelArgs(call *ast.CallExpr) (string, []ast.Expr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", nil
	}
	switch {
	case sel.Sel.Name == "WithLabelValues" || sel.Sel.Name == "DeleteLabelValues":
		return trailingName(sel.X), call.Args
	case constMetricFuncs[sel.Sel.Name]:
		if len(call.Args) < 4 {
			return "", nil
		}
		return trailingName(call.Args[0]), call.Args[3:]
	}
	return "", nil
}

// resolve reports the values an expression in a label position can take.
func (r *resolver) resolve(e ast.Expr, f srcFile, fn *ast.FuncDecl, depth int) []labelValue {
	if s, ok := stringLit(e); ok {
		return []labelValue{{value: s, origins: []string{f.path}}}
	}
	if call, ok := e.(*ast.CallExpr); ok {
		if depth >= resolveDepth {
			return nil
		}
		return r.resolveReturns(trailingName(call.Fun), depth+1)
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil
	}
	if s, ok := r.consts[f.dir][id.Name]; ok {
		// The use site is the origin, not the declaration: recoveryCauseVanished
		// is declared in a shared file and named in a scale-set-only one, and the
		// naming is what binds the value to a tier.
		return []labelValue{{value: s, origins: []string{f.path}}}
	}
	if fn == nil {
		return nil
	}
	if vals := r.resolveLocal(id.Name, f, fn); len(vals) > 0 {
		return vals
	}
	if depth >= resolveDepth {
		return nil
	}
	return r.resolveParam(id.Name, f, fn, depth)
}

// resolveReturns reports the values a classifier function returns. The scale-set
// listener reaches the shared poll-error counter through one — IncPollError(
// pollErrorReason(err)) — so without this the classic tier's literals would be
// the only ones derived and every reason would read classic-only.
func (r *resolver) resolveReturns(name string, depth int) []labelValue {
	d, ok := r.funcs[name]
	if !ok || d.fn.Body == nil {
		return nil
	}
	var out []labelValue
	ast.Inspect(d.fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			out = append(out, r.resolve(res, d.file, d.fn, depth)...)
		}
		return true
	})
	return out
}

// resolveLocal reports the values the enclosing function assigns to a local.
func (r *resolver) resolveLocal(name string, f srcFile, fn *ast.FuncDecl) []labelValue {
	var out []labelValue
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(assign.Rhs) {
				continue
			}
			if s, ok := stringLit(assign.Rhs[i]); ok {
				out = append(out, labelValue{value: s, origins: []string{f.path}})
				continue
			}
			if rid, ok := assign.Rhs[i].(*ast.Ident); ok {
				if s, ok := r.consts[f.dir][rid.Name]; ok {
					out = append(out, labelValue{value: s, origins: []string{f.path}})
				}
			}
		}
		return true
	})
	return out
}

// resolveParam reports the values callers pass in a parameter's position.
func (r *resolver) resolveParam(name string, f srcFile, fn *ast.FuncDecl, depth int) []labelValue {
	idx := paramIndex(fn, name)
	if idx < 0 {
		return nil
	}
	var out []labelValue
	for _, site := range r.callers[fn.Name.Name] {
		if idx >= len(site.args) {
			continue
		}
		out = append(out, r.resolve(site.args[idx], site.file, site.fn, depth+1)...)
	}
	return out
}

// paramIndex reports the positional index of a named parameter. A grouped field
// (owner, repo, runID string) declares one parameter per name, so the count
// walks names rather than fields.
func paramIndex(fn *ast.FuncDecl, name string) int {
	if fn.Type == nil || fn.Type.Params == nil {
		return -1
	}
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// derivedTier reports the tier a value's origins prove it exclusive to, or ""
// when they do not agree. A value named only in scale-set-only files cannot
// reach the classic tier; a value named in a shared file says nothing.
func derivedTier(origins []string) string {
	if len(origins) == 0 {
		return ""
	}
	classic, scaleSet := true, true
	for _, o := range origins {
		if !isClassicOnly(o) {
			classic = false
		}
		if !isScaleSetOnly(o) {
			scaleSet = false
		}
	}
	switch {
	case classic:
		return tierClassic
	case scaleSet:
		return tierScaleSet
	}
	return ""
}

// citesGoFile pulls the source citations out of a value row's note. A row whose
// tier the file layout cannot derive has to point at the guard that makes the
// claim true, so the claim stays anchored to code a reviewer can open.
func citesGoFile(note string) []string {
	var out []string
	for _, part := range strings.Split(note, "`") {
		if strings.HasSuffix(part, ".go") && !strings.Contains(part, " ") {
			out = append(out, part)
		}
	}
	return out
}
