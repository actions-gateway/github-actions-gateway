#!/usr/bin/env bash
#
# claude-go-throttle-hook.sh — Claude Code PreToolUse hook that auto-throttles
# raw `go build` / `go test` commands run outside `make`.
#
# Why this exists (Q92): the Makefile auto-throttles its own recipes
# (background-QoS prefix + I/O throttle + parallelism cap via
# scripts/local-throttle.sh), but a bare `go build` / `go test` that Claude or
# the user runs *directly* through the Bash tool gets none of that — full
# priority, uncapped, no I/O throttle. On a small Mac a heavy run (especially
# `-race`, a ~5–10× CPU/memory/I/O amplifier), *especially alongside other
# concurrent sessions*, can saturate the machine and trip the WindowServer
# watchdog: the GUI freezes/restarts. This was observed for real — an
# unthrottled `go test -race` in a parallel worktree session crashed
# WindowServer during the session that filed Q92.
#
# This hook automates the manual workaround documented in CLAUDE.md
# (`$(scripts/local-throttle.sh prefix) go test ...`): it transparently prepends
# the same platform QoS prefix that `make` uses, so a forgotten prefix no longer
# means an unthrottled run.
#
# Behaviour (fail-open everywhere — a parse/tool error never blocks a command):
#   * Non-Bash tool, or not a `go build`/`go test` command  -> allow unchanged.
#   * Throttle inactive (CI / headless / SSH / non-GUI, per local-throttle.sh)
#     or already throttled (command already carries the prefix or calls
#     local-throttle.sh)                                      -> allow unchanged.
#   * Simple `go build`/`go test` (no shell chaining)         -> rewrite the
#     command to prepend the QoS prefix and auto-allow it. Auto-allow is safe:
#     the command is strictly a bare `go build`/`go test`, the same boundary the
#     repo's `Bash(go build *)` / `Bash(go test *)` allowlist already trusts. It
#     is also necessary — without it the rewritten `taskpolicy … go test …` form
#     would no longer match that allowlist and would trigger a *new* prompt.
#   * A compound command (contains `&&`, `|`, `;`, `$()`, …) or one with a
#     redirect (`>`, `<`) carrying `-race` (the dangerous amplifier) -> rewrite
#     it to insert the QoS prefix before its single `go build`/`go test` token
#     and return an `ask` decision (not `allow`). `ask` still throttles the run
#     while keeping the permission system, the user, and the security-guard hooks
#     (branch-guard, workspace-guard) in the loop — an `allow` would ride the
#     whole command (its other segments, an outside-workspace redirect) past
#     them, which is why the bare form uses `allow` but this one must not.
#   * The same compound/redirected `-race` case when the hook cannot identify a
#     single `go build`/`go test` token to prefix (more than one invocation, or a
#     form it cannot parse) -> `deny` with the specific reason. We deny rather
#     than emit a command that throttles the wrong token — or none — because a
#     silently mis-thrown prefix would leave the real `-race` run unthrottled.
#   * Any other such `go build`/`go test` (no `-race`)         -> allow unchanged.
#
# Why redirects/chaining disqualify the *auto-allow* path (but not throttling):
# an outside-workspace redirect target (`go test … > /tmp/out`) is exactly what
# the workspace-guard hook prompts on, and a chained segment could be any guarded
# op. Claude Code does not document how a PreToolUse `allow` composes with another
# hook's `ask`/`deny`, so — secure by default — we never emit `allow` for a
# command a guard might care about. The auto-*allowed* class stays a bare
# `go build`/`go test`: no guarded file command, no git op, no redirect. But the
# throttle itself must still reach the dangerous `-race` forms, so for those we
# rewrite and emit `ask` — which, unlike `allow`, leaves the command in the
# permission flow for the user and the guard hooks to act on however Claude Code
# composes them — instead of blocking the caller outright.
#
# Wired up by .claude/settings.json as a PreToolUse hook on the Bash matcher.
# Requires jq; if jq is missing the hook is a no-op (fail-open).
set -euo pipefail

# emit_allow_unchanged exits 0 with no output, which lets the original command
# proceed through Claude Code's normal permission flow untouched.
emit_allow_unchanged() {
	exit 0
}

