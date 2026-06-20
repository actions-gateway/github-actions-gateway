#!/usr/bin/env bash
# Print the upstream SHA-256 of the polaris linux/amd64 release tarball for a
# given polaris version tag, with no trailing newline.
#
# Used by updatecli (updatecli.d/polaris.yaml) as a shell source so POLARIS_SHA256
# in .github/workflows/security-scan.yml stays in lockstep with POLARIS_VERSION:
# updatecli resolves the latest polaris tag, then calls this with that tag to pull
# the matching hash from the release's published checksums.txt (the same file a
# human would copy by hand). This version+checksum pair is exactly what Dependabot
# cannot track, because the pins are workflow env vars rather than a
# package-manifest entry.
#
# polaris embeds the version WITHOUT the leading "v" in its v10+ asset names
# (tag v10.2.0 -> polaris_10.2.0_linux_amd64.tar.gz), so the tag's "v" is stripped
# to build the asset filename matched in checksums.txt.
#
# Usage: polaris-linux-amd64-sha256.sh <polaris-version-tag>   # e.g. v10.2.0
set -euo pipefail

main() {
  local version="${1:?usage: polaris-linux-amd64-sha256.sh <polaris-version-tag>}"
  local asset="polaris_${version#v}_linux_amd64.tar.gz"
  local url="https://github.com/FairwindsOps/polaris/releases/download/${version}/checksums.txt"
  # checksums.txt is one "<sha256>  <asset>" line per asset. Emit only the hash
  # field for our asset, with no trailing newline, so updatecli uses it verbatim
  # as the source value. --retry tolerates a transient releases-CDN gateway error.
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "${url}" \
    | awk -v asset="${asset}" '$2 == asset { printf "%s", $1 }'
}

main "$@"
