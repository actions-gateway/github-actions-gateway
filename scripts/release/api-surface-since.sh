#!/usr/bin/env bash
#
# api-surface-since.sh — enumerate the API surface a release is about to publish
# for the first time. Written after Q476, where a mis-named enum value was one
# commit from being frozen for the life of v2beta1 and nothing surfaced it.
#
# Usage:
#   scripts/release/api-surface-since.sh [REF]
#
# REF defaults to the most recent tag reachable from HEAD, which is the span a
# release cut from HEAD would publish. Pass an explicit ref (e.g. v1.1.0) to
# review a different window.
#
# This is the input-gathering half of the pre-release API review documented in
# docs/development/api-review.md — it reports WHAT changed and leaves the
# judgement to the reviewer. It is deliberately not a pass/fail gate: every
# question the review asks ("does this enum carry two axes", "whose fact is
# this") needs a human, and a gate that answered them mechanically would be
# wrong in both directions.
#
# Exit status is 0 whether or not surface changed; `--quiet` suppresses the
# per-section output and exits 1 when nothing changed, for scripted callers that
# only want to know whether a review is needed.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# API_PATHS — every tree whose contents are a published wire contract. CRD
# manifests are included because a default or enum can change there via a marker
# edit that the Go diff alone reads as a comment change.
API_PATHS=(
	"api"
	"cmd/agc/api"
	"cmd/gmc/api"
	"cmd/agc/config/crd"
	"cmd/gmc/config/crd"
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

if [[ -z "$ref" ]]; then
	ref="$(git describe --tags --abbrev=0 2>/dev/null || true)"
	[[ -n "$ref" ]] || {
		echo "api-surface-since: no tag found; pass a ref explicitly" >&2
		exit 1
	}
fi

git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || {
	echo "api-surface-since: '$ref' is not a commit-ish in this repo" >&2
	exit 1
}

# Only diff paths that exist at both ends, so a tree added after REF does not
# abort the whole run.
existing_paths=()
for path in "${API_PATHS[@]}"; do
	[[ -e "$path" ]] && existing_paths+=("$path")
done

changed="$(git diff --name-only "$ref"..HEAD -- "${existing_paths[@]}")"

if [[ -z "$changed" ]]; then
	if ((quiet)); then
		exit 1
	fi
	echo "api-surface-since: no API surface changed between $ref and HEAD."
	exit 0
fi

((quiet)) && exit 0

# added_lines PATTERN — added (+) diff lines matching PATTERN, with the leading
# '+' and indentation stripped, de-duplicated. Suppresses its own exit status so
# an empty match is a normal empty section rather than a pipefail abort.
added_lines() {
	local pattern="$1"
	git diff "$ref"..HEAD -- "${existing_paths[@]}" |
		grep -E "^\+" | grep -v "^+++" |
		grep -E "$pattern" |
		awk '{sub(/^\+[ \t]*/, ""); print}' |
		sort -u || true
}

# values_at REV PATTERN — every distinct quoted string literal assigned on a line
# matching PATTERN, as it exists in REV. Used instead of reading diff lines for
# surface that MOVES between files: the Q374 refactor relocated the whole
# condition vocabulary into api/apiconditions, which a line-diff reports as a
# hundred new conditions when none of them are new. Comparing the value SETS at
# each end is immune to that, and to the per-version re-export duplication.
values_at() {
	local rev="$1" pattern="$2"
	git grep -h -E "$pattern" "$rev" -- "${existing_paths[@]}" 2>/dev/null |
		grep -vE '^\s*//' |
		grep -oE '"[^"]+"' |
		tr -d '"' |
		sort -u || true
}

# new_values PATTERN — values present at HEAD and absent at REF.
new_values() {
	local pattern="$1"
	comm -13 <(values_at "$ref" "$pattern") <(values_at HEAD "$pattern")
}

section() {
	local title="$1" body="$2"
	echo
	echo "== $title"
	if [[ -z "$body" ]]; then
		echo "  (none)"
	else
		echo "$body" | awk '{print "  " $0}'
	fi
}

echo "API surface between $ref and HEAD"
echo "Review checklist: docs/development/api-review.md"

section "Files changed" "$(git diff --stat "$ref"..HEAD -- "${existing_paths[@]}" | awk 'NF')"
section "Added fields (wire names)" "$(added_lines 'json:"')"
section "Added or changed enum constraints" "$(added_lines 'kubebuilder:validation:Enum')"
section "Added or changed defaults" "$(added_lines 'kubebuilder:default')"
section "New condition types and reasons" "$(new_values '^[[:space:]]*(Condition|Reason)[A-Z][A-Za-z0-9]*[[:space:]]*=[[:space:]]*"')"
section "New label and annotation keys" "$(new_values '=[[:space:]]*"(actions-gateway\.com|actions-gateway\.github\.com)/')"

echo
echo "Everything above is published for the first time by a release cut from HEAD."
echo "Record the verdict in the release plan doc; file deferrals with the release's gate label."
