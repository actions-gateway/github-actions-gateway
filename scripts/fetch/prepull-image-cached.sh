#!/usr/bin/env bash
# prepull-image-cached.sh — pre-pull one pinned image into the runner's local
# Docker daemon, caching it as a tarball so warm runs skip the registry entirely,
# and expose it under a stable local tag.
#
# The same load-or-pull-and-save shape as prepull-manifest-images.sh, for the
# single-image case where the ref is known up front rather than extracted from a
# manifest. Written for the buildkit builder image (Q460): the security-scan
# trivy job runs seven matrix shards, each of which otherwise pulls the same
# ~200 MB image from Docker Hub at the same moment — seven concurrent anonymous
# pulls per run, which is the rate-limit pressure that flaked #895.
#
# WHY A LOCAL TAG. `docker load` cannot restore a manifest digest: an image saved
# and reloaded comes back with its RepoTags but no RepoDigests, so a digest-pinned
# `name:tag@sha256:...` ref never resolves from the cache and the consumer pulls
# from the registry anyway — the cache would be dead weight, silently. Measured
# on Docker 29.6 (containerd image store); the same is true of the classic store.
# So the cached bytes are re-exposed under <local-tag>, which round-trips through
# save/load intact, and the consumer is pointed at that instead.
#
# This keeps the digest pin honest rather than weakening it. The cold-path pull
# is still by the digest-pinned <image-ref>, so Docker verifies the manifest
# digest before anything is cached; the cache key at the call site carries that
# same digest, so a bump invalidates the entry. <local-tag> can therefore only
# ever name the exact bytes the pin selected.
#
# Usage:
#   scripts/fetch/prepull-image-cached.sh <image-ref> <cache-dir> <local-tag>
#
#   image-ref  — the pinned upstream ref to pull (normally name:tag@sha256:...)
#   cache-dir  — directory (an actions/cache path) holding image.tar; created on
#                a cache miss
#   local-tag  — local-only tag the image is exposed under, for the consumer to
#                reference (e.g. local/buildkit:pinned)
#
# Environment:
#   PULL_RETRY_ATTEMPTS / PULL_RETRY_DELAY / PULL_RETRY_MAX_DELAY
#     — forwarded to pull-image-with-retry.sh, which owns the retry schedule.

set -euo pipefail
shopt -s inherit_errexit

image="${1:-}"
dir="${2:-}"
local_tag="${3:-}"
if [[ -z "${image}" || -z "${dir}" || -z "${local_tag}" ]]; then
  echo "usage: $0 <image-ref> <cache-dir> <local-tag>" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tar="${dir}/image.tar"

# Cache hit: load and return without touching the registry. The tag is verified
# rather than assumed — a tar written by an older call site, or a truncated
# restore, would otherwise leave the consumer to fail obscurely on a missing
# image. On any doubt, fall through to the cold path.
if [[ -f "${tar}" ]]; then
  echo "==> loading ${local_tag} from cache"
  if docker load -i "${tar}" && docker image inspect "${local_tag}" > /dev/null 2>&1; then
    exit 0
  fi
  echo "cached tar did not yield ${local_tag}; re-pulling ${image}" >&2
  rm -f "${tar}"
fi

# Cache miss. The pull is digest-verified by Docker and bounded by
# pull-image-with-retry.sh, so an unreachable registry fails here, clearly.
echo "==> pulling ${image}"
"${script_dir}/pull-image-with-retry.sh" "${image}"

docker tag "${image}" "${local_tag}"
mkdir -p "${dir}"
docker save -o "${tar}" "${local_tag}"
