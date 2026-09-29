#!/usr/bin/env bash
#
# check-release-ladder.sh — bind the release ladder's punted table to the
# backlog status of the items it names (Q932), and each rung's scope ledger to
# the `X.Y-gate` labels (Q1087). The page is
# docs/plan/release-ladder.md  no-plan-refs: it is this gate's subject, so naming it is the point
#
# The page partitions seven items: a table of what is punted past `v2.0.0`,
# every entry of which claims to be Deferred with a revive trigger, and a
# paragraph naming the ones whose triggers have since fired. Both halves are
# claims about `status:` in the item store, and nothing read either.
# `check-plan-index` is the neighbouring gate and does not cover this: its
# invariants bind a Status cell to a row *existing* (`[[ -f "$store/$id.md" ]]`),
# never to what that row's status says. So a revived item stays in the punted
# table indefinitely — Q408 and Q564 sat there for five days after both came
# back on 2026-08-13, and the page went on calling them punted.
#
# Three assertions, two of them directions of each other:
#   1. Every item the punted table names is `status: deferred`. This is the
#      five-day defect.
#   2. Every item the revived paragraph names is NOT deferred. The same claim
#      pointed the other way, and the half that decays quietest: re-parking an
#      item is a one-word edit in its own file, nowhere near this page.
#   3. The prose counts agree with what the sections hold — the punted count,
#      the revived count, and their sum against the stated original total. The
#      page states all three in words, so each is a claim that outlives the
#      edit that falsified it.
#
# The revived paragraph may legitimately name nobody, once the last revived item
# ships. That is spelled "None of the original N are back", and only the spelled
# count makes the emptiness legal: a page naming no ID while still claiming one
# has drifted, which is the case the emptiness check exists for.
#
# Every count reads `is|are`, and so does the marker that finds the paragraph at
# all. A section down to one item takes the singular, and pinning the plural left
# "One of the original M are back" as the only sentence this gate would accept
# (Q965). The verb is still required rather than wildcarded: dropping it must
# refuse, so a rewrite cannot quietly take the count out of the gate's reach.
#
# Assertion 1 alone would pass a tree where every punted row is correctly
# deferred and the revived paragraph still named one of them.
#
# Two more bind each rung's plan to the `X.Y-gate` labels (Q1087), again each
# the other's direction:
#   4. An item labelled `X.Y-gate` is marked `X.Y-gate` in the scope ledger of
#      the plan the ladder's X.Y rung links. A label with no rung, a rung with no
#      plan, and a plan with no ledger all fail rather than pass unread.
#   5. Every row a scope ledger marks as gating carries that label, and a ledger
#      marks only its own rung's label. A closed row has no item file and is
#      skipped, since the ledger keeps its Q-ID once it ships. A cell naming a
#      gate with anything else in it fails, since read loosely it would drop a
#      gating row out of this check.
# "Names as gating" is the ledger's `Gates?` column, which every release plan
# already carries by convention; the rest of a plan mentions many rows.
#
# Usage:
#   check-release-ladder.sh [--page PATH] [--store PATH]
#
# Exits 1 on a finding, and 2 when a section it reads is absent, empty, or a
# scope ledger lacks its `Gates?` column — a
# page whose shape moved must not report every claim in it verified.
# File-wide: the patterns below are awk source and markdown text, so a `$` in
# one is a literal the page or the parser owns, not a shell expansion.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

PAGE="docs/plan/release-ladder.md"
STORE="docs/queue"

while (($# > 0)); do
	case "$1" in
	--page)
		PAGE="$2"
		shift
		;;
	--store)
		STORE="$2"
		shift
		;;
	*)
		printf 'check-release-ladder.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

for p in "$PAGE" "$STORE"; do
	if [[ ! -e "$p" ]]; then
		printf 'release-ladder: %s does not exist, so this gate would verify nothing\n' "$p" >&2
		exit 2
	fi
done

# The heading each section is read from. Matched on the heading text rather than
# a line number so ordinary edits above them cannot shift the read.
PUNTED_HEADING='## What is punted past'
# `is|are` for the same reason the counts below take it: a single revived item
# takes a singular verb, and pinning the plural here made the paragraph itself
# unfindable, so the count fix alone would have left the gate refusing (Q965).
REVIVED_MARKER='(is|are) back\.\*\*'

# The Q-IDs inside the punted table: the rows between its heading and the next
# heading, table rows only, so the prose around it cannot contribute an ID.
punted_ids() {
	awk -v heading="$PUNTED_HEADING" '
		index($0, heading) == 1 { in_section = 1; next }
		in_section && /^## / { exit }
		in_section && /^\|/ {
			line = $0
			while (match(line, /Q[0-9]+/)) {
				print substr(line, RSTART, RLENGTH)
				line = substr(line, RSTART + RLENGTH)
			}
		}
	' "$PAGE" | sort -u
}

