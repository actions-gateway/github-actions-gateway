#!/usr/bin/env bash
# Print the latest STABLE envtest Kubernetes version as "1.<minor>.x", with no
# trailing newline.
#
# Used by updatecli (updatecli.d/envtest.yaml) as a shell source so the envtest
# version the integration-test tier runs against — ENVTEST_K8S_VERSION in the two
# controller Makefiles and integration-test.yml — stays current. The version is
# resolved from controller-tools' envtest-releases.yaml index (the same index
# setup-envtest reads), so the chosen version provably has published binaries:
# auto-bumping to a Kubernetes minor with no envtest release would break the tier.
# Pre-releases (-alpha/-beta) are excluded; the ".x" patch wildcard lets
# setup-envtest resolve the newest patch.
#
# This is the envtest twin of KIND_NODE_IMAGE, which is bumped by hand because the
# kind node image is published in release notes, not a clean datasource. envtest
# *does* have a clean datasource, so it is auto-bumped — but, like a kind bump, the
# PR is not auto-merged: changing the tested Kubernetes version is reviewed (and
# kept compatible with the e2e node image) before merge.
#
# Usage: latest-envtest-version.sh
set -euo pipefail

main() {
  local url="https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/main/envtest-releases.yaml"
  # Index keys are release tags, one per line: stable "  v1.36.0:" and
  # pre-release "  v1.35.0-alpha.3:". The trailing ":" in the grep pattern anchors
  # to a bare X.Y.Z tag, so pre-releases (which have "-alpha"/"-beta" before the
  # colon) are excluded. sort -V picks the highest; awk renders "1.<minor>.x".
  # --retry tolerates a transient raw.githubusercontent CDN error.
  local latest
  latest="$(curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "${url}" \
    | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+:' \
    | tr -d 'v:' \
    | sort -V \
    | tail -1)"
  if [[ -z "${latest}" ]]; then
    echo "no stable envtest release found in ${url}" >&2
    return 1
  fi
  printf '%s' "${latest}" | awk -F. '{ printf "%s.%s.x", $1, $2 }'
}

main "$@"
