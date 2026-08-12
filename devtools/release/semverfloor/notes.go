package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// operatorDocs is the tree doc-update-matrix.md requires an operator-visible
// change to land in. It is the one signal that separates a release artifact
// published outside an image or chart from the dev tooling it sits among.
const operatorDocs = "docs/operations"

// Notes is the release-notes reading of a Result: the same classification the
// floor is taken from, partitioned into what a release publishes and what it
// does not.
//
// Ships and Residue are disjoint and exhaust the window's feat/fix/perf
// commits. That is the property the reconciliation rests on — a commit cannot
// leave one list without arriving in the other — so Total is printed beside
// them rather than recomputed by the reader.
type Notes struct {
	Ships   []Verdict
	Residue []ResidueItem
	Total   int
}

// ResidueItem is one commit the notes leave out, carrying why it was left out
// and the operator surface it touched anyway.
type ResidueItem struct {
	Verdict
	CommentOnly bool     // ships a byte-identical artifact, so nothing to describe
	Added       []string // operator pages this commit added
	Edited      []string // operator pages this commit changed
}

// Operator reports whether this commit changed an operator-facing page.
func (r ResidueItem) Operator() bool { return len(r.Added)+len(r.Edited) > 0 }

// EnumerateNotes partitions a classified window for the notes.
//
// It classifies nothing itself. Ships is Result.Raising — feat, fix and perf
// commits whose paths reach a released artifact — and the residue is everything
// else those types produced, which is the list release.md tells a human to read
// rather than trust.
func EnumerateNotes(r Result, pages OperatorPager) Notes {
	n := Notes{Ships: r.Raising}
	add := func(vs []Verdict, commentOnly bool) {
		for _, v := range vs {
			item := ResidueItem{Verdict: v, CommentOnly: commentOnly}
			if pages != nil {
				item.Added, item.Edited = pages.OperatorPages(v.Commit)
			}
			n.Residue = append(n.Residue, item)
		}
	}
	add(r.Withheld, false)
	add(r.CommentOnly, true)
	n.Total = len(n.Ships) + len(n.Residue)

	// Operator-facing first, and an added page ahead of an edited one: a new page
	// is a new capability, an edit to an existing runbook usually is not.
	sort.SliceStable(n.Residue, func(i, j int) bool {
		a, b := n.Residue[i], n.Residue[j]
		if len(a.Added) != len(b.Added) {
			return len(a.Added) > len(b.Added)
		}
		return a.Operator() && !b.Operator()
	})
	return n
}

// OperatorPager reports the operator pages a commit added and changed.
type OperatorPager interface {
	OperatorPages(c Commit) (added, edited []string)
}

// gitPager answers OperatorPager by re-reading the commit's name-status, which
// is the only place the added/changed distinction lives — readCommits keeps
// paths alone, since the floor never needs it.
type gitPager struct{ root string }

func (g gitPager) OperatorPages(c Commit) (added, edited []string) {
	if !touchesOperatorDocs(c) {
		return nil, nil
	}
	//nolint:gosec // G204: fixed git verbs plus a SHA this program read out of git itself.
	cmd := exec.Command("git", "show", "--format=", "--name-status", c.SHA)
	cmd.Dir = g.root
	out, err := cmd.Output()
	if err != nil {
		// The paths already said this commit touched the tree; losing the
		// added/changed split must not lose the commit itself.
		return nil, operatorFiles(c)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		// A rename reports the destination last; every other status has one path.
		p := f[len(f)-1]
		if !underOperatorDocs(p) {
			continue
		}
		if f[0][0] == 'A' {
			added = append(added, p)
		} else {
			edited = append(edited, p)
		}
	}
	return added, edited
}

func underOperatorDocs(p string) bool { return strings.HasPrefix(p, operatorDocs+"/") }

func touchesOperatorDocs(c Commit) bool {
	for _, f := range c.Files {
		if underOperatorDocs(f) {
			return true
		}
	}
	return false
}

func operatorFiles(c Commit) []string {
	var out []string
	for _, f := range c.Files {
		if underOperatorDocs(f) {
			out = append(out, f)
		}
	}
	return out
}

// checkOperatorDocs reports a problem when the operator tree is not where this
// program expects it. A flag that silently stops firing reads exactly like a
// window with nothing to flag, so the absence is said out loud.
func checkOperatorDocs(root string) string {
	info, err := os.Stat(filepath.Join(root, operatorDocs))
	if err != nil || !info.IsDir() {
		return fmt.Sprintf("%s/ is not a directory in this tree, so no residue commit can be flagged "+
			"operator-facing; the tree moved and this program did not follow", operatorDocs)
	}
	return ""
}

