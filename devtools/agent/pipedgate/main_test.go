package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a checkout with a worktree beside it, each on its own branch, and
// a base ref both diverge from. The two branches change different files, so an
// answer about one is visibly not an answer about the other — which is the only
// way this defect shows at all.
type fixture struct {
	checkout string
	worktree string
	registry string
}

// newFixture builds the two trees. checkoutFile is what the launch checkout's
// branch changes and worktreeFile what the worktree's branch changes; the base
// gains alpha.txt and nothing else, so naming alpha.txt on one side decides
// which tree got probed.
func newFixture(t *testing.T, checkoutFile, worktreeFile string) fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	f := fixture{
		checkout: filepath.Join(base, "checkout"),
		worktree: filepath.Join(base, "worktree"),
		registry: filepath.Join(base, "checkout", ".claude", "piped-gate-guard.json"),
	}
	if err := os.MkdirAll(filepath.Join(f.checkout, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // G204: argv is this test's own literals, and no shell is involved
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	git(f.checkout, "init", "-q", "-b", "main", ".")
	git(f.checkout, "config", "user.email", "t@example.com")
	git(f.checkout, "config", "user.name", "t")
	write(f.checkout, "alpha.txt", "base\n")
	write(f.checkout, "beta.txt", "base\n")
	git(f.checkout, "add", "alpha.txt", "beta.txt")
	git(f.checkout, "commit", "-qm", "base")
	git(f.checkout, "branch", "checkout-branch")
	git(f.checkout, "branch", "worktree-branch")

	// The base moves ahead of both branch points, changing alpha.txt only.
	write(f.checkout, "alpha.txt", "moved\n")
	git(f.checkout, "commit", "-qam", "base moves")
	git(f.checkout, "update-ref", "refs/remotes/origin/main", "main")

	git(f.checkout, "checkout", "-q", "checkout-branch")
	write(f.checkout, checkoutFile, "checkout\n")
	git(f.checkout, "commit", "-qam", "checkout branch")

	git(f.checkout, "worktree", "add", "-q", f.worktree, "worktree-branch")
	write(f.worktree, worktreeFile, "worktree\n")
	git(f.worktree, "commit", "-qam", "worktree branch")

	reg := Registry{Gates: []string{`^make([[:space:]]|$)`}, BaseRef: "origin/main"}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.registry, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// pushReason runs the tool over a `git push` payload carrying cwd, and returns
// the decision's reason — empty for silence.
func pushReason(t *testing.T, f fixture, cwd string) string {
	t.Helper()
	in := fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":"git push -u origin HEAD"}}`, cwd)
	var out bytes.Buffer
	if rc := run(f.registry, strings.NewReader(in), &out); rc != 0 {
		t.Fatalf("run exited %d, want 0: a hook must never fail the call it fires on", rc)
	}
	if out.Len() == 0 {
		return ""
	}
	var decision hookOutput
	if err := json.Unmarshal(out.Bytes(), &decision); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return decision.HookSpecificOutput.PermissionDecisionReason
}

// The stale-base check must answer about the tree the push will run in, not the
// one the hook was installed in (Q859). Both directions are asserted because
// either alone reads as a pass under the other's bug: judging the wrong checkout
// invents an overlap on a branch that has none, and hides the one that does.
func TestRepoStateProbesJudgeTheSessionsWorktree(t *testing.T) {
	// One fixture per arrangement, shared across the subtests that read it: the
	// probes only read, and building a checkout plus a worktree is the expensive
	// part of this suite.
	worktreeOverlaps := newFixture(t, "beta.txt", "alpha.txt")
	checkoutOverlaps := newFixture(t, "alpha.txt", "beta.txt")

	t.Run("overlap the worktree has and the checkout does not", func(t *testing.T) {
		got := pushReason(t, worktreeOverlaps, worktreeOverlaps.worktree)
		if !strings.Contains(got, "alpha.txt") {
			t.Errorf("want a warning naming alpha.txt, got: %q", got)
		}
	})

	t.Run("overlap the checkout has and the worktree does not", func(t *testing.T) {
		if got := pushReason(t, checkoutOverlaps, checkoutOverlaps.worktree); got != "" {
			t.Errorf("want silence for a branch with no overlap, got: %s", got)
		}
	})

	// A cwd naming no checkout has to leave the check working as it did rather
	// than switch it off: this hook fires on every Bash call in the repo, so
	// falling back to the registry's own tree beats falling silent.
	t.Run("cwd outside any checkout falls back to the registry root", func(t *testing.T) {
		for name, cwd := range map[string]string{
			"no repo above it": t.TempDir(),
			"absent":           "",
			"since removed":    filepath.Join(t.TempDir(), "gone"),
		} {
			t.Run(name, func(t *testing.T) {
				got := pushReason(t, checkoutOverlaps, cwd)
				if !strings.Contains(got, "alpha.txt") {
					t.Errorf("want the registry root's warning naming alpha.txt, got: %q", got)
				}
			})
		}
	})

	// A session working in the checkout the hook was installed in is the common
	// case, and cwd has to agree with the registry there.
	t.Run("cwd in the registry's own checkout", func(t *testing.T) {
		got := pushReason(t, checkoutOverlaps, checkoutOverlaps.checkout)
		if !strings.Contains(got, "alpha.txt") {
			t.Errorf("want a warning naming alpha.txt, got: %q", got)
		}
	})
}

// worktreeRoot answers from .git alone, which is a directory in a primary
// checkout and a file in a linked worktree.
func TestWorktreeRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := worktreeRoot(nested); got != "" {
		t.Errorf("no .git anywhere above = %q, want empty", got)
	}

	for _, tc := range []struct{ name, entry string }{
		{"directory", "dir"},
		{"file", "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			deep := filepath.Join(dir, "x", "y")
			if err := os.MkdirAll(deep, 0o750); err != nil {
				t.Fatal(err)
			}
			dotGit := filepath.Join(dir, ".git")
			var err error
			if tc.entry == "dir" {
				err = os.Mkdir(dotGit, 0o750)
			} else {
				err = os.WriteFile(dotGit, []byte("gitdir: /elsewhere\n"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := worktreeRoot(deep); got != dir {
				t.Errorf("worktreeRoot from a subdirectory = %q, want %q", got, dir)
			}
		})
	}
}
