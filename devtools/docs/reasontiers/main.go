// Command reasontiers reconciles the condition reasons and Event reasons the AGC
// emits against the acquisition-tier ledger an operator reads (Q850).
//
// It is the sibling of metrictiers, which did the same for the 53 actions_gateway_*
// series (Q776). Metrics were one of three signal surfaces; a capability that
// reaches only the classic tier is just as invisible when it surfaces as a
// condition reason or a Kubernetes Event, and neither of those was derived from
// the source or gated.
//
// Five checks:
//
//	inventory     the reasons the AGC emits and the ledger's name the same set
//	vocabulary    every ledger row states one of the four tiers, with a reason
//	contradiction no single-tier row is emitted from the tier it excludes
//	resolution    every recorder call's reason argument resolves to a name
//	reference     every Event reason also has a runbook entry
//
// resolution is the check with no metric counterpart, and it exists because an
// Event reason is an argument rather than a declaration. It reaches the recorder
// as a literal at some sites, through a local at others, and the wrappers forward
// it as a parameter — so a scan keyed on the call name counts plumbing as emission
// and misses the computed cases. Every reason argument is placed in one of the
// forms placeReason lists, and what cannot be placed is a finding.
//
// Which argument holds the reason is itself derived, not tabulated: two different
// methods here are called Event and two are called recordEvent, and their reason
// sits at a different index in each. Keying on the name alone read the scale-set
// listener's action string as its reason and missed four reasons entirely, which
// is why the index comes from the callee's own declaration.
//
// Tier classification is the same seam metrictiers uses and is one-sided in the
// same way: a site under a tier-exclusive directory refutes the opposite claim,
// and a shared file says nothing. The ledger row carries the positive claim, and
// inventory is what makes the row unavoidable.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// Tier vocabulary, identical to the metric ledger's so an operator reads one set
// of words across all three tables.
const (
	tierBoth     = "Both"
	tierClassic  = "Classic only"
	tierScaleSet = "Scale-set only"
	tierNeutral  = "Tier-neutral"
)

// The ledger's two tables, found by the heading each sits under.
const (
	conditionHeading = "Condition reasons"
	eventHeading     = "Event reasons"
)

// Source subtrees only one acquisition tier ever executes, and the file-name
// convention for the scale-set arm of a package that serves both.
var (
	classicOnlyDirs    = []string{"internal/listener"}
	scaleSetOnlyDirs   = []string{"internal/scalesetlistener"}
	scaleSetOnlySuffix = "_scaleset.go"
)

// externalRecorders are the recorder signatures declared outside this repo, so
// their reason index cannot be read off a declaration in the tree.
var externalRecorders = []recorderSig{
	// events.EventRecorder.Eventf(object, related, eventtype, reason, action, note, args…)
	{name: "Eventf", arity: 6, reasonIdx: 3, variadic: true},
}

// eventTypeSelectors are the corev1 event-type constants. A call carrying one is
// recording an Event, which is how an unrecognized recorder is caught.
var eventTypeSelectors = map[string]bool{
	"EventTypeNormal":  true,
	"EventTypeWarning": true,
}

// reasonConstRE matches the condition-reason constants' declared names.
var reasonConstRE = regexp.MustCompile(`^Reason[A-Z][A-Za-z0-9]*$`)

// reasonPkgs are the import identifiers a condition reason is referenced through.
// The v2 version packages re-export api/apiconditions, so the value behind
// v2alpha1.ReasonX is resolved from there.
var reasonPkgs = map[string]bool{
	"v1alpha1":      true,
	"v2alpha1":      true,
	"v2beta1":       true,
	"apiconditions": true,
}

// recorderSig is one event-recorder signature: where its reason argument sits,
// and how many arguments a call to it has. arity is the minimum a call can
// pass, so a variadic recorder's trailing parameter is not counted — omitting
// the varargs entirely is a legal call, and requiring them would silently skip
// it rather than fail.
type recorderSig struct {
	name      string
	arity     int
	reasonIdx int
	variadic  bool
}

func (s recorderSig) matches(name string, args int) bool {
	if s.name != name {
		return false
	}
	if s.variadic {
		return args >= s.arity
	}
	return args == s.arity
}

// reason is one reason value the AGC emits, with the files it is referenced from.
type reason struct {
	value string
	sites []string
}

