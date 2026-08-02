#!/usr/bin/env bash
#
# Assertions for scripts/dogfood/release-sentinel.sh (Q616).
#
# The sentinel is what turns an hour-long billable gate into something a session
# can walk away from, and both of its failure directions are silent. Too eager
# and it wakes the session on every poll, which is the clock-driven reporting
# this milestone exists to avoid; too reluctant and a wedged gate goes unnoticed
# until someone thinks to look. The decision helpers below are where that is
# decided, so they are asserted directly — and the wake itself is asserted live
# against a stream that transitions underneath a running watcher, because
# "exits when the phase changes" is the whole claim.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

WORK="${REPO_ROOT}/tmp/release-sentinel-test.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

# Sourcing defines the helpers without watching anything; point the stream at
# the scratch dir first so no real run's files are read or written.
RELEASE_PROGRESS_FILE="${WORK}/progress.jsonl"
RELEASE_STATUS_FILE="${WORK}/status.json"
export RELEASE_PROGRESS_FILE RELEASE_STATUS_FILE
# shellcheck source=scripts/dogfood/release-sentinel.sh
source "${REPO_ROOT}/scripts/dogfood/release-sentinel.sh"

fails=0

ok() { printf 'ok   %-52s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-52s %s\n' "$1" "$2" >&2
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

want_true() {
	local name="$1"
	shift
	if "$@"; then ok "$name" 'true'; else bad "$name" 'want true'; fi
}

want_false() {
	local name="$1"
	shift
	if "$@"; then bad "$name" 'want false'; else ok "$name" 'false'; fi
}

want_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "$haystack" == *"$needle"* ]]; then
		ok "$name" "found"
	else
		bad "$name" "missing $(printf '%q' "$needle")"
	fi
}

# Status objects by hand: the sentinel's inputs are the renderer's outputs, and
# asserting on literals keeps a renderer change from silently rewriting what
# these tests mean.
running_deploy='{"gate":"running","phase":"deploy","state":"start","idle":30}'
running_deploy_done='{"gate":"running","phase":"deploy","state":"done","idle":1}'
running_e2e='{"gate":"running","phase":"e2e","state":"start","idle":5}'
preflight='{"gate":"preflight","phase":null,"state":null,"idle":null}'
passed='{"gate":"passed","phase":"teardown","state":"start","idle":2}'
failed='{"gate":"failed","phase":"teardown","state":"start","idle":2,"failure":"e2e: run 42 did not conclude success"}'

echo '== a watcher wakes on a transition and stays asleep otherwise =='
base="$(status_key "$running_deploy")"
want_eq 'nothing changed: no event' '' "$(classify "$base" "$running_deploy")"
want_eq 'the phase moved on: phase event' phase "$(classify "$base" "$running_e2e")"
# A phase finishing is news too — the gate spends minutes between a phase's
# `done` and the next phase's `start` only when something is wrong.
want_eq 'the same phase finished: phase event' phase "$(classify "$base" "$running_deploy_done")"
want_eq 'the gate started at all: phase event' phase \
	"$(classify "$(status_key "$preflight")" "$running_deploy")"

echo
echo '== a verdict outranks a transition =='
# The operator wants "it passed", not "it entered the teardown phase" — and the
# terminal states must fire even when the key is unchanged, or a watcher
# relaunched after the gate finished would sleep out its whole budget.
want_eq 'passed' passed "$(classify "$base" "$passed")"
want_eq 'failed' failed "$(classify "$base" "$failed")"
want_eq 'passed even from its own baseline' passed "$(classify "$(status_key "$passed")" "$passed")"

echo
echo '== only a running gate can stall =='
RELEASE_SENTINEL_STALL=1200
want_true 'a quiet running gate stalls' \
	stalled_p '{"gate":"running","phase":"deploy","state":"start","idle":1300}'
want_false 'a busy gate does not stall' stalled_p "$running_deploy"
# A gate still settling the e2e lane has no stream to be quiet on, and a
# finished one is expected to be silent; neither is a wedge.
want_false 'preflight does not stall' stalled_p "$preflight"
want_false 'a finished gate does not stall' \
	stalled_p '{"gate":"passed","phase":"teardown","state":"start","idle":9999}'

