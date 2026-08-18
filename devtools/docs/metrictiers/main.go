// Command metrictiers reconciles the AGC's Prometheus metric inventory against
// the acquisition-tier ledger in the operator metrics reference (Q776).
//
// Capability parity between the classic and scale-set acquisition tiers rested on
// a one-time seam walk, and that walk went stale four times: Q683, Q691, Q713 and
// Q844 each arrived classic-only from birth, after the walk declared parity, with
// nothing re-walking it. The tier badge on docs/features.md cannot see that case —
// it fails a badge that outlived its gap, not a gap that was never badged.
//
// This gate inverts the obligation. Every actions_gateway_* series the AGC
// defines must carry a tier in the ledger, so a metric cannot reach an operator
// without someone answering which tier emits it.
//
// Twelve checks, because the ledger can go stale in twelve ways that all read
// healthy. Six are about the series as a whole:
//
//	inventory     the AGC's metric names and the ledger's name the same set
//	reference     every AGC metric also has a row in the metrics reference tables
//	vocabulary    every ledger row states one of the four tiers, with a reason
//	emission      every metric has at least one emission site in the AGC source
//	contradiction no single-tier metric is emitted from the tier it excludes
//	parity        v2-ga.md's absent-by-design list is Classic only in the ledger
//
// and six about the label values inside one (Q851), of which values-contradiction
// through values-pinned share checkValueRows, all validating one row:
//
//	values-inventory     a value the source shows tier-exclusive has a value row
//	values-contradiction no value row is refuted by where the source names it
//	values-vocabulary    every value row names a real series, label and value
//	values-pinned        an underivable value row cites the guard that holds it
//	values-help          the Help an operator scrapes names every derived value
//	values-derivation    every call in a label position places on one declaration
//
// values-derivation is the one that reports the tool's own blind spot rather
// than the ledger's. It is loud because under-derivation is not a missing
// refusal: a value that stops being derived stops being demanded, so a true row
// could leave the ledger with everything still green.
//
// inventory catches the metric added on one tier and never accounted for.
// contradiction catches the other direction — a port lands, the series now reaches
// both tiers, and the ledger still calls it single-tier. Those are the two ways
// the record drifted historically, so both are asserted.
//
// The value checks exist because a series can be Both while one of its label
// values is not: eviction_retries_total is Both and cause="vanished" is emitted
// from a scale-set-only file, which the series checks cannot see. values-help is
// the one aimed at the operator rather than the ledger — a stale Help string is
// what an operator reads off /metrics with no docs open, and seven of them named
// a vocabulary the source had outgrown, or named none at all.
//
// Emission analysis is by field name over the AST rather than by type: the metric
// structs are shared across packages (a provisioner site writes a runnercore
// field), so package-scoped resolution would miss most sites. Two metrics whose
// struct fields share a name therefore pool their sites. That cannot manufacture a
// contradiction unless one of them is emitted from a tier-exclusive file, which
// would be a naming collision worth fixing at the source.
//
// The contradiction check reads emission sites and the derived value origins
// both. Sites alone see direct field writes, so a tier that increments through an
// adapter interface — the scale-set listener's PollErrorRecorder, whose method
// body sits in runnercore — was invisible to it, and message_poll_errors_total
// took a Classic only row with everything green (Q867). The derivation walks out
// of the Prometheus call to the literal, so it names the file the site analysis
// cannot reach. The check stays one-sided even so: it refutes a single-tier claim
// and never confirms one, because a shared file says nothing about which tiers
// execute it. The ledger row carries the positive claim, and inventory is the
// check that makes the row unavoidable.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/devtools/docs/markdown"
)

// Tier vocabulary. The ledger states one of these per metric; the first three are
// the acquisition-tier answer and tierNeutral is for series with no tier concept
// (build info), which are listed so the inventory check stays total.
const (
	tierBoth     = "Both"
	tierClassic  = "Classic only"
	tierScaleSet = "Scale-set only"
	tierNeutral  = "Tier-neutral"
)

