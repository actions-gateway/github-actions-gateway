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
# Updating the pins on a version bump: fetch the release checksums and copy the
# four cosign-<os>-<arch> lines, e.g.
#   curl -fsSL https://github.com/sigstore/cosign/releases/download/<ver>/cosign_checksums.txt
#
# Usage: scripts/download-cosign.sh <output-path> <version>
set -euo pipefail

out="${1:-}"
version="${2:-}"
if [[ -z "$out" || -z "$version" ]]; then
	echo "usage: $0 <output-path> <version>" >&2
	exit 2
fi

# Pinned SHA256 digests of the official cosign release binaries, keyed by
# "<version> <os>-<arch>". Add a block when bumping COSIGN_VERSION.
expected_sha256() {
	local key="$1"
	case "$key" in
		"v2.5.2 darwin-amd64") echo "0681abe20a482f4b9b3ed65b3debb8c6346591f2dc484b6bfa79609ff1318de4" ;;
		"v2.5.2 darwin-arm64") echo "51fdc6d8da8310d72df10065a52247b6bebfe990d4c946dd9f71e17588256011" ;;
		"v2.5.2 linux-amd64")  echo "bcfeae05557a9f313ee4392d2f335d0ff69ebbfd232019e3736fb04999fe1734" ;;
		"v2.5.2 linux-arm64")  echo "2cbcea1873ad76274c3f241ef175d204654e3aac3e73e6ec4504e5227015cb0a" ;;
		*) return 1 ;;
	esac
}

# Portable SHA256: coreutils sha256sum on Linux/CI, shasum -a 256 on macOS.
sha256_of() {
	local f="$1"
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$f" | awk '{print $1}'
	else
		shasum -a 256 "$f" | awk '{print $1}'
	fi
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

# Download to a temp file and verify before moving into place, so a failed
# integrity check never leaves an unverified binary at the output path.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL --retry 3 --retry-delay 2 -o "$tmp" "$url"

got="$(sha256_of "$tmp")"
if [[ "$got" != "$want" ]]; then
	echo "cosign $version ($platform) SHA256 mismatch:" >&2
	echo "  expected $want" >&2
	echo "  got      $got" >&2
	echo "refusing to install an unverified cosign binary" >&2
	exit 1
fi

mkdir -p "$(dirname "$out")"
mv "$tmp" "$out"
trap - EXIT
chmod +x "$out"
echo "verified cosign $version ($platform) against pinned SHA256"
