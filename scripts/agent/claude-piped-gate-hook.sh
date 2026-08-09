#!/usr/bin/env bash
#
# claude-piped-gate-hook.sh — Claude Code PreToolUse hook that denies when a
# command whose exit status IS the answer is piped into a filter, so the Bash
# call reports the filter's status instead of the gate's (Q625). A deny's reason
# is shown to the model rather than to the user, so the fix arrives where the
# rewrite happens; `PIPED_GATE_OVERRIDE=<reason> <command>` is the break-glass
# (Q697).
#
# `make check 2>&1 | tail -30; echo "EXIT=$?"` prints EXIT=0 for a failing gate:
# a pipeline's status is its last stage's, and zsh — the shell the Bash tool
# runs — has no PIPESTATUS to recover it. The rule is written in CLAUDE.md and
# in testing.md § The status you report is a claim too, and sessions still trip
# it, most recently a `git pull --ff-only 2>&1 | tail -5` that reported success
# for a pull that had failed.
#
# This script is only the entry point. The decision is
# devtools/agent/pipedgate, a Go program over a real shell parser
# (mvdan.cc/sh); this file resolves the binary and the registry and gets out of
# the way. What is and is not detected lives in that program's package comment,
# and in the assertions of its table test.
#
# Why Go, measured 2026-08-04 — the shell version this replaces spent 175 of its
# 257 code lines hand-rolling a shell-grammar scanner (quote state, heredoc
# bodies, nesting, matched delimiters), because regular expressions cannot count
# brackets. That is the parsing-density criterion in technical-debt.md
# § A shell gate becomes a Go devtool on parsing density, not length, and it
# failed the way that section predicts: silently, in both directions. Latency
# agreed rather than conflicted, though less dramatically than the binary alone
# suggests: the shell hook cost 33 ms per Bash call (17 ms of it bash startup
# and two jq spawns); this entry point plus the binary costs 18 ms, of which the
# decision itself is 4 ms and the rest is this script's own startup and staleness
# `find`. The remaining 14 ms is the price of building on demand from a fresh
# clone.
#
# What the build seam has to survive, and why it is shaped this way:
#   * A cold `go build` costs ~1.6 s and an up-to-date one still costs ~226 ms,
#     so the binary is cached in .build/ and staleness is decided by one `find`
#     against the sources rather than by asking `go` (~3 ms).
#   * Concurrent sessions each fire this hook on every Bash call, so the build
#     writes a PID-suffixed file and renames it into place. `mv` within a
#     filesystem is atomic, so a racing session execs a whole binary or an older
#     whole binary — never a half-written one.
#   * Every failure — no Go toolchain, a build error, a missing registry — exits
#     0 with no output, which Claude Code reads as "no opinion". A hook on every
#     Bash call must never be the reason one fails.
set -euo pipefail
# Fail open on bash 3.2 (stock macOS), where inherit_errexit does not exist:
# a hook must never block a tool call, which outranks the errexit coverage.
shopt -s inherit_errexit 2>/dev/null || true

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="$REPO_ROOT/.build/pipedgate"
SRC="$REPO_ROOT/devtools/agent/pipedgate"
REGISTRY="$REPO_ROOT/.claude/piped-gate-guard.json"

# ensure_binary makes $BIN current, or fails so the caller can stay silent.
ensure_binary() {
	if [[ -x "$BIN" ]]; then
		local newer
		newer="$(find "$SRC" -name '*.go' -newer "$BIN" -print -quit 2>/dev/null || true)"
		[[ -z "$newer" ]] && return 0
	fi
	command -v go >/dev/null 2>&1 || return 1
	mkdir -p "$REPO_ROOT/.build" || return 1
	local staged="$BIN.$$"
	if ! (cd "$REPO_ROOT/devtools" && GOWORK=off go build -o "$staged" ./agent/pipedgate) >/dev/null 2>&1; then
		rm -f "$staged"
		return 1
	fi
	mv -f "$staged" "$BIN" || {
		rm -f "$staged"
		return 1
	}
}

main() {
	[[ -r "$REGISTRY" ]] || exit 0
	ensure_binary || exit 0
	exec "$BIN" "$REGISTRY"
}

main "$@"