// ledgerRow is one row of a tier table.
type ledgerRow struct {
	name string
	tier string
	note string
	line int
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: reasontiers <agc-src-dir> <api-dir> <observability-metrics.md> <troubleshooting.md>")
		os.Exit(2)
	}
	findings, err := run(os.Args[1], os.Args[2], os.Args[3], os.Args[4])
	if err != nil {
		fmt.Fprintf(os.Stderr, "reasontiers: %v\n", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "reasontiers: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
}

func run(srcDir, apiDir, ledgerDoc, eventDoc string) ([]string, error) {
	values, err := reasonValues(apiDir, srcDir)
	if err != nil {
		return nil, err
	}
	sigs, err := recorderSignatures(srcDir)
	if err != nil {
		return nil, err
	}
	conds, events, unresolved, err := scanSource(srcDir, values, sigs)
	if err != nil {
		return nil, err
	}
	if len(conds) == 0 || len(events) == 0 {
		return nil, fmt.Errorf("scanning %s found %d condition reasons and %d Event reasons — an empty side is not a clean tree", srcDir, len(conds), len(events))
	}

	docBytes, err := os.ReadFile(ledgerDoc)
	if err != nil {
		return nil, err
	}
	condRows, err := parseLedger(docBytes, conditionHeading)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ledgerDoc, err)
	}
	eventRows, err := parseLedger(docBytes, eventHeading)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ledgerDoc, err)
	}

	eventDocBytes, err := os.ReadFile(eventDoc)
	if err != nil {
		return nil, err
	}

	var findings []string
	findings = append(findings, unresolved...)
	findings = append(findings, checkInventory(conds, condRows, ledgerDoc, "condition reason")...)
	findings = append(findings, checkInventory(events, eventRows, ledgerDoc, "Event reason")...)
	findings = append(findings, checkVocabulary(condRows, ledgerDoc)...)
	findings = append(findings, checkVocabulary(eventRows, ledgerDoc)...)
	findings = append(findings, checkContradiction(conds, condRows)...)
	findings = append(findings, checkContradiction(events, eventRows)...)
	findings = append(findings, checkEventReference(events, string(eventDocBytes), eventDoc)...)
	sort.Strings(findings)
	return findings, nil
}

// parseGo parses every non-test Go file under root, calling fn for each.
func parseGo(root string, fn func(file *ast.File, fset *token.FileSet, rel string) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		return fn(file, fset, filepath.ToSlash(path))
	})
}

// reasonValues resolves every Reason* constant to the string an operator sees.
// The v2 version packages alias api/apiconditions, so the alias targets are
// resolved after the literals are read.
func reasonValues(apiDir, srcDir string) (map[string]string, error) {
	literals := map[string]string{} // constant name -> value
	aliases := map[string]string{}  // constant name -> constant name it re-exports

	read := func(file *ast.File, _ *token.FileSet, _ string) error {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				name := vs.Names[0].Name
				if !reasonConstRE.MatchString(name) {
					continue
				}
				switch v := vs.Values[0].(type) {
				case *ast.BasicLit:
					if v.Kind != token.STRING {
						continue
					}
					if s, err := strconv.Unquote(v.Value); err == nil {
						literals[name] = s
					}
				case *ast.SelectorExpr:
					aliases[name] = v.Sel.Name
				}
			}
		}
		return nil
	}

	if err := parseGo(apiDir, read); err != nil {
		return nil, err
	}
	// The v1 vocabulary lives under the AGC rather than in the shared api module.
	if agcAPI := filepath.Join(srcDir, "api"); dirExists(agcAPI) {
		if err := parseGo(agcAPI, read); err != nil {
			return nil, err
		}
	}

	for name, target := range aliases {
		if v, ok := literals[target]; ok {
			literals[name] = v
		}
	}
	if len(literals) == 0 {
		return nil, fmt.Errorf("no Reason* constants found under %s — the vocabulary cannot be empty", apiDir)
	}
	return literals, nil
}

