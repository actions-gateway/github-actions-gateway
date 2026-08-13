#!/usr/bin/env bash
#
# download-verified.sh — download a pinned release asset with bounded retries
# and verify it against an expected SHA256 before it lands at the output path.
#
# Every CI step that installs a pinned third-party binary (kind, shellcheck,
# kubeconform, polaris, syft) and the local `$(COSIGN)` rule need the same two
# properties:
#
#   1. A transient GitHub releases-CDN denial must be retried in-step, on a
#      schedule long enough to outlast the denial. curl's own retry misses on
#      both counts. `--retry` covers 408/429/5xx and connection failures only,
#      so a 403 — the denial the CDN actually serves under load — failed the
#      download instantly (observed on PR #828, run 30207434700: `curl: (22)
#      ... 403` after 0.08s, which reddened the whole workflow via
#      `security-scan-gate`, Q433). And `--retry-delay` pins the wait flat, so
#      the whole budget burned in 10.3s against a syft release 503 (#1440,
#      Q829). The loop below retries any nonzero curl exit, on the exponential
#      jittered schedule pull-image-with-retry.sh already uses (Q460): the syft
#      and kind installs run from concurrent matrix shards, so a brown-out
#      denies them in the same second and a fixed delay retries them in
#      lockstep against the limit that is denying them.
#   2. The bytes must be checked against a pinned digest. GitHub release assets
#      are mutable for an existing tag, so a raw download is not trustworthy on
#      its own (Q126/Q127).
#
# Both live here so the retry can never drift away from the verification: the
# digest is a required argument, the download goes to a temp file, and the
# output path is only written after the digest matches. There is no flag or
# environment variable that skips the check.
#
# Sibling of retry.sh (any command) and pull-image-with-retry.sh (docker pull);
# this one is the `curl` + integrity-check case.
#
# Usage:
#   scripts/fetch/download-verified.sh <url> <sha256> <output-path>
#   scripts/fetch/download-verified.sh "$url" "$KUBECONFORM_SHA256" "$RUNNER_TEMP/kubeconform.tar.gz"
#
# Environment:
#   DOWNLOAD_RETRIES          — retries after the first attempt      (default: 5)
#   DOWNLOAD_RETRY_DELAY      — base seconds, doubled after each sleep
#                                                                   (default: 5)
#   DOWNLOAD_RETRY_MAX_DELAY  — cap on the doubled delay, before jitter
#                                                                  (default: 60)

set -euo pipefail
shopt -s inherit_errexit

url="${1:-}"
want="${2:-}"
out="${3:-}"
if [[ -z "$url" || -z "$want" || -z "$out" ]]; then
	echo "usage: $0 <url> <sha256> <output-path>" >&2
	exit 2
fi

# Reject anything that is not a full SHA256 rather than letting a truncated or
# placeholder digest through to a comparison it would trivially fail (or, if it
# were ever compared as a prefix, trivially pass).
if [[ ! "$want" =~ ^[0-9a-fA-F]{64}$ ]]; then
	echo "expected a 64-hex-character SHA256, got: $want" >&2
	exit 2
fi

retries="${DOWNLOAD_RETRIES:-5}"
attempts=$((retries + 1))
delay="${DOWNLOAD_RETRY_DELAY:-5}"
max_delay="${DOWNLOAD_RETRY_MAX_DELAY:-60}"

# Portable SHA256: coreutils sha256sum on Linux/CI, shasum -a 256 on macOS.
sha256_of() {
	local f="$1"
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$f" | awk '{print $1}'
	else
		shasum -a 256 "$f" | awk '{print $1}'
	fi
}

# Download to a temp file and verify before moving into place, so a failed
# integrity check never leaves unverified bytes at the output path.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "downloading $url"

# Doubled after every sleep and clamped to max_delay, so the schedule with the
# defaults is 5, 10, 20, 40, 60 seconds before jitter.
backoff="$delay"
rc=0
for ((attempt = 1; attempt <= attempts; attempt++)); do
	rc=0
	curl -fsSL -o "$tmp" "$url" || rc=$?
	if ((rc == 0)); then
		break
	fi
	if ((attempt < attempts)); then
		# Jitter up to half the delay. Shards that were denied in the same second
		# must not come back in the same second, or every round lands as one burst.
		sleep_for="$backoff"
		if ((sleep_for > 0)); then
			sleep_for=$((sleep_for + RANDOM % (sleep_for / 2 + 1)))
		fi
		echo "download of $url failed with curl exit $rc (attempt $attempt/$attempts); retrying in ${sleep_for}s" >&2
		sleep "$sleep_for"
		backoff=$((backoff * 2))
		if ((backoff > max_delay)); then
			backoff="$max_delay"
		fi
	fi
done
if ((rc != 0)); then
	echo "failed to download $url after $attempts attempts" >&2
	exit "$rc"
fi

# Hex digests are case-insensitive; compare in one case so an upper-case pin
# cannot read as a mismatch.
want="$(printf '%s' "$want" | tr '[:upper:]' '[:lower:]')"
got="$(sha256_of "$tmp")"
if [[ "$got" != "$want" ]]; then
	echo "SHA256 mismatch for $url:" >&2
	echo "  expected $want" >&2
	echo "  got      $got" >&2
	echo "refusing to write unverified bytes to $out" >&2
	exit 1
fi

out_dir="$(dirname "$out")"
[[ -d "$out_dir" ]] || mkdir -p "$out_dir"
mv "$tmp" "$out"
trap - EXIT
echo "verified $out against pinned SHA256 $want"
