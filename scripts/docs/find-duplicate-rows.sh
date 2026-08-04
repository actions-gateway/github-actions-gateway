#!/usr/bin/env bash
#
# find-duplicate-rows.sh — surface backlog rows that look like near-duplicates
# of a row about to be filed.
#
# The "search before you file" rule in docs/development/maintaining-backlog.md
# has been tripped three times: Q442 and Q456 both duplicated Q440, and Q635
# duplicated Q619. Every one satisfied the lint (a semantic duplicate is
# well-formed), and every one was filed mid-task, when the doc carrying the rule
# was not in context. `make queue-id` is the one chokepoint every row passes
# through, so the search runs there.
#
# What the matcher keys on comes from those pairs, not from a guess:
#
#   Q456 "The GMC CRD manifests are stale and no gate notices"
#   Q440 "GMC CRD manifest drifts from the AGC types it embeds"
#        -> 3 shared content words, and the SAME Item-cell link target.
#   Q635 "`doc-links` never reads a new doc's own links until it is staged..."
#   Q619 "Three gates scan tracked files only, so a new file misses its own..."
#        -> 4 shared content words, and the same Item-cell link target.
#   Q511 "Two live-GitHub runs collide invisibly, and a killed one poisons..."
#   Q500 "Two concurrent live-GitHub runs collide on the fixture repo"
#        -> 5 shared content words, different targets.
#
# So neither signal alone covers the evidence: two pairs agreed on the target
# and barely on the words, one agreed on the words and not the target. The rule
# below is their union, with a shared-word floor that a threshold ratio alone
# does not provide — containment divides by the *shorter* title, so a five-word
# row scores 0.40 on two incidental words.
#
# Deferred and Flake watch are searched too. A row duplicating a parked item is
# the same mistake, and the parked tables are exactly the ones nobody greps.
#
# Notes cells are deliberately NOT matched. Containment normalises by the
# shorter side, so folding a ~250-char Notes cell into the row's token set can
# only raise every score; it inflates the ranking without adding a cut.
#
# Advisory by construction: this always exits 0 and never blocks a filing. The
# filer routinely knows something the matcher cannot — that two rows sharing a
# file are genuinely separate defects, say.
#
# Usage:
#   find-duplicate-rows.sh "<the row title you are about to file>"
#   find-duplicate-rows.sh --target <item-link> "<title>"
#   find-duplicate-rows.sh --file <path> "<title>"
#   find-duplicate-rows.sh --audit    # score every existing pair; how noisy is it?
#
# Calibration against the shipped backlog: docs/development/maintaining-backlog.md

set -euo pipefail

# Flag a row on either signal:
#   text alone      — >= MIN_SHARED shared content words AND containment >= MIN_SCORE
#   same link target — a lower bar on both, since the target is itself evidence
#
# Measured over the 119 shipped Queue + Deferred + Flake-watch rows: 9 of 7,021
# pairs flag, ~0.15 candidates per row filed. Loosening either ratio by 0.05
# roughly doubles it. See maintaining-backlog.md for the full table.
readonly MIN_SHARED=3
readonly MIN_SCORE=0.40
readonly TARGET_MIN_SHARED=2
readonly TARGET_MIN_SCORE=0.25

die() {
	printf 'find-duplicate-rows: %s\n' "$*" >&2
	exit 1
}

usage() {
	awk '/^# Usage:/,/^$/ { sub(/^#[[:space:]]?/, ""); print }' "$0"
}

