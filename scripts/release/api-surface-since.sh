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
# and adds a build plus two `git archive` extractions to a run that has AGC or
# GMC source in its window (Q780, Q925). A window with none skips all of it.
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

# REASON_TREES — the binaries whose Event reasons are enumerated, each scanned
# against the shared vocabulary in api/. REASON_SRC is those plus that
# vocabulary: the enumeration is a pure function of them, so a window that
# changes none cannot have changed a reason, which is what lets the scan be
# skipped entirely.
REASON_TREES=(
	"cmd/agc"
	"cmd/gmc"
)
REASON_SRC=(
	"${REASON_TREES[@]}"
	"api"
)

# OPERATOR_PATHS — where the two surfaces an operator configures and watches are
# declared. Wider than API_PATHS on purpose: a metric is registered in the binary
# and a chart value is neither Go nor a CRD, so neither is reachable from a wire
# contract tree.
OPERATOR_PATHS=(
	"cmd"
	"api"
	"charts"
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

operator_paths=()
for path in "${OPERATOR_PATHS[@]}"; do
	[[ -e "$path" ]] && operator_paths+=("$path")
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
	# git archive is fatal on a pathspec matching nothing, so name only the trees
	# REV actually carries.
	local -a reason_src_at_rev=()
	local path
	for path in "${REASON_SRC[@]}"; do
		git cat-file -e "${rev}:${path}" 2>/dev/null && reason_src_at_rev+=("$path")
	done
	if ((${#reason_src_at_rev[@]} == 0)); then
		reason_error="$rev carries none of ${REASON_SRC[*]}"
		return 1
	fi
	if ! git archive --format=tar --output="$WORK/$tag.tar" "$rev" -- "${reason_src_at_rev[@]}" 2>"$WORK/$tag.err"; then
		reason_error="git archive $rev failed: $(tail -n 1 "$WORK/$tag.err")"
		return 1
	fi
	if ! tar -xf "$WORK/$tag.tar" -C "$WORK/$tag" 2>"$WORK/$tag.err"; then
		reason_error="extracting $rev failed: $(tail -n 1 "$WORK/$tag.err")"
		return 1
	fi
	: >"$WORK/$tag.reasons"
	local tree
	for tree in "${REASON_TREES[@]}"; do
		# A tree absent at REV contributes nothing rather than aborting: one added
		# after the tag reports every reason as new, which is what it is.
		[[ -d "$WORK/$tag/$tree" ]] || continue
		if ! "$reasons_bin" -list "$WORK/$tag/$tree" "$WORK/$tag/api" >>"$WORK/$tag.reasons" 2>"$WORK/$tag.err"; then
			reason_error="scanning $tree at $rev: $(tail -n 1 "$WORK/$tag.err")"
			return 1
		fi
	done
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

# literals_at REV REGEX PATHS… — every distinct string literal at REV whose
# surrounding text matches REGEX. Unlike values_at, which takes every quoted
# string on a matching line, this reads the literal out of the match itself: a
# metric registration and a flag declaration both sit on lines carrying other
# strings (a help string, a label list), and taking the line's strings would
# enumerate those too.
literals_at() {
	local rev="$1" regex="$2"
	shift 2
	git grep -h -oE "$regex" "$rev" -- "$@" 2>/dev/null |
		grep -oE '"[^"]+"' |
		tr -d '"' |
		sort -u || true
}

# new_literals WHAT REGEX PATHS… — the literals new at HEAD, or a refusal when
# the enumeration came back empty at REF.
#
# The refusal is the whole point of this helper, and it is what these two
# surfaces had no way to say before. Measured 2026-08-29 drafting the 1.7.0
# notes: a hand-rolled CLI-flag query matched the wrong declaration shape and
# returned zero on BOTH sides of the window. Two empty sets diff to an empty
# set, which is indistinguishable from a real "nothing changed", and the reading
# survives into the notes as a published claim; a corrected query then found 33
# flags at each end.
#
# A released tag has metrics and it has flags, so an empty set at REF is this
# query matching nothing — never the surface being empty. Saying so is the same
# discipline the Event-reason scanner already applies, and the reason it is
# worth having: an unenumerable surface has to block the claim rather than
# report a silent none.
new_literals() {
	local what="$1" regex="$2"
	shift 2
	local at_ref
	at_ref="$(literals_at "$ref" "$regex" "$@")"
	if [[ -z "$at_ref" ]]; then
		printf 'COULD NOT ENUMERATE: no %s found at %s, so this query is matching nothing.\n' "$what" "$ref"
		printf 'This is not a report of none-new. Fix the query, or enumerate by hand, before publishing.'
		return
	fi
	comm -13 <(printf '%s\n' "$at_ref") <(literals_at HEAD "$regex" "$@")
}

# CHART_VALUES — the values files whose keys an operator sets. Enumerated by
# path rather than by glob so a chart added without being listed here is a
# reviewer's omission rather than a silent widening of what the notes claim.
CHART_VALUES=(
	"charts/actions-gateway/values.yaml"
	"charts/actions-gateway-crds-v2/values.yaml"
)

# chart_values_at REV — every key path in REV's values files, as dotted paths.
# A YAML key is not a string literal, so literals_at cannot see one: this tracks
# indentation instead, which is enough for the flat-to-three-deep shape these
# files have and reports a nested key as parent.child so two charts' `enabled`
# do not collide. A file absent at REV contributes nothing.
chart_values_at() {
	local rev="$1" file
	for file in "${CHART_VALUES[@]}"; do
		git cat-file -e "${rev}:${file}" 2>/dev/null || continue
		git show "${rev}:${file}" | awk -v chart="${file}" '
			/^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
			match($0, /^[[:space:]]*[A-Za-z_][A-Za-z0-9_-]*:/) {
				line = $0
				indent = match(line, /[^ ]/) - 1
				key = line
				sub(/^[[:space:]]*/, "", key)
				sub(/:.*$/, "", key)
				depth = int(indent / 2)
				path[depth] = key
				out = chart
				for (i = 0; i <= depth; i++) out = out " " path[i]
				print out
			}
		'
	done | sort -u
}

# new_chart_values — chart keys new at HEAD, with the same refusal as
# new_literals: a released tag has chart values, so an empty set at REF is the
# reader failing rather than the surface being empty.
new_chart_values() {
	local at_ref
	at_ref="$(chart_values_at "$ref")"
	if [[ -z "$at_ref" ]]; then
		printf 'COULD NOT ENUMERATE: no chart value found at %s, so this reader is matching nothing.\n' "$ref"
		printf 'This is not a report of none-new. Fix the reader, or enumerate by hand, before publishing.'
		return
	fi
	comm -13 <(printf '%s\n' "$at_ref") <(chart_values_at HEAD)
}

# The operator surfaces are computed here rather than at their sections because
# they participate in the early exit below. A window that adds only a metric or
# only a flag touches none of API_PATHS and emits no Event reason, so deciding
# "nothing changed" without them would report exactly the silent false negative
# these sections were added to stop (Q1037).
new_metric_names="$(new_literals 'metric name' '"actions_gateway_[a-z0-9_]+"' "${operator_paths[@]}")"
new_cli_flags="$(new_literals 'CLI flag' '\.(String|Int|Int64|Uint|Bool|Duration|Float64|StringSlice|StringArray|IntSlice)Var[P]?\(&?[A-Za-z0-9_.]+, "[a-zA-Z0-9._-]+"' "${operator_paths[@]}")"
new_chart_keys="$(new_chart_values)"

# A failed scan counts as "something to review" for the same reason it is not an
# empty section: nobody can say the Event surface is unchanged until it runs. A
# refusal from an operator surface counts for the same reason, which is why the
# test is on the section bodies rather than on a separate error flag.
if [[ -z "$changed" && -z "$new_event_reasons" && -z "$reason_error" &&
	-z "$new_metric_names" && -z "$new_cli_flags" && -z "$new_chart_keys" ]]; then
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
section "New Event reasons" "$(event_reason_body)"
section "New label and annotation keys" "$(new_values '=[[:space:]]*"(actions-gateway\.com|actions-gateway\.github\.com)/')"
section "New metric names" "$new_metric_names"
section "New CLI flags" "$new_cli_flags"
section "New chart values" "$new_chart_keys"

echo
echo "Everything above is published for the first time by a release cut from HEAD."
echo "Record the verdict in the release plan doc; file deferrals with the release's gate label."
