package main

import (
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// repo runs git for the tree holding the backlog file. Every method answers
// "" / nil / false outside a repository, which is what makes the git-baseline
// rules no-ops on a fresh clone with no origin rather than failures.
type repo struct {
	// dir is the repo root, so a pathspec — which git resolves against the
	// working directory, not the root — means the same thing on every call.
	dir    string
	prefix string // path from the root down to the backlog file's directory
	inRepo bool
}

// newRepo locates the repository containing dir.
func newRepo(dir string) *repo {
	r := &repo{dir: dir}
	prefix, ok := r.runOK("rev-parse", "--show-prefix")
	if !ok {
		return r
	}
	top, ok := r.runOK("rev-parse", "--show-toplevel")
	if !ok {
		return r
	}
	r.prefix, r.dir, r.inRepo = prefix, top, true
	return r
}

// run executes a git command and returns its trimmed stdout, or "" on any
// failure. Callers treat "" as "git could not answer", which is the same
// no-op path as "not a repository".
func (r *repo) run(args ...string) string {
	out, _ := r.runOK(args...)
	return out
}

// runOK is run plus whether git actually succeeded, for the commands whose
// legitimate output can be empty.
func (r *repo) runOK(args ...string) (string, bool) {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the args are this program's own git verbs plus a ref and the backlog path, never operator input.
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(out), "\n"), true
}

// ok reports whether a git command succeeded, for the predicate commands whose
// output is empty either way.
func (r *repo) ok(args ...string) bool {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: same fixed verb set as runOK.
	cmd.Dir = r.dir
	return cmd.Run() == nil
}

// relpath prints file relative to the repo root, for `git show REF:path`.
//
// Built from the prefix git reports rather than by stripping the toplevel off
// the front: on macOS the two spellings of a temp path (/var/… and
// /private/var/…) differ by a symlink, so the string strip silently failed to
// match and quietly disabled the git-baseline rules.
func (r *repo) relpath(file string) string {
	if !r.inRepo {
		return ""
	}
	return r.prefix + filepath.Base(file)
}

// baseline is the state the working file is measured against: a ref git reads,
// and the name a finding quotes for it. An empty ref means git could not answer,
// which is what makes the git-baseline rules no-ops rather than failures.
type baseline struct {
	ref   string
	label string
}

// baselineRef is the state the backlog is compared against: the pre-commit tree
// in --staged mode, otherwise the merge base with origin/main.
//
// The merge base, not origin/main's tip: every rule below asks what THIS branch
// changed, which is a question about the branch point. A row main deleted while
// the branch was behind is still present at the merge base, so it reads as
// pre-existing rather than added here — against the tip it read as new, and rule
// 12 then demanded an ID for a row another session had already finished (Q684).
func (r *repo) baselineRef(staged bool) baseline {
	if staged {
		return baseline{ref: "HEAD", label: "HEAD"}
	}
	if !r.ok("rev-parse", "--verify", "--quiet", "origin/main") {
		return baseline{}
	}
	// A shallow clone can carry origin/main with the common ancestor cut off. The
	// tip is then the only answer available, and the per-rule ancestry checks
	// below are what hold that case together.
	if base, ok := r.runOK("merge-base", "HEAD", "origin/main"); ok && base != "" {
		return baseline{ref: base, label: "origin/main"}
	}
	return baseline{ref: "origin/main", label: "origin/main"}
}

// show returns a file's content at a ref, or "" when it is absent there.
func (r *repo) show(ref, path string) string {
	return r.run("show", ref+":"+path)
}

// touchedBy returns the commit that last added or removed needle from path,
// searching only ref's history. Empty when the history never carried it.
func (r *repo) touchedBy(ref, path, needle string) string {
	return r.run("log", "-1", "--format=%H", "-S"+needle, ref, "--", path)
}

// isAncestorOfHEAD reports whether commit is already contained in HEAD.
func (r *repo) isAncestorOfHEAD(commit string) bool {
	return r.ok("merge-base", "--is-ancestor", commit, "HEAD")
}

// stagedFiles lists the repo-relative paths staged for commit.
func (r *repo) stagedFiles() ([]string, error) {
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "--diff-filter=ACMRD")
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached: %w", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func anchorNeedle(id string) string { return fmt.Sprintf(`<a id="%s"></a>`, id) }

// checkBaseline runs the three rules a deletion is only visible to: a flake
// row that vanished (8), a plan whose Progress verdict is owed (9), and a done
// row that came back (10).
func (l *linter) checkBaseline(b *backlog) {
	if l.rel == "" {
		return
	}
	from := l.git.baselineRef(l.cfg.staged)
	if from.ref == "" {
		return
	}
	baselineSrc := l.git.show(from.ref, l.rel)
	if baselineSrc == "" {
		return
	}
	base := parseBacklog([]byte(baselineSrc))

	l.checkFlakeRowsPreserved(b, base, from)
	l.checkNoResurrectedRows(b, base, from)
	l.checkNewIDsClaimed(b, base)
	l.checkProgressRederived(b, base, from)
}

