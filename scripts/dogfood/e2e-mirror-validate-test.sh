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
		printf '%s v2 200\n%s manifest 200\n%s push 405\n' \
			"${instance}" "${instance}" "${instance}"
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
done
check "the probe covers every instance and check" "" "${missing}"

# The probe must not address 5001 at all. Each Service declares only 5000/5000
# and the ingress policy admits only TCP/5000, so a ClusterIP probe on 5001 is
# graded by the dataplane rather than by the listener: on Dataplane V2 an
# unmatched port is dropped, which times out and fails a healthy cluster, and a
# reject would pass a bound listener. `debug` is an object read for that reason,
# and this assertion is what keeps it from drifting back to a probe.
check_not_contains "the probe never addresses the debug port" ":5001" "${probe}"

# The namespace is a parameter, and it reaches every address the probe builds.
check_contains "the probe honours the namespace" \
	"mirror-docker-io.other-ns.svc.cluster.local:5000" \
	"$(mirror_probe_script other-ns)"
check_not_contains "and leaves no default behind" \
	"gag-registry-mirror" "$(mirror_probe_script other-ns)"

# --- the object reads: availability and the debug listener ------------------
#
# Both come off one `kubectl get deployment` per declared instance, whose output
# the stub returns verbatim as `<Available>|<env name>|<env value>`. Those three
# fields were measured against the real manifests and two controls, using the
# exact jsonpath the script builds: healthy renders
# `True|REGISTRY_HTTP_DEBUG_ADDR|`, an instance with the env entry removed
# renders `True||`, and one bound to an address renders
# `True|REGISTRY_HTTP_DEBUG_ADDR|:5001`.

CALL_LOG="${WORKDIR}/calls.log"
OBJECTS_FILE="${WORKDIR}/objects"
kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	local name
	name="$(awk '{ for (i = 1; i <= NF; i++) if ($i == "deployment") { print $(i + 1); exit } }' <<<"$*")"
	awk -v n="${name}" '$1 == n { print $2 }' "${OBJECTS_FILE}"
	return 0
}

# arm_objects "<instance> <raw>"... — what `kubectl get` returns per instance.
# An instance left out returns nothing, which is the absent case.
arm_objects() {
	printf '%s\n' "$@" >"${OBJECTS_FILE}"
	: >"${CALL_LOG}"
}

run_objects() {
	set +e
	OBJ_OUT="$(check_instance_objects)"
	OBJ_RC=$?
	set -e
}

# healthy NAME — the raw read a correctly-configured instance produces.
healthy() { printf '%s True|REGISTRY_HTTP_DEBUG_ADDR|' "$1"; }

all_healthy=()
for row in "${MIRROR_INSTANCES[@]}"; do
	read -r instance repo ref <<<"${row}"
	all_healthy+=("$(healthy "${instance}")")
done

arm_objects "${all_healthy[@]}"
run_objects
check "a healthy cluster passes" 0 "${OBJ_RC}"
check "one read per declared instance" "${#MIRROR_INSTANCES[@]}" "$(wc -l <"${CALL_LOG}" | tr -d ' ')"
check "both object checks are graded per instance" \
	"$((${#MIRROR_INSTANCES[@]} * 2))" "$(grep -c '^PASS ' <<<"${OBJ_OUT}")"

# An instance the cluster does not have at all. The reason the table is data
# rather than a `kubectl get -l app=registry-mirror` listing: derived, four
# healthy mirrors out of five declared would report four passes and no failure.
arm_objects "$(healthy mirror-docker-io)" "$(healthy mirror-ghcr-io)" \
	"$(healthy mirror-quay-io)" "$(healthy mirror-registry-k8s-io)"
run_objects
check "a missing instance fails" 1 "${OBJ_RC}"
check_contains "a missing instance is named" \
	"FAIL mirror-gcr-io available: Available=<absent>" "${OBJ_OUT}"
check "the other four still pass availability" 4 \
	"$(grep -c '^PASS .* available$' <<<"${OBJ_OUT}")"

# --- the debug check reads the env NAME, not only its value -----------------
#
# This is the case that would grade the dangerous state green if the check read
# `.value` alone. kubectl's jsonpath renders an empty value and an absent entry
# identically, and the absent one is the state where the bundled config's :5001
# listener is bound. Measured with the script's own jsonpath: `True||`.
arm_objects "$(healthy mirror-docker-io)" "$(healthy mirror-ghcr-io)" \
	"$(healthy mirror-quay-io)" "$(healthy mirror-registry-k8s-io)" \
	"mirror-gcr-io True||"
run_objects
check "an unset debug var fails" 1 "${OBJ_RC}"
check_contains "an unset debug var says the listener is bound" \
	"FAIL mirror-gcr-io debug: REGISTRY_HTTP_DEBUG_ADDR is not set" "${OBJ_OUT}"
check_contains "its availability is unaffected" "PASS mirror-gcr-io available" "${OBJ_OUT}"

# Bound to a real address: present, non-empty.
arm_objects "$(healthy mirror-docker-io)" "$(healthy mirror-ghcr-io)" \
	"$(healthy mirror-quay-io)" "$(healthy mirror-registry-k8s-io)" \
	"mirror-gcr-io True|REGISTRY_HTTP_DEBUG_ADDR|:5001"
run_objects
check "a bound debug listener fails" 1 "${OBJ_RC}"
check_contains "a bound debug listener reports the address" \
	"FAIL mirror-gcr-io debug: REGISTRY_HTTP_DEBUG_ADDR=:5001, want empty" "${OBJ_OUT}"

# A kubectl that cannot read the object at all fails both checks rather than one.
arm_objects "$(healthy mirror-docker-io)"
run_objects
check "an unreadable object fails both checks" 1 "${OBJ_RC}"
check "four instances fail twice each" 8 "$(grep -c '^FAIL ' <<<"${OBJ_OUT}")"

# --- the probe rides a worker's network path --------------------------------

check_contains "the probe pod carries the workload label" \
	'"actions-gateway/component":"workload"' "$(probe_pod_overrides)"

echo
if ((fails)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all checks passed"
