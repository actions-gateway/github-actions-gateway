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
# The lease's API primitives, faked: teardown reads the lease, and a real read
# would fetch cluster credentials.
# shellcheck source=scripts/dogfood/lib/lease-fake.sh
source "${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh"

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

check_not_contains() {
	local name="$1" needle="$2" haystack="$3"
	if [[ "${haystack}" != *"${needle}"* ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: '${needle}' unexpectedly present" >&2
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

# A list that answers with an OLDER dispatch after the new one is created
# (v1.9.0-rc.2's gate) must be skipped, not watched: that run's old verdict
# would stand in for the gate's own.
reset_gh 100 EMPTY 50 200
E2E_RESOLVED_RUN_ID=""
dispatch_e2e_run >/dev/null
check "an older run id after the dispatch is skipped for the new one" "200" "${E2E_RESOLVED_RUN_ID}"

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
LEASE_FILE="$(lease_fake_path "${PROJECT}" "${ZONE}" "${CLUSTER}")"
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
check_contains "the reclaim says what it found" "did not finish tearing down" "${RECLAIM_OUT}"
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

# remote_lease RENEWED_AGO — another host's record, renewed that long ago.
remote_lease() {
	local t=$(($(date +%s) - $1)) iso
	iso="$(date -u -r "${t}" +%Y-%m-%dT%H:%M:%S.000000Z 2>/dev/null ||
		date -u -d "@${t}" +%Y-%m-%dT%H:%M:%S.000000Z)"
	lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" ci-runner-7/4242 "${iso}" 600 "${iso}" v9.9.9-rc.1
}

# A gate on another host renewing its lease owns the cluster as surely as one
# here does: the case Q1158 exists for, a CI run and a local one at once.
arm_lease free
remote_lease 30
run_reclaim
check "another host's live lease is not reclaimed" 1 "${RECLAIM_RC}"
check "another host's live lease sees no teardown" "" "$(cat "${STOP_LOG}")"
check_contains "the refusal names the other host's gate" "ci-runner-7/4242" "${RECLAIM_OUT}"

# A runner lost before its own reclaim step ran stops renewing; once the lease
# lapses, any host reclaims it, and holds it while the stop scripts drain.
arm_lease free
remote_lease 3600
run_reclaim
check "a vanished runner's lapsed lease is reclaimed" 0 "${RECLAIM_RC}"
check "the reclaim holds the lease while the stop scripts run" \
	"e2e-stop
stop lease=held" "$(cat "${STOP_LOG}")"
check "a reclaimed runner's lease is released" "free" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A record that names no host/pid holder cannot be judged at all, so it is
# reported, never acted on.
arm_lease free
lease_fake_write "${PROJECT}" "${ZONE}" "${CLUSTER}" kube-controller-manager "" 600 "" ""
run_reclaim
check "an unattributable lease is not reclaimed" 1 "${RECLAIM_RC}"
check "an unattributable lease sees no teardown" "" "$(cat "${STOP_LOG}")"
check_contains "the refusal explains why it cannot judge" "cannot judge" "${RECLAIM_OUT}"

# An unreadable lease is not a free one: with bad credentials it may hide a live
# gate, and a reclaim that read it as free would carry on to scale up over it.
arm_lease orphaned
LEASE_FAKE_READ_FAILS=1
run_reclaim
unset LEASE_FAKE_READ_FAILS
check "an unreadable lease fails the gate" 1 "${RECLAIM_RC}"
check "an unreadable lease sees no teardown" "" "$(cat "${STOP_LOG}")"
check_contains "the refusal says an unreadable lease is not free" "not a free one" "${RECLAIM_OUT}"

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

# A stop script that refuses and returns leaves nodes up exactly as a kill
# mid-drain does, so the lease must outlive it: --reclaim keys on nothing else,
# and dropping it reported "nothing to reclaim" while three instances billed
# (Q1155). Each stop script is failed on its own, since either refusal strands.
for failing in RECLAIM_E2E_STOP_RC RECLAIM_STOP_RC; do
	script="stop.sh"
	[[ "${failing}" == RECLAIM_E2E_STOP_RC ]] && script="e2e-stop.sh"
	arm_lease held
	out="$(
		set +e
		WORKDIR=""
		export "${failing}=1"
		teardown 2>&1
	)" || true # a pass with a refused stop exits 1; gate_exit below asserts it
	check "a teardown whose ${script} fails keeps its lease" "held" \
		"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"
	check_contains "a teardown whose ${script} fails says the lease is kept" \
		"Teardown INCOMPLETE" "${out}"
	check_contains "a teardown whose ${script} fails names --reclaim" "--reclaim" "${out}"
	if [[ "${out}" == *"Teardown complete"* ]]; then
		echo "FAIL a teardown whose ${script} fails must not report complete" >&2
		fails=$((fails + 1))
	else
		echo "ok   a teardown whose ${script} fails does not report complete"
	fi
done

# Once the gate process exits, that kept lease reads as orphaned, which is the
# state --reclaim acts on.
OWNER_ALIVE=""
check "a kept lease is reclaimable once the gate exits" "orphaned" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# --- Q1157: a pass whose teardown left nodes up does not exit 0 ---------------
#
# The status is the gate's process status after its EXIT trap, which only a child
# bash can observe: calling teardown in a subshell never fires the trap, and a
# trap that returns keeps the script's status (measured on bash 5.3.15).

# gate_exit GATE_RC — the exit status of a child gate that ends with GATE_RC and
# tears down through the real EXIT trap. The failed-pin stub keeps the failure
# diagnostics off the network. REPO is required: main() sets it before arming the
# trap, and without it set -u kills the trap and every case reads 1.
gate_exit() {
	local rc=0
	REPO="${REPO}" PROJECT="${PROJECT}" ZONE="${ZONE}" CLUSTER="${CLUSTER}" bash -c '
		source "$1"
		source "$4"
		SCRIPT_DIR="$2" WORKDIR=""
		gke_get_credentials_and_verify() { return 1; }
		trap teardown EXIT
		exit "$3"
	' gate "${REPO_ROOT}/scripts/dogfood/validate-release.sh" "${STUB_DIR}" "$1" \
		"${REPO_ROOT}/scripts/dogfood/lib/lease-fake.sh" >/dev/null 2>&1 || rc=$?
	echo "${rc}"
}

arm_lease free
check "a pass with a complete teardown exits 0" 0 "$(gate_exit 0)"
for failing in RECLAIM_E2E_STOP_RC RECLAIM_STOP_RC; do
	script="stop.sh"
	[[ "${failing}" == RECLAIM_E2E_STOP_RC ]] && script="e2e-stop.sh"
	export "${failing}=1"
	# Each run starts free: a refused stop keeps the lease the teardown re-took,
	# and the next child would read that as another holder's.
	arm_lease free
	check "a pass whose ${script} refuses exits 1" 1 "$(gate_exit 0)"
	arm_lease free
	check "a failure whose ${script} refuses keeps its own status" 3 "$(gate_exit 3)"
	unset "${failing}"
done

# A gate whose renewals lapsed can find another holder on its lease: a host
# reclaimed it and may be running its own gate. That teardown must leave the
# cluster alone and the successor's lease intact, and must not exit 0 (Q1158).
arm_lease free
remote_lease 30
out="$(
	set +e
	WORKDIR=""
	(exit 0)
	teardown 2>&1
)" && teardown_rc=0 || teardown_rc=$?
check "a teardown whose lease another holder took exits 1" 1 "${teardown_rc}"
check "a teardown whose lease another holder took runs no stop script" "" "$(cat "${STOP_LOG}")"
check_contains "the skipped teardown says why" "Teardown SKIPPED" "${out}"
check "a teardown never releases another holder's lease" "held" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A reclaimer that tore this run's cluster down released the lease, and its own
# gate may be about to acquire one. Teardown re-takes the lease before running
# anything, so it runs on the record (and that gate refuses) or not at all.
arm_lease held
rm -f "${LEASE_FILE}"
(
	set +e
	WORKDIR=""
	teardown
) >/dev/null 2>&1
check "a teardown whose lease was deleted re-takes it before the stop scripts" \
	"e2e-stop