// queueIDRefNS is the allocator's claim namespace. An ID with no claim was
// never reserved, so a concurrent session can still be handed it.
const queueIDRefNS = "refs/queue-ids"

// checkNewIDsClaimed is rule 12. Rule 1 removes the shared counter; this
// removes the other way to obtain an ID without reserving it, which is to read
// the file's highest and add one. Scoped to IDs this branch ADDS, and to IDs at
// or above the namespace's lowest claim — everything below predates the
// allocator and holds no ref.
func (l *linter) checkNewIDsClaimed(b, base *backlog) {
	// Collect the new IDs before touching the network: a branch that adds no
	// row pays nothing, which is most runs of `make check`.
	baseline := base.anchorIDs()
	var newIDs []string
	for _, id := range b.sortedAnchorIDs() {
		if !baseline[id] {
			newIDs = append(newIDs, id)
		}
	}
	if len(newIDs) == 0 {
		return
	}

	// No remote, no network, no claims yet: nothing to check against. An empty
	// namespace is indistinguishable from an unreachable one here, and both
	// mean this clone cannot answer the question.
	out, ok := l.git.runOK("ls-remote", "origin", queueIDRefNS+"/*")
	if !ok || out == "" {
		return
	}
	claims := map[string]bool{}
	floor := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ref := fields[len(fields)-1]
		id := path.Base(ref)
		if !queueIDRE.MatchString(id) {
			continue
		}
		claims[id] = true
		if n, err := strconv.Atoi(id[1:]); err == nil && (floor == 0 || n < floor) {
			floor = n
		}
	}

	for _, id := range newIDs {
		n, err := strconv.Atoi(id[1:])
		if err != nil || n < floor {
			continue
		}
		if claims[id] || contains(l.cfg.allowUnclaimedID, id) {
			continue
		}
		l.failWith(0, fmt.Sprintf(
			"%s is a new row here but holds no %s/%s claim, so it was never reserved and a concurrent session can be handed the same ID. Allocate it with make queue-id TITLE=... and renumber the row now, while it is still one file. See docs/development/queue-id-allocation.md",
			id, queueIDRefNS, id),
			"  An unclaimed ID was never reserved, so a concurrent session can still be\n"+
				"  handed it — and the loser renumbers the row, its anchor, every cross-\n"+
				"  reference, the plan doc, the PR body and the commit subject. Allocate it:\n"+
				"    make queue-id TITLE='the row title'\n"+
				"  then renumber now, while the change is still one file. See\n"+
				"  docs/development/queue-id-allocation.md.\n"+
				fmt.Sprintf("  Claimed from another clone or session? BACKLOG_ALLOW_UNCLAIMED_ID=%s\n", id))
	}
}

var queueIDRE = regexp.MustCompile(`^Q[0-9]+$`)

// checkFlakeRowsPreserved is rule 8. Once a flake mitigation ships the row
// moves to Deferred § Flake watch, so a recurrence reads as a recurrence
// rather than a fresh find.
func (l *linter) checkFlakeRowsPreserved(b, base *backlog, from baseline) {
	present := b.anchorIDs()
	for _, r := range base.queue {
		if r.id == "" || !strings.Contains(r.cell(2), "`flake`") {
			continue
		}
		// Still present anywhere in the file (Queue or Deferred) -> fine.
		if present[r.id] {
			continue
		}
		// Absent — but a branch opened before the row was filed never had it to
		// delete. Only flag when HEAD already carries the commit that added it;
		// otherwise every branch that is merely behind main reports a deletion
		// it did not make. (Same staleness trap as rule 9.)
		if added := l.git.touchedBy(from.ref, l.rel, anchorNeedle(r.id)); added != "" {
			if !l.git.isAncestorOfHEAD(added) {
				continue
			}
		}
		if contains(l.cfg.allowFlakeDelete, r.id) {
			continue
		}
		l.failWith(0, fmt.Sprintf(
			"%s was a flake-labelled Queue row in %s and is now gone; a shipped flake mitigation moves the row to Deferred, Flake watch (trigger: Event: recurs on main after the fix) — it is not deleted. See docs/development/maintaining-backlog.md#flake-fixes-go-first",
			r.id, from.label),
			"  A shipped flake mitigation moves the row to Deferred, Flake watch, with an\n"+
				"  \"Event: recurs on main after the fix\" trigger — kept, not closed, so a second\n"+
				"  occurrence reads as a recurrence rather than a fresh find. See\n"+
				"  docs/development/maintaining-backlog.md#flake-fixes-go-first.\n"+
				fmt.Sprintf("  Retiring the row instead? BACKLOG_ALLOW_FLAKE_DELETE=%s\n", r.id))
	}
}

