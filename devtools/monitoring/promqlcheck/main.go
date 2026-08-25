// Command promqlcheck validates the shipped monitoring artifacts (Q827, Q818).
//
// deploy/monitoring/prometheusrule.yaml is an appliable artifact — its README
// tells an operator to kubectl apply it — but nothing parsed its PromQL, so a
// malformed expression merged and then silently never fired. That is the same
// failure mode the alerts themselves exist to catch, arriving through the alerts.
//
// Four checks, because the shipped monitoring artifacts can be wrong in four
// ways that all read as healthy:
//
//	expr        every rule's expression parses as PromQL
//	mirror      the alerting doc reproduces the shipped rules exactly
//	runbook     every alert's runbook_url resolves to a runbook heading
//	dashboard   every Grafana panel target's expr parses as PromQL
//
// The dashboard check is Q910. manifest-validate parses the dashboards with
// `jq empty`, which asserts the JSON is well formed and says nothing about the
// query strings inside it — `sum by ((((` is a valid JSON string. The dashboards
// are appliable artifacts by the same argument as the rule file: their README
// tells an operator to import them, and a panel whose query does not parse shows
// an error where a number should be.
//
// The mirror check is why this is a Go program rather than promtool. promtool
// validates expressions and nothing else, cannot read a PrometheusRule (it wants
// a top-level `groups:`, not `spec.groups`), and would need yq or python3+pyyaml
// on PATH to extract the spec first. Reading the YAML here costs one dependency
// the module already had.
//
// Both directions are asserted on every set comparison: a rule shipped but
// undocumented and a rule documented but unshipped are both drift, and Q818 was
// the second kind — a rule the docs described for weeks that no operator ever
// received.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"
)

// rule is one entry under a group. Only the fields the checks read are named;
// the mirror check compares the raw decoded form instead, so an added field
// cannot slip past by being absent from this struct.
type rule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	Annotations map[string]string `yaml:"annotations"`
}

type group struct {
	Name  string `yaml:"name"`
	Rules []rule `yaml:"rules"`
}

type prometheusRule struct {
	Spec struct {
		Groups []group `yaml:"groups"`
	} `yaml:"spec"`
}

type docGroups struct {
	Groups []group `yaml:"groups"`
}

// panel is one Grafana panel. A row panel nests its children under `panels`
// while collapsed and promotes them to siblings while expanded, so the walk
// recurses rather than reading the top level only — a collapsed row would
// otherwise hide every query under it.
type panel struct {
	Title   string   `json:"title"`
	Panels  []panel  `json:"panels"`
	Targets []target `json:"targets"`
}

type target struct {
	RefID string `json:"refId"`
	Expr  string `json:"expr"`
}

// templateVar is one entry under templating.list. Query is a RawMessage because
// Grafana spells it two ways: an object for a query variable, a bare string for a
// textbox or constant, and only the second carries a value worth substituting.
type templateVar struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Query json.RawMessage `json:"query"`
}

type dashboard struct {
	Panels     []panel `json:"panels"`
	Templating struct {
		List []templateVar `json:"list"`
	} `json:"templating"`
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: promqlcheck <prometheusrule.yaml> <observability-alerting.md> <runbook.md> [dashboard.json...]")
		os.Exit(2)
	}
	findings, targets, err := run(os.Args[1], os.Args[2], os.Args[3], os.Args[4:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "promqlcheck: %v\n", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Printf("%s\n", f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "promqlcheck: %d finding(s)\n", len(findings))
		os.Exit(1)
	}
	// The dashboard target count is printed rather than merely tallied: it is
	// what a reader checks the gate's reach against, and a walk that quietly
	// stopped reaching panels reads as a pass otherwise.
	fmt.Printf("promqlcheck: ok (%s, %d dashboard target(s))\n", os.Args[1], targets)
}

