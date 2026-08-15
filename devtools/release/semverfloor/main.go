// Command semverfloor reports the minimum semver bump the merged work already
// requires — the floor a release cannot be cut below.
//
// The question it answers is "what has landed", not "what should we call it".
// Counting `feat` subjects answers neither: of the 17 in the v1.3.0..v1.4.0
// window, 11 are dev tooling, CI, and docs that ship in no image and no chart.
// So a commit's weight is read from the paths it touched, checked against the
// surface the publish pipeline actually packages (see sources.go), and its
// Conventional Commit type only says how much that weighs.
//
// The path is where attribution starts. A commit that edits a godoc line inside
// a released package directory touches the surface and ships a byte-identical
// binary, so the shipped files are read again as source and kept only when the
// change reaches the compiler — see narrow.go, which errs toward keeping them.
//
// It is a report, not a gate. The floor is monotonic and saturates early —
// measured across the four release windows to date, it reached `minor` at
// commit 15 of 341, 6 of 95, 40 of 463, and 16 of 121 — so a gate that fired on
// the transition would speak on 4 pull requests out of 1,020 and be silent for
// the rest of every cycle. What that gate would catch is not an accident worth
// catching: the first shipping feature after a tag is the expected event.
//
// The one thing here that can silently rot is gated instead — see CheckSources.
//
// # Two readings of one classification
//
// -notes emits the same classification as the release notes need it: the
// shipping commits to curate, and the residue that reaches no released
// artifact. It replaces a hand-written scope allow-list in release.md that
// admitted commits by their scope string, which fails in both directions —
// across v1.2.0..v1.3.0 it dropped seven shipping fixes whose scopes nobody had
// listed, and at v1.4.0 it dropped the scopeless `feat:` carrying Q166 while
// keeping a `feat(metrics)` that touched only the usage tooling.
//
// Paths close that whole class. What they cannot close is an artifact a release
// publishes outside an image or chart, which is why the residue is printed
// rather than summarised: v1.4.0's runner template library (Q554) is a headline
// feature living in deploy/templates/, and no surface derived from publish.yml
// will ever see it. See notes.go for the signal that raises it to the top.
//
// Usage:
//
//	semverfloor [-from REF] [-to REF]
//	semverfloor [-from REF] [-to REF] -notes
//	semverfloor -check-sources
//	git diff --name-only A..B | semverfloor -ships
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	var (
		from         = flag.String("from", "", "start of the window (default: the newest stable v* tag)")
		to           = flag.String("to", "", "end of the window (default: origin/main, else HEAD)")
		checkSources = flag.Bool("check-sources", false, "assert the release surface derivation still matches publish.yml; exit 1 if not")
		notes        = flag.Bool("notes", false, "enumerate the window for the release notes, with the residue path-classified")
		ships        = flag.Bool("ships", false, "read paths on stdin, print those on the released surface; ignores commit type entirely")
	)
	flag.Parse()

	if *ships {
		if err := runShips(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "semverfloor:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*from, *to, *checkSources, *notes); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "semverfloor:", err)
		os.Exit(1)
	}
}

// runShips filters stdin paths through the derived release surface and prints the
// ones a released artifact is built from.
//
// It exists because the floor answers a different question. The floor reads a
// commit's *type* first, so a `build:` or `docs:` commit that edits a shipped file
// is withheld from it by design — correct for choosing a version bump, wrong for
// asking whether an artifact moved. A chart README is the case that separates them:
// it ships inside the chart tarball and carries no semver weight.
//
// Callers get the derivation rather than a path list of their own: the surface
// follows publish.yml's image matrix through each Dockerfile's COPY --from= edges
// to the go build behind it, so adding an image or a chart needs nothing updated
// here. A hand-kept list in a caller would silently stop covering a new artifact.
func runShips() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	sources, err := DeriveSources(root)
	if err != nil {
		return err
	}
	surface, err := deriveSurface(root, sources)
	if err != nil {
		return err
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if p == "" {
			continue
		}
		if surface.Ships(p) {
			fmt.Println(p)
		}
	}
	return sc.Err()
}

