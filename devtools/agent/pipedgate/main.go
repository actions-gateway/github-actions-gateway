// Command pipedgate decides whether a Claude Code PreToolUse Bash payload is
// about to make an unchecked claim, and prints the `deny` decision when it is.
// It answers two families of question.
//
// # A gate whose exit status never reaches the caller
//
// Two routes reach the same false green:
//
//   - A pipe (Q625). A pipeline's status is its LAST stage's, so
//     `make check 2>&1 | tail -30; echo "EXIT=$?"` prints EXIT=0 for a failing
//     gate. zsh, the shell the Bash tool runs, has no PIPESTATUS to recover it.
//   - A backgrounded call (Q681). A `;`-list also yields its last statement's
//     status, so `make check > log 2>&1; echo "EXIT=$?"` run with
//     run_in_background — or ended with `&` — notifies exit code 0 for a failing
//     gate. Measured 2026-08-04: a backgrounded `false; echo "EXIT=$?"` logged
//     EXIT=1 and notified exit code 0. The same command in the FOREGROUND is
//     correct and documented, because the echo lands where it can be read; only
//     the backgrounded shape loses it, and only there does this warn.
//
// Why a parser and not regular expressions: the question is whether a gate sits
// at command position on the LEFT of a pipe, which means tracking quoting,
// heredoc bodies, subshell nesting, and command substitution. Regular
// expressions cannot count brackets, so the shell version this replaces
// hand-rolled a character scanner for it (175 of its 257 code lines). Against a
// real parse tree the question is a node type: a BinaryCmd whose Op is Pipe.
// Quoted text and heredoc bodies stop being a special case, because a command
// named inside a string is a Lit, never a command.
//
// The decision is `deny`, with `PIPED_GATE_OVERRIDE=<reason>` as the break-glass
// (Q697). Claude Code shows a deny's reason to the model and an ask's to the
// user (measured against the hooks reference, 2026-08-07), and the reason here
// is addressed to whoever is about to rewrite the command. Under `ask` a right
// catch cost a deny-and-paste — the user had to relay the fix by hand — while a
// wrong one had no break-glass at all, because continuing was the only
// alternative to the prompt. The prefix restores that escape, and asks for a
// reason rather than a keystroke: piping a gate into a filter is sometimes
// exactly right, and nothing in the command string distinguishes that from the
// bug.
//
// What it detects — a registered gate (see Registry) on the LEFT of a pipe,
// including through a subshell or brace group whose status the pipe consumes,
// inside a command substitution, and in any stage of a longer pipeline. Also a
// $PIPESTATUS read anywhere, which is a bug on its own: that name is bash's, and
// in zsh it expands to empty, so every test against it reads as success. And a
// registered gate in a backgrounded call whose last statement cannot carry the
// gate's failure out — an `echo`, a `||` fallback, a `&` fork.
//
// What it deliberately does not detect:
//
//   - A command bash cannot parse. zsh-only syntax reaches this tool as a parse
//     error and gets silence, because a half-parse is a guess.
//   - A gate reached through a variable or eval ($CMD | tail), or one piped
//     inside a script this call merely invokes. The input is one command
//     string, not a program.
//   - A gate behind a throttle wrapper (taskpolicy ... go test ... | tail): the
//     wrapper's own flags are not peeled, so the head does not match.
//   - A capability probe — an invocation carrying --version, --help, -V or -h
//     (Q730). The tool prints and exits, so no gate result exists to lose.
//     `-v` is not one of them: it is --version to make but verbose to
//     `go test`, and `go test -v ./... | tail` is the bug.
//   - A pipeline whose status genuinely does not matter, which is what the
//     override prefix is for.
//   - Any command carrying a heavy `go ... -race`, which
//     claude-go-throttle-hook.sh rewrites; see defersToThrottleHook.
//
// Mitigations that suppress the pipe verdict: `set -o pipefail` in the same
// command, zsh's $pipestatus, and redirecting to a file instead of piping.
// Neither pipefail nor $pipestatus mitigates a lost background status, so they
// do not suppress that one; ending the command in `exit $rc` — or in the gate
// itself — does. A `PIPED_GATE_OVERRIDE=<reason>` assignment suppresses every
// verdict, including the repo-state ones, and is the only thing that does.
//
// # A command whose correctness depends on repository state
//
// Two moments where the rule is written down and the slip happens anyway,
// because it happens in flow rather than while reading CONTRIBUTING.md:
//
//   - `git push` on a base that moved into the branch's own files (Q665). A
//     stale base is benign — the merge queue validates the candidate merge —
//     so this warns only when what `main` gained overlaps what the branch
//     changes, which is where a queue kickback costs a full check cycle a local
//     re-run would have caught. Measured 2026-08-05: `main` takes ~47 merges a
//     day, so the bare "the base moved" signal is non-empty at nearly every
//     push and a warning on it would be accepted reflexively.
//   - `gh pr create` opening a PR whose files an open PR already changes
//     (Q668). Duplicated or mutually-invalidating work, which a PR title does
//     not reveal.
//
// Both discount the merge-driver-owned files (overlap_ignore in the registry):
// nearly every PR edits docs/STATUS.md, and most conflicts there are resolved by
// row ID rather than reviewed. Most, not all — the driver refuses on a row
// deleted on one side and edited on the other — so the push check holds the
// discount only while the merge really resolves, and asks `git merge-tree` when
// one of those paths is in both change sets (Q790). The create check cannot: the
// other PR's head is not a local ref and a hook must not fetch, and duplicated
// work rather than a conflict is what it is looking for.
//
// These are the only checks that cost a subprocess, and they run only after the
// parse finds their trigger at command position — two `git diff`s for the push
// plus one `git merge-tree` when a discounted path is in both change sets, one
// `gh pr list` round trip for the create. Both probes fail silent on any
// error, which is what offline, an expired or rate-limited gh token, a shallow
// clone, and a missing origin/main all reduce to; `git` is bounded at 3 s and
// `gh` at 5 s so a dead network costs a pause rather than a stall. The base is
// read from the LOCAL origin/main ref — the hook does not fetch, because a
// PreToolUse hook must not mutate refs behind the session — so a session that
// has not fetched recently gets an under-report, never a false one.
//
// Quoting and heredocs need no special case. A gate named inside a string or a
// heredoc body parses as a literal, never a command, so a commit message
// quoting `make check | tail` is silent by construction. The one asymmetry is
// deliberate: an UNQUOTED heredoc really does expand $PIPESTATUS, so that reads
// as the bug it is, while a <<'EOF' body does not.
//
// Usage:
//
//	pipedgate <registry.json>    # PreToolUse payload on stdin, decision on stdout
//
// Silence means "proceed": Claude Code reads an empty stdout as no opinion.
// Every failure path is silent — a missing registry, an unparseable command, a
// payload for another tool. A hook that runs on every Bash call must never be
// the reason one fails.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type payload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
		// RunInBackground is absent on a foreground call, so the zero value is
		// the answer there.
		RunInBackground bool `json:"run_in_background"`
	} `json:"tool_input"`
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: pipedgate <registry.json>")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Stdin, os.Stdout))
}

func run(registryPath string, stdin io.Reader, stdout io.Writer) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return 0
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}
	if p.ToolName != "Bash" || p.ToolInput.Command == "" {
		return 0
	}

	regRaw, err := os.ReadFile(registryPath)
	if err != nil {
		return 0
	}
	var reg Registry
	if err := json.Unmarshal(regRaw, &reg); err != nil {
		return 0
	}
	c, _ := reg.compile()
	if len(c.gates) == 0 {
		return 0
	}

	// The registry sits at <root>/.claude/, and the probes must run against the
	// worktree the hook was installed in rather than wherever the tool was
	// exec'd from.
	root, err := filepath.Abs(filepath.Dir(filepath.Dir(registryPath)))
	if err != nil {
		return 0
	}

	reason := Decide(p.ToolInput.Command, p.ToolInput.RunInBackground, c, execRepo{dir: root, baseRef: c.baseRef})
	if reason == "" {
		return 0
	}

	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = reason
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 0
	}
	return 0
}
