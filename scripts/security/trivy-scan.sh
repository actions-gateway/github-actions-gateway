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
#   TRIVY_IMAGES       Space-separated image names — each is a named stage of
#                      the root Dockerfile and a leg of the CI trivy matrix.
#   TRIVY_REPORT_ONLY  Space-separated image names scanned report-only
#                      (findings printed, never fail the scan): the worker
#                      image is built FROM the upstream actions-runner and
#                      carries upstream CVEs we cannot fix. Matches the worker
#                      leg's exit-code 0 in security-scan.yml.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

require_cmd trivy "https://trivy.dev/latest/getting-started/installation/"
require_cmd docker "https://docs.docker.com/get-docker/"

TRIVY_SEVERITY="${TRIVY_SEVERITY:-HIGH,CRITICAL}"
TRIVY_IMAGES="${TRIVY_IMAGES:-gmc agc proxy worker wrapper build-runner fakegithub}"
# build-runner is `worker` plus a Docker client, so it inherits that image's
# actions-runner base and its CVE floor; report-only for the same reason.
TRIVY_REPORT_ONLY="${TRIVY_REPORT_ONLY:-worker build-runner}"

for name in $TRIVY_IMAGES; do
	code=1
	for ro in $TRIVY_REPORT_ONLY; do
		[[ "$ro" == "$name" ]] && code=0
	done
	echo "==> building local/$name:trivy from the root Dockerfile (target $name)"
	# The base-image pull inside the build reaches the registry with no retry of
	# its own, so one transient denial fails the whole scan (Q863). retry.sh
	# re-runs the build; buildkit serves whatever layers the previous attempt
	# completed, so a retry is cheap. The CI trivy matrix this script mirrors
	# gets the same second attempt, spelled as a step because a `uses:` step
	# cannot be wrapped in a shell loop.
	RETRY_ATTEMPTS=3 "$REPO_ROOT/scripts/fetch/retry.sh" \
		docker buildx build --load -t "local/$name:trivy" --target "$name" -f Dockerfile .
	echo "==> trivy image local/$name:trivy (exit-code $code)"
	trivy image --severity "$TRIVY_SEVERITY" --ignore-unfixed --exit-code "$code" "local/$name:trivy"
done
