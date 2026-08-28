#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-mirror-validate.sh: the Q408 Phase 2
# battery that grades whether the in-cluster registry pull-through cache serves.
#
# Why it is tested. The script's whole value is that a booked dogfood session
# gets a verdict rather than a transcript, and every way this kind of battery
# lies is silent:
#
#   1. A probe pod that never ran produces empty output, and a grader that walks
#      what it received finds no failures in it. Green from an instrument that
#      measured nothing. So the expected set is the instance table, and an absent
#      line is a FAIL — asserted below in both the empty and the partial case.
#   2. `GET /v2/` answers 200 from the API layer alone. A mirror whose storage
#      root is unwritable serves 200 there and 500 for every pull (the plan's
#      §3.6 fsGroup finding). A battery that graded only `/v2/` would pass a
#      mirror that cannot cache anything, so the 200/500 split is pinned here.
#   3. Read-only is the posture's load-bearing property (§3.1). A push that is
#      ACCEPTED must be a failure, not an unexpected-status note.
#
# The expected values the grader asserts were measured against the pinned image
# rather than read off the design: five instances in proxy mode, one real
# manifest per upstream, all 200 / 405 / connection-refused. Both controls fire
# — a bundled-config instance answers on :5001 (curl 0, not 7), and a non-root
# instance with no writable storage answers `/v2/` 200 and the manifest 500. See
# the PR body for the commands.
#
# The script is sourced with E2E_MIRROR_VALIDATE_LIB_ONLY=1 so main() does not
# run; `kubectl` is stubbed, so no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_MIRROR_VALIDATE_LIB_ONLY=1
export E2E_MIRROR_VALIDATE_LIB_ONLY
# shellcheck source=scripts/dogfood/e2e-mirror-validate.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-mirror-validate.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

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
		fails=$((fails + 1))
	fi
}

check_not_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" != *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' unexpectedly present" >&2
		fails=$((fails + 1))
	fi
}

# grade INPUT — run grade_probe_output over INPUT, recording GRADE_RC and
# GRADE_OUT. Not an operand of `||`, so the function's own errexit context is
# unchanged (the `set +e` pattern e2e-start-test.sh records).
GRADE_RC=0
GRADE_OUT=""
grade() {
	set +e
	GRADE_OUT="$(grade_probe_output <<<"$1")"
	GRADE_RC=$?
	set -e
}

# a_clean_transcript — what a healthy cluster reports: the measured 200/200/405/7
# for every declared instance.
a_clean_transcript() {
	local row instance repo ref
	for row in "${MIRROR_INSTANCES[@]}"; do
		read -r instance repo ref <<<"${row}"
		printf '%s v2 200\n%s manifest 200\n%s push 405\n%s debug 7\n' \
			"${instance}" "${instance}" "${instance}" "${instance}"
	done
}

echo "scripts/dogfood/e2e-mirror-validate-test.sh"

# --- the battery is graded green only when every check reported -------------

grade "$(a_clean_transcript)"
check "a healthy transcript passes" 0 "${GRADE_RC}"
check_not_contains "a healthy transcript has no FAIL" "FAIL" "${GRADE_OUT}"
check "every declared check is graded" \
	"$((${#MIRROR_INSTANCES[@]} * ${#PROBE_CHECKS[@]}))" \
	"$(grep -c '^PASS ' <<<"${GRADE_OUT}")"

# The control this whole grader exists for: a probe pod that died before its
# first echo. Walking the transcript would find nothing wrong with it.
grade ""
check "an empty transcript fails" 1 "${GRADE_RC}"
check "an empty transcript fails every check" \
	"$((${#MIRROR_INSTANCES[@]} * ${#PROBE_CHECKS[@]}))" \
	"$(grep -c '^FAIL ' <<<"${GRADE_OUT}")"
check_contains "an absent result says so" "no result reported" "${GRADE_OUT}"

# A partial transcript — the pod ran, then was evicted mid-battery.
grade "mirror-docker-io v2 200
mirror-docker-io manifest 200"
check "a partial transcript fails" 1 "${GRADE_RC}"
check_contains "the reported checks still pass" "PASS mirror-docker-io v2" "${GRADE_OUT}"
check_contains "an unreported check fails" \
	"FAIL mirror-docker-io push: no result reported" "${GRADE_OUT}"
check_contains "an unreported instance fails" \
	"FAIL mirror-gcr-io v2: no result reported" "${GRADE_OUT}"

# --- /v2/ green over a mirror that cannot serve a pull ----------------------
#
# The plan's §3.6 fsGroup shape, measured against the image: 200 on /v2/, 500 on
# every manifest. A battery graded on /v2/ alone calls this healthy.
grade "$(a_clean_transcript | awk '$1 == "mirror-quay-io" && $2 == "manifest" { $3 = 500 } { print }')"
check "an unwritable mirror fails" 1 "${GRADE_RC}"
check_contains "the manifest check is what catches it" \
	"FAIL mirror-quay-io manifest: got 500, want 200" "${GRADE_OUT}"
