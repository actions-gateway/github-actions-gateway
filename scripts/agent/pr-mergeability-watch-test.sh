#!/usr/bin/env bash
#
# Unit tests for scripts/agent/pr-mergeability-watch.sh.
#
# This watcher is the only thing covering a handed-off PR, so both directions
# fail silently and both are asserted: it must exit on the states that need the
# dispatcher (DIRTY, BEHIND, a closed PR), and it must NOT exit on the states
# that do not (CLEAN, BLOCKED, and above all UNKNOWN, which GitHub reports while
# it computes mergeability asynchronously — waking on that would fire for every
# freshly pushed PR).
#
# `gh` and `sleep` are stubbed. `sleep` records a tick and returns at once, and
# the script derives its budget from the seconds it asked to sleep rather than
# from a wall clock, so the timeout case is deterministic and there is no second
# timebase to race (testing.md#two-clocks-in-one-assertion). Each case is bounded
# by the subject's own configured budget rather than a fixed wall-clock deadline,
# which is what made the backstops in Q752 and Q642 flake.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
WATCHER="$REPO_ROOT/scripts/agent/pr-mergeability-watch.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/pr-mergeability-test.$$"
mkdir -p "$FIXTURE_DIR/bin"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

# A `gh` stub that walks a scripted sequence of "STATE MERGESTATE" replies, one
# per call, and repeats the last one forever. GH_FAIL_FIRST makes the leading N
# calls fail, so a transient error is distinguishable from a persistent one.
cat >"$FIXTURE_DIR/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
count_file="${GH_COUNT_FILE}"
n=$(<"$count_file")
n=$((n + 1))
printf '%s' "$n" >"$count_file"
if ((n <= ${GH_FAIL_FIRST:-0})); then
	printf 'gh: transient failure %s\n' "$n" >&2
	exit 1
fi
idx=$((n - ${GH_FAIL_FIRST:-0}))
IFS='|' read -r -a replies <<<"${GH_REPLIES}"
if ((idx > ${#replies[@]})); then
	idx=${#replies[@]}
fi
printf '%s\n' "${replies[$((idx - 1))]}"
STUB
chmod +x "$FIXTURE_DIR/bin/gh"

# A `sleep` stub that returns immediately and records one tick per call, so a
# case can assert the watcher actually paced itself instead of spinning.
cat >"$FIXTURE_DIR/bin/sleep" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"${SLEEP_LOG}"
STUB
chmod +x "$FIXTURE_DIR/bin/sleep"
export PATH="$FIXTURE_DIR/bin:$PATH"

fails=0
LAST_OUT=""
LAST_RC=0

# run_watch REPLIES [FAIL_FIRST] — run the watcher against a scripted gh.
# Every case sets a small budget and interval, so a watcher that failed to exit
# terminates on its own timeout rather than hanging the suite.
run_watch() {
	local replies="$1" fail_first="${2:-0}"
	: >"$FIXTURE_DIR/sleeps"
	printf '0' >"$FIXTURE_DIR/count"
	set +e
	LAST_OUT=$(
		GH_REPLIES="$replies" \
			GH_FAIL_FIRST="$fail_first" \
			GH_COUNT_FILE="$FIXTURE_DIR/count" \
			SLEEP_LOG="$FIXTURE_DIR/sleeps" \
			PR_MERGEABILITY_INTERVAL=10 \
			PR_MERGEABILITY_TIMEOUT=100 \
			"$WATCHER" 42 2>&1
	)
	LAST_RC=$?
	set -e
}

expect_event() {
	local want="$1" desc="$2"
	if [[ "$LAST_OUT" == *"event: ${want}"* ]] && ((LAST_RC == 0)); then
		echo "PASS: $desc"
	else
		echo "FAIL: $desc — wanted event ${want} at rc 0, got rc ${LAST_RC}:"
		echo "$LAST_OUT"
		fails=$((fails + 1))
	fi
}

# expect_usage_error ARGS... — a mistyped launch must be distinguishable from an
# event, because a dispatcher drops the PR from its tracker on either.
expect_usage_error() {
	local desc="$1"
	shift
	set +e
	LAST_OUT=$("$WATCHER" "$@" 2>&1)
	LAST_RC=$?
	set -e
	if ((LAST_RC == 2)) && [[ "$LAST_OUT" != *"event:"* ]]; then
		echo "PASS: $desc"
	else
		echo "FAIL: $desc — wanted rc 2 and no event, got rc ${LAST_RC}: ${LAST_OUT}"
		fails=$((fails + 1))
	fi
}

# --- exits when the dispatcher is needed ------------------------------------

run_watch "OPEN DIRTY"
expect_event conflict "DIRTY wakes the dispatcher"

run_watch "OPEN BEHIND"
expect_event conflict "BEHIND wakes the dispatcher"

run_watch "MERGED CLEAN"
expect_event closed "a merged PR ends the watch"

run_watch "CLOSED CLEAN"
expect_event closed "a closed PR ends the watch"

# The transition is the point: a PR that is fine now and dirtied by a sibling
# merge later is the entire reason this watcher exists.
run_watch "OPEN CLEAN|OPEN CLEAN|OPEN DIRTY"
expect_event conflict "a CLEAN PR that later goes DIRTY wakes the dispatcher"

# --- does NOT exit when it is not ------------------------------------------

run_watch "OPEN UNKNOWN"
expect_event timeout "UNKNOWN is never treated as a conflict"

run_watch "OPEN CLEAN"
expect_event timeout "a CLEAN PR is watched, not reported"

run_watch "OPEN BLOCKED"
expect_event timeout "BLOCKED is a check state, not a merge conflict"

# --- pacing and failure handling -------------------------------------------

run_watch "OPEN CLEAN"
if [[ -s "$FIXTURE_DIR/sleeps" ]]; then
	echo "PASS: the watcher sleeps between polls"
else
	echo "FAIL: the watcher never slept — it spun"
	fails=$((fails + 1))
fi

# A transient gh failure must not be read as an answer about the PR.
run_watch "OPEN DIRTY" 2
expect_event conflict "a transient gh failure is retried, not reported"

# A persistent one must stop rather than poll a broken command to the budget.
run_watch "OPEN CLEAN" 99
expect_event error "a persistently failing gh reports error"

# --- usage errors are not events -------------------------------------------

expect_usage_error "a missing PR number is a usage error, not an event"
expect_usage_error "a non-numeric PR number is a usage error, not an event" not-a-number

if ((fails > 0)); then
	echo "pr-mergeability-watch-test: ${fails} failure(s)"
	exit 1
fi
echo "pr-mergeability-watch-test: ok"
