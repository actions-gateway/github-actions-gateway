#!/usr/bin/env bash
#
# Unit tests for scripts/e2e/mirror-timing.sh — the Q1020 probe that asks whether
# a mirror cache hit is distinguishable from a miss from inside a Kata guest.
#
# Why the pure half is worth testing when the probe only reports. The numbers are
# taken in a booked Kata window and read once; a summariser that got them wrong
# would be believed, and nothing downstream would contradict it. Three ways it
# could:
#
#   1. Reading a reading that was not taken. An arm with no samples must refuse,
#      never render as a refuted channel — "the hits and misses did not separate"
#      and "there were no hits" are opposite findings that a careless summary
#      prints identically.
#   2. Grading the ratio instead of the overlap. An attacker times ONE fetch, so
#      arms whose ranges touch leak nothing however far apart their medians sit.
#      The case that separates those two verdicts is arms with a wide median gap
#      and one overlapping sample, and it is asserted below.
#   3. Inventing a value. An interpolating median can print a number no sample
#      held, which in a two-sample arm is the midpoint of a hit and a miss.
#
# Nothing here touches curl or the network: the probe is sourced with
# MIRROR_TIMING_LIB_ONLY=1 so main() does not run.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
MIRROR_TIMING_LIB_ONLY=1
export MIRROR_TIMING_LIB_ONLY
# shellcheck source=scripts/e2e/mirror-timing.sh
source "${REPO_ROOT}/scripts/e2e/mirror-timing.sh"

fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

check_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" == *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' not in output" >&2
		echo "     output: ${haystack}" >&2
		fails=$((fails + 1))
	fi
}

verdict() {
	set +e
	V_OUT="$(separation_verdict <<<"$1")"
	V_RC=$?
	set -e
}

echo "scripts/e2e/mirror-timing-test.sh"

# --- stats -------------------------------------------------------------------

check "min median max, unsorted input" "10 44 637" "$(printf '637\n10\n44\n' | stats)"

# The lower middle on an even count. An interpolating median would print 43 here,
# a value neither sample held — and in a two-sample arm that is the midpoint of a
# hit and a miss.
check "the median is a value some sample actually held" "10 11 70" \
	"$(printf '10\n11\n13\n70\n' | stats)"

check "an empty arm produces nothing" "" "$(printf '' | stats)"

# --- the verdict is the overlap, not the ratio -------------------------------

# The measured shape from the laptop reading: blob hits in the tens of ms against
# misses in the hundreds.
verdict "hit 10
hit 13
hit 11
miss 637
miss 419
miss 604"
check "cleanly separated arms are graded 0" 0 "${V_RC}"
check_contains "and named SEPARATED" "SEPARATED" "${V_OUT}"
check_contains "with the two bounds that make it separated" \
	"every hit (<=13ms) was faster than every miss (>=419ms)" "${V_OUT}"

# The case a ratio-based verdict gets wrong: medians an order of magnitude apart,
# and one miss inside the hit range. An attacker sees one fetch, so this leaks
# nothing reliable and must NOT read as a channel.
verdict "hit 10
hit 12
hit 400
miss 390
miss 620
miss 640"
check "overlapping arms are still graded 0" 0 "${V_RC}"
check_contains "but named OVERLAPPING despite the median gap" "OVERLAPPING" "${V_OUT}"
check_contains "and it says a single fetch cannot tell them apart" \
	"a single fetch does not tell them apart" "${V_OUT}"

# Adjacent but not overlapping is separation: the line is strict.
verdict "hit 100
miss 101"
check_contains "one millisecond of daylight is separation" "SEPARATED" "${V_OUT}"
verdict "hit 100
miss 100"
check_contains "an equal boundary is not" "OVERLAPPING" "${V_OUT}"

# --- a reading that was not taken is never a refuted channel -----------------

verdict "hit 10
hit 12"
check "an arm with no misses refuses" 1 "${V_RC}"
check_contains "and says which arm was empty" "0 miss sample(s)" "${V_OUT}"

verdict "miss 600"
check "an arm with no hits refuses" 1 "${V_RC}"
check_contains "naming the empty hit arm" "0 hit sample(s)" "${V_OUT}"

verdict ""
check "no samples at all refuses" 1 "${V_RC}"
check_contains "rather than reporting arms" "the reading was not taken" "${V_OUT}"

# Lines that are neither arm are ignored rather than counted: the probe prints a
# `timeout` row for a fetch that produced no timing, and a summary that counted
# it would report a sample it never had.
verdict "hit 10
timeout - -
miss 600"
check "unrecognised lines do not become samples" 0 "${V_RC}"
check_contains "the hit arm holds one sample" "hit   n=1" "${V_OUT}"
check_contains "the miss arm holds one sample" "miss  n=1" "${V_OUT}"

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all mirror-timing.sh assertions passed"
