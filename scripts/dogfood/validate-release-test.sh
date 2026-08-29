#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/validate-release.sh: the pre-billable steps —
# settle_e2e_lane (the lane must be idle before the gate dispatches into its
# concurrency group) and preflight_cosign (the local-tool check for the CRD
# smoke) — plus dispatch_e2e_run (the run-scoped e2e dispatch) and the
# teardown-time failure diagnostics.
#
# Why the pre-billable steps are tested: both used to fail LATE. A run
# dispatched into a busy concurrency group parks in its single pending slot,
# where the next push to main cancels it — and the latest run usually is in
# flight, minutes after a merge — which would abort the gate *after* the node
# scale-up, RC deploy, and on-demand e2e AGC (PR #710 hit the rerun-era analog);
# a missing .build/cosign aborted the CRD-smoke leg ~25 minutes in, after a full
# cluster cycle (Q356). Both now run before any billable work, and the paths
# that regress (in-flight wait, timeout, missing/overridden cosign) are
# asserted here. dispatch_e2e_run is tested because it carries the run-scoped
# routing (the `runner` input) that replaced the repo-wide GAG_E2E_RUNNER flip
# (2026-07-31 incident) — a dispatch that silently dropped the input would
# re-run the matrix on GitHub-hosted runners and validate nothing.
#
# Why the diagnostics are tested: teardown's scale-to-0 evicts every pod and so
# destroys the evidence (FailedScheduling reasons above all) that explains a
# failed gate (Q355). Both directions matter: the snapshot must run on failure
# BEFORE the stop scripts, and a broken snapshot (e.g. no cluster credentials)
# must never block the teardown that keeps billable nodes from stranding.
#
# Why the reclaim is tested: it is the one path here that tears down a cluster
# the running process never scaled up (Q640), so its trigger has to be exactly
# an orphaned lease and nothing else — a free target and a live gate must both
# leave the cluster alone, and the lease has to survive a reclaim that could not
# finish, or the leak it records is lost. lease-test.sh covers the lease states
# themselves; this covers what the gate does with each of them.
#
# The gate script is sourced with VALIDATE_RELEASE_LIB_ONLY=1 so main() does not
# run; `gh` and `run_status` are stubbed, so no network and no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
VALIDATE_RELEASE_LIB_ONLY=1
export VALIDATE_RELEASE_LIB_ONLY
# Set before sourcing lib/lease.sh (via the gate): its default resolves under
# $HOME at source time, and no test may write there.
RELEASE_LEASE_DIR="${REPO_ROOT}/tmp/validate-release-test-lease.$$"
export RELEASE_LEASE_DIR
# The teardown assertions below drive real progress events. Disable the stream
# before sourcing so this suite cannot leave a status file behind claiming a
# failed gate — a sentinel started afterwards would read it and report one.
RELEASE_PROGRESS_FILE=""
export RELEASE_PROGRESS_FILE
# Scoped as well as disabled: progress_init removes RELEASE_STATUS_FILE whether
# or not the stream is on, so disabling the stream alone leaves the live default
# reachable from here (Q777).
RELEASE_STATUS_FILE="${REPO_ROOT}/tmp/validate-release-test-status.$$.json"
export RELEASE_STATUS_FILE
# shellcheck source=scripts/dogfood/validate-release.sh
source "${REPO_ROOT}/scripts/dogfood/validate-release.sh"

REPO="octo/repo"
E2E_POLL_INTERVAL=1 # keep the wait loop fast

WORKDIR="$(mktemp -d)"
# Scratch for the sections below the teardown tests, which shadow WORKDIR inside
# subshells (teardown deletes whatever it names) — a separate dir keeps those
# reads out of the shadowed variable.
SCRATCH="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}" "${SCRATCH}" "${RELEASE_LEASE_DIR}"' EXIT
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

# Stubs replacing the `gh` touchpoints. run_status consumes the STATUSES queue;
# gh logs its argv (so a test can assert what was dispatched) and consumes the
# GH_OUTPUTS queue, repeating the last entry once exhausted — so a test scripts
# only the outputs that differ. `EMPTY` prints nothing (a workflow with no runs).
run_status() {
	local i
	i="$(cat "${CURSOR}")"
	echo $((i + 1)) >"${CURSOR}"
	echo "${STATUSES[i]:-completed}"
}

GH_LOG="${WORKDIR}/gh.log"
GH_OUTPUTS_FILE="${WORKDIR}/gh-outputs"
reset_gh() {
	printf '%s\n' "$@" >"${GH_OUTPUTS_FILE}"
	: >"${GH_LOG}"
}
scripted_gh() {
	printf '%s\n' "$*" >>"${GH_LOG}"
	local head
	head="$(head -n 1 "${GH_OUTPUTS_FILE}")"
	if (($(wc -l <"${GH_OUTPUTS_FILE}") > 1)); then
		tail -n +2 "${GH_OUTPUTS_FILE}" >"${GH_OUTPUTS_FILE}.next"
		mv "${GH_OUTPUTS_FILE}.next" "${GH_OUTPUTS_FILE}"
	fi
	[[ "${head}" == "EMPTY" ]] || printf '%s\n' "${head}"
}
gh() { scripted_gh "$@"; }

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

