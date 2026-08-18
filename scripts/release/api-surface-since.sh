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
# The Event reasons are enumerated by devtools/docs/reasontiers, which needs Go
# and adds a build plus two `git archive` extractions to a run that has AGC
# source in its window (Q780). A window with none skips all of it.
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

# REASON_SRC — the trees the Event-reason scan reads, and the only trees it
# reads: the AGC, and the shared vocabulary its reasons are declared in. The
# enumeration is a pure function of these two, so a window that changes neither
# cannot have changed a reason, which is what lets the scan be skipped entirely.
REASON_SRC=(
	"cmd/agc"
	"api"
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

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

# An Event reason is an argument at the recording site rather than a declaration,
# so no pattern over the source enumerates it: two recorders here are named Event
# and two recordEvent, with the reason at a different index in each. The v1.4.0
# notes were enumerated by a pattern that read the wrong declaration, published
# the action string `ProvisionWorker` as a reason, and missed four real ones.
# devtools/docs/reasontiers reads the index off the callee's own declaration and
# fails rather than shortening its list, which is what makes a set diff of its
# output mean anything (Q780).
#
# reason_error carries why a scan did not produce a set, so the section can say
# that instead of printing an empty one — an unreadable ref would otherwise
# report every reason as new, and an unreadable HEAD as none.
reason_error=""
reasons_bin=""

build_reasontiers() {
	if ! command -v go >/dev/null 2>&1; then
		reason_error="go is not on PATH"
		return 1
	fi
	reasons_bin="$WORK/reasontiers"
	# devtools/ is outside the Go workspace, hence GOWORK=off — see
	# docs/development/go-workspaces.md.
	if ! (cd devtools && GOWORK=off go build -o "$reasons_bin" ./docs/reasontiers) >"$WORK/build.log" 2>&1; then
		reason_error="building devtools/docs/reasontiers failed: $(tail -n 1 "$WORK/build.log")"
		return 1
	fi
}

# scan_ref REV TAG — extract REV's reason trees under $WORK/TAG and write the
# scan to $WORK/TAG.reasons. HEAD is extracted rather than read from the working
# tree so both ends are git content, as every other section here already is.
scan_ref() {
	local rev="$1" tag="$2"
	mkdir -p "$WORK/$tag"
	if ! git archive --format=tar --output="$WORK/$tag.tar" "$rev" -- "${REASON_SRC[@]}" 2>"$WORK/$tag.err"; then
		reason_error="git archive $rev failed: $(tail -n 1 "$WORK/$tag.err")"
		return 1
	fi
	if ! tar -xf "$WORK/$tag.tar" -C "$WORK/$tag" 2>"$WORK/$tag.err"; then
		reason_error="extracting $rev failed: $(tail -n 1 "$WORK/$tag.err")"
		return 1
	fi
	if ! "$reasons_bin" -list "$WORK/$tag/cmd/agc" "$WORK/$tag/api" >"$WORK/$tag.reasons" 2>"$WORK/$tag.err"; then
		reason_error="scanning $rev: $(tail -n 1 "$WORK/$tag.err")"
		return 1
	fi
}

# event_reasons_at TAG — the Event reason values from TAG's scan, re-sorted here
# so both sides of the comm below collate the way this shell's sort does.
event_reasons_at() {
	awk '$1 == "event" { print $2 }' "$WORK/$1.reasons" | sort
}

# event_reason_body — what the section says: the reasons new since REF, or why
# there is no set to report. Never empty in the second case, because an empty
# section there would read as "none new".
event_reason_body() {
	if [[ -z "$reason_error" ]]; then
		printf '%s' "$new_event_reasons"
		return
	fi
	printf 'COULD NOT ENUMERATE: %s\n' "$reason_error"
	printf 'This is not a report of none-new. Fix it, or enumerate by hand, before publishing.'
}

new_event_reasons=""
if [[ -n "$(git diff --name-only "$ref"..HEAD -- "${REASON_SRC[@]}")" ]]; then
	if build_reasontiers && scan_ref "$ref" ref && scan_ref HEAD head; then
		new_event_reasons="$(comm -13 <(event_reasons_at ref) <(event_reasons_at head))"
	fi
fi

# A failed scan counts as "something to review" for the same reason it is not an
# empty section: nobody can say the Event surface is unchanged until it runs.
if [[ -z "$changed" && -z "$new_event_reasons" && -z "$reason_error" ]]; then
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
section "New Event reasons (AGC only; nothing enumerates the GMC's)" "$(event_reason_body)"
section "New label and annotation keys" "$(new_values '=[[:space:]]*"(actions-gateway\.com|actions-gateway\.github\.com)/')"

echo
echo "Everything above is published for the first time by a release cut from HEAD."
echo "Record the verdict in the release plan doc; file deferrals with the release's gate label."
