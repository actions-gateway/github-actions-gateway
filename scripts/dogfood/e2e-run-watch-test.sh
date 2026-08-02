#!/usr/bin/env bash
#
# Assertions for scripts/dogfood/e2e-run-watch.sh's relay helpers (Q615).
#
# This watcher decides whether an hour-long billable release gate passes, and it
# is the operator's only live view of a run happening on someone else's machine.
# The three helpers below are where that can go quietly wrong: a conclusion
# mapped to the wrong exit status passes a red release, and a broken
# already-seen calculation either replays the whole heartbeat every poll or
# stops relaying it entirely.
#
# The log fixture is real: lines copied verbatim from run 30751971883, timestamp
# prefixes and all, so the filter is asserted against what GitHub actually
# serves rather than an idealized shape.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
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
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
