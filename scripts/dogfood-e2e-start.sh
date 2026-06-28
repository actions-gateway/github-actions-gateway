#!/usr/bin/env bash
# Route e2e CI jobs to GAG self-hosted runners (Kata-backed, on GKE).
# The system pool must already be running (run dogfood-start.sh first).
# See docs/plan/gke-dogfood.md Part F.
#
# Required env vars (export before running):
#   REPO   GitHub repo slug (e.g. actions-gateway/github-actions-gateway)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

main() {
	: "${REPO:?REPO must be set}"

	require_cmd gh "https://cli.github.com/"

	gh variable set GAG_E2E_RUNNER \
		--body '["self-hosted","linux","gag-ci-e2e"]' \
		--repo "${REPO}"

	echo "E2e jobs will now route to GAG (Kata runners on GKE)."
	echo "e2e pool nodes will autoscale 0→2 as jobs arrive."
}

main "$@"
