#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/workers.sh — the worker-pod occupancy
# probes the dogfood teardown scripts gate on (Q434).
#
# Why this is tested: the drain gate decides whether a billable scale-down is
# allowed to proceed, and both of its failure modes are silent and expensive.
# Reading an unreachable cluster as "idle" scales the pool down under a live
# job, evicting the AGC that is the only thing that reaps worker pods — the
# incident that stranded 82 spot node-hours. Reading a settled cluster as "busy"
# leaves the system pool billing forever. kubectl and sleep are stubbed, so no
# network, no cluster, and no wall-clock wait.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/workers.sh
source "${REPO_ROOT}/scripts/dogfood/lib/workers.sh"

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

# --- Stubs -----------------------------------------------------------------
#
# Stub state lives in files, not variables: the probes read kubectl inside a
# command substitution, so a counter or a consumed queue entry updated in that
# subshell would vanish before the assertion could see it.
#
#   readings  A queue of successive kubectl results, one per line, consumed one
#             per poll — so a drain run can be scripted busy → busy → empty. The
#             last line repeats once the queue is exhausted, so a test scripts
#             only the readings that differ. `EMPTY` is a clean reading and
#             `FAIL` is a cluster that cannot be reached; `|` separates the pod
#             lines *within* one reading, since a line is one whole reading.
#   calls     kubectl invocation count.
#   sleeps    Stubbed-sleep count, so a test can assert the loop paced itself
#             rather than spinning.
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

kubectl() {
	echo x >>"${STUB_DIR}/calls"
	local head
	head="$(head -n 1 "${STUB_DIR}/readings")"
	# Consume, unless this is the last entry — which then repeats.
	if (($(wc -l <"${STUB_DIR}/readings") > 1)); then
		tail -n +2 "${STUB_DIR}/readings" >"${STUB_DIR}/readings.next"
		mv "${STUB_DIR}/readings.next" "${STUB_DIR}/readings"
	fi
	case "${head}" in
		FAIL) return 1 ;;
		EMPTY) return 0 ;;
		*) printf '%s\n' "${head//|/$'\n'}" ;;
	esac
}

sleep() { echo x >>"${STUB_DIR}/sleeps"; }

# script_readings LINE... — arm the kubectl queue with the given readings.
script_readings() {
	printf '%s\n' "$@" >"${STUB_DIR}/readings"
	: >"${STUB_DIR}/calls"
	: >"${STUB_DIR}/sleeps"
}

count_of() { wc -l <"${STUB_DIR}/$1" | tr -d ' '; }

script_readings EMPTY

# --- count_inflight_workers ------------------------------------------------

script_readings 'gag-dogfood runner-a 1/1 Running 0 3m'
check "counts a Running worker" 1 "$(count_inflight_workers)"

# Pending counts too: the job is already acquired, so scaling down on it strands
# exactly as much as scaling down on a Running one.
script_readings 'gag-dogfood runner-a 1/1 Running 0 3m|gag-dogfood runner-b 0/1 Pending 0 1m'
check "counts a Pending worker as in flight" 2 "$(count_inflight_workers)"

script_readings EMPTY
check "empty result counts 0" 0 "$(count_inflight_workers)"

# The load-bearing one: an unreadable cluster must never look idle, because the
# caller turns "0" into a billable scale-down.
script_readings FAIL
check "unreachable cluster reads 'unknown', not 0" "unknown" "$(count_inflight_workers)"

# --- the selector is the one the AGC actually stamps ------------------------

# Pinned literally, because a typo'd selector matches nothing, which reads as a
# drained cluster and re-opens the incident. Must stay in step with
# workerAppName/workerComponent in cmd/agc/internal/provisioner/provisioner.go.
check "selects both app.kubernetes.io worker labels" \
	"app.kubernetes.io/name=actions-runner,app.kubernetes.io/component=runner" \
	"${WORKER_POD_SELECTOR}"

# --- wait_for_worker_drain -------------------------------------------------

