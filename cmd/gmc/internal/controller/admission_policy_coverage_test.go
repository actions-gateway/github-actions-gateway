package controller

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	admissionPolicyDir = "../../config/admission-policy"
	// The chart sits outside this module, so it is read through a testdata symlink
	// that keeps it a test-cache input.
	chartAdmissionPolicyDir = "testdata/chart-templates"
)

// ungatedManagerWrites are the manager ClusterRole's write grants that no shipped
// admission policy matches, each with the reason it is left cluster-wide.
// docs/design/05-security.md lists the same set as what a compromised GMC can
// still write outside a tenant namespace.
var ungatedManagerWrites = map[string]string{
	"/events":              "an Event records an occurrence and configures nothing",
	"events.k8s.io/events": "an Event records an occurrence and configures nothing",

	// The GMC's own API kinds. Nothing is provisioned from one outside a tenant
	// namespace, because every kind it would produce is gated; and gating the
	// finalizer update would wedge the deletion of one created there.
	"actions-gateway.com/actionsgateways":            "provisions nothing outside a tenant namespace",
	"actions-gateway.com/actionsgateways/finalizers": "provisions nothing outside a tenant namespace",
	"actions-gateway.com/actionsgateways/status":     "provisions nothing outside a tenant namespace",
	"actions-gateway.com/egressproxies":              "provisions nothing outside a tenant namespace",
	"actions-gateway.com/egressproxies/status":       "provisions nothing outside a tenant namespace",
}

// writeVerbOperations maps an RBAC write verb to the admission operation that
// carries it.
var writeVerbOperations = map[string]string{
	"create":           "CREATE",
	"update":           "UPDATE",
	"patch":            "UPDATE",
	"delete":           "DELETE",
	"deletecollection": "DELETE",
}

// decodeYAMLDocs decodes every non-empty document in a multi-document YAML stream.
func decodeYAMLDocs(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		require.NoError(t, err)
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
}

func stringsOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// policyResourceRule is one matchConstraints.resourceRules entry.
type policyResourceRule struct {
	groups, resources, operations []string
}

func (r policyResourceRule) matches(group, resource, operation string) bool {
	has := func(set []string, want string) bool {
		return slices.Contains(set, want) || slices.Contains(set, "*")
	}
	return has(r.groups, group) && has(r.resources, resource) && has(r.operations, operation)
}

// gmcIdentityCondition is how a policy's matchConditions pin the GMC
// ServiceAccount, in the config copies.
const gmcIdentityCondition = "request.userInfo.username == 'system:serviceaccount:gmc-system:gmc-controller-manager'"

// confinesGMC reports whether a policy spec is scoped to the GMC ServiceAccount.
// A policy that names a kind for every principal (the PriorityClass guard) checks
// something else about the write and does not confine where the GMC makes it.
func confinesGMC(spec map[string]any) bool {
	conditions, _ := spec["matchConditions"].([]any)
	for _, entry := range conditions {
		condition, _ := entry.(map[string]any)
		if expression, _ := condition["expression"].(string); expression == gmcIdentityCondition {
			return true
		}
	}
	return false
}

// gmcPolicyResourceRules reads the resourceRules of every ValidatingAdmissionPolicy
// under config/admission-policy that confines the GMC ServiceAccount.
func gmcPolicyResourceRules(t *testing.T) []policyResourceRule {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(admissionPolicyDir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no admission policies under %s", admissionPolicyDir)

	var rules []policyResourceRule
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, doc := range decodeYAMLDocs(t, data) {
			if doc["kind"] != "ValidatingAdmissionPolicy" {
				continue
			}
			spec, _ := doc["spec"].(map[string]any)
			if !confinesGMC(spec) {
				continue
			}
			constraints, _ := spec["matchConstraints"].(map[string]any)
			entries, _ := constraints["resourceRules"].([]any)
			for _, entry := range entries {
				rule, _ := entry.(map[string]any)
				rules = append(rules, policyResourceRule{
					groups:     stringsOf(rule["apiGroups"]),
					resources:  stringsOf(rule["resources"]),
					operations: stringsOf(rule["operations"]),
				})
			}
		}
	}
	require.NotEmpty(t, rules, "no resourceRules read from %s", admissionPolicyDir)
	return rules
}