# Pacing sleeps are stubbed out: every sleep in the tested paths is loop pacing,
# and the dispatch-timeout test otherwise takes 24 real seconds.
sleep() { :; }

echo "scripts/dogfood/validate-release-test.sh"

# --- settle_e2e_lane: the lane must be idle before the gate dispatches into it ---

# An in-flight latest run is waited out, not dispatched into.
reset_gh 555
reset_statuses in_progress queued completed
E2E_WAIT_TIMEOUT=60
if settle_e2e_lane >"${WORKDIR}/out"; then
	echo "ok   an in-flight run is waited out, then the lane settles"
else
	echo "FAIL an in-flight run that completes must settle the lane" >&2
	fails=$((fails + 1))
fi
check_contains "the wait is announced" "waiting for it to complete" "$(cat "${WORKDIR}/out")"

# Still in flight at the deadline: fail, before the caller does anything billable.
reset_gh 556
reset_statuses in_progress
E2E_WAIT_TIMEOUT=0
if err="$(settle_e2e_lane 2>&1)"; then
	echo "FAIL a run still in flight at the deadline must fail the settle" >&2
	fails=$((fails + 1))
else
	echo "ok   a run still in flight at the deadline fails the settle"
	check_contains "the timeout error names E2E_WAIT_TIMEOUT" "E2E_WAIT_TIMEOUT" "${err}"
	check_contains "the timeout error explains the pending-slot hazard" "pending slot" "${err}"
fi

# A workflow with no runs at all is a FREE lane, not a failure — there is
# nothing to collide with (the rerun-era resolver had to fail here; the
# dispatcher does not need a prior run to exist).
reset_gh EMPTY
reset_statuses completed
E2E_WAIT_TIMEOUT=60
if out="$(settle_e2e_lane 2>&1)"; then
	echo "ok   a workflow with no runs settles as a free lane"
else
	echo "FAIL a workflow with no runs must settle as a free lane" >&2
	fails=$((fails + 1))
fi
check_contains "the free lane is announced" "lane is free" "${out}"

# E2E_WORKFLOW is respected (e.g. the e2e-calico.yml lane).
reset_gh 777
reset_statuses completed
E2E_WORKFLOW=e2e-calico.yml E2E_WAIT_TIMEOUT=60
settle_e2e_lane >"${WORKDIR}/out"
check_contains "E2E_WORKFLOW selects the lane" "e2e-calico.yml" "$(cat "${WORKDIR}/out")"
unset E2E_WORKFLOW

# --- Q854: a transient gh read is retried; the dispatch is not ---------------
#
# One `HTTP 401: Bad credentials` at 645s of the settle wait killed a
# v1.5.0-rc.1 run after 43 good polls, with twenty-odd calls succeeding
# immediately afterwards. The gate polls `gh` for the whole of an hour-long
# billable window, so a single denial anywhere in it must not be terminal.
#
# The direction that would cost a run the other way is the last case here: the
# dispatch is the one call that is not a read, and repeating it would queue a
# second e2e run into the concurrency group.

GH_ATTEMPTS="${WORKDIR}/gh-attempts"
GH_FAILS="${WORKDIR}/gh-fails"
GH_FAIL_MATCH=""
GH_FLAKY_OUTPUT=""

# flaky_gh — a `gh` that denies its first N calls whose argv contains
# GH_FAIL_MATCH, then serves GH_FLAKY_OUTPUT. The counters live in files because
# every call site here runs gh inside a command substitution.
flaky_gh() {
	local n
	if [[ "$*" == *"${GH_FAIL_MATCH}"* ]]; then
		n=$(($(cat "${GH_ATTEMPTS}") + 1))
		echo "${n}" >"${GH_ATTEMPTS}"
		if ((n <= $(cat "${GH_FAILS}"))); then
			echo "gh: HTTP 401: Bad credentials (HTTP 401)" >&2
			return 1
		fi
	fi
	printf '%s\n' "${GH_FLAKY_OUTPUT}"
}

# arm_flaky MATCH FAILS OUTPUT — install flaky_gh with a denial budget.
arm_flaky() {
	GH_FAIL_MATCH="$1"
	echo 0 >"${GH_ATTEMPTS}"
	echo "$2" >"${GH_FAILS}"
	GH_FLAKY_OUTPUT="$3"
	gh() { flaky_gh "$@"; }
}

GH_RETRIES=5

# The measured shape: two denials, then the answer.
arm_flaky "run view" 2 "in_progress"
check "a transient gh denial is retried to an answer" "in_progress" \
	"$(gh_retry run view 999 --json status)"