# is_heavy_go_command returns success when the command contains a `go build` or
# `go test` invocation. The leading boundary ((^|non-word)) keeps `cargo test`,
# `mongo build`, `django test` and similar from matching the trailing `go`.
is_heavy_go_command() {
	local cmd="$1"
	[[ "$cmd" =~ (^|[^[:alnum:]_-])go[[:space:]]+(build|test)([[:space:]]|$) ]]
}

# already_throttled returns success when the command already carries a throttle
# prefix (taskpolicy / nice) or computes one via local-throttle.sh — i.e. the
# documented manual workaround, or a previous wrap, is already in place.
already_throttled() {
	local cmd="$1"
	case "$cmd" in
	*local-throttle.sh* | *"taskpolicy "* | *"nice -n"*) return 0 ;;
	*) return 1 ;;
	esac
}

# is_compound returns success when the command contains shell control operators
# that introduce additional commands (chaining, pipes, command substitution,
# backgrounding, newlines). Such a command cannot be auto-allowed: its other
# segments would ride past the permission system on our `allow`.
is_compound() {
	local cmd="$1"
	# Single quotes are intentional: these are literal substrings to match
	# (`$(` is command substitution), not expressions to expand.
	# shellcheck disable=SC2016
	case "$cmd" in
	*'|'* | *'&'* | *';'* | *'$('* | *'`'* | *$'\n'*) return 0 ;;
	*) return 1 ;;
	esac
}

# has_redirect returns success when the command contains a redirection (`>`,
# `>>`, `<`). Any redirect disqualifies the transparent auto-allow path: a
# redirect to an outside-workspace path is exactly what workspace-guard prompts
# on, and we must never auto-allow past that guard. fd-dups like `2>&1` are
# swept up too — harmless to exclude (they fall back to the normal flow), and
# not worth the fragile parsing it would take to tell them from file redirects.
has_redirect() {
	local cmd="$1"
	case "$cmd" in
	*'>'* | *'<'*) return 0 ;;
	*) return 1 ;;
	esac
}