// Every kind the manager ClusterRole can write cluster-wide is either matched by a
// shipped admission policy scoped to the GMC, for each operation the grant allows, or listed in
// ungatedManagerWrites. A new write marker therefore fails here until the guard
// gains its kind or the exemption is argued (Q1321).
func TestManagerRole_WriteGrantsAreGatedOrExempt(t *testing.T) {
	rules := gmcPolicyResourceRules(t)

	ungated := map[string]bool{}
	for _, grant := range loadClusterRole(t).Rules {
		for _, group := range grant.APIGroups {
			for _, resource := range grant.Resources {
				for _, verb := range grant.Verbs {
					operation, isWrite := writeVerbOperations[verb]
					if !isWrite {
						continue
					}
					matched := slices.ContainsFunc(rules, func(r policyResourceRule) bool {
						return r.matches(group, resource, operation)
					})
					if !matched {
						ungated[group+"/"+resource] = true
					}
				}
			}
		}
	}

	var unexplained []string
	for kind := range ungated {
		if _, exempt := ungatedManagerWrites[kind]; !exempt {
			unexplained = append(unexplained, kind)
		}
	}
	slices.Sort(unexplained)
	require.Empty(t, unexplained,
		"the manager ClusterRole grants writes that no GMC-scoped policy under %s matches: add each kind to "+
			"tenant-resource-guard (chart template and config copy), or to ungatedManagerWrites and "+
			"docs/design/05-security.md with the reason it is safe cluster-wide", admissionPolicyDir)
	for kind := range ungatedManagerWrites {
		require.True(t, ungated[kind],
			"ungatedManagerWrites lists %q, which is no longer an ungated write grant; remove the entry", kind)
	}
}

var (
	templateActionRE = regexp.MustCompile(`\{\{.*?\}\}`)
	templateOnlyRE   = regexp.MustCompile(`^\s*\{\{-.*\}\}\s*$`)
)

// templatedValue stands in for a value the chart renders per install.
const templatedValue = "TEMPLATED"

// stripTemplateActions makes a chart template parseable as YAML: a line holding
// only a template action is dropped, and an inline action becomes templatedValue.
func stripTemplateActions(data []byte) []byte {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if templateOnlyRE.MatchString(line) {
			continue
		}
		out = append(out, templateActionRE.ReplaceAllString(line, templatedValue))
	}
	return []byte(strings.Join(out, "\n"))
}

// equalExceptTemplated reports whether the config copy's value equals the chart's
// wherever the chart's is not rendered per install.
func equalExceptTemplated(chart, config any) bool {
	switch c := chart.(type) {
	case string:
		s, ok := config.(string)
		return ok && (strings.Contains(c, templatedValue) || c == s)
	case map[string]any:
		m, ok := config.(map[string]any)
		if !ok || len(m) != len(c) {
			return false
		}
		for key, value := range c {
			other, present := m[key]
			if !present || !equalExceptTemplated(value, other) {
				return false
			}
		}
		return true
	case []any:
		l, ok := config.([]any)
		if !ok || len(l) != len(c) {
			return false
		}
		for i := range c {
			if !equalExceptTemplated(c[i], l[i]) {
				return false
			}
		}
		return true
	default:
		return chart == config
	}
}

// The policies under config/admission-policy are what the envtest suite applies,
// and the chart templates are what an install gets. They are hand-synced, so a
// rule added to one side only is tested and not shipped, or shipped and not
// tested (Q1321: the config copy lacked the chart's VerticalPodAutoscaler rule).
func TestAdmissionPolicies_ConfigCopiesMatchChartTemplates(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(admissionPolicyDir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	specsByKind := func(data []byte) map[string]any {
		specs := map[string]any{}
		for _, doc := range decodeYAMLDocs(t, data) {
			kind, _ := doc["kind"].(string)
			if strings.HasPrefix(kind, "ValidatingAdmissionPolicy") {
				specs[kind] = doc["spec"]
			}
		}
		return specs
	}

	for _, file := range files {
		name := filepath.Base(file)
		t.Run(name, func(t *testing.T) {
			configData, err := os.ReadFile(file)
			require.NoError(t, err)
			chartData, err := os.ReadFile(filepath.Join(chartAdmissionPolicyDir, name))
			require.NoError(t, err, "every config copy has a chart template of the same name")

			config := specsByKind(configData)
			chart := specsByKind(stripTemplateActions(chartData))
			require.Len(t, chart, 2, "the chart template holds a policy and its binding")
			for kind, chartSpec := range chart {
				require.True(t, equalExceptTemplated(chartSpec, config[kind]),
					"%s spec differs between %s and its chart template, outside the values the chart renders per install",
					kind, file)
			}
		})
	}
}
