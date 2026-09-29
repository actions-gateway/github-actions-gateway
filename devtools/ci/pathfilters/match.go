package main

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// lanePatterns returns every pattern in every `filters:` block of a workflow,
// as one set. A lane whose gated jobs hang off several filters runs when any of
// them matches, so the union is the lane's scope.
//
// A pattern this package cannot match faithfully is an error rather than a
// skip: dropping one narrows the lane, and a narrower lane lets
// check-artifact-unchanged.sh report a change the lane never validated as
// outside its scope.
func lanePatterns(root *yaml.Node) ([]string, error) {
	var blocks []string
	filterBlocks(root, &blocks)
	var out []string
	for _, block := range blocks {
		var inner yaml.Node
		if err := yaml.Unmarshal([]byte(block), &inner); err != nil {
			return nil, fmt.Errorf("filters block is not valid YAML: %w", err)
		}
		m := contentRoot(&inner)
		if m == nil || m.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			name, v := m.Content[i].Value, m.Content[i+1]
			entries := []*yaml.Node{v}
			if v.Kind == yaml.SequenceNode {
				entries = v.Content
			}
			for _, e := range entries {
				if e.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("filter %q has a non-string entry (a change-type rule?), which match cannot evaluate", name)
				}
				if err := checkSupported(e.Value); err != nil {
					return nil, fmt.Errorf("filter %q: %w", name, err)
				}
				out = append(out, e.Value)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no filters: patterns, so the lane is not path-gated")
	}
	return out, nil
}

// checkSupported rejects the picomatch syntax matchPattern does not implement
// (negation, extglobs, brace expansion) and any segment path.Match cannot
// parse, which it would otherwise report as a silent non-match.
func checkSupported(pattern string) error {
	if strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "{}()") {
		return fmt.Errorf("pattern %q uses negation, an extglob, or braces, which match does not implement", pattern)
	}
	for _, s := range strings.Split(pattern, "/") {
		if _, err := path.Match(s, ""); err != nil {
			return fmt.Errorf("pattern %q: %w", pattern, err)
		}
	}
	return nil
}

// matchPattern reports whether a repo-relative file path matches a
// dorny/paths-filter pattern, for the subset of picomatch the workflows use:
// `*`, `?` and `[...]` within a segment, `**` as a whole segment for zero or
// more segments, and a pattern-initial `**` fused to more characters (`**.go`)
// as a globstar too. A `**` anywhere else degrades to `*`, as picomatch does
// (docs/development/testing.md § Where a globstar works in a filter glob).
//
// Wildcards here also match dot-prefixed names. That can only match more paths
// than picomatch would, which is the conservative direction for the caller.
func matchPattern(pattern, file string) bool {
	segs := strings.Split(pattern, "/")
	if s := segs[0]; strings.HasPrefix(s, "**") && s != "**" {
		segs = append([]string{"**", "*" + strings.TrimLeft(s, "*")}, segs[1:]...)
	}
	for i, s := range segs {
		if s != "**" {
			segs[i] = strings.ReplaceAll(s, "**", "*")
		}
	}
	return matchSegments(segs, strings.Split(file, "/"))
}

func matchSegments(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], parts[1:])
}

// writeMatches reads one path per line and prints those matched by any of the
// workflow's filter patterns, in input order.
func writeMatches(out *bufio.Writer, in io.Reader, root *yaml.Node) error {
	patterns, err := lanePatterns(root)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		file := sc.Text()
		if file == "" {
			continue
		}
		for _, p := range patterns {
			if matchPattern(p, file) {
				if _, err := fmt.Fprintln(out, file); err != nil {
					return err
				}
				break
			}
		}
	}
	return sc.Err()
}
