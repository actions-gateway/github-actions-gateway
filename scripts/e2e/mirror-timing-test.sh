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

# --- main(), which nothing above reaches ---------------------------------------
#
# Everything before this exercises the two pure helpers. main() holds the parts
# the probe is actually FOR -- the interleave, the pairing, the trailing hit, and
# the formatting of a fetch that produced nothing -- and none of it was covered.
# That is not hypothetical: the `timeoutms` defect lived here and shipped, caught
# by a reader rather than by this file.
#
# mirror_for, blob_digest and fetch_ms are replaced, so no network and no mirror.
# fetch_ms answers from a per-reference queue, which is what lets a run be given
# a faithful cache (first fetch slow, repeat fast) or a flat one.

# THE PLAN LIVES IN A FILE, not a variable, and that is not incidental. main()
# calls `ms="$(fetch_ms ...)"`, so the fake runs in a SUBSHELL and any state it
# mutates in memory is discarded when that subshell exits -- every fetch would
# then replay the queue's first row and a faithful cache would read as flat. The
# first version of this file did exactly that and reported OVERLAPPING for an
# input that separates cleanly.
PLAN="$(mktemp)"
trap 'rm -f "${PLAN}"' EXIT

# THE PLAN IS KEYED ON THE DIGEST, not the repository, because PROBE_REFS holds
# two references per repository (alpine:3.18 and :3.19, busybox:1.35 and :1.36).
# The real blob_digest returns a different digest per tag, so those are four
# independent cache states; a fake returning one digest for all four collapses
# them into one queue and a faithful plan then reads as flat. The first version
# of this file did that and reported OVERLAPPING for an input that separates.
mirror_for() { printf 'mirror.invalid:5000'; }
blob_digest() { printf 'sha256:%s-%s' "$2" "$3"; }
fetch_ms() {
	local digest="$3" line rest
	line="$(awk -v d="${digest}" '$1 == d { print $2; exit }' "${PLAN}")"
	# Drop the row just consumed so a second fetch of the same blob gets the next.
	rest="$(awk -v d="${digest}" 'BEGIN { used = 0 }
		$1 == d && !used { used = 1; next } { print }' "${PLAN}")"
	printf '%s\n' "${rest}" > "${PLAN}"
	[[ "${line}" == "-" ]] && return 0
	printf '%s' "${line}"
}

# plan_write TIMES... — one row per reference per fetch, in the order that
# reference's blob is fetched. `plan_write 500 20` is a faithful cache (first
# fetch slow, repeat fast), `300 300` a flat one, `- -` a run that timed out.
plan_write() {
	local ref t
	: > "${PLAN}"
	for ref in "${PROBE_REFS[@]}"; do
		for t in "$@"; do printf 'sha256:%s-%s %s\n' "${ref%:*}" "${ref##*:}" "${t}" >> "${PLAN}"; done
	done
}

run_main() {
	set +e
	MAIN_OUT="$(main 2>&1)"
	MAIN_RC=$?
	set -e
}

plan_write 500 20; run_main
check "a faithful cache runs clean" 0 "${MAIN_RC}"
check_contains "and reads SEPARATED" "SEPARATED" "${MAIN_OUT}"
check "one miss line per reference" "${#PROBE_REFS[@]}" "$(grep -c '^  miss ' <<<"${MAIN_OUT}")"
check "one hit line per reference" "${#PROBE_REFS[@]}" "$(grep -c '^  hit ' <<<"${MAIN_OUT}")"
check "the arms are equal and complete" "n=${#PROBE_REFS[@]}" \
	"$(grep -o 'hit   n=[0-9]*' <<<"${MAIN_OUT}" | sed 's/hit   //')"

# The trailing hit: the last reference has no later iteration to be re-fetched
# in, so it is fetched again after the loop. Without that the last reference
# contributes a miss and no hit, and the arms come out uneven -- which the
# equality check above would catch only because it is asserted per arm.
check "the last reference gets its hit too" 1 \
	"$(grep -c "^  hit   ${PROBE_REFS[-1]} " <<<"${MAIN_OUT}")"

# Every hit is one whole miss away from its own miss. Read off the emitted order
# rather than asserted from the source.
check "no hit is timed immediately after its own miss" "" \
	"$(awk '/^  (miss|hit) /{ a[++n] = $1 " " $2 }
	       END { for (i = 2; i <= n; i++)
	               if (a[i] ~ /^hit/ && a[i-1] ~ /^miss/) {
	                   split(a[i], h, " "); split(a[i-1], m, " ")
	                   if (h[2] == m[2]) print "adjacent: " h[2]
	               } }' <<<"${MAIN_OUT}")"

# A flat cache: every fetch the same, so the arms cannot separate. Same code
# path, opposite verdict -- which is what makes the verdict a reading rather
# than a fixed string.
plan_write 300 300; run_main
check "a flat cache runs clean" 0 "${MAIN_RC}"
check_contains "and reads OVERLAPPING" "OVERLAPPING" "${MAIN_OUT}"

# Every fetch times out: no samples at all, so the reading was not taken. This
# is the route from main() to the refusal, which the helper tests reach only by
# calling separation_verdict directly.
plan_write - -; run_main
check "a run that timed out throughout refuses" 1 "${MAIN_RC}"
check_contains "and says the reading was not taken" "the reading was not taken" "${MAIN_OUT}"
# The formatting of a fetch that produced nothing. `timeoutms` shipped here.
check_contains "a timed-out fetch prints a bare timeout" " timeout" "${MAIN_OUT}"
check "and never concatenates ms onto it" 0 "$(grep -c 'timeoutms' <<<"${MAIN_OUT}")"

# No mirror wired is a skip, not a failure: the hosted lane and a developer's
# `make e2e` both land here.
mirror_for() { printf ''; }
run_main
check "no mirror wired skips cleanly" 0 "${MAIN_RC}"
check_contains "and says why" "no mirror wired" "${MAIN_OUT}"

echo
if ((fails)); then
	echo "${fails} check(s) failed" >&2
	exit 1
fi
echo "all mirror-timing.sh assertions passed"
