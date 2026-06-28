#!/usr/bin/env bash
# Route e2e CI jobs back to GitHub-hosted runners.
# The e2e node pool autoscales to 0 once jobs drain (~10 min).
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
		--body '"ubuntu-latest"' \
		--repo "${REPO}"

	echo "E2e jobs will now route to GitHub-hosted runners."
	echo "e2e pool nodes autoscale to 0 once in-flight jobs finish (~10 min)."
}

main "$@"
