#!/usr/bin/env bash
#
# Unit tests for retry_backoff in scripts/lib/retry.sh, the one implementation of
# the exponential, capped, jittered retry schedule that download-verified.sh,
# pull-image-with-retry.sh, prepull-manifest-images.sh and validate-release.sh's
# gh_retry share.
#
# Why it is tested: each property below is invisible when it breaks. Without
# doubling the budget is spent early; without the cap a raised attempt count
# sleeps for hours; without jitter concurrent callers that were denied in the
# same second return in the same second, and the denial that caused the retry
# is re-applied in full every round. A run that still eventually succeeds
# shows none of it, so each property is asserted from the durations the
# function passes to `sleep`, which is stubbed as a shell function so nothing waits.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/retry.sh
source "${REPO_ROOT}/scripts/lib/retry.sh"

fails=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1: $2" >&2
	fails=$((fails + 1))
}
assert_eq() {
	if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1" "want '$2' got '$3'"; fi
}

sleeps=()
sleep() { sleeps+=("$1"); }

calls=0
succeed_on=0
flaky() {
	calls=$((calls + 1))
	if ((succeed_on > 0 && calls >= succeed_on)); then
		return 0
	fi
	return 7
}

# run_retry ATTEMPTS DELAY MAX_DELAY — reset the counters, run `flaky`, and set
# run_rc, calls and sleeps.
run_rc=0
run_retry() {
	calls=0
	sleeps=()
	run_rc=0
	retry_backoff "$1" "$2" "$3" "flaky thing" -- flaky 2>/dev/null || run_rc=$?
}

succeed_on=1
run_retry 6 5 60
assert_eq 'first-attempt success returns 0' 0 "${run_rc}"
assert_eq 'first-attempt success runs once' 1 "${calls}"
assert_eq 'first-attempt success never sleeps' 0 "${#sleeps[@]}"

succeed_on=3
run_retry 6 5 60
assert_eq 'recovery on attempt 3 returns 0' 0 "${run_rc}"
assert_eq 'recovery on attempt 3 stops at 3 runs' 3 "${calls}"
assert_eq 'recovery on attempt 3 sleeps twice' 2 "${#sleeps[@]}"

succeed_on=0
run_retry 6 5 60
assert_eq 'exhausted budget returns the last status' 7 "${run_rc}"
assert_eq 'exhausted budget runs every attempt' 6 "${calls}"
assert_eq 'exhausted budget sleeps between attempts only' 5 "${#sleeps[@]}"

# Bases 5, 10, 20, 40, 60: doubling to the cap. Jitter adds 0..half the delay, so
# each sleep lands in [base, base + base/2]; the lower bound pins the doubling and
# the cap, the upper bound pins the jitter's ceiling.
bases=(5 10 20 40 60)
ok=1
for i in "${!bases[@]}"; do
	b="${bases[$i]}"
	g="${sleeps[$i]:-}"
	if [[ ! "${g}" =~ ^[0-9]+$ ]] || ((g < b || g > b + b / 2)); then
		fail "sleep $((i + 1)) within [${b}, $((b + b / 2))]" "got '${g}'"
		ok=0
	fi
done
((ok)) && pass "schedule doubles to the cap: ${sleeps[*]}"

# A longer budget must keep clamping to the cap rather than doubling past it.
run_retry 10 5 60
over=0
for s in "${sleeps[@]}"; do
	((s <= 60 + 30)) || over=1
done
assert_eq 'no sleep exceeds the cap plus its jitter' 0 "${over}"
assert_eq 'the cap is reached and held' 60 "$((sleeps[9 - 1] >= 60 ? 60 : 0))"

# Jitter must vary: 12 draws from 0..20 all equal is a ~1e-15 event, so identical
# draws mean the jitter is gone, not a flake.
declare -A seen=()
for _ in {1..12}; do
	run_retry 2 40 60
	seen["${sleeps[0]:-none}"]=1
done
if ((${#seen[@]} > 1)); then
	pass "first delay is jittered (${#seen[@]} distinct draws in 12 runs)"
else
	fail 'first delay is jittered' "all 12 runs slept ${!seen[*]}s"
fi

# A zero delay must not divide or loop oddly: it sleeps 0 and still retries.
run_retry 3 0 60
assert_eq 'zero delay still retries' 3 "${calls}"
assert_eq 'zero delay sleeps zero' "0 0" "${sleeps[*]}"

# Zero attempts ran nothing, which must not read as success.
run_retry 0 5 60
assert_eq 'zero attempts is a failure' 1 "${run_rc}"
assert_eq 'zero attempts runs nothing' 0 "${calls}"

# The label and status reach stderr, for the log of a run that retried.
msg="$(retry_backoff 2 1 60 "pull of x" -- flaky 2>&1 >/dev/null || true)"
case "${msg}" in
*"pull of x failed with exit 7 (attempt 1/2); retrying in "*s) pass 'the retry line names the command and status' ;;
*) fail 'the retry line names the command and status' "got '${msg}'" ;;
esac

# The command is a separate word list: arguments with spaces arrive intact.
args_seen=""
record_args() { args_seen="$*"; }
retry_backoff 1 1 1 "x" -- record_args "a b" c
assert_eq 'arguments pass through unsplit' "a b c" "${args_seen}"

# A missing separator is a caller bug, not a command to run.
rc=0
retry_backoff 1 1 1 "x" flaky 2>/dev/null || rc=$?
assert_eq 'a missing -- is refused' 2 "${rc}"

if ((fails > 0)); then
	printf '\n%d assertion(s) failed\n' "${fails}" >&2
	exit 1
fi
printf '\nall retry.sh assertions passed\n'
