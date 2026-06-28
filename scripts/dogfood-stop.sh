#!/usr/bin/env bash
# Take the dogfood GKE cluster offline and route CI jobs back to GitHub-hosted.
# See docs/plan/gke-dogfood.md Part D.
#
# Required env vars (export before running):
#   CLUSTER   GKE cluster name (e.g. gag-dogfood)
#   ZONE      GCP zone (e.g. us-central1-a)
#   REPO      GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

main() {
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd gh "https://cli.github.com/"

	echo "Routing CI jobs back to GitHub-hosted runners..."
	gh variable set GAG_RUNNER \
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	echo "Scaling system pool to 0 nodes..."
	gcloud container clusters resize "${CLUSTER}" \
		--node-pool=default-pool --num-nodes=0 --zone="${ZONE}" --quiet

	echo "Done. Worker nodes drain and autoscale to 0 automatically (~10 min)."
}

main "$@"