// recorderSignatures reads the reason argument's index off every event-recorder
// declared in the tree — the concrete wrappers and the sink interfaces both.
// A recorder is a function taking an eventtype and a reason; the pair is what
// distinguishes it from the many other functions with a reason parameter.
func recorderSignatures(srcDir string) ([]recorderSig, error) {
	sigs := append([]recorderSig(nil), externalRecorders...)
	seen := map[string]bool{}

	add := func(name string, params *ast.FieldList) {
		if params == nil {
			return
		}
		var names []string
		variadic := false
		for _, f := range params.List {
			_, isVariadic := f.Type.(*ast.Ellipsis)
			if len(f.Names) == 0 {
				names = append(names, "")
				variadic = variadic || isVariadic
				continue
			}
			for _, id := range f.Names {
				names = append(names, id.Name)
			}
			variadic = variadic || isVariadic
		}
		reasonIdx, hasType := -1, false
		for i, n := range names {
			switch n {
			case "reason":
				reasonIdx = i
			case "eventtype", "eventType":
				hasType = true
			}
		}
		if reasonIdx < 0 || !hasType {
			return
		}
		arity := len(names)
		if variadic {
			arity--
		}
		key := fmt.Sprintf("%s/%d/%d/%t", name, arity, reasonIdx, variadic)
		if seen[key] {
			return
		}
		seen[key] = true
		sigs = append(sigs, recorderSig{name: name, arity: arity, reasonIdx: reasonIdx, variadic: variadic})
	}

	err := parseGo(srcDir, func(file *ast.File, _ *token.FileSet, _ string) error {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				add(node.Name.Name, node.Type.Params)
			case *ast.InterfaceType:
				for _, m := range node.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 {
						continue
					}
					add(m.Names[0].Name, ft.Params)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sigs, nil
}

// scanSource walks the AGC for condition-reason references and Event-reason
// emissions, returning both keyed by the operator-visible value, plus a finding
// for every reason argument that could not be placed.
func scanSource(srcDir string, values map[string]string, sigs []recorderSig) (conds, events map[string]*reason, unresolved []string, err error) {
	conds, events = map[string]*reason{}, map[string]*reason{}

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			// cmd/agc/test holds the load harness and cmd/agc/api the v1 vocabulary
			// itself; neither emits.
			if d.Name() == "testdata" || path == filepath.Join(srcDir, "test") || path == filepath.Join(srcDir, "api") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel := filepath.ToSlash(path)
		collectConditions(file, rel, values, conds)
		unresolved = append(unresolved, collectEvents(file, fset, rel, values, sigs, events)...)
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return conds, events, unresolved, nil
}

// collectConditions records every reference to a condition-reason constant. A
// reason the AGC only compares against is one it also writes, or the comparison
// is dead — so references, not writes, are the inventory. Over-approximating
// adds a ledger row; it never drops one.
func collectConditions(file *ast.File, rel string, values map[string]string, out map[string]*reason) {
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if v, ok := constValue(sel, values); ok {
			record(out, v, rel)
		}
		return true
	})
}

// constValue resolves a qualified Reason* reference to its string value.
func constValue(sel *ast.SelectorExpr, values map[string]string) (string, bool) {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !reasonPkgs[pkg.Name] || !reasonConstRE.MatchString(sel.Sel.Name) {
		return "", false
	}
	v, ok := values[sel.Sel.Name]
	return v, ok
}

// collectEvents records the Event reasons a file decides, and reports every
// recorder call whose reason argument it cannot place.
func collectEvents(file *ast.File, fset *token.FileSet, rel string, values map[string]string, sigs []recorderSig, out map[string]*reason) []string {
	var findings []string

	// Function scope is what separates a forwarder's parameter from a local, so
	// each call is placed against the innermost function enclosing it. The whole
	// node stack is tracked because ast.Inspect's nil signals the exit of some
	// subtree rather than of a chosen node kind: push everything, pop on nil.
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, known := calleeName(call)
		if !known {
			return true
		}
		arg, recognized := reasonArg(call, name, sigs)
		if !recognized {
			if carriesEventType(call) {
				findings = append(findings, fmt.Sprintf(
					"%s:%d: %s records an Event and matches no recorder signature reasontiers can read — give its declaration parameters named eventtype and reason, or the reasons it emits reach an operator ungated",
					rel, fset.Position(call.Pos()).Line, name))
			}
			return true
		}
		literals, form := placeReason(arg, enclosingFunc(stack), values)
		switch form {
		case formLiteral:
			for _, v := range literals {
				record(out, v, rel)
			}
		case formForward, formCondition:
			// A forwarder passes its caller's reason through, and a condition
			// reason re-emitted as an Event is carried by the condition ledger.
		case formUnplaceable:
			findings = append(findings, fmt.Sprintf(
				"%s:%d: this Event's reason argument does not resolve to a name — the ledger cannot carry a reason nobody can state; pass a literal, or assign it from literals or Reason* constants",
				rel, fset.Position(call.Pos()).Line))
		}
		return true
	})
	return findings
}

