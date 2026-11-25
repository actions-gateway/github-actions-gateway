#!/usr/bin/env bash
#
# Render the Helm charts that run pods (digest-pinned, matching the production
# posture) and audit the rendered Kubernetes manifests with polaris. The gate fails on
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
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd helm "https://helm.sh/docs/intro/install/"
require_cmd polaris "https://polaris.docs.fairwinds.com/infrastructure-as-code/#cli"

chart="$REPO_ROOT/charts/actions-gateway"
config="$chart/polaris.yaml"
render="$REPO_ROOT/.build/polaris-render.yaml"
oom_render="$REPO_ROOT/.build/polaris-render-oom-reporter.yaml"

mkdir -p "$REPO_ROOT/.build"
echo "==> helm template $chart (digest-pinned posture)"
helm template ag "$chart" "${RENDER_DIGEST_ARGS[@]}" >"$render"
echo "==> helm template charts/oom-reporter (digest-pinned posture)"
helm template oom "$REPO_ROOT/charts/oom-reporter" "${OOM_REPORTER_RENDER_ARGS[@]}" >"$oom_render"
for audited in "$render" "$oom_render"; do
	echo "==> polaris audit ${audited#"$REPO_ROOT"/} (gate: danger findings fail; warnings reported)"
	polaris audit --merge-config --config "$config" --audit-path "$audited" \
		--format=pretty --only-show-failed-tests --set-exit-code-on-danger
done
