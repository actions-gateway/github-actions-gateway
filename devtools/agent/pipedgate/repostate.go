package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// method returns an error rather than a partial answer, and every caller reads
// an error as silence: both checks are warnings, and one that fires because a
// probe failed — offline, a rate-limited or expired gh token, no origin/main —
// is worse than no warning at all.
type Repo interface {
	// BranchFiles returns the paths this branch changes against the base.
	BranchFiles() ([]string, error)
	// BaseGained returns the paths the base gained since this branch left it.
	BaseGained() ([]string, error)
	// OpenPRs returns the open pull requests whose head is not this branch.
	OpenPRs() ([]PR, error)
}

// PR is one open pull request and the paths it changes.
type PR struct {
	Number int
	Files  []string
}

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
	if len(overlap) == 0 {
		return ""
	}
	return "`git push` — " + reg.baseRef + " has moved and changed " + files(len(overlap)) +
		" this branch also changes: " + joinPaths(overlap, 5) + ". A stale base on its own is benign, " +
		"because the merge queue validates the candidate merge — but an overlap is where a kickback costs " +
		"a full check cycle that a local re-run would have caught first. Rebase onto " + reg.baseRef +
		" and re-run the gate; if the overlap cannot affect it, re-run this push prefixed with " +
		overrideVar + "=<reason>. Read from the local " +
		reg.baseRef + " ref, so it under-reports until you fetch. " +
		"See CONTRIBUTING.md#pushing-to-a-pr-that-is-already-open."
}

// prOverlapWarning warns when an open PR already changes files this branch
// changes (Q668).
func prOverlapWarning(reg *compiled, repo Repo) string {
	mine, err := repo.BranchFiles()
	if err != nil || len(mine) == 0 {
		return ""
	}
	prs, err := repo.OpenPRs()
	if err != nil {
		return ""
	}
	var hits []string
	for _, pr := range prs {
		overlap := intersect(mine, pr.Files, reg.overlapIgnore)
		if len(overlap) == 0 {
			continue
		}
		hits = append(hits, "#"+strconv.Itoa(pr.Number)+" ("+joinPaths(overlap, 3)+")")
		if len(hits) == 3 {
			break
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return "`gh pr create` — an open PR already changes files this branch changes: " + strings.Join(hits, ", ") +
		". Overlapping PRs duplicate or invalidate each other, and the title says nothing about it: read the " +
		"diff and the body before opening, or scope this PR so they do not overlap. If the overlap is " +
		"intended, re-run this create prefixed with " + overrideVar + "=<reason>. The merge-driver-owned " +
		"files are excluded, so this is a real overlap and not a shared backlog edit. " +
		"See CONTRIBUTING.md#re-check-concurrent-work-before-opening."
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
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
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
