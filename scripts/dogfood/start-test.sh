#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/start.sh: wait_agc_rollout, the readiness wait
# that gates every dogfood bring-up.
#
# Why it is tested: the wait it replaced was a pod-label `kubectl wait`, which
# selects the outgoing ReplicaSet's pod during a rollout. That pod is
# terminating and never reaches Ready, so the wait burned its whole budget and
# kubectl then reported every selected pod as timed out — including the healthy
# new one — turning a successful AGC rollout into a failed bring-up. A release
# validation run died there with the AGC 1/1 Running. The regression is silent
# (it looks like a genuine timeout), so the shape of the wait is asserted here:
# it must track the Deployment's rollout, never a pod selector.
#
# The script is sourced with START_LIB_ONLY=1 so main() does not run; `kubectl`
# is stubbed, so no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
START_LIB_ONLY=1
export START_LIB_ONLY
# shellcheck source=scripts/dogfood/start.sh
source "${REPO_ROOT}/scripts/dogfood/start.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0

# Stub kubectl: logs its argv so a test can assert what was called, and answers
# `get deployment/...` from GET_EXITS — a queue of exit codes modelling "absent,
# absent, then created". The last entry repeats once the queue is exhausted.
KUBECTL_LOG="${WORKDIR}/kubectl.log"
GET_EXITS_FILE="${WORKDIR}/get-exits"
ROLLOUT_EXIT=0
reset_kubectl() {
	printf '%s\n' "$@" >"${GET_EXITS_FILE}"
	: >"${KUBECTL_LOG}"
	ROLLOUT_EXIT=0
}
kubectl() {
	printf '%s\n' "$*" >>"${KUBECTL_LOG}"
	case "$*" in
	*"rollout status"*) return "${ROLLOUT_EXIT}" ;;
	*get\ deployment/*)
		local head
		head="$(head -n 1 "${GET_EXITS_FILE}")"
		if (($(wc -l <"${GET_EXITS_FILE}") > 1)); then
			tail -n +2 "${GET_EXITS_FILE}" >"${GET_EXITS_FILE}.next"
			mv "${GET_EXITS_FILE}.next" "${GET_EXITS_FILE}"
		fi
		return "${head}"
		;;
	esac
	return 0
}

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

# Loop pacing only — the absent-Deployment test otherwise takes 120 real seconds.
sleep() { :; }

echo "scripts/dogfood/start-test.sh"

# --- start.sh sources the shared GMC helper ----------------------------------
#
# It calls wait_for_gmc before applying anything the conversion webhook has to
# serve, and a missing `source` line fails only at runtime, with `command not
# found` on a cluster that is otherwise fine. `bash -n` cannot see it and the
# helper's own suite does not know who calls it, so the seam is asserted here.
# What the helper then does is scripts/dogfood/gmc-test.sh's subject.

check "sources lib/gmc.sh, so wait_for_gmc resolves" \
	"function" "$(type -t wait_for_gmc || true)"

# --- the rollout is tracked by Deployment, never by a pod selector ------------

reset_kubectl 0
wait_agc_rollout gag-dogfood dogfood-agc >/dev/null 2>&1
log="$(cat "${KUBECTL_LOG}")"
check_contains "waits on the Deployment's rollout" \
	"rollout status deployment/dogfood-agc" "${log}"
check_contains "scopes the wait to the tenant namespace" \
	"-n gag-dogfood" "${log}"
# The regression guard: a pod-label wait is what reports a healthy rollout as a
# timeout, because it selects the terminating pod of the outgoing ReplicaSet.
check_not_contains "never waits on a pod label selector" \
	"--for=condition=Ready pod" "${log}"

# --- an absent Deployment is polled for, not fast-failed ---------------------

# rollout status fast-fails "not found", so a Deployment the GMC has not created
# yet must be waited for rather than treated as a failure on the first look.
reset_kubectl 1 1 1 0
wait_agc_rollout gag-dogfood dogfood-agc >/dev/null 2>&1
rc=$?
check "polls until the GMC creates the Deployment" "0" "${rc}"
check_contains "still reaches the rollout wait" \
	"rollout status deployment/dogfood-agc" "$(cat "${KUBECTL_LOG}")"

# --- a Deployment that never appears fails, and says why ---------------------

reset_kubectl 1
set +e
err="$(wait_agc_rollout gag-dogfood dogfood-agc 2>&1 >/dev/null)"
rc=$?
set -e
check "fails when the Deployment never appears" "1" "${rc}"
check_contains "names the missing Deployment" \
	"no deployment/dogfood-agc" "${err}"
check_not_contains "does not wait on a rollout that cannot exist" \
	"rollout status" "$(cat "${KUBECTL_LOG}")"

# --- a genuinely failed rollout still fails ----------------------------------

# The fix must not convert a real rollout failure into a pass.
reset_kubectl 0
ROLLOUT_EXIT=1
set +e
wait_agc_rollout gag-dogfood dogfood-agc >/dev/null 2>&1
rc=$?
set -e
check "propagates a failed rollout" "1" "${rc}"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all start.sh tests passed"