check_contains "its /v2/ check still passes" "PASS mirror-quay-io v2" "${GRADE_OUT}"

# --- read-only ---------------------------------------------------------------

grade "$(a_clean_transcript | awk '$1 == "mirror-ghcr-io" && $2 == "push" { $3 = 202 } { print }')"
check "an accepted upload fails" 1 "${GRADE_RC}"
check_contains "an accepted upload is named as such" \
	"FAIL mirror-ghcr-io push: upload ACCEPTED (202)" "${GRADE_OUT}"

# A refusal that is not the measured 405 is still a failure: the posture rests on
# knowing which refusal it got, not on the request having gone badly somehow.
grade "$(a_clean_transcript | awk '$1 == "mirror-ghcr-io" && $2 == "push" { $3 = 404 } { print }')"
check "an unexpected refusal fails" 1 "${GRADE_RC}"
check_contains "an unexpected refusal reports both codes" \
	"FAIL mirror-ghcr-io push: got 404, want 405" "${GRADE_OUT}"

# --- the debug listener ------------------------------------------------------
#
# Measured control: an instance running the image's bundled development config
# answers on :5001, so curl exits 0 rather than 7.
grade "$(a_clean_transcript | awk '$1 == "mirror-gcr-io" && $2 == "debug" { $3 = 0 } { print }')"
check "a bound debug listener fails" 1 "${GRADE_RC}"
check_contains "a bound debug listener reports curl's status" \
	"FAIL mirror-gcr-io debug: got 0, want 7" "${GRADE_OUT}"

# --- the probe script covers the whole table --------------------------------

probe="$(mirror_probe_script gag-registry-mirror)"
check "the probe is valid sh" 0 "$(
	set +e
	sh -n <<<"${probe}"
	echo $?
)"
missing=""
for row in "${MIRROR_INSTANCES[@]}"; do
	read -r instance repo ref <<<"${row}"
	for c in "${PROBE_CHECKS[@]}"; do
		[[ "${probe}" == *"${instance} ${c} "* ]] || missing="${missing} ${instance}/${c}"
	done
	# The manifest probe must name the instance's own upstream repository and
	# reference: a probe that fetched some other repo would grade the mirror on a
	# path the e2e suite never walks.
	[[ "${probe}" == *"/v2/${repo}/manifests/${ref}"* ]] || missing="${missing} ${instance}/ref"
	[[ "${probe}" == *"${instance}.gag-registry-mirror.svc.cluster.local:5000"* ]] ||
		missing="${missing} ${instance}/host"
	[[ "${probe}" == *"${instance}.gag-registry-mirror.svc.cluster.local:5001"* ]] ||
		missing="${missing} ${instance}/debug-host"
done
check "the probe covers every instance and check" "" "${missing}"

# The namespace is a parameter, and it reaches every address the probe builds.
check_contains "the probe honours the namespace" \
	"mirror-docker-io.other-ns.svc.cluster.local:5000" \
	"$(mirror_probe_script other-ns)"
check_not_contains "and leaves no default behind" \
	"gag-registry-mirror" "$(mirror_probe_script other-ns)"

# --- availability is read per declared instance, not from a listing ---------

CALL_LOG="${WORKDIR}/calls.log"
AVAILABLE_FILE="${WORKDIR}/available"
kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	local name
	name="$(awk '{ for (i = 1; i <= NF; i++) if ($i == "deployment") { print $(i + 1); exit } }' <<<"$*")"
	grep -qx "${name}" "${AVAILABLE_FILE}" && echo -n True
	return 0
}

# arm_available NAME... — only these instances report Available.
arm_available() {
	printf '%s\n' "$@" >"${AVAILABLE_FILE}"
	: >"${CALL_LOG}"
}

run_available() {
	set +e
	AVAIL_OUT="$(check_instances_available)"
	AVAIL_RC=$?
	set -e
}

all_instances=()
for row in "${MIRROR_INSTANCES[@]}"; do
	read -r instance repo ref <<<"${row}"
	all_instances+=("${instance}")
done

arm_available "${all_instances[@]}"
run_available
check "all instances Available passes" 0 "${AVAIL_RC}"
check "one read per declared instance" "${#MIRROR_INSTANCES[@]}" "$(wc -l <"${CALL_LOG}" | tr -d ' ')"

# An instance the cluster does not have at all. The reason the table is data
# rather than a `kubectl get -l app=registry-mirror` listing: derived, four
# healthy mirrors out of five declared would report four passes and no failure.
arm_available mirror-docker-io mirror-ghcr-io mirror-quay-io mirror-registry-k8s-io
run_available
check "a missing instance fails" 1 "${AVAIL_RC}"
check_contains "a missing instance is named" \
	"FAIL mirror-gcr-io available: Available=<absent>" "${AVAIL_OUT}"
check "the other four still pass" 4 "$(grep -c '^PASS ' <<<"${AVAIL_OUT}")"

# --- the probe rides a worker's network path --------------------------------

check_contains "the probe pod carries the workload label" \
	'"actions-gateway/component":"workload"' "$(probe_pod_overrides)"

echo
if ((fails)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all checks passed"
