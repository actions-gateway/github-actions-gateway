#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/gmc.sh — the shared GMC readiness helpers
# three dogfood entrypoints wait on before applying a v2 object.
#
# Why it is tested: the helper exists because every v2 CRD routes its apply
# through the GMC's conversion webhook, so a wait that silently stops waiting is
# a bring-up that fails later with `no endpoints available for service
# "webhook-service"`, a message naming the dataplane rather than the cause. Two
# failure directions matter and both are silent. A wait that swallows a failed
# rollout lets the caller apply against a webhook that is not there; and
# gmc_ready doing the opposite — failing its caller instead of answering — would
# abort setup.sh at the probe it uses to decide whether to restart, which is the
# one call site that must survive a not-ready GMC.
#
# kubectl is stubbed, so no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/gmc.sh
source "${REPO_ROOT}/scripts/dogfood/lib/gmc.sh"

WORKDIR="${REPO_ROOT}/tmp/gmc-test"
rm -rf "${WORKDIR}"
mkdir -p "${WORKDIR}"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0
CALL_LOG="${WORKDIR}/calls.log"
KUBECTL_EXIT=0

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	return "${KUBECTL_EXIT}"
}

reset() {
	: >"${CALL_LOG}"
	KUBECTL_EXIT=0
	unset GMC_ROLLOUT_TIMEOUT
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
		echo "FAIL ${name}: '${needle}' not in '${haystack}'" >&2
		fails=$((fails + 1))
	fi
}

echo "scripts/dogfood/gmc-test.sh"

# --- it waits on the right object --------------------------------------------
#
# The deployment and namespace are the whole point of the extraction: they were
# spelled four times across three scripts before this file existed.

reset
wait_for_gmc >/dev/null
check_contains "waits on the GMC deployment" \
	"rollout status deployment/gmc-controller-manager" "$(cat "${CALL_LOG}")"
check_contains "in the gmc-system namespace" \
	"--namespace gmc-system" "$(cat "${CALL_LOG}")"

# --- the timeout: default, env override, explicit argument -------------------

reset
wait_for_gmc >/dev/null
check_contains "defaults to 5m" "--timeout=5m" "$(cat "${CALL_LOG}")"

reset
GMC_ROLLOUT_TIMEOUT=90s wait_for_gmc >/dev/null
check_contains "honours GMC_ROLLOUT_TIMEOUT" "--timeout=90s" "$(cat "${CALL_LOG}")"

# setup.sh passes 3m explicitly, so an argument has to beat the environment or
# that call site silently gets the wrong bound.
reset
GMC_ROLLOUT_TIMEOUT=90s wait_for_gmc 3m >/dev/null
check_contains "an explicit argument beats the environment" \
	"--timeout=3m" "$(cat "${CALL_LOG}")"

# --- a failed rollout must reach the caller ----------------------------------
#
# The direction that hurts: swallow this and the caller applies a v2 object
# against a webhook with no endpoints.

reset
KUBECTL_EXIT=1
rc=0
wait_for_gmc >/dev/null 2>&1 || rc=$?
check "a failed rollout fails the caller" 1 "${rc}"

# --- gmc_ready answers, it does not assert -----------------------------------
#
# setup.sh probes with this to decide whether to restart a stuck GMC, so a
# not-ready GMC must return non-zero without aborting the caller, and must say
# nothing on either stream.

reset
KUBECTL_EXIT=1
rc=0
gmc_ready || rc=$?
check "a not-ready GMC returns non-zero" 1 "${rc}"

reset
KUBECTL_EXIT=1
out="$(gmc_ready 2>&1 || true)"
check "and prints nothing either way" "" "${out}"

reset
KUBECTL_EXIT=0
rc=0
gmc_ready || rc=$?
check "a ready GMC returns zero" 0 "${rc}"
check_contains "and probes with a short default timeout" \
	"--timeout=5s" "$(cat "${CALL_LOG}")"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all lib/gmc.sh tests passed"
