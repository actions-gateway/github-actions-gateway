// Command promqlcheck validates the shipped monitoring artifacts (Q827, Q818).
//
// deploy/monitoring/prometheusrule.yaml is an appliable artifact — its README
// tells an operator to kubectl apply it — but nothing parsed its PromQL, so a
// malformed expression merged and then silently never fired. That is the same
// failure mode the alerts themselves exist to catch, arriving through the alerts.
//
// Three checks, because a rule file can be wrong in three ways that all read as
// healthy:
//
//	expr      every rule's expression parses as PromQL
//	mirror    the alerting doc reproduces the shipped rules exactly
//	runbook   every alert's runbook_url resolves to a runbook heading
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

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: promqlcheck <prometheusrule.yaml> <observability-alerting.md> <runbook.md>")
		os.Exit(2)
	}
	findings, err := run(os.Args[1], os.Args[2], os.Args[3])
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
	fmt.Printf("promqlcheck: ok (%s)\n", os.Args[1])
}

func run(rulePath, docPath, runbookPath string) ([]string, error) {
	ruleRaw, err := os.ReadFile(rulePath)
	if err != nil {
		return nil, err
	}
	var shipped prometheusRule
	if err := yaml.Unmarshal(ruleRaw, &shipped); err != nil {
		return nil, fmt.Errorf("%s: %w", rulePath, err)
	}
	if len(shipped.Spec.Groups) == 0 {
		return nil, fmt.Errorf("%s: no groups under .spec", rulePath)
	}

	docRaw, err := os.ReadFile(docPath)
	if err != nil {
		return nil, err
	}
	runbookRaw, err := os.ReadFile(runbookPath)
	if err != nil {
		return nil, err
	}

	var findings []string
	findings = append(findings, checkExprs(rulePath, shipped.Spec.Groups)...)

	documented, err := docBlocks(docRaw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", docPath, err)
	}
	findings = append(findings, checkMirror(rulePath, docPath, shipped.Spec.Groups, documented)...)
	findings = append(findings, checkRunbook(rulePath, runbookPath, shipped.Spec.Groups, runbookRaw)...)
	return findings, nil
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
