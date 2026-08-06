package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// The release surface is derived from the pipeline that publishes it, not from
// a list of scopes or paths kept by hand. Three files describe what a release
// contains, and each is read for a different part of it:
//
//   - .github/workflows/publish.yml — which images are published (the job's
//     matrix), which charts are packaged (`helm package <dir>`), and which
//     scripts it runs to produce the remaining assets;
//   - Dockerfile — how each published image is built, followed from the image's
//     stage through its `COPY --from=` edges to the `go build` that produced
//     the binary;
//   - the resulting `go build` invocations — expanded by `go list -deps` into
//     the package directories that actually compile in.
//
// The one thing not derived is the gag-migrate CLI's package. Its build lives
// in scripts/release/build-migrate-binaries.sh behind a `cd` into the module,
// so reading it would mean tracking shell working-directory state to recover
// two strings. It is declared below and gated instead: CheckSources fails when
// publish.yml stops invoking that script, or invokes another script that builds
// something, so the declaration cannot outlive what it describes.

// declaredBuild is a release binary whose build command is not derivable from
// the Dockerfile. Each is pinned to the publish.yml step that produces it, so
// the staleness check can assert the step is still there.
type declaredBuild struct {
	ModuleDir string // directory to run `go list` from
	Package   string // package pattern, as passed to `go build`
	Script    string // the publish.yml-invoked script that builds it
	Why       string
}

var declaredBuilds = []declaredBuild{{
	ModuleDir: "cmd/gmc",
	Package:   "./migrate",
	Script:    "scripts/release/build-migrate-binaries.sh",
	Why:       "the gag-migrate CLI, attached to the GitHub Release as a signed asset (Q306)",
}}

// GoBuild is one `go build` invocation reachable from a published artifact.
type GoBuild struct {
	ModuleDir string
	Package   string
	Origin    string // what named it, for the staleness report
}

// Sources is what the publish pipeline says a release contains.
type Sources struct {
	Images    []string  // image names from publish.yml's matrix
	Charts    []string  // chart directories `helm package` packages
	Builds    []GoBuild // every go build reachable from a published artifact
	RunScript []string  // scripts publish.yml invokes
}

