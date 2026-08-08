package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRepo stands in for the working tree and the GitHub API. The exec
// implementation is two `git diff`s and one `gh pr list`; what needs asserting
// is the decision they feed, and — separately, in TestParsePRList — the JSON
// shape they return.
type fakeRepo struct {
	branchFiles []string
	baseGained  []string
	openPRs     []PR
	branchErr   error
	baseErr     error
	prErr       error
}

func (r fakeRepo) BranchFiles() ([]string, error) { return r.branchFiles, r.branchErr }
func (r fakeRepo) BaseGained() ([]string, error)  { return r.baseGained, r.baseErr }
func (r fakeRepo) OpenPRs() ([]PR, error)         { return r.openPRs, r.prErr }

var errProbe = errors.New("probe failed")

// mergeDriverOwned reads the paths a custom merge driver resolves from
// .gitattributes, which is their source of truth. The registry's
// overlap_ignore has to track that file: a third driver-owned path added there
// and not here would start counting as a real overlap on every branch, and
// both checks would fire always.
func mergeDriverOwned(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".gitattributes"))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "merge=") {
			continue
		}
		out = append(out, strings.Fields(line)[0])
	}
	if len(out) == 0 {
		t.Fatal(".gitattributes lists no merge-driver-owned paths")
	}
	return out
}

