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
#
# The status also decides the verdict: a signal death is reported under KILLED
# and exits with its own 128+n status, because it reached no verdict and is not
# a defect to go and read (Q837). The cases below pin both halves, since
# silencing a kill and calling it a failure are both wrong.
#
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
want_rc 'a kill exits with the killed status' 137
want 'a signal death is named as a signal' 'KILLED:.*killed \(signal 9, exit 137\)'
want_no 'a kill is not counted as a failure' 'FAILED'
want 'the summary says a kill is not a gate failure' 'not a gate failure'

# Q837's measurement: SIGTERM under host contention. An external kill must not
# read as a real gate failure, and must not vanish from the output either.
rp "term:sh -c 'kill -15 \$\$'"
want_rc 'a SIGTERM kill exits 143, not 1' 143
want 'a SIGTERM kill is named as signal 15' 'KILLED:.*term \(signal 15, exit 143\)'
want_no 'a SIGTERM kill is not counted as a failure' 'FAILED'

# A verdict outranks a kill: something reached a bad verdict, so the fan-out
# exits 1 — and the kill is still reported beside it.
rp "boom:sh -c 'exit 3'" "killed:sh -c 'kill -9 \$\$'"
want_rc 'a real failure beside a kill exits 1' 1
want 'the failure is reported' 'FAILED:.*boom \(exit 3\)'
want 'the kill is reported beside it' 'KILLED:.*killed \(signal 9, exit 137\)'

# Two kills exit with the first status, and the second is still reported. This
# also pins that the first-kill bookkeeping survives `set -e`.
rp "first:sh -c 'kill -15 \$\$'" "second:sh -c 'kill -9 \$\$'"
want_rc 'two kills exit with the first status' 143
want 'a second kill is reported too' 'KILLED:.*second \(signal 9, exit 137\)'

# 128 is not a signal death: git spends it on any fatal error, which is what
# Q820's temp-file signature looks like. It stays a primary failure, so the
# split is rc > 128 rather than the row's rc >= 128.
rp "gitfatal:sh -c 'exit 128'"
want_rc 'exit 128 still fails the fan-out' 1
want 'exit 128 is a failure, not a kill' 'FAILED:.*gitfatal \(exit 128\)'
want_no 'exit 128 is not reported as a kill' 'KILLED'

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
