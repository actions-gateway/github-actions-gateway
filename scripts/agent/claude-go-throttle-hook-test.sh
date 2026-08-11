#!/usr/bin/env bash
#
# Contract tests for scripts/agent/claude-go-throttle-hook.sh — the PreToolUse
# hook that auto-throttles raw `go build`/`go test` commands (Q92, Q347).
#
# The load-bearing property is that a heavy `-race` run never escapes the
# throttle: a bare form is rewritten and auto-allowed, a compound/redirected/
# wrapped `-race` form is rewritten and returned as an `ask` (Q347 — no longer
# blocked), and a `-race` form the hook cannot pin a single go token in is denied
# with a specific reason. These are asserted against synthetic hook payloads.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# The decision itself is devtools/agent/gothrottle (Q708) and its table test
# carries the case matrix. What this suite covers is everything around it: that
# the entry point builds and execs the binary, that the prefix resolved from the
# real local-throttle.sh lands in the right place, and that the emitted JSON is
# the shape Claude Code reads.
#
# Both directions are asserted, because both fail silently (Q624). The hook
# counts a `go` in *command position*, or behind an allowlisted wrapper (Q696),
# so a command that merely NAMES one — a `git commit` message, a heredoc body —
# must pass through untouched; but a matcher that stopped matching would let a
# real unthrottled `-race` run freeze the GUI, which is the more expensive
# error. Every must-not-match case below is paired with a must-match one built
# from the same text.
#
# Every assertion here reports the hook's GOTHROTTLE_DEBUG trace alongside its
# own message (Q703). Without it a failure reads `got decision= reason=`, which
# every silent path across the hook and the binary produces identically — the
# symptom an occurrence under `run-parallel` left behind, and no mechanism. The
# gothrottle package comment counts those paths and names the trace's contract.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
HOOK="$REPO_ROOT/scripts/agent/claude-go-throttle-hook.sh"
THROTTLE="$REPO_ROOT/scripts/agent/local-throttle.sh"

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

# The hook's stderr from the most recent run_hook call. GOTHROTTLE_DEBUG makes
# each silent path in the hook and the binary name itself there; nothing reads
# it unless an assertion fails.
TRACE_FILE="$(mktemp)"
trap 'rm -f "$TRACE_FILE"' EXIT

# run_hook CMD — feed CMD to the hook as a Bash PreToolUse payload, print stdout.
run_hook() {
	local cmd="$1"
	jq -cn --arg c "$cmd" '{tool_name: "Bash", tool_input: {command: $c}}' \
		| throttle_env GOTHROTTLE_DEBUG=1 "$HOOK" 2>"$TRACE_FILE"
}

# field OUT EXPR — read a jq path from the hook output (empty if absent/invalid).
field() {
	local out="$1" expr="$2"
	printf '%s' "$out" | jq -r "$expr // empty" 2>/dev/null || true
}