check "  ...and it took the two retries" 3 "$(cat "${GH_ATTEMPTS}")"

# Bounded, so a real outage fails the gate rather than holding billable nodes
# on a schedule that never ends.
arm_flaky "run view" 99 ""
retry_rc=0
retry_err="$(gh_retry run view 999 2>&1)" || retry_rc=$?
check "a persistent denial still fails" 1 "${retry_rc}"
check "the retries are bounded at GH_RETRIES+1" 6 "$(cat "${GH_ATTEMPTS}")"
check_contains "an exhausted retry says how many it tried" "after 6 attempts" "${retry_err}"

# Through the real caller, which is what the 401 actually killed. The assertion
# is on the answer, not on the exit status: a denied read leaves run_id empty,
# and an empty run_id reads as a lane with no prior run — settling clean while
# having learned nothing. So the resolved run id is what has to survive.
arm_flaky "run list" 2 "555"
reset_statuses completed
E2E_WAIT_TIMEOUT=60
settle_rc=0
settle_out="$(settle_e2e_lane 2>&1)" || settle_rc=$?
check "a denied lane read no longer kills the settle" 0 "${settle_rc}"
check_contains "  ...and the lane read still resolves its run" \
	"lane settled — latest e2e-test.yml run 555" "${settle_out}"

# The dispatch is NOT a read. `latest_dispatch_run_id` answers normally; the
# dispatch itself is denied once, and must be attempted exactly once.
arm_flaky "workflow run" 99 "100"
dispatch_rc=0
dispatch_e2e_run >/dev/null 2>&1 || dispatch_rc=$?
check "a denied dispatch fails" 1 "${dispatch_rc}"
check "a dispatch is never retried" 1 "$(cat "${GH_ATTEMPTS}")"

# Restore the argv-logging stub the sections below script.
gh() { scripted_gh "$@"; }

# --- dispatch_e2e_run: the run-scoped routing that replaced the repo-wide flip ---

# The dispatched run is resolved by the newest dispatch id changing from the
# pre-dispatch baseline. gh outputs: baseline list, the (ignored) dispatch, the
# post-dispatch list. Not `$(...)`: E2E_RESOLVED_RUN_ID must land in THIS shell.
reset_gh 100 EMPTY 200
E2E_RESOLVED_RUN_ID=""
if dispatch_e2e_run >"${WORKDIR}/out"; then
	echo "ok   a dispatched run resolves to the new run id"
else
	echo "FAIL a dispatched run that appears must resolve" >&2
	fails=$((fails + 1))
fi
check "the new dispatch run id is resolved" "200" "${E2E_RESOLVED_RUN_ID}"
# The load-bearing argument: without the runner input the matrix re-runs on
# GitHub-hosted runners and the gate validates nothing.
check_contains "the dispatch pins the runner input" 'runner="gag-ci-e2e"' "$(cat "${GH_LOG}")"
check_contains "the dispatch targets the workflow" "workflow run e2e-test.yml" "$(cat "${GH_LOG}")"
check_contains "the dispatch pins the ref" "--ref main" "$(cat "${GH_LOG}")"

# A first-ever dispatch (no baseline run) still resolves.
reset_gh EMPTY EMPTY 300
E2E_RESOLVED_RUN_ID=""
dispatch_e2e_run >/dev/null
check "a first-ever dispatch resolves from an empty baseline" "300" "${E2E_RESOLVED_RUN_ID}"

# A dispatch whose run never appears must fail rather than watch the baseline
# run (rerun-era bug shape: watching a stale run reads its old green result).
reset_gh 100 EMPTY 100
E2E_RESOLVED_RUN_ID=""
if err="$(dispatch_e2e_run 2>&1)"; then
	echo "FAIL a dispatch that never appears must fail" >&2
	fails=$((fails + 1))
else
	echo "ok   a dispatch that never appears fails instead of watching a stale run"
fi
check "  ...and resolves no run id" "" "${E2E_RESOLVED_RUN_ID}"

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

# --- Q631: the e2e pool's CPU budget is reserved before CI can compete for it -
#
# The gate is its own competitor: the deploy leg routes CI to GAG, whose
# `workers` pool autoscales out of the same project-wide CPUS_ALL_REGIONS budget
# the e2e leg then needs 16 vCPU from. When the budget cannot cover both, the
# autoscaler refuses the e2e scale-up as a bare FailedScaleUp that names no
# quota, ~25 minutes in and with a full cluster cycle already paid for. Two
# v1.3.0-rc.5 runs died there.
#
# quota-test.sh owns the arithmetic; this owns the composition — which numbers
# feed it, that the cap is applied only when it binds, and that the ceiling is
# always put back. The lib readers are stubbed rather than gcloud, so a case is
# a set of live values rather than a gcloud format string.