stop lease=held" "$(cat "${STOP_LOG}")"
check "the re-taken lease is released after the teardown" "free" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# The successor got there first: the re-take fails and another holder is now on
# record, so this teardown is the one that must leave the cluster alone.
arm_lease held
rm -f "${LEASE_FILE}"
out="$(
	set +e
	WORKDIR=""
	lease_api_create() {
		remote_lease 1
		return 1
	}
	teardown 2>&1
)" && teardown_rc=0 || teardown_rc=$?
check "a teardown that loses the re-take to a successor exits 1" 1 "${teardown_rc}"
check "a teardown that loses the re-take runs no stop script" "" "$(cat "${STOP_LOG}")"
check "a teardown that loses the re-take leaves the successor's lease" "held" \
	"$(lease_state "${PROJECT}" "${ZONE}" "${CLUSTER}")"

# A re-take refused by the API with no other holder on record is no evidence of
# a successor, so the nodes this run scaled up still come down.
arm_lease held
rm -f "${LEASE_FILE}"
LEASE_FAKE_WRITE_FAILS=1
(
	set +e
	WORKDIR=""
	teardown
) >/dev/null 2>&1
unset LEASE_FAKE_WRITE_FAILS
check "a teardown whose re-take the API refuses still stops the cluster" \
	"e2e-stop
stop lease=released" "$(cat "${STOP_LOG}")"

# An unreadable lease is not evidence of a successor, so teardown still stops
# this run's nodes: stranding them is the leak the lease exists to end.
arm_lease held
LEASE_FAKE_READ_FAILS=1
(
	set +e
	WORKDIR=""
	teardown
) >/dev/null 2>&1
unset LEASE_FAKE_READ_FAILS
check "a teardown that cannot read its lease still stops the cluster" \
	"e2e-stop
stop lease=held" "$(cat "${STOP_LOG}")"

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

# An unreadable lease cannot rule a live owner out, so its stream is left too.
arm_lease held
seed_spent_stream
LEASE_FAKE_READ_FAILS=1
RELEASE_PROGRESS_FILE="${STALE_STREAM}" RELEASE_STATUS_FILE="" progress_reset_unless_held
unset LEASE_FAKE_READ_FAILS
check_contains "an unreadable lease's stream is left alone" "v1.4.0-rc.2" "$(cat "${STALE_STREAM}")"

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

# The placeability rung's own verdict, published as the WorkerCapacityDeclined
# condition. An EMPTY reason models the condition being absent, which is what mode
# Off does — the set did not opt in — and is deliberately distinct from a False
# condition carrying CapacityAvailable, which is the opt-in's evidence.
CAP_GATE_STATUS="False"
CAP_GATE_REASON="CapacityAvailable"
CAP_GATE_MESSAGE="the cluster can place this runner set's worker pods; job intake is not gated"

# cap_reset — put the modelled tenant back to its manifest shape and zero the
# patch counter. Called before each case so one case cannot inherit another's.
cap_reset() {
	printf '6' >"${CAP_HARD_FILE}"
	printf '0' >"${CAP_PATCHES_FILE}"
	CAPACITY_DRIVEN=""
	CAP_GATE_STATUS="False"
	CAP_GATE_REASON="CapacityAvailable"
	CAP_GATE_MESSAGE="the cluster can place this runner set's worker pods; job intake is not gated"
	# Cleared here too, or "a declined drive leaves nothing to undo" can only
	# redden off a leak from the happy path above it. Isolated, it asserts what
	# it names: the declined path returns before anything sets the flag, so
	# moving that assignment above the baseline guard is caught.
	E2E_QUOTA_RESTORE=""
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
	*WorkerCapacityDeclined*.status\}*) printf '%s' "${CAP_GATE_STATUS}" ;;
	*WorkerCapacityDeclined*.reason\}*) printf '%s' "${CAP_GATE_REASON}" ;;
	*WorkerCapacityDeclined*.message\}*) printf '%s' "${CAP_GATE_MESSAGE}" ;;
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
die_if_killed "an evaluated, binding, releasing quota rung" "$rc"
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
# The half it cannot drive must still be named, not silently skipped: a leg that
# reported only what it proved would read as having covered the whole ladder.
# The rung's verdict IS now asserted (see capacity_gate_verdict below); its
# negative verdict is what remains undriven, and the two must not be conflated.
check_contains "the undriven negative verdict is called out" "stays undriven" "${out}"
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
die_if_killed "an already-constrained tenant" "$rc"
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
die_if_killed "an unevaluated quota rung" "$rc"
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
die_if_killed "an unpublished advertisedCapacity" "$rc"
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
die_if_killed "a non-binding quota rung" "$rc"
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
die_if_killed "a latched quota rung" "$rc"
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

