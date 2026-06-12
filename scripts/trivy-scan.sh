#!/usr/bin/env bash
#
# Build each production/test image locally and scan it with trivy. Backs
# `make trivy-scan`; parameters mirror the CI `trivy` matrix job in
# .github/workflows/security-scan.yml exactly so local and CI verdicts match.
#
# Env:
#   TRIVY_SEVERITY     Severities that fail the scan (default HIGH,CRITICAL).
#                      --ignore-unfixed additionally drops CVEs with no
#                      released fix (nothing actionable here); only fixable
#                      findings fail.
#   TRIVY_IMAGES       Space-separated "<name>=<dockerfile>" entries — the five
#                      images the CI trivy matrix scans.
#   TRIVY_REPORT_ONLY  Space-separated image names scanned report-only
#                      (findings printed, never fail the scan): the worker
#                      image is built FROM the upstream actions-runner and
#                      carries upstream CVEs we cannot fix. Matches the worker
#                      leg's exit-code 0 in security-scan.yml.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd trivy "https://trivy.dev/latest/getting-started/installation/"
require_cmd docker "https://docs.docker.com/get-docker/"

TRIVY_SEVERITY="${TRIVY_SEVERITY:-HIGH,CRITICAL}"
TRIVY_IMAGES="${TRIVY_IMAGES:-gmc=cmd/gmc/Dockerfile agc=cmd/agc/Dockerfile proxy=cmd/proxy/Dockerfile worker=cmd/worker/Dockerfile fakegithub=test/fakegithub/Dockerfile}"
TRIVY_REPORT_ONLY="${TRIVY_REPORT_ONLY:-worker}"

for entry in $TRIVY_IMAGES; do
	name="${entry%%=*}"
	dockerfile="${entry#*=}"
	code=1
	for ro in $TRIVY_REPORT_ONLY; do
		[[ "$ro" == "$name" ]] && code=0
	done
	echo "==> building local/$name:trivy from $dockerfile"
	docker buildx build --load -t "local/$name:trivy" -f "$dockerfile" .
	echo "==> trivy image local/$name:trivy (exit-code $code)"
	trivy image --severity "$TRIVY_SEVERITY" --ignore-unfixed --exit-code "$code" "local/$name:trivy"
done
