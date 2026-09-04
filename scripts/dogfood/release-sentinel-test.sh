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
shopt -s inherit_errexit

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
echo '== quiet is only a wedge once the run agrees (Q630) =='
# The failure this reconciliation exists for. Through the e2e leg the stream's
# only writer is the relayed spec heartbeat, which needs a fetchable job log;
# GitHub served BlobNotFound for the whole of one 30-minute run that PASSED. The
# stall threshold (1200s) is shorter than a healthy leg (25-33 min), so with the
# log unreadable the stream is silent past the threshold every single time — a
# false stall was certain, not merely possible.
quiet_e2e='{"gate":"running","phase":"e2e","state":"start","idle":1500,"updatedAt":1000,"runRepo":"owner/repo","runId":"42"}'

# Positive control on the OLD predicate: the fixture must be one the pre-Q630
# detector reported, or the assertions below prove nothing. This is verbatim
# what stalled_p was before the run-status check was added.
old_stalled_p() {
	local json="$1" idle
	[[ "$(status_field "$json" gate)" == "running" ]] || return 1
	idle="$(status_field "$json" idle)"
	[[ -n "$idle" ]] || return 1
	((idle >= RELEASE_SENTINEL_STALL))
}
want_true 'the old detector did report this quiet' old_stalled_p "$quiet_e2e"

# gh answers from the run record, which is a different endpoint from the job log
# and survives the log being unservable. One stub answering from a variable
# rather than a redefinition per case: the answer is read in the subshell
# `waiting_on_live_run_p` runs it in, which is fine — only writes would be lost
# there. An empty answer is gh failing to answer at all.
fake_run_status=''
gh() {
	[[ -n "$fake_run_status" ]] || return 1
	echo "$fake_run_status"
}

# The seam itself. Everything below reaches gh through this one call.
fake_run_status=in_progress
want_eq 'the run status comes from the run record' in_progress \
	"$(sentinel_run_status owner/repo 42)"

want_false 'an unreadable log on a progressing run is not a stall' stalled_p "$quiet_e2e"
want_true 'and the run reads as live' waiting_on_live_run_p "$quiet_e2e"

# The control: the same silence, on a run that is over. The gate should have
# moved on and did not, so this one is real news.
fake_run_status=completed
want_true 'the same quiet on a finished run still stalls' stalled_p "$quiet_e2e"
want_false 'and the run does not read as live' waiting_on_live_run_p "$quiet_e2e"

# A phase with no run to consult — deploy, teardown — keeps the original
# meaning: nothing is fetching anything, so quiet is the gate's own. The stub is
# armed with the one answer that would suppress a stall, so this passing is what
# proves no run was consulted.
fake_run_status=in_progress
want_true 'quiet with no run reference still stalls' \
	stalled_p '{"gate":"running","phase":"deploy","state":"start","idle":1300,"updatedAt":1000}'

# Unaskable is not evidence. This detector's expensive failure is crying wolf,
# and a gate truly stuck on a run that never concludes is caught by
# e2e-run-watch.sh's own deadline (Q629) failing the gate instead.
fake_run_status=''
want_false 'an unreachable gh does not manufacture a stall' stalled_p "$quiet_e2e"
fake_run_status=some_future_status
want_false 'a status GitHub adds later is not a wedge' stalled_p "$quiet_e2e"
unset -f gh

echo
echo '== a reported stall is not reported again (Q630) =='
# A stall does not clear on its own: idle is still over the threshold when a
# relaunched watcher looks, and the check runs before the first sleep. Without
# memory every relaunch exits instantly and the session spins.
SENTINEL_STREAM="${WORK}/memory.jsonl"
rm -f "$(stall_marker)"
want_false 'nothing remembered yet' stall_reported_p "$quiet_e2e"
remember_stall "$quiet_e2e"
want_true 'the same quiet is suppressed on relaunch' stall_reported_p "$quiet_e2e"

# Re-arms when the stream moves: a new event means a different quiet.
want_false 'a quiet that follows new events is a new stall' \
	stall_reported_p '{"gate":"running","phase":"e2e","state":"start","idle":1300,"updatedAt":2000}'
