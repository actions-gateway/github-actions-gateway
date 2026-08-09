#!/usr/bin/env bash
#
# operator-caveats-since.sh — enumerate the operator-facing caveats a release is
# about to publish, so the release notes can carry them.
#
# `publish.yml` auto-generates a Release body from the commit log, which is a fine
# default and a poor changelog: a commit subject does not say "this upgrade needs a
# manual step first". Richer notes are opt-in — you create the Release before
# pushing the tag and the pipeline leaves your body alone (release.md § 5) — but
# nothing tells you there is anything to curate. This does.
#
# It needs no new bookkeeping. The doc-update matrix already REQUIRES an
# operator-visible change (a new default, a new failure mode, a required step, a
# removed values key) to land in docs/operations/, so the diff of those pages
# since the last tag already IS the list. This surfaces it in a scannable shape
# rather than as a release-sized raw diff.
#
# What it prints, per watched doc:
#   - added section headings — a new section is almost always a new thing to know
#   - added bold-lead bullets (`- **…**`), this repo's idiom for a caveat
#   - added lines mentioning BREAKING
#
# Reading it is judgement, not a gate: not every new section belongs in the notes,
# and it cannot tell a clarification from a caveat. It only guarantees you have
# SEEN them.
#
#   scripts/release/operator-caveats-since.sh            # since the latest tag
#   scripts/release/operator-caveats-since.sh v1.2.0     # since an explicit ref
#   scripts/release/operator-caveats-since.sh --quiet    # exit 0 if anything changed, 1 if not
set -euo pipefail
shopt -s inherit_errexit

# Operator-facing docs whose diff is worth reading before a tag. upgrade.md is the
# primary one (it is where upgrade-time edits are required to land);
# troubleshooting.md carries new failure modes an operator can hit.
CAVEAT_PATHS=(
	"docs/operations/upgrade.md"
	"docs/operations/troubleshooting.md"
)

quiet=0
ref=""
for arg in "$@"; do
	case "$arg" in
	--quiet) quiet=1 ;;
	-h | --help)
		awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
		exit 0
		;;
	*) ref="$arg" ;;
	esac
done

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

if [[ -z "$ref" ]]; then
	ref="$(git describe --tags --abbrev=0 2>/dev/null || true)"
	[[ -n "$ref" ]] || {
		echo "operator-caveats-since: no tag found; pass a ref explicitly" >&2
		exit 1
	}
fi

git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || {
	echo "operator-caveats-since: '$ref' is not a commit-ish in this repo" >&2
	exit 1
}

# Only diff paths present at both ends, so a page added after REF does not abort
# the run.
existing_paths=()
for path in "${CAVEAT_PATHS[@]}"; do
	[[ -e "$path" ]] && existing_paths+=("$path")
done

if ((${#existing_paths[@]} == 0)); then
	echo "operator-caveats-since: none of the watched docs exist; update CAVEAT_PATHS." >&2
	exit 1
fi

changed="$(git diff --name-only "$ref"..HEAD -- "${existing_paths[@]}")"

if [[ -z "$changed" ]]; then
	((quiet)) && exit 1
	echo "operator-caveats-since: no operator-facing doc changed between $ref and HEAD."
	exit 0
fi

((quiet)) && exit 0

# added_in PATH PATTERN — added (+) diff lines for one path matching PATTERN, with
# the leading '+' and indentation stripped. Suppresses its own status so an empty
# match is an empty section rather than a pipefail abort.
added_in() {
	local path="$1" pattern="$2"
	git diff "$ref"..HEAD -- "$path" |
		grep -E "^\+" | grep -v "^+++" |
		grep -E "$pattern" |
		awk '{sub(/^\+[ \t]*/, ""); print}' || true
}

echo "Operator-facing changes between $ref and HEAD."
echo "Carry the real caveats into curated release notes (release.md § 5 — create the"
echo "Release BEFORE pushing the tag, or the pipeline writes a generated body)."
echo

for path in "${existing_paths[@]}"; do
	sections="$(added_in "$path" '^\+#{2,6} ')"
	bullets="$(added_in "$path" '^\+- \*\*')"
	breaking="$(added_in "$path" 'BREAKING')"

	if [[ -z "$sections" && -z "$bullets" && -z "$breaking" ]]; then
		continue
	fi

	echo "=== $path"
	if [[ -n "$breaking" ]]; then
		echo "--- BREAKING"
		printf '%s\n' "$breaking" | sed 's/^/    /'
	fi
	if [[ -n "$sections" ]]; then
		echo "--- new sections"
		printf '%s\n' "$sections" | sed 's/^/    /'
	fi
	if [[ -n "$bullets" ]]; then
		echo "--- new bullets"
		printf '%s\n' "$bullets" | cut -c1-140 | sed 's/^/    /'
	fi
	echo
done

echo "Nothing here is automatic: decide per item whether an operator needs it at"
echo "upgrade time. A clarification is not a caveat."
