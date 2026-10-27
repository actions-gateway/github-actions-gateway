#!/usr/bin/env bash
#
# Download a pinned cosign release binary for the current platform and verify it
# against an in-repo pinned SHA256. cosign is a non-Go-vendored binary tool (its
# dependency tree is too large to vendor like the kubebuilder-ecosystem tools),
# so it is downloaded at a pinned version — the same pattern as the kubeconform
# and pinned-linter CI installs. Backs the Makefile's $(COSIGN) rule (which in
# turn backs `make verify-release`); the version pin lives there
# (COSIGN_VERSION).
#
# Integrity (Q126/Q127): GitHub release assets are mutable for an existing tag,
# so a raw download is not trustworthy on its own — and cosign is the sharpest
# case, since it is the binary that verifies release signatures. We therefore
# pin the expected SHA256 per (version, platform) below and fail closed if the
# downloaded bytes don't match, or if the requested version has no pinned digest
# (a deliberate gate: bumping COSIGN_VERSION must add the new digests here,
# mirroring the KIND_BINARY_SHA256 pin in e2e-test.yml). The publish pipeline
# obtains cosign via the SHA-pinned sigstore/cosign-installer action instead
# (which performs its own signature verification); this script covers the local
# verify path.
#
# Updating the pins on a version bump: updatecli.d/cosign.yaml does it weekly,
# rewriting the four lines below together with COSIGN_VERSION and publish.yml's
# cosign-release, from the release's sigstore-signed cosign_checksums.txt (Q927).
# By hand, the same source:
#   curl -fsSL https://github.com/sigstore/cosign/releases/download/<ver>/cosign_checksums.txt
#
# Usage: scripts/release/download-cosign.sh <output-path> <version>
set -euo pipefail
shopt -s inherit_errexit

out="${1:-}"
version="${2:-}"
if [[ -z "$out" || -z "$version" ]]; then
	echo "usage: $0 <output-path> <version>" >&2
	exit 2
fi

# Pinned SHA256 digests of the official cosign release binaries, keyed by
# "<version> <os>-<arch>". Add a block when bumping COSIGN_VERSION.
#
# One line per platform, single-spaced: updatecli.d/cosign.yaml rewrites each
# with one regexp per platform, and aligning the digests would need a different
# pattern per row for no reader's benefit.
expected_sha256() {
	local key="$1"
	case "$key" in
		"v3.1.4 darwin-amd64") echo "9d1e4abe2b6be279ed0d2ff706fde1ccd738747c834e3e6d44039c238867334f" ;;
		"v3.1.4 darwin-arm64") echo "9f9b8cd557cf2e5b3df3373a00baa005a86b02391e3e06dd0ce37b3a1533d3a4" ;;
		"v3.1.4 linux-amd64") echo "424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa" ;;
		"v3.1.4 linux-arm64") echo "ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2" ;;
		*) return 1 ;;
	esac
}

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
	aarch64|arm64) arch=arm64 ;;
	x86_64|amd64) arch=amd64 ;;
	*) echo "unsupported arch $arch" >&2; exit 1 ;;
esac

platform="$os-$arch"
want="$(expected_sha256 "$version $platform" || true)"
if [[ -z "$want" ]]; then
	echo "no pinned SHA256 for cosign $version ($platform); add it to $0 before bumping COSIGN_VERSION" >&2
	exit 1
fi

url="https://github.com/sigstore/cosign/releases/download/$version/cosign-$platform"
echo "downloading cosign $version ($platform)"

# download-verified.sh owns the retry-and-verify half: it retries a transient
# releases-CDN denial (including a 403, which curl's plain --retry does not
# cover — Q433), and only writes $out once the bytes match $want, so a failed
# integrity check never leaves an unverified binary at the output path.
"$(dirname "$0")/../fetch/download-verified.sh" "$url" "$want" "$out"

chmod +x "$out"
echo "verified cosign $version ($platform) against pinned SHA256"
