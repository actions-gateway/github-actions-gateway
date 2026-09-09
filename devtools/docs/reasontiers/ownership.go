package main

// The ownership check (Q994). A condition type with two producers needs one of
// them enumerated, so a consumer can tell whose verdict it is holding. That
// enumeration is a switch in the consumer's own package, and nothing derived it
// from what the producer emits — so a reason added to the producer and forgotten
// here reads as the other producer's, and the consumer wipes a verdict GitHub
// actually made.
//
// A marker comment on the enumerating function is what puts it in scope:
//
//	// reasontiers:owns RunnerVersionTooOld internal/listener
//	func IsSessionSourcedRunnerVersion(reason string) bool {
//
// Two findings come out of it:
//
//	membership  the producer emits a reason on that condition type and the
//	            switch does not list it
//	consumer    a package other than the enumeration's own compares a reason
//	            against one of its entries, which is a second membership site
//	            at its own width — the defect the marker exists to close
//
// The polarity is the enumerated side's, deliberately, and the check does not
// widen it: the OTHER producer's reasons are never enumerated anywhere, so a
// reason added there is out of scope here by construction. That is the failure
// direction the switch was written to prefer, and reversing it would trade a
// stale condition for a wiped one.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ownsRE matches the marker comment: the condition type an enumeration owns a
// producer's reasons on, and the source subtree that producer is.
var ownsRE = regexp.MustCompile(`^\s*reasontiers:owns\s+(\S+)\s+(\S+)\s*$`)

// conditionTypeParams are the parameter names a condition setter's type argument
// goes by. The reason argument is found by the same `reason` name the recorder
// signatures use, so a setter is a function taking both.
var conditionTypeParams = map[string]bool{
	"condType":      true,
	"conditionType": true,
}

// ownership is one marked enumeration: the condition type and producer subtree it
// claims, and the reasons it lists.
type ownership struct {
	condType string
	producer string
	members  map[string]string // reason value -> the constant name listed
	pkgDir   string            // the enumeration's own directory, exempt from the consumer check
	site     string            // file:line of the marked declaration
}

// emission is one condition write the producer makes.
type emission struct {
	reason string
	site   string
}

// checkOwnership reconciles every marked enumeration against what its producer
// emits, and against the consumers that ask the same question elsewhere.
func checkOwnership(srcDir string, reasons, conditions map[string]string) ([]string, error) {
	owners, err := collectOwnership(srcDir, reasons, conditions)
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		// The marker is the check's only input. A tree with none is one where the
		// scan matched nothing, which reads exactly like a tree with nothing to
		// check — the same reason errEmptySide refuses an empty inventory.
		return nil, fmt.Errorf("no reasontiers:owns marker found under %s — the ownership check has no input, which is not the same as having nothing to check", srcDir)
	}

	setters, err := conditionSetters(srcDir)
	if err != nil {
		return nil, err
	}

	var findings []string
	for _, o := range owners {
		emitted, unplaceable, err := collectEmissions(srcDir, o, setters, reasons, conditions)
		if err != nil {
			return nil, err
		}
		findings = append(findings, unplaceable...)
		if len(emitted) == 0 && len(unplaceable) == 0 {
			return nil, fmt.Errorf("%s: %s emits nothing on %s, so this marker gates nothing — point it at the producer, or drop it",
				o.site, o.producer, o.condType)
		}
		for _, e := range emitted {
			if _, ok := o.members[e.reason]; ok {
				continue
			}
			findings = append(findings, fmt.Sprintf(
				"%s: %s is published on %s here and %s does not list it — a consumer asking whose verdict it holds reads it as the other producer's and overwrites it; add it beside the entries there",
				e.site, e.reason, o.condType, o.site))
		}
		consumers, err := checkConsumers(srcDir, o, reasons)
		if err != nil {
			return nil, err
		}
		findings = append(findings, consumers...)
	}
	sort.Strings(findings)
	return findings, nil
}

