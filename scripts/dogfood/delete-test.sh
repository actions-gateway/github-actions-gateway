#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/delete.sh: the destructive teardown — the
# existence check, the occupancy probes the confirmation quotes, the routing
# reset, the cluster delete, and the post-delete kubeconfig prune and orphan
# report.
#
# Why it is tested: this script destroys the whole dogfood environment, and its
# only brake is a confirmation the operator reads. What that confirmation says
# is therefore load-bearing. The dangerous shape is an occupancy probe that
# fails and reports zero: an operator told "0 worker pods in flight" about a
# cluster that could not be read approves a delete that kills the AGCs mid-job,
# and worker pods outlive their AGC with do-not-disrupt annotations pinning
# billable nodes (one incident stranded 82 spot node-hours). So an unreadable
# cluster must read "unknown" here, never 0 — and the confirmation must gate
# every mutation, not just the delete.
#
# The inverse matters too: nothing may be destroyed that recreate cannot
# rebuild. The script deletes exactly one cluster, pinned by project and zone,
# and only reports the billable resources that outlived it rather than sweeping
# them away.
#
# The script is sourced with DELETE_LIB_ONLY=1 so main() does not run;
# `gcloud`, `kubectl`, `gh` and require_cmd are stubbed, so nothing is deleted
# anywhere. confirm_or_exit runs for real against a scripted stdin, so the
# decline path is the real gate rather than a stubbed stand-in.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
DELETE_LIB_ONLY=1
export DELETE_LIB_ONLY
# shellcheck source=scripts/dogfood/delete.sh
source "${REPO_ROOT}/scripts/dogfood/delete.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0

PROJECT=dogfood-proj
CLUSTER=gag-dogfood
ZONE=us-east1-b
REPO=octo/repo

# --- Stubs -----------------------------------------------------------------
#
# Every external command logs its argv to one shared file, so a test can assert
# both what was called and in what order — ordering is the safety argument (a
# delete before the routing reset leaves jobs pointed at a cluster that is
# disappearing). The log is a file because main() runs in a subshell.
CALL_LOG="${WORKDIR}/calls.log"
PODS_FILE="${WORKDIR}/pods"
DISKS_FILE="${WORKDIR}/disks"
ADDRESSES_FILE="${WORKDIR}/addresses"
CLUSTER_EXISTS=1
NODE_COUNT_OK=1
NODE_COUNT=3
POD_READ_OK=1
CONFIRM_REPLY=""

gcloud() {
	printf 'gcloud %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	*currentNodeCount*)
		((NODE_COUNT_OK)) || return 1
		echo "${NODE_COUNT}"
		;;
	container\ clusters\ describe*) ((CLUSTER_EXISTS)) || return 1 ;;
	compute\ disks\ list*) cat "${DISKS_FILE}" ;;
	compute\ addresses\ list*) cat "${ADDRESSES_FILE}" ;;
	esac
	return 0
}

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	*get\ pods*)
		((POD_READ_OK)) || return 1
		cat "${PODS_FILE}"
		;;
	esac
	return 0
}

gh() { printf 'gh %s\n' "$*" >>"${CALL_LOG}"; }
require_cmd() { :; }

# reset_stubs — a cluster that exists, is readable, has three nodes and no
# worker pods, with the confirmation auto-approved.
reset_stubs() {
	: >"${CALL_LOG}"
	: >"${PODS_FILE}"
	: >"${DISKS_FILE}"
	: >"${ADDRESSES_FILE}"
	CLUSTER_EXISTS=1
	NODE_COUNT_OK=1
	NODE_COUNT=3
	POD_READ_OK=1
	CONFIRM_REPLY=""
	ASSUME_YES=1
}

# run_main — run main() in a subshell and record its status in MAIN_RC and its
# combined output in MAIN_OUT. CONFIRM_REPLY feeds the interactive confirmation
# when ASSUME_YES is not set.
#
# The subshell must not be an operand of `||` or `if`: bash suppresses errexit
# inside such a subshell even when it re-runs `set -e` itself, which would let
# main() sail past a declined confirmation and make the abort assertions below
# pass vacuously. Hence set +e around a plain call.
MAIN_RC=0
MAIN_OUT=""
run_main() {
	set +e
	(
		set -e
		main
	) <<<"${CONFIRM_REPLY}" >"${WORKDIR}/main.out" 2>&1
	MAIN_RC=$?
	set -e
	MAIN_OUT="$(cat "${WORKDIR}/main.out")"
}