// enclosingFunc returns the innermost FuncDecl or FuncLit on the stack.
func enclosingFunc(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return stack[i]
		}
	}
	return nil
}

// The forms a reason argument can take. Only formLiteral adds to the Event
// inventory; formForward is plumbing and formCondition is carried by the
// condition ledger.
const (
	formLiteral = iota
	formForward
	formCondition
	formUnplaceable
)

func calleeName(call *ast.CallExpr) (string, bool) {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name, true
	case *ast.Ident:
		return f.Name, true
	}
	return "", false
}

// reasonArg returns the reason argument of a recorder call, and whether the call
// matched a known recorder signature at all.
func reasonArg(call *ast.CallExpr, name string, sigs []recorderSig) (ast.Expr, bool) {
	for _, s := range sigs {
		if s.matches(name, len(call.Args)) && s.reasonIdx < len(call.Args) {
			return call.Args[s.reasonIdx], true
		}
	}
	return nil, false
}

// carriesEventType reports whether a call passes a corev1 event-type constant,
// which is the shape of a recorder whose signature was not recognized.
func carriesEventType(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if sel, ok := a.(*ast.SelectorExpr); ok && eventTypeSelectors[sel.Sel.Name] {
			return true
		}
	}
	return false
}

// placeReason classifies one reason argument, resolving a local by scanning the
// assignments to it inside the enclosing function.
func placeReason(e ast.Expr, fn ast.Node, values map[string]string) ([]string, int) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return nil, formUnplaceable
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return nil, formUnplaceable
		}
		return []string{s}, formLiteral
	case *ast.SelectorExpr:
		if _, ok := constValue(x, values); ok {
			return nil, formCondition
		}
		// A `.Reason`/`.reason` field read off a condition or a queued record.
		if n := x.Sel.Name; n == "Reason" || n == "reason" {
			return nil, formCondition
		}
		return nil, formUnplaceable
	case *ast.Ident:
		if fn == nil {
			return nil, formUnplaceable
		}
		if isParam(fn, x.Name) {
			return nil, formForward
		}
		return resolveLocal(fn, x.Name, values)
	}
	return nil, formUnplaceable
}

// isParam reports whether name is a parameter of fn, which makes the call a
// forwarder rather than the site that decides the reason.
func isParam(fn ast.Node, name string) bool {
	var params *ast.FieldList
	switch f := fn.(type) {
	case *ast.FuncDecl:
		params = f.Type.Params
	case *ast.FuncLit:
		params = f.Type.Params
	}
	if params == nil {
		return false
	}
	for _, field := range params.List {
		for _, id := range field.Names {
			if id.Name == name {
				return true
			}
		}
	}
	return false
}

// resolveLocal reads every assignment to name inside fn. All-literals makes the
// Event reasons those literals; all-constants makes it a re-emitted condition
// reason. A mix, or anything else, is unplaceable — the two ledgers would each
// half-carry it.
func resolveLocal(fn ast.Node, name string, values map[string]string) ([]string, int) {
	var lits []string
	consts, other := 0, 0
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(as.Rhs) {
				continue
			}
			switch rhs := as.Rhs[i].(type) {
			case *ast.BasicLit:
				if rhs.Kind == token.STRING {
					if s, err := strconv.Unquote(rhs.Value); err == nil {
						lits = append(lits, s)
						continue
					}
				}
				other++
			case *ast.SelectorExpr:
				if _, ok := constValue(rhs, values); ok {
					consts++
					continue
				}
				other++
			default:
				other++
			}
		}
		return true
	})
	switch {
	case other > 0:
		return nil, formUnplaceable
	case len(lits) > 0 && consts == 0:
		return lits, formLiteral
	case consts > 0 && len(lits) == 0:
		return nil, formCondition
	}
	return nil, formUnplaceable
}

func record(out map[string]*reason, value, site string) {
	r, ok := out[value]
	if !ok {
		r = &reason{value: value}
		out[value] = r
	}
	for _, s := range r.sites {
		if s == site {
			return
		}
	}
	r.sites = append(r.sites, site)
}

