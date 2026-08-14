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
// Six checks, because the ledger can go stale in six ways that all read healthy:
//
//	inventory     the AGC's metric names and the ledger's name the same set
//	reference     every AGC metric also has a row in the metrics reference tables
//	vocabulary    every ledger row states one of the four tiers, with a reason
//	emission      every metric has at least one emission site in the AGC source
//	contradiction no single-tier metric is emitted from the tier it excludes
//	parity        v2-ga.md's absent-by-design list is Classic only in the ledger
//
// inventory catches the metric added on one tier and never accounted for.
// contradiction catches the other direction — a port lands, the series now reaches
// both tiers, and the ledger still calls it single-tier. Those are the two ways
// the record drifted historically, so both are asserted.
//
// Emission analysis is by field name over the AST rather than by type: the metric
// structs are shared across packages (a provisioner site writes a runnercore
// field), so package-scoped resolution would miss most sites. Two metrics whose
// struct fields share a name therefore pool their sites. That cannot manufacture a
// contradiction unless one of them is emitted from a tier-exclusive file, which
// would be a naming collision worth fixing at the source.
//
// The contradiction check sees direct field writes only. A tier that increments
// through an adapter interface — the scale-set listener's PollErrorRecorder, whose
// method body sits in runnercore — writes no site in a tier-exclusive file, so its
// reach is invisible here. That is why the check is one-sided: it refutes a
// single-tier claim and never confirms one, and the ledger row is what carries the
// positive claim. inventory is the check that makes the row unavoidable.
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
	name  string
	field string // struct field or var the collector is held in
	file  string // repo-relative path of the definition
}

// series is every definition of one metric name, folded together.
type series struct {
	name   string
	fields []string
	files  []string
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

// ledger is the parsed table plus the line span it occupies, so the reference
// check can ask whether a metric is described anywhere *else* in the document
// without depending on where the section sits.
type ledger struct {
	rows       []ledgerRow
	first, end int // 1-based, half-open
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
	defs, sites, err := scanSource(srcDir)
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

	var findings []string
	findings = append(findings, checkInventory(all, led, metricsDoc)...)
	findings = append(findings, checkReference(all, doc, led, metricsDoc)...)
	findings = append(findings, checkVocabulary(led, metricsDoc)...)
	findings = append(findings, checkEmission(all, sites)...)
	findings = append(findings, checkContradiction(all, sites, led)...)
	findings = append(findings, checkParityList(string(parityBytes), led, parityDoc)...)
	sort.Strings(findings)
	return findings, nil
}

// scanSource parses every non-test Go file under srcDir, returning the metrics it
// defines and, keyed by struct-field name, the files that emit them.
func scanSource(srcDir string) ([]metric, map[string][]string, error) {
	var defs []metric
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
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })
	return defs, sites, nil
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
			*defs = append(*defs, metric{name: name, field: holderName(stack), file: rel})
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
// reference above it grows another sub-table.
const ledgerHeading = "## Acquisition-tier reach"

func parseLedger(doc string) (ledger, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == ledgerHeading {
			start = i
			break
		}
	}
	if start < 0 {
		return ledger{}, fmt.Errorf("no %q section — the tier ledger is the gate's input and cannot be absent", ledgerHeading)
	}

	var rows []ledgerRow
	inTable := false
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "## ") {
			end = i
			break
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitRow(line)
		if len(cells) < 3 {
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			inTable = true
			continue
		}
		if !inTable {
			continue // header row
		}
		name := strings.Trim(cells[0], "`")
		if !metricNameRE.MatchString(name) {
			return ledger{}, fmt.Errorf("line %d: %q is not an actions_gateway_* metric name", i+1, cells[0])
		}
		rows = append(rows, ledgerRow{name: name, tier: cells[1], note: cells[2], line: i + 1})
	}
	if len(rows) == 0 {
		return ledger{}, fmt.Errorf("the %q table is empty", ledgerHeading)
	}
	return ledger{rows: rows, first: start + 1, end: end + 1}, nil
}

func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
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
		if n := i + 1; n >= led.first && n < led.end {
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
func checkContradiction(all []series, sites map[string][]string, led ledger) []string {
	tier := map[string]string{}
	for _, r := range led.rows {
		tier[r.name] = r.tier
	}
	var findings []string
	for _, s := range all {
		for _, site := range s.sites(sites) {
			switch {
			case tier[s.name] == tierClassic && isScaleSetOnly(site):
				findings = append(findings, fmt.Sprintf(
					"%s: %s is emitted here, and the ledger calls it %q",
					site, s.name, tierClassic))
			case tier[s.name] == tierScaleSet && isClassicOnly(site):
				findings = append(findings, fmt.Sprintf(
					"%s: %s is emitted here, and the ledger calls it %q",
					site, s.name, tierScaleSet))
			}
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