# --- capacity_gate_verdict: the placeability rung published a real verdict ---
#
# The rung's withheldCapacity zero, asserted above, says the ladder ran; it does
# not say the set opted in, because that zero is written either way. These cover
# the condition that does say it, in both directions: the two shapes that mean
# the opt-in is not in force must fail the gate, and the two real verdicts must
# not — a decline least of all, since it is the verdict the leg cannot drive.

# Absent: mode Off removes the condition rather than publishing it False, so an
# empty reason is "did not opt in" and not "opted in and found capacity".
cap_reset
CAP_GATE_STATUS=""
CAP_GATE_REASON=""
CAP_GATE_MESSAGE=""
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
die_if_killed "an absent WorkerCapacityDeclined condition" "$rc"
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL an absent WorkerCapacityDeclined condition must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   an absent WorkerCapacityDeclined condition fails the gate"
fi
check_contains "the failure says absent is not False" "ABSENT, not False" "${out}"
check "an unopted-in set still restores the ceiling" "6" "$(cap_hard)"

# Unsupported: the CRD accepted the field but this AGC does not implement the
# mode, so the gate fails open and the rung ships unproven for the release.
cap_reset
CAP_GATE_REASON="GateModeUnsupported"
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
die_if_killed GateModeUnsupported "$rc"
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "FAIL GateModeUnsupported must fail the gate" >&2
	fails=$((fails + 1))
else
	echo "ok   GateModeUnsupported fails the gate"
fi
check_contains "the failure points at the AGC image, not the CRD" \
	"AGC older than the CRD" "${out}"

# The happy verdict: opted in, evaluated, capacity available.
cap_reset
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
die_if_killed "a published CapacityAvailable verdict" "$rc"
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "ok   a published CapacityAvailable verdict passes the leg"
else
	echo "FAIL a published CapacityAvailable verdict must pass the leg" >&2
	fails=$((fails + 1))
fi
check_contains "the leg reports the verdict" "reason=CapacityAvailable" "${out}"
check_contains "the leg still says the negative verdict is undriven" \
	"stays undriven" "${out}"

# Declined: the rung bound on real infra. Not a failure — it is a fact about the
# cluster rather than the release, and it is the verdict this leg cannot
# manufacture — but it must be reported loudly rather than passing silently.
cap_reset
CAP_GATE_STATUS="True"
CAP_GATE_REASON="PodsUnschedulable"
CAP_GATE_MESSAGE="job intake is gated: no node can place the worker"
rc=0
capacity_leg >"${CAP_OUT}" 2>&1 || rc=$?
die_if_killed "a declined placeability rung" "$rc"
out="$(cat "${CAP_OUT}")"
if ((rc == 0)); then
	echo "ok   a declined placeability rung passes the leg"
else
	echo "FAIL a declined placeability rung must not fail the gate" >&2
	fails=$((fails + 1))
fi
check_contains "a decline is reported" "DECLINED — reason=PodsUnschedulable" "${out}"
check_contains "the decline is qualified as possibly stale (Q1035)" "stale" "${out}"
check_contains "the decline carries the rung's own message" \
	"no node can place the worker" "${out}"
check_contains "the decline is recorded as driven" \
	"placeability rung DECLINED" "${CAPACITY_DRIVEN}"
cap_reset

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


# --- the soak readings and the mirror census (Q1048, Q1059, Q1060) ----------
#
# Both legs are READINGS: they must report what they found and must never fail
# the gate, because a negative reading is evidence the release exists to gather.
# So every case below asserts the report text AND that the leg returned 0.

PROJECT=p ZONE=z CLUSTER=c
SOAK_TMP="$(mktemp -d)"
SCRIPT_DIR="${SOAK_TMP}/soak-bin"
mkdir -p "${SCRIPT_DIR}"
gke_get_credentials_and_verify() { :; }

# The census script stands in for the real one, returning the exit class a case
# is about. Its contract is the thing under test here, not its internals:
# 0 all clients labelled, 1 a finding, 2 a reading that could not be taken.
export FAKE_CENSUS_RC=0
cat >"${SCRIPT_DIR}/e2e-mirror-clients.sh" <<'CENSUS'
exit "${FAKE_CENSUS_RC:-0}"
CENSUS

# Every branch below must leave a record behind. The reports these legs print
# are read once, by whoever is watching the window; the record is what the plan
# is written from weeks later, so a branch that reports and does not record is a
# reading lost to scrollback -- the exact failure this wiring exists to close.
# progress_reading reads the path at call time, so pointing it here is enough.
RELEASE_READINGS_FILE="${SOAK_TMP}/readings.jsonl"
reading() { tail -1 "${RELEASE_READINGS_FILE}" | jq -r ".$1"; }
# soak_leg takes two readings per run, so they are read by id rather than by
# position: an assertion keyed on "the last line" would silently start grading
# Q1060 if the Q1059 branch ever stopped recording.
reading_for() { jq -r --arg id "$1" --arg f "$2" 'select(.id==$id)|.[$f]' "${RELEASE_READINGS_FILE}" | tail -1; }