# --- Assertions -------------------------------------------------------------

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

# call_index NEEDLE — 1-based position of the first call log line containing
# NEEDLE, or 0 when absent.
call_index() {
	local needle="$1" i=0 line
	while IFS= read -r line; do
		i=$((i + 1))
		if [[ "${line}" == *"${needle}"* ]]; then
			echo "${i}"
			return
		fi
	done <"${CALL_LOG}"
	echo 0
}

check_before() {
	local name="$1" first="$2" second="$3" a b
	a="$(call_index "${first}")"
	b="$(call_index "${second}")"
	if ((a > 0 && b > 0 && a < b)); then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${first}' at ${a}, '${second}' at ${b}" >&2
		fails=$((fails + 1))
	fi
}

# call_line NEEDLE — the first call log line containing NEEDLE, empty if absent.
call_line() { grep -m1 -F -- "$1" "${CALL_LOG}" || true; }

echo "scripts/dogfood/delete-test.sh"

# --- the happy path ----------------------------------------------------------

reset_stubs
run_main
check "an existing cluster is deleted" 0 "${MAIN_RC}"
delete_call="$(call_line 'clusters delete')"
check_contains "deletes the named cluster" "clusters delete ${CLUSTER}" "${delete_call}"
check_contains "pins the project on the delete" "--project=${PROJECT}" "${delete_call}"
check_contains "pins the zone on the delete" "--zone=${ZONE}" "${delete_call}"
# The project, billing, APIs, quota and the GitHub App are what make recreate
# possible; this script must never reach past the cluster.
check_not_contains "never deletes the project" "projects delete" "$(cat "${CALL_LOG}")"
check_not_contains "never touches the GitHub App" "gh app" "$(cat "${CALL_LOG}")"

# --- routing goes off the cluster before it disappears -----------------------

# Deleting first leaves a window where a dispatched job routes at a cluster
# mid-deletion; it hangs to its timeout instead of falling back to a hosted
# runner.
check_contains "routes classic CI back to GitHub-hosted" \
	'variable set GAG_RUNNER --body "ubuntu-latest"' "$(call_line 'GAG_RUNNER')"
check_contains "routes e2e CI back to GitHub-hosted" \
	'variable set GAG_E2E_RUNNER --body "ubuntu-latest"' "$(call_line 'GAG_E2E_RUNNER')"
check_before "resets routing before deleting the cluster" \
	"variable set" "clusters delete"

# --- the confirmation quotes what is actually running ------------------------

check_contains "quotes the cluster being deleted" "Cluster: ${CLUSTER}" "${MAIN_OUT}"
check_contains "quotes the live node count" "Nodes currently up:      3" "${MAIN_OUT}"
check_contains "quotes the in-flight worker count" "Worker pods in flight:   0" "${MAIN_OUT}"

reset_stubs
printf '%s\n' \
	"gag-dogfood-ci   runner-ci-aaa" \
	"gag-dogfood-e2e  runner-e2e-bbb" >"${PODS_FILE}"
run_main
check_contains "counts worker pods across every tenant" \
	"Worker pods in flight:   2" "${MAIN_OUT}"

# The false-green direction: a probe that failed must never be quoted back as
# zero, or the operator approves a delete on evidence that does not exist.
reset_stubs
POD_READ_OK=0
run_main
check "an unreadable cluster still reaches the confirmation" 0 "${MAIN_RC}"
check_contains "reports unreadable occupancy as unknown" \
	"Worker pods in flight:   unknown" "${MAIN_OUT}"
check_not_contains "never reports an unreadable cluster as idle" \
	"Worker pods in flight:   0" "${MAIN_OUT}"

reset_stubs
NODE_COUNT_OK=0
run_main
check_contains "reports an unreadable node count as unknown" \
	"Nodes currently up:      unknown" "${MAIN_OUT}"

