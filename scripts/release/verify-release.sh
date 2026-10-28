#!/usr/bin/env bash
#
# Verify the keyless cosign signatures of a published release: the six images
# (gmc, agc, proxy, worker, wrapper, build-runner) at the version tag, the SPDX
# SBOM attestation on each image's per-arch manifests (amd64 and arm64 both
# required), both Helm chart OCI
# artifacts, and every `*.cosign.bundle` blob signature on the GitHub Release.
# Backs `make verify-release VERSION=vX.Y.Z` and `make verify-release-compat`;
# the identity/issuer constraints match what .github/workflows/publish.yml signs
# with. See docs/operations/release.md.
#
# Needs docker (buildx imagetools, to resolve per-arch digests), gh and jq.
#
# Usage: scripts/release/verify-release.sh vX.Y.Z
#
# Env:
#   COSIGN  Path to the cosign binary (default .build/cosign at the repo root —
#           download the pinned version with `make cosign`).
#   COSIGN_TIMEOUT  Seconds one signature check or index lookup may take
#           (default 180). cosign's own --timeout does not bound the registry
#           credential lookup: a hung Docker credential helper stalled v2.5.2
#           for over 10 minutes with no output (2026-10-26), past its 3m
#           default, and hangs `docker buildx imagetools inspect` the same way.
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
RELEASE_REPO="${RELEASE_REPO:-actions-gateway/github-actions-gateway}"
# Tags-only signing identity (Q124). The pattern lives in common.sh so
# verify-release-test.sh can assert the tags-only property holds; publish.yml
# refuses to run from a non-tag ref as the first defense, this is the second.
id_re="$(release_identity_regexp)"
issuer='https://token.actions.githubusercontent.com'
chart_ver="${VERSION#v}"
COSIGN_TIMEOUT="${COSIGN_TIMEOUT:-180}"
timed_out=0

# bounded OUT CMD... — run CMD with stdout to OUT, killing it after
# COSIGN_TIMEOUT seconds; returns CMD's status, or 124 if it was killed. A file
# rather than $(...): a credential helper orphaned by the kill would hold a
# command substitution's pipe open.
bounded() {
	local out="$1" pid waited=0
	shift
	"$@" >"$out" 2>/dev/null &
	pid=$!
	# Polled in tenths of a second, so a fast check is not billed a whole one.
	while kill -0 "$pid" 2>/dev/null; do
		if ((waited >= COSIGN_TIMEOUT * 10)); then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
			timed_out=1
			return 124
		fi
		sleep 0.1
		waited=$((waited + 1))
	done
	wait "$pid"
}

# check SUBCOMMAND ARGS... — run one cosign verification with the identity and
# issuer constraints and print OK/FAIL/TIMEOUT; returns non-zero unless cosign
# verified it within COSIGN_TIMEOUT seconds.
check() {
	local sub="$1" status=0
	shift
	bounded /dev/null "$COSIGN" "$sub" --certificate-identity-regexp "$id_re" \
		--certificate-oidc-issuer "$issuer" "$@" || status=$?
	case "$status" in
		0) echo OK ;;
		124) echo TIMEOUT; return 1 ;;
		*) echo FAIL; return 1 ;;
	esac
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

rc=0
for img in gmc agc proxy worker wrapper build-runner; do
	printf '==> %-28s %s ... ' "$img" "$VERSION"
	check verify "$repo/$img:$VERSION" || rc=1
	# SBOMs are attested per arch (publish.yml), so the index carries none. The
	# index lookup calls the same credential helper cosign does, so it gets the
	# same deadline. Unknown-platform entries are attestation manifests.
	lookup=0
	bounded "$work/index.json" docker buildx imagetools inspect "$repo/$img:$VERSION" --raw || lookup=$?
	if ((lookup == 124)); then
		printf '==> %-28s %s ... TIMEOUT (resolving per-arch manifests)\n' "$img sbom" "$VERSION"
		rc=1
		continue
	fi
	arches=""
	if ((lookup == 0)); then
		arches="$(jq -r '.manifests[] | select(.platform.os != "unknown") | "\(.platform.architecture) \(.digest)"' \
			"$work/index.json" 2>/dev/null)" || arches=""
	fi
	if [[ -z "$arches" ]]; then
		printf '==> %-28s %s ... FAIL (no per-arch manifests resolved)\n' "$img sbom" "$VERSION"
		rc=1
		continue
	fi
	for want in amd64 arm64; do
		if [[ $'\n'"$arches" != *$'\n'"$want "* ]]; then
			printf '==> %-28s %s ... FAIL (index has no linux/%s manifest)\n' "$img sbom $want" "$VERSION" "$want"
			rc=1
		fi
	done
	while read -r arch digest; do
		printf '==> %-28s %s ... ' "$img sbom $arch" "$VERSION"
		check verify-attestation --type spdxjson "$repo/$img@$digest" || rc=1
	done <<< "$arches"
done
printf '==> %-28s %s ... ' "chart" "$chart_ver"
check verify "$repo/charts/actions-gateway:$chart_ver" || rc=1
printf '==> %-28s %s ... ' "crds-v2 chart" "$chart_ver"
check verify "$repo/charts/actions-gateway-crds-v2:$chart_ver" || rc=1

# Every blob signature on the Release: the v2 CRD manifest always, and the
# gag-migrate SHA256SUMS while that asset ships. Each bundle signs the asset
# named by dropping its .cosign.bundle suffix.
blob_dir="$work/blobs"
mkdir -p "$blob_dir"
bundles=()
if ! gh release view "$VERSION" --repo "$RELEASE_REPO" --json assets \
	--jq '.assets[].name | select(endswith(".cosign.bundle"))' >"$work/bundles" 2>"$work/gh.err"; then
	printf '==> %-28s %s ... FAIL (gh release view: %s)\n' "release assets" "$VERSION" "$(head -n 1 "$work/gh.err")"
	rc=1
else
	mapfile -t bundles <"$work/bundles"
	if [[ " ${bundles[*]} " != *" actions-gateway-crds-v2.yaml.cosign.bundle "* ]]; then
		printf '==> %-28s %s ... FAIL (no actions-gateway-crds-v2.yaml.cosign.bundle on the release)\n' "crds-v2 manifest" "$VERSION"
		rc=1
	fi
fi
for bundle in "${bundles[@]}"; do
	subject="${bundle%.cosign.bundle}"
	printf '==> %-28s %s ... ' "$subject" "$VERSION"
	if ! gh release download "$VERSION" --repo "$RELEASE_REPO" --dir "$blob_dir" \
		--pattern "$bundle" --pattern "$subject" --clobber >/dev/null 2>&1; then
		echo "FAIL (download)"
		rc=1
		continue
	fi
	check verify-blob --bundle "$blob_dir/$bundle" "$blob_dir/$subject" || rc=1
done

if [[ "$rc" -ne 0 ]]; then
	if ((timed_out)); then
		echo "a registry lookup or signature check exceeded ${COSIGN_TIMEOUT}s; a hung Docker credential helper does this, so retry with DOCKER_CONFIG=\$(mktemp -d)" >&2
	fi
	echo "signature verification FAILED (if local docker creds are misconfigured, retry with DOCKER_CONFIG=\$(mktemp -d))" >&2
	exit 1
fi
echo "all signatures verified for $VERSION"
