package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The AGC's permission set has two hand-maintained sources that must agree: the
// +kubebuilder:rbac markers in doc.go (what the code declares it needs) and the
// Helm chart's rules fragments (what an install actually grants). Neither
// generates the other — the chart roles deliberately differ from the generated
// agc-role (see the header comment on templates/agc-tenant-role.yaml) — so the
// only thing standing between them is a maintainer editing both.
//
// agc-tenant-role already has a read-back: the GMC integration suite installs it
// straight from the fragment (installAGCTenantClusterRole), so the RBAC-scope
// test exercises the shipped rules. agc-clusterrunnertemplate-reader had none
// (Q454), and it is the role where drift is most expensive: every rule in it is
// a cluster-wide grant bound by a ClusterRoleBinding, not scoped to a tenant
// namespace. These two tests are that gate.
const (
	agcTenantRoleRulesFile    = "agc-tenant-role-rules.yaml"
	agcClusterReaderRulesFile = "agc-clusterrunnertemplate-reader-rules.yaml"
	agcRBACMarkerFile         = "doc.go"
	clusterReaderClusterRole  = "agc-clusterrunnertemplate-reader"
	// testdata/chartfiles is an in-module symlink to charts/actions-gateway/files:
	// the chart tree sits outside the cmd/agc module root, and go drops such reads
	// from the test-cache key (testing.md § The out-of-module test read gate).
	chartRulesFragmentRelative = "testdata/chartfiles"
)

// readOnlyVerbs are the only verbs agc-clusterrunnertemplate-reader may grant.
// Every rule in it is cluster-wide, so a write verb there would hand the AGC
// authority over platform-owned objects in every namespace at once
// (docs/design/05-security.md § The AGC's cluster-scoped read surface).
var readOnlyVerbs = []string{"get", "list", "watch"}

// rbacMarkerRE matches a controller-gen RBAC marker comment and captures its
// comma-separated key=value payload.
var rbacMarkerRE = regexp.MustCompile(`^//\s*\+kubebuilder:rbac:(.+)$`)

// grantKey identifies one (apiGroup, resource) pair a PolicyRule or an RBAC
// marker names. The core group is the empty string, matching both the marker
// syntax (groups="") and rbacv1 (APIGroups: [""]).
type grantKey struct {
	group    string
	resource string
}

func (k grantKey) String() string {
	if k.group == "" {
		return k.resource + " (core)"
	}
	return k.resource + "." + k.group
}

// chartRulesPath resolves a rules fragment under charts/actions-gateway/files,
// reached through the in-module symlink named by chartRulesFragmentRelative.
func chartRulesPath(name string) string {
	return filepath.Join(filepath.FromSlash(chartRulesFragmentRelative), name)
}

// readChartRules parses a Helm chart rules fragment — a bare YAML list of
// PolicyRules that the chart embeds into a ClusterRole via .Files.Get.
func readChartRules(t *testing.T, name string) []rbacv1.PolicyRule {
	t.Helper()
	path := chartRulesPath(name)
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read chart rules fragment %s", path)

	var rules []rbacv1.PolicyRule
	require.NoErrorf(t, utilyaml.Unmarshal(data, &rules), "parse chart rules fragment %s", path)
	require.NotEmptyf(t, rules, "chart rules fragment %s parsed to zero rules", path)
	return rules
}

// grantKeys flattens PolicyRules into the (group, resource) pairs they grant,
// mapped to the sorted, de-duplicated verb set each pair carries.
func grantKeys(rules []rbacv1.PolicyRule) map[grantKey][]string {
	grants := make(map[grantKey][]string)
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := grantKey{group: group, resource: resource}
				grants[key] = sortedUnion(grants[key], rule.Verbs)
			}
		}
	}
	return grants
}

// sortedUnion merges verbs into a sorted, de-duplicated slice.
func sortedUnion(existing, add []string) []string {
	merged := append(slices.Clone(existing), add...)
	slices.Sort(merged)
	return slices.Compact(merged)
}

