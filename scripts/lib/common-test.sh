#!/usr/bin/env bash
#
# Unit tests for the assertion helpers in scripts/lib/common.sh.
#
# Currently die_if_killed, the guard the scripts/ test suites put in front of a
# numeric exit-status comparison so a signal death is reported as a kill rather
# than as the subject answering wrongly (Q1023).
#
# Why both directions are asserted. The guard sits on the path every suite takes
# to report a failure, so the two ways it can be wrong are equally quiet and
# point opposite ways. A guard that stops firing puts the repo back where Q1023
# found it: `expected rc=1, got rc=143` blaming a gate for a SIGTERM. A guard
# that fires too eagerly is worse, because it swallows real verdicts — an
# assertion that should have failed exits with a signal status instead, and
# run-parallel.sh files it under KILLED, which reads as host contention and is
# explicitly not something to go and read. So every case here pins one side: a
# genuine rc=1 mismatch must still reach the caller's own comparison untouched,
# and only a status above 128 that is not the wanted one may exit.
#
# The 128 boundary is its own case in both directions. git spends 128 on any
# fatal error, so treating it as a signal death would hand a real git failure
# back as contention (Q837, where run-parallel.sh drew the same line).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

fails=0

ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1" >&2
	fails=$((fails + 1))
}

# passes_through NAME RC [WANT] — assert the guard returns to its caller, which
# is what leaves the suite's own comparison to decide the verdict.
passes_through() {
	local name="$1"
	shift
	local out rc=0
	out="$(die_if_killed "$@" 2>&1)" || rc=$?
	if ((rc != 0)); then
		bad "$name: guard exited $rc; wanted it to return"
		return
	fi
	if [[ -n "$out" ]]; then
		bad "$name: guard returned but printed '$out'"
		return
	fi
	ok "$name"
}

# reports_kill NAME WANT_RC WANT_SIGNAL -- args... — assert the guard exits with
# the killed status and names the signal.
reports_kill() {
	local name="$1" want_rc="$2" want_signal="$3"
	shift 4
	local out rc=0
	out="$(die_if_killed "$@" 2>&1)" || rc=$?
	if ((rc != want_rc)); then
		bad "$name: guard exited $rc, wanted $want_rc"
		return
	fi
	if [[ "$out" != *"signal $want_signal"* ]]; then
		bad "$name: report did not name signal $want_signal; got '$out'"
		return
	fi
	if [[ "$out" != *KILLED* ]]; then
		bad "$name: report did not say KILLED; got '$out'"
		return
	fi
	ok "$name"
}

# --- a verdict is a verdict: the guard must not touch it ----------------------

passes_through 'a pass (rc=0) reaches the caller' 'case' 0
passes_through 'a real mismatch (rc=1) reaches the caller' 'case' 1
passes_through 'a refusal (rc=2) reaches the caller' 'case' 2
passes_through 'a timeout (rc=124) reaches the caller' 'case' 124
passes_through "git's fatal error (rc=128) is not a signal death" 'case' 128

# --- a signal death is handed back, not compared ------------------------------

reports_kill 'SIGTERM (143) is reported as a kill' 143 15 -- 'case' 143
reports_kill 'the OOM killer (137) is reported as a kill' 137 9 -- 'case' 137
reports_kill 'the first signal (129) is reported as a kill' 129 1 -- 'case' 129
reports_kill 'a kill against a wanted status still exits' 143 15 -- 'case' 143 1

# --- want-aware: a suite that asserts a kill reached its verdict ---------------
#
# scripts/ci/run-parallel-test.sh expects 137 and 143 from the runner's own
# KILLED path. A want-blind guard would exit before those assertions ran, which
# is why WANT is part of the signature rather than an afterthought.

passes_through 'a wanted SIGTERM (143) is a verdict, not a kill' 'case' 143 143
passes_through 'a wanted OOM kill (137) is a verdict, not a kill' 'case' 137 137

# --- the report names the case, so a fan-out stays attributable ----------------

kill_out="$(die_if_killed 'the endpoint-parity fake' 143 1 2>&1)" || true
if [[ "$kill_out" == *'the endpoint-parity fake'* ]]; then
	ok 'the report names the assertion that was killed'
else
	bad "the report did not name the assertion; got '$kill_out'"
fi

# --- end to end: a suite that captures a kill exits with the killed status -----
#
# The shape Q1023 measured, in miniature: capture a killed command's status, then
# assert against a wanted one. The point is the suite's own exit status, because
# that is what run-parallel.sh reads to tell KILLED from FAILED. Without the
# guard this fixture exits 1 with a FAIL line, which is the false finding.

fixture_rc=0
fixture_out="$(
	# Inside the subshell, not on the assignment: a command substitution is
	# expanded before any redirection on the assignment applies, so `... )" 2>&1`
	# leaves the guard's own KILLED line on the suite's stderr. In a
	# `make scripts-test` fan-out that stray line reads as a real kill.
	exec 2>&1
	set -euo pipefail
	source "${REPO_ROOT}/scripts/lib/common.sh"
	rc=0
	# A command the host killed, rather than one that answered.
	bash -c 'kill -TERM $$' || rc=$?
	die_if_killed 'a fake missing the endpoint fails' "$rc" 1
	echo "FAIL a fake missing the endpoint fails: expected rc=1, got rc=$rc"
	exit 1
)" || fixture_rc=$?

if ((fixture_rc == 143)); then
	ok 'a killed assertion exits with the killed status, not 1'
else
	bad "a killed assertion exited $fixture_rc, wanted 143"
fi
if [[ "$fixture_out" != *'expected rc=1'* ]]; then
	ok 'a killed assertion does not report the gate as having answered wrongly'
else
	bad "the false finding survived: '$fixture_out'"
fi

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall assertions passed\n'
