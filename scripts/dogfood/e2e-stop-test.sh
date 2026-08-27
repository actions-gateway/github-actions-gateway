#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-stop.sh: the on-demand e2e tenant teardown
# — the routing reset, the two pre-delete drains, the ActionsGateway delete they
# gate, and the system-pool restore that follows.
#
# Why it is tested: teardown inverts the bring-up's risk. Every failure here is
# silent and billable in one direction or the other.
#
# A drain read as converged deletes the AGC out from under live work: the AGC is
# the only thing that serves jobs already queued on the scale set and the only
# thing that reaps worker pods, so a premature delete wedges a queued job's
# workflow concurrency group forever, and an orphaned worker pod's
# do-not-disrupt annotations pin its billable node until a human notices (one
# incident stranded 82 spot node-hours). So the drains must run before the
# delete, and a drain that times out must abort BEFORE it — leaving the AGC
# alive to finish the work.
#
# The other direction stalls a teardown that is actually healthy: a pod drain
# scoped cluster-wide waits on the always-on CI tenant's workers, which keep
# flowing after the e2e window closes and never drain; a routing reset that ran
# after the drains would let new e2e jobs keep arriving into the queue being
# drained. Either one burns the whole timeout and reports a converged teardown
# as stuck — the shape that aborted the v1.3.0-rc.3 gate from the bring-up side.
#
# The script is sourced with E2E_STOP_LIB_ONLY=1 so main() does not run;
# `kubectl`, `gcloud`, `gh` and require_cmd are stubbed, so no cluster and no
# GitHub. The drains and the pool sizing run for real against those stubs, so
# this covers the e2e-stop.sh/lib/workers.sh and e2e-stop.sh/lib/pool.sh seams
# rather than restating what workers-test.sh and pool-test.sh already assert.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_STOP_LIB_ONLY=1
export E2E_STOP_LIB_ONLY
# A short drain budget: it is read once at source time, and every sleep below is
# stubbed out, so this only decides how many polls a non-draining scenario takes
# (timeout/interval + 1) before it gives up.
E2E_DRAIN_TIMEOUT=30
export E2E_DRAIN_TIMEOUT
# shellcheck source=scripts/dogfood/e2e-stop.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-stop.sh"

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
# both what was called and in what order — ordering carries the whole safety
# argument here (a delete before the drains strands work; a routing reset after
# them lets the queue refill). The log is a file because main() runs in a
# subshell.
CALL_LOG="${WORKDIR}/calls.log"
PODS_FILE="${WORKDIR}/pods"
RUNS_FILE="${WORKDIR}/runs"
GATEWAYS_FILE="${WORKDIR}/gateways"
MIRRORS_FILE="${WORKDIR}/mirrors"
POD_READ_OK=1
QUEUED_PER_RUN=0
GH_RUN_LIST_OK=1
CONTEXT=""

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	config\ current-context) echo "${CONTEXT}" ;;
	get\ pods*)
		((POD_READ_OK)) || return 1
		cat "${PODS_FILE}"
		;;
	get\ actionsgateways*) cat "${GATEWAYS_FILE}" ;;
	get\ deployment*) cat "${MIRRORS_FILE}" ;;
	esac
	return 0
}

gcloud() { printf 'gcloud %s\n' "$*" >>"${CALL_LOG}"; }

gh() {
	printf 'gh %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	run\ list*)
		((GH_RUN_LIST_OK)) || return 1
		cat "${RUNS_FILE}"
		;;
	api*) echo "${QUEUED_PER_RUN}" ;;
	esac
	return 0
}

require_cmd() { :; }
# Loop pacing only — the drain-timeout tests otherwise wait out real seconds.
sleep() { :; }

# reset_stubs NAMESPACE... — arm the gateway listing with the given namespaces
# (one ActionsGateway each, feeding the pool-size derivation) and restore the
# default knobs: a cluster whose e2e queue and e2e worker pods are both already
# empty, on the expected context.
reset_stubs() {
	printf '%s\n' "$@" >"${GATEWAYS_FILE}"
	: >"${CALL_LOG}"
	: >"${PODS_FILE}"
	: >"${RUNS_FILE}"
	: >"${MIRRORS_FILE}"
	POD_READ_OK=1
	QUEUED_PER_RUN=0
	GH_RUN_LIST_OK=1
	CONTEXT="gke_${PROJECT}_${ZONE}_${CLUSTER}"
	unset SKIP_E2E_DRAIN
	SYSTEM_POOL_AT_REST_NODES=""
}

# queue_busy — arm GitHub with one queued run whose jobs never leave the queue.
queue_busy() {
	echo 4242 >"${RUNS_FILE}"
	QUEUED_PER_RUN=1
}

# mirror_deployed — arm the cluster with the five registry-mirror Deployments,
# the shape `kubectl get deployment -o name` returns.
mirror_deployed() {
	printf 'deployment.apps/mirror-%s\n' \
		docker-io ghcr-io quay-io registry-k8s-io gcr-io >"${MIRRORS_FILE}"
}

