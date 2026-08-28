#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/e2e-start.sh: the on-demand e2e tenant bring-up
# — pool sizing, the overlay apply, the AGC readiness wait, and the routing flip
# the wait gates.
#
# Why it is tested: the readiness wait is the whole bring-up's verdict, and both
# of its failure directions are silent. A healthy tenant reported as timed out
# is what killed the v1.3.0-rc.3 validation gate from start.sh (a pod-label wait
# selecting the outgoing ReplicaSet's terminating pod); here the same verdict
# turns on the wait's shape, on running after the apply that creates the CR, and
# on a resize that never drops the system pool below the size the deployed
# tenant AGCs need — undersize it and the on-demand AGC stays Pending for the
# whole timeout on a cluster that is fine (Q335/Q357). The other direction is
# worse: a wait whose failure does not abort would flip repo-wide e2e routing
# onto a tenant that never came up, wedging every other session's CI — the
# 2026-07-31 incident, whose blast radius is why routing is opt-in at all.
#
# The script is sourced with E2E_START_LIB_ONLY=1 so main() does not run;
# `kubectl`, `gcloud`, `gh` and require_cmd are stubbed, so no cluster and no
# GitHub. The pool sizing runs for real against a stubbed gateway listing, so
# this covers the e2e-start.sh/lib/pool.sh seam rather than restating it.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
E2E_START_LIB_ONLY=1
export E2E_START_LIB_ONLY
# shellcheck source=scripts/dogfood/e2e-start.sh
source "${REPO_ROOT}/scripts/dogfood/e2e-start.sh"

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
# both what was called and in what order — ordering is load-bearing here (a wait
# that runs before the apply, or a resize that runs after the wait, reports a
# healthy bring-up as a failure). The log is a file because main() runs in a
# subshell.
CALL_LOG="${WORKDIR}/calls.log"
GATEWAYS_FILE="${WORKDIR}/gateways"
WAIT_EXIT=0
ROLLOUT_EXIT=0

kubectl() {
	printf 'kubectl %s\n' "$*" >>"${CALL_LOG}"
	case "$*" in
	get\ actionsgateways*) cat "${GATEWAYS_FILE}" ;;
	config\ current-context) echo "gke_${PROJECT}_${ZONE}_${CLUSTER}" ;;
	wait*) return "${WAIT_EXIT}" ;;
	rollout\ status*) return "${ROLLOUT_EXIT}" ;;
	esac
	return 0
}

gcloud() { printf 'gcloud %s\n' "$*" >>"${CALL_LOG}"; }
gh() { printf 'gh %s\n' "$*" >>"${CALL_LOG}"; }
require_cmd() { :; }

# reset_stubs NAMESPACE... — arm the gateway listing with the given namespaces
# (one ActionsGateway each) and restore the default knobs.
reset_stubs() {
	printf '%s\n' "$@" >"${GATEWAYS_FILE}"
	: >"${CALL_LOG}"
	WAIT_EXIT=0
	ROLLOUT_EXIT=0
	E2E_VARIANT=kata
	E2E_SYSTEM_NODES=2
	unset E2E_ROUTE_VAR
	unset REGISTRY_MIRROR_PERSISTENT
}

# run_main — run main() in a subshell and record its status in MAIN_RC and its
# combined output in MAIN_OUT.
#
# The subshell must not be an operand of `||` or `if`: bash suppresses errexit
# inside such a subshell even when it re-runs `set -e` itself (measured, bash
# 5.3), which would let main() sail past a failed `kubectl wait` and make the
# abort assertions below pass vacuously. Hence set +e around a plain call.
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

echo "scripts/dogfood/e2e-start-test.sh"

# --- the readiness wait tracks the tenant gateway, not a pod selector --------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
check "a healthy bring-up succeeds" 0 "${MAIN_RC}"
# Needle the namespace too: e2e-start.sh makes a second `kubectl wait`, for the
# registry mirror, and call_line returns the first match.
wait_call="$(call_line 'kubectl wait --namespace gag-dogfood-e2e')"
check_contains "waits on the tenant gateway's Ready condition" \
	"--for=condition=Ready actionsgateway/dogfood-e2e" "${wait_call}"