// Both directions, for the same reason the status suite asserts both: a check
// that stops firing lets the original waste back in (a queue kickback, an
// overlapping PR), and one that fires too widely turns `git push` and
// `gh pr create` into unconditional permission prompts, which get accepted
// without reading. The Q624 shapes are here because that is exactly how a hook
// starts matching text that merely names a command.
func TestRepoStateWarnings(t *testing.T) {
	reg := shippedRegistry(t)
	driverOwned := mergeDriverOwned(t)

	cases := []struct {
		name   string
		cmd    string
		repo   fakeRepo
		warn   bool
		substr string
	}{
		// --- Q665: a base that moved into this branch's own files ------------
		{
			name:   "push with an overlapping moved base",
			cmd:    "git push -u origin HEAD",
			repo:   fakeRepo{branchFiles: []string{"cmd/agc/run.go", "Makefile"}, baseGained: []string{"cmd/agc/run.go", "README.md"}},
			warn:   true,
			substr: "cmd/agc/run.go",
		},
		{
			name:   "the overlap, not the move, is the signal",
			cmd:    "git push --force-with-lease",
			repo:   fakeRepo{branchFiles: []string{"docs/design/05-security.md"}, baseGained: []string{"docs/design/05-security.md"}},
			warn:   true,
			substr: "origin/main",
		},
		// ~47 merges a day land on main, so a base that moved in files this
		// branch does not touch is the common case and must stay silent.
		{
			name: "moved base with no overlap",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{branchFiles: []string{"cmd/agc/run.go"}, baseGained: []string{"README.md", "docs/design/01-overview.md"}},
		},
		{
			name: "a base that did not move",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{branchFiles: []string{"cmd/agc/run.go"}},
		},
		// Every branch edits the backlog, and a merge driver resolves it by row
		// ID. Counting it would make this fire on every push.
		{
			name: "overlap only in the merge-driver-owned files",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{branchFiles: driverOwned, baseGained: driverOwned},
		},
		{
			name: "a driver-owned overlap does not mask a real one",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{branchFiles: []string{"docs/STATUS.md", "Makefile"}, baseGained: []string{"docs/STATUS.md", "Makefile"}},
			warn: true,
		},
		// No origin/main, a shallow clone, no git at all.
		{
			name: "a failed base probe is silent",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseErr: errProbe},
		},
		{
			name: "a failed branch probe is silent",
			cmd:  "git push -u origin HEAD",
			repo: fakeRepo{baseGained: []string{"Makefile"}, branchErr: errProbe},
		},

		// --- Q668: an open PR that already changes these files ---------------
		{
			name:   "pr create overlapping an open PR",
			cmd:    "gh pr create --fill",
			repo:   fakeRepo{branchFiles: []string{".github/workflows/publish.yml"}, openPRs: []PR{{Number: 1267, Files: []string{".github/workflows/publish.yml", ".github/workflows/dockerfile-lint.yml"}}}},
			warn:   true,
			substr: "#1267",
		},
		{
			name: "pr create with no overlap",
			cmd:  "gh pr create --fill",
			repo: fakeRepo{branchFiles: []string{"cmd/agc/run.go"}, openPRs: []PR{{Number: 1267, Files: []string{".github/workflows/publish.yml"}}}},
		},
		{
			name: "pr create with no open PRs at all",
			cmd:  `gh pr create --title "x" --body "y"`,
			repo: fakeRepo{branchFiles: []string{"cmd/agc/run.go"}},
		},
		{
			name: "pr create overlapping only the merge-driver-owned files",
			cmd:  "gh pr create --fill",
			repo: fakeRepo{branchFiles: driverOwned, openPRs: []PR{{Number: 1267, Files: driverOwned}}},
		},
		// Offline, an expired or rate-limited token: failing closed on a warning
		// is worse than useless, so the probe error is silence.
		{
			name: "a failed gh probe is silent",
			cmd:  "gh pr create --fill",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, prErr: errProbe},
		},
		{
			name: "a branch with no files cannot overlap",
			cmd:  "gh pr create --fill",
			repo: fakeRepo{openPRs: []PR{{Number: 1267, Files: []string{"Makefile"}}}},
		},

		// --- Commands that merely NAME the trigger (the Q624 shape) ----------
		// Every one of these would fire if the trigger were matched against the
		// raw command string instead of a call's head.
		{
			name: "commit message quoting git push",
			cmd:  `git commit -m "docs: say when to git push after a rebase"`,
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
		},
		{
			name: "commit message in a heredoc body",
			cmd:  "git commit -F - <<'EOF'\nfix: warn before gh pr create overlaps an open PR\nEOF",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},
		{
			name: "grep for the command in docs",
			cmd:  `grep -rn "git push" CONTRIBUTING.md`,
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
		},
		{
			name: "echo naming the command",
			cmd:  `echo "next step: gh pr create"`,
			repo: fakeRepo{branchFiles: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},
		{
			name: "git show of a file containing it",
			cmd:  `git show origin/main:CONTRIBUTING.md | grep -n "gh pr create"`,
			repo: fakeRepo{branchFiles: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},

		// --- Capability probes are not the trigger either (Q730) -------------
		// `--help` prints usage and touches neither the remote nor a PR, so
		// there is no moment for these checks to attach to.
		{
			name: "git push --help",
			cmd:  "git push --help | head",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
		},
		{
			name: "gh pr create --help",
			cmd:  "gh pr create --help",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},
		// A flag spelled inside a quoted argument is a word, so the real push
		// underneath it still warns.
		{
			name: "push after a commit whose message names --help",
			cmd:  `git commit -m "docs: document --help" && git push -u origin HEAD`,
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
			warn: true,
		},

		// --- Neighbouring commands that are not the trigger ------------------
		{
			name: "gh pr list is not gh pr create",
			cmd:  "gh pr list --state open",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},
		{
			name: "gh pr view is not gh pr create",
			cmd:  "gh pr view 1267 --json state",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, openPRs: []PR{{Number: 1, Files: []string{"Makefile"}}}},
		},
		{
			name: "git pull is not git push",
			cmd:  "git pull --ff-only",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
		},
		{
			name: "git push-related plumbing is not git push",
			cmd:  "git push-nonexistent",
			repo: fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.cmd, false, reg, tc.repo)
			if tc.warn && got == "" {
				t.Fatalf("want a warning, got silence\ncommand: %s", tc.cmd)
			}
			if !tc.warn && got != "" {
				t.Fatalf("want silence, got a warning\ncommand: %s\nreason: %s", tc.cmd, got)
			}
			if tc.substr != "" && !strings.Contains(got, tc.substr) {
				t.Fatalf("reason missing %q\nreason: %s", tc.substr, got)
			}
		})
	}
}

// A nil Repo disables the checks rather than panicking: every other suite, and
// any caller that has no repo to probe, passes one.
func TestNilRepoIsSilent(t *testing.T) {
	reg := shippedRegistry(t)
	for _, cmd := range []string{"git push -u origin HEAD", "gh pr create --fill"} {
		if got := Decide(cmd, false, reg, nil); got != "" {
			t.Errorf("want silence with a nil repo for %q, got: %s", cmd, got)
		}
	}
}