PROJECT=p ZONE=z CLUSTER=c
GCLOUD_LOG="${WORKDIR}/quota-gcloud.log"
gke_get_credentials_and_verify() { :; }

# The live values the stubbed readers below serve. Globals rather than closed-
# over locals: a bash function body is re-read at call time, so a `local` set
# while defining it is long gone by then.
FAKE_BUDGET=""
FAKE_USED=""
FAKE_SYSTEM_NODES=""
FAKE_WORKERS_MAX=""

global_cpu_budget() { echo "${FAKE_BUDGET} ${FAKE_USED}"; }
required_system_nodes() { echo "${FAKE_SYSTEM_NODES}"; }
pool_machine_type() {
	case "$1" in
		default-pool) echo "e2-standard-2" ;;
		e2e) echo "n2-standard-8" ;;
		workers) echo "e2-standard-4" ;;
	esac
}
# The normalized contract, space-separated, and nothing at all for a pool with
# autoscaling off. What this stub must not do is model gcloud: the real
# separator and its leading empty field are asserted against the live output in
# quota-test.sh, and a stub that sent a literal min here is what hid the parse.
pool_autoscaling() {
	case "$1" in
		e2e) echo "0 2" ;;
		workers) [[ -z "${FAKE_WORKERS_MAX}" ]] || echo "0 ${FAKE_WORKERS_MAX}" ;;
	esac
}
set_pool_autoscale_max() { echo "set $*" >>"${GCLOUD_LOG}"; }

# stub_quota BUDGET USED SYSTEM_NODES WORKERS_MAX — model the live cluster the
# preflight reads. Machine types are the real ones: e2-standard-2 system,
# n2-standard-8 e2e (max 2), e2-standard-4 workers.
stub_quota() {
	FAKE_BUDGET="$1"
	FAKE_USED="$2"
	FAKE_SYSTEM_NODES="$3"
	FAKE_WORKERS_MAX="$4"
	: >"${GCLOUD_LOG}"
	WORKERS_MAX_CAP=""
	WORKERS_MAX_RESTORE=""
}

# Today's live shape (measured 2026-08-12): a 64-vCPU limit, nothing in use, two
# always-on tenants. 64 - 4 system - 16 e2e = 44, which is 11 e2-standard-4
# nodes — more than the pool's configured 8, so nothing is capped. A gate that
# throttled CI here would be reserving capacity nobody is contending for.
stub_quota 64 0 2 8
quota_preflight >/dev/null
check "today's budget derives an 11-node ceiling" 11 "${WORKERS_MAX_CAP}"
reserve_cpu_budget >/dev/null
check "a ceiling that already fits is not touched" "" "$(cat "${GCLOUD_LOG}")"
check "an untouched ceiling leaves nothing to restore" "" "${WORKERS_MAX_RESTORE}"

# The rc.5 shape: the same cluster against the 32-vCPU limit that killed two
# runs. The reservation now binds — `workers` is held at 3 nodes so the e2e
# pool's 16 vCPU stays available.
stub_quota 32 0 2 8
quota_preflight >/dev/null
check "the old 32-vCPU limit derives a 3-node ceiling" 3 "${WORKERS_MAX_CAP}"
reserve_cpu_budget >/dev/null
check "a binding cap is applied to the workers pool" "set workers 0 3" "$(cat "${GCLOUD_LOG}")"
check "a binding cap records the ceiling to restore" "0 8" "${WORKERS_MAX_RESTORE}"

# ...and teardown puts it back. A ceiling left low outlives the gate and
# throttles everyone's CI.
: >"${GCLOUD_LOG}"
restore_cpu_budget >/dev/null
check "teardown restores the configured ceiling" "set workers 0 8" "$(cat "${GCLOUD_LOG}")"
check "a completed restore is not repeated" "" "${WORKERS_MAX_RESTORE}"

# A restore that cannot reach gcloud must not abort teardown before the lease is
# released — stranded billable nodes cost more than a throttled CI pool — but it
# must say so, and name the command that fixes it.
stub_quota 32 0 2 8
quota_preflight >/dev/null
reserve_cpu_budget >/dev/null
set_pool_autoscale_max() { return 1; }
restore_rc=0
restore_out="$(restore_cpu_budget 2>&1)" || restore_rc=$?
check "a failed restore does not fail teardown" 0 "${restore_rc}"
check_contains "a failed restore names the fix" "--max-nodes=8" "${restore_out}"
set_pool_autoscale_max() { echo "set $*" >>"${GCLOUD_LOG}"; }

# A third always-on tenant grows the system pool (lib/pool.sh derives one node
# per tenant AGC), which comes out of the same budget: 32 - 6 - 16 = 10, two
# worker nodes rather than three.
stub_quota 32 0 3 8
quota_preflight >/dev/null
check "a third tenant's system node comes out of the workers share" 2 "${WORKERS_MAX_CAP}"

