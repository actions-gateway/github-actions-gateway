#!/usr/bin/env bash
#
# Download a pinned cosign release binary for the current platform. cosign is
# a non-Go-vendored binary tool (its dependency tree is too large to vendor
# like the kubebuilder-ecosystem tools), so it is downloaded at a pinned
# version — the same pattern as the shellcheck/kubeconform CI installs. Backs
# the Makefile's $(COSIGN) rule; the pin lives there (COSIGN_VERSION).
#
# Usage: scripts/download-cosign.sh <output-path> <version>
set -euo pipefail

out="${1:-}"
version="${2:-}"
if [[ -z "$out" || -z "$version" ]]; then
	echo "usage: $0 <output-path> <version>" >&2
	exit 2
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
	aarch64|arm64) arch=arm64 ;;
	x86_64|amd64) arch=amd64 ;;
	*) echo "unsupported arch $arch" >&2; exit 1 ;;
esac

url="https://github.com/sigstore/cosign/releases/download/$version/cosign-$os-$arch"
echo "downloading cosign $version ($os-$arch)"
mkdir -p "$(dirname "$out")"
curl -fsSL --retry 3 --retry-delay 2 -o "$out" "$url"
chmod +x "$out"
