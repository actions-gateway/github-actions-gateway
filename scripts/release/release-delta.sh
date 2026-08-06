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
# Conventional Commit subjects, and a delete-on-done Queue whose every mutation
# is a commit to docs/STATUS.md — so there is no recording step to keep current:
#
#   - commits by Conventional Commit type, with breaking changes called out;
#   - Queue rows closed in the window (the delete-on-done Queue erases delivered
#     work from STATUS.md by design, so this is the only view of it);
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

STATUS_FILE="docs/STATUS.md"

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

# ids_at REV SECTION — the Q-IDs listed in one `## ` section of STATUS.md at REV.
# Reading each revision's file beats replaying the diff: a diff line carries no
# section context, so a row MOVED from Queue to Deferred is indistinguishable
# from one deleted outright, and only the first of those is delivered work.
ids_at() {
	local rev="$1" want="$2"
	{ git show "$rev:$STATUS_FILE" 2>/dev/null || true; } |
		awk -v want="$want" '
			/^## / { sec = substr($0, 4); next }
			sec == want && /^\| *<a id="Q[0-9]+"><\/a>Q/ {
				match($0, /id="Q[0-9]+"/)
				print substr($0, RSTART + 4, RLENGTH - 5)
			}' | sort -u
}

closed=""
for commit in $(git rev-list --reverse "$range" -- "$STATUS_FILE"); do
	gone="$(comm -23 <(ids_at "$commit^" Queue) <(ids_at "$commit" Queue))"
	[[ -n "$gone" ]] || continue
	# A row parked rather than finished lands in Deferred in the same commit.
	parked="$(ids_at "$commit" Deferred)"
	gone="$(comm -23 <(printf '%s\n' "$gone") <(printf '%s\n' "$parked"))"
	[[ -n "$gone" ]] || continue
	subject="$(git log -1 --format='%s' "$commit")"
	closed+="$(awk -v s="$subject" 'NF { print $0 "\t" s }' <<<"$gone")"$'\n'
done

# Drop IDs back in the Queue at TO (a row resurrected by a bad merge resolution
# and re-dropped appears twice), and keep each ID's earliest removal — the
# commit that delivered it.
closed_rows="$(printf '%s' "$closed" | awk -F'\t' -v open_ids="$(ids_at "$to" Queue | tr '\n' ' ')" '
	BEGIN { n = split(open_ids, a, " "); for (i = 1; i <= n; i++) still[a[i]] = 1 }
	NF && !($1 in still) && !seen[$1]++ { printf "%-6s %s\n", $1, $2 }')"

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