# A benchmark pool left up after a campaign is the realistic way the budget
# shrinks under an otherwise unchanged gate: 4x e2-standard-4 workers-od is
# 16 vCPU of `used`, which the preflight reads live rather than predicting.
stub_quota 64 16 2 8
quota_preflight >/dev/null
check "capacity already in use is taken off the workers share" 7 "${WORKERS_MAX_CAP}"

# Nothing left for CI at all — the system and e2e pools consume the budget
# exactly. Fail here, where failure is free, rather than after the deploy. The
# remedy is to free capacity, not to raise the limit: a bigger number moves the
# collision rather than removing it.
stub_quota 20 0 2 8
preflight_rc=0
preflight_out="$(quota_preflight 2>&1)" || preflight_rc=$?
check "a budget that cannot cover both legs fails the preflight" 1 "${preflight_rc}"
check_contains "the failure names the quota" "CPUS_ALL_REGIONS" "${preflight_out}"
check_contains "the failure says to free capacity, not raise the limit" \
	"Free capacity before raising the limit" "${preflight_out}"

# An unreadable quota is not an unlimited one: the gate refuses rather than
# deploying blind on the constraint that starves it.
stub_quota 64 0 2 8
global_cpu_budget() { return 1; }
preflight_rc=0
preflight_out="$(quota_preflight 2>&1)" || preflight_rc=$?
check "an unreadable quota fails the preflight" 1 "${preflight_rc}"
check_contains "the unreadable-quota error says why it will not proceed" \
	"will not run blind" "${preflight_out}"
global_cpu_budget() { echo "${FAKE_BUDGET} ${FAKE_USED}"; }

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

# --- Q640: an orphaned run is reclaimed; nothing else is ---------------------
#
# The killed-gate state is a lease for this target whose owning process is gone.
# The stop scripts are stubbed through SCRIPT_DIR and log to a file (they run as
# child processes), so these assert both that the reclaim tears down and — the
# direction that would cost somebody their cluster — that it does not.

PROJECT=p ZONE=z CLUSTER=c
STOP_LOG="${SCRATCH}/stop.log"
LEASE_FILE="$(lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}")"
export STOP_LOG LEASE_FILE
cat >"${STUB_DIR}/e2e-stop.sh" <<'STUB'
printf 'e2e-stop\n' >>"${STOP_LOG}"
exit "${RECLAIM_E2E_STOP_RC:-0}"
STUB
# The stop stub records whether the lease was still there while it ran, which is
# what makes "released last" observable instead of read off the source.
cat >"${STUB_DIR}/stop.sh" <<'STUB'
if [[ -f "${LEASE_FILE}" ]]; then
	printf 'stop lease=held\n' >>"${STOP_LOG}"
else
	printf 'stop lease=released\n' >>"${STOP_LOG}"
fi
exit "${RECLAIM_STOP_RC:-0}"
STUB
SCRIPT_DIR="${STUB_DIR}"

# The gate's own pid is what a real acquire records, so a stub decides whether
# that pid still looks like a running gate.
GATE_CMD="bash scripts/dogfood/validate-release.sh v1.3.0-rc.4"
lease_process_command() { [[ "$1" == "$$" ]] && echo "${OWNER_ALIVE:+${GATE_CMD}}"; }

# arm_lease STATE — put the target's lease into STATE and clear the call log.
arm_lease() {
	rm -rf "${RELEASE_LEASE_DIR}"
	: >"${STOP_LOG}"
	case "$1" in
	free) OWNER_ALIVE="" ;;
	held)
		OWNER_ALIVE=1
		lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" v1.3.0-rc.4
		;;
	orphaned)
		OWNER_ALIVE=1
		lease_acquire "${PROJECT}" "${ZONE}" "${CLUSTER}" v1.3.0-rc.4
		OWNER_ALIVE=""
		;;
	esac
}

run_reclaim() {
	set +e
	reclaim_orphaned_gate >"${SCRATCH}/reclaim.out" 2>&1
	RECLAIM_RC=$?
	set -e
	RECLAIM_OUT="$(cat "${SCRATCH}/reclaim.out")"
}

# A target no lease claims is the case that must cost nothing: an operator who
# scaled the cluster up by hand leaves exactly this state, and a mechanism that
# read nodes-are-up as an orphan would delete their environment.
arm_lease free
run_reclaim
check "an unclaimed target reclaims nothing" 0 "${RECLAIM_RC}"
check "an unclaimed target runs no teardown" "" "$(cat "${STOP_LOG}")"

# The Q640 state itself.
arm_lease orphaned
run_reclaim
check "an orphaned gate's cluster is torn back down" 0 "${RECLAIM_RC}"
check "the reclaim runs both stop scripts, e2e first" \
	"e2e-stop