# ...and when the same quiet has deepened by another full threshold, so memory
# silences a repeat rather than a fact.
want_false 'a quiet deepened by another threshold is worth repeating' \
	stall_reported_p '{"gate":"running","phase":"e2e","state":"start","idle":2700,"updatedAt":1000}'
want_true 'but not before that' \
	stall_reported_p '{"gate":"running","phase":"e2e","state":"start","idle":2699,"updatedAt":1000}'
rm -f "$(stall_marker)"
SENTINEL_STREAM="${RELEASE_PROGRESS_FILE}"

echo
echo '== the run reference reaches the status object =='
# The sentinel can only consult a run it knows about, and the relay is what
# tells it. A reference must not conjure a stream for a gate that is not running.
run_stream="${WORK}/runref.jsonl"
RELEASE_PROGRESS_FILE="$run_stream" progress_run owner/repo 42
want_eq 'no stream, no run reference' '' "$([[ -e "$run_stream" ]] && echo exists)"
echo '{"kind":"phase","t":1000,"phase":"gate","state":"start","detail":"v1.2.3-rc.4"}' >"$run_stream"
RELEASE_PROGRESS_FILE="$run_stream" RELEASE_STATUS_FILE='' progress_run owner/repo 42
run_json="$(progress_status_json "$run_stream")"
want_eq 'the repo is carried' owner/repo "$(status_field "$run_json" runRepo)"
want_eq 'the run id is carried' 42 "$(status_field "$run_json" runId)"
want_contains 'and the report names the run' \
	'https://github.com/owner/repo/actions/runs/42' "$(report phase "$run_json")"

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
# The claims the helpers cannot make on their own: a watcher started against a
# stream mid-run must exit with the phase report when the stream moves, and must
# stay asleep when it does not.
#
# Nothing below bounds real seconds. The watcher reads the clock through `date`
# and paces itself through `sleep`, both PATH commands, so stubbing the two
# gives every case a virtual clock that advances only when the watcher sleeps.
# "Still asleep" becomes a count of the watcher's own polls, and the transition
# lands on a poll rather than after a wall-clock guess.
#
# The two clocks are what made this suite fail only under a loaded `make check`:
# the window was `sleep`, a timer, while the watch budget was `date +%s`, wall
# clock. Any forward step of the clock between them retires a 60s budget inside
# a 3s window, and the watcher reports `timeout` before the window is up (Q690,
# and testing.md § Two clocks in one assertion).
mkdir -p "${WORK}/bin" "${WORK}/hooks"

# Resolved before ${WORK}/bin goes on PATH, so the stub can still reach the real
# thing for the formats it does not serve.
SENTINEL_REAL_DATE="$(command -v date)"
SENTINEL_TICKS="${WORK}/ticks"
SENTINEL_ELAPSED="${WORK}/elapsed"
# The fixtures below are stamped relative to real now, so the virtual clock
# starts there and their ages mean what they say.
SENTINEL_BASE="$(date +%s)"
export SENTINEL_REAL_DATE SENTINEL_TICKS SENTINEL_ELAPSED SENTINEL_BASE

cat >"${WORK}/bin/sleep" <<'STUB'
#!/usr/bin/env bash
# Records the poll, advances the virtual clock by the interval the watcher asked
# for, runs the case's hook, then caps the loop. Both outcomes terminate through
# the watcher's own control flow, so neither end needs a deadline. The cap kills
# with SIGKILL, so a watcher that should have reported and did not still exits
# non-zero rather than passing as a clean exit.
printf '%s\n' "${1-}" >>"$SENTINEL_TICKS"
ticks=$(($(wc -l <"$SENTINEL_TICKS")))
elapsed="$(cat "$SENTINEL_ELAPSED" 2>/dev/null || true)"
printf '%s\n' "$((${elapsed:-0} + ${1-0}))" >"$SENTINEL_ELAPSED"
[[ -n "${SENTINEL_TICK_HOOK:-}" ]] && "$SENTINEL_TICK_HOOK" "$ticks"
((ticks >= SENTINEL_MAX_TICKS)) && kill -KILL "$PPID"
exit 0
STUB
chmod +x "${WORK}/bin/sleep"

