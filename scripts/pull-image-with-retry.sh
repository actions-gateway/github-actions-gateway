#!/usr/bin/env bash
# pull-image-with-retry.sh — `docker pull` an image with bounded retries.
#
# `docker pull` has no built-in retry, so a transient registry hiccup — a Docker
# Hub timeout, a 5xx, or an anonymous rate-limit (HTTP 429) — otherwise fails the
# whole CI step and needs a manual job re-run. Wrap the pull in a retry loop so a
# transient failure is absorbed in-step. Used by the e2e and security-scan
# workflows to pre-pull the buildkit builder image and mirror the curl test
# image; equivalent to `curl --retry` for the steps that go through `docker pull`.
#
# Usage:
#   scripts/pull-image-with-retry.sh <image-ref>
#
# Environment:
#   PULL_RETRY_ATTEMPTS  — max pull attempts        (default: 5)
#   PULL_RETRY_DELAY     — seconds between attempts  (default: 5)

set -euo pipefail

image="${1:-}"
if [[ -z "${image}" ]]; then
  echo "usage: $0 <image-ref>" >&2
  exit 2
fi

attempts="${PULL_RETRY_ATTEMPTS:-5}"
delay="${PULL_RETRY_DELAY:-5}"

for (( attempt = 1; attempt <= attempts; attempt++ )); do
  if docker pull "${image}"; then
    exit 0
  fi
  if (( attempt < attempts )); then
    echo "pull of ${image} failed (attempt ${attempt}/${attempts}); retrying in ${delay}s" >&2
    sleep "${delay}"
  fi
done

echo "failed to pull ${image} after ${attempts} attempts" >&2
exit 1