# diag OUT — what the hook actually produced, for a failure message (Q703).
#
# A missing decision is exit 0 with empty stdout, and every silent path across
# the entry point and the binary shares that one observable — which is why the
# 2026-08-08 occurrence, reported as `got decision= reason=`, named no
# mechanism. The trace says which path ran; the raw stdout separates a hook that
# printed nothing from one that printed something field() could not read, since
# field() swallows jq's status and stderr alike.
diag() {
	local trace
	trace="$(tr '\n' ';' <"$TRACE_FILE")"
	printf 'raw-stdout=[%s] trace=[%s]' "$1" "${trace:-<none>}"
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
		fail "$name: expected no output; $(diag "$out")"
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
		fail "$name: want decision=$want_decision got=${got_decision:-<none>}; $(diag "$out")"
		return
	fi
	if [[ -n "$want_cmd" && "$got_cmd" != "$want_cmd" ]]; then
		fail "$name: want cmd=[$want_cmd] got=[$got_cmd]; $(diag "$out")"
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
	fail "safety: -race not throttled by rewrite: [$race_cmd]; $(diag "$race_out")"
fi

# --- Q347: a -race form we cannot pin a single go token in is denied ----------

deny_out="$(run_hook 'go build ./... && go test -race ./...')"
deny_decision="$(field "$deny_out" '.hookSpecificOutput.permissionDecision')"
deny_reason="$(field "$deny_out" '.hookSpecificOutput.permissionDecisionReason')"
if [[ "$deny_decision" == "deny" && "$deny_reason" == *"more than one go build/test"* ]]; then
	pass 'unrewritable: two go invocations -> deny with specific reason'
else
	fail "unrewritable: want deny w/ specific reason, got decision=$deny_decision reason=$deny_reason; $(diag "$deny_out")"
fi

# --- Q624: text that only NAMES a go command is not an invocation -------------
#
# Each must-not-match case is followed by a must-match one built from the same
# text, so a matcher that quietly stopped seeing real invocations fails here.

# `read -d ''` returns non-zero at EOF; the payloads are multi-line by design.
read -r -d '' commit_two_mentions <<'CASE' || true
git commit -F - <<'MSG'
fix(agent): stop denying a commit that quotes a throttled command

Before this, `go test -race ./...` in the message read as an invocation, so a
message naming `go test -race` twice was denied outright.
MSG
CASE

read -r -d '' commit_one_mention <<'CASE' || true
git commit -F - <<'MSG'
docs(testing): note the throttle prefix

Running `go test -race ./...` locally needs the prefix.
MSG
CASE

read -r -d '' commit_then_race <<'CASE' || true
git commit -F - <<'MSG'
docs(testing): note the throttle prefix

Running `go test -race ./...` locally needs the prefix.
MSG
go test -race ./...
CASE

read -r -d '' commit_dash_heredoc <<'CASE' || true
git commit -F - <<-'MSG'
	docs(testing): note the throttle prefix

	Running `go test -race ./...` locally needs the prefix.
	MSG
CASE

expect_unchanged 'commit heredoc: two -race mentions -> unchanged (was deny)' \
	"$commit_two_mentions"
expect_unchanged 'commit heredoc: one -race mention  -> unchanged (was ask+rewrite)' \
	"$commit_one_mention"
expect_unchanged 'commit heredoc: <<- tab-indented   -> unchanged' \
	"$commit_dash_heredoc"
expect_unchanged 'commit -m: message quotes -race    -> unchanged' \
	'git commit -m "docs: note that go test -race needs the throttle prefix"'
expect_unchanged 'commit -m: quoted, then chained    -> unchanged' \
	'git commit -m "docs: go test -race notes" && git push'
expect_unchanged 'grep: pattern names go test -race  -> unchanged' \
	"grep -rn 'go test -race' scripts/agent"
# `-race` inside a message must not upgrade a plain `go test` to the -race path:
# the flag is read from the invocation's own arguments, not the whole string.
expect_unchanged 'mention: -race in msg, plain go test -> unchanged' \
	'go test ./... ; git commit -m "docs: go test -race notes"'

# The paired must-match direction: the very same message text with a real
# invocation after it is still seen, throttled, and asked.
race_after_out="$(run_hook "$commit_then_race")"
race_after_decision="$(field "$race_after_out" '.hookSpecificOutput.permissionDecision')"
race_after_cmd="$(field "$race_after_out" '.hookSpecificOutput.updatedInput.command')"
# The backticks are literal message text, not a command substitution: this
# asserts the commit message still reads un-prefixed.
# shellcheck disable=SC2016
if [[ "$race_after_decision" == "ask" &&
	"$race_after_cmd" == *"MSG"$'\n'"$PREFIX go test -race ./..." &&
	"$race_after_cmd" == *'Running `go test -race ./...` locally'* ]]; then
	pass 'commit heredoc + real -race: ask, prefix outside the message'
else
	fail "commit heredoc + real -race: want ask w/ prefix only on the real go; got decision=$race_after_decision cmd=[$race_after_cmd]; $(diag "$race_after_out")"
fi

# Two genuine invocations still deny, even when a message mentions a third.
read -r -d '' two_real_with_mention <<'CASE' || true
go build ./... && go test -race ./...
git commit -m "docs: go test -race notes"
CASE
deny2_out="$(run_hook "$two_real_with_mention")"
if [[ "$(field "$deny2_out" '.hookSpecificOutput.permissionDecision')" == "deny" ]]; then
	pass 'two real invocations + a mention -> still deny'
else
	fail "two real invocations + a mention: want deny; $(diag "$deny2_out")"
fi

# --- Q696: a -race behind a wrapper is throttled, not skipped -----------------
#
# `go` is an argument to `timeout` rather than a command, so the scanner this
# replaced reported no invocation and the run escaped the throttle entirely. The
# peel is an allowlist over words, so both directions matter: an unrecognised
# wrapper must stay silent rather than have the prefix guessed into the wrong
# place, and a wrapped run must never take the auto-allow path — `timeout … go
# test` is not the bare shape the `Bash(go test *)` allowlist trusts.
expect_decision 'wrapper: timeout … -race  -> ask + prefix before go' \
	'timeout 900 go test -race ./...' ask "timeout 900 $PREFIX go test -race ./..."
expect_decision 'wrapper: nested           -> ask + prefix before go' \
	'nohup timeout 900 go test -race ./...' ask "nohup timeout 900 $PREFIX go test -race ./..."
expect_unchanged 'wrapper: unknown name     -> unchanged (peel stops)' \
	'xargs -n1 go test -race ./...'
expect_unchanged 'wrapper: unknown option   -> unchanged (peel stops)' \
	'timeout --frobnicate 900 go test -race ./...'
expect_unchanged 'wrapper: no -race         -> unchanged (adds no new prompt)' \
	'timeout 900 go test ./...'

# Command position is what counts, not a subshell: a newline-separated and a
# bare `cd … && …` form must both still be caught.
expect_decision 'newline: cd then -race    -> ask + prefix before go' \
	"$(printf 'cd cmd/agc\ngo test -race ./...')" ask \
	"$(printf 'cd cmd/agc\n%s go test -race ./...' "$PREFIX")"
expect_decision 'chain: cd && -race        -> ask + prefix before go' \
	'cd cmd/agc && go test -race ./...' ask "cd cmd/agc && $PREFIX go test -race ./..."

# --- Cases the hook must leave untouched --------------------------------------

# A non-`-race` compound stays on the normal permission flow (unchanged).
expect_unchanged 'compound: no -race        -> unchanged' \
	'(cd cmd/agc && go test ./...)'
# An already-throttled compound short-circuits before the compound branch.
expect_unchanged 'compound: already throttled -> unchanged' \
	'(cd cmd/agc && nice -n 10 taskpolicy -d throttle go test -race ./...)'
# The pre-Q441 prefix must still read as throttled: a stale worktree still emits
# it, and re-wrapping an already-demoted command would stack two prefixes.
expect_unchanged 'compound: legacy prefix     -> unchanged' \
	'(cd cmd/agc && taskpolicy -c utility go test -race ./...)'
# A non-go command is not our concern.
expect_unchanged 'non-go: cargo test        -> unchanged' \
	'(cd rust && cargo test)'

# --- Q703: the trace names the silent path, and stays off unless asked --------
#
# Silence is the whole failure contract, so an occurrence under load reports
# `got decision= reason=` and nothing more: many paths, one observable, no
# mechanism. Both directions matter for the same reason they do for the matcher:
# a trace that stopped naming its site leaves the next occurrence as
# unattributable as the last, and one that leaked into a real invocation would
# put text on the stderr of every Bash call in the session.

run_hook '(cd rust && cargo test)' >/dev/null
if grep -q 'no command-position go build/test' "$TRACE_FILE"; then
	pass 'trace: names the silent path taken'
else
	fail "trace: want the silent path named, got: $(tr '\n' ';' <"$TRACE_FILE")"
fi

# Unset explicitly rather than via throttle_env: the variable is what this case
# controls, and inheriting one from the caller would make it assert nothing.
quiet_err="$(jq -cn '{tool_name: "Bash", tool_input: {command: "(cd rust && cargo test)"}}' \
	| env -u CI -u GOTHROTTLE_DEBUG DISPLAY="${DISPLAY:-:0}" "$HOOK" 2>&1 >/dev/null)"
if [[ -z "$quiet_err" ]]; then
	pass 'trace: silent unless GOTHROTTLE_DEBUG is set'
else
	fail "trace: want no stderr with GOTHROTTLE_DEBUG unset, got: $quiet_err"
fi

if ((fails > 0)); then
	printf '\nclaude-go-throttle-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nclaude-go-throttle-hook-test: ok\n'
