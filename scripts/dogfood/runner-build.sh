#!/usr/bin/env bash
# Build and push the build-capable dogfood runner image (Q239).
#
# This is a dev convenience, NOT part of the signed release pipeline: it lets the
# GKE dogfood run this repo's own make-based CI green by giving the worker pods a
# runner image that carries make + a C toolchain (the bare upstream
# actions-runner the AGC gap-fills by default has none, so every `make …` job
# fails exit 127). The Q235 wrapper is still injected on top at provision time.
# See scripts/dogfood/runner/Dockerfile and docs/plan/gke-dogfood.md.
#
# Pushes to ghcr.io/actions-gateway/dogfood-runner. Set it as the dogfood
# RunnerTemplate's workerImage (scripts/dogfood/setup.sh wires this when
# DOGFOOD_RUNNER_IMAGE is exported). Single-arch linux/amd64 — the dogfood
# workers pool is e2-standard-4 (amd64).
#
# Usage:
#   scripts/dogfood/runner-build.sh                 # build + push :<runner-version>
#   IMAGE_TAG=test scripts/dogfood/runner-build.sh  # override the tag
#
# Requires: docker (with buildx), gh (authenticated; supplies the GHCR token).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

readonly IMAGE_REPO="${IMAGE_REPO:-ghcr.io/actions-gateway/dogfood-runner}"
readonly DOCKERFILE="${REPO_ROOT}/scripts/dogfood/runner/Dockerfile"

# Derive the default tag from the runner version the Dockerfile is pinned to, so
# the published tag tracks the base it was built from.
runner_version() {
	awk -F: '/^FROM ghcr.io\/actions\/actions-runner:/ { split($2, a, "@"); print a[1] }' "${DOCKERFILE}"
}

main() {
	require_cmd docker "https://docs.docker.com/get-docker/"
	require_cmd gh "https://cli.github.com/"

	local tag
	tag="${IMAGE_TAG:-$(runner_version)}"
	if [[ -z "${tag}" ]]; then
		echo "could not derive runner version from ${DOCKERFILE}" >&2
		exit 1
	fi
	local ref="${IMAGE_REPO}:${tag}"

	echo "Logging in to ghcr.io with the gh token..."
	gh auth token | docker login ghcr.io -u "$(gh api user --jq .login)" --password-stdin

	echo "Building and pushing ${ref} (linux/amd64)..."
	docker buildx build \
		--platform linux/amd64 \
		--file "${DOCKERFILE}" \
		--tag "${ref}" \
		--push \
		"${REPO_ROOT}/scripts/dogfood/runner"

	echo
	echo "Pushed ${ref}"
	echo "Digest:"
	docker buildx imagetools inspect "${ref}" --format '{{json .Manifest.Digest}}'
	echo
	echo "Set it as the dogfood worker image and re-apply the tenant objects:"
	echo "  DOGFOOD_RUNNER_IMAGE=${ref} scripts/dogfood/setup.sh"
}

main "$@"
