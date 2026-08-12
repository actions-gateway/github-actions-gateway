#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/nodes.sh — the instance-occupancy probes
# that answer "is the dogfood cluster back at rest?" (Q779).
#
# Why this is tested: the answer gates whether an operator walks away from a
# billable GKE cluster, and the failure mode it replaces is silent in the
# expensive direction. A probe that reports "0" when it could not read anything
# looks exactly like a clean teardown, and the reading it replaced did that by
# construction — a `value(currentNodeCount)` projection prints empty both when
# the cluster is at 0 nodes and when the key resolved to nothing at all.
#
# So both directions are asserted: an empty *successful* read is 0, an
# unreadable project is "unknown", and the probe never regresses to asking the
# cluster object for a node count. gcloud is stubbed — no network, no project.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/nodes.sh
source "${REPO_ROOT}/scripts/dogfood/lib/nodes.sh"

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

# --- Stubs -----------------------------------------------------------------
#
# Stub state lives in files: the probes call gcloud inside a command
# substitution, so an argv recorded in that subshell would vanish before the
# assertion could read it.
#
#   argv     The gcloud arguments of the last call, so a test can assert what
#            was asked as well as what came back.
#   result   `FAIL` for a read that could not answer, `EMPTY` for one that
#            answered with no instances, otherwise the instance lines with `|`
#            separating them.
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

PROJECT=dogfood-proj

gcloud() {
	printf '%s\n' "$*" >"${STUB_DIR}/argv"
	local result
	result="$(cat "${STUB_DIR}/result")"
	case "${result}" in
	FAIL) return 1 ;;
	EMPTY) return 0 ;;
	*) printf '%s\n' "${result//|/$'\n'}" ;;
	esac
}

script_result() { printf '%s\n' "$1" >"${STUB_DIR}/result"; }

# run_report — run report_dogfood_at_rest, recording its status in REPORT_RC and
# its output in REPORT_OUT. Not an operand of `||`: that would suppress errexit
# inside the function and let a later assertion pass vacuously.
REPORT_RC=0
REPORT_OUT=""
run_report() {
	set +e
	REPORT_OUT="$(report_dogfood_at_rest 2>&1)"
	REPORT_RC=$?
	set -e
}

echo "scripts/dogfood/nodes-test.sh"

# --- counting ---------------------------------------------------------------

script_result 'gke-node-a us-east1-b e2-standard-2 RUNNING|gke-node-b us-east1-b e2-standard-2 RUNNING'
check "counts the instances that are up" 2 "$(count_dogfood_instances)"

# The reading that matters: a successful read of an idle project is 0, and only
# a successful one may be.
script_result EMPTY
check "an empty successful read counts 0" 0 "$(count_dogfood_instances)"

script_result FAIL
check "an unreadable project counts unknown" unknown "$(count_dogfood_instances)"

# --- the probe shape --------------------------------------------------------

# The defect this replaces: `describe --format='value(currentNodeCount)'` prints
# empty at 0 nodes *and* when the projection resolves to nothing, so the two are
# one reading. Instances carry a name, so their empty list means what it says.
script_result EMPTY
count_dogfood_instances >/dev/null
argv="$(cat "${STUB_DIR}/argv")"
check_contains "lists instances" "compute instances list" "${argv}"
check_contains "pins the project" "--project=${PROJECT}" "${argv}"
check_not_contains "never asks the cluster object for a node count" \
	"currentNodeCount" "${argv}"
check_not_contains "never describes the cluster" "clusters describe" "${argv}"
# Unfiltered on purpose: a --filter that stops matching (a renamed GKE label, a
# node pool made by hand) returns empty exactly like an idle project, which is
# the same false-zero in a new place.
check_not_contains "does not filter the list" "--filter" "${argv}"

# --- the at-rest verdict ----------------------------------------------------

script_result EMPTY
run_report
check "at rest exits 0" 0 "${REPORT_RC}"
check_contains "says it is at rest" "AT REST" "${REPORT_OUT}"

script_result 'gke-node-a us-east1-b e2-standard-2 RUNNING'
run_report
check "instances up exits 1" 1 "${REPORT_RC}"
check_contains "says it is not at rest" "NOT AT REST" "${REPORT_OUT}"
check_contains "names what is still up" "gke-node-a" "${REPORT_OUT}"

# The expensive direction: an unreadable project must not be reported as an idle
# one, and must not share an exit status with one either — a caller gating a
# walk-away on this can then tell the two apart.
script_result FAIL
run_report
check "an unreadable project exits 2" 2 "${REPORT_RC}"
check_contains "says the answer is unknown" "UNKNOWN" "${REPORT_OUT}"
check_not_contains "never reports an unreadable project as at rest" \
	"AT REST: " "${REPORT_OUT}"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all nodes.sh tests passed"
