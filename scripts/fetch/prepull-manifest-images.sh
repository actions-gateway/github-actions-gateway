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
# The fetched manifest is persisted too (manifest.yaml), so a consumer that
# *applies* it can do so from the cache rather than fetching the same URL a
# second time — unretried, on the e2e critical path (Q1125). It is written on a
# miss only, so a consumer reading it must key its actions/cache entry such that
# every entry was written by a version of this script that persists it: an entry
# predating it still hits, loads the tar and returns, so the consumer fails on a
# missing path rather than silently refetching.
#
# Usage:
#   scripts/fetch/prepull-manifest-images.sh <name> <manifest-url> <cache-dir>
#
#   name         — friendly label used in log lines (e.g. cert-manager)
#   manifest-url — URL of the pinned manifest to read image refs from
#   cache-dir    — directory (an actions/cache path) holding images.tar,
#                  images.txt + manifest.yaml; created on a cache miss
#
# Environment:
#   PULL_RETRY_ATTEMPTS — forwarded to pull-image-with-retry.sh (default: 3)
#   PULL_RETRY_DELAY    — forwarded to pull-image-with-retry.sh (default: 15)
#   MANIFEST_FETCH_ATTEMPTS  — max manifest fetch attempts           (default: 6)
#   MANIFEST_FETCH_DELAY     — base seconds, doubled after each sleep (default: 5)
#   MANIFEST_FETCH_MAX_DELAY — cap on the doubled delay, before jitter
#                                                                   (default: 60)

set -euo pipefail
shopt -s inherit_errexit

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
saved_manifest="${dir}/manifest.yaml"

# Cache hit: the tar and its extracted list are both present, so load and return
# without touching the network.
if [[ -f "${tar}" && -f "${list}" ]]; then
  echo "==> loading ${name} images from cache"
  docker load -i "${tar}"
  exit 0
fi

# Cache miss: fetch the pinned manifest and extract the image refs it names.
#
# The fetch retries on the exponential jittered schedule download-verified.sh
# uses, not curl's own --retry. A flat `--retry 5 --retry-delay 2` spent its
# whole budget in 12.7s of release-CDN 500s and killed the e2e lane before the
# cluster existed (Q1133); this schedule spans 135-202s of backoff.
manifest="$(mktemp)"
trap 'rm -f "${manifest}"' EXIT

attempts="${MANIFEST_FETCH_ATTEMPTS:-6}"
delay="${MANIFEST_FETCH_DELAY:-5}"
max_delay="${MANIFEST_FETCH_MAX_DELAY:-60}"
backoff="${delay}"
rc=0
for (( attempt = 1; attempt <= attempts; attempt++ )); do
  rc=0
  curl -fsSL -o "${manifest}" "${url}" || rc=$?
  if (( rc == 0 )); then
    break
  fi
  if (( attempt < attempts )); then
    # Jitter up to half the delay, so the two e2e lanes failing in the same
    # second do not retry in the same second.
    sleep_for="${backoff}"
    if (( sleep_for > 0 )); then
      sleep_for=$(( sleep_for + RANDOM % (sleep_for / 2 + 1) ))
    fi
    echo "fetch of ${name} manifest failed with curl exit ${rc} (attempt ${attempt}/${attempts}); retrying in ${sleep_for}s" >&2
    sleep "${sleep_for}"
    backoff=$(( backoff * 2 ))
    if (( backoff > max_delay )); then
      backoff="${max_delay}"
    fi
  fi
done
if (( rc != 0 )); then
  echo "failed to fetch ${name} manifest from ${url} after ${attempts} attempts" >&2
  exit "${rc}"
fi

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
cp "${manifest}" "${saved_manifest}"
