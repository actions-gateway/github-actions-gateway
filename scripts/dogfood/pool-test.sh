#!/usr/bin/env bash
#
# Unit tests for scripts/dogfood/lib/pool.sh — the derived system-pool sizing
# the dogfood scripts share (Q357).
#
# Why this is tested: the sizing decides a billable GKE resize AND whether every
# tenant AGC can schedule. The two regressions that matter are silent — a count
# that misses a tenant leaves its AGC Pending forever, and a count that includes
# the on-demand e2e tenant grows the pool on every e2e re-run. kubectl is
# stubbed, so no network and no cluster.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/dogfood/lib/pool.sh
source "${REPO_ROOT}/scripts/dogfood/lib/pool.sh"

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

# --- system_nodes_for: one node per always-on AGC, floored at 2 ---

check "0 AGCs floors at 2" 2 "$(system_nodes_for 0)"
check "1 AGC floors at 2" 2 "$(system_nodes_for 1)"
check "2 AGCs need 2 nodes" 2 "$(system_nodes_for 2)"
check "3 AGCs need 3 nodes" 3 "$(system_nodes_for 3)"
check "5 AGCs need 5 nodes" 5 "$(system_nodes_for 5)"

# --- count_always_on_gateways: stubbed kubectl ---

# FAKE_NAMESPACES models `kubectl get actionsgateways -A -o custom-columns=NS`
# output (one namespace per line); FAKE_KUBECTL_FAIL models a cluster that
# cannot answer (CRD not installed yet).
kubectl() {
	if [[ -n "${FAKE_KUBECTL_FAIL:-}" ]]; then
		return 1
	fi
	printf '%s' "${FAKE_NAMESPACES:-}"
}

FAKE_NAMESPACES=$'gag-dogfood\ngag-dogfoodss'
check "counts the always-on tenants" 2 "$(count_always_on_gateways)"

FAKE_NAMESPACES=$'gag-dogfood\ngag-dogfoodss\ngag-dogfood-e2e'
check "excludes the on-demand e2e tenant" 2 "$(count_always_on_gateways)"

FAKE_NAMESPACES=$'gag-dogfood\ngag-dogfoodss\ngag-third'
check "sees a third always-on tenant" 3 "$(count_always_on_gateways)"

FAKE_NAMESPACES=""
check "empty cluster counts 0" 0 "$(count_always_on_gateways)"

FAKE_KUBECTL_FAIL=1
check "kubectl failure counts 0 (floor absorbs it)" 0 "$(count_always_on_gateways)"
unset FAKE_KUBECTL_FAIL

# --- required_system_nodes: composition ---

FAKE_NAMESPACES=$'gag-dogfood\ngag-dogfoodss\ngag-dogfood-e2e'
check "today's cluster derives the validated 2" 2 "$(required_system_nodes)"

FAKE_NAMESPACES=$'gag-dogfood\ngag-dogfoodss\ngag-third\ngag-dogfood-e2e'
check "a third tenant derives 3" 3 "$(required_system_nodes)"

FAKE_KUBECTL_FAIL=1
check "unanswerable cluster derives the floor" 2 "$(required_system_nodes)"
unset FAKE_KUBECTL_FAIL

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all pool sizing tests passed"
