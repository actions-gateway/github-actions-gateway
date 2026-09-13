#!/usr/bin/env bash
#
# release-delta.sh — report what has accumulated since the last stable release,
# so "is there enough here to justify a tag?" is answered from the record rather
# than from memory.
#
# Usage:
#   scripts/release/release-delta.sh [FROM] [TO]
#
# FROM defaults to the highest stable (non-RC) `v*` tag; TO defaults to
# `origin/main`, falling back to HEAD when no such ref exists locally.
#
# Everything it prints is derived from disciplines the repo already enforces —
# Conventional Commit subjects, and a delete-on-done item store whose every
# mutation is a commit under docs/queue/ — so there is no recording step to
# keep current:
#
#   - commits by Conventional Commit type, with breaking changes called out;
#   - Queue rows closed in the window, read as the deletion of each row's file
#     (the store erases delivered work by design, so this is the only view of
#     it), with the verb the deleting commit recorded beside each id;
#   - the API-surface diffstat, which is the semver signal;
#   - the operator-visible docs/operations/ pages touched.
#
# This is the delta-out half of release decision support: it reports what exists
# and answers "should a release be scoped at all?" Once a release IS scoped, the
# scope-in view takes over — the release plan doc's scope ledger and the `-gate`
# labels answer "is it done?" (maintaining-backlog.md § Cutting a release).
#
# It is a report, not a gate: exit status is 0 whether or not anything
# accumulated. The triggers that turn it into a decision are in
# docs/operations/release.md § When to cut.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Trees whose contents are a published wire contract — kept in step with
# api-surface-since.sh, which reviews the same surface in detail.
API_PATHS=(
	"api"
	"cmd/agc/api"
	"cmd/gmc/api"
	"cmd/agc/config/crd"
	"cmd/gmc/config/crd"
)

# The item store, and the ledger a retiring commit writes beside a deleted
# flake-watch row. Both are paths in the repo under analysis.
STORE_DIR="docs/queue"
FLAKE_LEDGER="docs/development/flake-watch-retired.md"

# The closure verb comes from queue.py, which already classifies it, rather than
# from a second copy of its verb table here. Resolved from this script's own
# location and not from the analysed repo's root, which a test suite scopes to a
# throwaway repo (backlog-metrics.sh resolves its reporter the same way).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUEUE_PY="$SCRIPT_DIR/../docs/queue.py"

for arg in "$@"; do
	case "$arg" in
	-h | --help)
		awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"
		exit 0
		;;
	esac
done

from="${1:-}"
to="${2:-}"

if [[ -z "$from" ]]; then
	# Stable tags only: an RC is a step inside a release, not the last one.
	from="$(git tag --list 'v*' --sort=-v:refname | grep -v -- '-' | head -1)"
	[[ -n "$from" ]] || {
		echo "release-delta: no stable v* tag found; pass FROM explicitly" >&2
		exit 1
	}
fi

if [[ -z "$to" ]]; then
	if git rev-parse --verify --quiet origin/main >/dev/null; then
		to="origin/main"
	else
		to="HEAD"
	fi
fi

for ref in "$from" "$to"; do
	git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || {
		echo "release-delta: '$ref' is not a commit-ish in this repo" >&2
		exit 1
	}
done

range="$from..$to"

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

# --- commits by type ---------------------------------------------------------

subjects="$(git log --no-merges --format='%s' "$range")"
total="$(printf '%s' "$subjects" | grep -c '' || true)"

# User-visible types first: those are what a release note is made of, and the
# rest is churn that does not by itself justify a tag.
type_counts="$(printf '%s\n' "$subjects" | awk '
	BEGIN { split("feat fix perf refactor test build ci chore docs style revert", order, " ") }
	{
		if (match($0, /^[a-z]+(\([^)]*\))?!?:/)) {
			t = substr($0, 1, RLENGTH); sub(/[(!:].*/, "", t)
		} else {
			t = "(non-conventional)"
		}
		count[t]++
	}
	END {
		for (i = 1; i <= length(order); i++) {
			t = order[i]
			if (t in count) { printf "%6d  %s\n", count[t], t; delete count[t] }
		}
		for (t in count) printf "%6d  %s\n", count[t], t
	}')"

# `!` in the subject prefix, or a BREAKING CHANGE trailer in the body.
breaking="$(
	{
		printf '%s\n' "$subjects" | grep -E '^[a-z]+(\([^)]*\))?!:' || true
		git log --no-merges --format='%s' --grep='^BREAKING[ -]CHANGE' "$range" || true
	} | sort -u
)"

# --- Queue rows closed -------------------------------------------------------

# A delete-on-done store records a delivered row as the deletion of its file, so
# one --diff-filter=D walk over the store IS the closure list. Two removals are
# not deliveries and come off:
#
#   - a flake-watch retirement. Retiring a soaked row deletes it and writes a
#     ledger line naming it in the same commit; the delivery was the earlier fix
#     PR, which only parked the row, and crediting it here would bill this
#     release for work an earlier one shipped.
#   - a row resurrected by a bad merge resolution and re-dropped. That is one
#     delivery and not two, which under the store is just "absent at TO".
#
# Parking needs no subtraction of its own, which the STATUS.md table did require:
# a parked row is a `status:` edit, so its file survives and it never shows up as
# a deletion at all.