// run returns the findings and the number of dashboard targets parsed.
func run(rulePath, docPath, runbookPath string, dashboardPaths []string) ([]string, int, error) {
	ruleRaw, err := os.ReadFile(rulePath)
	if err != nil {
		return nil, 0, err
	}
	var shipped prometheusRule
	if err := yaml.Unmarshal(ruleRaw, &shipped); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", rulePath, err)
	}
	if len(shipped.Spec.Groups) == 0 {
		return nil, 0, fmt.Errorf("%s: no groups under .spec", rulePath)
	}

	docRaw, err := os.ReadFile(docPath)
	if err != nil {
		return nil, 0, err
	}
	runbookRaw, err := os.ReadFile(runbookPath)
	if err != nil {
		return nil, 0, err
	}

	var findings []string
	findings = append(findings, checkExprs(rulePath, shipped.Spec.Groups)...)

	documented, err := docBlocks(docRaw)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", docPath, err)
	}
	findings = append(findings, checkMirror(rulePath, docPath, shipped.Spec.Groups, documented)...)
	findings = append(findings, checkRunbook(rulePath, runbookPath, shipped.Spec.Groups, runbookRaw)...)

	targets := 0
	for _, path := range dashboardPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		f, n, err := checkDashboard(path, raw)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
		findings = append(findings, f...)
		targets += n
	}
	return findings, targets, nil
}

// checkExprs parses every expression. A recording rule's expression is checked
// too: it is the same PromQL and the same silent-failure mode.
func checkExprs(rulePath string, groups []group) []string {
	p := parser.NewParser(parser.Options{})
	var findings []string
	for _, g := range groups {
		for _, r := range g.Rules {
			if _, err := p.ParseExpr(r.Expr); err != nil {
				findings = append(findings, fmt.Sprintf("%s: %s/%s: expr does not parse: %v",
					rulePath, g.Name, ruleName(r), err))
			}
		}
	}
	return findings
}

// checkDashboard parses every panel target's expression, and asserts the walk
// reached at least one. The count is the half that catches a restructured
// dashboard: a panel tree this walk no longer reaches would otherwise report
// clean by checking nothing, which is exactly the state Q910 found `jq empty`
// in.
//
// Expressions are substituted before parsing. A query variable sits inside a
// quoted label matcher (namespace=~"$namespace"), which is a valid label value
// and is left as written; a variable in syntactic position is not, so
// `[$__range]` and `* $rate` are resolved by substituteVars first.
func checkDashboard(path string, raw []byte) ([]string, int, error) {
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, 0, err
	}
	vars, declared := dashboardVars(d)

	p := parser.NewParser(parser.Options{})
	var findings []string
	targets := 0
	var walk func(where string, panels []panel)
	walk = func(where string, panels []panel) {
		for _, pan := range panels {
			title := pan.Title
			if where != "" {
				title = where + "/" + title
			}
			for _, t := range pan.Targets {
				targets++
				if t.Expr == "" {
					findings = append(findings, fmt.Sprintf("%s: panel %q target %q has no expr",
						path, title, t.RefID))
					continue
				}
				expr, unknown := substituteVars(t.Expr, vars, declared)
				for _, name := range unknown {
					findings = append(findings, fmt.Sprintf("%s: panel %q target %q: uses $%s, which the dashboard declares no template variable for",
						path, title, t.RefID, name))
				}
				if _, err := p.ParseExpr(expr); err != nil {
					findings = append(findings, fmt.Sprintf("%s: panel %q target %q: expr does not parse: %v",
						path, title, t.RefID, err))
				}
			}
			walk(title, pan.Panels)
		}
	}
	walk("", d.Panels)

	if targets == 0 {
		findings = append(findings, fmt.Sprintf("%s: no panel target found — the dashboard check is not exercising anything", path))
	}
	return findings, targets, nil
}

// grafanaVar matches one variable reference in either spelling Grafana accepts.
var grafanaVar = regexp.MustCompile(`\$(?:\{(\w+)\}|(\w+))`)

// builtinVars are the Grafana built-ins, with a stand-in of the right PromQL
// shape for each. Values are arbitrary within that shape: the check is that the
// surrounding expression is well formed, not that the window is a particular
// length.
//
// The shape is what matters, and it is not uniform. The interval family expands
// to a duration and is written in a range selector; the rest expand to a number
// and are written in scalar position, as in `rate(x[$__interval]) *
// $__interval_ms / 1000`. A number where a duration belongs does not parse and
// nor does the reverse, so one stand-in cannot serve both.
//
// Substituting the numeric ones is not merely cosmetic. Exempting them from the
// undeclared-variable finding is enough only where they sit inside a quoted
// matcher; in scalar position, which is the only place `$__interval_ms` and
// `$__range_s` are ever written, an unsubstituted name reaches the parser and
// fails there instead.
var builtinVars = map[string]string{
	"__range":         "5m",
	"__interval":      "5m",
	"__rate_interval": "5m",
	"__range_s":       "300",
	"__range_ms":      "300000",
	"__interval_ms":   "300000",
	"__from":          "0",
	"__to":            "0",
}

