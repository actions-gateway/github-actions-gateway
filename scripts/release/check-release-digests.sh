#!/usr/bin/env bash
# check-release-digests.sh — do the notes' image digests match what was published?
#
#   scripts/release/check-release-digests.sh vX.Y.Z
#
# Exit 0 when every digest in the note matches the registry and is an index
# carrying both architectures, 1 on a mismatch, 2 on a usage or registry error.
#
# The Container images section is the one part of a release note that cannot be
# written before the tag, because the digests do not exist until the pipeline has
# run — so it is transcribed by hand, after the release is already published, into
# the document operators copy their `--set …image.digest=` pins from. A wrong
# character there is a broken install command in the most-read prose the project
# ships, and no gate saw it: check-release-links.sh resolves URLs, and the notes
# are not built by the site.
#
# Two things are checked, because a digest can be wrong in two ways.
#
#   1. It must equal what the registry serves for that tag.
#   2. It must be an INDEX digest, not a per-arch manifest. Provenance binds to
#      the index while SBOM attestations bind per-arch, so the two are easy to
#      confuse and a per-arch digest pasted here pins operators to one
#      architecture while still verifying cleanly against its own attestation.
set -euo pipefail
shopt -s inherit_errexit

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") vX.Y.Z [note.md]

		  Compares the note's Container images digests against the registry, and
		  asserts each is a multi-arch index.
		  note.md defaults to docs/releases/vX.Y.Z.md; pass one to check a draft,
		  or to plant a known failure and confirm this reports it.
	EOF
	exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
[[ "$1" == -h || "$1" == --help ]] && usage

VERSION="$1"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NOTE="${2:-${REPO_ROOT}/docs/releases/${VERSION}.md}"

[[ -f "$NOTE" ]] || {
	echo "check-release-digests: no release note at docs/releases/${VERSION}.md" >&2
	exit 2
}

# noted_digests FILE — "image<TAB>digest" for every pinned image in the note.
# Split out so the parsing half is exercised without a registry (see the test).
noted_digests() {
	grep -oE 'ghcr\.io/[^/]+/[a-z0-9-]+@sha256:[a-f0-9]{64}' "$1" |
		sort -u |
		awk -F'@' '{ n = split($1, p, "/"); print p[n] "\t" $2 }'
}

pairs="$(noted_digests "$NOTE")"
if [[ -z "$pairs" ]]; then
	echo "check-release-digests: ${VERSION} names no pinned images — has the Container images section been written?" >&2
	exit 1
fi

echo "check-release-digests: ${VERSION} ($(printf '%s\n' "$pairs" | grep -c .) image(s) pinned in the note)"

bad=0
while IFS=$'\t' read -r image noted; do
	[[ -n "$image" ]] || continue
	ref="ghcr.io/actions-gateway/${image}:${VERSION}"

	if ! raw="$(docker buildx imagetools inspect "$ref" --raw 2>/dev/null)"; then
		printf '  %-9s ERROR — cannot inspect %s\n' "$image" "$ref" >&2
		bad=$((bad + 1))
		continue
	fi

	live="$(docker buildx imagetools inspect "$ref" --format '{{.Manifest.Digest}}' 2>/dev/null || true)"
	media="$(printf '%s' "$raw" | jq -r '.mediaType // ""')"
	arches="$(printf '%s' "$raw" |
		jq -r '[.manifests[]? | select(.platform.os == "linux") | .platform.architecture] | sort | join(",")')"

	if [[ "$noted" != "$live" ]]; then
		printf '  %-9s MISMATCH\n    note:     %s\n    registry: %s\n' "$image" "$noted" "$live" >&2
		bad=$((bad + 1))
		continue
	fi
	case "$media" in
	*index* | *manifest.list*) ;;
	*)
		printf '  %-9s NOT AN INDEX — mediaType %s (a per-arch digest pins one architecture)\n' \
			"$image" "$media" >&2
		bad=$((bad + 1))
		continue
		;;
	esac
	if [[ "$arches" != "amd64,arm64" ]]; then
		printf '  %-9s INCOMPLETE — index carries linux/[%s], want amd64 and arm64\n' "$image" "$arches" >&2
		bad=$((bad + 1))
		continue
	fi

	printf '  %-9s ok (index, linux/%s)\n' "$image" "${arches//,/ + linux\/}"
done <<<"$pairs"

if ((bad > 0)); then
	printf '\ncheck-release-digests: %d image(s) wrong in %s.\n' "$bad" "${NOTE#"${REPO_ROOT}"/}" >&2
	printf 'Immutability leaves the notes editable, so fix the file and re-publish with\n' >&2
	printf '  gh release edit %s --notes-file <(scripts/release/render-release-body.sh docs/releases/%s.md)\n' \
		"$VERSION" "$VERSION" >&2
	exit 1
fi
echo "check-release-digests: ok"
