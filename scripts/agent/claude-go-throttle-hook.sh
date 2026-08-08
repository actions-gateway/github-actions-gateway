#!/usr/bin/env bash
#
# claude-go-throttle-hook.sh — Claude Code PreToolUse hook that auto-throttles
# raw `go build` / `go test` commands run outside `make`, so a forgotten
# `$(scripts/agent/local-throttle.sh prefix)` no longer means an unthrottled run
# (Q92).
#
# This script is only the entry point. The decision is devtools/agent/gothrottle,
# a Go program over a real shell parser (mvdan.cc/sh); this file resolves the
# binary and the throttle script and gets out of the way. The three outcomes it
# emits — a bare invocation rewritten and allowed, a compound or redirected
# `-race` rewritten and asked, an unpinnable `-race` denied — live in that
# program's package comment, and in the assertions of
# claude-go-throttle-hook-test.sh.
#
# Why Go, measured 2026-08-06 under Q624: the shell version this replaces spent
# 178 of its 423 lines hand-rolling a shell-grammar scanner (quote state,
# heredoc bodies, command position), because regular expressions cannot count
# brackets. That is the parsing-density criterion in technical-debt.md § A shell
# gate becomes a Go devtool on parsing density, not length, and the sibling hook
# was ported for the same reason (Q625). It failed the way that section
# predicts, silently and in both directions: a heredoc body naming `go test
# -race` read as an invocation until Q624, and a `-race` passed to a wrapper
# still read as none until Q696.
#
# The build seam is the sibling's, for the same reasons: the binary is cached in
# .build/ and staleness decided by one `find` (a cold `go build` costs ~1.6 s and
# an up-to-date one still ~226 ms); concurrent sessions each fire this on every
# Bash call, so the build writes a PID-suffixed file and renames it into place,
# which is atomic within a filesystem; and every failure — no Go toolchain, a
# build error, a missing throttle script — exits 0 with no output, which Claude
# Code reads as "no opinion".
#
# Wired up by .claude/settings.json as a PreToolUse hook on the Bash matcher.
set -euo pipefail
# Fail open on bash 3.2 (stock macOS), where inherit_errexit does not exist:
# a hook must never block a tool call, which outranks the errexit coverage.
shopt -s inherit_errexit 2>/dev/null || true

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="$REPO_ROOT/.build/gothrottle"
SRC="$REPO_ROOT/devtools/agent/gothrottle"
THROTTLE="$REPO_ROOT/scripts/agent/local-throttle.sh"

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
	if ! (cd "$REPO_ROOT/devtools" && GOWORK=off go build -o "$staged" ./agent/gothrottle) >/dev/null 2>&1; then
		rm -f "$staged"
		return 1
	fi
	mv -f "$staged" "$BIN" || {
		rm -f "$staged"
		return 1
	}
}

main() {
	[[ -x "$THROTTLE" ]] || exit 0
	ensure_binary || exit 0
	exec "$BIN" "$THROTTLE"
}

main "$@"
