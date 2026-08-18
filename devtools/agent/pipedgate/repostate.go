package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// The two moments the repo-state checks attach to, matched against a call's
// HEAD — the same command-position discipline the gate patterns use. A commit
// message or a grep pattern that merely names `git push` parses as a word, so
// it is never a trigger (the Q624 shape).
var (
	gitPushHead    = regexp.MustCompile(`^git[[:space:]]+push([[:space:]]|$)`)
	ghPRCreateHead = regexp.MustCompile(`^gh[[:space:]]+pr[[:space:]]+create([[:space:]]|$)`)
)

// Repo answers questions about local git state and open pull requests. Every
// method returns an error rather than a partial answer. The four that decide
// whether a check fires at all read an error as silence: both checks are
// warnings, and one that fires because a probe failed — offline, a
// rate-limited or expired gh token, no origin/main — is worse than no warning
// at all. The two hunk probes only narrow a warning the path sets already
// raised, so an error there falls back to the path-only reading instead.
type Repo interface {
	// BranchFiles returns the paths this branch changes against the base.
	BranchFiles() ([]string, error)
	// BaseGained returns the paths the base gained since this branch left it.
	BaseGained() ([]string, error)
	// OpenPRs returns the open pull requests whose head is not this branch.
	OpenPRs() ([]PR, error)
	// MergeConflicts returns the paths merging the base into this branch leaves
	// conflicted, empty when the merge is clean.
	MergeConflicts() ([]string, error)
	// BranchHunks returns the line ranges this branch changes, by path.
	BranchHunks() (Hunks, error)
	// PRHunks returns the line ranges the given pull request changes, by path.
	PRHunks(number int) (Hunks, error)
}

// PR is one open pull request and the paths it changes.
type PR struct {
	Number int
	Files  []string
}

// LineRange is a half-open [Start, End) range of pre-image line numbers — the
// coordinate space both sides of a comparison share, because both are diffed
// against a merge base rather than against each other.
type LineRange struct {
	Start int
	End   int
}

// Hunks maps a path to the line ranges a diff changes in it.
type Hunks map[string][]LineRange

// repoStateWarning returns the warning for a command whose correctness depends
// on repository state rather than on its own text. Nothing runs until the parse
// finds a trigger at command position, so the probes cost nothing on the Bash
// calls that are not `git push` or `gh pr create`.
func repoStateWarning(f *syntax.File, reg *compiled, repo Repo) string {
	if repo == nil {
		return ""
	}
	switch {
	case hasCallHead(f, gitPushHead):
		return staleBaseWarning(reg, repo)
	case hasCallHead(f, ghPRCreateHead):
		return prOverlapWarning(reg, repo)
	}
	return ""
}

// staleBaseWarning warns when the base gained changes to files this branch also
// changes (Q665).
//
// Deliberately narrower than "the base moved". `main` takes ~47 merges a day
// here (measured 2026-08-05 over the preceding week) and a local gate runs for
// minutes, so a bare `git diff HEAD...origin/main` is non-empty at almost every
// push — a warning that always fires is one that is always accepted. The
// overlap is the part that costs a queue kickback.
func staleBaseWarning(reg *compiled, repo Repo) string {
	gained, err := repo.BaseGained()
	if err != nil || len(gained) == 0 {
		return ""
	}
	mine, err := repo.BranchFiles()
	if err != nil {
		return ""
	}
	overlap := intersect(mine, gained, reg.overlapIgnore)
	dirty := conflictingIgnored(repo, reg, mine, gained)
	if len(overlap) == 0 && len(dirty) == 0 {
		return ""
	}
	all := append(append([]string{}, overlap...), dirty...)
	sort.Strings(all)

	msg := "`git push` — " + reg.baseRef + " has moved and changed " + files(len(all)) +
		" this branch also changes: " + joinPaths(all, 5) + ". "
	if len(dirty) > 0 {
		msg += "Merging " + reg.baseRef + " already leaves " + joinPaths(dirty, 2) +
			" conflicted, so the discount that normally covers the merge-driver-owned files does not " +
			"apply here: this branch is dirty now, not at kickback time. "
	}
	return msg + "A stale base on its own is benign, " +
		"because the merge queue validates the candidate merge — but an overlap is where a kickback costs " +
		"a full check cycle that a local re-run would have caught first. Rebase onto " + reg.baseRef +
		" and re-run the gate; if the overlap cannot affect it, re-run this push prefixed with " +
		overrideVar + "=<reason>. Read from the local " +
		reg.baseRef + " ref, so it under-reports until you fetch. " +
		"See CONTRIBUTING.md#pushing-to-a-pr-that-is-already-open."
}

