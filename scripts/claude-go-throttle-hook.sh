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
#   * Compound command (contains `&&`, `|`, `;`, `$()`, …) carrying `-race`
#     (the dangerous amplifier) -> block (exit 2) and tell the caller to use the
#     documented compound-form prefix. We block rather than silently rewrite
#     because auto-allowing an arbitrary compound command would bypass the
#     permission system for its non-go segments.
#   * Any other compound `go build`/`go test` (no `-race`)    -> allow unchanged.
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
# backgrounding, newlines). Simple file redirections (>, <) are NOT treated as
# compound — they do not introduce a new command. A simple command can be safely
# auto-allowed after the prefix is prepended; a compound one cannot.
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

	if is_compound "$command"; then
		# A compound command cannot be safely auto-allowed (its other segments
		# would bypass the permission system). Block only the genuinely
		# dangerous case — a compound carrying `-race` — and steer the caller to
		# the documented compound-form prefix. Other compounds run unchanged.
		if [[ "$command" == *-race* ]]; then
			cat >&2 <<-'EOF'
				Blocked: a heavy `go ... -race` inside a compound command bypasses the
				auto-throttle and can saturate the machine — an unthrottled `-race` run
				has frozen the macOS GUI (WindowServer watchdog restart).

				Re-run with the throttle prefix, e.g. from a module subdir:

				  TP="$(git rev-parse --show-toplevel)/scripts/local-throttle.sh"; (cd cmd/agc && $("$TP" prefix) go test -race ./...)

				or prefer the matching `make` target, which throttles itself. See CLAUDE.md
				("Throttle raw go build/test run outside make").
			EOF
			exit 2
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
			permissionDecisionReason: "Auto-throttled heavy go build/test (utility QoS) to keep the local GUI responsive — see CLAUDE.md.",
			updatedInput: { command: $cmd }
		}
	}'
}

main "$@"
