#!/usr/bin/env bash
# Route e2e CI jobs back to GitHub-hosted runners and tear down the on-demand
# e2e tenant's AGC. The e2e node pool autoscales to 0 once jobs drain (~10 min).
# See docs/plan/gke-dogfood.md Part F.
#
# On-demand (Q231): deleting the ActionsGateway makes the GMC tear down the
# per-tenant AGC pod, freeing its standing ~500m CPU on the system node. The
# namespace, App Secret, ResourceQuota, ClusterRunnerTemplate, RunnerSet, and
# NetworkPolicy are left in place — inert without the gateway and cheap to keep,
# so a later e2e-start.sh re-applies the gateway and the AGC comes back.
#
# Capacity (Q335/Q357): e2e-start.sh scaled the system pool up for the e2e
# window; this script scales it back to the running size dogfood/start.sh
# leaves it in (derived from the deployed always-on tenants — lib/pool.sh).
# dogfood/stop.sh later takes the whole system pool to 0 for the
# zero-cost-at-rest state.
#
# Required env vars (export before running):
#   PROJECT   GCP project ID (e.g. actions-gateway-dogfood)
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-east1-b)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/dogfood/lib/pool.sh
source "${REPO_ROOT}/scripts/dogfood/lib/pool.sh"

# System pool sizing (Q335/Q357). After the e2e window this script restores the
# running size dogfood/start.sh computes — derived from the deployed always-on
# ActionsGateways (lib/pool.sh), so both scripts agree by construction and a
# third tenant is not re-evicted here. SYSTEM_POOL_AT_REST_NODES pins the
# restore size explicitly instead.
SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
SYSTEM_POOL_AT_REST_NODES="${SYSTEM_POOL_AT_REST_NODES:-}"

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gh "https://cli.github.com/"
	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"

	# Route e2e off GAG FIRST, so no new e2e job lands on the tenant while its
	# AGC is being torn down.
	gh variable set GAG_E2E_RUNNER \
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	# Pin the target cluster and fail closed if it is not the active context,
	# so the teardown delete never lands on another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	# Tear down only the ActionsGateway — the GMC deletes the AGC pod, releasing
	# the standing ~500m CPU. Everything else in the tenant is left in place.
	echo "Tearing down the on-demand e2e AGC (deleting the ActionsGateway)..."
	kubectl delete actionsgateway dogfood-e2e \
		--namespace gag-dogfood-e2e --ignore-not-found

	# Restore the system pool to the running size now the e2e window is over
	# (Q335/Q357) — derived from the deployed always-on ActionsGateways unless
	# pinned. The e2e gateway was deleted above (and its namespace is excluded
	# from the count anyway), so the derivation sees only the always-on tenants.
	# --project/--zone are pinned per call so the resize never relies on the
	# active gcloud config; a resize to the current node count is a no-op, so
	# this is safe to re-run. dogfood/stop.sh later takes it to 0.
	local restore="${SYSTEM_POOL_AT_REST_NODES}"
	[[ -n "${restore}" ]] || restore="$(required_system_nodes)"
	echo "Scaling system pool (${SYSTEM_POOL}) back to ${restore} node(s)..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes="${restore}" --zone="${ZONE}" --quiet

	echo "E2e jobs will now route to GitHub-hosted runners."
	echo "e2e pool nodes autoscale to 0 once in-flight jobs finish (~10 min)."
}

main "$@"
