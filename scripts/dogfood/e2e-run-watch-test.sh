#!/usr/bin/env bash
#
# Assertions for scripts/dogfood/e2e-run-watch.sh's relay helpers (Q615).
#
# This watcher decides whether an hour-long billable release gate passes, and it
# is the operator's only live view of a run happening on someone else's machine.
# The helpers below are where that can go quietly wrong: a conclusion mapped to
# the wrong exit status passes a red release, a broken already-seen calculation
# either replays the whole heartbeat every poll or stops relaying it entirely,
# and a log fetch that propagates its exit status kills the gate outright. The
# watch's own deadline is asserted here too (Q629): unbounded, it holds the
# cluster on a run that never concludes; too tight, it fails a healthy release.
#
# The log fixture is real: lines copied verbatim from run 30751971883, timestamp
# prefixes and all, so the filter is asserted against what GitHub actually
# serves rather than an idealized shape.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

WORK="${REPO_ROOT}/tmp/e2e-run-watch-test.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

# Set before sourcing: progress.sh defaults these only when unset, and watch_run
# records the run it parks on (progress_run, Q630). Unscoped, that append lands
# in the repo's live release-validation stream and repoints the sentinel's stall
# check at a run that does not exist — a `make check` during the v1.4.0-rc.2 gate
# put three owner/repo 42 events there (Q777).
RELEASE_PROGRESS_FILE="${WORK}/progress.jsonl"
RELEASE_STATUS_FILE="${WORK}/status.json"
export RELEASE_PROGRESS_FILE RELEASE_STATUS_FILE

# shellcheck source=scripts/dogfood/e2e-run-watch.sh
source "$REPO_ROOT/scripts/dogfood/e2e-run-watch.sh"

fails=0

ok() { printf 'ok   %-50s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-50s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

want_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$name" "$(printf '%q' "$got")"
	else
		bad "$name" "want $(printf '%q' "$want") got $(printf '%q' "$got")"
	fi
}

want_contains() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == *"$want"* ]]; then
		ok "$name" "$(printf '%q' "$want")"
	else
		bad "$name" "want a match for $(printf '%q' "$want") in $(printf '%q' "$got")"
	fi
}

# Verbatim from the run's job log, including the surrounding noise the filter
# has to reject and the runner's ISO timestamp prefix it has to strip.
FIXTURE_LOG='2026-08-02T14:19:32.0013876Z ##[group]Run make e2e KIND_CLUSTER=actions-gateway-e2e
2026-08-02T14:20:12.5506255Z Running Suite: e2e suite - /home/runner/work/cmd/gmc/test/e2e
2026-08-02T14:20:14.1000000Z [e2e t+0:14] 0/73 specs | 0 ok, 0 failed, 0 skipped | running: none
2026-08-02T14:20:44.2000000Z [e2e t+0:44] 10/73 specs | 4 ok, 0 failed, 6 skipped | running: E2E_GMC_RBAC AGC... (7s)
2026-08-02T14:21:14.3000000Z [e2e t+1:14] 24/73 specs | 18 ok, 0 failed, 6 skipped | running: E2E_V2_DirectEgress... (14s)
2026-08-02T14:27:53.6890255Z SUCCESS! -- 62 Passed | 0 Failed | 0 Pending | 11 Skipped'

echo '== the filter finds heartbeats in a real log and strips the timestamp =='
got="$(printf '%s\n' "$FIXTURE_LOG" | heartbeat_lines)"
want_eq 'three heartbeat lines, timestamps stripped' \
	'[e2e t+0:14] 0/73 specs | 0 ok, 0 failed, 0 skipped | running: none
[e2e t+0:44] 10/73 specs | 4 ok, 0 failed, 6 skipped | running: E2E_GMC_RBAC AGC... (7s)
[e2e t+1:14] 24/73 specs | 18 ok, 0 failed, 6 skipped | running: E2E_V2_DirectEgress... (14s)' \
	"$got"

# The suite banner and the SUCCESS! line are the two most heartbeat-adjacent
# things in the log; neither is a heartbeat.
want_eq 'suite banner is not relayed' '' \
	"$(printf '%s\n' '2026-08-02T14:20:12Z Running Suite: e2e suite' | heartbeat_lines)"