# The occupancy read happens on a path that may abort, so it pins the context
# per call instead of making it active — a delete that the operator declines
# must not have repointed their kubectl at the dogfood cluster.
reset_stubs
run_main
check_contains "pins the context on the occupancy probe" \
	"--context=gke_${PROJECT}_${ZONE}_${CLUSTER} get pods" "$(call_line 'get pods')"
check_not_contains "never fetches credentials to read occupancy" \
	"get-credentials" "$(cat "${CALL_LOG}")"

# --- the confirmation gates every mutation -----------------------------------

reset_stubs
unset ASSUME_YES
CONFIRM_REPLY="n"
run_main
check "a declined confirmation aborts" 1 "${MAIN_RC}"
check_not_contains "deletes nothing when declined" "clusters delete" "$(cat "${CALL_LOG}")"
check_not_contains "leaves CI routing alone when declined" \
	"variable set" "$(cat "${CALL_LOG}")"
check_not_contains "leaves the kubeconfig alone when declined" \
	"config delete-context" "$(cat "${CALL_LOG}")"

reset_stubs
unset ASSUME_YES
CONFIRM_REPLY="y"
run_main
check "an accepted confirmation proceeds" 0 "${MAIN_RC}"
check_contains "deletes once accepted" "clusters delete" "$(cat "${CALL_LOG}")"

# --- a missing cluster is a no-op, not a failure -----------------------------

reset_stubs
CLUSTER_EXISTS=0
run_main
check "a missing cluster exits clean" 0 "${MAIN_RC}"
check_contains "says there is nothing to delete" "nothing to delete" "${MAIN_OUT}"
check_not_contains "issues no delete for a cluster that is gone" \
	"clusters delete" "$(cat "${CALL_LOG}")"
check_not_contains "does not prompt for a cluster that is gone" \
	"About to DELETE" "${MAIN_OUT}"
# Re-running after a partial failure is the reason this path exists, so it must
# still clear the stale kubeconfig entries a half-finished delete left behind.
check_contains "still prunes the stale kubeconfig entries" \
	"config delete-context gke_${PROJECT}_${ZONE}_${CLUSTER}" "$(cat "${CALL_LOG}")"

# --- post-delete hygiene -----------------------------------------------------

reset_stubs
run_main
log="$(cat "${CALL_LOG}")"
# A lingering context makes a later kubectl target a cluster that no longer
# exists, which surfaces as an auth or timeout error rather than "no such
# cluster".
check_contains "drops the kubeconfig context" \
	"config delete-context gke_${PROJECT}_${ZONE}_${CLUSTER}" "${log}"
check_contains "drops the kubeconfig cluster entry" \
	"config delete-cluster gke_${PROJECT}_${ZONE}_${CLUSTER}" "${log}"
check_contains "drops the kubeconfig user entry" \
	"config unset users.gke_${PROJECT}_${ZONE}_${CLUSTER}" "${log}"
check_before "prunes the kubeconfig only after the delete" \
	"clusters delete" "config delete-context"

reset_stubs
echo "pvc-abc  us-east1-b  100" >"${DISKS_FILE}"
echo "gag-ingress  us-east1  34.1.2.3" >"${ADDRESSES_FILE}"
run_main
# A Retain-policy disk or a hand-reserved address outlives its cluster and bills
# silently, so the survivors are named.
check_contains "names a leftover disk" "pvc-abc" "${MAIN_OUT}"
check_contains "names a leftover address" "gag-ingress" "${MAIN_OUT}"
check_contains "says the survivors bill" "these bill" "${MAIN_OUT}"
# Reported, not swept: an unexpected survivor is a signal worth reading, and
# deleting a disk on a guess is not recoverable.
check_not_contains "never deletes a leftover disk" "disks delete" "$(cat "${CALL_LOG}")"
check_not_contains "never releases a leftover address" \
	"addresses delete" "$(cat "${CALL_LOG}")"

reset_stubs
run_main
check_contains "reports a clean sweep when nothing survived" \
	"no leftover disks" "${MAIN_OUT}"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all delete.sh tests passed"
