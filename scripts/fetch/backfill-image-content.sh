#!/usr/bin/env bash
# backfill-image-content.sh — make sure the Docker daemon holds every layer blob
# of an image's platform manifest, so `docker save --platform` can export it.
#
# On the containerd image store, `docker pull` skips fetching a layer whose
# unpacked snapshot already exists. Another image sharing the uncompressed layer
# under a different compressed blob is enough, so a pull made after loading that
# image leaves the pulled manifest's blobs absent from the content store, and
# `docker save --platform` refuses it:
#
#   no suitable export target found: image with reference ... was found but
#   does not provide the specified platform (linux/amd64)
#
# A plain `docker save` of the same image exits 0 having written an archive with
# no layers, and `docker load` of that archive also exits 0 and leaves the same
# image behind. Upstream: moby/moby#52193.
#
# Measured 2026-11-01 on Docker 29.4.1 and 29.6.2 (containerd store): after
# `docker load` of the cert-manager v1.20.2 images, from a tar saved by either
# image store, a pull of metrics-server v0.8.1 fetches 4 of its 13 layers and the
# save fails as above (Q1242). A BuildKit export of `FROM <image>` on the
# daemon's own builder fetches the missing blobs into the content store it
# shares with the daemon, after which the original image exports unchanged.
# `--output type=cacheonly` fetches nothing, so the export is written and
# discarded.
#
# An image whose platform already exports is left untouched, so this is cheap
# to call on every cold pull.
#
# Usage:
#   scripts/fetch/backfill-image-content.sh <platform> <image-ref>...
#
# Environment:
#   RETRY_ATTEMPTS / RETRY_DELAY — forwarded to retry.sh for the BuildKit fetch.
#   REGISTRY_MIRRORS            — <upstream>=<mirror> map, as for
#                                 pull-image-with-retry.sh; unset fetches direct.

set -euo pipefail
shopt -s inherit_errexit

if (( $# < 2 )); then
  echo "usage: $0 <platform> <image-ref>..." >&2
  exit 2
fi
platform="$1"
shift

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${script_dir}/../lib/registry-mirror.sh"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# The daemon's own builder is named for its context. A docker-container builder
# made current by setup-buildx keeps its own content store, so it would fetch the
# blobs somewhere the daemon cannot see them.
builder="$(docker context show)"

# exports IMG — whether IMG's ${platform} manifest exports. Leaves save's stderr
# in ${work}/save.err.
exports() {
  local img="$1" rc=0
  docker save --platform "${platform}" -o "${work}/probe.tar" "${img}" 2> "${work}/save.err" || rc=$?
  rm -f "${work}/probe.tar"
  return "${rc}"
}

for img in "$@"; do
  if exports "${img}"; then
    continue
  fi
  if ! grep -q 'does not provide the specified platform' "${work}/save.err"; then
    cat "${work}/save.err" >&2
    exit 1
  fi
  echo "==> ${img} is missing layer blobs for ${platform}; fetching them through BuildKit"
  printf 'FROM %s\n' "$(mirror_rewrite "${img}")" > "${work}/Dockerfile"
  "${script_dir}/retry.sh" docker buildx build --builder "${builder}" \
    --platform "${platform}" --provenance=false \
    --output "type=docker,dest=${work}/backfill.tar" "${work}"
  rm -f "${work}/backfill.tar"
  if ! exports "${img}"; then
    cat "${work}/save.err" >&2
    echo "${img} still does not export ${platform} after the BuildKit fetch" >&2
    exit 1
  fi
done