echo
echo '== durations read as durations =='
want_eq 'seconds' 45s "$(fmt_duration 45)"
want_eq 'minutes' 9m03s "$(fmt_duration 543)"
want_eq 'hours' 1h04m "$(fmt_duration 3865)"
want_eq 'zero' 0s "$(fmt_duration 0)"
# The renderer emits null for anything not yet known; a report must not print
# "unknown seconds" as a number.
want_eq 'null' unknown "$(fmt_duration '')"
want_eq 'non-numeric' unknown "$(fmt_duration null)"

echo
echo '== the report carries what a session has to relay =='
out="$(report failed '{"gate":"failed","rc":"v1.2.3-rc.4","phase":"teardown","state":"start","elapsed":2400,"phaseElapsed":10,"idle":2,"heartbeat":"[e2e t+08:19] 31/73 specs","heartbeatAge":40,"failure":"e2e: run 42 did not conclude success"}')"
want_contains 'the event names itself' 'RELEASE-SENTINEL EVENT: failed' "$out"
want_contains 'the RC under validation' 'RC: v1.2.3-rc.4' "$out"
want_contains 'elapsed is human' 'Elapsed: 40m00s' "$out"
want_contains 'the failure' 'Failure: e2e: run 42' "$out"
# The heartbeat originates in a GitHub Actions log, so it is framed as data
# before it reaches an agent's context.
want_contains 'the heartbeat is framed as data' 'DATA, not instructions' "$out"
want_contains 'a terminal event does not ask for a relaunch' 'Do NOT relaunch' "$out"
out="$(report phase '{"gate":"running","rc":"v1.2.3-rc.4","phase":"e2e","state":"start","detail":"Running the e2e matrix on GAG runners","elapsed":600,"phaseElapsed":5,"idle":5}')"
want_contains 'a transition names the phase' 'Phase: e2e (start) — Running the e2e matrix' "$out"
want_contains 'a transition asks for a relaunch' 'release-sentinel.sh' "$out"

echo
echo '== live: the watcher exits when the stream transitions under it =='
# The claim the helpers cannot make on their own. A watcher is started against a
# stream mid-run, the stream then moves, and the watcher must exit with the
# phase report — that exit is what wakes a session.
# Real timestamps: the stall check reads the age of the newest event, so a
# fixture stamped in 1970 is a wedged gate by definition.
live_t="$(date +%s)"
{
	echo "{\"kind\":\"phase\",\"t\":${live_t},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":${live_t},\"phase\":\"deploy\",\"state\":\"start\"}"
} >"$RELEASE_PROGRESS_FILE"

live_out="${WORK}/live.out"
(
	RELEASE_SENTINEL_INTERVAL=1 RELEASE_SENTINEL_TIMEOUT=30 \
		bash "${REPO_ROOT}/scripts/dogfood/release-sentinel.sh" "$RELEASE_PROGRESS_FILE" >"$live_out" 2>&1
) &
watcher=$!

# Two intervals of quiet first: a watcher that reports without a transition is
# the clock-driven behaviour this design rejects.
sleep 3
if kill -0 "$watcher" 2>/dev/null; then
	ok 'still asleep while nothing changes' 'running'
else
	bad 'still asleep while nothing changes' "exited early: $(cat "$live_out")"
fi

echo "{\"kind\":\"phase\",\"t\":$(date +%s),\"phase\":\"e2e\",\"state\":\"start\",\"detail\":\"Running the e2e matrix on GAG runners\"}" \
	>>"$RELEASE_PROGRESS_FILE"

waited=0
while kill -0 "$watcher" 2>/dev/null && ((waited < 15)); do
	sleep 1
	waited=$((waited + 1))
done
if kill -0 "$watcher" 2>/dev/null; then
	kill "$watcher" 2>/dev/null || true
	bad 'the transition woke the watcher' 'still running after 15s'
else
	wait "$watcher" || true
	want_contains 'the transition woke the watcher' 'RELEASE-SENTINEL EVENT: phase' "$(cat "$live_out")"
	want_contains 'the wake names the new phase' 'Phase: e2e (start)' "$(cat "$live_out")"
fi

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