export FAKE_CENSUS_RC=0
: >"${RELEASE_READINGS_FILE}"
out="$(census_mirror_clients 2>&1)"; rc=$?
check "census: a clean reading returns 0" "0" "${rc}"
check_contains "census: a clean reading says so" "workload-labelled pod" "${out}"
check "census: a clean reading is recorded against Q1048" "Q1048" "$(reading id)"
check "census: a clean reading is recorded as a pass" "pass" "$(reading verdict)"

export FAKE_CENSUS_RC=1
: >"${RELEASE_READINGS_FILE}"
out="$(census_mirror_clients 2>&1)"; rc=$?
check "census: a FINDING does not fail the gate" "0" "${rc}"
check "census: a finding is recorded as a finding" "finding" "$(reading verdict)"
check_contains "census: a finding is called a finding" "FINDING" "${out}"
check_contains "census: a finding says why it is not this cluster's problem" "isolated topology" "${out}"

export FAKE_CENSUS_RC=2
: >"${RELEASE_READINGS_FILE}"
out="$(census_mirror_clients 2>&1)"; rc=$?
check "census: an untaken reading does not fail the gate" "0" "${rc}"
check_contains "census: an untaken reading is NOT graded as a pass" "NOT TAKEN" "${out}"
check "census: an untaken reading is recorded as not-taken" "not-taken" "$(reading verdict)"

# An unclassified exit is the one an author forgets. It is still a window that
# produced no reading, so it must record that rather than recording nothing.
export FAKE_CENSUS_RC=9
: >"${RELEASE_READINGS_FILE}"
out="$(census_mirror_clients 2>&1)"; rc=$?
check "census: an unclassified exit does not fail the gate" "0" "${rc}"
check "census: an unclassified exit is recorded as not-taken" "not-taken" "$(reading verdict)"
check_contains "census: an unclassified exit names the status" "exit 9" "$(reading detail)"

# A reading's detail is pasted into the v2 GA plan verbatim, so a cause guessed
# in advance becomes a plan claim nobody re-examines. The TTL hypothesis was
# wrong for the one window that hit this arm: the unresolved address was
# link-local and had never been a pod. It belongs in the echo, not the record.
export FAKE_CENSUS_RC=2
: >"${RELEASE_READINGS_FILE}"
out="$(census_mirror_clients 2>&1)"; rc=$?
check_not_contains "census: the untaken reading does not guess a cause" "reaped" "$(reading detail)"
check_not_contains "census: nor names the TTL as the reason" "TTL" "$(reading detail)"
check_contains "census: the operator still sees the hypothesis in the run output" "TTL" "${out}"

# --- soak_leg --------------------------------------------------------------
#
# kubectl is scripted per verb. The apply is fed from stdin, so it is drained:
# leaving it unread makes the heredoc land in the next command's input.
KLOG="${SOAK_TMP}/soak-kubectl.log"
FAKE_WAIT_RC=0
FAKE_BETA_SPEC=''
FAKE_ALPHA_SPEC=''
FAKE_KINDS_PRESENT=1
# Q1156 reads the standing objects at v2 too. Unset, the v2 read mirrors the
# v2beta1 one, so a case about Q1059 or Q1060 sees a clean Q1156 beside it.
unset FAKE_V2_SPEC
FAKE_WRITE_BETA='{"maxReplicas":1,"minReplicas":1}'
FAKE_WRITE_V2='{"maxReplicas":1,"minReplicas":1}'
FAKE_APPLY_V2_RC=0
kubectl() {
	echo "$*" >>"${KLOG}"
	case "$1" in
	apply)
		local manifest
		manifest="$(cat)"
		printf '%s\n' "${manifest}" >>"${SOAK_TMP}/soak-applied.yaml"
		[[ "${manifest}" == *$'apiVersion: actions-gateway.com/v2\n'* ]] && return "${FAKE_APPLY_V2_RC}"
		return 0
		;;
	wait) return "${FAKE_WAIT_RC}" ;;
	delete) return 0 ;;
	get)
		case "$*" in
		# Q1156's listing: one namespaced gateway and one cluster-scoped template.
		*actionsgateways.v2beta1.actions-gateway.com\ --all-namespaces\ -o\ jsonpath*) echo "gag-dogfood dogfood" ;;
		*clusterrunnertemplates.v2beta1.actions-gateway.com\ --all-namespaces\ -o\ jsonpath*) echo " kata-dind" ;;
		*v2beta1.actions-gateway.com\ --all-namespaces\ -o\ jsonpath*) ;;
		*clusterrunnertemplates.v2*.actions-gateway.com\ kata-dind*) printf '%s' '{"t":1}' ;;
		*egressproxies.v2beta1.actions-gateway.com\ soak-reading-v2*) printf '%s' "${FAKE_WRITE_BETA}" ;;
		*egressproxies.v2.actions-gateway.com\ soak-reading-v2*) printf '%s' "${FAKE_WRITE_V2}" ;;
		*actionsgateways.v2.actions-gateway.com\ dogfood*) printf '%s' "${FAKE_V2_SPEC-${FAKE_BETA_SPEC}}" ;;
		*actionsgateways.v2beta1.*\ dogfood*) printf '%s' "${FAKE_BETA_SPEC}" ;;
		*actionsgateways.v2alpha1.*\ dogfood*) printf '%s' "${FAKE_ALPHA_SPEC}" ;;
		*egressproxy\ soak-reading*) echo "    Ready=False reason=NoPool stalled" ;;
		*v2beta1.actions-gateway.com*)
			((FAKE_KINDS_PRESENT)) && echo "someresource/x"
			;;
		esac
		return 0
		;;
	esac
	return 0
}

: >"${KLOG}"
: >"${RELEASE_READINGS_FILE}"
: >"${SOAK_TMP}/soak-applied.yaml"
FAKE_BETA_SPEC='{"a":1}'; FAKE_ALPHA_SPEC='{"a":1}'
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a clean run returns 0" "0" "${rc}"
check "soak: a clean run records Q1059 as a pass" "pass" "$(reading_for Q1059 verdict)"
check "soak: a clean run records Q1060 as a pass" "pass" "$(reading_for Q1060 verdict)"
check "soak: a clean run records Q1156 as a pass" "pass" "$(reading_for Q1156 verdict)"
check "soak: one run takes three readings and no more" "3" "$(wc -l <"${RELEASE_READINGS_FILE}" | tr -d ' ')"
check_contains "soak: Q1156 counts the two standing objects it compared" "2 standing objects" \
	"$(reading_for Q1156 detail)"