cat >"${WORK}/bin/date" <<'STUB'
#!/usr/bin/env bash
# The virtual clock: the base plus everything the watcher has slept.
if [[ "${1-}" == '+%s' ]]; then
	elapsed="$(cat "$SENTINEL_ELAPSED" 2>/dev/null || true)"
	printf '%s\n' "$((SENTINEL_BASE + ${elapsed:-0}))"
	exit 0
fi
exec "$SENTINEL_REAL_DATE" "$@"
STUB
chmod +x "${WORK}/bin/date"

# `gh` is shimmed onto PATH from here down — these tests never reach GitHub.
fake_gh() {
	printf '#!/usr/bin/env bash\necho %s\n' "$1" >"${WORK}/bin/gh"
	chmod +x "${WORK}/bin/gh"
}

# `watcher_rc` is deliberately left unguarded by die_if_killed (Q1023, Q1055):
# the stub `sleep` caps the loop with SIGKILL, so 137 is how a watcher that
# should have reported and did not fails these cases. A guard would report that
# as KILLED rather than as the failure it is.
#
# run_watcher STREAM OUT — run a watcher to completion. In the foreground, and
# with no pid to track: the watcher either reports and returns, or its own
# capped poll kills it, so nothing here has to decide when to stop waiting.
# Leaves the exit status in `watcher_rc` and the polls it took in
# `watcher_ticks`.
run_watcher() {
	local stream="$1" out="$2"
	: >"$SENTINEL_TICKS"
	echo 0 >"$SENTINEL_ELAPSED"
	watcher_rc=0
	# Whichever shell reaps the capped poll's SIGKILL prints "Killed: 9" to its
	# own stderr. The subshell below is that shell — it holds two commands, so
	# bash cannot exec-optimise it away and leave the notice to this one — and
	# its stderr is discarded. Only the notice is: the watcher's own stderr is
	# already merged into $out, and 137 still reaches `watcher_rc`, which is what
	# tells a silenced watcher from a clean exit.
	(
		rc=0
		PATH="${WORK}/bin:${PATH}" RELEASE_SENTINEL_INTERVAL=1 \
			RELEASE_SENTINEL_TIMEOUT="${sentinel_budget}" RELEASE_SENTINEL_STALL=1200 \
			bash "${REPO_ROOT}/scripts/dogfood/release-sentinel.sh" "$stream" >"$out" 2>&1 || rc=$?
		exit "$rc"
	) 2>/dev/null || watcher_rc=$?
	watcher_ticks=$(($(wc -l <"$SENTINEL_TICKS")))
}

# Defaults every case inherits; a case that means something else says so.
sentinel_budget=60
export SENTINEL_MAX_TICKS=3
export SENTINEL_TICK_HOOK=''

# Real timestamps: the stall check reads the age of the newest event, so a
# fixture stamped in 1970 is a wedged gate by definition.
live_t="$SENTINEL_BASE"
{
	echo "{\"kind\":\"phase\",\"t\":${live_t},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":${live_t},\"phase\":\"deploy\",\"state\":\"start\"}"
} >"$RELEASE_PROGRESS_FILE"

fake_gh in_progress
live_out="${WORK}/live.out"
run_watcher "$RELEASE_PROGRESS_FILE" "$live_out"
# A watcher that reports without a transition is the clock-driven behaviour this
# design rejects, so the claim is that three of its own polls produced nothing.
if ((watcher_ticks == 3)) && [[ ! -s "$live_out" ]]; then
	ok 'still asleep while nothing changes' 'three polls, no report'
else
	bad 'still asleep while nothing changes' \
		"polls $watcher_ticks output $(printf '%q' "$(cat "$live_out")")"
fi

# The transition lands on the watcher's first poll, which is the synchronisation
# this case needs: strictly after the baseline, strictly before the next read.
cat >"${WORK}/hooks/transition" <<HOOK
#!/usr/bin/env bash
(( \$1 == 1 )) || exit 0
echo '{"kind":"phase","t":${live_t},"phase":"e2e","state":"start","detail":"Running the e2e matrix on GAG runners"}' \\
	>>"${RELEASE_PROGRESS_FILE}"
HOOK
chmod +x "${WORK}/hooks/transition"