want_eq 'a log with no heartbeats yields nothing' '' \
	"$(printf '%s\n' 'nothing to see' | heartbeat_lines)"

echo
echo '== only unseen heartbeats are relayed =='
# The log endpoint returns the WHOLE log every poll. Without this the operator
# gets the entire heartbeat history re-printed every 30 s.
all="$(printf '%s\n' "$FIXTURE_LOG" | heartbeat_lines)"
want_eq 'nothing seen yet: all three' 3 "$(printf '%s\n' "$all" | lines_after 0 | grep -c .)"
want_eq 'two seen: only the third' \
	'[e2e t+1:14] 24/73 specs | 18 ok, 0 failed, 6 skipped | running: E2E_V2_DirectEgress... (14s)' \
	"$(printf '%s\n' "$all" | lines_after 2)"
want_eq 'all seen: nothing' '' "$(printf '%s\n' "$all" | lines_after 3)"
# A poll that lands between two heartbeats must stay quiet rather than repeat.
want_eq 'seen beyond the total: still nothing' '' "$(printf '%s\n' "$all" | lines_after 9)"

echo
echo '== only an outright success passes the gate =='
want_eq 'success passes' 0 "$(conclusion_rc success)"
want_eq 'failure fails' 1 "$(conclusion_rc failure)"
# A cancelled or timed-out release validation is as fatal as a failed one, and
# neither is spelled "failure".
want_eq 'cancelled fails' 1 "$(conclusion_rc cancelled)"
want_eq 'timed_out fails' 1 "$(conclusion_rc timed_out)"
want_eq 'startup_failure fails' 1 "$(conclusion_rc startup_failure)"
# The property that matters most: a conclusion GitHub adds later must not
# default to passing a release.
want_eq 'an unrecognized conclusion fails' 1 "$(conclusion_rc some_future_state)"
want_eq 'an empty conclusion fails' 1 "$(conclusion_rc '')"

echo
echo '== a failed log fetch cannot kill the gate =='
# The logs endpoint 404s for a job that is queued — the normal state for the
# minutes between the job appearing and a runner picking it up. Under pipefail
# that status escapes the pipe, escapes the assignment that captures it, and
# reaches `set -e`, killing the gate one poll into a ~25-minute leg and
# reporting the still-queued run as failed. Measured on run 30762026452 while
# validating v1.3.0-rc.5.
REPO=owner/repo

gh() { return 1; }
rc=0
got="$(collect_heartbeats '1 2')" || rc=$?
want_eq 'a failing fetch yields no heartbeats' '' "$got"
want_eq 'a failing fetch does not fail the watcher' 0 "$rc"

# Positive control. Without it, a stub that never ran would satisfy the two
# assertions above exactly as well as the fix does.
gh() { printf '%s\n' "$FIXTURE_LOG"; }
rc=0
got="$(collect_heartbeats '1')" || rc=$?
want_eq 'a succeeding fetch relays its heartbeats' 3 "$(printf '%s\n' "$got" | grep -c .)"
want_eq 'a succeeding fetch exits clean' 0 "$rc"

# The gate job and the e2e job are fetched in one sweep; whichever 404s must not
# discard the other's heartbeats. The failing job goes LAST on purpose: a loop
# whose final iteration succeeds returns 0 whether or not the fetch status is
# neutralized, so ordering it the other way asserts nothing.
gh() {
	if [[ "$*" == *"/jobs/1/logs" ]]; then
		return 1
	fi
	printf '%s\n' "$FIXTURE_LOG"
}
rc=0
got="$(collect_heartbeats '2 1')" || rc=$?
want_eq 'one job 404ing keeps the other job'\''s heartbeats' 3 \
	"$(printf '%s\n' "$got" | grep -c .)"
want_eq 'a partial failure exits clean' 0 "$rc"

unset -f gh

echo
echo '== the watch is bounded =='
# Q629. The loop's only other exit is `completed`, so a run that never reaches it
# holds the gate — and the billable nodes it is watching — for as long as the
# process lives. Measured on the rc.5 re-run: 33 minutes in `queued`, ended by
# hand. The clock is faked, so this asserts the deadline rather than waiting on
# one.
#
# watch_run reads both the clock and the run state through command substitution,
# so a fake that counts in a shell variable loses every increment to the
# subshell and the watch runs forever. Both keep their state in a file.
CLOCK="${WORK}/clock"
POLLS="${WORK}/polls"

