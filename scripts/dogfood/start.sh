#!/usr/bin/env bash
# Bring the dogfood GKE cluster online and route CI jobs to GAG.
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

	echo "Scaling system pool to 1 node..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool=default-pool --num-nodes=1 --zone="${ZONE}" --quiet

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

	echo "Routing CI jobs to GAG..."
	gh variable set GAG_RUNNER \
		--body '["self-hosted","linux","gag-ci"]' \
		--repo "${REPO}"

	echo "Done. CI jobs now route to GAG self-hosted runners."
}

main "$@"