// parseRBACMarkers reads the +kubebuilder:rbac markers out of the AGC
// controller package's doc.go and returns the same (group, resource) → verbs
// shape as grantKeys, so the two sources compare directly.
//
// A marker carrying resourceNames grants strictly less than the pair it names,
// which this comparison cannot represent; rather than silently over-report such
// a marker as full coverage, the test fails and asks to be taught the narrower
// form. The AGC markers carry none today.
func parseRBACMarkers(t *testing.T, path string) map[grantKey][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read RBAC marker source %s", path)

	markers := make(map[grantKey][]string)
	for i, line := range strings.Split(string(data), "\n") {
		match := rbacMarkerRE.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		var groups, resources, verbs []string
		for _, field := range strings.Split(match[1], ",") {
			key, value, ok := strings.Cut(field, "=")
			require.Truef(t, ok, "%s:%d: RBAC marker field %q is not key=value", path, i+1, field)
			values := strings.Split(strings.Trim(value, `"`), ";")
			switch strings.TrimSpace(key) {
			case "groups":
				groups = values
			case "resources":
				resources = values
			case "verbs":
				verbs = values
			case "resourceNames":
				t.Fatalf("%s:%d: RBAC marker restricts resourceNames=%v; this drift test compares "+
					"whole (group, resource) grants and would over-report it as full coverage — "+
					"teach it the narrower form before adding such a marker", path, i+1, values)
			}
		}
		require.NotEmptyf(t, groups, "%s:%d: RBAC marker names no groups", path, i+1)
		require.NotEmptyf(t, resources, "%s:%d: RBAC marker names no resources", path, i+1)
		require.NotEmptyf(t, verbs, "%s:%d: RBAC marker names no verbs", path, i+1)

		for _, group := range groups {
			for _, resource := range resources {
				key := grantKey{group: group, resource: resource}
				markers[key] = sortedUnion(markers[key], verbs)
			}
		}
	}
	require.NotEmptyf(t, markers, "no +kubebuilder:rbac markers found in %s", path)
	return markers
}

// TestClusterReaderFragmentMatchesRBACMarkers is the Q454 drift gate on the
// cluster-scoped half of the AGC's permission set: the shipped
// agc-clusterrunnertemplate-reader rules must name exactly the verbs the doc.go
// markers declare for the same kinds, and nothing beyond a read.
//
// Editing the fragment without the marker (or widening a marker's verbs without
// the fragment) fails here rather than in a cluster.
func TestClusterReaderFragmentMatchesRBACMarkers(t *testing.T) {
	rules := readChartRules(t, agcClusterReaderRulesFile)
	markers := parseRBACMarkers(t, agcRBACMarkerFile)

	for key, verbs := range grantKeys(rules) {
		for _, verb := range verbs {
			require.Containsf(t, readOnlyVerbs, verb,
				"%s grants %q on %s: every rule in this ClusterRole is a cluster-wide grant, so it "+
					"must stay read-only (docs/design/05-security.md § The AGC's cluster-scoped read surface)",
				clusterReaderClusterRole, verb, key)
		}

		markerVerbs, ok := markers[key]
		require.Truef(t, ok,
			"%s grants %s but no +kubebuilder:rbac marker in %s declares it — the chart hands the AGC a "+
				"cluster-wide permission its code never asked for; add the marker or drop the rule",
			agcClusterReaderRulesFile, key, agcRBACMarkerFile)
		require.Equalf(t, markerVerbs, verbs,
			"verb drift on %s: %s grants %v, the %s marker declares %v — the two are hand-synced, "+
				"so edit both",
			key, agcClusterReaderRulesFile, verbs, agcRBACMarkerFile, markerVerbs)
	}
}

// TestRBACMarkersAreShippedByAChartRole closes the other drift direction: a
// marker added to doc.go without a matching chart rule compiles, generates, and
// then 403s at runtime in a real install. Every (group, resource) the AGC
// declares must be granted by one of the two ClusterRoles the chart ships.
//
// The comparison is per (group, resource), not per verb: agc-tenant-role
// deliberately withholds verbs the markers grant (runnergroups create/delete) for
// least privilege, so verb equality holds only for the cluster reader — which the
// test above asserts.
func TestRBACMarkersAreShippedByAChartRole(t *testing.T) {
	shipped := grantKeys(readChartRules(t, agcTenantRoleRulesFile))
	for key, verbs := range grantKeys(readChartRules(t, agcClusterReaderRulesFile)) {
		shipped[key] = sortedUnion(shipped[key], verbs)
	}

	for key := range parseRBACMarkers(t, agcRBACMarkerFile) {
		_, ok := shipped[key]
		require.Truef(t, ok,
			"%s declares a grant on %s that neither %s nor %s ships — a real install would 403 on it; "+
				"add the rule to the fragment for the role whose binding scope fits the kind "+
				"(namespaced → tenant role, cluster-scoped → %s)",
			agcRBACMarkerFile, key, agcTenantRoleRulesFile, agcClusterReaderRulesFile, clusterReaderClusterRole)
	}
}
