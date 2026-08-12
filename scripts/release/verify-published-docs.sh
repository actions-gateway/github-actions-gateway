#!/usr/bin/env bash
#
# verify-published-docs.sh — fail when the published /X.Y.Z/ docs advertise a
# release other than X.Y.Z (Q784).
#
# check-release-pins.sh holds the working tree to the newest tag, which fixes
# `main` and the `dev` docs and nothing else: the site builds each version from
# its own tag, so /X.Y.Z/ is frozen at what that tag's tree said — the PREVIOUS
# release's pins. Three of the four releases cut since 1.0.0 published the
# previous version's install command as their landing page: /1.1.0/ and /1.2.0/
# still advertise `--version 1.0.0`, and /1.4.0/ advertised 1.3.0 (with the
# v1.3.0 CRD manifest URL) until it was reported three hours later. The republish
# that repairs it is a hand step (release.md step 7); this is the check that says
# whether it worked.
#
# Reads the PUBLISHED pages, not the tree — that is the whole point, and it is
# why this is a release step rather than part of `make check`.
#
# Scanning is scoped to each page's <article> element. The surrounding chrome is
# deliberately NOT the version being read: hooks/release_version.py resolves the
# announce bar to the newest release at build time, so a correct /1.1.0/ page
# announces v1.2.0 and asserting over the whole document would report site chrome
# as a stale pin. A page whose <article> cannot be found fails rather than
# passing by scanning nothing.
#
# Which literals count, and the two deliberate exemptions, come from
# release_version_literals / release_pin_exempt_versions_regexp in
# scripts/lib/common.sh — the same extractor check-release-pins.sh runs over the
# sources, so a pin shape one gate sees cannot be invisible to the other.
#
# Usage:
#   verify-published-docs.sh vX.Y.Z [--no-stable] [--base URL]
#
#   --no-stable  Skip the `stable` alias and root-redirect checks. Pass it for a
#                backport to an older line, whose pages dispatch drops
#                `alias=stable set_default=true` (release.md step 7).
#   --base URL   Site root to read (default https://actions-gateway.com).
#
# Env:
#   CURL  curl binary to fetch with (default `curl`).
#
# Backs `make verify-published-docs VERSION=vX.Y.Z`. Assertions:
# verify-published-docs-test.sh, under `make scripts-test`.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

BASE_URL="https://actions-gateway.com"
CURL="${CURL:-curl}"
check_stable=1
VERSION=""

while (($# > 0)); do
	case "$1" in
	--no-stable)
		check_stable=0
		;;
	--base)
		BASE_URL="${2:-}"
		shift
		;;
	-*)
		printf 'verify-published-docs: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	*)
		VERSION="$1"
		;;
	esac
	shift
done

