// Command usespin fails a build when a GitHub Actions `uses:` reference is not
// immutably pinned.
//
// actionlint — v1.7.12, the version tools/ pins — validates that a `uses:` ref
// is present and well formed, not that it is a commit SHA. Measured against that
// build: `actions/checkout@v7.0.1`, `@v4` and `@main` all exit 0, and it resolves
// the action's `with:` inputs against the tag's metadata while doing so, so it
// reads the ref and simply never asserts 40-hex (Q579 measured the same gap;
// Q644 is this gate). A tag is mutable by whoever owns the action, and a step
// runs with the job's token, so a tag-pinned `uses:` is a supply-chain hole no
// other gate in this repo closes — Dependabot bumps pins, it cannot stop one
// being written.
//
// Rules by reference shape. Every shape GitHub accepts is either matched or
// rejected; an unrecognized one is a finding, never a skip, because a gate that
// passes what it cannot classify protects nothing:
//
//	owner/repo@<sha>                          remote action
//	owner/repo/path@<sha>                     remote action in a subdirectory
//	owner/repo/.github/workflows/x.yml@<sha>   remote reusable workflow
//	  — all three: 40-hex SHA plus a version comment
//	./path                                    local action or reusable workflow;
//	  exempt from pinning, it is in-tree code the same PR reviews
//	docker://image@sha256:<64-hex>            Docker image; digest required, for
//	  the reason a tag will not do above
//
// The version comment (`@<sha> # v7.0.1`) is required rather than cosmetic: it
// is what Dependabot reads to learn which release a SHA is, so a pin without one
// is a pin nothing will ever bump. Every reference in the tree already carries
// one, so this costs no migration.
//
// Only the three places GitHub defines `uses:` are walked — jobs.<id>.uses,
// jobs.<id>.steps[].uses, and an action's runs.steps[].uses. That is what makes
// the gate immune to the word appearing in a comment or inside a `run:` block,
// both of which occur in this repo's workflows and would defeat a regex.
//
// Usage:
//
//	usespin [-min N] <workflow.yml>...
//
// Prints one finding per violation, then the number of references checked. Exits
// 1 on a finding, 2 when a file cannot be read or parsed or when fewer than -min
// references were found (default 1) — an extractor that silently stops matching
// looks identical to a clean tree, so the floor is what tells them apart.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	shaRef    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRef = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// A version comment must carry at least major.minor; `# v4` is too coarse to
	// identify a release, and `# checkout` is not a version at all.
	versionComment = regexp.MustCompile(`(^|[\s=])v?[0-9]+\.[0-9]+`)
)

type finding struct {
	file string
	line int
	uses string
	msg  string
}

func main() {
	min := flag.Int("min", 1, "fail when fewer than this many references were checked")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s [-min N] <workflow.yml>...\n", os.Args[0])
		os.Exit(2)
	}

	var all []finding
	checked := 0
	for _, path := range flag.Args() {
		n, found, err := checkFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "usespin: %v\n", err)
			os.Exit(2)
		}
		checked += n
		all = append(all, found...)
	}

	for _, f := range all {
		fmt.Printf("%s:%d: uses: %s\n    %s\n", f.file, f.line, f.uses, f.msg)
	}

	if checked < *min {
		fmt.Fprintf(os.Stderr, "usespin: checked %d references across %d file(s), want at least %d.\n"+
			"Either the workflows no longer declare `uses:` or the extractor stopped matching them.\n",
			checked, flag.NArg(), *min)
		os.Exit(2)
	}
	if len(all) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d unpinned reference(s). Pin each to the full 40-hex commit SHA of the\n"+
			"release and keep the version in a trailing comment, which is what Dependabot bumps:\n"+
			"    uses: owner/repo@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n", len(all))
		os.Exit(1)
	}
	fmt.Printf("all %d `uses:` reference(s) are SHA-pinned with a version comment\n", checked)
}

// checkFile parses one workflow or action file and returns the number of `uses:`
// references it declares along with any that are not immutably pinned. A file
// that cannot be read or parsed is an error, not an empty result: skipping it
// would let an unparseable workflow through the gate.
func checkFile(path string) (int, []finding, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return 0, nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return 0, nil, nil
	}
	lines := strings.Split(string(b), "\n")

	checked := 0
	var found []finding
	visit := func(step *yaml.Node) {
		v := mapValue(step, "uses")
		if v == nil || v.Kind != yaml.ScalarNode {
			return
		}
		checked++
		msg, wantVersion := classify(v.Value)
		if msg == "" && wantVersion && !versionComment.MatchString(trailingComment(lines, v)) {
			msg = "SHA pin carries no version comment, so nothing records which release it is " +
				"and Dependabot will not bump it; append ` # v<major.minor.patch>`"
		}
		if msg != "" {
			found = append(found, finding{file: path, line: v.Line, uses: v.Value, msg: msg})
		}
	}

	root := doc.Content[0]
	if jobs := mapValue(root, "jobs"); jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 1; i < len(jobs.Content); i += 2 {
			job := jobs.Content[i]
			visit(job)
			if steps := mapValue(job, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
				for _, s := range steps.Content {
					visit(s)
				}
			}
		}
	}
	// An action.yml composite: runs.steps[].uses.
	if runs := mapValue(root, "runs"); runs != nil {
		if steps := mapValue(runs, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
			for _, s := range steps.Content {
				visit(s)
			}
		}
	}
	return checked, found, nil
}

// classify reports why a reference is not immutably pinned, empty when it is,
// and whether the shape additionally requires a trailing version comment.
func classify(uses string) (string, bool) {
	switch {
	case strings.HasPrefix(uses, "./"):
		if strings.Contains(uses, "@") {
			return "a local reference is resolved from this commit and must not carry a ref", false
		}
		return "", false
	case strings.HasPrefix(uses, "docker://"):
		image := strings.TrimPrefix(uses, "docker://")
		at := strings.LastIndex(image, "@")
		if at < 0 || !digestRef.MatchString(image[at+1:]) {
			return "Docker image is not digest-pinned; a tag is mutable, so use docker://image@sha256:<64-hex>", false
		}
		return "", false
	default:
		at := strings.LastIndex(uses, "@")
		if at < 0 {
			return "no ref; expected owner/repo@<40-hex commit SHA>", false
		}
		if ref := uses[at+1:]; !shaRef.MatchString(ref) {
			return fmt.Sprintf("ref %q is a tag or branch, which whoever owns the action can move; "+
				"expected a 40-hex commit SHA", ref), false
		}
		return "", true
	}
}

// mapValue returns the value node for key in a mapping, or nil. Aliases are
// followed: an unresolved one is a mapping this walk would skip silently, and
// skipping is the one outcome a pin gate must never have.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
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

// trailingComment returns the `#` comment following a scalar on its own source
// line. Read from the source rather than from yaml.Node.LineComment because
// which node the parser hangs a comment off varies with the surrounding shape,
// and a comment lookup that quietly returns "" would make the version-comment
// rule fail open. A `uses:` value never contains `#`, so scanning from the end
// of the scalar cannot pick one out of the value itself.
func trailingComment(lines []string, n *yaml.Node) string {
	if n.Line < 1 || n.Line > len(lines) {
		return ""
	}
	line := lines[n.Line-1]
	start := n.Column - 1 + len(n.Value)
	if start < 0 || start > len(line) {
		return ""
	}
	i := strings.Index(line[start:], "#")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[start+i+1:])
}
