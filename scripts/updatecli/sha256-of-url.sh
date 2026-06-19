#!/usr/bin/env bash
# Print the SHA-256 of the file at a URL, lowercase hex, with no trailing
# newline.
#
# Used by updatecli manifests as a shell source to keep a "<TOOL>_SHA256" pin in
# lockstep with its version when the upstream publishes no checksum file, so the
# bytes must be hashed directly (e.g. shellcheck). For upstreams that DO publish
# a per-asset checksum, fetch that file instead (e.g. kind-linux-amd64-sha256.sh)
# — it is cheaper and matches the value a human would copy by hand.
#
# Usage: sha256-of-url.sh <url>
set -euo pipefail

# Script-scope so the EXIT trap can see it: a var local to main() is out of scope
# by the time the trap fires, which under `set -u` would abort with a non-zero
# exit that updatecli reads as a failed source.
tmp=""
cleanup() { [[ -n "${tmp}" ]] && rm -f "${tmp}"; }
trap cleanup EXIT

main() {
  local url="${1:?usage: sha256-of-url.sh <url>}"
  tmp="$(mktemp)"
  # --retry tolerates a transient releases-CDN gateway error; the hash is the
  # integrity check, so a corrupt download surfaces as a changed value.
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o "${tmp}" "${url}"
  # Prefer sha256sum (Linux/CI); fall back to shasum -a 256 (macOS/local diff).
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${tmp}" | awk '{ printf "%s", $1 }'
  else
    shasum -a 256 "${tmp}" | awk '{ printf "%s", $1 }'
  fi
}

main "$@"