// The status verdict wins when a push is also piped: the pipe reason names the
// nearer cause, and one ask at a time is the point.
func TestStatusVerdictWinsOverRepoState(t *testing.T) {
	reg := shippedRegistry(t)
	repo := fakeRepo{branchFiles: []string{"Makefile"}, baseGained: []string{"Makefile"}}
	got := Decide("git push -u origin HEAD 2>&1 | tail -3", false, reg, repo)
	if !strings.Contains(got, "exit status is the filter's") {
		t.Errorf("want the pipe reason, got: %s", got)
	}
}

// The shipped registry has to carry the repo-state settings, not just gates: a
// missing overlap_ignore would make both checks fire on every branch that
// touches the backlog, which is all of them. Reconciled against .gitattributes
// in both directions, so neither file can gain an entry alone.
func TestShippedRegistryCarriesRepoStateSettings(t *testing.T) {
	reg := shippedRegistry(t)
	if reg.baseRef != "origin/main" {
		t.Errorf("base_ref = %q, want origin/main", reg.baseRef)
	}
	owned := mergeDriverOwned(t)
	for _, p := range owned {
		if !reg.overlapIgnore[p] {
			t.Errorf("overlap_ignore is missing the merge-driver-owned %s", p)
		}
	}
	if len(reg.overlapIgnore) != len(owned) {
		t.Errorf("overlap_ignore has %d entries for %d merge-driver-owned paths %v",
			len(reg.overlapIgnore), len(owned), owned)
	}
}

// A base_ref that is absent or does not look like a ref must not reach `git
// diff` as an argument: empty means the working tree, and a leading dash is
// read as an option.
func TestBaseRefFallsBackUnlessItLooksLikeARef(t *testing.T) {
	for _, bad := range []string{"", "--upload-pack=x", "-x", "a b", "a;b", "$(x)"} {
		c, _ := Registry{Gates: []string{"^make([[:space:]]|$)"}, BaseRef: bad}.compile()
		if c.baseRef != defaultBaseRef {
			t.Errorf("base_ref %q = %q, want the %s fallback", bad, c.baseRef, defaultBaseRef)
		}
	}
	for _, ok := range []string{"origin/main", "upstream/release-1.2", "main"} {
		c, _ := Registry{Gates: []string{"^make([[:space:]]|$)"}, BaseRef: ok}.compile()
		if c.baseRef != ok {
			t.Errorf("base_ref %q = %q, want it kept", ok, c.baseRef)
		}
	}
}

// The `gh pr list --json number,headRefName,files` shape, captured from the
// real command on 2026-08-05. This is the half of the exec probe with logic in
// it: the rest is two `git diff --name-only` calls.
func TestParsePRList(t *testing.T) {
	raw := []byte(`[
	  {"number":1267,"headRefName":"dependabot/github_actions/actions-6982ec250a",
	   "files":[{"path":".github/workflows/dockerfile-lint.yml","additions":1,"deletions":1,"changeType":"MODIFIED"},
	            {"path":".github/workflows/publish.yml","additions":2,"deletions":2,"changeType":"MODIFIED"}]},
	  {"number":1299,"headRefName":"claude/mine","files":[{"path":"Makefile"}]}
	]`)

	prs, err := parsePRList(raw, "claude/mine")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The current branch's own PR is dropped: a re-run after one exists must
	// not report the branch overlapping itself.
	if len(prs) != 1 || prs[0].Number != 1267 {
		t.Fatalf("want only #1267, got %+v", prs)
	}
	if len(prs[0].Files) != 2 || prs[0].Files[0] != ".github/workflows/dockerfile-lint.yml" {
		t.Errorf("files not read from the path field: %+v", prs[0].Files)
	}

	if _, err := parsePRList([]byte("not json"), ""); err == nil {
		t.Error("want an error for unparseable output, so the caller stays silent")
	}
	// `gh` prints `[]` when nothing is open, which is not an error.
	prs, err = parsePRList([]byte("[]"), "")
	if err != nil || len(prs) != 0 {
		t.Errorf("empty list: got %+v, %v", prs, err)
	}
}

// Truncation keeps a reason readable when a rebase touched a large branch.
func TestJoinPathsTruncates(t *testing.T) {
	got := joinPaths([]string{"a", "b", "c", "d"}, 2)
	if got != "a, b and 2 more" {
		t.Errorf("joinPaths = %q", got)
	}
	if got := joinPaths([]string{"a", "b"}, 2); got != "a, b" {
		t.Errorf("joinPaths under the limit = %q", got)
	}
}
