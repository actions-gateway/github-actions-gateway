#!/usr/bin/env bash
#
# Verify the keyless cosign signatures of a published release: the six images
# (gmc, agc, proxy, worker, wrapper, build-runner) at the version tag plus both
# Helm chart OCI artifacts. Backs `make verify-release VERSION=vX.Y.Z`; the
# identity/issuer constraints match what .github/workflows/publish.yml signs
# with. See docs/operations/release.md.
#
# Usage: scripts/release/verify-release.sh vX.Y.Z
#
# Env:
#   COSIGN  Path to the cosign binary (default .build/cosign at the repo root —
#           download the pinned version with `make cosign`).
#   COSIGN_TIMEOUT  Seconds one signature check may take (default 180). cosign's
#           own --timeout does not bound the registry credential lookup: a hung
#           Docker credential helper stalled v2.5.2 for over 10 minutes with no
#           output (2026-10-26), past its 3m default.
set -euo pipefail
shopt -s inherit_errexit

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
COSIGN_TIMEOUT="${COSIGN_TIMEOUT:-180}"
timed_out=0

# verify REF — print OK/FAIL/TIMEOUT for one signed artifact; returns non-zero
# unless cosign verified it within COSIGN_TIMEOUT seconds.
verify() {
	local ref="$1" pid waited=0
	"$COSIGN" verify --certificate-identity-regexp "$id_re" \
		--certificate-oidc-issuer "$issuer" "$ref" >/dev/null 2>&1 &
	pid=$!
	while kill -0 "$pid" 2>/dev/null; do
		if ((waited >= COSIGN_TIMEOUT)); then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
			echo TIMEOUT
			timed_out=1
			return 1
		fi
		sleep 1
		waited=$((waited + 1))
	done
	if wait "$pid"; then
		echo OK
	else
		echo FAIL
		return 1
	fi
}

rc=0
for img in gmc agc proxy worker wrapper build-runner; do
	printf '==> %-12s %s ... ' "$img" "$VERSION"
	verify "$repo/$img:$VERSION" || rc=1
done
printf '==> %-12s %s ... ' "chart" "$chart_ver"
verify "$repo/charts/actions-gateway:$chart_ver" || rc=1
printf '==> %-12s %s ... ' "crds-v2" "$chart_ver"
verify "$repo/charts/actions-gateway-crds-v2:$chart_ver" || rc=1

if [[ "$rc" -ne 0 ]]; then
	if ((timed_out)); then
		echo "a signature check exceeded ${COSIGN_TIMEOUT}s; a hung Docker credential helper does this, so retry with DOCKER_CONFIG=\$(mktemp -d)" >&2
	fi
	echo "signature verification FAILED (if local docker creds are misconfigured, retry with DOCKER_CONFIG=\$(mktemp -d))" >&2
	exit 1
fi
echo "all signatures verified for $VERSION"