func run(from, to string, checkSources, notes bool) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	sources, err := DeriveSources(root)
	if err != nil {
		return err
	}

	if checkSources {
		problems := CheckSources(root, sources)
		for _, p := range problems {
			_, _ = fmt.Fprintln(os.Stderr, "semverfloor:", p)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d release-surface derivation problem(s)", len(problems))
		}
		fmt.Printf("release surface derives cleanly: %d image(s), %d chart(s), %d go build(s)\n",
			len(sources.Images), len(sources.Charts), len(sources.Builds))
		return nil
	}

	if from == "" {
		if from, err = newestStableTag(); err != nil {
			return err
		}
	}
	if to == "" {
		to = "HEAD"
		if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "origin/main").Run(); err == nil {
			to = "origin/main"
		}
	}

	surface, err := deriveSurface(root, sources)
	if err != nil {
		return err
	}
	commits, err := readCommits(root, from+".."+to)
	if err != nil {
		return err
	}
	r := Classify(commits, surface, gitNarrower{root: root})
	if notes {
		n := EnumerateNotes(r, gitPager{root: root})
		reportNotes(os.Stdout, from, to, commits, surface, sources, n, checkOperatorDocs(root))
		return nil
	}
	report(os.Stdout, from, to, commits, surface, sources, r)
	return nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// newestStableTag picks the highest v* tag with no prerelease suffix: an RC is a
// step inside a release, not the last one.
func newestStableTag() (string, error) {
	out, err := exec.Command("git", "tag", "--list", "v*", "--sort=-v:refname").Output()
	if err != nil {
		return "", err
	}
	for _, t := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if t != "" && !strings.Contains(t, "-") {
			return t, nil
		}
	}
	return "", fmt.Errorf("no stable v* tag found; pass -from explicitly")
}

// readCommits reads a window's commits with their subjects, bodies, and changed
// paths. The record separator is NUL and the field separator is US, so a body
// holding blank lines or a subject holding a colon cannot shift a field.
func readCommits(root, window string) ([]Commit, error) {
	//nolint:gosec // G204: fixed git verbs plus a revision range this program resolved.
	cmd := exec.Command("git", "log", "--no-merges",
		"--format=%x00%H%x1f%s%x1f%B%x1f", "--name-only", window)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", window, err)
	}
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x00") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		c := Commit{SHA: f[0], Subject: f[1]}
		c.Type, c.Scopes, c.Breaking, _ = parseSubject(c.Subject)
		if hasBreakingFooter(f[2]) {
			c.Breaking = true
		}
		for _, p := range strings.Split(f[3], "\n") {
			if p = strings.TrimSpace(p); p != "" {
				c.Files = append(c.Files, p)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// deriveSurface expands every reachable `go build` into the package directories
// it compiles, and adds the chart trees the pipeline packages wholesale.
func deriveSurface(root string, s Sources) (Surface, error) {
	surface := Surface{PkgDirs: map[string]bool{}, Trees: append([]string(nil), s.Charts...)}
	sort.Strings(surface.Trees)
	for _, b := range s.Builds {
		//nolint:gosec // G204: fixed go verbs plus a package pattern read from the repo's own Dockerfile.
		cmd := exec.Command("go", "list", "-deps", "-f", "{{.Dir}}", b.Package)
		cmd.Dir = filepath.Join(root, b.ModuleDir)
		out, err := cmd.Output()
		if err != nil {
			return surface, fmt.Errorf("go list -deps %s (in %s, from %s): %w", b.Package, b.ModuleDir, b.Origin, err)
		}
		for _, dir := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			rel, err := filepath.Rel(root, dir)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue // a module cache or toolchain package: not this repo's source
			}
			if rel == "vendor" || strings.HasPrefix(rel, "vendor/") || strings.Contains(rel, "/vendor/") {
				continue
			}
			surface.PkgDirs[rel] = true
		}
	}
	if len(surface.PkgDirs) == 0 {
		return surface, fmt.Errorf("derived an empty package surface; every commit would read as non-shipping")
	}
	return surface, nil
}

