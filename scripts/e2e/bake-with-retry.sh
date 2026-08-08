#!/usr/bin/env bash
# bake-with-retry.sh — run `docker buildx bake` with bounded retries.
#
# The e2e image build bakes six images and pushes them to the kind local
# registry (127.0.0.1:5000) as it goes. That registry occasionally resets the
# connection mid-push under the concurrent load of six parallel pushes, failing
# the whole bake with a "connection reset by peer" / "unexpected EOF" and
# needing a manual job re-run (Q256, seen on the e2e-calico lane). buildx has no
# push retry of its own, so wrap the bake in a bounded retry loop: the build
# layers are already in the buildkit cache after the first attempt, so a retry
# only re-pushes and is cheap. This is the push-side analogue of
# pull-image-with-retry.sh.
#
# Usage:
#   scripts/e2e/bake-with-retry.sh [docker buildx bake args...]
#
# Environment:
#   BAKE_RETRY_ATTEMPTS  — max bake attempts        (default: 3)
#   BAKE_RETRY_DELAY     — seconds between attempts  (default: 10)

set -euo pipefail
shopt -s inherit_errexit

attempts="${BAKE_RETRY_ATTEMPTS:-3}"
delay="${BAKE_RETRY_DELAY:-10}"

for (( attempt = 1; attempt <= attempts; attempt++ )); do
  if docker buildx bake "$@"; then
    exit 0
  fi
  if (( attempt < attempts )); then
    echo "docker buildx bake failed (attempt ${attempt}/${attempts}); retrying in ${delay}s" >&2
    sleep "${delay}"
  fi
done

echo "docker buildx bake failed after ${attempts} attempts" >&2
exit 1