SENTINEL_TICK_HOOK="${WORK}/hooks/transition"
run_watcher "$RELEASE_PROGRESS_FILE" "$live_out"
SENTINEL_TICK_HOOK=''
if ((watcher_rc == 0)); then
	want_contains 'the transition woke the watcher' 'RELEASE-SENTINEL EVENT: phase' "$(cat "$live_out")"
	want_contains 'the wake names the new phase' 'Phase: e2e (start)' "$(cat "$live_out")"
else
	bad 'the transition woke the watcher' "exit $watcher_rc after $watcher_ticks poll(s)"
fi

echo
echo '== live: an unreadable log does not wake the watcher, a finished run does =='
# The helpers above decide it; this asserts the watcher is actually wired to
# them, which is where Q630 went wrong.

# A gate 25 minutes into the e2e leg with nothing relayed: exactly the shape a
# BlobNotFound log leaves behind.
quiet_t=$((SENTINEL_BASE - 1500))
live_quiet_stream="${WORK}/live-quiet.jsonl"
{
	echo "{\"kind\":\"phase\",\"t\":${quiet_t},\"phase\":\"gate\",\"state\":\"start\",\"detail\":\"v1.2.3-rc.4\"}"
	echo "{\"kind\":\"phase\",\"t\":${quiet_t},\"phase\":\"e2e\",\"state\":\"start\"}"
	echo "{\"kind\":\"run\",\"t\":${quiet_t},\"repo\":\"owner/repo\",\"id\":\"42\"}"
} >"$live_quiet_stream"
live_dead_stream="${WORK}/live-dead.jsonl"
cp "$live_quiet_stream" "$live_dead_stream"

fake_gh in_progress
live_out="${WORK}/live-quiet.out"
run_watcher "$live_quiet_stream" "$live_out"
if ((watcher_ticks == 3)) && [[ ! -s "$live_out" ]]; then
	ok 'a 25-minute silence on a live run does not wake it' 'three polls, no report'
else
	bad 'a 25-minute silence on a live run does not wake it' \
		"polls $watcher_ticks output $(printf '%q' "$(cat "$live_out")")"
fi

# The control. Same silence, same threshold, run over: this one must wake. The
# stall is evaluated before the first sleep, so it reports without polling.
fake_gh completed
live_out="${WORK}/live-dead.out"
run_watcher "$live_dead_stream" "$live_out"
if ((watcher_rc == 0)); then
	want_contains 'the same silence on a finished run wakes it' \
		'RELEASE-SENTINEL EVENT: stalled' "$(cat "$live_out")"
else
	bad 'the same silence on a finished run wakes it' \
		"exit $watcher_rc after $watcher_ticks poll(s)"
fi

# And the second bug: relaunched against the identical quiet, it must not fire
# again the instant it starts. The marker the report above wrote is the memory.
live_out="${WORK}/live-refire.out"
run_watcher "$live_dead_stream" "$live_out"
if ((watcher_ticks == 3)) && [[ ! -s "$live_out" ]]; then
	ok 'a relaunched watcher does not repeat the stall' 'three polls, no report'
else
	bad 'a relaunched watcher does not repeat the stall' \
		"polls $watcher_ticks output $(printf '%q' "$(cat "$live_out")")"
fi

echo
echo '== the budget is spent by the watch, not by the wall clock =='
# The control for the stubbed clock above: with it frozen, every "still asleep"
# case would pass whatever the watcher did, so one case has to spend the budget
# and reach the timeout. Two polls of a 2s budget retire it exactly. The run
# reads as live again, or the quiet stalls before the budget is ever spent.
fake_gh in_progress
sentinel_budget=2
SENTINEL_MAX_TICKS=10
live_out="${WORK}/live-budget.out"
run_watcher "$live_quiet_stream" "$live_out"
sentinel_budget=60
SENTINEL_MAX_TICKS=3
if ((watcher_rc == 0)) && ((watcher_ticks == 2)); then
	want_contains 'a spent budget reports a timeout' \
		'RELEASE-SENTINEL EVENT: timeout' "$(cat "$live_out")"
else
	bad 'a spent budget reports a timeout' \
		"exit $watcher_rc after $watcher_ticks poll(s): $(printf '%q' "$(cat "$live_out")")"
fi

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
