#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/validate-release.sh: the pre-billable steps —
# resolve_e2e_run_id (which e2e run the gate re-runs) and preflight_cosign (the
# local-tool check for the CRD smoke) — plus the teardown-time failure
# diagnostics.
#
# Why the pre-billable steps are tested: both used to fail LATE. `gh run rerun`
# refuses an in-flight run — and the latest run usually is one, minutes after a
# merge — which aborted the gate *after* the node scale-up, RC deploy, and
# on-demand e2e AGC (PR #710); a missing .build/cosign aborted the CRD-smoke leg
# ~25 minutes in, after a full cluster cycle (Q356). Both now run before any
# billable work, and the paths that regress (in-flight wait, timeout, E2E_RUN_ID
# override, missing/overridden cosign) are asserted here.
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

# A missing cosign binary fails the preflight — before anything billable.
if err="$(COSIGN="${WORKDIR}/no-such-cosign" preflight_cosign 2>&1)"; then
	echo "FAIL a missing cosign binary must fail the preflight" >&2
	fails=$((fails + 1))
else
	echo "ok   a missing cosign binary fails the preflight"
	check_contains "the cosign error says how to fix it" "make cosign" "${err}"
fi

# A present binary (via the COSIGN override) resolves into COSIGN_BIN.
printf '#!/bin/sh\n' >"${WORKDIR}/fake-cosign"
chmod +x "${WORKDIR}/fake-cosign"
COSIGN_BIN=""
COSIGN="${WORKDIR}/fake-cosign" preflight_cosign
check "COSIGN override resolves into COSIGN_BIN" "${WORKDIR}/fake-cosign" "${COSIGN_BIN}"

# A non-executable file is as unusable as a missing one (e.g. a partial download).
printf '' >"${WORKDIR}/noexec-cosign"
if COSIGN="${WORKDIR}/noexec-cosign" preflight_cosign 2>/dev/null; then
	echo "FAIL a non-executable cosign must fail the preflight" >&2
	fails=$((fails + 1))
else
	echo "ok   a non-executable cosign fails the preflight"
fi

# --- The sizing-profile leg: the gate must be able to FAIL on a dead profile ---
#
# The whole point of sizing_leg is that a profile which silently falls back to
# Static still provisions a healthy pod and still runs the matrix green, so
# every other leg reports success. These assertions pin that the leg can
# actually fail — a gate that cannot fail is decoration — and, just as
# importantly, that it does NOT fail on the one condition that is not a defect:
# Throughput's sample history not having matured yet.

# stub_sizing_kubectl installs a kubectl stub answering the three jsonpath reads
# sizing_leg makes: $1 = ci-e2e profile state, $2 = ci profile state,
# $3 = ci sample counts.
stub_sizing_kubectl() {
	local e2e_state="$1" ci_state="$2" ci_samples="$3"
	eval "kubectl() {
		case \"\$*\" in
			*runnerset\ ci-e2e*) printf '%s' '${e2e_state}' ;;
			*sizingRecommendation*) printf '%s' '${ci_samples}' ;;
			*runnerset\ ci\ *) printf '%s' '${ci_state}' ;;
		esac
	}"
}

# sizing_leg pins the cluster context before it reads anything, so the target
# vars must be set even though the stub ignores them (set -u).
PROJECT=p ZONE=z CLUSTER=c
gke_get_credentials_and_verify() { echo "pin"; }

# The happy path: NodeShare Active, the sampled worker matches the envelope.
stub_sizing_kubectl "Active" "Active" "31 27"
printf '%s' "${EXPECTED_NODESHARE_CPU}" >"${WORKDIR}/e2e-runner-cpu"
if out="$(sizing_leg 2>&1)"; then
	echo "ok   an actuating NodeShare passes the leg"
else
	echo "FAIL an actuating NodeShare must pass the leg" >&2
	fails=$((fails + 1))
fi
check_contains "the leg reports the derived request" "cpu request=${EXPECTED_NODESHARE_CPU}" "${out}"
check_contains "an active Throughput is reported as validated" "Throughput IS actuating" "${out}"

