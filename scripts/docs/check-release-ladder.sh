#!/usr/bin/env bash
#
# check-release-ladder.sh — bind the release ladder's punted table to the
# backlog status of the items it names (Q932). The page is
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
# Assertion 1 alone would pass a tree where every punted row is correctly
# deferred and the revived paragraph still named one of them.
#
# Usage:
#   check-release-ladder.sh [--page PATH] [--store PATH]
#
# Exits 1 on a finding, and 2 when a section it reads is absent or empty — a
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
REVIVED_MARKER='are back\.\*\*'

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
still_word="$(count_word '[(][a-z]+ of them still are[)]' '[(]')"
back_word="$(count_word '[*][*][A-Za-z]+ of the original [a-z]+ are back[.][*][*]' '[*][*]')"
total_word="$(count_word 'of the original [a-z]+ are back' 'of the original ')"

if [[ -z "$still_word" || -z "$back_word" || -z "$total_word" ]]; then
	printf 'release-ladder: %s no longer states its punted/revived/original counts in the expected wording, so they cannot be checked\n' "$PAGE" >&2
	printf 'expected a "(N of them still are)" aside and a "**N of the original M are back.**" sentence\n' >&2
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

if ((errors > 0)); then
	printf '\n%d release-ladder check(s) failed. The punted table and the revived paragraph are\n' "$errors" >&2
	printf 'claims about `status:` in %s; nothing else reads them.\n' "$STORE" >&2
	exit 1
fi

printf 'release ladder: %d punted item(s) deferred, %d revived item(s) live, counts agree\n' \
	"${#punted[@]}" "${#revived[@]}"
