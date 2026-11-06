// Command metricreference holds the AGC's Prometheus metrics to the operator metrics
// reference and to the source that emits them.
//
// Four checks, because a metric can go stale in four ways that all read healthy:
//
//	reference          every AGC metric has an entry in the metrics reference
//	emission           every metric has at least one emission site in the AGC source
//	values-help        the Help an operator scrapes names every derived label value
//	values-derivation  every call in a label position places on one declaration
//
// values-derivation is the one that reports the tool's own blind spot. It is loud
// because under-derivation is not a missing refusal: a value that stops being
// derived stops being demanded, so a Help string could fall behind its vocabulary
// with everything still green. values-help is aimed at what an operator reads off
// /metrics with no docs open.
//
// Emission analysis is by field name over the AST rather than by type: the metric
// structs are shared across packages (a provisioner site writes a runnercore
// field), so package-scoped resolution would miss most sites. Two metrics whose
// struct fields share a name therefore pool their sites.
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
)

// Prometheus methods that write a sample. A metric referenced only by an
// assignment or a Describe send is not emitting anything.
var emitMethods = map[string]bool{
	"Inc":                true,
	"Add":                true,
	"Dec":                true,
	"Sub":                true,
	"Set":                true,
	"Observe":            true,
	"WithLabelValues":    true,
	"With":               true,
	"DeleteLabelValues":  true,
	"DeletePartialMatch": true,
}

// Constructors that turn a *prometheus.Desc into a sample at scrape time. The
// custom collectors emit this way rather than through a Vec.
var constMetricFuncs = map[string]bool{
	"MustNewConstMetric": true,
	"NewConstMetric":     true,
}

// buildInfoMetric's label values are the build's identity, not a vocabulary Help could
// publish.
const buildInfoMetric = "actions_gateway_build_info"

var metricNameRE = regexp.MustCompile(`^actions_gateway_[a-z0-9_]+$`)

// metric is one series defined in the AGC source. A name can be defined more
// than once, which is why the checks aggregate by name rather than taking the
// first definition.
type metric struct {
	name   string
	field  string   // struct field or var the collector is held in
	file   string   // repo-relative path of the definition
	labels []string // label names, in the order a WithLabelValues call fills them
	help   string   // the Help text an operator scrapes
}

// series is every definition of one metric name, folded together.
type series struct {
	name   string
	fields []string
	files  []string
	labels []string
	help   string
}

// srcFile is one parsed non-test source file, retained so the label-value
// derivation can resolve an identifier against the package that declares it.
type srcFile struct {
	path string // repo-relative, slash-separated
	dir  string // package directory, the resolution scope
	file *ast.File
}

// byName folds the definitions into one entry per metric name, in name order.
func byName(defs []metric) []series {
	index := map[string]int{}
	var out []series
	for _, d := range defs {
		i, ok := index[d.name]
		if !ok {
			index[d.name] = len(out)
			out = append(out, series{name: d.name})
			i = len(out) - 1
		}
		out[i].fields = append(out[i].fields, d.field)
		out[i].files = append(out[i].files, d.file)
		if len(d.labels) > 0 {
			out[i].labels = d.labels
		}
		if d.help != "" {
			out[i].help = d.help
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// sites returns every file that emits any of the series' fields.
func (s series) sites(all map[string][]string) []string {
	var out []string
	for _, f := range s.fields {
		if f == "" {
			continue
		}
		out = append(out, all[f]...)
	}
	return out
}

func (s series) where() string { return strings.Join(s.files, ", ") }

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: metricreference <agc-src-dir> <observability-metrics.md>")
		os.Exit(2)
	}
	findings, err := run(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "metricreference: %v\n", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "metricreference: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
}

func run(srcDir, metricsDoc string) ([]string, error) {
	defs, sites, files, err := scanSource(srcDir)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("no actions_gateway_* metrics found under %s — the scan matched nothing, which is not a clean tree", srcDir)
	}

	docBytes, err := os.ReadFile(metricsDoc)
	if err != nil {
		return nil, err
	}
	all := byName(defs)
	values, gaps := deriveValues(files, defs)

	var findings []string
	findings = append(findings, checkReference(all, string(docBytes), metricsDoc)...)
	findings = append(findings, checkEmission(all, sites)...)
	findings = append(findings, checkValueHelp(all, values)...)
	findings = append(findings, checkDerivationGaps(gaps)...)
	sort.Strings(findings)
	return findings, nil
}

// checkDerivationGaps fails a label position whose callers could not be placed
// on one declaration, because two functions share the name and the argument at
// a given index need not be the same argument.
//
// This is reported rather than absorbed, for the reason the package comment gives.
// Renaming one of the two functions, or
// passing the value as a literal at the emission site, restores the derivation.
func checkDerivationGaps(gaps []string) []string {
	var findings []string
	for _, name := range gaps {
		findings = append(findings, fmt.Sprintf(
			"cmd/agc: %s has more than one declaration, so a call to it in a label position cannot be placed — the label's vocabulary is derived incompletely, and a value that goes underived also goes undemanded",
			name))
	}
	return findings
}

// checkValueHelp fails a Help string that has fallen behind the vocabulary the
// source emits. Help is what an operator reads off /metrics with no docs open,
// so a value missing from it reads as a value that cannot occur — which is how
// cause="vanished" and two reap reasons stayed unpublished after they shipped.
//
// Tier-neutral series are exempt along with the rest of the value checks: their
// labels carry identity rather than a vocabulary, and build_info naming its own
// version string in its Help would be noise, not publication.
func checkValueHelp(all []series, values valueSet) []string {
	var findings []string
	for _, s := range all {
		if s.help == "" || s.name == buildInfoMetric {
			continue
		}
		for label, vals := range values[s.name] {
			for _, v := range vals {
				if !strings.Contains(s.help, v.value) {
					findings = append(findings, fmt.Sprintf(
						"%s: %s emits %s=%q (%s) and its Help does not name it",
						s.where(), s.name, label, v.value, strings.Join(v.origins, ", ")))
				}
			}
		}
	}
	return findings
}

// scanSource parses every non-test Go file under srcDir, returning the metrics it
// defines, the files that emit them keyed by struct-field name, and the parsed
// files themselves for the label-value derivation.
func scanSource(srcDir string) ([]metric, map[string][]string, []srcFile, error) {
	var defs []metric
	var files []srcFile
	sites := map[string][]string{}

	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// cmd/agc/test holds the load harness, which reads the metrics
			// rather than defining the shipped set.
			if d.Name() == "testdata" || path == filepath.Join(srcDir, "test") {
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
		collect(file, rel, &defs, sites)
		files = append(files, srcFile{path: rel, dir: filepath.ToSlash(filepath.Dir(path)), file: file})
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })
	return defs, sites, files, nil
}

