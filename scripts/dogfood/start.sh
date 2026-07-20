#!/usr/bin/env bash
# Bring the dogfood GKE cluster online and dispatch an isolated CI validation
# burst onto GAG. Ordinary push/PR CI is NOT re-routed — only the explicit
# workflow_dispatch runs below target GAG (opt-in model; see Q224/Q264 P4).
# See docs/plan/gke-dogfood.md Part D.
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

# System pool sizing for the running state. A single e2-standard-2 has 1930m
# allocatable against a ~1080m kube-system baseline, leaving room for exactly
# one 500m tenant AGC — so dogfood-agc and dogfoodss-agc race for the node and
# the loser stays Pending indefinitely. When dogfoodss wins, the Ready wait
# below (which selects instance=dogfood) times out and the caller exits 1. Two
# nodes fit both. Same capacity ceiling Q335 fixed for the on-demand e2e AGC in
# e2e-start.sh; dogfood/stop.sh still takes the pool to 0 at rest.
SYSTEM_POOL="${SYSTEM_POOL:-default-pool}"
SYSTEM_NODES="${SYSTEM_NODES:-2}"

main() {
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gke-gcloud-auth-plugin \
		"https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	require_cmd gh "https://cli.github.com/"

	# --project/--zone are pinned per call so the resize never relies on the
	# active gcloud config; a resize to the current node count is a no-op, so
	# this is safe to re-run.
	echo "Scaling system pool (${SYSTEM_POOL}) to ${SYSTEM_NODES} node(s)..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool="${SYSTEM_POOL}" --num-nodes="${SYSTEM_NODES}" --zone="${ZONE}" --quiet

	# Point kubectl at the dogfood cluster and fail closed if it is not the
	# active context, so the readiness waits never run against another cluster.
	gke_get_credentials_and_verify "${PROJECT}" "${ZONE}" "${CLUSTER}"

	echo "Waiting for GMC to be ready (~3 min)..."
	kubectl rollout status deployment/gmc-controller-manager \
		-n gmc-system --timeout=5m

	echo "Waiting for AGC pod..."
	kubectl wait --for=condition=Ready pod \
		-l app.kubernetes.io/name=actions-gateway-controller,app.kubernetes.io/instance=dogfood \
		-n gag-dogfood --timeout=3m

	# Set the runner label the opt-in dispatches consume. This alone does NOT
	# route any push/PR CI — the migrated jobs read GAG_RUNNER only on a
	# workflow_dispatch run with target_gag=true.
	echo "Setting GAG runner label..."
	gh variable set GAG_RUNNER \
		--body '["self-hosted","linux","gag-ci"]' \
		--repo "${REPO}"

	# Dispatch isolated validation bursts onto GAG. Scoped to these runs only —
	# other PRs, Dependabot, and push CI stay on GitHub-hosted runners.
	echo "Dispatching validation runs onto GAG (ref: main)..."
	gh workflow run unit-test.yml -f target_gag=true --ref main --repo "${REPO}"
	gh workflow run integration-test.yml -f target_gag=true --ref main --repo "${REPO}"

	echo "Done. Dispatched unit-test and integration-test onto GAG."
	echo "Watch: gh run list --workflow=unit-test.yml --repo ${REPO}"
}

main "$@"
