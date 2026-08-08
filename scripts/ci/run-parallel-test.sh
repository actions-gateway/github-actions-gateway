#!/usr/bin/env bash
#
# Unit tests for scripts/ci/run-parallel.sh — the fan-out behind `make check`,
# `make status-gates` and `make scripts-test`.
#
# The load-bearing claim is that a failure is attributable, and attributable to
# the right thing: output stays labeled, every pid is waited so a red suite is
# never collateral from a sibling, and the summary carries each failure's exit
# status. The status is what tells an assertion failure (small rc) apart from a
# command the kernel killed (128+n; 137 is the OOM killer's) or one that was
# never found (127). Five flakes in this family went undiagnosed while the
# summary named only a label (Q703).
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
RP="$REPO_ROOT/scripts/ci/run-parallel.sh"

fails=0
out=""
rc=0

# rp SPEC... — run the fan-out, capturing merged output and exit code.
rp() {
	rc=0
	out="$("$RP" "$@" 2>&1)" || rc=$?
}

# want NAME PATTERN — assert the captured output matched an extended regexp.
want() {
	local name="$1" pattern="$2"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'ok   %-42s\n' "$name"
	else
		printf 'FAIL %-42s no match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	fi
}

# want_no NAME PATTERN — assert the captured output did NOT match.
want_no() {
	local name="$1" pattern="$2"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'FAIL %-42s unexpected match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	else
		printf 'ok   %-42s\n' "$name"
	fi
}

# want_rc NAME WANT — assert the fan-out's own exit code.
want_rc() {
	local name="$1" want="$2"
	if [[ "$rc" == "$want" ]]; then
		printf 'ok   %-42s rc=%s\n' "$name" "$rc"
	else
		printf 'FAIL %-42s want rc=%s got rc=%s\n%s\n' "$name" "$want" "$rc" "$out" >&2
		fails=$((fails + 1))
	fi
}

# An all-passing run is silent about failure and exits 0.
rp "a:true" "b:true"
want_rc 'all commands pass' 0
want_no 'a passing run names no failure' 'FAILED'

# Output stays attributable to its command.
rp "alpha:echo hello" "beta:echo world"
want 'output is labeled per command' '^\[alpha\] hello$'
want 'a second command keeps its own label' '^\[beta\] world$'

# stderr is captured and labeled too, not lost.
rp "noisy:sh -c 'echo oops >&2'"
want 'stderr is labeled, not dropped' '^\[noisy\] oops$'

# A failure names the label AND the exit status (Q703). Without the status,
# an assertion failure and a killed command read identically.
rp "ok-one:true" "boom:sh -c 'exit 3'"
want_rc 'one failure fails the fan-out' 1
want 'a failure names its label' 'FAILED:.*boom'
want 'a failure reports its exit status' 'boom \(exit 3\)'
want_no 'a passing sibling is not reported' 'ok-one \(exit'

# A command the kernel kills is reported as a signal, not as an assertion
# failure. 137 (SIGKILL) is what an OOM kill looks like in a labeled log.
rp "killed:sh -c 'kill -9 \$\$'"
want_rc 'a killed command fails the fan-out' 1
want 'a signal death is named as a signal' 'killed \(signal 9, exit 137\)'

# 127 is a missing command, not a failing one — the distinction a bare label
# also erased.
rp "absent:this-command-does-not-exist-q703"
want 'a missing command reports 127' 'absent \(exit 127\)'

# Every pid is waited, so a red suite is never collateral from a sibling — the
# claim the Q703 row had to make by reading the source. The later command
# outlives the earlier failure, so its status is reported only if the wait loop
# keeps going past one. Asserting a survivor's side effect instead would prove
# nothing: the EXIT trap kills the wrapper subshells, not the commands under
# them, so an abandoned sibling still finishes and still writes its output.
rp "early:sh -c 'exit 2'" "late:sh -c 'sleep 0.5; exit 4'"
want_rc 'a sibling failure fails the fan-out' 1
want 'the first failure is named' 'early \(exit 2\)'
want 'a failure after it is still waited for' 'late \(exit 4\)'

# A command may contain colons; only the first splits label from command.
rp "colon:sh -c 'echo a:b:c'"
want 'only the first colon splits the spec' '^\[colon\] a:b:c$'

# No arguments is a usage error, not a silent success.
rp
want_rc 'no arguments exits non-zero' 1
want 'no arguments prints usage' 'usage:'

if (( fails > 0 )); then
	echo "run-parallel-test: $fails failure(s)" >&2
	exit 1
fi
echo "run-parallel-test: all assertions passed"
