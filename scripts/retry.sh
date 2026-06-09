#!/usr/bin/env bash
# retry.sh — run an arbitrary command with bounded retries and linear backoff.
#
# Several release steps push to GHCR (cosign sign/attest, helm push) and
# occasionally hit a transient registry error — an HTTP/2 stream INTERNAL_ERROR,
# a 5xx, or a rate-limit — that would otherwise fail the whole publish and need a
# manual job re-run. These operations are idempotent, so wrap them in a retry
# loop and a transient failure is absorbed in-step. Sibling of
# pull-image-with-retry.sh (which is docker-pull-specific); this one retries any
# command passed as arguments.
#
# Usage:
#   scripts/retry.sh <command> [args...]
#   scripts/retry.sh cosign sign --yes "$IMAGE"
#
# Environment:
#   RETRY_ATTEMPTS  — max attempts                            (default: 5)
#   RETRY_DELAY     — base seconds, scaled by attempt number  (default: 5)

set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

attempts="${RETRY_ATTEMPTS:-5}"
delay="${RETRY_DELAY:-5}"

for (( attempt = 1; attempt <= attempts; attempt++ )); do
  if "$@"; then
    exit 0
  fi
  if (( attempt < attempts )); then
    echo "command failed (attempt ${attempt}/${attempts}); retrying in $(( delay * attempt ))s: $*" >&2
    sleep "$(( delay * attempt ))"
  fi
done

echo "command failed after ${attempts} attempts: $*" >&2
exit 1
