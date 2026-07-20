# Shared system-pool sizing for the dogfood scripts. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/dogfood/lib/pool.sh
#   source "$REPO_ROOT/scripts/dogfood/lib/pool.sh"
#
# Derives how many system-pool nodes the deployed tenant AGCs need, instead of
# each script hardcoding a count (Q357). The hardcoded predecessors kept
# re-hitting the same ceiling as tenants were added (Q335, PR #709): a new
# ~500m AGC no longer fit, the loser of the scheduling race stayed Pending
# forever, and the cause was only visible by measuring allocatable vs. requests
# on a live node. Deriving the size makes a new tenant grow the pool instead of
# silently re-breaking scheduling.
#
# Callers must have `set -euo pipefail` active and point kubectl at the dogfood
# cluster first (count_always_on_gateways reads the active context — gate it
# with gke_get_credentials_and_verify).
# shellcheck shell=bash

# Floor for the running system pool. Two nodes is the live-validated running
# size (PR #709): the first e2-standard-2 offers 1930m allocatable against a
# ~1080m kube-system baseline — room for exactly one 500m tenant AGC — while
# later nodes carry only the DaemonSet share and fit more.
SYSTEM_POOL_NODE_FLOOR=2

# The on-demand e2e tenant's namespace, excluded from the always-on count: its
# AGC exists only inside the e2e window and packs into the larger headroom of
# the non-first nodes (live-validated: both always-on AGCs plus the e2e AGC on
# 2 nodes, PR #709), so it does not add a node even when its ActionsGateway is
# present.
E2E_TENANT_NAMESPACE="gag-dogfood-e2e"

# system_nodes_for COUNT — print the node count that fits COUNT always-on
# tenant AGCs: one node per AGC, floored at SYSTEM_POOL_NODE_FLOOR. One node
# per AGC is deliberately conservative (non-first nodes fit more than one);
# a spare node during the running window is cheaper than a Pending AGC. Pure
# (no cluster access) so pool-test.sh asserts it directly.
system_nodes_for() {
	local count="$1"
	if ((count > SYSTEM_POOL_NODE_FLOOR)); then
		echo "${count}"
	else
		echo "${SYSTEM_POOL_NODE_FLOOR}"
	fi
}

# count_always_on_gateways — print the number of ActionsGateway CRs outside the
# on-demand e2e tenant namespace; each one gets a standing ~500m AGC pod on the
# system pool. Reads the active kubectl context. A cluster that cannot answer
# (CRD not yet installed — a fresh cluster before setup.sh) counts as 0, which
# the floor absorbs to today's baseline.
count_always_on_gateways() {
	local namespaces
	namespaces="$(kubectl get actionsgateways --all-namespaces \
		-o custom-columns='NS:.metadata.namespace' --no-headers 2>/dev/null)" || namespaces=""
	local count=0 ns
	while IFS= read -r ns; do
		[[ -z "${ns}" || "${ns}" == "${E2E_TENANT_NAMESPACE}" ]] && continue
		count=$((count + 1))
	done <<<"${namespaces}"
	echo "${count}"
}

# required_system_nodes — print the derived running size of the system pool:
# one node per deployed always-on tenant AGC, floored at the validated baseline.
required_system_nodes() {
	system_nodes_for "$(count_always_on_gateways)"
}
