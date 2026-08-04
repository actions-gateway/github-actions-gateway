#!/usr/bin/env bash
#
# Unit tests for scripts/agent/claude-piped-gate-hook.sh — the PreToolUse hook
# that warns when a gate's exit code is read through a pipe (Q625).
#
# Both directions are asserted, because both fail silently. A pattern that stops
# matching lets the original bug back in — a failing `make check` piped into
# `tail` reports success and reads exactly like a real green. A pattern that
# matches too much turns every `git show`, `grep`, and commit message that
# merely NAMES a gate into a permission prompt; that is the shape Q624 records
# against the sibling throttle hook, and this hook sees every Bash call, so a
# false positive taxes every session.
#
# The registry is read from .claude/piped-gate-guard.json by the hook, so these
# cases assert the shipped list, not a copy of it.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# shellcheck disable=SC2016 # File-scope: every case below is a literal command
# string handed to the hook as data. `$?`, `$PIPESTATUS`, and `$(…)` inside them
# must reach the hook unexpanded — expanding one would assert a different
# command than the one the case is named for.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
HOOK="$REPO_ROOT/scripts/agent/claude-piped-gate-hook.sh"

fails=0

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# run_hook CMD — feed CMD to the hook as a Bash PreToolUse payload, print stdout.
run_hook() {
	jq -cn --arg c "$1" '{tool_name: "Bash", tool_input: {command: $c}}' | "$HOOK"
}

decision_of() {
	printf '%s' "$1" | jq -r '.hookSpecificOutput.permissionDecision // empty' 2>/dev/null || true
}

# expect_ask NAME CMD [REASON_SUBSTRING]
expect_ask() {
	local name="$1" cmd="$2" want_reason="${3:-}" out decision reason
	out="$(run_hook "$cmd")"
	decision="$(decision_of "$out")"
	if [[ "$decision" != "ask" ]]; then
		fail "$name: want ask, got ${decision:-<allow-unchanged>}"
		return
	fi
	if [[ -n "$want_reason" ]]; then
		reason="$(printf '%s' "$out" | jq -r '.hookSpecificOutput.permissionDecisionReason // empty')"
		if [[ "$reason" != *"$want_reason"* ]]; then
			fail "$name: reason missing [$want_reason]: $reason"
			return
		fi
	fi
	pass "$name"
}

# expect_quiet NAME CMD — the hook must produce no output at all, which is
# Claude Code's "proceed through the normal permission flow".
expect_quiet() {
	local name="$1" cmd="$2" out
	out="$(run_hook "$cmd")"
	if [[ -z "$out" ]]; then
		pass "$name"
	else
		fail "$name: expected no output, got: $out"
	fi
}

# --- Must warn: a gate whose status is swallowed by a pipe --------------------

expect_ask 'make check | tail' \
	'make check | tail -30' 'exit status is the filter'
expect_ask 'make check 2>&1 | tail; echo EXIT (the canonical false green)' \
	'make check 2>&1 | tail -30; echo "EXIT=$?"'
# The recurrence that reopened Q625: a failed pull reported EXIT=0.
expect_ask 'git pull --ff-only 2>&1 | tail; echo EXIT' \
	'git pull --ff-only 2>&1 | tail -5; echo "EXIT=$?"'
expect_ask 'git push | tail' \
	'git push -u origin HEAD 2>&1 | tail -3'
expect_ask 'make -C cmd/agc test-integration | grep' \
	'make -C cmd/agc test-integration | grep -E "FAIL|ok"'
expect_ask 'go test | tail' \
	'go test ./... | tail -20'
expect_ask 'a scripts/ gate | head' \
	'scripts/ci/check-tools.sh | head -20'
expect_ask 'bash-wrapped scripts/ gate | grep' \
	'bash scripts/docs/lint-backlog.sh | grep -v "^ok"'
expect_ask 'gate | tee still loses the status' \
	'make check | tee tmp/check.log'
expect_ask 'gate piped inside a command substitution' \
	'out=$(make check | tail -1)'
expect_ask 'gate in a subshell whose group is piped' \
	'(cd cmd/agc && go test ./...) | tail -5'
expect_ask 'gate after an unrelated leading segment' \
	'mkdir -p tmp; make check | grep FAIL'
expect_ask 'env-prefixed gate' \
	'GOFLAGS=-mod=mod go build ./... | tail -5'

# --- Must warn: PIPESTATUS does not exist in zsh ------------------------------

