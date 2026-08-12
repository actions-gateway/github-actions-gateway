#!/usr/bin/env bash
#
# Assertions for the release-validation status renderer (Q616).
#
# The status object is what an agent reads to tell an operator where an
# hour-long billable gate is, and the sentinel's every decision is derived from
# it. Both consequences of getting it wrong are silent: a failure attributed to
# the wrong phase sends the diagnosis in the wrong direction, and a torn line
# that kills the render leaves a running gate looking like one that never
# started.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

WORK="${REPO_ROOT}/tmp/release-status-test.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

# Point both renderer targets at the scratch dir BEFORE sourcing: the lib reads
# them at source time so a real run's files are never touched.
RELEASE_PROGRESS_FILE="${WORK}/progress.jsonl"
RELEASE_STATUS_FILE="${WORK}/status.json"
export RELEASE_PROGRESS_FILE RELEASE_STATUS_FILE
# shellcheck source=scripts/dogfood/lib/progress.sh
source "${REPO_ROOT}/scripts/dogfood/lib/progress.sh"

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

want_absent() {
	local name="$1" path="$2"
	if [[ -e "$path" ]]; then
		bad "$name" "$path exists"
	else
		ok "$name" "absent"
	fi
}

want_present() {
	local name="$1" path="$2"
	if [[ -e "$path" ]]; then
		ok "$name" "present"
	else
		bad "$name" "$path was removed"
	fi
}

