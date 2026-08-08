#!/usr/bin/env bash
#
# Cross-compile the gag-migrate CLI (cmd/gmc/migrate) for the release platform
# matrix and emit a SHA256SUMS manifest alongside the binaries. The publish
# workflow keyless-signs the manifest (cosign sign-blob) and uploads the binaries
# + manifest + signature as GitHub Release assets — the same no-secret Fulcio/Rekor
# path the charts and the v2 CRD manifest use (docs/operations/release.md).
#
# Usage:
#   scripts/release/build-migrate-binaries.sh <output-dir> [version]
#
# <output-dir>  directory to write the binaries and SHA256SUMS into (created).
# [version]     version string stamped into the binary name (default: "dev").
#
# The build is reproducible-friendly: CGO is disabled, symbol tables are stripped
# (-s -w), and the module build metadata is trimmed (-trimpath).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# The release platform matrix: the OSes/arches an operator plausibly runs kubectl
# from. Keep in step with the asset list in docs/operations/release.md.
PLATFORMS=(
	"linux/amd64"
	"linux/arm64"
	"darwin/amd64"
	"darwin/arm64"
	"windows/amd64"
)

# sha256_all FILE... — print a "<hash>  <file>" line per argument, using whichever
# checksum tool the host provides (sha256sum on Linux, shasum -a 256 on macOS).
sha256_all() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

main() {
	local out_dir="${1:-}" version="${2:-dev}"
	if [[ -z "$out_dir" ]]; then
		echo "usage: $0 <output-dir> [version]" >&2
		exit 2
	fi
	require_cmd go "https://go.dev/dl/"

	mkdir -p "$out_dir"
	# Absolute so the `go build -C cmd/gmc` working-directory change can't misplace
	# the output.
	out_dir="$(cd "$out_dir" && pwd)"

	local platform os arch ext name
	for platform in "${PLATFORMS[@]}"; do
		os="${platform%/*}"
		arch="${platform#*/}"
		ext=""
		[[ "$os" == "windows" ]] && ext=".exe"
		name="gag-migrate-${version}-${os}-${arch}${ext}"
		echo "building ${name}"
		# -C enters the gmc module (Go workspace); the migrate command is ./migrate.
		( cd "$REPO_ROOT/cmd/gmc" && \
			CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
			go build -trimpath -ldflags="-s -w" -o "$out_dir/$name" ./migrate )
	done

	# One SHA256SUMS manifest over every binary — a single signed blob then covers
	# the whole set (the release verifies the signature on the manifest, then the
	# manifest's checksums against the downloaded binaries). `sha256sum` on Linux
	# (the CI release runner), `shasum -a 256` on macOS — both emit the same
	# "<hash>  <file>" format that `sha256sum -c` consumes.
	( cd "$out_dir" && sha256_all gag-migrate-* > SHA256SUMS )
	echo "wrote $out_dir/SHA256SUMS"
}

main "$@"