// collectOwnership reads every marked enumeration and the reasons it lists. The
// listed set is read from the case clauses that return true, rather than from
// every reason the function mentions: a comparison the function makes for some
// other purpose is not membership, and counting it would let a reason in without
// anybody having decided it belongs.
func collectOwnership(srcDir string, reasons, conditions map[string]string) ([]ownership, error) {
	byValue := map[string]bool{}
	for _, v := range conditions {
		byValue[v] = true
	}

	var owners []ownership
	var parseErr error
	err := parseGoMode(srcDir, parser.ParseComments, func(file *ast.File, fset *token.FileSet, rel string) error {
		imports := importedPkgs(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				m := ownsRE.FindStringSubmatch(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*"))
				if m == nil {
					continue
				}
				site := fmt.Sprintf("%s:%d", rel, fset.Position(fn.Pos()).Line)
				if !byValue[m[1]] {
					parseErr = fmt.Errorf("%s: reasontiers:owns names condition type %q, which is no Condition* constant's value", site, m[1])
					return nil
				}
				members, err := switchMembers(fn, imports, reasons)
				if err != nil {
					parseErr = fmt.Errorf("%s: %w", site, err)
					return nil
				}
				owners = append(owners, ownership{
					condType: m[1],
					producer: filepath.ToSlash(m[2]),
					members:  members,
					pkgDir:   filepath.ToSlash(filepath.Dir(rel)),
					site:     site,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].site < owners[j].site })
	return owners, nil
}

// switchMembers reads the reasons a marked enumeration admits: the case
// expressions of every clause whose body returns true.
func switchMembers(fn *ast.FuncDecl, imports, reasons map[string]string) (map[string]string, error) {
	members := map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok || !returnsTrue(clause.Body) {
			return true
		}
		for _, e := range clause.List {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if v, ok := constValue(sel, imports, reasons); ok {
				members[v] = sel.Sel.Name
			}
		}
		return true
	})
	if len(members) == 0 {
		return nil, fmt.Errorf("the marked function admits no reason this scan can read — list them as `case pkg.ReasonX:` clauses returning true, or the marker gates nothing")
	}
	return members, nil
}

func returnsTrue(body []ast.Stmt) bool {
	for _, st := range body {
		ret, ok := st.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		if id, ok := ret.Results[0].(*ast.Ident); ok && id.Name == "true" {
			return true
		}
	}
	return false
}

// conditionSetters reads the type and reason argument indexes off every condition
// setter declared in the tree, the way recorderSignatures does for recorders. A
// setter is a function taking both a condition type and a reason; the pair is what
// separates it from the many other functions carrying one or the other.
func conditionSetters(srcDir string) ([]condSetterSig, error) {
	var sigs []condSetterSig
	seen := map[string]bool{}
	err := parseGo(srcDir, func(file *ast.File, _ *token.FileSet, _ string) error {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				return true
			}
			var names []string
			for _, f := range fn.Type.Params.List {
				if len(f.Names) == 0 {
					names = append(names, "")
					continue
				}
				for _, id := range f.Names {
					names = append(names, id.Name)
				}
			}
			typeIdx, reasonIdx := -1, -1
			for i, nm := range names {
				switch {
				case conditionTypeParams[nm]:
					typeIdx = i
				case nm == "reason":
					reasonIdx = i
				}
			}
			if typeIdx < 0 || reasonIdx < 0 {
				return true
			}
			key := fmt.Sprintf("%s/%d/%d/%d", fn.Name.Name, len(names), typeIdx, reasonIdx)
			if seen[key] {
				return true
			}
			seen[key] = true
			sigs = append(sigs, condSetterSig{name: fn.Name.Name, arity: len(names), typeIdx: typeIdx, reasonIdx: reasonIdx})
			return true
		})
		return nil
	})
	return sigs, err
}

type condSetterSig struct {
	name      string
	arity     int
	typeIdx   int
	reasonIdx int
}