type workflowFile struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix struct {
				Name []string `yaml:"name"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

var (
	helmPackageRE = regexp.MustCompile(`helm package (\S+)`)
	scriptRE      = regexp.MustCompile(`(?m)(^|\s)(scripts/[\w./-]+\.sh)`)
)

// ParsePublishWorkflow reads the image matrix, the packaged charts, and the
// scripts the publish workflow invokes.
func ParsePublishWorkflow(path string) (images, charts, scripts []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seenImg, seenChart, seenScript := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, job := range wf.Jobs {
		for _, n := range job.Strategy.Matrix.Name {
			if !seenImg[n] {
				seenImg[n] = true
				images = append(images, n)
			}
		}
		for _, step := range job.Steps {
			for _, m := range helmPackageRE.FindAllStringSubmatch(step.Run, -1) {
				if c := m[1]; !seenChart[c] {
					seenChart[c] = true
					charts = append(charts, c)
				}
			}
			for _, m := range scriptRE.FindAllStringSubmatch(step.Run, -1) {
				if s := m[2]; !seenScript[s] {
					seenScript[s] = true
					scripts = append(scripts, s)
				}
			}
		}
	}
	sort.Strings(images)
	sort.Strings(charts)
	sort.Strings(scripts)
	return images, charts, scripts, nil
}

// dockerStage is one `FROM … AS name` block.
type dockerStage struct {
	name     string
	copyFrom []string
	goBuilds []GoBuild
}

var (
	fromRE     = regexp.MustCompile(`(?i)^FROM\s+\S+(?:\s+--\S+)*\s+AS\s+(\S+)`)
	copyFromRE = regexp.MustCompile(`(?i)^COPY\s+--from=(\S+)`)
	runRE      = regexp.MustCompile(`(?i)^RUN\s+(.*)$`)
)

// wordText renders one shell word to the text it stands for, leaving parameter
// expansions in their source form. Quoting is what matters here: it is the
// difference between `-ldflags="-buildid= -X main.version=${VERSION}"` being one
// argument and being three, and the second reading finds a package pattern
// where there is none.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	var parts func([]syntax.WordPart)
	parts = func(ps []syntax.WordPart) {
		for _, p := range ps {
			switch n := p.(type) {
			case *syntax.Lit:
				b.WriteString(n.Value)
			case *syntax.SglQuoted:
				b.WriteString(n.Value)
			case *syntax.DblQuoted:
				parts(n.Parts)
			case *syntax.ParamExp:
				b.WriteString("${" + n.Param.Value + "}")
			}
		}
	}
	parts(w.Parts)
	return b.String()
}

// goBuildsIn finds every `go build` invocation in a shell command, however it is
// nested — a subshell, an `&&` chain, a `cd … && go build` — and reads each
// one's module directory and package.
func goBuildsIn(src string) ([]GoBuild, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		// Not every RUN line is parseable bash (a JSON-form RUN, say). A line
		// that does not parse holds no go build this can read, which is not an
		// error: buildsFor reports an image that ends up with none.
		return nil, nil
	}
	var (
		builds []GoBuild
		bad    error
	)
	syntax.Walk(f, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if wordText(call.Args[0]) != "go" || wordText(call.Args[1]) != "build" {
			return true
		}
		args := make([]string, 0, len(call.Args)-2)
		for _, w := range call.Args[2:] {
			args = append(args, wordText(w))
		}
		b, err := parseGoBuildTokens(args)
		if err != nil {
			bad = err
			return false
		}
		builds = append(builds, b)
		return true
	})
	return builds, bad
}

// logicalLines joins Dockerfile continuations, so a `RUN` split across lines
// with trailing backslashes is read as the one command it is. Comment lines are
// dropped: a commented-out build is not a build.
func logicalLines(src string) []string {
	var out []string
	var acc strings.Builder
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if acc.Len() == 0 && strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			acc.WriteString(strings.TrimSuffix(line, `\`))
			acc.WriteString(" ")
			continue
		}
		if acc.Len() > 0 {
			acc.WriteString(line)
			out = append(out, strings.TrimSpace(acc.String()))
			acc.Reset()
			continue
		}
		out = append(out, line)
	}
	if acc.Len() > 0 {
		out = append(out, strings.TrimSpace(acc.String()))
	}
	return out
}

// ParseDockerfile reads the stage graph: each `FROM … AS` block, the stages it
// copies artifacts out of, and any `go build` it runs.
func ParseDockerfile(path string) (map[string]*dockerStage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stages := map[string]*dockerStage{}
	var cur *dockerStage
	for _, line := range logicalLines(string(raw)) {
		if m := fromRE.FindStringSubmatch(line); m != nil {
			cur = &dockerStage{name: m[1]}
			stages[m[1]] = cur
			continue
		}
		if cur == nil {
			continue
		}
		if m := copyFromRE.FindStringSubmatch(line); m != nil {
			cur.copyFrom = append(cur.copyFrom, m[1])
			continue
		}
		if m := runRE.FindStringSubmatch(line); m != nil {
			builds, err := goBuildsIn(m[1])
			if err != nil {
				return nil, fmt.Errorf("%s: stage %s: %w", path, cur.name, err)
			}
			for _, b := range builds {
				b.Origin = "Dockerfile stage " + cur.name
				cur.goBuilds = append(cur.goBuilds, b)
			}
		}
	}
	return stages, nil
}

// valueFlags are the `go build` flags that take their value as a separate
// argument. They matter because the package pattern is otherwise just "the
// token that is not a flag", and `-o /bin/proxy` ends an argument list with a
// path that looks exactly like one.
var valueFlags = map[string]bool{
	"-C": true, "-o": true, "-p": true, "-buildmode": true, "-buildvcs": true,
	"-compiler": true, "-gccgoflags": true, "-gcflags": true, "-asmflags": true,
	"-installsuffix": true, "-ldflags": true, "-mod": true, "-modfile": true,
	"-overlay": true, "-pgo": true, "-pkgdir": true, "-tags": true,
	"-toolexec": true, "-covermode": true, "-coverpkg": true,
}

// parseGoBuildTokens recovers the module directory and package pattern from a
// `go build` argument list that has already been split into shell words.
//
// The module directory comes from `-C`, which `go build` applies before
// anything else. Everything that is neither a flag nor a flag's value is a
// package pattern; anything other than exactly one is an error rather than a
// guess, since a misread pattern derives the wrong surface and every commit
// against the missing tree then reads as non-shipping.
func parseGoBuildTokens(fields []string) (GoBuild, error) {
	if len(fields) == 0 {
		return GoBuild{}, fmt.Errorf("empty go build argument list")
	}
	b := GoBuild{ModuleDir: "."}
	var pkgs []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			pkgs = append(pkgs, strings.Trim(f, `"'`))
			continue
		}
		name, inline, joined := strings.Cut(f, "=")
		if joined {
			if name == "-C" {
				b.ModuleDir = strings.Trim(inline, `"'`)
			}
			continue
		}
		if valueFlags[name] && i+1 < len(fields) {
			if name == "-C" {
				b.ModuleDir = strings.Trim(fields[i+1], `"'`)
			}
			i++ // the flag's value is not a package
		}
	}
	if len(pkgs) != 1 {
		return GoBuild{}, fmt.Errorf("read %d package pattern(s) from %q; expected exactly one",
			len(pkgs), strings.Join(fields, " "))
	}
	b.Package = pkgs[0]
	return b, nil
}