# Matched as a ledger table row with the id in its first cell, not as a bare id
# anywhere on the line: a prose edit to the ledger names ids it is not retiring
# (the commit rewording Q982's entry closed an unrelated row in the same diff).
retired_ids="$(git log --format='' --unified=0 -p "$range" -- "$FLAKE_LEDGER" |
	awk '/^\+\|[[:space:]]*Q[0-9]+[[:space:]]*\|/ {
		match($0, /Q[0-9]+/); print substr($0, RSTART, RLENGTH)
	}' | sort -u | tr '\n' ' ')"

# Oldest first, so the first sighting of an id is its earliest removal — the
# commit that delivered it — and a later re-drop is discarded by `seen`.
deletions="$(git log --reverse --no-renames --diff-filter=D --name-only \
	--format=$'\x01%s' "$range" -- "$STORE_DIR" |
	awk '
		# \001 as an octal escape, not \x01: hex escapes in a regex are a GNU
		# extension and this runs under whatever awk the host ships.
		substr($0, 1, 1) == "\001" { subject = substr($0, 2); next }
		{
			n = split($0, part, "/")
			id = part[n]
			sub(/\.md$/, "", id)
			if (id ~ /^Q[0-9]+$/) print id "\t" subject
		}')"

# Every id still in the store at TO, whatever its history in the window.
alive_ids="$(git ls-tree -r --name-only "$to" -- "$STORE_DIR" |
	awk '{ n = split($0, part, "/"); id = part[n]; sub(/\.md$/, "", id)
	       if (id ~ /^Q[0-9]+$/) print id }' | sort -u | tr '\n' ' ')"

# `queue.py metrics --events` replays from HEAD rather than from TO, so a row
# closed between the two has no verb to read. It prints as `-` and is counted,
# rather than being dropped or silently shown as an unclassified removal.
closure_verbs=""
if [[ -n "$deletions" && -d "$STORE_DIR" && -f "$QUEUE_PY" ]]; then
	closure_verbs="$(python3 "$QUEUE_PY" metrics --events |
		awk -F'\t' 'NR > 1 && $5 != "open" { print $1 ":" $5 }' | tr '\n' ' ')"
fi

closed_rows="$(printf '%s\n' "$deletions" | awk -F'\t' \
	-v alive="$alive_ids" -v retired="$retired_ids" -v verbs="$closure_verbs" '
	BEGIN {
		n = split(alive, a, " "); for (i = 1; i <= n; i++) still[a[i]] = 1
		n = split(retired, r, " "); for (i = 1; i <= n; i++) parked[r[i]] = 1
		n = split(verbs, v, " ")
		for (i = 1; i <= n; i++) { split(v[i], kv, ":"); verb[kv[1]] = kv[2] }
	}
	NF && !($1 in still) && !($1 in parked) && !seen[$1]++ {
		if ($1 in verb) { printf "%-7s %-9s %s\n", $1, verb[$1], $2 }
		else { printf "%-7s %-9s %s\n", $1, "-", $2; unread++ }
	}
	END {
		if (unread) {
			printf "\n(%d row(s) above show - for the verb: closed beyond "\
			       "HEAD, so the verb replay could not reach them.)\n", unread
		}
	}')"

# --- surface diffstats -------------------------------------------------------

# diffstat_for PATHS… — `git diff --stat` restricted to the paths that exist,
# so a tree added after FROM does not abort the run and an empty path list
# cannot silently widen the diff to the whole repo.
diffstat_for() {
	local existing=() path
	for path in "$@"; do
		[[ -e "$path" ]] && existing+=("$path")
	done
	((${#existing[@]})) || return 0
	git diff --stat "$range" -- "${existing[@]}" | awk 'NF'
}

api_stat="$(diffstat_for "${API_PATHS[@]}")"
ops_stat="$(diffstat_for docs/operations)"

# --- commit-type counts ------------------------------------------------------

count_of() { printf '%s\n' "$type_counts" | awk -v t="$1" '$2 == t { print $1; found = 1 } END { if (!found) print 0 }'; }

feats="$(count_of feat)"
fixes="$(count_of fix)"
perfs="$(count_of perf)"
breaking_count="$(printf '%s' "$breaking" | grep -c '' || true)"

# --- report ------------------------------------------------------------------

echo "Release delta $range"
echo "$total commits (merges excluded)"

section "Commits by type" "$type_counts"
section "Breaking changes" "$breaking"
section "Queue rows closed" "$closed_rows"
section "API surface (semver signal; review with scripts/release/api-surface-since.sh $from)" "$api_stat"
section "Operator-facing docs (curate notes with scripts/release/operator-caveats-since.sh $from)" "$ops_stat"

echo
if ((feats == 0 && fixes == 0 && perfs == 0)); then
	echo "Commit-type counts: no feat/fix/perf commits in this window."
else
	echo "Commit-type counts: $feats feat, $fixes fix, $perfs perf."
fi
# Subject counts, and a subject does not say whether a commit reaches an image
# or a chart: dev-tooling, CI, and docs commits carry the same types. The bump
# the merged work actually forces is derived from the paths, so it lives in
# semver-floor.sh rather than here.
echo "  Subject counts, not what ships. For the bump the merged work forces:"
echo "  scripts/release/semver-floor.sh $from"
if ((breaking_count > 0)); then
	echo "  $breaking_count breaking-marked commit(s) above. semver-floor.sh reports each as an"
	echo "  unresolved major and narrows it against the CRD surface $from published;"
	echo "  scripts/release/api-surface-since.sh $from is where the rest of that is settled."
fi
echo "Whether that is enough to cut: docs/operations/release.md § When to cut."