stop lease=held" "$(cat "${STOP_LOG}")"
check_contains "the reclaim says what it found" "killed before it finished tearing down" "${RECLAIM_OUT}"
check "a completed reclaim frees the target" "free" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A gate that is still running owns its cluster. Reclaiming here would delete
# the environment out from under it — strictly worse than the leak.
arm_lease held
run_reclaim
check "a live gate's cluster is not reclaimed" 1 "${RECLAIM_RC}"
check "a live gate's cluster sees no teardown" "" "$(cat "${STOP_LOG}")"
check_contains "the refusal names the other gate" "already owns" "${RECLAIM_OUT}"
check "a refused reclaim leaves the live lease alone" "held" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A pid from another host cannot be checked at all, so it is reported, never
# acted on.
arm_lease orphaned
awk '{ sub(/^host=.*/, "host=someone-elses-mac"); print }' \
	"$(lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}")" >"${SCRATCH}/foreign"
cp "${SCRATCH}/foreign" "$(lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}")"
run_reclaim
check "another host's lease is not reclaimed" 1 "${RECLAIM_RC}"
check "another host's lease sees no teardown" "" "$(cat "${STOP_LOG}")"
check_contains "the refusal explains why it cannot judge" "cannot judge" "${RECLAIM_OUT}"

# A reclaim that could not finish must keep the record. Discarding it would
# leave the nodes up with nothing left that knows they are orphaned — the
# original bug, re-created by the fix.
arm_lease orphaned
RECLAIM_STOP_RC=1
export RECLAIM_STOP_RC
run_reclaim
unset RECLAIM_STOP_RC
check "a failed reclaim fails the gate" 1 "${RECLAIM_RC}"
check "a failed reclaim keeps the lease for the next attempt" "orphaned" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check_contains "the failure says the lease is kept" "next run retries" "${RECLAIM_OUT}"

# --- teardown releases the lease, and only its own ---------------------------

# Released last, after the stop scripts: a teardown killed mid-drain must still
# read as orphaned to the next run.
arm_lease held
(
	set +e
	WORKDIR=""
	teardown
) >/dev/null 2>&1
check "a completed teardown releases its own lease" "free" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check "the lease is still held while the stop scripts run" \
	"stop lease=held" "$(grep -F 'stop lease' "${STOP_LOG}")"

# The loser of a two-gate race exits through the same teardown; it must not
# clear the winner's lease on its way out.
arm_lease held
awk '{ sub(/^pid=.*/, "pid=999999"); print }' \
	"$(lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}")" >"${SCRATCH}/other"
cp "${SCRATCH}/other" "$(lease_path "${PROJECT}" "${ZONE}" "${CLUSTER}")"
lease_process_command() { [[ "$1" == "999999" ]] && echo "${GATE_CMD}"; }
(
	set +e
	WORKDIR=""
	teardown
) >/dev/null 2>&1
check "a teardown never releases another gate's lease" "held" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# --- progress_reset_unless_held: a spent stream must not outlive its run ---
#
# The gate writes no event until after preflight, so until the stream is emptied
# every reader renders whatever the last run left. That is how release-sentinel
# reported `passed` for a v1.4.0-rc.2 run while a v1.5.0-rc.1 gate was still in
# its settle wait, having started nothing.
#
# The teardown block above narrowed lease_process_command to one foreign pid, so
# restore the liveness stub these cases need: without it `held` arms an orphan
# and the state under test never occurs. Each case asserts the lease state it
# claims rather than trusting arm_lease.
lease_process_command() { [[ "$1" == "$$" ]] && echo "${OWNER_ALIVE:+${GATE_CMD}}"; }

STALE_STREAM="${SCRATCH}/stale-progress.jsonl"
seed_spent_stream() {
	cat >"${STALE_STREAM}" <<'STREAM'
{"kind":"phase","t":1786326912,"phase":"gate","state":"start","detail":"v1.4.0-rc.2"}
{"kind":"phase","t":1786334974,"phase":"gate","state":"done","detail":"validation PASSED for v1.4.0-rc.2"}
STREAM
}

reset_with_lease() {
	arm_lease "$1"
	seed_spent_stream
	RELEASE_PROGRESS_FILE="${STALE_STREAM}" RELEASE_STATUS_FILE="" progress_reset_unless_held
}

reset_with_lease free
check "the free case really is free" "free" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check "a spent stream is emptied before preflight" "" "$(cat "${STALE_STREAM}")"
check "an emptied stream renders preflight, not the last verdict" "preflight" \
	"$(progress_status_json "${STALE_STREAM}" | jq -r .gate)"
check "an emptied stream carries no RC" "null" \
	"$(progress_status_json "${STALE_STREAM}" | jq -r '.rc // "null"')"

# The one state that must not be cleared: that stream belongs to the gate that
# is still writing it, and lease_acquire refuses this run moments later.
reset_with_lease held
check "the held case really is held" "held" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check_contains "a live gate's stream is left alone" "v1.4.0-rc.2" "$(cat "${STALE_STREAM}")"