# progress_run appends only to a stream that already exists, so without this the
# watches below record nothing and the scoping above is asserted by nobody.
progress_init

# 60s per reading, so a watch cannot outlast its budget by polling slowly.
now() {
	local t
	t=$(($(cat "$CLOCK") + 60))
	echo "$t" >"$CLOCK"
	printf '%s' "$t"
}
sleep() { :; }
e2e_job_ids() { :; }
REPO=owner/repo
E2E_RUN_WATCH_INTERVAL=0

# The run stays queued for RUN_STATE_AFTER polls, then reports RUN_STATE_FINAL.
# Reads reach the subshell fine; only the poll count has to come back out.
run_state() {
	local n
	n=$(($(cat "$POLLS") + 1))
	echo "$n" >"$POLLS"
	if ((n > RUN_STATE_AFTER)); then
		printf '%s' "$RUN_STATE_FINAL"
	else
		printf 'queued '
	fi
}

# arm AFTER FINAL — reset both fakes and set where the run concludes.
arm() {
	RUN_STATE_AFTER="$1"
	RUN_STATE_FINAL="$2"
	echo 0 >"$CLOCK"
	echo 0 >"$POLLS"
}

# The escape hatch matters: without a deadline the loop never ends and this test
# would hang rather than fail, so the run concludes green far past any poll the
# deadline permits. A watch with the deadline removed reaches it and exits 0,
# which is what turns the assertion below red.
arm 100 'completed success'

# A 300s budget against a clock that advances 60s per reading: the first reading
# sets the deadline at 360, so polls 1-4 read 120..300 and poll 5 reads 360 and
# gives up. Asserting that count, not merely "under the escape hatch", pins the
# arithmetic — an off-by-one that watched twice as long would still be bounded.
E2E_RUN_WATCH_TIMEOUT=300
rc=0
watch_run 42 >/dev/null 2>"${WORK}/timeout.err" || rc=$?
want_eq 'a run that never concludes times out' 124 "$rc"
want_eq 'it gave up on its deadline, not the escape hatch' 5 "$(cat "$POLLS")"
# An operator who hits this has to know which knob to turn.
want_contains 'the timeout error names E2E_RUN_WATCH_TIMEOUT' \
	'E2E_RUN_WATCH_TIMEOUT' "$(cat "${WORK}/timeout.err")"
want_contains 'and the run it gave up on' 'run 42' "$(cat "${WORK}/timeout.err")"

# The control that keeps this deadline from becoming #1171 again: a run that is
# slow but still moving must be watched to its conclusion, not killed. That
# failure — a gate that fails a healthy release — is the worse of the two.
arm 3 'completed success'
E2E_RUN_WATCH_TIMEOUT=3600
rc=0
watch_run 42 >/dev/null 2>&1 || rc=$?
want_eq 'a slow run inside the budget still passes' 0 "$rc"

# And a run that concludes red fails as a failure, not as a timeout: an operator
# reading the exit status has two different problems to tell apart.
arm 3 'completed failure'
rc=0
watch_run 42 >/dev/null 2>&1 || rc=$?
want_eq 'a failed run fails as a failure, not a timeout' 1 "$rc"

# Q777. Each watch above recorded the run it parked on; assert all three landed
# in this suite's own stream. Naming the path rather than the variable is the
# point: read through RELEASE_PROGRESS_FILE this passes just as well when the
# scoping is gone and the events went to the live stream instead.
want_eq 'the three watches recorded their run here, not in the live stream' 3 \
	"$(jq -c 'select(.kind == "run" and .repo == "owner/repo" and .id == "42")' \
		"${WORK}/progress.jsonl" 2>/dev/null | grep -c . || true)"
want_eq 'and it is this suite'\''s status file that names that run' 42 \
	"$(jq -r '.runId' "${WORK}/status.json" 2>/dev/null || true)"

unset -f now sleep run_state e2e_job_ids arm

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