check "soak: Q1156 applies an EgressProxy at v2, not only at v2beta1" "1" \
	"$(grep -c '^apiVersion: actions-gateway.com/v2$' "${SOAK_TMP}/soak-applied.yaml")"
check_contains "soak: Q1156 reads the standing gateway at v2" \
	"get actionsgateways.v2.actions-gateway.com dogfood -n gag-dogfood" "$(cat "${KLOG}")"
check_contains "soak: a cluster-scoped kind is read without a namespace" \
	"get clusterrunnertemplates.v2.actions-gateway.com kata-dind -o" "$(cat "${KLOG}")"
check_contains "soak: the v2-applied EgressProxy is deleted again" \
	"delete egressproxy soak-reading-v2" "$(cat "${KLOG}")"
check_contains "soak: the manufactured EgressProxy is v2beta1" \
	"apiVersion: actions-gateway.com/v2beta1" "$(cat "${SOAK_TMP}/soak-applied.yaml")"
check "soak: it is created in the standing tenant, not the e2e one" "gag-dogfood" \
	"$(awk '/^  namespace:/{print $2; exit}' "${SOAK_TMP}/soak-applied.yaml")"
check_contains "soak: the manufactured object is deleted again" \
	"delete egressproxy soak-reading" "$(cat "${KLOG}")"
check_contains "soak: an identical spec is reported lossless" "round-trip lossless" "${out}"

# A proxy that never reconciles IS criterion 2's negative reading, so it prints
# the conditions and still returns 0 rather than rejecting the candidate.
FAKE_WAIT_RC=1
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: an unready EgressProxy does not fail the gate" "0" "${rc}"
check_contains "soak: an unready EgressProxy is recorded as the reading" "did NOT reach Ready" "${out}"
# The five kinds are all present in this case, so a verdict derived from their
# count alone would read pass. It must follow the proxy instead.
check "soak: an unready EgressProxy makes Q1059 a finding, not a pass" "finding" \
	"$(reading_for Q1059 verdict)"
check_contains "soak: the Q1059 detail says the proxy is why" "did not reach Ready" \
	"$(reading_for Q1059 detail)"
FAKE_WAIT_RC=0

# An empty read is the webhook or the caBundle, never two equal objects: the
# leg must not let '' == '' read as a lossless round-trip.
FAKE_BETA_SPEC=''; FAKE_ALPHA_SPEC=''
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: an unreadable object does not fail the gate" "0" "${rc}"
check_contains "soak: an empty read is NOT TAKEN, not lossless" "Q1060: NOT TAKEN" "${out}"
check "soak: an empty read records Q1060 as not-taken" "not-taken" "$(reading_for Q1060 verdict)"
check_not_contains "soak: the untaken Q1060 reading does not guess a cause" "suspect" \
	"$(reading_for Q1060 detail)"
check_contains "soak: the operator still sees the caBundle hypothesis in the run output" \
	"caBundle" "${out}"

FAKE_BETA_SPEC='{"a":1}'; FAKE_ALPHA_SPEC='{"a":2}'
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a lossy round-trip does not fail the gate" "0" "${rc}"
check_contains "soak: a differing spec is reported as the reading" "spec DIFFERS" "${out}"
check "soak: a lossy round-trip records Q1060 as a finding" "finding" "$(reading_for Q1060 verdict)"

FAKE_BETA_SPEC='{"a":1}'; FAKE_ALPHA_SPEC='{"a":1}'
FAKE_KINDS_PRESENT=0
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a missing kind does not fail the gate" "0" "${rc}"
check_contains "soak: a missing kind is named as unmet" "criterion 2 is not met" "${out}"
check "soak: a missing kind records Q1059 as a finding" "finding" "$(reading_for Q1059 verdict)"
FAKE_KINDS_PRESENT=1

# Q1060 used to return early on an untaken reading, which would skip Q1156
# silently. An unreadable v2alpha1 view must still leave Q1156 recorded.
FAKE_BETA_SPEC='{"a":1}'; FAKE_ALPHA_SPEC=''
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: an untaken Q1060 still lets Q1156 run" "pass" "$(reading_for Q1156 verdict)"
FAKE_ALPHA_SPEC='{"a":1}'

# A standing object that differs at v2 is the reading v2.0.0's migration needs.
FAKE_V2_SPEC='{"a":2}'
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a v2 difference does not fail the gate" "0" "${rc}"
check "soak: a v2 difference records Q1156 as a finding" "finding" "$(reading_for Q1156 verdict)"
check_contains "soak: a v2 difference names the object" "actionsgateways gag-dogfood/dogfood: spec DIFFERS" "${out}"
check "soak: Q1060 is unaffected by a v2 difference" "pass" "$(reading_for Q1060 verdict)"

# The shapes measured on gag-dogfood 2026-10-03: a storage-version read keeps
# `value: ""` and has no empty metadata, while the webhook's Go types drop the
# one and add the other. Both decode to the same object, so neither reading may
# call that a difference -- and a non-empty change still must be (above).
FAKE_BETA_SPEC='{"podTemplate":{"spec":{"initContainers":[{"env":[{"name":"T","value":""}],"name":"dind"}]}}}'
FAKE_ALPHA_SPEC='{"podTemplate":{"metadata":{},"spec":{"initContainers":[{"env":[{"name":"T"}],"name":"dind"}]}}}'
FAKE_V2_SPEC="${FAKE_ALPHA_SPEC}"
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: zero-value serialization is not a Q1156 difference" "pass" "$(reading_for Q1156 verdict)"
check "soak: zero-value serialization is not a Q1060 difference" "pass" "$(reading_for Q1060 verdict)"
FAKE_BETA_SPEC='{"a":1}'; FAKE_ALPHA_SPEC='{"a":1}'
unset FAKE_V2_SPEC