// checkNoResurrectedRows is rule 10. It distinguishes the two cases a manual
// `comm` cannot: an ID missing from the baseline FILE is a new row when the
// baseline's HISTORY never carried it, and a resurrected done row when it did.
func (l *linter) checkNoResurrectedRows(b, base *backlog, from baseline) {
	baseline := base.anchorIDs()
	for _, id := range b.sortedAnchorIDs() {
		// Still in the baseline file: not a deletion, nothing to resurrect.
		if baseline[id] {
			continue
		}
		// Absent from the baseline file. If its history never held the anchor
		// either, this is simply a newly filed row.
		removedIn := l.git.touchedBy(from.ref, l.rel, anchorNeedle(id))
		if removedIn == "" {
			continue
		}
		// The history held it, so it was deleted — but by whom, relative to
		// this branch? If the deleting commit is not yet an ancestor of HEAD,
		// the branch simply predates the deletion and a rebase will apply it.
		// Only a branch that ALREADY carries the deletion and still shows the
		// row has actually brought it back.
		if !l.git.isAncestorOfHEAD(removedIn) {
			continue
		}
		if contains(l.cfg.allowResurrect, id) {
			continue
		}
		l.failWith(0, fmt.Sprintf(
			"%s is back in %s but %s deleted it — done rows are deleted, so this re-opens finished work. A reordered row merges cleanly over a delete, so a clean rebase is not evidence of a correct one. See docs/development/maintaining-backlog.md#a-moved-row-defeats-conflict-detection",
			id, filepath.Base(l.cfg.file), from.label),
			"  Done rows are deleted (git is the archive), so a row that comes back\n"+
				"  re-opens finished work. Reordering a row moves it, so a branch that\n"+
				"  relocates a row while main deletes it merges with NO conflict — a clean\n"+
				"  rebase is not evidence of a correct one. Check whether the work shipped:\n"+
				fmt.Sprintf("    git log -S'%s' --oneline %s -- %s\n", anchorNeedle(id), from.label, l.rel)+
				"  See docs/development/maintaining-backlog.md#a-moved-row-defeats-conflict-detection.\n"+
				fmt.Sprintf("  Deliberately re-opening it? BACKLOG_ALLOW_RESURRECT=%s\n", id))
	}
}

// checkProgressRederived is rule 9. Deleting the LAST Queue row that points at
// a plan changes that plan's Progress verdict to ✅ (deferred residuals don't
// count), and the flip must land in the same edit. Only plans whose last row
// just disappeared are checked: a steady-state scan would misread the many rows
// that merely cite a completed plan as evidence.
func (l *linter) checkProgressRederived(b, base *backlog, from baseline) {
	now := b.queuePlanLinks()
	for _, plan := range base.queuePlanLinks() {
		if contains(now, plan) {
			continue
		}
		// No Progress row, or already re-derived to ✅ -> nothing owed.
		st, line := b.progressStatus(plan)
		if st != "⚠️" {
			continue
		}
		if contains(l.cfg.allowProgressStop, plan) {
			continue
		}
		l.failWith(line, fmt.Sprintf(
			"the last Queue row pointing at %s is gone, but its Progress row is still ⚠️; a plan with only deferred residuals is ✅. Flip it in this same edit. See docs/development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count",
			plan),
			fmt.Sprintf("  It was in %s. ⚠️ means an open *Queue* row remains; deferred residuals do\n", from.label)+
				"  not count, so the row is now ✅. Flip it in this same edit — the Progress\n"+
				"  table is only ever re-derived by hand. See\n"+
				"  docs/development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count.\n"+
				fmt.Sprintf("  Was the vanished row only *citing* that plan, with real work left? BACKLOG_ALLOW_PROGRESS_STALE=%s\n", plan))
	}
}

// anchorIDs is the set of IDs with a row anchor anywhere in the file.
func (b *backlog) anchorIDs() map[string]bool {
	out := map[string]bool{}
	for _, rows := range [][]row{b.progress, b.queue, b.deferred} {
		for _, r := range rows {
			if r.anchor != "" {
				out[r.anchor] = true
			}
		}
	}
	return out
}

// sortedAnchorIDs lists the anchored IDs in row order, so findings come out
// stably.
func (b *backlog) sortedAnchorIDs() []string {
	var out []string
	seen := map[string]bool{}
	for _, rows := range [][]row{b.progress, b.queue, b.deferred} {
		for _, r := range rows {
			if r.anchor != "" && !seen[r.anchor] {
				seen[r.anchor] = true
				out = append(out, r.anchor)
			}
		}
	}
	return out
}

// queuePlanLinks lists every `plan/NAME.md` path linked from a Queue row.
// Deferred rows are deliberately excluded: a deferred residual does not hold a
// plan at ⚠️.
func (b *backlog) queuePlanLinks() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range b.queue {
		for _, m := range planRE.FindAllString(strings.Join(r.cells, "|"), -1) {
			p := strings.TrimPrefix(m, "(")
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// progressStatus returns the Progress table's Status cell for the row linking
// plan, with its source line. Empty when no such row exists.
func (b *backlog) progressStatus(plan string) (string, int) {
	for _, r := range b.progress {
		if !strings.Contains(r.cell(0), "("+plan) {
			continue
		}
		return strings.Join(strings.Fields(r.cell(2)), ""), r.line
	}
	return "", 0
}
