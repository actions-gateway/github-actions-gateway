#!/usr/bin/env bash
#
# Print the SHA-256 of one cosign release binary, read from the release's
# *signed* checksums file, with no trailing newline.
#
# Used by updatecli (updatecli.d/cosign.yaml) as a shell source, once per
# platform, so the four per-platform digests in
# scripts/release/download-cosign.sh move with COSIGN_VERSION instead of by hand
# (Q927). cosign is the one pinned tool nothing else bumps: Dependabot cannot see
# a Makefile variable or a shell `case` block, and Q903's cosign-pin-check holds
# the copies to each other rather than to upstream, so three sites agreeing on a
# stale version still agree.
#
# The signature check is the point, not a formality. cosign verifies every
# release this repo publishes, and its digests are pinned because GitHub release
# assets stay mutable for an existing tag (Q126/Q127) — so a resolver that hashes
# whatever the CDN serves, or copies an unverified cosign_checksums.txt, pins
# exactly the bytes the pin exists to distrust, and does it weekly and
# unattended. sigstore signs cosign_checksums.txt keylessly, and neither that
# signature nor its transparency-log entry is the release author's to rewrite.
# So: verify first, read a digest out of it second, print nothing if it does not
# verify.
#
# The verifier is the *currently pinned* cosign ($(COSIGN), from `make cosign`),
# which download-cosign.sh already checked against the in-repo digest table. The
# trust chain is therefore anchored in the pin being replaced, not in the release
# being fetched.
#
# sigstore signs its releases under a Google service-account identity rather than
# a GitHub Actions workflow one (measured on v2.5.2, 2026-08-19). Both are pinned
# below, so a change of release identity fails this closed and reaches a human
# instead of silently widening what counts as cosign.
#
# Usage: cosign-release-sha256.sh <version-tag> <os-arch>   # e.g. v2.5.3 darwin-arm64
#
# Env:
#   COSIGN  Path to the verifying cosign (default .build/cosign at the repo root)
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"

SIGNER_IDENTITY='keyless@projectsigstore.iam.gserviceaccount.com'
SIGNER_ISSUER='https://accounts.google.com'

# File-scope so the cleanup trap can still see it once main returns.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

main() {
	local version="${1:?usage: cosign-release-sha256.sh <version-tag> <os-arch>}"
	local platform="${2:?usage: cosign-release-sha256.sh <version-tag> <os-arch>}"

	local cosign="${COSIGN:-$REPO_ROOT/.build/cosign}"
	if [[ ! -x "$cosign" ]]; then
		echo "cosign not found at $cosign — download the pinned one with: make cosign" >&2
		exit 1
	fi

	local base="https://github.com/sigstore/cosign/releases/download/${version}"

	# --retry-all-errors covers the 403 the releases CDN serves under load, which
	# plain --retry does not (the reason scripts/fetch/download-verified.sh exists).
	local asset
	for asset in cosign_checksums.txt{,-keyless.pem,-keyless.sig}; do
		curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 \
			-o "${work}/${asset}" "${base}/${asset}"
	done

	# Both streams are held back so a success writes nothing but the digest: this
	# runs as an updatecli shell source, whose value is whatever the command
	# printed, and cosign announces "Verified OK" on stderr. On failure the held
	# output is the diagnosis, so it goes out then.
	if ! "$cosign" verify-blob \
		--certificate "${work}/cosign_checksums.txt-keyless.pem" \
		--signature "${work}/cosign_checksums.txt-keyless.sig" \
		--certificate-identity "$SIGNER_IDENTITY" \
		--certificate-oidc-issuer "$SIGNER_ISSUER" \
		"${work}/cosign_checksums.txt" > "${work}/verify.log" 2>&1; then
		echo "cosign_checksums.txt for ${version} is not signed by ${SIGNER_IDENTITY} (${SIGNER_ISSUER}):" >&2
		cat "${work}/verify.log" >&2
		exit 1
	fi

	# The checksums file is one "<sha256>  <asset>" line per asset. Emit only the
	# hash, with no trailing newline, so updatecli uses the value verbatim.
	local sha
	sha="$(awk -v asset="cosign-${platform}" '$2 == asset { print $1 }' "${work}/cosign_checksums.txt")"
	if [[ ! "$sha" =~ ^[0-9a-f]{64}$ ]]; then
		echo "no sha256 for cosign-${platform} in ${version}'s signed checksums" >&2
		exit 1
	fi
	printf '%s' "$sha"
}

main "$@"
