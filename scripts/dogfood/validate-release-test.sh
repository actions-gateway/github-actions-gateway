#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/validate-release.sh: resolve_e2e_run_id (the
# step that decides which e2e workflow run the dogfood gate re-runs) and the
# teardown-time failure diagnostics.
#
# Why resolve_e2e_run_id is tested: `gh run rerun` refuses an in-flight run, and
# the gate is normally started minutes after a merge, while that merge's push-run
# is still going. Selecting the run inside the e2e leg aborted the gate *after*
# the node scale-up, RC deploy, and on-demand e2e AGC — a wasted cluster cycle
# and ~5 minutes. The resolution now runs before any billable work, and the paths
# that regress (in-flight wait, timeout, E2E_RUN_ID override) are asserted here.
#
# Why the diagnostics are tested: teardown's scale-to-0 evicts every pod and so
# destroys the evidence (FailedScheduling reasons above all) that explains a
# failed gate (Q355). Both directions matter: the snapshot must run on failure
# BEFORE the stop scripts, and a broken snapshot (e.g. no cluster credentials)
# must never block the teardown that keeps billable nodes from stranding.
#
# The gate script is sourced with VALIDATE_RELEASE_LIB_ONLY=1 so main() does not
# run; `gh` and `run_status` are stubbed, so no network and no cluster.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
VALIDATE_RELEASE_LIB_ONLY=1
export VALIDATE_RELEASE_LIB_ONLY
# shellcheck source=scripts/dogfood/validate-release.sh
source "${REPO_ROOT}/scripts/dogfood/validate-release.sh"

REPO="octo/repo"
E2E_POLL_INTERVAL=1 # keep the wait loop fast

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
CURSOR="${WORKDIR}/cursor"

fails=0

# STATUSES is a queue of statuses returned by successive run_status calls, so a
# test can model "in flight, in flight, then completed". run_status is called
# inside a command substitution (a subshell), so the cursor lives in a file.
STATUSES=()
reset_statuses() {
	STATUSES=("$@")
	echo 0 >"${CURSOR}"
}

# Stubs replacing the two `gh` touchpoints of the resolver.
run_status() {
	local i
	i="$(cat "${CURSOR}")"
	echo $((i + 1)) >"${CURSOR}"
	echo "${STATUSES[i]:-completed}"
}
# FAKE_LATEST is what `gh run list ... --jq '.[0].databaseId'` prints; empty
# models a workflow with no runs at all.
gh() { echo "${FAKE_LATEST:-}"; }

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

echo "scripts/dogfood/validate-release-test.sh"

# The E2E_RUN_ID override still bypasses selection.
reset_statuses completed
E2E_RUN_ID=999 E2E_RESOLVED_RUN_ID=""
resolve_e2e_run_id >/dev/null
check "E2E_RUN_ID override is honored" "999" "${E2E_RESOLVED_RUN_ID}"
unset E2E_RUN_ID

# An in-flight latest run is waited out, not passed straight to `gh run rerun`.
# Not `$(...)`: the function must set E2E_RESOLVED_RUN_ID in THIS shell.
reset_statuses in_progress queued completed
FAKE_LATEST=555 E2E_WAIT_TIMEOUT=60 E2E_RESOLVED_RUN_ID=""
resolve_e2e_run_id >"${WORKDIR}/out"
check "in-flight run is waited out, then used" "555" "${E2E_RESOLVED_RUN_ID}"
check_contains "the wait is announced" "waiting for it to complete" "$(cat "${WORKDIR}/out")"

# Still in flight at the deadline: fail, before the caller does anything billable.
reset_statuses in_progress
FAKE_LATEST=556 E2E_WAIT_TIMEOUT=0 E2E_RESOLVED_RUN_ID=""
if err="$(resolve_e2e_run_id 2>&1)"; then
	echo "FAIL a run still in flight at the deadline must fail the resolver" >&2
	fails=$((fails + 1))
else
	echo "ok   a run still in flight at the deadline fails the resolver"
	check_contains "the timeout error names E2E_WAIT_TIMEOUT" "E2E_WAIT_TIMEOUT" "${err}"
	check_contains "the timeout error names E2E_RUN_ID" "E2E_RUN_ID" "${err}"