# A killed gate has no live owner, so its stream is spent like any other.
reset_with_lease orphaned
check "the orphaned case really is orphaned" "orphaned" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
check "an orphaned gate's stream is emptied" "" "$(cat "${STALE_STREAM}")"

# --- capacity_leg: the admission ladder is evaluated, and its quota rung binds ---
#
# The leg drives a MUTATION on the live tenant (it tightens the ResourceQuota to
# zero headroom), so both directions are asserted here: that the constrained path
# is really entered, and that the ceiling is put back on every exit — including
# the failure exits, where a gate that stopped at the error would leave the next
# run's e2e tenant throttled.
#
# Withholding is DERIVED from the modelled ceiling rather than scripted, so a
# test cannot assert a bind that the patch never caused: the stub recomputes
# headroom from whatever the leg last patched, exactly as the rung reads it.

# capacity_leg is run IN THIS SHELL, output redirected to a file, rather than
# captured through $(...). Command substitution forks a subshell, which swallows
# every global the leg sets — the stub's ceiling, and E2E_QUOTA_RESTORE, which
# main()'s teardown reads. Assertions on those then observe the value the test
# set and pass for any implementation (measured twice: with the stub state in
# shell variables, deleting the leg's rung check reddened no ceiling assertion;
# and with the leg in $(...), deleting restore_e2e_quota's clear left "the happy
# path leaves nothing to undo" green).
#
# The stub's own state stays in files regardless: cheap, and it keeps the model
# inspectable between cases.
CAP_HARD_FILE="${SCRATCH}/cap-hard"     # the tenant's pods ceiling, as patched
CAP_PATCHES_FILE="${SCRATCH}/cap-patch" # how many patches the leg has issued
CAP_OUT="${SCRATCH}/cap-out"            # the leg's own output, per case

CAP_USED="2"       # pods already counted against the quota (headroom = hard - used)
CAP_ADVERTISED="2" # X-ScaleSetMaxCapacity when nothing withholds
CAP_RUNGS="quota capacity scaleup"
CAP_QUOTA_BINDS=1   # 0 => the rung is evaluated but never binds
CAP_QUOTA_LATCHES=0 # 1 => it keeps withholding after the headroom returns

# cap_reset — put the modelled tenant back to its manifest shape and zero the
# patch counter. Called before each case so one case cannot inherit another's.
cap_reset() {
	printf '6' >"${CAP_HARD_FILE}"
	printf '0' >"${CAP_PATCHES_FILE}"
	CAPACITY_DRIVEN=""
}

cap_hard() { cat "${CAP_HARD_FILE}"; }
cap_patches() { cat "${CAP_PATCHES_FILE}"; }

capacity_model_withheld() {
	local rung slots headroom
	headroom=$(($(cap_hard) - CAP_USED))
	((headroom >= 0)) || headroom=0
	for rung in ${CAP_RUNGS}; do
		slots=0
		if [[ "${rung}" == "quota" ]]; then
			# A latch only engages once the rung has actually bound, which is what
			# separates it from a tenant that was already at its ceiling. Modelling
			# it as "always withholding" would instead reproduce a dirty baseline,
			# and the leg declines to drive from one — so the latch case would pass
			# for the wrong reason and assert nothing about releasing.
			if ((CAP_QUOTA_LATCHES)) && (($(cap_patches) > 0)); then
				slots="${CAP_ADVERTISED}"
			elif ((CAP_QUOTA_BINDS)) && ((headroom == 0)); then
				slots="${CAP_ADVERTISED}"
			fi
		fi
		printf '%s=%s\n' "${rung}" "${slots}"
	done
}

kubectl() {
	case "$*" in
	*patch*resourcequota*)
		printf '%s' "$*" | awk -F'"pods":"' '{print $2}' | awk -F'"' '{print $1}' \
			>"${CAP_HARD_FILE}"
		printf '%s' "$(($(cap_patches) + 1))" >"${CAP_PATCHES_FILE}"
		;;
	*resourcequota*status.used.pods*) printf '%s' "${CAP_USED}" ;;
	*resourcequota*status.hard.pods*) cap_hard ;;
	*advertisedCapacity*) printf '%s' "${CAP_ADVERTISED}" ;;
	*withheldCapacity*) capacity_model_withheld ;;
	esac
}

# Short enough that the two timeout cases below cost three stubbed iterations
# rather than five real minutes.
CAPACITY_POLL_TIMEOUT=3
E2E_QUOTA_RESTORE=""
cap_reset

# The happy path: every rung evaluated, the rung binds at zero headroom, and it
# releases once the ceiling goes back.
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "ok   an evaluated, binding, releasing quota rung passes the leg"
else
	echo "FAIL an evaluated, binding, releasing quota rung must pass the leg" >&2
	fails=$((fails + 1))
