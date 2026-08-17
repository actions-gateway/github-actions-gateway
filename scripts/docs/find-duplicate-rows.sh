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
# Deferred and flake-watch items are searched too. A row duplicating a parked
# item is the same mistake, and parked items are exactly the ones nobody greps.
# They are `status: deferred` in the store; a `flake` label separates the two,
# the same split `migrate` made when the tables became one directory.
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
#   find-duplicate-rows.sh "<the item title you are about to file>"
#   find-duplicate-rows.sh --target <item-link> "<title>"
#   find-duplicate-rows.sh --store <dir> "<title>"
#   find-duplicate-rows.sh --audit    # score every existing pair; how noisy is it?
#
# Calibration against the shipped backlog: docs/development/maintaining-backlog.md

set -euo pipefail
shopt -s inherit_errexit

# Flag a row on either signal:
#   text alone      — >= MIN_SHARED shared content words AND containment >= MIN_SCORE
#   same link target — a lower bar on both, since the target is itself evidence
#
# Measured on the shipped backlog: roughly one row in five surfaces a candidate,
# and every flagged pair is topically adjacent (2026-08-04: 11 of 6,903 pairs
# over 118 rows). Loosening either ratio by 0.05 roughly doubles it. Re-measure
# with --audit; the method and the pairs it keys on are in maintaining-backlog.md.
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

# Read the store into "ID<TAB>target<TAB>title<TAB>section" once. Every consumer
# below re-reads this file rather than the 174 item files, because --audit runs
# the matcher once per row and re-parsing the store each time is quadratic.
#
# The section label is derived, not stored: an item is deferred or not, and a
# deferred one carrying `flake` is what the table called Flake watch. That is
# the same split `migrate` made when three tables became one directory.
store_rows() {
	local store=$1
	awk '
	function flush() {
		if (id == "") return
		printf "%s\t%s\t%s\t%s\n", id, target, title, \
			(status == "deferred" ? (flake ? "Flake watch" : "Deferred") : "Queue")
	}
	FNR == 1 { flush(); id = ""; target = ""; title = ""; status = ""; flake = 0; fm = 0 }
	FNR == 1 && /^---$/ { fm = 1; next }
	fm && /^---$/ { fm = 0; next }
	fm && /^id:/     { id = $2 }
	fm && /^target:/ { target = $2 }
	fm && /^status:/ { status = $2 }
	fm && /^[[:space:]]*-[[:space:]]*flake[[:space:]]*$/ { flake = 1 }
	fm && /^labels:.*flake/ { flake = 1 }
	!fm && title == "" && /^# / { title = substr($0, 3) }
	END { flush() }
	' "$store"/Q*.md
}

# Rank every item against the title in $QUERY, emitting
# "score<TAB>ID<TAB>section<TAB>target-flag<TAB>title" for each over the bar,
# unsorted. With $LIST_ONLY set it emits "ID<TAB>target<TAB>title" for every item
# instead and scores nothing — the audit needs the same reading, and a second
# copy of it would be free to drift out of agreement with this one.
score_rows() {
	local rows=$1
	awk -F'\t' \
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
	# Two items about one defect point at one file even when they describe it in
	# different words. The anchor a table row carried is gone; the store rebases
	# every target one directory down, uniformly, so equality between two items
	# survives the move even though the strings changed.
	function bare(t) { sub(/#.*/, "", t); return t }
	BEGIN {
		split("the a an and or but so of to on at for from with by is are was were be been " \
		      "it its that this these those not only every each all any than then when where " \
		      "what which how why more most other such into over under out up down off no nor " \
		      "as if does do did has have had can could should would will", sw, " ")
		for (i in sw) stop[sw[i]] = 1
		list_only = (ENVIRON["LIST_ONLY"] != "")
		qn = tokenize(ENVIRON["QUERY"], qtok)
		qtarget = bare(ENVIRON["QUERY_TARGET"])
	}
	{
		id = $1; target = $2; title = $3; table = $4

		if (list_only) {
			printf "%s\t%s\t%s\n", id, target, title
			next
		}

		delete rtok
		rn = tokenize(title, rtok)
		if (rn == 0 || qn == 0) next

		shared = 0
		for (x in qtok) if (x in rtok) shared++
		smaller = (qn < rn) ? qn : rn
		score = shared / smaller
		same_target = (qtarget != "" && qtarget == bare(target))

		if (shared >= min_shared && score >= min_score)
			hit = 1
		else if (same_target && shared >= tgt_shared && score >= tgt_score)
			hit = 1
		else
			hit = 0
		if (!hit) next

		printf "%.2f\t%s\t%s\t%s\t%s\n", score, id, table, (same_target ? "same target" : ""), title
	}
	' "$rows"
}

# --audit: run every shipped row through the matcher as if it were being filed.
# The threshold constants are a claim about noise, and a claim about the backlog
# goes stale as the backlog grows — this is how it gets re-measured rather than
# re-asserted. Symmetric hits collapse to one line.
audit() {
	local rows_file=$1 id target title rows=0 flagged=0 hit_id
	while IFS=$'\t' read -r id target title; do
		rows=$((rows + 1))
		while IFS=$'\t' read -r _ hit_id _; do
			# One line per unordered pair, and never a row against itself.
			[[ "$hit_id" < "$id" ]] || continue
			flagged=$((flagged + 1))
			printf '%s\t%s\n' "$id" "$hit_id"
		done < <(QUERY="$title" QUERY_TARGET="$target" score_rows "$rows_file")
	done < <(LIST_ONLY=1 score_rows "$rows_file")
	printf 'rows=%d pairs=%d flagged=%d\n' \
		"$rows" "$((rows * (rows - 1) / 2))" "$flagged"
}

main() {
	local query='' target='' store='' mode=search

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
		--store)
			store=${2:-}
			[[ -n "$store" ]] || die '--store wants a directory'
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
	[[ -n "$store" ]] || store="$(git rev-parse --show-toplevel)/docs/queue"
	# A missing backlog is not an error here: this runs inside ID allocation,
	# which already tolerates one (a fresh clone, a detached scratch tree).
	# An empty directory is the same case and must take the same exit, or a
	# fresh clone starts failing its first `make queue-id`.
	[[ -d "$store" ]] || return 0
	local rows_file
	rows_file="$(mktemp)"
	trap 'rm -f "$rows_file"' RETURN
	store_rows "$store" >"$rows_file" 2>/dev/null || true
	[[ -s "$rows_file" ]] || return 0

	if [[ "$mode" == audit ]]; then
		audit "$rows_file"
		return 0
	fi

	local hits
	hits=$(QUERY="$query" QUERY_TARGET="$target" score_rows "$rows_file" | sort -rn)
	[[ -n "$hits" ]] || return 0

	printf 'Possible near-duplicates of "%s" — advisory, nothing is blocked:\n\n' "$query"
	printf '%s\n' "$hits" |
		awk -F'\t' '{ printf "  %-6s %-12s %s  %s\n", $2, $3, $1, ($4 == "" ? "" : "[" $4 "] ") $5 }'
	printf '\nRead those rows before filing. If yours is genuinely separate, say so in its Notes:\n'
	printf 'docs/development/maintaining-backlog.md#search-before-you-file\n'
}

main "$@"