check_contains "scopes the wait to the e2e tenant namespace" \
	"--namespace gag-dogfood-e2e" "${wait_call}"
check_contains "bounds the wait" "--timeout=" "${wait_call}"
# The rc.3 regression shape: a pod selector matches the outgoing ReplicaSet's
# terminating pod, so it burns the whole timeout and then reports every selected
# pod — the healthy new one included — as timed out.
check_not_contains "never waits on a pod label selector" \
	"condition=Ready pod" "${wait_call}"

# --- ordering: the wait can only tell the truth if it runs last --------------

# The gateway CR does not exist until the overlay is applied, and `kubectl wait`
# on an absent named resource fails immediately rather than waiting for it.
check_before "applies the overlay before waiting on it" \
	"apply -k" "kubectl wait"
# The AGC has nowhere to schedule until the pool has grown, so a resize after
# the wait leaves it Pending for the whole timeout (Q335).
check_before "grows the system pool before waiting for the AGC" \
	"clusters resize" "kubectl wait"
# Nothing may mutate a cluster before the context is pinned and verified.
check_before "pins the cluster context before the resize" \
	"config current-context" "clusters resize"
check_contains "pins the project on the resize" "--project=${PROJECT}" \
	"$(call_line 'clusters resize')"
check_contains "pins the zone on the resize" "--zone=${ZONE}" \
	"$(call_line 'clusters resize')"

# --- the resize never drops below the size the deployed AGCs need ------------

check "sizes to the e2e window default when it covers the tenants" \
	2 "$(resize_nodes)"

# Three always-on tenants need three nodes; resizing to the bare e2e default
# would evict a tenant AGC, and the eviction reads as an e2e wait timeout.
reset_stubs gag-dogfood gag-dogfood-ci gag-dogfood-three
run_main
check "never resizes below the derived running size" 3 "$(resize_nodes)"

# The on-demand e2e AGC packs into the non-first nodes' headroom, so its own
# namespace must not inflate the count and strand a billable node.
reset_stubs gag-dogfood gag-dogfood-ci gag-dogfood-e2e
run_main
check "excludes the e2e tenant from the derived size" 2 "$(resize_nodes)"

reset_stubs gag-dogfood gag-dogfood-ci
E2E_SYSTEM_NODES=5
run_main
check "honors an explicit size above the derived one" 5 "$(resize_nodes)"

# --- a failed wait aborts, and never leaves routing on a tenant that is down --

# The direction that hurts most: reporting a broken tenant as ready. With
# repo-wide routing opted into, a wait whose failure did not abort would point
# every other session's e2e jobs at an AGC that never came up.
reset_stubs gag-dogfood gag-dogfood-ci
WAIT_EXIT=1
E2E_ROUTE_VAR=1
run_main
check "a failed readiness wait fails the bring-up" 1 "${MAIN_RC}"
check_not_contains "never routes e2e jobs at a tenant that is not Ready" \
	"variable set" "$(cat "${CALL_LOG}")"

# --- routing is opt-in (2026-07-31 incident) ---------------------------------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
check "the default bring-up succeeds" 0 "${MAIN_RC}"
check_not_contains "leaves repo-wide routing untouched by default" \
	"variable set" "$(cat "${CALL_LOG}")"
check_contains "prints the run-scoped dispatch instead" \
	"-f runner=" "${MAIN_OUT}"

reset_stubs gag-dogfood gag-dogfood-ci
E2E_ROUTE_VAR=1
run_main
route_call="$(call_line 'variable set')"
check_contains "opts into the repo-wide window on request" \
	"variable set GAG_E2E_RUNNER" "${route_call}"
# A single JSON string, not the Classic multi-label array: both routing paths
# resolve it through fromJSON, and it must equal the RunnerSet's one runnerLabel
# or dispatched jobs queue forever unmatched.
check_contains "routes to the scale set as a single JSON string" \
	'--body "gag-ci-e2e"' "${route_call}"