# rewrite_simple prints the rewritten command for a simple `go build`/`go test`
# invocation: it inserts the QoS prefix immediately before the `go` token,
# preserving any leading `VAR=val` environment assignments so they still apply.
# Prints nothing and returns non-zero if the head is not a bare `go build`/`test`
# (e.g. an absolute path to go, or gofmt) — the caller then allows it unchanged.
rewrite_simple() {
	local cmd="$1" prefix="$2"
	local env_prefix="" rest="$cmd"

	# Peel off leading `NAME=value ` environment assignments.
	while [[ "$rest" =~ ^([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+) ]]; do
		env_prefix+="${BASH_REMATCH[1]}"
		rest="${rest#"${BASH_REMATCH[1]}"}"
	done

	# What remains must start with a bare `go build`/`go test`.
	[[ "$rest" =~ ^go[[:space:]]+(build|test)([[:space:]]|$) ]] || return 1

	printf '%s%s %s' "$env_prefix" "$prefix" "$rest"
}

# rewrite_compound prints a compound or redirected command with the QoS prefix
# inserted immediately before its single `go build`/`go test` invocation,
# preserving everything around it (a subshell wrapper, a leading `cd`, redirects,
# `VAR=val` assignments). It rewrites ONLY a form it can pin down unambiguously:
# exactly one `go build`/`go test` token. When the command holds more than one
# such invocation — or a shape this simple parse cannot place the prefix in — it
# prints nothing and returns non-zero, and the caller denies with a specific
# reason rather than throttle the wrong token (or none) and leave `-race`
# running at full tilt.
rewrite_compound() {
	local cmd="$1" prefix="$2"
	# Group 1 (greedy) captures everything up to and including the boundary char
	# before the LAST `go build`/`go test`; it is optional so a redirect-only
	# command whose `go` sits at position 0 (`go test -race … > out`) still
	# matches. Group 2 is that invocation through end of string.
	local re='^(.*[^[:alnum:]_-])?(go[[:space:]]+(build|test)([[:space:]].*)?)$'
	[[ "$cmd" =~ $re ]] || return 1
	local before="${BASH_REMATCH[1]}" invocation="${BASH_REMATCH[2]}"
	# A second `go build`/`go test` in the pre-match text means we cannot know
	# which invocation to throttle — bail so the caller denies.
	is_heavy_go_command "$before" && return 1
	printf '%s%s %s' "$before" "$prefix" "$invocation"
}

main() {
	# jq is required to parse the hook payload and emit a rewrite safely.
	command -v jq >/dev/null 2>&1 || emit_allow_unchanged

	local input
	input="$(cat)"
	[[ -n "$input" ]] || emit_allow_unchanged

	local tool_name command
	tool_name="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
	command="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null || true)"
	[[ "$tool_name" == "Bash" ]] || emit_allow_unchanged
	[[ -n "$command" ]] || emit_allow_unchanged

	# Not a heavy go command, or it is already throttled: leave it alone.
	is_heavy_go_command "$command" || emit_allow_unchanged
	if already_throttled "$command"; then
		emit_allow_unchanged
	fi

	# Resolve the platform throttle prefix from the sibling script. An empty
	# prefix means throttling is off (CI / headless / SSH / unsupported OS), so
	# there is nothing to do.
	local script_dir throttle prefix
	script_dir="$(cd "$(dirname "$0")" && pwd)"
	throttle="$script_dir/local-throttle.sh"
	[[ -x "$throttle" ]] || emit_allow_unchanged
	prefix="$("$throttle" prefix 2>/dev/null || true)"
	[[ -n "$prefix" ]] || emit_allow_unchanged

	if is_compound "$command" || has_redirect "$command"; then
		# We can only safely auto-*allow* a bare, single go build/test. A compound
		# command (its other segments would ride past the permission system) or one
		# with a redirect (an outside-workspace target is what workspace-guard
		# prompts on) must not be auto-allowed. For the genuinely dangerous case —
		# one carrying `-race` — we still MUST throttle it, so instead of blocking
		# we rewrite it and return `ask`: the throttle is applied and the prompt
		# keeps the user and the guard hooks in the loop. A non-`-race` form stays
		# on the normal permission flow (and the guards) unchanged.
		if [[ "$command" == *-race* ]]; then
			local racecmd
			if racecmd="$(rewrite_compound "$command" "$prefix")"; then
				jq -cn --arg cmd "$racecmd" '{
					hookSpecificOutput: {
						hookEventName: "PreToolUse",
						permissionDecision: "ask",
						permissionDecisionReason: "Auto-throttled a heavy `go ... -race` (I/O and CPU demoted below the desktop) — confirm to run the throttled form. An unthrottled -race run can saturate the machine and freeze the local GUI; see CLAUDE.md.",
						updatedInput: { command: $cmd }
					}
				}'
				return 0
			fi
			# Could not pin down a single go build/test token to prefix (more than
			# one invocation, or a shape the parse can't place the prefix in). Deny
			# with the specific reason rather than emit a command that throttles the
			# wrong token — or none — and leaves the real `-race` run unthrottled.
			jq -cn '{
				hookSpecificOutput: {
					hookEventName: "PreToolUse",
					permissionDecision: "deny",
					permissionDecisionReason: "Blocked: this `go ... -race` has more than one go build/test invocation (or a shape the throttle hook cannot parse), so the hook cannot insert the throttle prefix unambiguously. Give each `go ... -race` its own throttle prefix ($(scripts/local-throttle.sh prefix)), run each go line on its own, or use the matching `make` target (it throttles itself). See CLAUDE.md."
				}
			}'
			return 0
		fi
		emit_allow_unchanged
	fi

	# Simple command: prepend the prefix and auto-allow the throttled form.
	local newcmd
	newcmd="$(rewrite_simple "$command" "$prefix")" || emit_allow_unchanged

	jq -cn --arg cmd "$newcmd" '{
		hookSpecificOutput: {
			hookEventName: "PreToolUse",
			permissionDecision: "allow",
			permissionDecisionReason: "Auto-throttled heavy go build/test (I/O and CPU demoted below the desktop) to keep the local GUI responsive — see CLAUDE.md.",
			updatedInput: { command: $cmd }
		}
	}'
}

main "$@"