// conflictingIgnored returns the discounted paths this merge would actually
// leave conflicted (Q790).
//
// The discount reads docs/STATUS.md as row-ID resolved, and the driver usually
// does resolve it — but it refuses on a row deleted on one side and edited on
// the other, and on any conflict outside the Queue rows, which is what a flake
// row moving to Deferred § Flake watch writes. #1383 changed docs/STATUS.md and
// nothing else, #1384 landed under it, and the check that exists to catch that
// stayed silent because the only overlapping path was discounted.
//
// The verdict is git's own rather than a second reading of the tables here, so
// nothing can drift from the driver: `git merge-tree` runs whatever merge this
// clone would run — the custom driver where `make merge-driver` installed one,
// the plain three-way merge where it did not.
//
// It runs only when a discounted path is in both change sets, which keeps its
// cost off every other push: measured 2026-08-11 on this repo, 70-100 ms across
// a 20-commit divergence, against ~5 ms for the two `git diff`s beside it.
func conflictingIgnored(repo Repo, reg *compiled, mine, gained []string) []string {
	discounted := make(map[string]bool, len(reg.overlapIgnore))
	for _, p := range intersect(mine, gained, nil) {
		if reg.overlapIgnore[p] {
			discounted[p] = true
		}
	}
	if len(discounted) == 0 {
		return nil
	}
	conflicts, err := repo.MergeConflicts()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range conflicts {
		if discounted[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// prOverlapWarning warns when an open PR already changes lines this branch
// changes (Q668, narrowed by Q862).
func prOverlapWarning(reg *compiled, repo Repo) string {
	mine, err := repo.BranchFiles()
	if err != nil || len(mine) == 0 {
		return ""
	}
	prs, err := repo.OpenPRs()
	if err != nil {
		return ""
	}
	hunks := hunkOverlap{repo: repo, budget: prDiffBudget}
	var hits []string
	for _, pr := range prs {
		overlap := intersect(mine, pr.Files, reg.overlapIgnore)
		if len(overlap) == 0 {
			continue
		}
		overlap, measured := hunks.narrow(pr.Number, overlap)
		if len(overlap) == 0 {
			continue
		}
		hit := "#" + strconv.Itoa(pr.Number) + " (" + joinPaths(overlap, 3)
		if !measured {
			hit += ", paths only"
		}
		hits = append(hits, hit+")")
		if len(hits) == 3 {
			break
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return "`gh pr create` — an open PR already changes lines this branch changes: " + strings.Join(hits, ", ") +
		". Overlapping PRs duplicate or invalidate each other, and the title says nothing about it. Read that " +
		"PR's diff and body, then re-run this create prefixed with " + overrideVar + "=<reason> saying what it " +
		"claims, not only where it sits — reading it is the fix, and re-scoping this PR to dodge the overlap " +
		"usually is not. Merge-driver-owned files are excluded, and so are hunks too far apart to touch; " +
		"`paths only` marks a PR whose diff could not be read, reported on its file list alone. " +
		"See CONTRIBUTING.md#re-check-concurrent-work-before-opening."
}

// prDiffBudget caps the per-PR diff fetches one `gh pr create` can pay for.
// Only a PR already sharing a path is a candidate and the message renders three
// hits, so a fourth candidate keeps the path-only reading rather than adding a
// 5 s round trip to a hook that holds up the Bash call it fires on.
const prDiffBudget = 3

// hunkOverlap narrows a path overlap to the paths whose changed line ranges can
// reach each other (Q862).
//
// Path sets alone read two edits at opposite ends of one large file as a
// collision: three sightings in two days (#1505, #1527, #1531), each costing a
// manual `git merge-tree` and an override. Diff context is what decides —
// measured 2026-08-12, two edits to adjacent table rows conflicted while two
// 180 lines apart rebased as a pure line offset
// (docs/development/parallel-dispatch.md).
//
// Every failure keeps the path-only reading rather than dropping the warning.
// This fires on every session in this repo at once, so silence on a real
// collision costs more than the false positive being narrowed away.
type hunkOverlap struct {
	repo   Repo
	mine   Hunks
	looked bool
	budget int
}

// narrow returns the paths whose ranges can reach each other, and whether the
// ranges were read at all. An unmeasured verdict keeps every path it was given.
func (h *hunkOverlap) narrow(number int, paths []string) ([]string, bool) {
	if h.budget <= 0 {
		return paths, false
	}
	if !h.looked {
		h.looked = true
		if mine, err := h.repo.BranchHunks(); err == nil {
			h.mine = mine
		}
	}
	if len(h.mine) == 0 {
		return paths, false
	}
	h.budget--
	theirs, err := h.repo.PRHunks(number)
	if err != nil || len(theirs) == 0 {
		return paths, false
	}
	var kept []string
	for _, p := range paths {
		if rangesTouch(h.mine[p], theirs[p]) {
			kept = append(kept, p)
		}
	}
	return kept, true
}

// rangesTouch reports whether two sets of changed ranges can affect each other.
// An empty set means that path's ranges were not read — a binary file, a rename
// carrying no hunks, a diff GitHub declined to generate — and reads as touching,
// because then the shared path is the only evidence there is.
func rangesTouch(a, b []LineRange) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x.Start < y.End && y.Start < x.End {
				return true
			}
		}
	}
	return false
}

// hunkHeader reads the pre-image side of a `@@ -a,b +c,d @@` header.
var hunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+`)

// parseHunks reads a unified diff into the pre-image line ranges it changes, by
// path. Both sides carry the default three lines of context and a `@@ -a,b`
// count includes them, so two edits closer together than the context they carry
// already share a range here — the granularity a rebase conflicts at.
//
// A file header is a `--- ` line immediately followed by a `+++ ` line. The pair
// is what makes it one: a removed line whose own text begins `-- ` renders as
// `--- ` in the body, and alone would be read as a header.
func parseHunks(raw []byte) Hunks {
	out := Hunks{}
	path, prev := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "+++ ") && strings.HasPrefix(prev, "--- "):
			path = headerPath(line[4:], prev[4:])
		case strings.HasPrefix(line, "@@ ") && path != "":
			if r, ok := parseHunkHeader(line); ok {
				out[path] = append(out[path], r)
			}
		}
		prev = line
	}
	return out
}

// parseHunkHeader reads the pre-image range. An absent count means one line, and
// a zero count is an insertion between two lines, given width so it can still
// reach its neighbours.
func parseHunkHeader(line string) (LineRange, bool) {
	m := hunkHeader.FindStringSubmatch(line)
	if m == nil {
		return LineRange{}, false
	}
	start, err := strconv.Atoi(m[1])
	if err != nil {
		return LineRange{}, false
	}
	count := 1
	if m[2] != "" {
		if count, err = strconv.Atoi(m[2]); err != nil {
			return LineRange{}, false
		}
	}
	if count == 0 {
		count = 1
	}
	return LineRange{Start: start, End: start + count}, true
}

// headerPath takes the path from the `+++` side, falling back to the `---` side
// for a deletion, whose `+++` is /dev/null. The a/ and b/ prefixes are pinned on
// the git call rather than guessed here, so a session with diff.noprefix set
// still parses.
func headerPath(plus, minus string) string {
	if p := headerField(plus, "b/"); p != "" {
		return p
	}
	return headerField(minus, "a/")
}

func headerField(field, prefix string) string {
	field = strings.SplitN(field, "\t", 2)[0]
	if !strings.HasPrefix(field, prefix) {
		return ""
	}
	return strings.TrimPrefix(field, prefix)
}

// hasCallHead reports whether any call in the tree has a head matching re. A
// capability probe is not a trigger: `git push --help` prints usage and pushes
// nothing, so neither check has a moment to attach to — and skipping it skips
// their subprocesses too.
func hasCallHead(f *syntax.File, re *regexp.Regexp) bool {
	return findCall(f, func(c *syntax.CallExpr, head string) bool {
		return re.MatchString(head) && !isProbe(c)
	}) != ""
}

// intersect returns the paths in both lists, minus the ignored ones, sorted so
// the warning text is stable.
func intersect(a, b []string, ignore map[string]bool) []string {
	inA := make(map[string]bool, len(a))
	for _, p := range a {
		inA[p] = true
	}
	seen := make(map[string]bool, len(b))
	var out []string
	for _, p := range b {
		if !inA[p] || ignore[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return strconv.Itoa(n) + " files"
}

// joinPaths renders at most n paths, naming how many it left out.
func joinPaths(paths []string, n int) string {
	if len(paths) <= n {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:n], ", ") + " and " + strconv.Itoa(len(paths)-n) + " more"
}

// execRepo answers from the working tree and the GitHub API.
type execRepo struct {
	dir     string
	baseRef string
}

// Probe timeouts. A hook holds up the Bash call it fires on, so a dead network
// has to cost a bounded pause and then silence. `gh pr list --json files`
// measured 0.5 s against this repo on 2026-08-05.
const (
	gitTimeout = 3 * time.Second
	ghTimeout  = 5 * time.Second
)

func (r execRepo) BranchFiles() ([]string, error) {
	return r.lines(gitTimeout, "git", "diff", "--name-only", r.baseRef+"...HEAD")
}

func (r execRepo) BaseGained() ([]string, error) {
	return r.lines(gitTimeout, "git", "diff", "--name-only", "HEAD..."+r.baseRef)
}

// BranchHunks reads this branch's own changed ranges. The a/ and b/ prefixes are
// pinned because diff.noprefix is a config a session may have set and the parser
// keys on them; verified 2026-08-18 on git 2.55.0 that the flags win over it.
func (r execRepo) BranchHunks() (Hunks, error) {
	out, err := r.output(gitTimeout, "git", "diff", "--src-prefix=a/", "--dst-prefix=b/", r.baseRef+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseHunks(out), nil
}

// PRHunks reads an open PR's changed ranges. `gh pr diff` is GitHub's own diff
// against that PR's merge base, which is not necessarily this branch's: when
// something landed between the two branch points the pre-image numbers are
// shifted, and the context each range carries is the tolerance for it. Captured
// from the real command against #1610 on 2026-08-18 — the same a/ b/ prefixes
// and `@@ -a,b +c,d @@` headers git prints locally.
func (r execRepo) PRHunks(number int) (Hunks, error) {
	out, err := r.output(ghTimeout, "gh", "pr", "diff", strconv.Itoa(number))
	if err != nil {
		return nil, err
	}
	return parseHunks(out), nil
}

// MergeConflicts asks git what merging the base into this branch leaves
// conflicted. `--write-tree` is the only mode there is: it writes the merged
// blobs and trees as unreferenced objects that `git gc` prunes, and moves no
// ref, so a PreToolUse hook running it changes nothing the session can see.
//
// Exit 1 is the answer rather than a failure — it means "merged, with
// conflicts". It is also what an unresolvable ref exits, the ambiguity
// scripts/agent/pr-requeue-eligible.sh resolves by verifying the ref separately;
// here stdout separates them, because every merge that ran prints at least the
// tree OID and a ref that would not resolve prints nothing (measured on git
// 2.55.0, 2026-08-11). A merge that cannot be measured has to reach the caller
// as an error, never as an empty conflict set that reads like a clean merge.
func (r execRepo) MergeConflicts() ([]string, error) {
	out, err := r.output(gitTimeout, "git", "merge-tree", "--write-tree", "-z", "HEAD", r.baseRef)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(out) == 0 {
			return nil, err
		}
	}
	return parseMergeTree(out), nil
}

// parseMergeTree reads `git merge-tree --write-tree -z`: the merged tree's OID,
// then one `<mode> <oid> <stage>\t<path>` record per conflicted stage, then an
// empty record ahead of the informational messages. Captured from git 2.55.0 on
// 2026-08-11. A clean merge prints the OID and nothing else, so the loop ends on
// its first record.
func parseMergeTree(raw []byte) []string {
	recs := bytes.Split(raw, []byte{0})
	if len(recs) < 2 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, rec := range recs[1:] {
		if len(rec) == 0 {
			break
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		path := string(rec[tab+1:])
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func (r execRepo) OpenPRs() ([]PR, error) {
	branch, err := r.lines(gitTimeout, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	out, err := r.output(ghTimeout, "gh", "pr", "list", "--state", "open", "--limit", "100",
		"--json", "number,headRefName,files")
	if err != nil {
		return nil, err
	}
	current := ""
	if len(branch) > 0 {
		current = branch[0]
	}
	return parsePRList(out, current)
}

// parsePRList reads `gh pr list --json number,headRefName,files`, dropping the
// current branch's own PR: at `gh pr create` there is normally none, but a
// re-run after one exists must not report the branch overlapping itself.
func parsePRList(raw []byte, currentBranch string) ([]PR, error) {
	var listed []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
		Files       []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, err
	}
	var out []PR
	for _, l := range listed {
		if l.HeadRefName == currentBranch {
			continue
		}
		pr := PR{Number: l.Number}
		for _, f := range l.Files {
			pr.Files = append(pr.Files, f.Path)
		}
		out = append(out, pr)
	}
	return out, nil
}

func (r execRepo) output(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: argv is built in this file from constants plus a base_ref validated against refPattern, and no shell is involved
	cmd.Dir = r.dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Returned alongside the error, because `git merge-tree` reports its answer
	// on stdout with a non-zero status.
	err := cmd.Run()
	return stdout.Bytes(), err
}

func (r execRepo) lines(timeout time.Duration, name string, args ...string) ([]string, error) {
	out, err := r.output(timeout, name, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}