check_before "flips routing only after the tenant is Ready" \
	"kubectl wait" "variable set"

# --- the registry pull-through cache comes up with the tenant (Q408) ---------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
mirror_apply="$(call_line 'deploy/registry-mirror')"
check_contains "applies the registry pull-through cache" \
	"apply -k" "${mirror_apply}"
check_not_contains "renders the ephemeral base by default" \
	"registry-mirror/overlays/persistent" "${mirror_apply}"
check_contains "waits on the mirror deployments by label" \
	"-l app=registry-mirror" \
	"$(call_line 'kubectl wait --namespace gag-registry-mirror')"
# Five image pulls must not queue in front of the bring-up's own verdict.
check_before "waits for the AGC before applying the mirror" \
	"actionsgateway/dogfood-e2e" "deploy/registry-mirror"

reset_stubs gag-dogfood gag-dogfood-ci
REGISTRY_MIRROR_PERSISTENT=1
run_main
check_contains "renders the persistent overlay on opt-in" \
	"deploy/registry-mirror/overlays/persistent" \
	"$(call_line 'deploy/registry-mirror')"

# Routing is the first point at which a job can reach this tenant, so the mirror
# has to be up before it flips — otherwise the first routed job races the cache.
reset_stubs gag-dogfood gag-dogfood-ci
E2E_ROUTE_VAR=1
run_main
check_before "brings the mirror up before routing sends a job" \
	"gag-registry-mirror" "variable set"

# --- the isolation variant selects its overlay, and is validated first -------

reset_stubs gag-dogfood gag-dogfood-ci
run_main
check_contains "applies the kata overlay by default" \
	"deploy/dogfood-e2e/overlays/kata" "$(call_line 'apply -k')"

reset_stubs gag-dogfood gag-dogfood-ci
E2E_VARIANT=dind
run_main
check_contains "applies the dind overlay on opt-in" \
	"deploy/dogfood-e2e/overlays/dind" "$(call_line 'apply -k')"

reset_stubs gag-dogfood gag-dogfood-ci
E2E_VARIANT=bogus
run_main
check "rejects an unknown isolation variant" 1 "${MAIN_RC}"
check_contains "names the rejected variant" "bogus" "${MAIN_OUT}"
# Rejecting after the resize would leave a billable node pool grown for a run
# that cannot happen.
check_not_contains "rejects before any cluster mutation" \
	"clusters resize" "$(cat "${CALL_LOG}")"
check_not_contains "rejects before applying an overlay" \
	"apply -k" "$(cat "${CALL_LOG}")"

# --- the GMC rollout is waited for before the tenant apply -------------------
#
# The apply is converted by GMC's webhook, so from the 0-node at-rest state it
# fails with `no endpoints available for service "webhook-service"` unless GMC
# has rolled out first. Both the wait's presence and its position are asserted:
# placed before the resize it would wait on a cluster with no node to schedule
# GMC on, and placed after the apply it would not gate anything.

reset_stubs gag-dogfood gag-dogfood-ci
run_main
check_contains "waits for the GMC rollout" \
	"rollout status deployment/gmc-controller-manager" "$(cat "${CALL_LOG}")"
check_before "waits for GMC only once a node exists to schedule it" \
	"clusters resize" "rollout status"
check_before "waits for GMC before the apply its webhook converts" \
	"rollout status" "apply -k"

# The failure direction: a GMC that never rolls out must abort the bring-up
# rather than let the apply fail on the webhook message that names the
# dataplane instead of the cause.
reset_stubs gag-dogfood gag-dogfood-ci
ROLLOUT_EXIT=1
E2E_ROUTE_VAR=1
run_main
check "a failed GMC rollout fails the bring-up" 1 "${MAIN_RC}"
check_not_contains "never applies the tenant without the conversion webhook" \
	"apply -k" "$(cat "${CALL_LOG}")"
check_not_contains "never routes e2e jobs when GMC is down" \
	"variable set" "$(cat "${CALL_LOG}")"

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all e2e-start.sh tests passed"
