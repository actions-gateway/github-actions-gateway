#!/usr/bin/env bash
# check-artifact-unchanged.sh — does a window change what a release ships?
#
# Between a validated release candidate and the stable tag, only changes that
# leave the shipped artifacts byte-identical may land: the candidate's dogfood
# validation covers the tree it was cut from, and anything that moves an artifact
# after it makes the verdict cover something other than what ships.
#
#   scripts/release/check-artifact-unchanged.sh <validated-ref> [to-ref]
#
# Exit 0 when nothing on the released surface changed, 1 when something did (the
# files are listed), 2 on a usage or git error.
#
# "Doc-only" is NOT the same question, which is why this reads the surface rather
# than the diff's file extensions: `charts/actions-gateway/README.md` is a
# markdown file that ships inside the chart tarball. A pure-docs pull request can
# change a published chart's bytes.
#
# The surface is derived, never listed here. `semverfloor -ships` follows
# publish.yml's image matrix through each Dockerfile's COPY --from= edges to the
# go build behind it, plus the chart trees `helm package` packages, so adding an
# image or a chart is picked up with nothing to maintain in this script.
#
# Not `semver-floor.sh`: the floor reads a commit's *type* before its paths, so a
# `build:` or `docs:` commit that edits a shipped file is withheld from it by
# design. Measured 2026-08-15 — the floor reported "Nothing that ships has
# changed" across a window that changed a chart README.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEVTOOLS_DIR="$SCRIPT_DIR/../../devtools"

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") <validated-ref> [to-ref]

		  validated-ref  the commit the candidate was validated at (tag or SHA)
		  to-ref         default: origin/main if it resolves, else HEAD
	EOF
	exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
[[ "$1" == -h || "$1" == --help ]] && usage

from="$1"
to="${2:-}"
if [[ -z "$to" ]]; then
	to="HEAD"
	if git rev-parse --verify --quiet origin/main >/dev/null; then
		to="origin/main"
	fi
fi

for ref in "$from" "$to"; do
	if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
		echo "check-artifact-unchanged: not a commit: ${ref}" >&2
		exit 2
	fi
done

# Build the tool from the vendored module, the same way semver-floor.sh does.
bin="$(mktemp -d)/semverfloor"
trap 'rm -rf "$(dirname "$bin")"' EXIT
(cd "$DEVTOOLS_DIR" && GOWORK=off go build -o "$bin" ./release/semverfloor)

changed="$(git diff --name-only "${from}..${to}")"
shipped=""
if [[ -n "$changed" ]]; then
	# The tool decides; this script never pattern-matches a path itself.
	shipped="$(printf '%s\n' "$changed" | "$bin" -ships)"
fi

from_short="$(git rev-parse --short "${from}^{commit}")"
to_short="$(git rev-parse --short "${to}^{commit}")"

if [[ -z "$shipped" ]]; then
	printf 'check-artifact-unchanged: ok (%s..%s, %d file(s) changed, none on the released surface)\n' \
		"$from_short" "$to_short" "$(printf '%s\n' "$changed" | grep -c . || true)"
	exit 0
fi

printf 'check-artifact-unchanged: %d file(s) on the released surface changed between %s and %s:\n' \
	"$(printf '%s\n' "$shipped" | wc -l | tr -d ' ')" "$from_short" "$to_short" >&2
while IFS= read -r f; do printf '  %s\n' "$f" >&2; done <<<"$shipped"
cat >&2 <<-EOF

	The candidate validated at ${from_short} no longer covers what would ship.
	Either revert these before tagging, or cut and validate a new candidate.
EOF
exit 1