// isBuiltin reports whether name is supplied by Grafana rather than by the
// dashboard. The `__` prefix is Grafana's own reserved namespace, so this stays
// correct as they add more, which the map above would not.
//
// The two work together: builtinVars carries a stand-in for every built-in whose
// shape is known, and this exempts the rest from the undeclared-variable finding,
// which would otherwise name a cause that cannot be true and invite the repair of
// inventing a templating entry. One not in the map still fails to parse in
// syntactic position, which is honest — its shape is unknown, and a guessed
// stand-in of the wrong shape would fail anyway.
func isBuiltin(name string) bool { return strings.HasPrefix(name, "__") }

// dashboardVars returns the substitutions a dashboard's own templating declares,
// and the set of every name it declares at all.
//
// The two differ, and the gap is the point: the literal-valued types reach scalar
// or range position and must be substituted, while a query variable resolves to a
// label value and parses as written. Both are declared, so both are exempt from
// the undeclared-variable finding; only the first is rewritten.
//
// `interval` is the one worth naming: it exists to fill a range selector, so it is
// what a dashboard needing a variable window reaches for first. Its value and
// `custom`'s are comma-separated option lists, and Grafana defaults to the first,
// so that is what stands in.
func dashboardVars(d dashboard) (map[string]string, map[string]bool) {
	vars := make(map[string]string, len(builtinVars))
	for k, v := range builtinVars {
		vars[k] = v
	}
	declared := make(map[string]bool, len(d.Templating.List))
	for _, v := range d.Templating.List {
		declared[v.Name] = true
		if !literalVarTypes[v.Type] {
			continue
		}
		if literal := firstOption(v.Query); literal != "" {
			vars[v.Name] = literal
		}
	}
	return vars, declared
}

// literalVarTypes are the templating types whose value is a literal the
// expression needs, rather than a label value it already parses as.
var literalVarTypes = map[string]bool{
	"textbox":  true,
	"constant": true,
	"custom":   true,
	"interval": true,
}

// firstOption reads a template variable's default out of the two shapes Grafana
// writes it in — a bare string, or an object carrying its own `query` key — and
// takes the first entry of a comma-separated option list, which is what Grafana
// selects when nothing else is stored.
func firstOption(raw json.RawMessage) string {
	var literal string
	if json.Unmarshal(raw, &literal) != nil {
		var obj struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		literal = obj.Query
	}
	first, _, _ := strings.Cut(literal, ",")
	return strings.TrimSpace(first)
}

// substituteVars rewrites the variable references in expr with parse-valid
// stand-ins, and returns the names the dashboard declares nowhere.
//
// An unknown name is returned rather than substituted, so the expression still
// reaches the parser carrying it and fails there too. Substituting a default for
// one would let a panel referencing a variable that does not exist parse clean
// and then render an error, which is the state this whole check exists to catch.
//
// The match runs over the raw expression with no position awareness, so what it
// detects is "a $word here we could not attribute" rather than strictly an
// undeclared variable: a literal `$` inside a quoted label value is reported too.
// That is the intended trade. A misspelled variable lands inside a quoted matcher
// (namespace=~"$namespaec"), which is precisely where the check has to reach, and
// making it position-aware would exempt the case it exists for.
func substituteVars(expr string, vars map[string]string, declared map[string]bool) (string, []string) {
	var unknown []string
	seen := map[string]bool{}
	out := grafanaVar.ReplaceAllStringFunc(expr, func(m string) string {
		name := strings.Trim(m[1:], "{}")
		if v, ok := vars[name]; ok {
			return v
		}
		if !declared[name] && !isBuiltin(name) && !seen[name] {
			seen[name] = true
			unknown = append(unknown, name)
		}
		return m
	})
	return out, unknown
}

// fencedYAML matches the ```yaml blocks the alerting doc reproduces the rules in.
var fencedYAML = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// docBlocks decodes every fenced yaml block in the doc that carries a top-level
// `groups:` key, in document order. Other yaml blocks (config snippets) are
// skipped rather than erroring, so the doc can gain an example without the gate
// deciding it is a rule set.
func docBlocks(doc []byte) ([]group, error) {
	var out []group
	for _, m := range fencedYAML.FindAllSubmatch(doc, -1) {
		var d docGroups
		if err := yaml.Unmarshal(m[1], &d); err != nil {
			continue // not a groups block; a config example
		}
		out = append(out, d.Groups...)
	}
	if len(out) == 0 {
		return nil, errors.New("no fenced yaml block with a top-level `groups:` key")
	}
	return out, nil
}