# An empty v2 read is the webhook, never an equal object.
FAKE_V2_SPEC=''
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: an unreadable v2 view records Q1156 as not-taken" "not-taken" "$(reading_for Q1156 verdict)"
check_contains "soak: an unreadable v2 view says NOT TAKEN" "Q1156: NOT TAKEN" "${out}"
unset FAKE_V2_SPEC

# The write direction on its own: standing objects agree, the v2-applied one does not.
FAKE_WRITE_V2='{"maxReplicas":1}'
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a lossy v2 write records Q1156 as a finding" "finding" "$(reading_for Q1156 verdict)"
check_contains "soak: the finding names the v2 write" "v2-applied EgressProxy finding" "$(reading_for Q1156 detail)"
FAKE_WRITE_V2='{"maxReplicas":1,"minReplicas":1}'

# A refused v2 apply leaves the write direction untaken, never passed.
FAKE_APPLY_V2_RC=1
: >"${SOAK_TMP}/soak-applied.yaml"
: >"${RELEASE_READINGS_FILE}"
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a refused v2 apply does not fail the gate" "0" "${rc}"
check "soak: a refused v2 apply records Q1156 as not-taken" "not-taken" "$(reading_for Q1156 verdict)"
FAKE_APPLY_V2_RC=0

# The apply is the one path that returns early, so it is the one most likely to
# leave the window silent about criterion 2 entirely.
: >"${RELEASE_READINGS_FILE}"
kubectl() { case "$1" in apply) cat >/dev/null; return 1 ;; esac; return 0; }
out="$(soak_leg 2>&1)"; rc=$?
check "soak: a failed apply does not fail the gate" "0" "${rc}"
check "soak: a failed apply still records Q1059" "Q1059" "$(reading_for Q1059 id)"
check "soak: a failed apply records Q1059 as not-taken" "not-taken" "$(reading_for Q1059 verdict)"

# --- the DinD leg (Q1159) -----------------------------------------------------
#
# e2e_leg runs whatever E2E_VARIANT names, Kata by default. dind_leg must take
# the Kata tenant down before e2e-start.sh re-applies the dind overlay over the
# same namespace and RunnerSet, must hand e2e-start.sh the dind variant, and must
# skip the mirror census, which only the Kata tenant's mirror-only egress makes
# meaningful. Each case runs in a subshell so its stubs do not leak.

LEG_LOG="${SCRATCH}/legs.log"
leg_stubs() {
	bash() { printf 'bash %s variant=%s\n' "${1##*/}" "${E2E_VARIANT:-}" >>"${LEG_LOG}"; }
	dispatch_e2e_run() { E2E_RESOLVED_RUN_ID=42; }
	capture_worker_sizing() { :; }
	report_e2e_run() { :; }
	census_mirror_clients() { echo "census" >>"${LEG_LOG}"; }
}
# line_of NEEDLE — 1-based line of NEEDLE's first appearance in LEG_LOG, 0 if none.
line_of() {
	local n
	n="$(grep -nF -- "$1" "${LEG_LOG}" | head -1 | cut -d: -f1 || true)"
	echo "${n:-0}"
}

: >"${LEG_LOG}"
(leg_stubs; unset E2E_VARIANT; e2e_leg >/dev/null 2>&1)
check_contains "e2e leg: the Kata leg takes the mirror census" "census" "$(cat "${LEG_LOG}")"

: >"${LEG_LOG}"
(leg_stubs; unset E2E_VARIANT; dind_leg >/dev/null 2>&1)
log="$(cat "${LEG_LOG}")"
check_contains "dind leg: starts the e2e tenant as the dind variant" "bash e2e-start.sh variant=dind" "${log}"
check_not_contains "dind leg: skips the mirror census" "census" "${log}"
stop_at="$(line_of "bash e2e-stop.sh")"
start_at="$(line_of "bash e2e-start.sh")"
if ((stop_at > 0 && start_at > 0 && stop_at < start_at)); then
	check "dind leg: takes the Kata tenant down before starting the DinD one" "ok" "ok"
else
	check "dind leg: takes the Kata tenant down before starting the DinD one" "stop before start" "stop at ${stop_at}, start at ${start_at}"
fi

# --- the Dragonfly leg (Q1160) -----------------------------------------------
#
# dragonfly_leg follows dind_leg, so it must take the DinD tenant down, put the
# variant back to Kata (dind_leg exported dind), and hand e2e-start.sh the
# Dragonfly back end. The census and the Dragonfly readings run on it, and
# only on it; a red matrix still fails the gate after the readings are taken.

df_leg_stubs() {
	leg_stubs
	bash() {
		printf 'bash %s variant=%s backend=%s\n' "${1##*/}" "${E2E_VARIANT:-}" \
			"${E2E_MIRROR_BACKEND:-}" >>"${LEG_LOG}"
		[[ "${1##*/}" != e2e-run-watch.sh ]] || return "${FAKE_WATCH_RC:-0}"
	}
	dragonfly_readings() { echo "dragonfly-readings" >>"${LEG_LOG}"; }
}

: >"${LEG_LOG}"
(df_leg_stubs; E2E_VARIANT=dind; export E2E_VARIANT; unset E2E_MIRROR_BACKEND; dragonfly_leg >/dev/null 2>&1)
log="$(cat "${LEG_LOG}")"
check_contains "dragonfly leg: starts the e2e tenant as Kata on the Dragonfly back end" \
	"bash e2e-start.sh variant=kata backend=dragonfly" "${log}"
check_contains "dragonfly leg: takes the mirror census" "census" "${log}"
check_contains "dragonfly leg: takes the Dragonfly readings" "dragonfly-readings" "${log}"
stop_at="$(line_of "bash e2e-stop.sh")"
start_at="$(line_of "bash e2e-start.sh")"
if ((stop_at > 0 && start_at > 0 && stop_at < start_at)); then
	check "dragonfly leg: takes the DinD tenant down before starting its own" "ok" "ok"
