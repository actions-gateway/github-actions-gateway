#!/usr/bin/env bash
# Bring the dogfood GKE cluster online and route CI jobs to GAG.
# See docs/plan/gke-dogfood.md Part D.
#
# Required env vars (export before running):
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-central1-a)
#   REPO      GitHub repo slug (e.g. karlkfi/github-actions-gateway)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

main() {
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd kubectl "https://kubernetes.io/docs/tasks/tools/"
	require_cmd gh "https://cli.github.com/"

	echo "Scaling system pool to 1 node..."
	gcloud container clusters resize "${CLUSTER}" \
		--node-pool=default-pool --num-nodes=1 --zone="${ZONE}" --quiet

	echo "Waiting for GMC to be ready (~3 min)..."
	kubectl rollout status deployment/gmc-controller \
		-n gmc-system --timeout=5m

	echo "Waiting for AGC pod..."
	kubectl wait --for=condition=Ready pod \
		-l app=agc -n gag-dogfood --timeout=3m

	echo "Routing CI jobs to GAG..."
	gh variable set GAG_RUNNER \
		--body '["self-hosted","linux","gag-ci"]' \
		--repo "${REPO}"

	echo "Done. CI jobs now route to GAG self-hosted runners."
}

main "$@"