func report(w *os.File, from, to string, commits []Commit, surface Surface, s Sources, r Result) {
	_, _ = fmt.Fprintf(w, "Semver floor %s..%s\n", from, to)
	_, _ = fmt.Fprintf(w, "%d commits (merges excluded)\n\n", len(commits))

	_, _ = fmt.Fprintf(w, "Released surface: %d package dir(s) from %d go build(s) across %d image(s), plus %d chart tree(s)\n",
		len(surface.PkgDirs), len(s.Builds), len(s.Images), len(s.Charts))
	_, _ = fmt.Fprintf(w, "  images: %s\n", strings.Join(s.Images, " "))
	_, _ = fmt.Fprintf(w, "  charts: %s\n\n", strings.Join(s.Charts, " "))

	_, _ = fmt.Fprintf(w, "FLOOR: %s\n", strings.ToUpper(r.Floor.String()))
	if len(r.Unresolved) > 0 {
		_, _ = fmt.Fprintf(w, "MAJOR: unresolved — %d breaking marker(s) on shipped code (below)\n", len(r.Unresolved))
	}
	_, _ = fmt.Fprintln(w)

	if len(r.Raising) == 0 {
		_, _ = fmt.Fprintln(w, "Nothing that ships has changed: no commit touching a released artifact carries a")
		_, _ = fmt.Fprintln(w, "feat, fix, perf, or breaking marker.")
	} else {
		_, _ = fmt.Fprintf(w, "== What sets it (%d commit(s) touching the released surface)\n", len(r.Raising))
		for _, v := range r.Raising {
			_, _ = fmt.Fprintf(w, "  %-5s %s %s\n", v.Level, v.Commit.SHA[:8], v.Commit.Subject)
			_, _ = fmt.Fprintf(w, "        via %s\n", strings.Join(trim(v.Shipped, 3), ", "))
		}
	}

	if len(r.Unresolved) > 0 {
		_, _ = fmt.Fprintf(w, "\n== Major, unresolved (%d breaking marker(s) on shipped code)\n", len(r.Unresolved))
		_, _ = fmt.Fprintf(w, "   Each is major only if %s had already published the surface it changed. A field\n", from)
		_, _ = fmt.Fprintln(w, "   added and reshaped inside this window broke nothing — which is what all three")
		_, _ = fmt.Fprintln(w, "   markers in v1.2.0..v1.3.0 were, and why v1.3.0 shipped as a minor.")
		for _, v := range r.Unresolved {
			_, _ = fmt.Fprintf(w, "  %s %s\n", v.Commit.SHA[:8], v.Commit.Subject)
		}
		reportCRDDelta(w, from, to)
	}

	if len(r.Withheld) > 0 {
		_, _ = fmt.Fprintf(w, "\n== Withheld (%d commit(s) whose type would raise the floor, but which ship nothing)\n", len(r.Withheld))
		_, _ = fmt.Fprintln(w, "   This is the gap between counting subjects and reading what a release contains.")
		for _, v := range r.Withheld {
			_, _ = fmt.Fprintf(w, "  %-5s %s %s\n", v.Level, v.Commit.SHA[:8], v.Commit.Subject)
		}
	}

	if len(r.CommentOnly) > 0 {
		_, _ = fmt.Fprintf(w, "\n== Comment-only (%d commit(s) on shipped paths whose diff cannot change a build)\n", len(r.CommentOnly))
		_, _ = fmt.Fprintln(w, "   Every edit these made inside a released package is a comment or whitespace, so")
		_, _ = fmt.Fprintln(w, "   the binary is byte-identical. Only Go source is read this way; a chart file, a")
		_, _ = fmt.Fprintln(w, "   file that does not scan, and an added or deleted one all count as shipping.")
		for _, v := range r.CommentOnly {
			_, _ = fmt.Fprintf(w, "  %-5s %s %s\n", v.Level, v.Commit.SHA[:8], v.Commit.Subject)
			_, _ = fmt.Fprintf(w, "        comments in %s\n", strings.Join(trim(v.Shipped, 3), ", "))
		}
	}

	if div := DivergentScopes(r, ShippingScopes(r)); len(div) > 0 {
		_, _ = fmt.Fprintf(w, "\n== Scope says otherwise (%d)\n", len(div))
		_, _ = fmt.Fprintln(w, "   These carry a scope that other commits do ship under, but touch nothing released.")
		_, _ = fmt.Fprintln(w, "   A reused scope string is why release.md tells you to check paths, not subjects.")
		for _, v := range div {
			_, _ = fmt.Fprintf(w, "  %s %s\n", v.Commit.SHA[:8], v.Commit.Subject)
		}
	}

	if len(r.NonConventional) > 0 {
		_, _ = fmt.Fprintf(w, "\n== Unreadable subjects (%d) — weight not assessed\n", len(r.NonConventional))
		for _, c := range r.NonConventional {
			_, _ = fmt.Fprintf(w, "  %s %s\n", c.SHA[:8], c.Subject)
		}
	}

	_, _ = fmt.Fprintln(w)
	switch r.Floor {
	case LevelNone:
		_, _ = fmt.Fprintln(w, "Nothing user-visible has accumulated; a tag would publish no change.")
	default:
		_, _ = fmt.Fprintf(w, "The floor is the minimum, not the recommendation: %s is what the merged work\n", r.Floor)
		_, _ = fmt.Fprintln(w, "already forces. Whether to cut at all is docs/operations/release.md § When to cut.")
	}
}

