#!/usr/bin/env bash
#
# Unit tests for scripts/e2e/egress-negatives.sh — the Q408 Phase 4 battery that
# proves a Kata worker's only registry path is the in-cluster mirror.
#
# Why it is tested. This battery's whole job is to report FAIL, and a grader that
# cannot report FAIL is indistinguishable from a posture that holds. Three ways
# it could go quietly wrong, all asserted below: a blocked check that answers an
# HTTP status must fail (the posture is not enforced), a control that answers
# nothing must fail (the negatives are then unfalsifiable, so passing them proves
# nothing), and a check that reports no line at all must fail rather than be
# skipped — the shape a probe that died before its first echo produces.
#
# The probes themselves are not exercised: they are curl and `docker pull`
# against a live cluster, which is the dogfood session this battery exists to
# spend well. What is testable off-cluster is the grading, and the grading is
# where a false PASS would come from.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
EGRESS_NEGATIVES_LIB_ONLY=1
export EGRESS_NEGATIVES_LIB_ONLY
# shellcheck source=scripts/e2e/egress-negatives.sh
source "${REPO_ROOT}/scripts/e2e/egress-negatives.sh"

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

# grade <<< lines — run the grader over a reading, leaving its output in
# GRADE_OUT and its exit status in GRADE_RC. Half the assertions here are about
# the status, which a command substitution alone would swallow.
grade() {
	set +e
	GRADE_OUT="$(grade_negatives)"
	GRADE_RC=$?
	set -e
}

# The reading a fully enforced worker produces. Every later case is this one with
# a single value changed, so a failure names the change rather than the fixture.
ENFORCED=$(
	cat <<-'EOF'
		mirror-reachable 200
		github-reachable 200
		mirror-readonly 405
		docker-mirror-pull ok
		upstream-blocked -
		internet-blocked -
		metadata-blocked -
		docker-upstream-blocked fail
	EOF
)

# --- the table is complete ---------------------------------------------------
#
# A check added to NEGATIVE_CHECKS with no expectation would grade against an
# empty string and pass only when the probe reported nothing, which is the
# inverse of what any of these mean.

for name in "${NEGATIVE_CHECKS[@]}"; do
	want=''
	want="$(expected_value "${name}" || true)"
	check "every declared check has an expectation (${name})" 'set' "${want:+set}"
done

check 'an undeclared check has no expectation' '' "$(expected_value nonesuch || true)"

# The battery must actually contain negatives. A refactor that turned every
# blocked check into a reachability check would leave the grader green and the
# enforcement claim unmade.
blocked=0
for name in "${NEGATIVE_CHECKS[@]}"; do
	[[ "$(expected_value "${name}")" == '-' ]] && blocked=$((blocked + 1))
done
check 'the battery declares three blocked destinations' 3 "${blocked}"

# --- the enforced reading passes ---------------------------------------------

grade <<<"${ENFORCED}"
check 'an enforced worker grades 0' 0 "${GRADE_RC}"
check 'an enforced worker reports no FAIL' '' "$(grep FAIL <<<"${GRADE_OUT}" || true)"
check 'an enforced worker grades every declared check' \
	"${#NEGATIVE_CHECKS[@]}" "$(grep -c . <<<"${GRADE_OUT}")"

# --- a reachable destination fails -------------------------------------------
#
# The finding this battery exists for: `e2e-open-egress` still present, or a
# policy that never selected the worker, leaves the upstream answering.

for name in upstream-blocked internet-blocked metadata-blocked; do
	grade <<<"${ENFORCED//${name} -/${name} 200}"
	check "a reachable ${name} grades 1" 1 "${GRADE_RC}"
	check "a reachable ${name} says it is reachable" 'yes' \
		"$(grep -q "FAIL ${name}: answered HTTP 200" <<<"${GRADE_OUT}" && echo yes)"
done

grade <<<"${ENFORCED/docker-upstream-blocked fail/docker-upstream-blocked ok}"
check 'a successful direct upstream pull grades 1' 1 "${GRADE_RC}"

# --- a dead control fails, so the negatives are never read alone --------------
#
# Every blocked check passes when the pod has no network at all. These are the
# assertions that stop such a run grading green.

grade <<<"${ENFORCED/mirror-reachable 200/mirror-reachable -}"
check 'an unreachable mirror grades 1' 1 "${GRADE_RC}"

grade <<<"${ENFORCED/github-reachable 200/github-reachable -}"
check 'unreachable GitHub grades 1' 1 "${GRADE_RC}"
check 'unreachable GitHub says the negatives are unfalsifiable' 'yes' \
	"$(grep -q 'unfalsifiable' <<<"${GRADE_OUT}" && echo yes)"

grade <<<"${ENFORCED/docker-mirror-pull ok/docker-mirror-pull fail}"
check 'a failed pull through the mirror grades 1' 1 "${GRADE_RC}"

# GitHub is graded on having answered at all, not on which status: an
# unauthenticated API call is free to return anything.
grade <<<"${ENFORCED/github-reachable 200/github-reachable 403}"
check 'GitHub answering 403 still counts as reached' 0 "${GRADE_RC}"

# --- the mirror must stay read-only ------------------------------------------

grade <<<"${ENFORCED/mirror-readonly 405/mirror-readonly 202}"
check 'a mirror that accepts an upload grades 1' 1 "${GRADE_RC}"

# --- a check that never reported is a failure, not a skip --------------------

grade <<<"$(grep -v metadata-blocked <<<"${ENFORCED}")"
check 'a missing check grades 1' 1 "${GRADE_RC}"
check 'a missing check is named' 'yes' \
	"$(grep -q 'FAIL metadata-blocked: no result reported' <<<"${GRADE_OUT}" && echo yes)"
check 'a missing check still grades every other check' \
	"${#NEGATIVE_CHECKS[@]}" "$(grep -c . <<<"${GRADE_OUT}")"

grade </dev/null
check 'a probe run that produced nothing grades 1' 1 "${GRADE_RC}"

# --- the skip path -----------------------------------------------------------
#
# The hosted lane and a developer's `make e2e` have no mirror and no tight
# policy. The battery must say so and exit 0 rather than fail on an absent map.

set +e
skip_out="$(REGISTRY_MIRRORS='' main 2>&1)"
skip_rc=$?
set -e
check 'no map skips cleanly' 0 "${skip_rc}"
check 'no map says it skipped' 'yes' "$(grep -q 'SKIPPED' <<<"${skip_out}" && echo yes)"

# A map that omits an upstream the battery names is NOT a skip: it is a battery
# that would have run without its control, which must fail loudly instead.
set +e
partial_out="$(REGISTRY_MIRRORS='ghcr.io=mirror-ghcr-io:5000' main 2>&1)"
partial_rc=$?
set -e
check 'a map without docker.io fails rather than skipping' 1 "${partial_rc}"
check 'a map without docker.io says which mirror is missing' 'yes' \
	"$(grep -q 'no docker.io mirror' <<<"${partial_out}" && echo yes)"

set +e
nogcr_out="$(REGISTRY_MIRRORS='docker.io=mirror-docker-io:5000' main 2>&1)"
nogcr_rc=$?
set -e
check 'a map without the pull upstream fails' 1 "${nogcr_rc}"
check 'a map without the pull upstream says so' 'yes' \
	"$(grep -q "no ${PROBE_UPSTREAM} mirror" <<<"${nogcr_out}" && echo yes)"

if ((fails)); then
	echo "${fails} test(s) failed" >&2
	exit 1
fi
echo "all tests passed"