fi
check_contains "the leg reports the advertisement" "advertisedCapacity=2" "${out}"
check_contains "the leg reports the bind" "bound at zero headroom" "${out}"
check_contains "the leg reports the release" "released after the quota was restored" "${out}"
check "the happy path puts the ceiling back" "6" "$(cap_hard)"
check "the happy path leaves nothing to undo" "" "${E2E_QUOTA_RESTORE}"
# The rung it cannot drive must be named, not silently skipped: a leg that
# reported only what it proved would read as having covered the whole ladder.
check_contains "the undriven placeability rung is called out" "NOT driven this run" "${out}"
# A driven pass and a declined pass are both exit 0, so the phase event alone
# cannot separate them — the detail is what a release-sentinel.sh reader gets.
check_contains "a driven rung is recorded for the progress stream" "quota rung driven" "${CAPACITY_DRIVEN}"

# A tenant already at its quota ceiling withholds before the leg touches anything.
# Driving from there would assert a bind this leg did not cause, and the release
# could never return to zero, which the leg would report as a latch — a false
# alarm about the product. It must decline to drive, and say so.
cap_reset
printf '2' >"${CAP_HARD_FILE}" # == CAP_USED, so headroom is already zero
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "ok   an already-constrained tenant does not fail the leg"
else
	echo "FAIL an already-constrained tenant must not fail the leg" >&2
	fails=$((fails + 1))
fi
check_contains "the leg declines to drive from a dirty baseline" "already withholding" "${out}"
check_contains "the declined drive names the remedy" "raise the quota" "${out}"
check "a declined drive tightens nothing" "0" "$(cap_patches)"
check "a declined drive leaves nothing to undo" "" "${E2E_QUOTA_RESTORE}"
check_contains "a declined drive says so in the progress stream" "NOT driven" "${CAPACITY_DRIVEN}"

# A rung missing from withheldCapacity is the regression this leg exists for —
# an absent reason means the rung was never evaluated on this tier (Q443), which
# is a different statement from it not binding.
CAP_RUNGS="capacity scaleup"
cap_reset
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL an unevaluated quota rung must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   an unevaluated quota rung fails the gate"
fi
check_contains "the failure names the absent rung" "withheldCapacity: quota" "${out}"
check_contains "the failure distinguishes absent from zero" "explicit zero" "${out}"
# It fails BEFORE tightening anything: the ladder is not evaluated, so driving it
# would prove nothing and would spend a mutation to learn it.
check "an unevaluated rung tightens nothing" "0" "$(cap_patches)"
CAP_RUNGS="quota capacity scaleup"

# No advertisement at all: the listener never polled, or the set is not on the
# scale-set tier — either way the ladder never ran.
CAP_ADVERTISED=""
cap_reset
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL an unpublished advertisedCapacity must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   an unpublished advertisedCapacity fails the gate"
fi
check_contains "the failure names the empty advertisement" "no advertisedCapacity" "${out}"
CAP_ADVERTISED="2"

# Evaluated but not binding at zero headroom: the tenant would keep claiming jobs
# whose pods the quota cannot admit. The gate must fail AND must still restore.
CAP_QUOTA_BINDS=0
cap_reset
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL a non-binding quota rung must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   a non-binding quota rung fails the gate"
fi
check_contains "the failure names the claim-and-stall it allows" "claim-and-stall" "${out}"
check "a non-binding rung still restores the ceiling" "6" "$(cap_hard)"
CAP_QUOTA_BINDS=1

# Latched: the rung keeps withholding after the ceiling is restored. The
# advertisement is a per-poll read rather than a latch, so this would throttle a
# tenant indefinitely on a quota that has already been raised.
CAP_QUOTA_LATCHES=1
cap_reset
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL a latched quota rung must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   a latched quota rung fails the gate"
fi
check_contains "the failure says the rung is not a latch" "per-poll read, not" "${out}"
check "a latched rung still restores the ceiling" "6" "$(cap_hard)"
CAP_QUOTA_LATCHES=0

# restore_e2e_quota is what teardown reaches for after a gate killed mid-leg, so
# it has to put the ceiling back from nothing but the recorded value.
printf '2' >"${CAP_HARD_FILE}"
E2E_QUOTA_RESTORE="6"
restore_e2e_quota
check "teardown restores a ceiling a killed gate left tight" "6" "$(cap_hard)"
check "a completed restore leaves nothing to undo" "" "${E2E_QUOTA_RESTORE}"

# And it is a no-op when the gate never tightened anything — teardown runs it on
# every exit, including the ones that never reached the leg.
cap_reset
E2E_QUOTA_RESTORE=""
restore_e2e_quota
check "an untightened ceiling is left alone" "6" "$(cap_hard)"
check "a no-op restore issues no patch" "0" "$(cap_patches)"

if ((fails > 0)); then
	echo "validate-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "validate-release-test: ok"
