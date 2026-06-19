#!/usr/bin/env bash
# Print the upstream SHA-256 of the kind linux/amd64 release binary for a given
# kind version tag, with no trailing newline.
#
# Used by updatecli (updatecli.d/kind.yaml) as a shell source so KIND_BINARY_SHA256
# in .github/workflows/e2e-reusable.yml stays in lockstep with KIND_VERSION:
# updatecli resolves the latest kind tag, then calls this with that tag to fetch
# the matching checksum from the release's published kind-linux-amd64.sha256sum
# asset — the same file a human would copy by hand. This version+checksum pair is
# exactly what Dependabot cannot track, because the pins are workflow env vars
# rather than a package-manifest entry.
#
# Usage: kind-linux-amd64-sha256.sh <kind-version-tag>   # e.g. v0.32.0
set -euo pipefail

main() {
  local version="${1:?usage: kind-linux-amd64-sha256.sh <kind-version-tag>}"
  local url="https://github.com/kubernetes-sigs/kind/releases/download/${version}/kind-linux-amd64.sha256sum"
  # The asset is a single line: "<sha256>  kind-linux-amd64". Emit only the hash
  # field, with no trailing newline, so updatecli uses it verbatim as the source
  # value. --retry tolerates a transient releases-CDN gateway error.
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "${url}" \
    | awk '{ printf "%s", $1 }'
}

main "$@"