else
	check "dragonfly leg: takes the DinD tenant down before starting its own" "stop before start" "stop at ${stop_at}, start at ${start_at}"
fi

: >"${LEG_LOG}"
(df_leg_stubs; unset E2E_VARIANT E2E_MIRROR_BACKEND; e2e_leg >/dev/null 2>&1)
check_not_contains "e2e leg: the Distribution leg takes no Dragonfly readings" "dragonfly-readings" "$(cat "${LEG_LOG}")"
: >"${LEG_LOG}"
(df_leg_stubs; unset E2E_VARIANT E2E_MIRROR_BACKEND; dind_leg >/dev/null 2>&1)
check_not_contains "dind leg: takes no Dragonfly readings" "dragonfly-readings" "$(cat "${LEG_LOG}")"

: >"${LEG_LOG}"
rc=0
(df_leg_stubs; FAKE_WATCH_RC=1; unset E2E_VARIANT E2E_MIRROR_BACKEND; dragonfly_leg >/dev/null 2>&1) || rc=$?
check "dragonfly leg: a red matrix fails the leg" "1" "${rc}"
check_contains "dragonfly leg: a red matrix still takes the readings" "dragonfly-readings" "$(cat "${LEG_LOG}")"

# The census records one reading per mirror back end, so the Dragonfly leg's
# census cannot overwrite the plain Kata leg's in soak-readings.sh.
export FAKE_CENSUS_RC=0
: >"${RELEASE_READINGS_FILE}"
(unset E2E_MIRROR_BACKEND; census_mirror_clients >/dev/null 2>&1)
check "census: the Distribution leg keeps its criterion" "mirror-client-census" "$(reading criterion)"
(E2E_MIRROR_BACKEND=dragonfly; census_mirror_clients >/dev/null 2>&1)
check "census: the Dragonfly leg records under its own criterion" "mirror-client-census-dragonfly" "$(reading criterion)"

# An exported back end in the operator's shell must not reach the first two legs.
pinned="$(E2E_MIRROR_BACKEND=dragonfly command bash -c \
	'source "$1" >/dev/null 2>&1; echo "${E2E_MIRROR_BACKEND}"' _ "${REPO_ROOT}/scripts/dogfood/validate-release.sh")"
check "gate: an inherited mirror back end is pinned back to Distribution" "distribution" "${pinned}"

# --- dragonfly_probe: the probe pod the two network readings run in ----------
#
# The CONNECT probe runs in gag-registry-mirror, which enforces PSA restricted,
# so the manifest must carry the restricted fields or the pod is never admitted
# and both readings are lost to not-taken. The delete must run even when the
# create fails, so a half-made pod never outlives the gate.

PROBE_LOG="${SCRATCH}/probe-kubectl.log"
PROBE_MANIFEST="${SCRATCH}/probe-manifest.json"
probe_kubectl() {
	echo "kubectl $*" >>"${PROBE_LOG}"
	case "$1" in
	create) cat >"${PROBE_MANIFEST}"; return "${FAKE_CREATE_RC:-0}" ;;
	logs) echo "control 0 200" ;;
	esac
	return 0
}
: >"${PROBE_LOG}"
out="$(kubectl() { probe_kubectl "$@"; }; dragonfly_probe ns1 pod1 '{"app":"registry-mirror"}' 'echo hi')"
check "probe: prints the pod's log" "control 0 200" "${out}"
check "probe: the pod lands in the namespace asked for" "ns1" "$(jq -r .metadata.namespace "${PROBE_MANIFEST}")"
check "probe: carries the labels asked for" "registry-mirror" "$(jq -r '.metadata.labels.app' "${PROBE_MANIFEST}")"
check "probe: runs as non-root under PSA restricted" "true" "$(jq -r .spec.securityContext.runAsNonRoot "${PROBE_MANIFEST}")"
check "probe: sets a RuntimeDefault seccomp profile" "RuntimeDefault" "$(jq -r .spec.securityContext.seccompProfile.type "${PROBE_MANIFEST}")"
check "probe: drops every capability" "ALL" "$(jq -r '.spec.containers[0].securityContext.capabilities.drop[0]' "${PROBE_MANIFEST}")"
check "probe: forbids privilege escalation" "false" "$(jq -r '.spec.containers[0].securityContext.allowPrivilegeEscalation' "${PROBE_MANIFEST}")"
check "probe: runs the script it was given" "echo hi" "$(jq -r '.spec.containers[0].command[2]' "${PROBE_MANIFEST}")"
: >"${PROBE_LOG}"
(kubectl() { probe_kubectl "$@"; }; FAKE_CREATE_RC=1; dragonfly_probe ns1 pod1 '{}' 'x' >/dev/null)
check_contains "probe: a failed create still deletes the pod, detached" \
	"kubectl delete pod pod1 --namespace ns1 --ignore-not-found --wait=false" "$(cat "${PROBE_LOG}")"

# --- dragonfly_readings: readings, never gates -------------------------------
#
# Each case stubs the one read it is about and asserts the recorded verdict. A
# reading that could not be taken must never record as a pass, and no branch
# may fail the gate.

df_reading() { jq -r --arg c "$1" --arg f "$2" 'select(.id=="Q539" and .criterion==$c)|.[$f]' "${RELEASE_READINGS_FILE}" | tail -1; }

# dfdaemon's routing lines, in the shape the Q539 plan measured on kind.
p2p_log() {
	local i
	for ((i = 0; i < $1; i++)); do echo "INFO proxy HTTPS request via dfdaemon by rule config"; done
	for ((i = 0; i < $2; i++)); do echo "INFO proxy HTTPS request directly to remote server"; done
}
run_p2p() {
	: >"${RELEASE_READINGS_FILE}"
	out="$(kubectl() { [[ "$1" == logs ]] && printf '%s' "${FAKE_SEED_LOG}"; return 0; }; dragonfly_p2p_reading 2>&1)"
}
FAKE_SEED_LOG="$(p2p_log 2 35)"; run_p2p
check "p2p: a request by rule records a pass" "pass" "$(df_reading p2p-path verdict)"
check "p2p: the pass carries both counts" "2 request(s) by P2P rule, 35 direct" "$(df_reading p2p-path detail)"
FAKE_SEED_LOG="$(p2p_log 0 12)"; run_p2p
check "p2p: direct requests and none by rule record a finding" "finding" "$(df_reading p2p-path verdict)"
check_contains "p2p: the finding names the wording it looked for" "by rule config" "${out}"
FAKE_SEED_LOG="INFO something else entirely"; run_p2p
check "p2p: a log with neither routing line is not-taken, never a pass" "not-taken" "$(df_reading p2p-path verdict)"
FAKE_SEED_LOG=""; run_p2p
check "p2p: an unreadable log is not-taken" "not-taken" "$(df_reading p2p-path verdict)"

