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
# Backoff is EXPONENTIAL WITH JITTER, not fixed (Q460). The security-scan trivy
# job pre-pulls the same buildkit image from seven matrix shards at once, so a Hub
# brown-out fails all seven inside the same second. The former fixed 5s delay was
# wrong on both counts: it retried the six shards in lockstep — six synchronised
# requests per round against an IP-shared anonymous rate limit, which is itself
# what the limit responds to — and it exhausted the whole budget in ~95s, shorter
# than the brown-out that caused it. Doubling the delay per attempt de-correlates
# nothing on its own, so each sleep also carries up to 50% jitter; together they
# spread concurrent callers out and stretch the budget from ~20s of backoff to
# 135-202s. The growth is capped (PULL_RETRY_MAX_DELAY) and the attempt count is
# finite, so a registry that is genuinely down still fails clearly and on a
# bounded schedule — worst case ~5 minutes with the defaults, counting the
# attempts themselves, well inside every caller's job timeout — never hanging.
#
# THIS IS ALSO THE REGISTRY-MIRROR CHOKEPOINT (Q408 Phase 3). Every docker pull
# the e2e job makes goes through here, so pointing the job at the in-cluster
# pull-through cache is one rewrite here rather than one per call site. When
# REGISTRY_MIRRORS maps the ref's registry (see scripts/lib/registry-mirror.sh),
# the pull is made against the mirror and the image is then re-tagged to the ref
# the caller asked for and the mirror tag dropped — so the daemon is left in
# exactly the state a direct pull would have left it in, and every downstream
# `docker tag` / `docker save` / `kind load` of the original ref is unaffected.
# With no map configured nothing is rewritten, which is what the hosted lane,
# publish.yml and a developer's `make e2e` all see.
#
# Usage:
#   scripts/fetch/pull-image-with-retry.sh <image-ref>
#
# Environment:
#   PULL_RETRY_ATTEMPTS   — max pull attempts                       (default: 6)
#   PULL_RETRY_DELAY      — base seconds, doubled after each sleep  (default: 5)
#   PULL_RETRY_MAX_DELAY  — cap on the doubled delay, before jitter (default: 60)
#   REGISTRY_MIRRORS      — <upstream>=<mirror> map; unset means pull direct

set -euo pipefail
shopt -s inherit_errexit

image="${1:-}"
if [[ -z "${image}" ]]; then
  echo "usage: $0 <image-ref>" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/registry-mirror.sh
source "${REPO_ROOT}/scripts/lib/registry-mirror.sh"

# What is actually pulled. Identical to ${image} unless a mirror applies.
pull_ref="$(mirror_rewrite "${image}")"
if [[ "${pull_ref}" != "${image}" ]]; then
  echo "pulling ${image} via the registry mirror at ${pull_ref%%/*}" >&2
fi

attempts="${PULL_RETRY_ATTEMPTS:-6}"
delay="${PULL_RETRY_DELAY:-5}"
max_delay="${PULL_RETRY_MAX_DELAY:-60}"

# Doubled after every sleep and clamped to max_delay, so the schedule with the
# defaults is 5, 10, 20, 40, 60 seconds before jitter.
backoff="${delay}"

for (( attempt = 1; attempt <= attempts; attempt++ )); do
  if docker pull "${pull_ref}"; then
    if [[ "${pull_ref}" != "${image}" ]]; then
      # Restore the caller's own reference and remove the mirror's, so nothing
      # downstream has to know a mirror was involved.
      docker tag "${pull_ref}" "${image}"
      docker rmi "${pull_ref}" >/dev/null
    fi
    exit 0
  fi
  if (( attempt < attempts )); then
    # Jitter up to half the delay. Callers that failed in the same second must
    # not retry in the same second; without this the shards stay in lockstep for
    # the whole budget and every round lands as one burst on the registry.
    sleep_for="${backoff}"
    if (( sleep_for > 0 )); then
      sleep_for=$(( sleep_for + RANDOM % (sleep_for / 2 + 1) ))
    fi
    echo "pull of ${pull_ref} failed (attempt ${attempt}/${attempts}); retrying in ${sleep_for}s" >&2
    sleep "${sleep_for}"
    backoff=$(( backoff * 2 ))
    if (( backoff > max_delay )); then
      backoff="${max_delay}"
    fi
  fi
done

echo "failed to pull ${pull_ref} after ${attempts} attempts" >&2
exit 1