// checkMirror asserts the doc reproduces the shipped rules exactly, in both
// directions. The doc says the blocks "are the same rules"; before Q818 they
// were not, and the rule the doc described had never shipped.
func checkMirror(rulePath, docPath string, shipped, documented []group) []string {
	var findings []string
	shippedByName := byName(shipped)
	docByName := byName(documented)

	for name := range shippedByName {
		if _, ok := docByName[name]; !ok {
			findings = append(findings, fmt.Sprintf("%s: group %q is shipped but not reproduced in %s",
				rulePath, name, docPath))
		}
	}
	for name := range docByName {
		if _, ok := shippedByName[name]; !ok {
			findings = append(findings, fmt.Sprintf("%s: group %q is documented but not shipped",
				docPath, name))
		}
	}

	for _, name := range sortedKeys(shippedByName) {
		docGroup, ok := docByName[name]
		if !ok {
			continue
		}
		findings = append(findings, diffRules(rulePath, docPath, name, shippedByName[name], docGroup)...)
	}
	return findings
}

func diffRules(rulePath, docPath, groupName string, shipped, documented group) []string {
	var findings []string
	shippedRules := rulesByName(shipped)
	docRules := rulesByName(documented)

	for _, name := range sortedKeys(shippedRules) {
		doc, ok := docRules[name]
		if !ok {
			findings = append(findings, fmt.Sprintf("%s: %s/%s is shipped but not reproduced in %s",
				rulePath, groupName, name, docPath))
			continue
		}
		if !reflect.DeepEqual(shippedRules[name], doc) {
			findings = append(findings, fmt.Sprintf("%s: %s/%s has drifted from %s",
				rulePath, groupName, name, docPath))
		}
	}
	for _, name := range sortedKeys(docRules) {
		if _, ok := shippedRules[name]; !ok {
			findings = append(findings, fmt.Sprintf("%s: %s/%s is documented but not shipped in %s",
				docPath, groupName, name, rulePath))
		}
	}
	return findings
}

// runbookHeading matches the per-alert entries under the runbook's Alert Rule
// Reference. The anchor a runbook_url carries is the heading lowercased, which
// is how both GitHub and MkDocs slugify a single-word heading.
var runbookHeading = regexp.MustCompile(`(?m)^###\s+(ActionsGateway\w+)\s*$`)

// checkRunbook asserts every alert has a runbook entry and every runbook entry
// has an alert. The second direction is the one Q818 needed: a heading with no
// rule behind it sends on-call to a response procedure for an alert that can
// never fire.
func checkRunbook(rulePath, runbookPath string, groups []group, runbook []byte) []string {
	headings := map[string]bool{}
	for _, m := range runbookHeading.FindAllSubmatch(runbook, -1) {
		headings[string(m[1])] = true
	}

	var findings []string
	alerts := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.Rules {
			if r.Alert == "" {
				continue
			}
			alerts[r.Alert] = true
			url := r.Annotations["runbook_url"]
			switch {
			case url == "":
				findings = append(findings, fmt.Sprintf("%s: alert %s has no runbook_url annotation",
					rulePath, r.Alert))
			case !strings.HasSuffix(url, "#"+strings.ToLower(r.Alert)):
				findings = append(findings, fmt.Sprintf("%s: alert %s has runbook_url %q, which does not end in #%s",
					rulePath, r.Alert, url, strings.ToLower(r.Alert)))
			case !headings[r.Alert]:
				findings = append(findings, fmt.Sprintf("%s: alert %s has no `### %s` entry in %s",
					rulePath, r.Alert, r.Alert, runbookPath))
			}
		}
	}
	for _, name := range sortedKeys(headings) {
		if !alerts[name] {
			findings = append(findings, fmt.Sprintf("%s: `### %s` documents an alert that %s does not ship",
				runbookPath, name, rulePath))
		}
	}
	return findings
}

func ruleName(r rule) string {
	if r.Alert != "" {
		return r.Alert
	}
	return r.Record
}

func byName(groups []group) map[string]group {
	out := make(map[string]group, len(groups))
	for _, g := range groups {
		out[g.Name] = g
	}
	return out
}

func rulesByName(g group) map[string]rule {
	out := make(map[string]rule, len(g.Rules))
	for _, r := range g.Rules {
		out[ruleName(r)] = r
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