// Source subtrees that only one acquisition tier ever executes. The contradiction
// check reads these and nothing else: a shared file says nothing about the tier,
// while a site in one of these is proof the other tier's claim is wrong.
var (
	classicOnlyDirs  = []string{"internal/listener"}
	scaleSetOnlyDirs = []string{"internal/scalesetlistener"}
	// Files named *_scaleset.go hold the scale-set arm of a package that serves
	// both tiers — the convention provisioner/ and controller/ already follow.
	scaleSetOnlySuffix = "_scaleset.go"
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

var metricNameRE = regexp.MustCompile(`^actions_gateway_[a-z0-9_]+$`)

// metric is one series defined in the AGC source. A name can be defined more
// than once: message_poll_errors_total is built by both listeners under
// different fields so the two tiers write one series (Q446), which is why the
// checks aggregate by name rather than taking the first definition.
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

// ledgerRow is one row of the Acquisition-tier reach table.
type ledgerRow struct {
	name string
	tier string
	note string
	line int
}

// valueRow is one row of the Label-value reach table: a label value inside a
// series whose own row cannot state its tier.
type valueRow struct {
	metric string
	label  string
	value  string
	tier   string
	note   string
	line   int
}

// ledger is the parsed tables plus the line span they occupy, so the reference
// check can ask whether a metric is described anywhere *else* in the document
// without depending on where the section sits.
type ledger struct {
	rows       []ledgerRow
	values     []valueRow
	first, end int // 1-based, inclusive: the section body, heading excluded
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: metrictiers <agc-src-dir> <observability-metrics.md> <v2-ga.md>")
		os.Exit(2)
	}
	findings, err := run(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrictiers: %v\n", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "metrictiers: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
}

func run(srcDir, metricsDoc, parityDoc string) ([]string, error) {
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
	doc := string(docBytes)

	led, err := parseLedger(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", metricsDoc, err)
	}

	parityBytes, err := os.ReadFile(parityDoc)
	if err != nil {
		return nil, err
	}

	all := byName(defs)
	values, gaps := deriveValues(files, defs)

	var findings []string
	findings = append(findings, checkInventory(all, led, metricsDoc)...)
	findings = append(findings, checkReference(all, doc, led, metricsDoc)...)
	findings = append(findings, checkVocabulary(led, metricsDoc)...)
	findings = append(findings, checkEmission(all, sites)...)
	findings = append(findings, checkContradiction(all, sites, values, led)...)
	findings = append(findings, checkParityList(string(parityBytes), led, parityDoc)...)
	findings = append(findings, checkValueInventory(all, values, led, metricsDoc)...)
	findings = append(findings, checkValueRows(all, values, led, files, metricsDoc)...)
	findings = append(findings, checkValueHelp(all, values, led)...)
	findings = append(findings, checkDerivationGaps(gaps)...)
	sort.Strings(findings)
	return findings, nil
}

// checkDerivationGaps fails a label position whose callers could not be placed
// on one declaration, because two functions share the name and the argument at
// a given index need not be the same argument.
//
// This is reported rather than absorbed. Every other check here is one-sided and
// silent about what it cannot see, but under-derivation is not a missing refusal:
// a value that stops being derived stops being demanded, so a true row can leave
// the ledger with the gate still green. Renaming one of the two functions, or
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

// checkValueInventory fails a label value the source shows reaching one tier
// only, inside a series the ledger does not already call single-tier. This is
// the value-granularity form of the obligation inventory imposes on a series:
// cause="vanished" is named in a scale-set-only file while
// eviction_retries_total reads Both, so without a value row the ledger says a
// series populates on both tiers and stays silent about the value that does not.
func checkValueInventory(all []series, values valueSet, led ledger, docPath string) []string {
	tier := map[string]string{}
	for _, r := range led.rows {
		tier[r.name] = r.tier
	}
	claimed := map[string]string{}
	for _, v := range led.values {
		claimed[valueKey(v.metric, v.label, v.value)] = v.tier
	}

	var findings []string
	for _, s := range all {
		if tier[s.name] == tierNeutral {
			continue
		}
		for label, vals := range values[s.name] {
			for _, v := range vals {
				derived := derivedTier(v.origins)
				if derived == "" || derived == tier[s.name] {
					continue
				}
				if claimed[valueKey(s.name, label, v.value)] == derived {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s: %s{%s=%q} is named only in %s, so it is %q while the ledger calls the series %q — give it a %q row",
					docPath, s.name, label, v.value, strings.Join(v.origins, ", "), derived, tier[s.name], valueHeading))
			}
		}
	}
	return findings
}