# An already-idle cluster still takes SETTLE_READINGS readings before it counts
# as drained, so a reap-then-provision gap cannot be mistaken for a drain.
script_readings EMPTY
wait_for_worker_drain 60 5 >/dev/null && rc=0 || rc=1
check "idle cluster drains" 0 "${rc}"
check "  ...after the full settle window" "${WORKER_DRAIN_SETTLE_READINGS}" "$(count_of calls)"

# Busy, then busy, then empty: the loop must keep polling through the busy
# readings and only return once the settle window is clean.
script_readings 'gag-dogfood runner-a 1/1 Running 0 3m' 'gag-dogfood runner-a 1/1 Running 0 4m' EMPTY
wait_for_worker_drain 60 5 >/dev/null && rc=0 || rc=1
check "waits out in-flight workers, then drains" 0 "${rc}"
check "  ...polling once per reading" 4 "$(count_of calls)"
check "  ...sleeping between readings" 3 "$(count_of sleeps)"

# A cluster that never drains must time out rather than fall through.
script_readings 'gag-dogfood runner-a 1/1 Running 0 3m'
wait_for_worker_drain 20 5 >/dev/null && rc=0 || rc=1
check "a cluster that never drains times out" 1 "${rc}"
check "  ...after timeout/interval + 1 polls" 5 "$(count_of calls)"

# An unreadable cluster must time out too — "cannot tell" is not "drained".
script_readings FAIL
wait_for_worker_drain 20 5 >/dev/null 2>&1 && rc=0 || rc=1
check "an unreadable cluster times out, never drains" 1 "${rc}"

# A read failure landing inside the settle window must reset it, not be counted
# as one of the clean readings.
script_readings EMPTY FAIL EMPTY EMPTY
wait_for_worker_drain 60 5 >/dev/null 2>&1 && rc=0 || rc=1
check "a failed read resets the settle streak" 0 "${rc}"
check "  ...so the drain needs the extra readings" 4 "$(count_of calls)"

# A timeout shorter than the settle window still gets its readings, so a caller
# asking for a fast check gets a real answer rather than a guaranteed failure.
script_readings EMPTY
wait_for_worker_drain 0 15 >/dev/null && rc=0 || rc=1
check "a sub-settle timeout still completes the settle" 0 "${rc}"
check "  ...taking the settle count of readings" "${WORKER_DRAIN_SETTLE_READINGS}" "$(count_of calls)"

# --- count_queued_scale_set_jobs / wait_for_scale_set_idle ------------------
#
# The GitHub-side probes (2026-07-31 incident): a job still queued at GitHub
# has no pod, so the pod probes cannot see it — and deleting the AGC under it
# strands it. Stubbed like kubectl: one reading per gh call, `|` separating
# lines within a reading, FAIL a gh that cannot reach GitHub.
REPO="octo/repo"

gh() {
	echo x >>"${STUB_DIR}/calls"
	local head
	head="$(head -n 1 "${STUB_DIR}/gh-readings")"
	if (($(wc -l <"${STUB_DIR}/gh-readings") > 1)); then
		tail -n +2 "${STUB_DIR}/gh-readings" >"${STUB_DIR}/gh-readings.next"
		mv "${STUB_DIR}/gh-readings.next" "${STUB_DIR}/gh-readings"
	fi
	case "${head}" in
		FAIL) return 1 ;;
		EMPTY) return 0 ;;
		*) printf '%s\n' "${head//|/$'\n'}" ;;
	esac
}

script_gh_readings() {
	printf '%s\n' "$@" >"${STUB_DIR}/gh-readings"
	: >"${STUB_DIR}/calls"
	: >"${STUB_DIR}/sleeps"
}

# No queued runs at all: nothing can target the label.
script_gh_readings EMPTY
check "an empty queue counts 0 scale-set jobs" 0 "$(count_queued_scale_set_jobs gag-ci-e2e)"

# Two queued runs; the per-run job lookups report one matching job, then none.
script_gh_readings '11|12' 1 0
check "queued jobs are summed across runs" 1 "$(count_queued_scale_set_jobs gag-ci-e2e)"

