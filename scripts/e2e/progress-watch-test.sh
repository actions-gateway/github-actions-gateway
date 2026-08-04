#!/usr/bin/env bash
#
# Assertions for scripts/e2e/progress-watch.sh's heartbeat rendering (Q608).
#
# The heartbeat is the only live signal a reviewer has during a 25-minute e2e
# run, so a wrong line is worse than no line: "31/73, running: X" that omits a
# stuck spec reads as healthy progress. The cases below pin the parts that are
# easy to get subtly wrong — which states count as failures, which process is
# still running, and the silence before the suite's total arrives.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/e2e/progress-watch.sh
source "$REPO_ROOT/scripts/e2e/progress-watch.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0

ok() { printf 'ok   %-46s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-46s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# expect_line NAME NOW WANT <<< events — render the heredoc events at NOW and
# compare the whole line.
expect_line() {
	local name="$1" now="$2" want="$3" got
	cat >"$WORK/events.jsonl"
	got=$(render_progress "$WORK/events.jsonl" "$now")
	if [[ "$got" == "$want" ]]; then
		ok "$name" "$got"
	else
		bad "$name" "want $(printf '%q' "$want") got $(printf '%q' "$got")"
	fi
}

# expect_contains NAME NOW SUBSTRING <<< events
expect_contains() {
	local name="$1" now="$2" want="$3" got
	cat >"$WORK/events.jsonl"
	got=$(render_progress "$WORK/events.jsonl" "$now")
	if [[ "$got" == *"$want"* ]]; then
		ok "$name" "found $(printf '%q' "$want")"
	else
		bad "$name" "want substring $(printf '%q' "$want") in $(printf '%q' "$got")"
	fi
}

# expect_silent NAME NOW <<< events
expect_silent() {
	local name="$1" now="$2" got
	cat >"$WORK/events.jsonl"
	got=$(render_progress "$WORK/events.jsonl" "$now")
	if [[ -z "$got" ]]; then
		ok "$name" 'no output'
	else
		bad "$name" "should have printed nothing, got $(printf '%q' "$got")"
	fi
}

echo '== the heartbeat stays silent until it has a denominator =='
# The cluster bring-up in SynchronizedBeforeSuite runs for minutes before the
# first spec. Emitting "0/0" lines through it would train the reader to ignore
# the heartbeat exactly when it starts mattering.
expect_silent 'no events at all' 1000 </dev/null
expect_silent 'events but no total' 1100 <<-'EOF'
	{"kind":"start","t":1010,"proc":1,"spec":"a"}
EOF

echo
echo '== counts split by terminal state =='
# Ginkgo has five distinct failure states. Counting only "failed" would report a
# timed-out spec as neither passed nor failed and silently shrink the total.
expect_line 'every failure state counts as failed' 1105 \
	'[e2e t+1:45] 6/6 specs | 1 ok, 4 failed, 1 skipped | running: none' <<-'EOF'
	{"kind":"total","t":1000,"total":6}
	{"kind":"end","t":1010,"proc":1,"spec":"a","state":"failed","secs":10}
	{"kind":"end","t":1020,"proc":2,"spec":"b","state":"timedout","secs":20}
	{"kind":"end","t":1025,"proc":5,"spec":"e","state":"panicked","secs":5}
	{"kind":"end","t":1028,"proc":6,"spec":"f","state":"interrupted","secs":3}
	{"kind":"end","t":1030,"proc":3,"spec":"c","state":"skipped","secs":0}
	{"kind":"end","t":1040,"proc":4,"spec":"d","state":"passed","secs":40}
EOF

# Pending and skipped are different Ginkgo states but the same thing to a
# reader: a spec that did not run and is not a failure.
expect_contains 'pending counts as skipped, not failed' 1050 '0 ok, 0 failed, 1 skipped' <<-'EOF'
	{"kind":"total","t":1000,"total":1}
	{"kind":"end","t":1010,"proc":1,"spec":"a","state":"pending","secs":0}
EOF

echo
echo '== a process is running whatever it started and has not ended =='
# Each parallel process runs one spec at a time, so its LAST event decides:
# proc 1 has moved on to a second spec, proc 2 is still in its first, proc 3 is
# idle between specs. Reading "started but not ended" globally instead would
# leave proc 1's finished spec on the running list forever.
expect_line 'trailing start runs, trailing end does not' 1252 \
	'[e2e t+4:12] 2/73 specs | 1 ok, 0 failed, 1 skipped | running: isolation denies cross-tenant curl (2m30s), drain replaces the worker (4m00s)' <<-'EOF'
	{"kind":"total","t":1000,"total":73}
	{"kind":"start","t":1010,"proc":1,"spec":"provisioning creates the namespace"}
	{"kind":"start","t":1012,"proc":2,"spec":"drain replaces the worker"}
	{"kind":"end","t":1100,"proc":1,"spec":"provisioning creates the namespace","state":"passed","secs":90}
	{"kind":"start","t":1102,"proc":1,"spec":"isolation denies cross-tenant curl"}
	{"kind":"end","t":1150,"proc":3,"spec":"multi-node only","state":"skipped","secs":0}
EOF

echo
echo '== long spec text is clipped so one line stays one line =='
# Real spec text runs past 200 chars (container hierarchy + leaf). Six of them
# unclipped would wrap the heartbeat into a paragraph.
expect_contains 'spec text clipped to the configured width' 1010 \
	'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa... (10s)' <<-EOF
	{"kind":"total","t":1000,"total":1}
	{"kind":"start","t":1000,"proc":1,"spec":"$(printf 'a%.0s' {1..120})"}
EOF

echo
echo '== a torn read skips the tick instead of killing the watcher =='
# The suite appends while this reads. An unparseable tail must not take the
# watcher down — losing the heartbeat for the rest of the run is the one
# outcome worse than a missed line.
expect_silent 'invalid JSON does not fail' 1010 <<-'EOF'
	{"kind":"total","t":1000,"total":1}
	{"kind":"start","t":1000,"proc":1,"spec":"trunc
EOF

echo
echo '== interval 0 turns the watcher off =='
# TEST_PROGRESS_INTERVAL is shared with the unit tier's renderer, where 0 means
# "no progress reporting". Without the guard in main() the same value would make
# this watcher `sleep 0` in a tight loop for the length of an e2e run.
#
# main() sleeps only inside that loop, so a stub `sleep` ahead of the real one
# on PATH records exactly the ticks the guard is meant to prevent, and kills the
# watcher at the first. Off is zero ticks; a regression fails on the tick it
# took rather than spinning for the length of the gate. Nothing here bounds real
# seconds: a deadline around the watcher measures the scheduler, which is what
# failed this case only under a loaded `make check` (Q642, and testing.md
# § The clock is as often a deadline around a process as a `sleep` inside one).
mkdir -p "$WORK/bin"
cat >"$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "${1-}" >>"$PROGRESS_WATCH_TICKS"
kill -KILL "$PPID"
STUB
chmod +x "$WORK/bin/sleep"
: >"$WORK/ticks"

off_rc=0
PATH="$WORK/bin:$PATH" PROGRESS_WATCH_TICKS="$WORK/ticks" TEST_PROGRESS_INTERVAL=0 \
	"$REPO_ROOT/scripts/e2e/progress-watch.sh" >"$WORK/off.log" 2>&1 || off_rc=$?
mapfile -t ticks <"$WORK/ticks"

if ((${#ticks[@]} == 0)); then
	ok 'interval 0 never reaches a tick' 'sleep was not called'
else
	bad 'interval 0 never reaches a tick' \
		"slept ${#ticks[@]} time(s), first for $(printf '%q' "${ticks[0]}")s"
fi

# Exiting and exiting quietly are separate properties: the guard could return
# early and still have printed, and a killed watcher reports 128+SIGKILL here
# rather than passing as a clean exit.
if ((off_rc == 0)) && [[ ! -s "$WORK/off.log" ]]; then
	ok 'interval 0 exits 0 without output' 'exited 0, printed nothing'
else
	bad 'interval 0 exits 0 without output' \
		"exit $off_rc output $(printf '%q' "$(cat "$WORK/off.log")")"
fi

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