// reportCRDDelta narrows the major question with the one part of it that is
// measurable: whether the window removed a property the FROM tag published.
//
// It never answers the question. A clean result rules out a removed property
// and nothing else — see crdsurface.go for what it cannot see.
func reportCRDDelta(w *os.File, from, to string) {
	d, err := CRDSurfaceDelta(from, to)
	if err != nil {
		_, _ = fmt.Fprintf(w, "\n   (could not read the published CRD surface: %v)\n", err)
		return
	}
	if d.FromSeen == 0 || d.ToSeen == 0 {
		_, _ = fmt.Fprintf(w, "\n   (no CRD schemas found at %s or %s — %d and %d read; nothing narrowed)\n",
			from, to, d.FromSeen, d.ToSeen)
		return
	}
	_, _ = fmt.Fprintf(w, "\n   Published CRD surface, %s (%d schema files) → %s (%d): %d propert(y|ies) added, %d removed.\n",
		from, d.FromSeen, to, d.ToSeen, len(d.Added), len(d.Removed))
	if len(d.Removed) == 0 {
		_, _ = fmt.Fprintln(w, "   Nothing published was removed, so no marker above dropped a field an operator")
		_, _ = fmt.Fprintln(w, "   could be using. Still yours to check: a published field whose type changed or")
		_, _ = fmt.Fprintln(w, "   whose enum narrowed, and the non-CRD contracts (metrics, condition and Event")
		_, _ = fmt.Fprintln(w, "   reasons, chart values, env tunables) — release.md § Diff every surface.")
		return
	}
	_, _ = fmt.Fprintln(w, "   Removed from the published surface — each is a break unless the version itself")
	_, _ = fmt.Fprintln(w, "   was already withdrawn:")
	for _, p := range trim(d.Removed, 12) {
		_, _ = fmt.Fprintf(w, "     %s\n", p)
	}
}

// trim shortens an evidence list, keeping the count honest when it elides.
func trim(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(append([]string{}, items[:n]...), fmt.Sprintf("… %d more", len(items)-n))
}
