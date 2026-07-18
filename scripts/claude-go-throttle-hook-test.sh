#!/usr/bin/env bash
#
# Unit tests for scripts/claude-go-throttle-hook.sh — the PreToolUse hook that
# auto-throttles raw `go build`/`go test` commands (Q92, Q347).
#
# The load-bearing property is that a heavy `-race` run never escapes the
# throttle: a bare form is rewritten and auto-allowed, a compound/redirected
# `-race` form is rewritten and returned as an `ask` (Q347 — no longer blocked),
# and a `-race` form the hook cannot pin a single go token in is denied with a
# specific reason. These are asserted against synthetic hook payloads. Runs under
# `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
HOOK="$REPO_ROOT/scripts/claude-go-throttle-hook.sh"
THROTTLE="$REPO_ROOT/scripts/local-throttle.sh"

fails=0

# The hook is a no-op when throttling is inactive (CI / headless / SSH), which is
# exactly the environment this test runs in. Force the throttle-active path so
# the hook computes a real prefix: clear CI and present a graphical session. Both
# the hook and this test resolve the prefix from the same script under the same
# env, so the expected rewrite is derived, never hardcoded per-OS.
throttle_env() {
	env -u CI DISPLAY="${DISPLAY:-:0}" "$@"
}

PREFIX="$(throttle_env "$THROTTLE" prefix || true)"
if [[ -z "$PREFIX" ]]; then
	# No throttle prefix on this platform (e.g. an unsupported OS) — there is no
	# rewrite to assert. Skip loudly rather than pass a hollow suite.
	printf 'SKIP claude-go-throttle-hook-test: no throttle prefix on this platform\n'
	exit 0
fi

# run_hook CMD — feed CMD to the hook as a Bash PreToolUse payload, print stdout.
run_hook() {
	local cmd="$1"
	jq -cn --arg c "$cmd" '{tool_name: "Bash", tool_input: {command: $c}}' \
		| throttle_env "$HOOK"
}

# field OUT EXPR — read a jq path from the hook output (empty if absent/invalid).
field() {
	local out="$1" expr="$2"
	printf '%s' "$out" | jq -r "$expr // empty" 2>/dev/null || true
}

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# expect_unchanged NAME CMD — the hook must allow the command untouched (no
# output at all: an exit-0 with an empty stdout is Claude Code's "proceed").
expect_unchanged() {
	local name="$1" cmd="$2" out
	out="$(run_hook "$cmd")"
	if [[ -z "$out" ]]; then pass "$name"; else
		fail "$name: expected no output, got: $out"
	fi
}

# expect_decision NAME CMD WANT_DECISION WANT_CMD — assert the hook returns
# WANT_DECISION and, when WANT_CMD is non-empty, that updatedInput.command equals
# it exactly.
expect_decision() {
	local name="$1" cmd="$2" want_decision="$3" want_cmd="$4" out got_decision got_cmd
	out="$(run_hook "$cmd")"
	got_decision="$(field "$out" '.hookSpecificOutput.permissionDecision')"
	got_cmd="$(field "$out" '.hookSpecificOutput.updatedInput.command')"
	if [[ "$got_decision" != "$want_decision" ]]; then
		fail "$name: want decision=$want_decision got=${got_decision:-<none>}"
		return
	fi
	if [[ -n "$want_cmd" && "$got_cmd" != "$want_cmd" ]]; then
		fail "$name: want cmd=[$want_cmd] got=[$got_cmd]"
		return
	fi
	pass "$name"
}

# --- Bare form still auto-allows the throttled rewrite (regression) -----------

expect_decision 'bare: go test          -> allow + prefix' \
	'go test ./...' allow "$PREFIX go test ./..."
expect_decision 'bare: env + go test    -> allow, env kept' \
	'GOFLAGS=-mod=mod go test -race ./...' allow "GOFLAGS=-mod=mod $PREFIX go test -race ./..."

# --- Q347: compound / redirected -race is rewritten and asked, not blocked ----

expect_decision 'compound: (cd && -race) -> ask + prefix before go' \
	'(cd cmd/agc && go test -race ./...)' ask "(cd cmd/agc && $PREFIX go test -race ./...)"
expect_decision 'redirect: -race > out   -> ask + prefix before go' \
	'go test -race ./... > out.log 2>&1' ask "$PREFIX go test -race ./... > out.log 2>&1"
expect_decision 'pipe: -race | tee       -> ask + prefix before go' \
	'go test -race ./... | tee out.log' ask "$PREFIX go test -race ./... | tee out.log"

# Safety invariant: the rewritten command carries BOTH the throttle prefix and
# `-race`, and the prefix sits immediately before the throttled `go test`.
race_out="$(run_hook '(cd cmd/agc && go test -race ./...)')"
race_cmd="$(field "$race_out" '.hookSpecificOutput.updatedInput.command')"
if [[ "$race_cmd" == *"$PREFIX go test -race"* ]]; then
	pass 'safety: -race is throttled (prefix precedes go test -race)'
else
	fail "safety: -race not throttled by rewrite: [$race_cmd]"
fi

# --- Q347: a -race form we cannot pin a single go token in is denied ----------

deny_out="$(run_hook 'go build ./... && go test -race ./...')"
deny_decision="$(field "$deny_out" '.hookSpecificOutput.permissionDecision')"
deny_reason="$(field "$deny_out" '.hookSpecificOutput.permissionDecisionReason')"
if [[ "$deny_decision" == "deny" && "$deny_reason" == *"more than one go build/test"* ]]; then
	pass 'unrewritable: two go invocations -> deny with specific reason'
else
	fail "unrewritable: want deny w/ specific reason, got decision=$deny_decision reason=$deny_reason"
fi

# --- Cases the hook must leave untouched --------------------------------------

# A non-`-race` compound stays on the normal permission flow (unchanged).
expect_unchanged 'compound: no -race        -> unchanged' \
	'(cd cmd/agc && go test ./...)'
# An already-throttled compound short-circuits before the compound branch.
expect_unchanged 'compound: already throttled -> unchanged' \
	'(cd cmd/agc && taskpolicy -c utility go test -race ./...)'
# A non-go command is not our concern.
expect_unchanged 'non-go: cargo test        -> unchanged' \
	'(cd rust && cargo test)'

if ((fails > 0)); then
	printf '\nclaude-go-throttle-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nclaude-go-throttle-hook-test: ok\n'
