#!/usr/bin/env bash
# Take the dogfood GKE cluster offline and reset the GAG runner label. Opt-in
# dispatches are one-shot, so there is no standing push/PR CI route to revert;
# resetting the label just makes a later dispatch a safe no-op (and undoes any
# end-state global routing). See docs/plan/gke-dogfood.md Part D.
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
	require_cmd gh "https://cli.github.com/"

	echo "Resetting GAG runner label to ubuntu-latest..."
	gh variable set GAG_RUNNER \
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	echo "Scaling system pool to 0 nodes..."
	gcloud container clusters resize "${CLUSTER}" \
		--project="${PROJECT}" \
		--node-pool=default-pool --num-nodes=0 --zone="${ZONE}" --quiet

	echo "Done. Worker nodes drain and autoscale to 0 automatically (~10 min)."
}

main "$@"