# workers_busy — arm the cluster with two e2e worker pods that never get reaped.
workers_busy() {
	printf '%s\n' \
		"gag-dogfood-e2e   runner-e2e-aaa" \
		"gag-dogfood-e2e   runner-e2e-bbb" >"${PODS_FILE}"
}

# run_main — run main() in a subshell and record its status in MAIN_RC and its
# combined output in MAIN_OUT.
#
# The subshell must not be an operand of `||` or `if`: bash suppresses errexit
# inside such a subshell even when it re-runs `set -e` itself, which would let
# main() sail past a failed drain and make the abort assertions below pass
# vacuously. Hence set +e around a plain call.
MAIN_RC=0
MAIN_OUT=""
run_main() {
	set +e
	(
		set -e
		main
	) >"${WORKDIR}/main.out" 2>&1
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

# resize_nodes — the --num-nodes value the resize was called with.
resize_nodes() {
	local line
	line="$(call_line 'clusters resize')"
	if [[ "${line}" =~ --num-nodes=([0-9]+) ]]; then
		echo "${BASH_REMATCH[1]}"
	else
		echo none
	fi
}

echo "scripts/dogfood/e2e-stop-test.sh"

# --- the happy path: route off, drain, delete, restore -----------------------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
check "an idle tenant tears down cleanly" 0 "${MAIN_RC}"
check_contains "routes e2e jobs back to GitHub-hosted" \
	'variable set GAG_E2E_RUNNER --body "ubuntu-latest"' "$(call_line 'variable set')"
check_contains "deletes only the e2e ActionsGateway" \
	"delete actionsgateway dogfood-e2e --namespace gag-dogfood-e2e" \
	"$(call_line 'delete actionsgateway')"
# The tenant's namespace, App Secret, quota, RunnerSet and NetworkPolicy are
# inert without the gateway and cheap to keep; deleting them would turn the next
# e2e-start.sh from a re-apply into a re-bootstrap.
check_not_contains "leaves the rest of the tenant in place" \
	"delete namespace" "$(cat "${CALL_LOG}")"
check_not_contains "leaves the App Secret in place" \
	"delete secret" "$(cat "${CALL_LOG}")"
# A gateway already gone is the normal re-run case, not a teardown failure.
check_contains "tolerates an already-deleted gateway" \
	"--ignore-not-found" "$(call_line 'delete actionsgateway')"

# --- ordering: routing off first, then drain, then delete --------------------

# Reset routing BEFORE the drains: a drain that runs while vars.GAG_E2E_RUNNER
# still points at the scale set is draining a queue that keeps refilling, so it
# burns the whole budget and reports a healthy teardown as stuck.
check_before "resets routing before draining the queue" \
	"variable set" "run list"
# Queued jobs first: a job GitHub has not handed out yet has no pod, so the pod
# probe cannot see it, and only the still-alive AGC can acquire it.
check_before "drains the queue before the worker pods" \
	"run list" "get pods"
# The delete is what strands; both drains gate it.
check_before "drains the queue before deleting the AGC" \
	"run list" "delete actionsgateway"
check_before "drains the worker pods before deleting the AGC" \
	"get pods" "delete actionsgateway"
# Nothing may be deleted before the target cluster is pinned and verified.
check_before "verifies the cluster context before deleting" \
	"config current-context" "delete actionsgateway"

# --- the drains are scoped to the e2e tenant ---------------------------------

# The always-on CI tenant keeps serving jobs after the e2e window closes, so a
# cluster-wide pod probe never reads empty and the teardown times out on a
# cluster that is fine.
pods_call="$(call_line 'get pods')"
check_contains "scopes the worker drain to the e2e tenant namespace" \
	"--namespace=gag-dogfood-e2e" "${pods_call}"
check_not_contains "never drains worker pods cluster-wide" \
	"--all-namespaces" "${pods_call}"

# --- a queue that will not drain aborts BEFORE the delete --------------------

reset_stubs gag-dogfood gag-dogfood-ci
queue_busy
run_main
check "queued jobs that never drain fail the teardown" 1 "${MAIN_RC}"
# The false-green direction, and the expensive one: deleting the AGC here leaves
# nothing that can acquire the queued jobs, wedging each run's concurrency group.
check_not_contains "never deletes the AGC with jobs still queued" \
	"delete actionsgateway" "$(cat "${CALL_LOG}")"
check_not_contains "never scales the system pool down with jobs still queued" \
	"clusters resize" "$(cat "${CALL_LOG}")"
check_contains "names the scale set holding the teardown up" \
	"still queued on 'gag-ci-e2e'" "${MAIN_OUT}"
check_contains "offers the knowing-strand escape hatch" \
	"SKIP_E2E_DRAIN=1" "${MAIN_OUT}"

# An unreachable GitHub is not an empty queue: reading the queue as drained
# because the read failed is the same premature delete by another route.
reset_stubs gag-dogfood gag-dogfood-ci
GH_RUN_LIST_OK=0
run_main
check "an unreadable job queue fails the teardown" 1 "${MAIN_RC}"
check_not_contains "never deletes the AGC on an unreadable queue" \
	"delete actionsgateway" "$(cat "${CALL_LOG}")"

# --- worker pods that will not drain abort BEFORE the delete -----------------

reset_stubs gag-dogfood gag-dogfood-ci
workers_busy
run_main
check "in-flight worker pods that never drain fail the teardown" 1 "${MAIN_RC}"
check_not_contains "never deletes the AGC with workers in flight" \
	"delete actionsgateway" "$(cat "${CALL_LOG}")"
check_not_contains "never scales the system pool down with workers in flight" \
	"clusters resize" "$(cat "${CALL_LOG}")"
check_contains "names the pods pinning their nodes" \
	"runner-e2e-aaa" "${MAIN_OUT}"
# Counts alone cannot separate a wedged drain from one working through a
# backlog, so the timeout path reports turnover instead of just giving up.
check_contains "says whether the drain was converging" \
	"NOT converging" "${MAIN_OUT}"

# An unreadable cluster must not read as an idle one.
reset_stubs gag-dogfood gag-dogfood-ci
POD_READ_OK=0
run_main
check "an unreadable cluster fails the teardown" 1 "${MAIN_RC}"
check_not_contains "never deletes the AGC on an unreadable cluster" \
	"delete actionsgateway" "$(cat "${CALL_LOG}")"

# --- SKIP_E2E_DRAIN is the only way past a live tenant -----------------------

reset_stubs gag-dogfood gag-dogfood-ci
queue_busy
workers_busy
SKIP_E2E_DRAIN=1
run_main
check "the escape hatch deletes despite live work" 0 "${MAIN_RC}"
check_contains "says the drains were skipped" "skipping the queued-job" "${MAIN_OUT}"
check_not_contains "skips the queue drain entirely" "run list" "$(cat "${CALL_LOG}")"
check_not_contains "skips the worker drain entirely" "get pods" "$(cat "${CALL_LOG}")"

# --- the restore never strands, and never evicts an always-on AGC ------------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
# Back to the running size dogfood/start.sh leaves the pool in — not to 0, which
# would evict the always-on AGCs and strand the workers they were reaping.
check "restores the derived running size" 2 "$(resize_nodes)"
check_contains "pins the project on the resize" "--project=${PROJECT}" \
	"$(call_line 'clusters resize')"
check_contains "pins the zone on the resize" "--zone=${ZONE}" \
	"$(call_line 'clusters resize')"
check_before "restores the pool only after the AGC is gone" \
	"delete actionsgateway" "clusters resize"

# A third always-on tenant needs a third node; restoring to the two-node floor
# would evict one tenant's AGC and strand its workers.
reset_stubs gag-dogfood gag-dogfood-ci gag-dogfood-three
run_main
check "grows the restore size with the always-on tenants" 3 "$(resize_nodes)"

# A cluster whose gateways cannot be listed still restores the floor, never 0.
reset_stubs
run_main
check "never restores below the validated floor" 2 "$(resize_nodes)"

reset_stubs gag-dogfood gag-dogfood-ci
SYSTEM_POOL_AT_REST_NODES=4
run_main
check "honors an explicitly pinned restore size" 4 "$(resize_nodes)"

# --- the registry pull-through cache is scaled down with the tenant (Q408) ---

reset_stubs gag-dogfood gag-dogfood-ci
mirror_deployed
run_main
scale_call="$(call_line 'kubectl scale')"
check_contains "scales the mirror deployments to zero" \
	"--replicas=0" "${scale_call}"
check_contains "scopes the scale-down to the mirror namespace" \
	"--namespace gag-registry-mirror" "${scale_call}"
check_contains "selects the mirror deployments by label" \
	"-l app=registry-mirror" "${scale_call}"
# Scaling is not deleting: the namespace, the NetworkPolicies and any PVC-backed
# layer cache have to survive the window for the next one to start warm.
check_not_contains "never deletes the mirror namespace" \
	"delete namespace" "$(cat "${CALL_LOG}")"

# `kubectl scale` has no --ignore-not-found, so a cluster set up before Q408 has
# to be read first — an unguarded scale would abort the whole teardown there.
reset_stubs gag-dogfood gag-dogfood-ci
run_main
check "a cluster with no mirror still tears down cleanly" 0 "${MAIN_RC}"
check_not_contains "never scales a mirror that is not deployed" \
	"kubectl scale" "$(cat "${CALL_LOG}")"

# --- a mismatched context aborts before anything is deleted ------------------

# The teardown's delete is destructive and unqualified by cluster; if the
# context is not the one asked for, it must never run.
reset_stubs gag-dogfood gag-dogfood-ci
CONTEXT="gke_other-proj_us-west1-a_prod"
run_main
check "a context that is not the target fails the teardown" 1 "${MAIN_RC}"
check_not_contains "never deletes against the wrong cluster" \
	"delete actionsgateway" "$(cat "${CALL_LOG}")"
check_not_contains "never resizes the wrong cluster" \
	"clusters resize" "$(cat "${CALL_LOG}")"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all e2e-stop.sh tests passed"
