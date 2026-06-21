#!/usr/bin/env bash
#
# Verify the keyless cosign signatures of a published release: the four images
# (gmc, agc, proxy, worker) at the version tag plus the Helm chart OCI
# artifact. Backs `make verify-release VERSION=vX.Y.Z`; the identity/issuer
# constraints match what .github/workflows/publish.yml signs with. See
# docs/operations/release.md.
#
# Usage: scripts/verify-release.sh vX.Y.Z
#
# Env:
#   COSIGN  Path to the cosign binary (default .build/cosign at the repo root —
#           download the pinned version with `make cosign`).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
	echo "usage: $0 vX.Y.Z   (or: make verify-release VERSION=vX.Y.Z)" >&2
	exit 2
fi

COSIGN="${COSIGN:-$REPO_ROOT/.build/cosign}"
if [[ ! -x "$COSIGN" ]]; then
	echo "cosign not found at $COSIGN — download it with: make cosign" >&2
	exit 1
fi

repo="ghcr.io/actions-gateway"
# Tags-only signing identity (Q124). The pattern lives in common.sh so
# verify-release-test.sh can assert the tags-only property holds; publish.yml
# refuses to run from a non-tag ref as the first defense, this is the second.
id_re="$(release_identity_regexp)"
issuer='https://token.actions.githubusercontent.com'
chart_ver="${VERSION#v}"

# verify REF — print OK/FAIL for one signed artifact; returns cosign's status.
verify() {
	local ref="$1"
	if "$COSIGN" verify --certificate-identity-regexp "$id_re" \
		--certificate-oidc-issuer "$issuer" "$ref" >/dev/null 2>&1; then
		echo OK
	else
		echo FAIL
		return 1
	fi
}

rc=0
for img in gmc agc proxy worker; do
	printf '==> %-7s %s ... ' "$img" "$VERSION"
	verify "$repo/$img:$VERSION" || rc=1
done
printf '==> %-7s %s ... ' "chart" "$chart_ver"
verify "$repo/charts/actions-gateway:$chart_ver" || rc=1
printf '==> %-7s %s ... ' "crds-v2" "$chart_ver"
verify "$repo/charts/actions-gateway-crds-v2:$chart_ver" || rc=1

if [[ "$rc" -ne 0 ]]; then
	echo "signature verification FAILED (if local docker creds are misconfigured, retry with DOCKER_CONFIG=\$(mktemp -d))" >&2
	exit 1
fi
echo "all signatures verified for $VERSION"