# The two network readings, fed the probe's output directly.
run_net() {
	local fn="$1"
	: >"${RELEASE_READINGS_FILE}"
	# shellcheck disable=SC2329 # the stub is invoked by "${fn}", which shellcheck 0.11 cannot follow
	out="$(dragonfly_probe() { printf '%s\n' "${FAKE_PROBE_OUT}"; }; "${fn}" 2>&1)"; rc=$?
}

FAKE_PROBE_OUT=$'control 200\nseed 28 000'; run_net dragonfly_worker_reading
check "worker: the reading returns 0" "0" "${rc}"
check "worker: a timeout behind a working control records a pass" "pass" "$(df_reading worker-to-seed-proxy verdict)"
FAKE_PROBE_OUT=$'control 200\nseed 0 404'; run_net dragonfly_worker_reading
check "worker: an HTTP answer from the seed proxy records a finding" "finding" "$(df_reading worker-to-seed-proxy verdict)"
FAKE_PROBE_OUT=$'control 200\nseed 52 000'; run_net dragonfly_worker_reading
check "worker: an empty reply is still a connection, a finding" "finding" "$(df_reading worker-to-seed-proxy verdict)"
FAKE_PROBE_OUT=$'control 000\nseed 28 000'; run_net dragonfly_worker_reading
check "worker: a timeout with a dead control is not-taken" "not-taken" "$(df_reading worker-to-seed-proxy verdict)"
FAKE_PROBE_OUT=$'control 200\nseed 6 000'; run_net dragonfly_worker_reading
check "worker: a curl error that is neither is not-taken" "not-taken" "$(df_reading worker-to-seed-proxy verdict)"
FAKE_PROBE_OUT=""; run_net dragonfly_worker_reading
check "worker: no probe output is not-taken" "not-taken" "$(df_reading worker-to-seed-proxy verdict)"

FAKE_PROBE_OUT=$'control 0 200\nselfsigned 60 000'; run_net dragonfly_connect_reading
check "connect: the reading returns 0" "0" "${rc}"
check "connect: a TLS refusal behind a working control records a pass" "pass" "$(df_reading connect-upstream-tls verdict)"
FAKE_PROBE_OUT=$'control 0 200\nselfsigned 0 502'; run_net dragonfly_connect_reading
check "connect: a 5xx from the proxy is a refusal" "pass" "$(df_reading connect-upstream-tls verdict)"
FAKE_PROBE_OUT=$'control 0 200\nselfsigned 0 200'; run_net dragonfly_connect_reading
check "connect: a self-signed host answering 200 records a finding" "finding" "$(df_reading connect-upstream-tls verdict)"
check_contains "connect: the finding says what it means" "not verifying upstream TLS" "${out}"
FAKE_PROBE_OUT=$'control 7 000\nselfsigned 7 000'; run_net dragonfly_connect_reading
check "connect: a refusal behind a dead control is not-taken" "not-taken" "$(df_reading connect-upstream-tls verdict)"
FAKE_PROBE_OUT=""; run_net dragonfly_connect_reading
check "connect: no probe output is not-taken" "not-taken" "$(df_reading connect-upstream-tls verdict)"

# The battery's exit classes. Its stand-in reads the namespace it was handed,
# because the gate's own TENANT_NAMESPACE is the standing tenant, and a probe
# run there would not ride a worker's path.
cat >"${SCRIPT_DIR}/e2e-mirror-validate.sh" <<'BATTERY'
echo "tenant=${TENANT_NAMESPACE}"
exit "${FAKE_BATTERY_RC:-0}"
BATTERY
run_battery() {
	: >"${RELEASE_READINGS_FILE}"
	out="$(dragonfly_battery_reading 2>&1)"; rc=$?
}
FAKE_BATTERY_RC=0; export FAKE_BATTERY_RC; run_battery
check "battery: the reading returns 0" "0" "${rc}"
check "battery: a clean run records a pass" "pass" "$(df_reading mirror-battery verdict)"
check_contains "battery: the probe runs in the e2e tenant" "tenant=${E2E_NAMESPACE}" "${out}"
FAKE_BATTERY_RC=1; run_battery
check "battery: a failed check does not fail the gate" "0" "${rc}"
check "battery: a failed check records a finding" "finding" "$(df_reading mirror-battery verdict)"
FAKE_BATTERY_RC=127; run_battery
check "battery: an unclassified exit records not-taken" "not-taken" "$(df_reading mirror-battery verdict)"
FAKE_BATTERY_RC=1

# The wrapper takes all four and never fails the gate, whatever they find.
: >"${RELEASE_READINGS_FILE}"
rc=0
(
	kubectl() { return 1; }
	dragonfly_probe() { :; }
	dragonfly_readings >/dev/null 2>&1
) || rc=$?
check "readings: a cluster that answers nothing does not fail the gate" "0" "${rc}"
check "readings: all four are recorded" "4" "$(jq -s 'map(select(.id=="Q539"))|length' "${RELEASE_READINGS_FILE}")"
check "readings: none of them reads as a pass" "0" "$(jq -s 'map(select(.id=="Q539" and .verdict=="pass"))|length' "${RELEASE_READINGS_FILE}")"

if ((fails > 0)); then
	echo "validate-release-test: ${fails} assertion(s) failed" >&2
	exit 1
fi
echo "validate-release-test: ok"