if [[ ! "$VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "usage: $0 vX.Y.Z [--no-stable] [--base URL]   (or: make verify-published-docs VERSION=vX.Y.Z)" >&2
	exit 2
fi

BASE_URL="${BASE_URL%/}"
version="${VERSION#v}"
minor="${version%.*}"
exempt_versions_re="$(release_pin_exempt_versions_regexp)"

# The published half of check-release-pins.sh's DEFAULT_PIN_FILES: docs/index.md
# and the three docs/operations/ pages, as mkdocs publishes them. README.md is in
# that set but is not a site page, so the source gate is its only cover.
PAGES=(
	""
	"operations/install/"
	"operations/upgrade/"
	"operations/gitops/"
)

WORK_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd)/tmp/verify-published-docs"
mkdir -p "$WORK_DIR"
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

# fetch URL DEST — retrieve a page, or fail with the URL that could not be read.
fetch() {
	local url="$1" dest="$2"
	if ! "$CURL" -fsSL --max-time 30 --retry 3 --retry-delay 2 -o "$dest" "$url"; then
		printf 'verify-published-docs: cannot read %s\n' "$url" >&2
		return 1
	fi
}

# article_text FILE — print the text of the page's <article>, one block element
# per line. Tokenizes on `<`: each record is `tag>text`, which is enough for the
# generated markup (MkDocs escapes `<` and `>` in content). Exits 3 when the page
# has no <article>, so a theme change is reported rather than scanned past.
article_text() {
	awk '
		BEGIN { RS = "<"; ORS = "" }
		NR == 1 { next }
		{
			gt = index($0, ">")
			if (gt == 0) next
			tag = substr($0, 1, gt - 1)
			text = substr($0, gt + 1)
			closing = (substr(tag, 1, 1) == "/")
			name = tolower(tag)
			sub(/^\//, "", name)
			sub(/[ \t\r\n\/].*$/, "", name)
			if (name == "article") {
				if (closing) { inside = 0; seen = 1; next }
				inside = 1
				print "\n" text
				next
			}
			if (!inside) next
			if (name ~ /^(p|li|h[1-6]|div|blockquote|tr|td|th|pre|ul|ol|table|section|br|dt|dd)$/) print "\n"
			print text
		}
		END { if (!seen) exit 3 }
	' "$1"
}

fail=0
total=0

for page in "${PAGES[@]}"; do
	url="$BASE_URL/$version/$page"
	label="/$version/$page"
	html="$WORK_DIR/page.html"
	text="$WORK_DIR/page.txt"

	fetch "$url" "$html" || {
		fail=1
		continue
	}
	if ! article_text "$html" > "$text"; then
		printf 'verify-published-docs: %s: no <article> element — the page did not render, or\n' "$label" >&2
		printf '                      the theme changed and this scan now reads nothing.\n' >&2
		fail=1
		continue
	fi

	found=0
	while IFS=$'\t' read -r _ tok kind; do
		[[ -n "$tok" ]] || continue
		[[ "$tok" =~ $exempt_versions_re ]] && continue
		found=$((found + 1))
		if [[ "$kind" == "patchline" ]]; then
			[[ "${tok#v}" == "${minor}.z" ]] && continue
			printf "verify-published-docs: %s: patch-line hint \`%s\` names another release; this page is %s\n" \
				"$label" "$tok" "${minor}.z" >&2
		else
			[[ "${tok#v}" == "$version" ]] && continue
			printf 'verify-published-docs: %s: advertises %s, but this is the %s docs\n' \
				"$label" "$tok" "$version" >&2
		fi
		fail=1
	done < <(release_version_literals "$text")

	# An empty result cannot tell "this page pins the right release" from "the
	# pin moved and my scan no longer sees it", so a page yielding nothing is a
	# failure — the same rule check-release-pins.sh applies to the sources.
	if ((found == 0)); then
		printf 'verify-published-docs: %s: no release-version literal found. This page pins a\n' "$label" >&2
		printf '                      version in the source tree, so finding none here means the\n' >&2
		printf '                      scan missed it, not that the page is clean.\n' >&2
		fail=1
		continue
	fi
	total=$((total + found))
	printf 'verify-published-docs: %-26s %d pin(s)\n' "$label" "$found"
done

if ((check_stable)); then
	# `stable` and the root redirect move with the highest release, and were
	# wrong alongside /1.4.0/. The alias page names the version dir it serves in
	# its canonical link; the root is a meta refresh to the alias.
	html="$WORK_DIR/stable.html"
	if fetch "$BASE_URL/stable/" "$html"; then
		canonical="$(awk 'match($0, /rel="canonical" href="[^"]*"/) {
			s = substr($0, RSTART, RLENGTH); sub(/.*href="/, "", s); sub(/"$/, "", s); print s; exit }' "$html")"
		if [[ "${canonical%/}" == "$BASE_URL/$version" ]]; then
			printf 'verify-published-docs: %-26s -> %s\n' "/stable/" "$canonical"
		else
			printf 'verify-published-docs: /stable/ serves %s, not %s/%s/\n' \
				"${canonical:-<no canonical link>}" "$BASE_URL" "$version" >&2
			fail=1
		fi
	else
		fail=1
	fi

	html="$WORK_DIR/root.html"
	if fetch "$BASE_URL/" "$html"; then
		target="$(awk 'match(tolower($0), /url=[^"'"'"' >]+/) {
			s = substr($0, RSTART + 4, RLENGTH - 4); print s; exit }' "$html")"
		if [[ "${target%/}" == "stable" ]]; then
			printf 'verify-published-docs: %-26s -> %s\n' "/" "$target"
		else
			printf 'verify-published-docs: / redirects to %s, not stable/ — a visitor landing on the\n' \
				"${target:-<no redirect>}" >&2
			printf '                      root does not reach the current release.\n' >&2
			fail=1
		fi
	else
		fail=1
	fi
fi

if ((fail)); then
	printf '\nverify-published-docs: the published docs for %s do not advertise %s.\n' "$version" "$version" >&2
	printf "Landing the pin bump on \`main\` does not reach them — republish from the release\n" >&2
	printf 'branch, per docs/operations/release.md#the-bump-on-main-does-not-reach-the-published-release.\n' >&2
	exit 1
fi

printf 'verify-published-docs: ok (%d pin(s) across %d page(s), all naming %s)\n' \
	"$total" "${#PAGES[@]}" "$version"