// collect walks one file for metric definitions and emission sites. The parent
// stack is what resolves a name literal back to the field holding the collector:
// the outermost enclosing key is the field, the innermost is the Opts key.
func collect(file *ast.File, rel string, defs *[]metric, sites map[string][]string) {
	var stack []ast.Node

	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(node.Value)
			if err != nil || !metricNameRE.MatchString(name) {
				return true
			}
			labels, help := constructorArgs(stack, name)
			*defs = append(*defs, metric{name: name, field: holderName(stack), file: rel, labels: labels, help: help})
		case *ast.CallExpr:
			if f := emittedField(node); f != "" {
				sites[f] = append(sites[f], rel)
			}
		}
		return true
	})
}

// holderName reports the identifier a metric literal is stored under: the
// outermost enclosing composite-literal key, or the var it is assigned to.
func holderName(stack []ast.Node) string {
	// The stack runs outermost first, so the first key is the field holding the
	// collector and any later one is an Opts key inside it.
	for _, n := range stack {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name == "Name" {
			continue
		}
		return id.Name
	}
	for _, n := range stack {
		switch d := n.(type) {
		case *ast.ValueSpec:
			if len(d.Names) == 1 {
				return d.Names[0].Name
			}
		case *ast.AssignStmt:
			if len(d.Lhs) == 1 {
				if id, ok := d.Lhs[0].(*ast.Ident); ok {
					return id.Name
				}
			}
		}
	}
	return ""
}

// constructorArgs reads the label names and the Help text off the prometheus
// constructor enclosing a metric-name literal. Two shapes carry them here: a Vec
// takes an Opts struct plus a []string of labels, and NewDesc takes the name,
// the help and the labels positionally.
func constructorArgs(stack []ast.Node, name string) ([]string, string) {
	var call *ast.CallExpr
	for i := len(stack) - 1; i >= 0; i-- {
		if c, ok := stack[i].(*ast.CallExpr); ok {
			call = c
			break
		}
	}
	if call == nil {
		return nil, ""
	}

	var labels []string
	var help string
	for _, arg := range call.Args {
		switch a := arg.(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(a); ok && s != name && help == "" {
				help = s
			}
		case *ast.CompositeLit:
			if _, isSlice := a.Type.(*ast.ArrayType); isSlice {
				for _, el := range a.Elts {
					if s, ok := stringLit(el); ok {
						labels = append(labels, s)
					}
				}
				continue
			}
			for _, el := range a.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Help" {
					if s, ok := stringLit(kv.Value); ok {
						help = s
					}
				}
			}
		}
	}
	return labels, help
}

// emittedField reports the field name a call writes a sample through, or "".
func emittedField(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if constMetricFuncs[sel.Sel.Name] {
		if len(call.Args) == 0 {
			return ""
		}
		return trailingName(call.Args[0])
	}
	if !emitMethods[sel.Sel.Name] {
		return ""
	}
	return trailingName(sel.X)
}

// trailingName reports the last identifier of a selector chain (the field a
// receiver expression lands on), or the identifier itself for a bare name.
func trailingName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// checkReference asserts every metric is named in the reference an operator reads,
// which is how eviction_recovery_evidence_lost_total shipped undocumented (Q809).
func checkReference(all []series, doc, docPath string) []string {
	var findings []string
	for _, s := range all {
		if !strings.Contains(doc, "`"+s.name+"`") {
			findings = append(findings, fmt.Sprintf(
				"%s: %s (defined in %s) has no entry, so an operator who scrapes it gets no description",
				docPath, s.name, s.where()))
		}
	}
	return findings
}

func checkEmission(all []series, sites map[string][]string) []string {
	var findings []string
	for _, s := range all {
		named := false
		for _, f := range s.fields {
			if f != "" {
				named = true
			}
		}
		if !named {
			findings = append(findings, fmt.Sprintf(
				"%s: %s is not held in a named field or var, so its emission sites cannot be resolved",
				s.where(), s.name))
			continue
		}
		if len(s.sites(sites)) == 0 {
			findings = append(findings, fmt.Sprintf(
				"%s: %s (field %s) is registered and never emitted — a permanent zero reads as a quiet system",
				s.where(), s.name, strings.Join(s.fields, ", ")))
		}
	}
	return findings
}
