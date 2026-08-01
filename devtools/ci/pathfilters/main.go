// Command pathfilters extracts the two path lists that
// scripts/ci/check-path-filters.sh reconciles against go.work and the repo
// tree: the dorny/paths-filter `filters:` blocks, and a workflow's
// `on.push.paths` list.
//
// A `filters:` value is a YAML string whose contents are themselves YAML, so
// it is parsed twice. Reading it with a real parser rather than by indentation
// is the point: the awk it replaces accepted only `filters: |` written exactly
// that way, and a valid reformat (`|-`, flow style, an anchor) made the gate
// report a wall of coverage errors naming patterns that were already present.
//
// Usage:
//
//	pathfilters filters <workflow.yml>     # one "<filter>\t<pattern>" per line
//	pathfilters push-paths <workflow.yml>  # one path per line
//
// Output is in document order, which is what the caller's `sort` and its
// set comparisons expect.
package main

import (
	"bufio"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s filters|push-paths <workflow.yml>\n", os.Args[0])
		os.Exit(2)
	}
	mode, path := os.Args[1], os.Args[2]

	root, err := parseFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pathfilters: %v\n", err)
		os.Exit(1)
	}

	out := bufio.NewWriter(os.Stdout)
	switch mode {
	case "filters":
		err = writeFilters(out, root)
	case "push-paths":
		err = writePushPaths(out, root)
	default:
		fmt.Fprintf(os.Stderr, "pathfilters: unknown mode %q\n", mode)
		os.Exit(2)
	}
	if err == nil {
		err = out.Flush()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pathfilters: %s: %v\n", path, err)
		os.Exit(1)
	}
}

// parseFile reads a workflow and returns its root mapping node, or nil for an
// empty document.
func parseFile(path string) (*yaml.Node, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return contentRoot(&doc), nil
}

// contentRoot unwraps a document node to the value it holds.
func contentRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

// mapValue returns the value node for key in a mapping, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// scalars flattens a node to its string values: a sequence yields each entry, a
// lone scalar yields itself. dorny/paths-filter accepts both spellings for a
// filter's pattern list.
func scalars(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			}
		}
		return out
	default:
		return nil
	}
}

// filterBlocks collects the scalar value of every `filters` key in the tree, in
// document order. The key sits under a step's `with:`, at a nesting depth that
// varies by workflow, so the whole document is walked rather than a fixed path.
func filterBlocks(n *yaml.Node, acc *[]string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "filters" && v.Kind == yaml.ScalarNode {
				*acc = append(*acc, v.Value)
			}
			filterBlocks(v, acc)
		}
		return
	}
	for _, c := range n.Content {
		filterBlocks(c, acc)
	}
}

// writeFilters prints one "<filter>\t<pattern>" row per pattern.
func writeFilters(out *bufio.Writer, root *yaml.Node) error {
	var blocks []string
	filterBlocks(root, &blocks)
	for _, block := range blocks {
		var inner yaml.Node
		if err := yaml.Unmarshal([]byte(block), &inner); err != nil {
			return fmt.Errorf("filters block is not valid YAML: %w", err)
		}
		m := contentRoot(&inner)
		if m == nil || m.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			name := m.Content[i].Value
			for _, pattern := range scalars(m.Content[i+1]) {
				if _, err := fmt.Fprintf(out, "%s\t%s\n", name, pattern); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writePushPaths prints the `on.push.paths` entries, one per line.
//
// The `on` key stays a string: gopkg.in/yaml.v3 resolves only true/True/TRUE
// as booleans, not the YAML 1.1 `on`/`yes` spellings that would turn this
// lookup into a miss.
func writePushPaths(out *bufio.Writer, root *yaml.Node) error {
	for _, p := range scalars(mapValue(mapValue(mapValue(root, "on"), "push"), "paths")) {
		if _, err := fmt.Fprintln(out, p); err != nil {
			return err
		}
	}
	return nil
}