# The Q-IDs in the revived paragraph, up to its first colon. That boundary is
# the prose's own: the clause before it names the items whose triggers fired,
# and the clause after gives the evidence they fired on, which cites other
# items. Reading the whole paragraph collects the evidence too — Q725 is named
# there as the demand behind Q564's trigger, and is neither punted nor revived.
revived_ids() {
	awk -v marker="$REVIVED_MARKER" '
		$0 ~ marker {
			line = $0
			i = index(line, ":")
			if (i > 0) line = substr(line, 1, i - 1)
			while (match(line, /Q[0-9]+/)) {
				print substr(line, RSTART, RLENGTH)
				line = substr(line, RSTART + RLENGTH)
			}
		}
	' "$PAGE" | sort -u
}

# The page spells its counts, so they are read through a fixed map rather than
# by digit. An unmapped word is a refusal, not a zero.
word_to_number() {
	case "$1" in
	none | zero) printf '0' ;;
	one) printf '1' ;;
	two) printf '2' ;;
	three) printf '3' ;;
	four) printf '4' ;;
	five) printf '5' ;;
	six) printf '6' ;;
	seven) printf '7' ;;
	eight) printf '8' ;;
	nine) printf '9' ;;
	ten) printf '10' ;;
	*) return 1 ;;
	esac
}

mapfile -t punted < <(punted_ids)
mapfile -t revived < <(revived_ids)

