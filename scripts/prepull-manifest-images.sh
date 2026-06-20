#!/usr/bin/env bash
# prepull-manifest-images.sh — pre-pull the container images referenced by a
# pinned Kubernetes manifest into the runner's local Docker daemon, caching the
# result as a tarball so warm runs skip the registry entirely.
#
# Several e2e images (Calico CNI, cert-manager, metrics-server) are otherwise
# pulled by kubelet from quay.io / registry.k8s.io on the kind *nodes* during
# install — a recurring flake source under registry rate limits and a latency
# cost on the test critical path. The fix is uniform: pull the exact refs the
# pinned manifest names into the runner's Docker daemon here (cached + retried),
# then `kind load` whatever is present onto the nodes so the in-cluster pull is a
# local hit. This script is the shared pre-pull half of that pattern; the
# kind-load half stays with each consumer (its placement differs: cert-manager
# rides the cluster build step, metrics-server has its own preload step, Calico
# loads inside kind-with-registry.sh).
#
# The image list is extracted from the same manifest the consumer applies, so the
# pre-pulled set can never drift from what is referenced. On a cache hit the tar
# is loaded directly and no manifest fetch happens; the extracted list is
# persisted alongside the tar (images.txt) so a consumer that kind-loads the
# images needs neither a re-fetch nor a re-extract.
#
# Usage:
#   scripts/prepull-manifest-images.sh <name> <manifest-url> <cache-dir>
#
#   name         — friendly label used in log lines (e.g. cert-manager)
#   manifest-url — URL of the pinned manifest to read image refs from
#   cache-dir    — directory (an actions/cache path) holding images.tar +
#                  images.txt; created on a cache miss
#
# Environment:
#   PULL_RETRY_ATTEMPTS — forwarded to pull-image-with-retry.sh (default: 3)
#   PULL_RETRY_DELAY    — forwarded to pull-image-with-retry.sh (default: 15)

set -euo pipefail

name=${1:-}
url=${2:-}
dir=${3:-}
if [[ -z "${name}" || -z "${url}" || -z "${dir}" ]]; then
  echo "usage: $0 <name> <manifest-url> <cache-dir>" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tar="${dir}/images.tar"
list="${dir}/images.txt"

# Cache hit: the tar and its extracted list are both present, so load and return
# without touching the network.
if [[ -f "${tar}" && -f "${list}" ]]; then
  echo "==> loading ${name} images from cache"
  docker load -i "${tar}"
  exit 0
fi

# Cache miss: fetch the pinned manifest and extract the image refs it names.
manifest="$(mktemp)"
trap 'rm -f "${manifest}"' EXIT
curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o "${manifest}" "${url}"

mapfile -t images < <(awk '$1 == "image:" { gsub(/"/, "", $2); print $2 }' "${manifest}" | sort -u)
if (( ${#images[@]} == 0 )); then
  echo "no images found in ${name} manifest at ${url}" >&2
  exit 1
fi
echo "${name} images: ${images[*]}"

for img in "${images[@]}"; do
  PULL_RETRY_ATTEMPTS="${PULL_RETRY_ATTEMPTS:-3}" PULL_RETRY_DELAY="${PULL_RETRY_DELAY:-15}" \
    "${script_dir}/pull-image-with-retry.sh" "${img}"
done

mkdir -p "${dir}"
docker save -o "${tar}" "${images[@]}"
printf '%s\n' "${images[@]}" > "${list}"