fi

# A workflow with no runs at all still fails with the original message.
reset_statuses completed
FAKE_LATEST="" E2E_WAIT_TIMEOUT=60 E2E_RESOLVED_RUN_ID=""
if err="$(resolve_e2e_run_id 2>&1)"; then
	echo "FAIL an empty run selection must fail the resolver" >&2
	fails=$((fails + 1))
else
	check_contains "no run found fails clearly" "no e2e-test.yml run found" "${err}"
fi

# E2E_WORKFLOW is respected (e.g. the e2e-calico.yml lane).
reset_statuses completed
E2E_WORKFLOW=e2e-calico.yml FAKE_LATEST=777 E2E_RESOLVED_RUN_ID=""
resolve_e2e_run_id >"${WORKDIR}/out"
check_contains "E2E_WORKFLOW selects the lane" "e2e-calico.yml run 777" "$(cat "${WORKDIR}/out")"

# --- Q355: failure diagnostics are captured before teardown evicts them ---

# Stub the cluster touchpoints. kubectl records its argv (proving which
# snapshots run) and prints nothing, so the unhealthy-pod describe loop has no
# lines to read.
PROJECT=p ZONE=z CLUSTER=c
KUBECTL_LOG="${WORKDIR}/kubectl.log"
gke_get_credentials_and_verify() { echo "pin $1/$2/$3"; }
kubectl() { echo "kubectl $*" >>"${KUBECTL_LOG}"; }

: >"${KUBECTL_LOG}"
out="$(dump_diagnostics)"
check_contains "diagnostics snapshot the nodes" "get nodes" "$(cat "${KUBECTL_LOG}")"
check_contains "diagnostics snapshot the pods" "get pods -A -o wide" "$(cat "${KUBECTL_LOG}")"
check_contains "diagnostics snapshot the events" "get events" "$(cat "${KUBECTL_LOG}")"

# A failed context pin skips the snapshot without failing — teardown follows.
gke_get_credentials_and_verify() { return 1; }
: >"${KUBECTL_LOG}"
if out="$(dump_diagnostics 2>&1)"; then
	echo "ok   a failed context pin does not fail the dump"
else
	echo "FAIL a failed context pin must not fail the dump" >&2
	fails=$((fails + 1))
fi
check_contains "a failed pin announces the skip" "skipping the snapshot" "${out}"
check "a failed pin runs no kubectl" "" "$(cat "${KUBECTL_LOG}")"

# teardown dumps diagnostics only on failure, and BEFORE the stop scripts run.
# The stop scripts are stubbed via SCRIPT_DIR; WORKDIR is cleared inside the
# subshell so teardown's cleanup cannot delete this test's own workdir.
STUB_DIR="${WORKDIR}/stubs"
mkdir -p "${STUB_DIR}"
printf 'echo "stub e2e-stop"\n' >"${STUB_DIR}/e2e-stop.sh"
printf 'echo "stub stop"\n' >"${STUB_DIR}/stop.sh"
gke_get_credentials_and_verify() { echo "pin"; }

out="$(
	set +e
	SCRIPT_DIR="${STUB_DIR}" WORKDIR=""
	(exit 3)
	teardown 2>&1
)"
check_contains "a failed gate dumps diagnostics" "Failure diagnostics" "${out}"
check_contains "diagnostics run before the stop scripts" "Failure diagnostics" "${out%%stub e2e-stop*}"
check_contains "teardown still stops after the dump" "stub stop" "${out}"
check_contains "teardown reports the gate's exit code" "(exit 3)" "${out}"

out="$(
	set +e
	SCRIPT_DIR="${STUB_DIR}" WORKDIR=""
	(exit 0)
	teardown 2>&1
)"
if [[ "${out}" == *"Failure diagnostics"* ]]; then
	echo "FAIL a green gate must not dump diagnostics" >&2
	fails=$((fails + 1))
else
	echo "ok   a green gate does not dump diagnostics"
fi
check_contains "a green teardown still stops" "stub stop" "${out}"

if ((fails > 0)); then
	echo "validate-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "validate-release-test: ok"
