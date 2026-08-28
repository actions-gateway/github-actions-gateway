// Command rungorder reconciles the order of the classic admission ladder's rungs
// as the design doc lists them against the order Provisioner.Admit evaluates
// them in.
//
// The ladder's order is load-bearing, not presentational. The rate rung comes
// last precisely so that nothing refuses after the bucket has been charged, and
// Q977 is filed against a transient that exists only because the ceiling rung
// reserves before the rate rung refuses. A doc that lists the two the other way
// round describes a system with different failure modes from the one that ships.
//
// It drifted undetected: 04-operational-flows.md listed Rate before Ceiling from
// Q717 until Q972 happened to edit that paragraph. Prose and code are the two
// halves nothing paired, which is the gap metrictiers and reasontiers already
// close for a metric's tier and a reason's tier.
//
// The code side is read from the AST rather than by grepping for the constants:
// order is the whole question here, and a grep reports file order for a
// declaration list as readily as for an evaluation sequence. Taking the rungs
// from Admit's own body in position order means the checker reads the sequence
// the gate actually walks.
//
// Usage:
//
//	rungorder <admission.go> <flows.md>
//
// Exits 1 on a finding, 2 on a read it could not take. A read it could not take
// is never a pass: a doc block that has moved, or an Admit this cannot find, is
// reported rather than treated as agreement, because a check whose subject is
// absent has verified nothing.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
)

// rungReason maps the doc's rung label to the AdmitReason* constant Admit
// returns for that rung. It is the one hand-maintained pairing here, so an entry
// missing from either side is a finding rather than a skip: a rung added to the
// code with no label, or a label with no rung, means the two halves have stopped
// describing the same ladder, which is the drift this exists to catch.
//
// "Rate" pairs with AdmitReasonScaleUp because the doc names the knob's function
// and the constant names the spec field.
var rungReason = map[string]string{
	"Quota":    "AdmitReasonQuota",
	"Capacity": "AdmitReasonCapacity",
	"Ceiling":  "AdmitReasonCeiling",
	"Rate":     "AdmitReasonScaleUp",
}

// rungLine matches a doc rung: a bold single-word label opening a line. The
// anchor line above the block carries parentheses in its label, so it cannot
// match this even if the rung scan starts too early.
var rungLine = regexp.MustCompile(`^\s*\*\*([A-Z][A-Za-z]*):\*\*`)

// docAnchor opens the ladder block. Its own trailing text is prose, so the rung
// scan starts on the following line.
const docAnchor = "**Admit ("

// reasonPrefix is the constant-name prefix Admit returns as its refusal reason.
const reasonPrefix = "AdmitReason"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rungorder <admission.go> <flows.md>")
		os.Exit(2)
	}
	srcPath, docPath := os.Args[1], os.Args[2]

	code, err := readCodeOrder(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rungorder: %v\n", err)
		os.Exit(2)
	}
	doc, err := readDocOrder(docPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rungorder: %v\n", err)
		os.Exit(2)
	}

	findings := compare(srcPath, docPath, code, doc)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		os.Exit(1)
	}
	fmt.Printf("check-rung-order: ok (%d rungs, %s order matches %s)\n",
		len(code), docPath, srcPath)
}

// readCodeOrder returns the AdmitReason* constants Admit refuses with, in the
// order its body evaluates them. The result is deduplicated on first occurrence,
// so a reason returned twice is ordered by where the ladder reaches it first.
func readCodeOrder(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Admit" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		body = fn.Body
		break
	}
	if body == nil {
		return nil, fmt.Errorf("%s declares no method Admit; the ladder this checks has moved", path)
	}

	var order []string
	seen := map[string]bool{}
	// ast.Inspect walks in source order, and the rungs live inside the closure
	// Admit returns, so the walk has to cover the whole body rather than only its
	// top-level statements.
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, reasonPrefix) {
			return true
		}
		if !seen[sel.Sel.Name] {
			seen[sel.Sel.Name] = true
			order = append(order, sel.Sel.Name)
		}
		return true
	})
	if len(order) < 2 {
		return nil, fmt.Errorf("%s: found %d %s* constant(s) in Admit; expected the whole ladder, so this is a read failure rather than agreement",
			path, len(order), reasonPrefix)
	}
	return order, nil
}

// readDocOrder returns the ladder's rung labels in the order the doc lists them.
func readDocOrder(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	anchor := -1
	for i, line := range lines {
		if strings.Contains(line, docAnchor) {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return nil, fmt.Errorf("%s: no ladder block found (no line contains %q); the block has moved or been reworded",
			path, docAnchor)
	}

	var order []string
	for _, line := range lines[anchor+1:] {
		m := rungLine.FindStringSubmatch(line)
		if m == nil {
			break
		}
		order = append(order, m[1])
	}
	if len(order) < 2 {
		return nil, fmt.Errorf("%s: ladder block at line %d lists %d rung(s); expected the whole ladder, so this is a read failure rather than agreement",
			path, anchor+1, len(order))
	}
	return order, nil
}

// compare reconciles the two orders and returns one line per finding. An
// unpairable rung on either side is reported before an order mismatch, because a
// missing pairing makes the order comparison meaningless rather than merely
// wrong.
func compare(srcPath, docPath string, code, doc []string) []string {
	var findings []string

	reasonRung := make(map[string]string, len(rungReason))
	for rung, reason := range rungReason {
		reasonRung[reason] = rung
	}

	var want []string
	for _, rung := range doc {
		reason, ok := rungReason[rung]
		if !ok {
			findings = append(findings, fmt.Sprintf(
				"%s: ladder rung %q has no %s* constant in this checker's table; pair it or rename the rung",
				docPath, rung, reasonPrefix))
			continue
		}
		want = append(want, reason)
	}
	for _, reason := range code {
		if _, ok := reasonRung[reason]; !ok {
			findings = append(findings, fmt.Sprintf(
				"%s: Admit refuses with %s, which no ladder rung in %s documents; add the rung or pair the constant",
				srcPath, reason, docPath))
		}
	}
	if len(findings) > 0 {
		return findings
	}

	if len(want) != len(code) {
		return append(findings, fmt.Sprintf(
			"admission ladder rung count differs: %s lists %d (%s), %s evaluates %d (%s)",
			docPath, len(want), strings.Join(doc, " -> "),
			srcPath, len(code), strings.Join(code, " -> ")))
	}
	for i := range want {
		if want[i] != code[i] {
			return append(findings, fmt.Sprintf(
				"admission ladder order differs at rung %d: %s lists %s (%s), %s evaluates %s\n  doc:  %s\n  code: %s",
				i+1, docPath, doc[i], want[i], srcPath, code[i],
				strings.Join(doc, " -> "), strings.Join(code, " -> ")))
		}
	}
	return findings
}