func reportNotes(w io.Writer, from, to string, commits []Commit, surface Surface, s Sources, n Notes, warning string) {
	_, _ = fmt.Fprintf(w, "Release notes enumeration %s..%s\n", from, to)
	_, _ = fmt.Fprintf(w, "%d commits (merges excluded), %d carrying feat/fix/perf\n\n", len(commits), n.Total)

	_, _ = fmt.Fprintf(w, "Released surface: %d package dir(s) from %d go build(s) across %d image(s), plus %d chart tree(s)\n",
		len(surface.PkgDirs), len(s.Builds), len(s.Images), len(s.Charts))
	_, _ = fmt.Fprintf(w, "  images: %s\n", strings.Join(s.Images, " "))
	_, _ = fmt.Fprintf(w, "  charts: %s\n\n", strings.Join(s.Charts, " "))

	if warning != "" {
		_, _ = fmt.Fprintf(w, "WARNING: %s\n\n", warning)
	}

	_, _ = fmt.Fprintf(w, "== Ships (%d of %d) — the enumeration to curate\n", len(n.Ships), n.Total)
	_, _ = fmt.Fprintln(w, "   Admitted by the paths each commit touched, so no scope string admits or excludes")
	_, _ = fmt.Fprintln(w, "   anything. Keep the trailing (#NNNN): GitHub auto-links a bare one in a release body.")
	for _, v := range n.Ships {
		mark := ""
		if v.Commit.Breaking {
			mark = "!"
		}
		_, _ = fmt.Fprintf(w, "  %-5s%-1s %s %s\n", v.Commit.Type, mark, v.Commit.SHA[:8], stripPrefix(v.Commit.Subject))
	}

	_, _ = fmt.Fprintf(w, "\n== Residue (%d of %d) — read it, never assume it is all tooling\n", len(n.Residue), n.Total)
	_, _ = fmt.Fprintln(w, "   Every feat/fix/perf commit reaching no released artifact. A release also publishes")
	_, _ = fmt.Fprintf(w, "   things no image and no chart carries, and this is where they hide: v1.4.0's runner\n")
	_, _ = fmt.Fprintln(w, "   template library (Q554) ships as deploy/templates/ and sits in this list.")

	var flagged, rest []ResidueItem
	for _, item := range n.Residue {
		if item.Operator() {
			flagged = append(flagged, item)
		} else {
			rest = append(rest, item)
		}
	}

	_, _ = fmt.Fprintf(w, "\n   -- Also changes %s/ (%d) — read every one\n", operatorDocs, len(flagged))
	_, _ = fmt.Fprintf(w, "      doc-update-matrix.md requires an operator-visible change to land there, so each of\n")
	_, _ = fmt.Fprintln(w, "      these moved an operator's surface with no binary behind it. An added page is a new")
	_, _ = fmt.Fprintln(w, "      capability; an edited one usually is not.")
	for _, item := range flagged {
		_, _ = fmt.Fprintf(w, "  %s\n", residueSubject(item))
		if len(item.Added) > 0 {
			_, _ = fmt.Fprintf(w, "        adds %s\n", strings.Join(trim(item.Added, 3), ", "))
		}
		if len(item.Edited) > 0 {
			_, _ = fmt.Fprintf(w, "        edits %s\n", strings.Join(trim(item.Edited, 3), ", "))
		}
	}

	_, _ = fmt.Fprintf(w, "\n   -- No operator surface (%d) — dev tooling, CI, tests\n", len(rest))
	for _, item := range rest {
		_, _ = fmt.Fprintf(w, "  %s\n", residueSubject(item))
	}

	_, _ = fmt.Fprintf(w, "\n%d ships + %d residue = %d feat/fix/perf commits in the window.\n",
		len(n.Ships), len(n.Residue), n.Total)
	_, _ = fmt.Fprintln(w, "Both lists are yours to read. The residue is the half a scope allow-list used to drop")
	_, _ = fmt.Fprintln(w, "silently — release.md § Writing the curated notes.")
}

func residueSubject(item ResidueItem) string {
	s := item.Commit.SHA[:8] + " " + item.Commit.Subject
	if item.CommentOnly {
		s += "  [comment-only: the artifact is byte-identical]"
	}
	return s
}

// stripPrefix removes the Conventional Commit prefix, which the notes never
// carry. The type is printed in its own column instead, so nothing is lost.
func stripPrefix(subject string) string {
	if m := subjectRE.FindString(subject); m != "" {
		return subject[len(m):]
	}
	return subject
}