// checkValueRows holds the label-value table to the source: every row names a
// series, a label and a value that exist, states one of the two single-tier
// answers with a reason, and is not refuted by where the source names the value.
// A row the file layout cannot derive has to cite the guard instead, so a claim
// like outcome="identity_unknown" being unreachable on the scale-set tier stays
// anchored to the early return that makes it so rather than to a reviewer's memory.
func checkValueRows(all []series, values valueSet, led ledger, files []srcFile, docPath string) []string {
	labels := map[string][]string{}
	for _, s := range all {
		labels[s.name] = s.labels
	}
	// Citations are matched by suffix, not equality: the gate is invoked with a
	// relative source path by the Makefile and an absolute one by its own test
	// suite, so the scanned paths carry a prefix the doc cannot know.
	scanned := func(cited string) bool {
		for _, f := range files {
			if f.path == cited || strings.HasSuffix(f.path, "/"+cited) {
				return true
			}
		}
		return false
	}

	var findings []string
	for _, v := range led.values {
		at := fmt.Sprintf("%s:%d", docPath, v.line)
		byLabel, known := values[v.metric]
		if !known {
			findings = append(findings, fmt.Sprintf(
				"%s: %s has a %q row and no AGC source defines it", at, v.metric, valueHeading))
			continue
		}
		if !slices.Contains(labels[v.metric], v.label) {
			findings = append(findings, fmt.Sprintf(
				"%s: %s has no %q label — it carries %s", at, v.metric, v.label, strings.Join(labels[v.metric], ", ")))
			continue
		}
		if v.label == tierLabel {
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s=%q} states the acquisition tier as its own value, which the series row already answers",
				at, v.metric, v.label, v.value))
			continue
		}
		if v.tier != tierClassic && v.tier != tierScaleSet {
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s=%q} has tier %q — a value row records an exception, so it is %q or %q",
				at, v.metric, v.label, v.value, v.tier, tierClassic, tierScaleSet))
			continue
		}
		if v.note == "" {
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s=%q} is %q with no reason — say why the other tier never emits it, and what it counts there instead",
				at, v.metric, v.label, v.value, v.tier))
		}

		var origins []string
		found := false
		for _, dv := range byLabel[v.label] {
			if dv.value == v.value {
				origins, found = dv.origins, true
				break
			}
		}
		if !found {
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s=%q} has a %q row and no site in the AGC source names that value",
				at, v.metric, v.label, v.value, valueHeading))
			continue
		}

		if opposite := oppositeTier(v.tier); opposite != "" {
			for _, o := range origins {
				if (opposite == tierClassic && isClassicOnly(o)) || (opposite == tierScaleSet && isScaleSetOnly(o)) {
					findings = append(findings, fmt.Sprintf(
						"%s: %s{%s=%q} is named here, and the ledger calls it %q",
						o, v.metric, v.label, v.value, v.tier))
				}
			}
		}

		// A value the file layout already proves needs no citation; one it does
		// not is held by a guard in a shared file, and the row must point at it.
		if derivedTier(origins) == v.tier {
			continue
		}
		cited := citesGoFile(v.note)
		if len(cited) == 0 {
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s=%q} is %q and the source names it in %s, which does not say so — cite the .go file whose guard does",
				at, v.metric, v.label, v.value, v.tier, strings.Join(origins, ", ")))
			continue
		}
		for _, c := range cited {
			if !scanned(c) {
				findings = append(findings, fmt.Sprintf(
					"%s: %s{%s=%q} cites %s, which is not a file the AGC scan parsed",
					at, v.metric, v.label, v.value, c))
			}
		}
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
func checkValueHelp(all []series, values valueSet, led ledger) []string {
	tier := map[string]string{}
	for _, r := range led.rows {
		tier[r.name] = r.tier
	}
	var findings []string
	for _, s := range all {
		if s.help == "" || tier[s.name] == tierNeutral {
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

func valueKey(metric, label, value string) string { return metric + "\x00" + label + "\x00" + value }

func oppositeTier(t string) string {
	switch t {
	case tierClassic:
		return tierScaleSet
	case tierScaleSet:
		return tierClassic
	}
	return ""
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

// ledgerHeading is the section the ledger table lives under. Matching on the
// heading rather than on the first table keeps the parse anchored when the
// reference above it grows another sub-table. Both constants carry their marker
// because findings quote them; the parse takes the level separately.
const ledgerHeading = "## Acquisition-tier reach"

// valueHeading is the sub-section holding the label-value table. It sits inside
// the ledger section so the reference check keeps treating the whole span as
// tier bookkeeping rather than as a metric's description.
const valueHeading = "### Label-value reach"

func parseLedger(doc string) (ledger, error) {
	parsed := markdown.Parse([]byte(doc))
	first, end, ok := parsed.SectionRange(2, strings.TrimPrefix(ledgerHeading, "## "))
	if !ok {
		return ledger{}, fmt.Errorf("no %q section — the tier ledger is the gate's input and cannot be absent", ledgerHeading)
	}
	valueFirst, valueEnd, hasValues := parsed.SectionRange(3, strings.TrimPrefix(valueHeading, "### "))

	var rows []ledgerRow
	var values []valueRow
	for _, tbl := range parsed.Tables() {
		if tbl.Line < first || tbl.Line > end {
			continue
		}
		inValues := hasValues && tbl.Line >= valueFirst && tbl.Line <= valueEnd
		for _, r := range tbl.Rows {
			if len(r.Cells) < 3 {
				continue
			}
			name := strings.Trim(r.Cells[0], "`")
			if !metricNameRE.MatchString(name) {
				return ledger{}, fmt.Errorf("line %d: %q is not an actions_gateway_* metric name", r.Line, r.Cells[0])
			}
			if inValues {
				if len(r.Cells) < 5 {
					return ledger{}, fmt.Errorf("line %d: a %q row needs metric, label, value, tier and a reason", r.Line, valueHeading)
				}
				values = append(values, valueRow{
					metric: name,
					label:  strings.Trim(r.Cells[1], "`"),
					value:  strings.Trim(r.Cells[2], "`"),
					tier:   r.Cells[3],
					note:   r.Cells[4],
					line:   r.Line,
				})
				continue
			}
			rows = append(rows, ledgerRow{name: name, tier: r.Cells[1], note: r.Cells[2], line: r.Line})
		}
	}
	if len(rows) == 0 {
		return ledger{}, fmt.Errorf("the %q table is empty", ledgerHeading)
	}
	return ledger{rows: rows, values: values, first: first, end: end}, nil
}

func checkInventory(all []series, led ledger, docPath string) []string {
	inLedger := map[string]bool{}
	for _, r := range led.rows {
		inLedger[r.name] = true
	}
	inCode := map[string]bool{}
	var findings []string
	for _, s := range all {
		inCode[s.name] = true
		if !inLedger[s.name] {
			findings = append(findings, fmt.Sprintf(
				"%s: %s is defined in %s and has no row in %q — state which acquisition tier emits it",
				docPath, s.name, s.where(), ledgerHeading))
		}
	}
	for _, r := range led.rows {
		if !inCode[r.name] {
			findings = append(findings, fmt.Sprintf(
				"%s:%d: %s is listed in %q but no AGC source defines it",
				docPath, r.line, r.name, ledgerHeading))
		}
	}
	return findings
}

// checkReference asserts the ledger never becomes the only place a metric is
// described. The reference tables are what an operator reads; a series that
// reaches the ledger alone is documented by its tier and nothing else, which is
// how eviction_recovery_evidence_lost_total shipped undocumented (Q809).
func checkReference(all []series, doc string, led ledger, docPath string) []string {
	lines := strings.Split(doc, "\n")
	var body []string
	for i, l := range lines {
		if n := i + 1; n >= led.first && n <= led.end {
			continue
		}
		body = append(body, l)
	}
	outside := strings.Join(body, "\n")

	var findings []string
	for _, s := range all {
		if !strings.Contains(outside, "`"+s.name+"`") {
			findings = append(findings, fmt.Sprintf(
				"%s: %s (defined in %s) is in the tier ledger and nowhere else — an operator gets its tier and no description",
				docPath, s.name, s.where()))
		}
	}
	return findings
}

func checkVocabulary(led ledger, docPath string) []string {
	var findings []string
	for _, r := range led.rows {
		switch r.tier {
		case tierBoth, tierNeutral:
		case tierClassic, tierScaleSet:
			if r.note == "" {
				findings = append(findings, fmt.Sprintf(
					"%s:%d: %s is %q with no reason — say why the other tier does not emit it, and what to read there instead",
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

// checkEmission fails a metric nothing writes a sample through. A registered
// collector with no emission site publishes a permanent zero, which an operator
// cannot tell from a quiet system.
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

// checkContradiction fails a single-tier claim the source refutes. This is the
// stale-after-a-port direction: Q766 moved the abandoned-run recovery onto the
// scale-set tier, and nothing would have reported a ledger still calling it
// classic-only.
//
// Two kinds of evidence, because an emission site is not the only place a tier's
// reach shows. A tier that increments through an adapter writes no site in a
// tier-exclusive file: the scale-set listener hands a reason to runnercore's
// PollErrorRecorder, whose method body holds the Prometheus call, so
// message_poll_errors_total read Classic only with the gate green (Q867). The
// value derivation walks the other way — from the call out to the literal — and
// its origins name scalesetlistener where the sites do not, so a value origin in
// the excluded tier's subtree refutes the row as well (Q851).
//
// An origin is read only once it has been placed on one declaration, so a
// same-named function elsewhere cannot lend a file to a series it never emits;
// an unplaceable call goes to checkDerivationGaps instead.
func checkContradiction(all []series, sites map[string][]string, values valueSet, led ledger) []string {
	tier := map[string]string{}
	for _, r := range led.rows {
		tier[r.name] = r.tier
	}
	var findings []string
	for _, s := range all {
		excluded := oppositeTier(tier[s.name])
		if excluded == "" {
			continue
		}
		refutes := isScaleSetOnly
		if excluded == tierClassic {
			refutes = isClassicOnly
		}

		reported := map[string]bool{}
		for _, site := range s.sites(sites) {
			if !refutes(site) || reported[site] {
				continue
			}
			reported[site] = true
			findings = append(findings, fmt.Sprintf(
				"%s: %s is emitted here, and the ledger calls it %q",
				site, s.name, tier[s.name]))
		}

		// One finding per file rather than per value: the row states one claim
		// about the series, and every reason a file names refutes the same claim.
		named := map[string][]string{}
		for label, vals := range values[s.name] {
			for _, v := range vals {
				for _, o := range v.origins {
					if !refutes(o) || reported[o] {
						continue
					}
					named[o] = append(named[o], fmt.Sprintf("%s=%q", label, v.value))
				}
			}
		}
		for _, o := range slices.Sorted(maps.Keys(named)) {
			slices.Sort(named[o])
			findings = append(findings, fmt.Sprintf(
				"%s: %s{%s} is named here, and the ledger calls the series %q — the value is named here and counted elsewhere, so the emission sites alone do not show this tier's reach",
				o, s.name, strings.Join(named[o], ", "), tier[s.name]))
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

// absentByDesignRE pulls the metric names out of the v2-ga.md paragraph that
// lists what the scale-set tier omits on purpose. That list gates the classic
// removal, so it must agree with the ledger the operator reads.
var absentByDesignRE = regexp.MustCompile("`(actions_gateway_)?([a-z0-9_]+)`")

const absentByDesignAnchor = "**Correctly absent from the scale-set tier**"

func checkParityList(parityDoc string, led ledger, path string) []string {
	i := strings.Index(parityDoc, absentByDesignAnchor)
	if i < 0 {
		return []string{fmt.Sprintf(
			"%s: no %q paragraph — the absent-by-design list gates the classic removal and cannot go missing",
			path, absentByDesignAnchor)}
	}
	rest := parityDoc[i:]
	if end := strings.Index(rest, "\n\n"); end > 0 {
		rest = rest[:end]
	}

	tier := map[string]string{}
	for _, r := range led.rows {
		tier[r.name] = r.tier
	}

	var findings []string
	for _, m := range absentByDesignRE.FindAllStringSubmatch(rest, -1) {
		name := "actions_gateway_" + m[2]
		got, listed := tier[name]
		if !listed {
			continue // prose, not a metric name
		}
		if got != tierClassic {
			findings = append(findings, fmt.Sprintf(
				"%s: %s is named absent-by-design on the scale-set tier, and the ledger calls it %q",
				path, name, got))
		}
	}
	return findings
}