// collectEmissions walks the producer subtree for condition writes on o.condType,
// in both shapes the AGC uses: a call to a condition setter, and a metav1.Condition
// composite literal. A write whose type resolves to o.condType and whose reason does
// not resolve is a finding rather than a silent skip — an emission the scan cannot
// read is exactly the one that would slip past membership.
func collectEmissions(srcDir string, o ownership, setters []condSetterSig, reasons, conditions map[string]string) ([]emission, []string, error) {
	root := filepath.Join(srcDir, filepath.FromSlash(o.producer))
	if !dirExists(root) {
		return nil, nil, fmt.Errorf("%s: reasontiers:owns names producer %q, which is not a directory under %s", o.site, o.producer, srcDir)
	}

	var emitted []emission
	var unplaceable []string
	err := parseGo(root, func(file *ast.File, fset *token.FileSet, rel string) error {
		imports := importedPkgs(file)
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)

			typeArg, reasonArg := conditionWrite(n, setters)
			if typeArg == nil {
				return true
			}
			if v, ok := literalOrConst(typeArg, imports, conditions); !ok || v != o.condType {
				return true
			}
			site := fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line)
			if v, ok := literalOrConst(reasonArg, imports, reasons); ok {
				emitted = append(emitted, emission{reason: v, site: site})
				return true
			}
			// A parameter is the setter forwarding its caller's choice, not a site
			// that decides one — config.go's own setCondition body is this shape.
			if id, ok := reasonArg.(*ast.Ident); ok && isParam(enclosingFunc(stack), id.Name) {
				return true
			}
			unplaceable = append(unplaceable, fmt.Sprintf(
				"%s: this write to %s does not name a reason this scan can read, so membership against %s cannot be checked — pass a Reason* constant",
				site, o.condType, o.site))
			return true
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(emitted, func(i, j int) bool { return emitted[i].site < emitted[j].site })
	return emitted, unplaceable, nil
}

// conditionWrite returns the type and reason expressions of a condition write, or
// a nil type when the node is not one.
func conditionWrite(n ast.Node, setters []condSetterSig) (typeArg, reasonArg ast.Expr) {
	switch x := n.(type) {
	case *ast.CallExpr:
		name, ok := calleeName(x)
		if !ok {
			return nil, nil
		}
		for _, s := range setters {
			if s.name == name && len(x.Args) == s.arity {
				return x.Args[s.typeIdx], x.Args[s.reasonIdx]
			}
		}
	case *ast.CompositeLit:
		sel, ok := x.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Condition" {
			return nil, nil
		}
		for _, elt := range x.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Type":
				typeArg = kv.Value
			case "Reason":
				reasonArg = kv.Value
			}
		}
		// A nil reasonArg is returned rather than swallowed: the caller reports it
		// as unreadable, so a literal that sets Type here and Reason somewhere else
		// cannot pass the membership check by being invisible to it.
		return typeArg, reasonArg
	}
	return nil, nil
}

// literalOrConst resolves an argument to the string an operator sees, from a
// string literal or a qualified constant in the vocabulary.
//
// constValue is not reused here because it also applies reasonConstRE, and half
// of what this resolves is a Condition* name. The values map is already scoped to
// one kind of constant, so the package qualifier is the whole test.
func literalOrConst(e ast.Expr, imports, values map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok || !reasonPkgs[imports[pkg.Name]] {
			return "", false
		}
		v, ok := values[x.Sel.Name]
		return v, ok
	}
	return "", false
}

// checkConsumers reports a package other than the enumeration's own comparing a
// reason against one of its entries. That comparison is a second membership site
// held at its own width, which is how the two came apart: the enumeration grew a
// reason and the open-coded comparison did not, so one consumer deferred to the
// producer and the other overwrote it.
func checkConsumers(srcDir string, o ownership, reasons map[string]string) ([]string, error) {
	var findings []string
	err := parseGo(srcDir, func(file *ast.File, fset *token.FileSet, rel string) error {
		if filepath.ToSlash(filepath.Dir(rel)) == o.pkgDir {
			return nil
		}
		imports := importedPkgs(file)
		ast.Inspect(file, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				sel, ok := side.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				v, ok := constValue(sel, imports, reasons)
				if !ok {
					continue
				}
				if _, member := o.members[v]; !member {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s:%d: %s is compared against here, and it is one of the reasons %s enumerates — call that instead, or this is a second membership site at its own width",
					rel, fset.Position(bin.Pos()).Line, sel.Sel.Name, o.site))
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}