# status_default_for [STREAM] — the RELEASE_STATUS_FILE a fresh source picks
# with only the stream set; with no argument, neither is set, which is the
# operator's own case. Runs in a subshell because the library defaults at source
# time and this suite already sourced it with both pointed at $WORK.
# shellcheck disable=SC2016 # the inner shell is what must expand these, not this one
status_default_for() {
	local lib="${REPO_ROOT}/scripts/dogfood/lib/progress.sh"
	if (($# == 0)); then
		env -u RELEASE_STATUS_FILE -u RELEASE_PROGRESS_FILE \
			bash -c 'source "$1"; printf %s "${RELEASE_STATUS_FILE}"' _ "$lib"
	else
		env -u RELEASE_STATUS_FILE RELEASE_PROGRESS_FILE="$1" \
			bash -c 'source "$1"; printf %s "${RELEASE_STATUS_FILE}"' _ "$lib"
	fi
}

# field NAME — one field of the current status object, rendered at a pinned now.
field() {
	RELEASE_STATUS_NOW="${NOW}" progress_status_json | jq -r --arg f "$1" '.[$f] // "null" | tostring'
}

# A gate's stream, written by hand at known timestamps so every elapsed number
# in the assertions is exact rather than approximately now.
NOW=2000
T0=1000

echo '== a gate that has not started reads as preflight, not as missing =='
want_eq 'no stream file: gate=preflight' preflight "$(field gate)"
want_eq 'no stream file: phase is null' null "$(field phase)"
want_eq 'no stream file: elapsed is null' null "$(field elapsed)"
: >"$RELEASE_PROGRESS_FILE"
want_eq 'empty stream: gate=preflight' preflight "$(field gate)"

echo
echo '== a running gate reports its phase, its RC, and both elapsed clocks =='
{
	echo "{\"kind\":\"phase\",\"t\":${T0},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 10)),\"phase\":\"deploy\",\"state\":\"start\",\"detail\":\"Deploying the RC\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 400)),\"phase\":\"deploy\",\"state\":\"done\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 401)),\"phase\":\"e2e\",\"state\":\"start\",\"detail\":\"Running the e2e matrix on GAG runners\"}"
} >"$RELEASE_PROGRESS_FILE"
want_eq 'gate=running' running "$(field gate)"
want_eq 'rc comes from the gate-start detail' v1.2.3-rc.4 "$(field rc)"
want_eq 'phase is the latest one' e2e "$(field phase)"
want_eq 'state is the latest one' start "$(field state)"
want_eq 'elapsed measures from gate start' 1000 "$(field elapsed)"
want_eq 'phaseElapsed measures from this phase' 599 "$(field phaseElapsed)"
want_eq 'idle measures from the last event' 599 "$(field idle)"
want_eq 'no heartbeat yet' null "$(field heartbeat)"

echo
echo '== the e2e heartbeat survives later phase events =='
# Without this the status would say "e2e, started 18 minutes ago" for the whole
# ~25-minute leg — the phase is the only thing that does not change in it.
echo "{\"kind\":\"heartbeat\",\"t\":$((T0 + 900)),\"text\":\"[e2e t+08:19] 31/73 specs | 29 ok\"}" >>"$RELEASE_PROGRESS_FILE"
want_eq 'heartbeat text' '[e2e t+08:19] 31/73 specs | 29 ok' "$(field heartbeat)"
want_eq 'heartbeatAge' 100 "$(field heartbeatAge)"
want_eq 'a heartbeat does not become the phase' e2e "$(field phase)"
echo "{\"kind\":\"phase\",\"t\":$((T0 + 950)),\"phase\":\"sizing\",\"state\":\"start\"}" >>"$RELEASE_PROGRESS_FILE"
want_eq 'the last heartbeat is still reported' '[e2e t+08:19] 31/73 specs | 29 ok' "$(field heartbeat)"
want_eq 'phase moved on' sizing "$(field phase)"

echo
echo '== a torn final line costs a field, never the object =='
# The gate appends while a reader may be mid-read; an unparseable line must not
# make a running gate look like one that never started.
printf '{"kind":"phase","t":1999,"phase":"crd-sm' >>"$RELEASE_PROGRESS_FILE"
want_eq 'still renders the events before the tear' sizing "$(field phase)"
want_eq 'still running' running "$(field gate)"

echo
echo '== a failed gate names the phase that broke, not the teardown consequence =='
{
	echo "{\"kind\":\"phase\",\"t\":${T0},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 5)),\"phase\":\"e2e\",\"state\":\"start\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 600)),\"phase\":\"e2e\",\"state\":\"fail\",\"detail\":\"run 42 did not conclude success\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 601)),\"phase\":\"gate\",\"state\":\"fail\",\"detail\":\"gate exited 1\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 602)),\"phase\":\"teardown\",\"state\":\"start\"}"
} >"$RELEASE_PROGRESS_FILE"
want_eq 'gate=failed' failed "$(field gate)"
want_eq 'failure names the first failing phase' \
	'e2e: run 42 did not conclude success' "$(field failure)"

echo
echo '== a passed gate stays passed while teardown runs =='
{
	echo "{\"kind\":\"phase\",\"t\":${T0},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 900)),\"phase\":\"gate\",\"state\":\"done\",\"detail\":\"validation PASSED\"}"
	echo "{\"kind\":\"phase\",\"t\":$((T0 + 901)),\"phase\":\"teardown\",\"state\":\"start\"}"
} >"$RELEASE_PROGRESS_FILE"
want_eq 'gate=passed' passed "$(field gate)"
want_eq 'phase still reports the teardown in flight' teardown "$(field phase)"
want_eq 'no failure' null "$(field failure)"

echo
echo '== the writers produce what the renderer reads =='
progress_init
want_eq 'progress_init leaves an empty stream' 0 "$(wc -l <"$RELEASE_PROGRESS_FILE" | tr -d ' ')"
want_eq 'progress_init writes a preflight status file' preflight \
	"$(jq -r '.gate' <"$RELEASE_STATUS_FILE")"
progress_event gate start 'v9.9.9-rc.1'
progress_event deploy start $'a "quoted" detail\nwith a newline'
want_eq 'the status file tracks the stream' deploy "$(jq -r '.phase' <"$RELEASE_STATUS_FILE")"
want_eq 'a detail with quotes and newlines stays one valid record' 2 \
	"$(jq -s 'length' <"$RELEASE_PROGRESS_FILE")"
want_eq 'rc round-trips through the writers' v9.9.9-rc.1 \
	"$(jq -r '.rc' <"$RELEASE_STATUS_FILE")"

echo
echo '== heartbeats are capped, and never conjure a stream =='
long="$(printf 'x%.0s' {1..500})"
RELEASE_HEARTBEAT_MAX_CHARS=200 progress_heartbeat "$long"
want_eq 'a pathological heartbeat is truncated' 200 \
	"$(jq -r '.heartbeat | length' <"$RELEASE_STATUS_FILE")"
# e2e-run-watch.sh also runs standalone as a recovery path; a heartbeat then
# must not leave a renderer looking at a gate that is not running.
rm -f "$RELEASE_PROGRESS_FILE"
progress_heartbeat '[e2e t+00:30] 1/73 specs'
want_absent 'no stream, no heartbeat file' "$RELEASE_PROGRESS_FILE"

echo
echo '== the status file defaults beside the stream, so scoping one scopes both =='
# Both defaults name paths a live gate is writing. A suite that scopes only
# RELEASE_PROGRESS_FILE would inherit the operator's real status file and
# progress_init would unlink it, so the default is derived from the stream
# rather than fixed (Q786). Every suite here scopes both already, which is why
# these run a fresh source instead of asserting on the sourced state: a test
# that reads this suite's own variables passes no matter what the library does.
want_eq 'a scoped stream scopes the status file' \
	"${WORK}/release-validation-status.json" "$(status_default_for "${WORK}/progress.jsonl")"
want_eq 'an empty stream leaves no status path to reach' '' "$(status_default_for '')"
# The unscoped path is the one release.md documents and validate-release.sh
# prints to the operator; deriving it must land on exactly that path.
want_eq 'the operator default is unchanged' \
	"${REPO_ROOT}/tmp/release-validation-status.json" "$(status_default_for)"

echo
echo '== disabling the stream disables the status file with it =='
saved_progress="$RELEASE_PROGRESS_FILE"
saved_status="$RELEASE_STATUS_FILE"
RELEASE_PROGRESS_FILE=""
RELEASE_STATUS_FILE="${WORK}/should-not-exist.json"
progress_init
progress_event gate start v0.0.0
want_absent 'no status file when the stream is off' "$RELEASE_STATUS_FILE"
# Absence alone cannot tell "never written" from "written then deleted", and it
# was the delete that reached the live default. So plant one first: with the
# stream off, progress_init must leave a status file it was told not to manage.
RELEASE_STATUS_FILE="${WORK}/opted-out.json"
echo '{"gate":"running"}' >"$RELEASE_STATUS_FILE"
progress_init
want_present 'progress_init does not unlink an opted-out status file' "$RELEASE_STATUS_FILE"
RELEASE_PROGRESS_FILE="$saved_progress"
RELEASE_STATUS_FILE="$saved_status"

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