if ((${#punted[@]} == 0)); then
	printf 'release-ladder: no Q-ID found in the "%s" table of %s, so this gate would verify nothing\n' \
		"$PUNTED_HEADING" "$PAGE" >&2
	exit 2
fi
# `status:` for one item, or the empty string when the item has no file.
item_status() {
	local f="$STORE/$1.md"
	[[ -f "$f" ]] || return 0
	awk '/^status:[ \t]*/ { sub(/^status:[ \t]*/, ""); print; exit }' "$f"
}

errors=0
fail() {
	printf 'release-ladder: %s\n' "$1" >&2
	((errors++)) || true
}

# 1. Punted means deferred.
for id in "${punted[@]}"; do
	status="$(item_status "$id")"
	if [[ -z "$status" ]]; then
		fail "$PAGE lists $id as punted past v2.0.0, but $STORE/$id.md does not exist
       the item closed or was renamed; drop it from the punted table"
		continue
	fi
	if [[ "$status" != "deferred" ]]; then
		fail "$PAGE lists $id as punted past v2.0.0, but its item is 'status: $status'
       its revive trigger fired, so move it out of the punted table and into the revived paragraph (Q932)"
	fi
done

# 2. Revived means not deferred — assertion 1 pointed the other way.
for id in "${revived[@]}"; do
	status="$(item_status "$id")"
	if [[ -z "$status" ]]; then
		fail "$PAGE names $id as revived, but $STORE/$id.md does not exist
       the item closed; drop it from the revived paragraph and correct the counts"
		continue
	fi
	if [[ "$status" == "deferred" ]]; then
		fail "$PAGE names $id as revived, but its item is parked again ('status: deferred')
       re-parking is a one-word edit in the item file, so this page has to be corrected with it (Q932)"
	fi
done

# 3. The counts the prose states.
# Two-argument match plus substr, not gawk's three-argument form: the awk on a
# macOS dev box is BSD awk, which rejects it outright, and the failure would
# arrive as an empty count rather than as an error.
count_word() {
	awk -v pat="$1" -v pre="$2" '
		match($0, pat) {
			seg = substr($0, RSTART, RLENGTH)
			sub(pre, "", seg)
			match(seg, /[A-Za-z]+/)
			print substr(seg, RSTART, RLENGTH)
			exit
		}
	' "$PAGE"
}

# Bracket expressions rather than backslash escapes: these regexes reach awk as
# string variables, and BSD awk drops the backslash while parsing the string, so
# `\(` arrives as a bare `(` and is rejected as an illegal primary. A character
# class needs no escaping in either awk.
# `is|are` in all three: a section down to a single item takes a singular verb,
# and pinning the plural made the only grammatical sentence one the gate rejected
# (Q965). `(is|are)` rather than a wildcard, so a rewrite that drops the verb
# still fails to match and the refusal below fires rather than the count drifting.
still_word="$(count_word '[(][a-z]+ of them still (is|are)[)]' '[(]')"
back_word="$(count_word '[*][*][A-Za-z]+ of the original [a-z]+ (is|are) back[.][*][*]' '[*][*]')"
total_word="$(count_word 'of the original [a-z]+ (is|are) back' 'of the original ')"

if [[ -z "$still_word" || -z "$back_word" || -z "$total_word" ]]; then
	printf 'release-ladder: %s no longer states its punted/revived/original counts in the expected wording, so they cannot be checked\n' "$PAGE" >&2
	printf 'expected a "(N of them still are)" aside and a "**N of the original M are back.**" sentence\n' >&2
	printf 'a section down to one item takes the singular: "(one of them still is)", "**One of the original M is back.**"\n' >&2
	exit 2
fi

still_n="$(word_to_number "$(tr '[:upper:]' '[:lower:]' <<<"$still_word")")" || {
	printf 'release-ladder: cannot read "%s" as a number in %s\n' "$still_word" "$PAGE" >&2
	exit 2
}
back_n="$(word_to_number "$(tr '[:upper:]' '[:lower:]' <<<"$back_word")")" || {
	printf 'release-ladder: cannot read "%s" as a number in %s\n' "$back_word" "$PAGE" >&2
	exit 2
}
total_n="$(word_to_number "$(tr '[:upper:]' '[:lower:]' <<<"$total_word")")" || {
	printf 'release-ladder: cannot read "%s" as a number in %s\n' "$total_word" "$PAGE" >&2
	exit 2
}

if ((still_n != ${#punted[@]})); then
	fail "$PAGE says $still_word of the punted items are still deferred, but its table names ${#punted[@]}"
fi
# An empty revived paragraph is legal only when the prose says so. Every revived
# item eventually ships, and the last one to do it leaves the paragraph naming
# nobody -- Q408 was that item. Reading the emptiness as a shape change would
# then make the page ungateable at exactly the moment the ladder is working. So
# the declared count decides: a page claiming "None ... are back" has said the
# set is empty, and a page claiming a number while naming no ID has drifted.
if ((${#revived[@]} == 0 && back_n != 0)); then
	printf 'release-ladder: no Q-ID found in the revived paragraph of %s, so half this gate would verify nothing\n' "$PAGE" >&2
	exit 2
fi

if ((back_n != ${#revived[@]})); then
	fail "$PAGE says $back_word of the original set are back, but its revived paragraph names ${#revived[@]}"
fi
if ((${#punted[@]} + ${#revived[@]} != total_n)); then
	fail "$PAGE says the original set was $total_word, but its two sections hold $((${#punted[@]} + ${#revived[@]})) items between them"
fi

# 4 and 5. A gate label and its release plan's scope ledger, each direction of the
# other (Q1087). The rung-to-plan mapping is read off the ladder table's links,
# never templated from the version: the 2.0 rung's plan is not named for 2.0, and
# a template passes every release whose plan is named the way it guesses.
declare -A rung_plan=()
while IFS=$'\t' read -r version link; do
	rung_plan["$version"]="$link"
done < <(awk '
	/^\| \*\*[0-9]+\.[0-9]+\*\* \|/ {
		n = split($0, cell, "|")
		version = cell[2]
		gsub(/[* \t]/, "", version)
		link = ""
		if (match(cell[n - 1], /\]\([^)#]+/)) link = substr(cell[n - 1], RSTART + 2, RLENGTH - 2)
		printf "%s\t%s\n", version, link
	}
' "$PAGE")

# The `X.Y-gate` labels one item's frontmatter carries, one per line. Both YAML
# list forms queue-lint accepts are read, quoted or bare: a block list, and an
# inline `labels: [a, "b"]`. Reading only the block form passes a gate label
# written the other way unbound.
item_gate_labels() {
	awk -v sq="'" '
		function emit(v) {
			gsub(/["]/, "", v)
			gsub(sq, "", v)
			gsub(/^[ \t]+|[ \t]+$/, "", v)
			if (v ~ /^[0-9]+\.[0-9]+-gate$/) print v
		}
		NR == 1 && /^---$/ { in_fm = 1; next }
		in_fm && /^---$/ { exit }
		in_fm && /^[a-z]/ {
			in_labels = ($0 ~ /^labels:/)
			if (in_labels && match($0, /\[.*\]/)) {
				n = split(substr($0, RSTART + 1, RLENGTH - 2), item, ",")
				for (i = 1; i <= n; i++) emit(item[i])
			}
			next
		}
		in_fm && in_labels && /^[ \t]*-/ { v = $0; sub(/^[ \t]*-/, "", v); emit(v) }
	' "$1"
}

# "ID<TAB>gates-cell" for each Q-ID row of a plan's scope ledger. The Gates?
# column is found by its header rather than by position, so a ledger that grows a
# column still reads. Exits 3 when the section exists but no header names it.
ledger_rows() {
	awk '
		/^## Scope ledger/ { in_section = 1; next }
		in_section && /^## / { exit }
		in_section && /^\|/ {
			n = split($0, cell, "|")
			if (!col) {
				for (i = 2; i < n; i++) if (cell[i] ~ /Gates\?/) col = i
				next
			}
			if (!match(cell[2], /Q[0-9]+/)) next
			id = substr(cell[2], RSTART, RLENGTH)
			g = cell[col]
			gsub(/[`* \t]/, "", g)
			printf "%s\t%s\n", id, g
		}
		END { if (in_section && !col) exit 3 }
	' "$1"
}

# The plan file a rung links to, resolved against the ladder page's directory.
rung_plan_path() {
	local link="${rung_plan[$1]-}"
	[[ -n "$link" ]] || return 1
	printf '%s/%s\n' "$(dirname "$PAGE")" "$link"
}

# rung version -> "ID=cell ..." for every ledger row, and the gating subset.
declare -A ledger_cell=()
ledger_gating_n=0
for version in "${!rung_plan[@]}"; do
	plan="$(rung_plan_path "$version")" || continue
	[[ -f "$plan" ]] || continue
	grep -q '^## Scope ledger' "$plan" || continue
	rows_rc=0
	rows="$(ledger_rows "$plan")" || rows_rc=$?
	if ((rows_rc != 0)); then
		printf 'release-ladder: %s has a scope ledger with no "Gates?" column, so nothing in it can be read as gating\n' "$plan" >&2
		exit 2
	fi
	while IFS=$'\t' read -r id cell; do
		[[ -n "$id" ]] || continue
		ledger_cell["$version/$id"]="$cell"
		if [[ "$cell" == *-gate* && ! "$cell" =~ ^[0-9]+\.[0-9]+-gate$ ]]; then
			fail "$plan's scope ledger marks $id as '$cell', which names a gate but is not one label
       a Gates? cell holds exactly one X.Y-gate label, 'rides' or 'gates'; put any qualifier in the Status cell"
			continue
		fi
		[[ "$cell" =~ ^[0-9]+\.[0-9]+-gate$ ]] || continue
		((ledger_gating_n++)) || true
		# 5. A row the ledger names as gating carries that label.
		if [[ "$cell" != "$version-gate" ]]; then
			fail "$plan's scope ledger marks $id as '$cell', but that ledger is the $version rung's
       a ledger can only say what gates its own release; name $id in the $cell plan instead"
			continue
		fi
		f="$STORE/$id.md"
		# A closed row has no file; the ledger keeps its Q-ID with a ✅.
		[[ -f "$f" ]] || continue
		if ! item_gate_labels "$f" | grep -qx -- "$version-gate"; then
			fail "$plan's scope ledger says $id gates $version, but $f does not carry the $version-gate label
       add the label, or mark the ledger row 'rides' if the tag does not wait for it (Q1087)"
		fi
	done <<<"$rows"
done

# 4. A gate-labelled row is named as gating by its release's ledger.
labelled_n=0
for f in "$STORE"/Q*.md; do
	[[ -f "$f" ]] || continue
	id="$(basename "$f" .md)"
	while IFS= read -r label; do
		[[ -n "$label" ]] || continue
		((labelled_n++)) || true
		version="${label%-gate}"
		if [[ -z "${rung_plan[$version]+set}" ]]; then
			fail "$id carries $label, but $PAGE's ladder has no $version rung
       a gate label needs a rung whose plan says what the release waits for"
			continue
		fi
		plan="$(rung_plan_path "$version")" || {
			fail "$id carries $label, but the $version rung in $PAGE links no plan"
			continue
		}
		if [[ ! -f "$plan" ]] || ! grep -q '^## Scope ledger' "$plan"; then
			fail "$id carries $label, but $plan has no '## Scope ledger', so nothing states what $version waits for"
			continue
		fi
		cell="${ledger_cell["$version/$id"]-}"
		if [[ -z "$cell" ]]; then
			fail "$id carries $label, but $plan's scope ledger does not name it
       add a ledger row marking it '$label', or drop the label if the tag does not wait for it (Q1087)"
		elif [[ "$cell" != "$label" ]]; then
			fail "$id carries $label, but $plan's scope ledger marks it '$cell'"
		fi
	done < <(item_gate_labels "$f")
done

if ((errors > 0)); then
	printf '\n%d release-ladder check(s) failed. The punted table, the revived paragraph and each\n' "$errors" >&2
	printf 'rung'"'"'s scope ledger are claims about %s; nothing else reads them.\n' "$STORE" >&2
	exit 1
fi

printf 'release ladder: %d punted item(s) deferred, %d revived item(s) live, counts agree\n' \
	"${#punted[@]}" "${#revived[@]}"
printf 'release ladder: %d gate-labelled item(s) and %d ledger gating row(s) agree across %d rung(s)\n' \
	"$labelled_n" "$ledger_gating_n" "${#rung_plan[@]}"