# Rank every Queue/Deferred/Flake-watch row against the title in $QUERY, emitting
# "score<TAB>ID<TAB>table<TAB>target-flag<TAB>title" for each row over the bar,
# unsorted. With $LIST_ONLY set it emits "ID<TAB>target<TAB>title" for every row
# instead and scores nothing — the audit needs the same row parsing, and a second
# copy of it would be free to drift out of agreement with this one.
score_rows() {
	local file=$1
	awk \
		-v min_shared="$MIN_SHARED" -v min_score="$MIN_SCORE" \
		-v tgt_shared="$TARGET_MIN_SHARED" -v tgt_score="$TARGET_MIN_SCORE" '
	function norm(s,   t) {
		t = tolower(s)
		gsub(/`/, " ", t)
		gsub(/[^a-z0-9]+/, " ", t)
		return t
	}
	# Crude singular stem. "manifests"/"manifest" and "gates"/"gate" are the
	# shapes the real pairs differed by; anything deeper needs a stemmer.
	function stem(w) {
		if (length(w) > 3 && w ~ /s$/ && w !~ /ss$/) return substr(w, 1, length(w) - 1)
		return w
	}
	# Content words of s, into out[]. Returns the count.
	function tokenize(s, out,   n, w, i, x, k) {
		n = split(norm(s), w, " ")
		k = 0
		for (i = 1; i <= n; i++) {
			x = stem(w[i])
			if (length(x) < 3 || (x in stop)) continue
			if (!(x in out)) { out[x] = 1; k++ }
		}
		return k
	}
	# Item-cell link target, anchor stripped: two rows about one defect point at
	# one file even when they describe it in different words.
	function link_target(cell,   t) {
		if (!match(cell, /\]\([^)]*\)/)) return ""
		t = substr(cell, RSTART + 2, RLENGTH - 3)
		sub(/#.*/, "", t)
		return t
	}
	function link_text(cell,   t) {
		t = cell
		gsub(/\]\([^)]*\)/, " ", t)
		gsub(/\[/, " ", t)
		return t
	}
	BEGIN {
		split("the a an and or but so of to on at for from with by is are was were be been " \
		      "it its that this these those not only every each all any than then when where " \
		      "what which how why more most other such into over under out up down off no nor " \
		      "as if does do did has have had can could should would will", sw, " ")
		for (i in sw) stop[sw[i]] = 1
		list_only = (ENVIRON["LIST_ONLY"] != "")
		qn = tokenize(ENVIRON["QUERY"], qtok)
		qtarget = ENVIRON["QUERY_TARGET"]
		sub(/#.*/, "", qtarget)
	}
	/^## Queue/        { table = "Queue";       next }
	/^## Deferred/     { table = "Deferred";    next }
	/^### Flake watch/ { table = "Flake watch"; next }
	# Any other section — Progress above all, whose anchors are plan rows rather
	# than items (Q509) — takes the matcher out of scope until the next heading.
	/^## /             { table = "";            next }
	table == "" { next }
	# A Queue/Deferred row: anchor immediately followed by the visible ID.
	/^\| <a id="Q[0-9]+"><\/a>Q[0-9]+ \|/ {
		match($0, /id="Q[0-9]+"/)
		id = substr($0, RSTART + 4, RLENGTH - 5)
		split($0, cell, /\|/)
		title = link_text(cell[3])
		gsub(/^[[:space:]]+|[[:space:]]+$/, "", title)

		if (list_only) {
			printf "%s\t%s\t%s\n", id, link_target(cell[3]), title
			next
		}

		delete rtok
		rn = tokenize(title, rtok)
		if (rn == 0 || qn == 0) next

		shared = 0
		for (x in qtok) if (x in rtok) shared++
		smaller = (qn < rn) ? qn : rn
		score = shared / smaller
		same_target = (qtarget != "" && qtarget == link_target(cell[3]))

		if (shared >= min_shared && score >= min_score)
			hit = 1
		else if (same_target && shared >= tgt_shared && score >= tgt_score)
			hit = 1
		else
			hit = 0
		if (!hit) next

		printf "%.2f\t%s\t%s\t%s\t%s\n", score, id, table, (same_target ? "same target" : ""), title
	}
	' "$file"
}

# --audit: run every shipped row through the matcher as if it were being filed.
# The threshold constants are a claim about noise, and a claim about the backlog
# goes stale as the backlog grows — this is how it gets re-measured rather than
# re-asserted. Symmetric hits collapse to one line.
audit() {
	local file=$1 id target title rows=0 flagged=0 hit_id
	while IFS=$'\t' read -r id target title; do
		rows=$((rows + 1))
		while IFS=$'\t' read -r _ hit_id _; do
			# One line per unordered pair, and never a row against itself.
			[[ "$hit_id" < "$id" ]] || continue
			flagged=$((flagged + 1))
			printf '%s\t%s\n' "$id" "$hit_id"
		done < <(QUERY="$title" QUERY_TARGET="$target" score_rows "$file")
	done < <(LIST_ONLY=1 score_rows "$file")
	printf 'rows=%d pairs=%d flagged=%d\n' \
		"$rows" "$((rows * (rows - 1) / 2))" "$flagged"
}

main() {
	local query='' target='' file='' mode=search

	while (($# > 0)); do
		case "$1" in
		--audit)
			mode=audit
			shift
			;;
		--target)
			target=${2:-}
			[[ -n "$target" ]] || die '--target wants a link'
			shift 2
			;;
		--file)
			file=${2:-}
			[[ -n "$file" ]] || die '--file wants a path'
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		-*) die "unknown option: $1" ;;
		*)
			[[ -z "$query" ]] || die 'one title per invocation; got a second positional argument'
			query=$1
			shift
			;;
		esac
	done

	[[ "$mode" == audit || -n "$query" ]] ||
		die 'wants the title of the row you are about to file'
	[[ -n "$file" ]] || file="$(git rev-parse --show-toplevel)/docs/STATUS.md"
	# A missing backlog is not an error here: this runs inside ID allocation,
	# which already tolerates one (a fresh clone, a detached scratch tree).
	[[ -f "$file" ]] || return 0

	if [[ "$mode" == audit ]]; then
		audit "$file"
		return 0
	fi

	local hits
	hits=$(QUERY="$query" QUERY_TARGET="$target" score_rows "$file" | sort -rn)
	[[ -n "$hits" ]] || return 0

	printf 'Possible near-duplicates of "%s" — advisory, nothing is blocked:\n\n' "$query"
	printf '%s\n' "$hits" |
		awk -F'\t' '{ printf "  %-6s %-12s %s  %s\n", $2, $3, $1, ($4 == "" ? "" : "[" $4 "] ") $5 }'
	printf '\nRead those rows before filing. If yours is genuinely separate, say so in its Notes:\n'
	printf 'docs/development/maintaining-backlog.md#search-before-you-file\n'
}

main "$@"
