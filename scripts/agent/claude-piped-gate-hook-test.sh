#!/usr/bin/env bash
#
# Seam tests for scripts/agent/claude-piped-gate-hook.sh (Q625, Q665, Q668).
#
# The decision matrix — which commands warn and which stay silent — is a table
# test in devtools/agent/pipedgate. What cannot be asserted from Go is the seam
# this script owns: resolving the binary, building it when absent or stale,
# surviving a concurrent build, and failing open when anything goes wrong. A
# hook fires on every Bash call in every session, so a seam that breaks costs
# more than a missed warning.
#
# Two end-to-end cases are kept here as a wiring check: if the script resolved
# the wrong registry or the wrong binary, every Go assertion would still pass
# while the installed hook did nothing.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
HOOK="$REPO_ROOT/scripts/agent/claude-piped-gate-hook.sh"
BIN="$REPO_ROOT/.build/pipedgate"

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

payload() { jq -cn --arg c "$1" '{tool_name: "Bash", tool_input: {command: $c}}'; }
bg_payload() { jq -cn --arg c "$1" '{tool_name: "Bash", tool_input: {command: $c, run_in_background: true}}'; }
decision() { printf '%s' "$1" | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null || true; }

# --- Wiring: the installed script reaches the real binary and registry --------

out="$(payload 'make check 2>&1 | tail -30; echo "EXIT=$?"' | "$HOOK")"
if [[ "$(decision "$out")" == "deny" ]]; then
	pass 'end-to-end: the canonical false green is denied'
else
	fail "end-to-end: want deny, got: ${out:-<silence>}"
fi

# The deny reason is what the model reads, so it has to name the way out (Q697).
if printf '%s' "$out" | jq -er '.hookSpecificOutput.permissionDecisionReason | test("PIPED_GATE_OVERRIDE=<reason>")' >/dev/null; then
	pass 'end-to-end: the deny reason names the override prefix'
else
	fail "end-to-end: deny reason does not name the override: $out"
fi

out="$(payload 'PIPED_GATE_OVERRIDE=want-the-output-only make check 2>&1 | tail -30' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'end-to-end: the override prefix suppresses the deny'
else
	fail "end-to-end override: want silence, got: $out"
fi

out="$(payload 'make check > tmp/check.log 2>&1; echo "EXIT=$?"' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'end-to-end: the documented redirect stays silent'
else
	fail "end-to-end: want silence, got: $out"
fi

# The same command decided both ways by run_in_background alone (Q681) — the
# one field that separates the correct foreground form from the bug, and the
# one this seam has to carry through from the payload.
out="$(bg_payload 'make check > tmp/check.log 2>&1; echo "EXIT=$?"' | "$HOOK")"
if [[ "$(decision "$out")" == "deny" ]]; then
	pass 'end-to-end: a backgrounded gate ending in echo is denied'
else
	fail "end-to-end background: want deny, got: ${out:-<silence>}"
fi

# shellcheck disable=SC2016 # the payload is literal shell text; expanding $rc here is the bug
out="$(bg_payload 'make check > tmp/check.log 2>&1; rc=$?; echo "EXIT=$rc"; exit $rc' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'end-to-end: the backgrounded fix stays silent'
else
	fail "end-to-end background fix: want silence, got: $out"
fi

# The repo-state checks (Q665, Q668) reach real `git` and `gh`, so whether they
# warn depends on what has merged and what is open — the decision matrix for
# them is the Go table test against a fake probe. What is deterministic here,
# and worth asserting against the installed binary, is the direction that must
# never cost a subprocess: a command that merely NAMES the trigger. If a trigger
# regressed to whole-string matching, these would start probing.
out="$(payload 'git commit -m "docs: say when to git push after a rebase"' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'end-to-end: a commit message quoting git push stays silent'
else
	fail "end-to-end commit message: want silence, got: $out"
fi

out="$(payload 'grep -rn "gh pr create" CONTRIBUTING.md' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'end-to-end: a grep for gh pr create stays silent'
else
	fail "end-to-end grep: want silence, got: $out"
fi

# --- The binary is built on demand -------------------------------------------

rm -f "$BIN"
payload 'make check | tail' | "$HOOK" >/dev/null
if [[ -x "$BIN" ]]; then
	pass 'a missing binary is built on demand'
else
	fail 'a missing binary was not built'
fi

# --- A stale binary is rebuilt ------------------------------------------------
# Backdate the binary behind its sources; the next call must replace it. This is
# the check that keeps an edited decision from sitting unused behind a cached
# build.
touch -t 200001010000 "$BIN"
payload 'make check | tail' | "$HOOK" >/dev/null
if [[ "$BIN" -nt "$REPO_ROOT/devtools/agent/pipedgate/decide.go" ]]; then
	pass 'a stale binary is rebuilt'
else
	fail 'a stale binary was not rebuilt'
fi

# --- Concurrent callers each get a whole binary -------------------------------
# Ten hooks racing from cold, which is what a parallel-dispatch batch does. The
# staged-then-renamed build must leave exactly one binary and no debris.
rm -f "$BIN"
for _ in $(seq 10); do
	payload 'make check | tail' | "$HOOK" >/dev/null 2>&1 &
done
wait
debris="$(find "$REPO_ROOT/.build" -maxdepth 1 -name 'pipedgate.*' 2>/dev/null | wc -l | tr -d ' ')"
if [[ -x "$BIN" && "$debris" == "0" ]]; then
	pass 'ten concurrent callers leave one whole binary and no debris'
else
	fail "concurrent build left debris=$debris binary=$([[ -x "$BIN" ]] && echo yes || echo no)"
fi

# --- Fail open ----------------------------------------------------------------

out="$(jq -cn '{tool_name: "Read", tool_input: {file_path: "/x"}}' | "$HOOK")"
if [[ -z "$out" ]]; then
	pass 'a non-Bash payload is silent'
else
	fail "non-Bash payload: want silence, got: $out"
fi

# No Go toolchain and no cached binary: the hook must still exit 0 in silence
# rather than break every Bash call on a machine without Go.
rm -f "$BIN"
set +e
out="$(payload 'make check | tail' | PATH="/usr/bin:/bin" "$HOOK" 2>/dev/null)"
rc=$?
set -e
if [[ $rc -eq 0 && -z "$out" ]]; then
	pass 'no Go toolchain, no binary -> exit 0, silent'
else
	fail "no-toolchain fallback: rc=$rc out=$out"
fi

# Rebuild so the tree is left as the suite found it.
payload 'make check | tail' | "$HOOK" >/dev/null 2>&1 || true

if ((fails > 0)); then
	printf '\nclaude-piped-gate-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nclaude-piped-gate-hook-test: ok\n'
