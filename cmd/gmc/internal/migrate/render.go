package migrate

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// RenderManifests serializes a fan-out Result to a multi-document YAML manifest
// stream — the dry-run output an operator reviews before `--apply`. Objects are
// emitted in dependency order (EgressProxy and RunnerTemplates before the
// RunnerSets that reference them, ActionsGateway first) so a `kubectl apply -f -`
// of the stream applies cleanly; reference integrity is a runtime condition, not an
// apply-order gate (§H.7), so the order is for readability, not correctness.
//
// The namespace metadata patch is rendered as a trailing comment block with the
// equivalent `kubectl label`/`kubectl annotate` commands rather than as a bare
// Namespace object: applying a partial Namespace manifest risks pruning labels the
// operator did not intend to touch, whereas the additive commands only add the v2
// keys (the same additive semantics `--apply` uses). No Secret content is ever
// emitted — only the githubAppRef name carried on the ActionsGateway.
func RenderManifests(res *Result) (string, error) {
	var docs []string

	add := func(label string, obj any) error {
		b, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", label, err)
		}
		docs = append(docs, strings.TrimRight(string(b), "\n"))
		return nil
	}

	if res.Gateway != nil {
		if err := add("ActionsGateway", res.Gateway); err != nil {
			return "", err
		}
	}
	if res.Proxy != nil {
		if err := add("EgressProxy", res.Proxy); err != nil {
			return "", err
		}
	}
	for _, t := range res.Templates {
		if err := add("RunnerTemplate "+t.Name, t); err != nil {
			return "", err
		}
	}
	for _, s := range res.Sets {
		if err := add("RunnerSet "+s.Name, s); err != nil {
			return "", err
		}
	}

	out := strings.Join(docs, "\n---\n")
	if patch := renderNamespacePatchComment(res.NamespacePatch); patch != "" {
		out += "\n" + patch
	}
	if len(res.Warnings) > 0 {
		out += "\n" + renderWarningsComment(res.Warnings)
	}
	return out + "\n", nil
}

// renderNamespacePatchComment renders the namespace patch as a commented block of
// equivalent kubectl commands. Returns "" when there is nothing to patch.
func renderNamespacePatchComment(p *NamespacePatch) string {
	if p == nil || (len(p.Labels) == 0 && len(p.Annotations) == 0) {
		return ""
	}
	var b strings.Builder
	b.WriteString("# ----------------------------------------------------------------------\n")
	b.WriteString("# Namespace metadata patch (applied to the existing namespace, additively).\n")
	b.WriteString(fmt.Sprintf("# Equivalent kubectl commands for namespace %q:\n", p.Name))
	for _, k := range sortedKeys(p.Labels) {
		b.WriteString(fmt.Sprintf("#   kubectl label   namespace %s %s=%s --overwrite\n", p.Name, k, p.Labels[k]))
	}
	for _, k := range sortedKeys(p.Annotations) {
		b.WriteString(fmt.Sprintf("#   kubectl annotate namespace %s %s=%s --overwrite\n", p.Name, k, p.Annotations[k]))
	}
	b.WriteString("# ----------------------------------------------------------------------")
	return b.String()
}

// renderWarningsComment renders operator warnings as a commented block.
func renderWarningsComment(warnings []string) string {
	var b strings.Builder
	b.WriteString("# Warnings:\n")
	for _, w := range warnings {
		b.WriteString("#   - " + w + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// sortedKeys returns the map keys in deterministic order for stable output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