expect_ask 'PIPESTATUS[0] is itself the bug' \
	'make check 2>&1 | tail -5; echo "EXIT=${PIPESTATUS[0]}"' 'does not exist in zsh'
expect_ask 'bare $PIPESTATUS on a non-gate command' \
	'ls -l | wc -l; echo $PIPESTATUS' 'does not exist in zsh'

# --- Must NOT warn: the correct forms ----------------------------------------

expect_quiet 'redirect + echo $? is the documented fix' \
	'make check > tmp/check.log 2>&1; echo "EXIT=$?"'
expect_quiet 'redirect, then grep the FILE (not the gate)' \
	'make check > tmp/check.log 2>&1; echo "EXIT=$?"; grep -E "FAILED|Error [0-9]|^make:" tmp/check.log'
expect_quiet 'pipefail propagates the failure' \
	'set -o pipefail; make check | tail -30'
expect_quiet 'set -euo pipefail counts too' \
	'set -euo pipefail; make check 2>&1 | tail -30'
expect_quiet 'zsh $pipestatus recovers the stage status' \
	'make check 2>&1 | tail -5; echo "EXIT=${pipestatus[1]}"'
expect_quiet 'no pipe at all' \
	'make check'
expect_quiet 'gate on the RIGHT of the pipe keeps its own status' \
	'printf "%s" "$msg" | git commit -F -'

# --- Must NOT warn: commands that merely NAME a gate (the Q624 shape) --------

expect_quiet 'git show of a file containing the command' \
	'git show origin/main:CLAUDE.md | grep -n "make check"'
expect_quiet 'commit message quoting the bug' \
	'git commit -m "fix(ci): make check | tail was reporting EXIT=0"'
expect_quiet 'commit message in a heredoc body' \
	"$(printf 'git commit -F - <<%s\nfix(ci): stop doing make check | tail -30\nEOF\n' "'EOF'")"
expect_quiet 'grep for the pattern in the docs tree' \
	'grep -rn "make check | tail" docs/'
expect_quiet 'echo of the offending form' \
	'echo "never run: make check | tail"'

# --- Must NOT warn: non-gate commands piped into filters ---------------------

expect_quiet 'git log | head' 'git log --oneline | head -5'
expect_quiet 'git diff | head' 'git diff origin/main | head -40'
expect_quiet 'gh pr list | head' 'gh pr list | head -20'
expect_quiet 'cat a log | tail' 'cat tmp/check.log | tail -30'
expect_quiet 'kubectl get | grep' 'kubectl get pods -n gag | grep Running'
expect_quiet 'make help is informational, not a gate' 'make help | grep check'

# --- Must NOT warn: cases owned by another hook or another tool --------------

# A heavy `-race` form belongs to claude-go-throttle-hook.sh, which rewrites it.
# Two hooks answering one Bash call is undefined, and clobbering that rewrite
# would let an unthrottled -race run freeze the GUI (Q92).
expect_quiet 'go test -race defers to the throttle hook' \
	'go test -race ./... | tee tmp/race.log'
# `make test-race` carries "-race" but no `go build`/`go test` token, so the
# throttle hook never sees it and this one must still warn.
expect_ask 'make test-race | tail still warns' \
	'make test-race 2>&1 | tail -40'

# A non-Bash payload needs its own shape, not an empty command.
non_bash_out="$(jq -cn '{tool_name: "Read", tool_input: {file_path: "/x"}}' | "$HOOK")"
if [[ -z "$non_bash_out" ]]; then
	pass 'non-Bash tool payload -> unchanged'
else
	fail "non-Bash tool payload: expected no output, got: $non_bash_out"
fi

# --- Registry integrity ------------------------------------------------------

registry="$REPO_ROOT/.claude/piped-gate-guard.json"
if jq -e '.gates | length > 0' "$registry" >/dev/null; then
	pass 'registry parses and lists gates'
else
	fail 'registry does not parse or lists no gates'
fi

# Every registry pattern must be anchored at command position. An unanchored one
# searches the whole segment, which is how a pattern starts matching text that
# merely mentions the command.
while IFS= read -r pat; do
	[[ -n "$pat" ]] || continue
	if [[ "$pat" == '^'* ]]; then
		pass "registry pattern anchored: $pat"
	else
		fail "registry pattern not anchored to command position: $pat"
	fi
done < <(jq -r '(.gates[]?, .exempt[]?) // empty' "$registry")

if ((fails > 0)); then
	printf '\nclaude-piped-gate-hook-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nclaude-piped-gate-hook-test: ok\n'