# The load-bearing one, same shape as the kubectl probe: an unreadable GitHub
# must never look like an empty queue — the caller turns "0" into an AGC delete.
script_gh_readings FAIL
check "an unreachable GitHub reads 'unknown', not 0" "unknown" "$(count_queued_scale_set_jobs gag-ci-e2e)"

script_gh_readings '11|12' 1 FAIL
check "a failed per-run lookup reads 'unknown', not a partial count" "unknown" \
	"$(count_queued_scale_set_jobs gag-ci-e2e)"

# A busy queue is waited out; the wait returns once a reading is clean.
script_gh_readings '11' 1 EMPTY
wait_for_scale_set_idle gag-ci-e2e 60 5 >/dev/null && rc=0 || rc=1
check "a busy queue is waited out, then idles" 0 "${rc}"

# A queue that never drains must time out rather than fall through.
script_gh_readings '11' 1
wait_for_scale_set_idle gag-ci-e2e 20 5 >/dev/null && rc=0 || rc=1
check "a queue that never drains times out" 1 "${rc}"

# 'unknown' readings run the clock out; they never count as idle.
script_gh_readings FAIL
wait_for_scale_set_idle gag-ci-e2e 20 5 >/dev/null 2>&1 && rc=0 || rc=1
check "an unreadable queue times out, never idles" 1 "${rc}"

# --- kubectl invocation ----------------------------------------------------
#
# Last, because these replace the recording kubectl stub above with an
# echoing one (bash functions are global, so the swap is not scoped).

ARGS_FILE="${STUB_DIR}/args"
kubectl() { printf '%s\n' "$*" >"${ARGS_FILE}"; }

# The field selector is as load-bearing as the label selector: drop it and a
# finished job's Succeeded pod counts as in-flight, so a "drained" cluster never
# drains. Asserted on the real invocation, not re-derived here.
count_inflight_workers >/dev/null
check "queries in-flight workers by label, excluding terminal phases" \
	"get pods --all-namespaces --selector=${WORKER_POD_SELECTOR} --field-selector=status.phase!=Succeeded,status.phase!=Failed --no-headers" \
	"$(cat "${ARGS_FILE}")"

# e2e-stop.sh scopes its pre-delete drain to the e2e tenant namespace so the CI
# tenant's ordinary traffic never holds that teardown up; the scope must reach
# the actual invocation.
WORKER_POD_NAMESPACE="gag-dogfood-e2e"
count_inflight_workers >/dev/null
check "scopes to a namespace when WORKER_POD_NAMESPACE is set" \
	"get pods --namespace=gag-dogfood-e2e --selector=${WORKER_POD_SELECTOR} --field-selector=status.phase!=Succeeded,status.phase!=Failed --no-headers" \
	"$(cat "${ARGS_FILE}")"
WORKER_POD_NAMESPACE=""

# The drain-timeout diagnostic names the pod: a broken column spec printed
# '<none>' for every pod name in the 2026-07-31 incident's output, hiding
# exactly the thing an operator needed to chase.
describe_inflight_workers >/dev/null
check_args="$(cat "${ARGS_FILE}")"
if [[ "${check_args}" == *"POD:.metadata.name"* && "${check_args}" == *"NS:.metadata.namespace"* ]]; then
	echo "ok   the drain diagnostic asks for the pod name and namespace as columns"
else
	echo "FAIL the drain diagnostic must ask for the pod name and namespace as columns: ${check_args}" >&2
	fails=$((fails + 1))
fi

# delete.sh pins the context per call instead of making it active; the probe
# must actually pass it through, or that read silently targets whatever context
# happens to be selected.
pin_context_and_probe() {
	local WORKER_KUBECTL_CONTEXT="gke_p_z_c"
	worker_kubectl get pods
}
pin_context_and_probe
check "pins the context when one is set" "--context=gke_p_z_c get pods" "$(cat "${ARGS_FILE}")"

worker_kubectl get pods
check "uses the active context when none is set" "get pods" "$(cat "${ARGS_FILE}")"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all worker occupancy tests passed"
