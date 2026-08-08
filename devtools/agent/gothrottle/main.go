// Command gothrottle decides whether a Claude Code PreToolUse Bash payload
// carries a raw `go build`/`go test` that would run outside `make`, and rewrites
// it to prepend the platform throttle prefix.
//
// # Why (Q92)
//
// The Makefile auto-throttles its own recipes (background-QoS prefix, I/O
// throttle, parallelism cap, via scripts/agent/local-throttle.sh), but a bare
// `go build`/`go test` run directly through the Bash tool gets none of that:
// full priority, uncapped, no I/O throttle. On a small Mac a heavy run —
// `-race` above all, a ~5-10x CPU/memory/I/O amplifier — especially alongside
// concurrent sessions, can saturate the machine and trip the WindowServer
// watchdog, freezing or restarting the GUI. That happened for real: an
// unthrottled `go test -race` in a parallel worktree session crashed
// WindowServer during the session that filed Q92.
//
// # The three outcomes
//
//   - A bare `go build`/`go test` is rewritten and auto-allowed. Auto-allow is
//     safe because the shape is strictly a bare invocation, the same boundary
//     the repo's `Bash(go build *)` / `Bash(go test *)` allowlist already
//     trusts; it is also necessary, since the rewritten `taskpolicy … go test …`
//     form would otherwise stop matching that allowlist and raise a new prompt.
//   - A compound or redirected form carrying `-race` is rewritten and asked.
//     An allow would carry the command's other segments — a chained git op, a
//     redirect to an outside-workspace path — past branch-guard and
//     workspace-guard, and Claude Code does not document how a PreToolUse allow
//     composes with another hook's ask or deny. Secure by default: never allow a
//     command a guard might care about. But the throttle must still reach the
//     dangerous form, so it is applied and the decision downgraded to ask rather
//     than the command blocked.
//   - A `-race` form with more than one invocation to throttle and one prefix to
//     place is denied with that reason, rather than throttling one invocation
//     and leaving the other at full tilt.
//
// A non-race compound stays on the normal permission flow untouched.
//
// # Why a parser and not a scanner (Q708)
//
// The question is whether a `go` sits at command position, which means tracking
// quoting, heredoc bodies, subshell nesting, and command substitution. The shell
// version this replaces hand-rolled a character scanner for it: 178 of its 423
// lines, measured 2026-08-06. That is the parsing-density criterion in
// technical-debt.md § A shell gate becomes a Go devtool on parsing density, not
// length, and it is the same argument that ported the sibling hook (Q625).
//
// Against a real parse tree the question is a node type. Quoted text and heredoc
// bodies stop being a special case, because a command named inside a string is a
// Lit and never a call — so a `git commit` message quoting `go test -race` is
// silent by construction rather than by a rule that has to be maintained.
//
// The scanner also failed in the other direction, which is what Q696 records: a
// `-race` run passed to a wrapper (`timeout 900 go test -race ./...`) put `go`
// in argument position, so the scan reported nothing and the run escaped the
// throttle. The parser does not fix that on its own — it is a real argument
// list either way — but it makes the fix a small allowlist over words rather
// than more scanner state. See wrappers.
//
// Usage:
//
//	gothrottle <local-throttle.sh>   # PreToolUse payload on stdin, decision on stdout
//
// Silence means "proceed": Claude Code reads an empty stdout as no opinion.
// Every failure path is silent — a missing throttle script, an unparseable
// command, a payload for another tool. A hook that runs on every Bash call must
// never be the reason one fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type payload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

type updatedInput struct {
	Command string `json:"command"`
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string        `json:"hookEventName"`
		PermissionDecision       string        `json:"permissionDecision"`
		PermissionDecisionReason string        `json:"permissionDecisionReason"`
		UpdatedInput             *updatedInput `json:"updatedInput,omitempty"`
	} `json:"hookSpecificOutput"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gothrottle <local-throttle.sh>")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Stdin, os.Stdout))
}

func run(throttlePath string, stdin io.Reader, stdout io.Writer) int {
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

	d := Decide(p.ToolInput.Command, func() string { return throttlePrefix(throttlePath) })
	if d == nil {
		return 0
	}

	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = d.Permission
	out.HookSpecificOutput.PermissionDecisionReason = d.Reason
	if d.Command != "" {
		out.HookSpecificOutput.UpdatedInput = &updatedInput{Command: d.Command}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 0
	}
	return 0
}

// throttleTimeout bounds the one subprocess this tool runs. The hook holds up
// the Bash call it fires on, so a wedged probe has to cost a bounded pause and
// then silence.
const throttleTimeout = 5 * time.Second

// throttlePrefix asks local-throttle.sh for the platform prefix. An empty
// string — no such script, a non-zero exit, an unsupported OS, throttling off
// in CI or over SSH — means there is nothing to apply.
func throttlePrefix(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), throttleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "prefix") //nolint:gosec // G204: the path is this hook's own sibling script, passed by the entry point; no shell is involved
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