# A profile that fell back to Static is the exact failure this leg exists for.
stub_sizing_kubectl "AwaitingSamples" "Active" "31"
if out="$(sizing_leg 2>&1)"; then
	echo "FAIL a non-Active NodeShare must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   a non-Active NodeShare fails the gate"
fi
check_contains "the failure explains the fallback" "static values" "${out}"

# An empty state (no profile configured at all) must fail the same way — this is
# the pre-2026-07-26 condition, where the gate validated no profile whatsoever.
stub_sizing_kubectl "" "" ""
if sizing_leg >/dev/null 2>&1; then
	echo "FAIL an unconfigured NodeShare must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   an unconfigured NodeShare fails the gate"
fi

# Active but deriving the wrong number: the manifest envelope and this gate's
# expectation drifted apart.
stub_sizing_kubectl "Active" "Active" "31"
printf '%s' "999m" >"${WORKDIR}/e2e-runner-cpu"
if out="$(sizing_leg 2>&1)"; then
	echo "FAIL a mismatched derived request must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   a mismatched derived request fails the gate"
fi
check_contains "the mismatch names both values" "want '${EXPECTED_NODESHARE_CPU}'" "${out}"

# No worker sampled: the state assertion still holds, and the skip is announced
# rather than passing silently.
stub_sizing_kubectl "Active" "Active" "31"
rm -f "${WORKDIR}/e2e-runner-cpu"
if out="$(sizing_leg 2>&1)"; then
	echo "ok   an unsampled worker does not fail the leg"
else
	echo "FAIL an unsampled worker must not fail the leg" >&2
	fails=$((fails + 1))
fi
check_contains "the unsampled run announces the skip" "NOT checked" "${out}"

# Throughput below its sample threshold is NOT a release blocker — but it must
# be said out loud, because the profile then ships live-unvalidated.
stub_sizing_kubectl "Active" "AwaitingSamples" "4 6"
printf '%s' "${EXPECTED_NODESHARE_CPU}" >"${WORKDIR}/e2e-runner-cpu"
if out="$(sizing_leg 2>&1)"; then
	echo "ok   an immature Throughput history does not block the release"
else
	echo "FAIL an immature Throughput history must not block the release" >&2
	fails=$((fails + 1))
fi
check_contains "an unvalidated Throughput is called out" "NOT VALIDATED THIS RUN" "${out}"
check_contains "the sample counts are printed" "sampleCounts=[4 6]" "${out}"
# Q488: AwaitingSamples is the ONLY state whose cause is short history, so it is
# the only one allowed to say so — and it must not repeat the old advice to
# deploy spec.sizing ahead of the RC window, which sampling never depended on.
check_contains "an immature history names the sample shortfall" "short of ${MIN_SAMPLES_FOR_DRIFT} samples" "${out}"
check_contains "an immature history denies the soak requirement" "not a multi-day soak" "${out}"

# Q488: an EMPTY ci state is a different defect — spec.sizing never reached the
# cluster — and must not be reported as a sample shortfall. The distinction is
# load-bearing: start.sh cannot deploy a CR edit, so an operator sent to wait for
# samples would wait forever on a tenant that has no profile configured at all.
stub_sizing_kubectl "Active" "" ""
printf '%s' "${EXPECTED_NODESHARE_CPU}" >"${WORKDIR}/e2e-runner-cpu"
if out="$(sizing_leg 2>&1)"; then
	echo "ok   an undeployed Throughput does not block the release"
else
	echo "FAIL an undeployed Throughput must not block the release" >&2
	fails=$((fails + 1))
fi
check_contains "an undeployed Throughput is called out" "NOT VALIDATED THIS RUN" "${out}"
check_contains "an undeployed Throughput names the deploy gap" "spec.sizing is not on the live" "${out}"
check_contains "an undeployed Throughput names start.sh as unable to apply" "never applies CRs" "${out}"
if [[ "${out}" == *"short of ${MIN_SAMPLES_FOR_DRIFT} samples"* ]]; then
	echo "FAIL an undeployed Throughput must not be blamed on sample history" >&2
	fails=$((fails + 1))
else
	echo "ok   an undeployed Throughput is not blamed on sample history"
fi

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
