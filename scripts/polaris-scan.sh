#!/usr/bin/env bash
#
# Render the Helm chart (digest-pinned, matching the production posture) and
# audit the rendered Kubernetes manifests with polaris. The gate fails on
# `danger` findings only; `warning` findings are reported but do not block —
# tuned exceptions live in charts/actions-gateway/polaris.yaml, each with a
# justifying comment. Backs `make polaris-scan` and mirrors the CI `polaris`
# job in .github/workflows/security-scan.yml exactly so local and CI verdicts
# match.
#
# Env:
#   POLARIS_RENDER_DIGEST  Placeholder digest used to render the chart — see
#                          scripts/lib/common.sh for why a digest is required.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd helm "https://helm.sh/docs/intro/install/"
require_cmd polaris "https://polaris.docs.fairwinds.com/infrastructure-as-code/#cli"

chart="$REPO_ROOT/charts/actions-gateway"
config="$chart/polaris.yaml"
render="$REPO_ROOT/.build/polaris-render.yaml"

mkdir -p "$REPO_ROOT/.build"
echo "==> helm template $chart (digest-pinned posture)"
helm template ag "$chart" --set-string "gmc.image.digest=$POLARIS_RENDER_DIGEST" >"$render"
echo "==> polaris audit (gate: danger findings fail; warnings reported)"
polaris audit --merge-config --config "$config" --audit-path "$render" \
	--format=pretty --only-show-failed-tests --set-exit-code-on-danger