// buildsFor walks a published image's stage graph and returns every `go build`
// that contributed a binary to it.
func buildsFor(stages map[string]*dockerStage, image string) ([]GoBuild, error) {
	start, ok := stages[image]
	if !ok {
		return nil, fmt.Errorf("publish.yml publishes image %q but the Dockerfile has no %q stage", image, image)
	}
	var out []GoBuild
	seen := map[string]bool{}
	var walk func(s *dockerStage)
	walk = func(s *dockerStage) {
		if s == nil || seen[s.name] {
			return
		}
		seen[s.name] = true
		out = append(out, s.goBuilds...)
		for _, from := range s.copyFrom {
			walk(stages[from])
		}
	}
	walk(start)
	return out, nil
}

// DeriveSources reads the publish pipeline and returns what it publishes.
func DeriveSources(root string) (Sources, error) {
	var s Sources
	images, charts, scripts, err := ParsePublishWorkflow(filepath.Join(root, ".github", "workflows", "publish.yml"))
	if err != nil {
		return s, err
	}
	s.Images, s.Charts, s.RunScript = images, charts, scripts

	stages, err := ParseDockerfile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		return s, err
	}
	seen := map[string]bool{}
	for _, img := range images {
		builds, err := buildsFor(stages, img)
		if err != nil {
			return s, err
		}
		for _, b := range builds {
			key := b.ModuleDir + "|" + b.Package
			if !seen[key] {
				seen[key] = true
				s.Builds = append(s.Builds, b)
			}
		}
	}
	for _, d := range declaredBuilds {
		key := d.ModuleDir + "|" + d.Package
		if !seen[key] {
			seen[key] = true
			s.Builds = append(s.Builds, GoBuild{
				ModuleDir: d.ModuleDir, Package: d.Package,
				Origin: "declared (" + d.Script + ")",
			})
		}
	}
	return s, nil
}

// CheckSources is the staleness gate. The derivation reads publish.yml, so it
// cannot miss a new image or chart — but it can miss a release asset built by a
// script, because those are declared. This fails when publish.yml's script
// invocations and the declarations stop agreeing.
func CheckSources(root string, s Sources) []string {
	var problems []string
	invoked := map[string]bool{}
	for _, sc := range s.RunScript {
		invoked[sc] = true
	}
	for _, d := range declaredBuilds {
		if !invoked[d.Script] {
			problems = append(problems, fmt.Sprintf(
				"declared build %s %s (%s) cites %s, which publish.yml no longer invokes — re-derive it or drop the declaration",
				d.ModuleDir, d.Package, d.Why, d.Script))
		}
	}
	// A script publish.yml runs that itself builds a Go binary is a release
	// asset the surface may not cover. retry/fetch helpers wrap other commands
	// and build nothing, so only a `go build` in the script body is reported.
	for _, sc := range s.RunScript {
		if declaredFor(sc) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, sc))
		if err != nil {
			continue // not a gate's business: shellcheck and CI cover missing scripts
		}
		if strings.Contains(string(raw), "go build") {
			problems = append(problems, fmt.Sprintf(
				"publish.yml invokes %s, which runs `go build`, but no declared build covers it — a release binary may be missing from the surface",
				sc))
		}
	}
	return problems
}

func declaredFor(script string) bool {
	for _, d := range declaredBuilds {
		if d.Script == script {
			return true
		}
	}
	return false
}
