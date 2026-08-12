#!/usr/bin/env bash
# Print the upstream SHA-256 of the syft linux/amd64 release tarball for a given
# syft version tag, with no trailing newline.
#
# Used by updatecli (updatecli.d/syft.yaml) as a shell source so SYFT_SHA256 in
# .github/workflows/publish.yml and .github/workflows/security-scan.yml stays in
# lockstep with SYFT_VERSION: updatecli resolves the latest syft tag, then calls
# this with that tag to pull the matching hash from the release's published
# checksums file (the same file a human would copy by hand). This
# version+checksum pair is exactly what Dependabot cannot track, because the pins
# are workflow env vars rather than a package-manifest entry — and syft is a
# runtime tool download, so nothing else bumps it either (Q806).
#
# syft embeds the version WITHOUT the leading "v" in both the asset names and the
# checksums filename (tag v1.45.1 -> syft_1.45.1_linux_amd64.tar.gz, listed in
# syft_1.45.1_checksums.txt), so the tag's "v" is stripped for both.
#
# Usage: syft-linux-amd64-sha256.sh <syft-version-tag>   # e.g. v1.45.1
set -euo pipefail
shopt -s inherit_errexit

main() {
  local version="${1:?usage: syft-linux-amd64-sha256.sh <syft-version-tag>}"
  local bare="${version#v}"
  local asset="syft_${bare}_linux_amd64.tar.gz"
  local url="https://github.com/anchore/syft/releases/download/${version}/syft_${bare}_checksums.txt"
  # The checksums file is one "<sha256>  <asset>" line per asset. Emit only the
  # hash field for our asset, with no trailing newline, so updatecli uses it
  # verbatim as the source value. --retry tolerates a transient releases-CDN
  # gateway error.
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "${url}" \
    | awk -v asset="${asset}" '$2 == asset { printf "%s", $1 }'
}

main "$@"