// parseLedger reads the tier table under the given heading. The document goes
// through the shared goldmark layer rather than a hand-rolled scan, which is
// what every Markdown gate here reads with since Q612.
func parseLedger(doc []byte, heading string) ([]ledgerRow, error) {
	d := markdown.Parse(doc)
	start, end, ok := d.SectionRange(3, heading)
	if !ok {
		return nil, fmt.Errorf("no %q section — the tier ledger is the gate's input and cannot be absent", heading)
	}
	var rows []ledgerRow
	for _, t := range d.Tables() {
		if t.Line < start || t.Line > end {
			continue
		}
		for _, r := range t.Rows {
			if len(r.Text) < 3 {
				continue
			}
			rows = append(rows, ledgerRow{name: r.Text[0], tier: r.Text[1], note: r.Text[2], line: r.Line})
		}
	}
	// An empty table is not an error here: the inventory check then reports every
	// reason by name, which is the more useful failure than one line saying the
	// table went missing.
	return rows, nil
}

func checkInventory(emitted map[string]*reason, rows []ledgerRow, docPath, kind string) []string {
	inLedger := map[string]bool{}
	for _, r := range rows {
		inLedger[r.name] = true
	}
	var findings []string
	for _, name := range sortedKeys(emitted) {
		if !inLedger[name] {
			findings = append(findings, fmt.Sprintf(
				"%s: %s is a %s emitted from %s and has no ledger row — state which acquisition tier reaches it",
				docPath, name, kind, strings.Join(emitted[name].sites, ", ")))
		}
	}
	for _, r := range rows {
		if _, ok := emitted[r.name]; !ok {
			findings = append(findings, fmt.Sprintf(
				"%s:%d: %s is listed as a %s and the AGC emits no such reason",
				docPath, r.line, r.name, kind))
		}
	}
	return findings
}

func checkVocabulary(rows []ledgerRow, docPath string) []string {
	var findings []string
	for _, r := range rows {
		switch r.tier {
		case tierBoth, tierNeutral:
		case tierClassic, tierScaleSet:
			if r.note == "" {
				findings = append(findings, fmt.Sprintf(
					"%s:%d: %s is %q with no reason — say why the other tier does not reach it, and what to read there instead",
					docPath, r.line, r.name, r.tier))
			}
		default:
			findings = append(findings, fmt.Sprintf(
				"%s:%d: %s has tier %q, which is not one of %q, %q, %q, %q",
				docPath, r.line, r.name, r.tier, tierBoth, tierClassic, tierScaleSet, tierNeutral))
		}
	}
	return findings
}

// checkContradiction fails a single-tier row a site refutes. One-sided by
// construction: a shared file says nothing about the tier, so this can refute a
// claim and never confirm one.
func checkContradiction(emitted map[string]*reason, rows []ledgerRow) []string {
	tier := map[string]string{}
	for _, r := range rows {
		tier[r.name] = r.tier
	}
	var findings []string
	for _, name := range sortedKeys(emitted) {
		for _, site := range emitted[name].sites {
			switch {
			case tier[name] == tierClassic && isScaleSetOnly(site):
				findings = append(findings, fmt.Sprintf(
					"%s: %s is emitted here, and the ledger calls it %q", site, name, tierClassic))
			case tier[name] == tierScaleSet && isClassicOnly(site):
				findings = append(findings, fmt.Sprintf(
					"%s: %s is emitted here, and the ledger calls it %q", site, name, tierScaleSet))
			}
		}
	}
	return findings
}

// checkEventReference asserts the ledger never becomes the only place an Event
// reason is described. An operator meets an Event in `kubectl describe` and looks
// it up in the runbook; a reason that reaches the ledger alone gets a tier and no
// remedy, which is how a metric shipped undocumented in Q809.
func checkEventReference(events map[string]*reason, doc, docPath string) []string {
	var findings []string
	for _, name := range sortedKeys(events) {
		if !strings.Contains(doc, "`"+name+"`") {
			findings = append(findings, fmt.Sprintf(
				"%s: %s (recorded from %s) has a tier and no runbook entry — an operator who sees the Event gets no remedy",
				docPath, name, strings.Join(events[name].sites, ", ")))
		}
	}
	return findings
}

func isClassicOnly(path string) bool {
	for _, d := range classicOnlyDirs {
		if strings.Contains(path, d+"/") {
			return true
		}
	}
	return false
}

func isScaleSetOnly(path string) bool {
	if strings.HasSuffix(path, scaleSetOnlySuffix) {
		return true
	}
	for _, d := range scaleSetOnlyDirs {
		if strings.Contains(path, d+"/") {
			return true
		}
	}
	return false
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func sortedKeys(m map[string]*reason) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
