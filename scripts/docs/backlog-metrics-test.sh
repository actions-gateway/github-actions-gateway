#!/usr/bin/env bash
#
# Unit tests for scripts/docs/backlog-metrics.sh — the STATUS.md history replay.
#
# The replay reads raw diff lines with no section context, so the assertion
# that matters is a negative one: a Progress-table anchor must never register
# as a backlog item (Q509 — Q248's ✅ plan anchor sat at the top of the
# aging-WIP report, the groom's staleness signal). A negative assertion needs
# positive controls, so the same fixture proves a real Queue row IS counted,
# a Deferred row counts as open but stays out of the aging list, and a commit
# that deletes a Queue row while adding a Progress anchor for the same ID
# still records the removal (the exact shape that kept Q248 "open").
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
METRICS="$REPO_ROOT/scripts/docs/backlog-metrics.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# Throwaway repos have no identity configured and must not borrow the
# developer's.
GIT_ID=(-c user.email=test@example.invalid -c user.name=test)

# --- fixtures -----------------------------------------------------------------

# prow ID — a Progress-table row: the anchor carries a Q-ID but the next cell
# is a plan link, not the bare ID (the Q248 shape).
prow() {
	printf '| <a id="%s"></a>[Some plan](plan/p.md) | infra | ✅ |' "$1"
}

# qrow ID — one Queue row.
qrow() {
	printf '| <a id="%s"></a>%s | [Item %s](plan/p.md) | infra | 🔲 | S | notes |' \
		"$1" "$1" "$1"
}

# drow ID — one Deferred row.
drow() {
	printf '| <a id="%s"></a>%s | [Item %s](plan/p.md) | infra | S | **Event:** x. |' \
		"$1" "$1" "$1"
}

# status_md PROGRESS_ROW... -- QUEUE_ROW... -- DEFERRED_ROW...
status_md() {
	local seen=0 arg
	local -a progress=() queue=() deferred=()
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		0) progress+=("$arg") ;;
		1) queue+=("$arg") ;;
		*) deferred+=("$arg") ;;
		esac
	done
	printf '# Project Status\n\n## Progress\n\n'
	printf '| Item | Labels | Status |\n|---|---|---|\n'
	local row
	for row in ${progress+"${progress[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Queue\n\n'
	printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
	for row in ${queue+"${queue[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Deferred\n\n'
	printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
	for row in ${deferred+"${deferred[@]}"}; do printf '%s\n' "$row"; done
}

# --- history: file Q1/Q2/Q4, then complete Q2 with a same-commit plan anchor ---

REPO="$WORKDIR/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q -b trunk
# Q820: no detached maintenance racing the next command in a fixture repo.
git -C "$REPO" config maintenance.auto false

status_md "$(prow Q3)" -- "$(qrow Q1)" "$(qrow Q2)" -- "$(drow Q4)" \
	>"$REPO/STATUS.md"
git -C "$REPO" add STATUS.md
git -C "$REPO" "${GIT_ID[@]}" commit -qm 'docs(status): add Q1, Q2, Q4'

status_md "$(prow Q3)" "$(prow Q2)" -- "$(qrow Q1)" -- "$(drow Q4)" \
	>"$REPO/STATUS.md"
git -C "$REPO" "${GIT_ID[@]}" commit -qam 'docs(status): complete Q2'

EVENTS="$("$METRICS" --events "$REPO/STATUS.md")"
SUMMARY="$("$METRICS" "$REPO/STATUS.md")"

# --- assertions ---------------------------------------------------------------

# reason ID — the reason column of ID's event row ("-" when the ID has none).
reason() {
	awk -F'\t' -v id="$1" '$1 == id { print $5; found = 1 }
		END { if (!found) print "-" }' <<<"$EVENTS"
}

expect_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want %q, got %q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect_eq 'queue row counts as open (positive control)' open "$(reason Q1)"
expect_eq 'deferred row counts as open' open "$(reason Q4)"
expect_eq 'progress-table anchor is not an item' - "$(reason Q3)"
expect_eq 'queue-delete + same-commit progress-add records the removal' \
	completed "$(reason Q2)"

expect_eq 'progress anchor does not raise the arrival high-water' \
	'backlog metrics — high-water Q5, 3 items ever filed' \
	"$(head -1 <<<"$SUMMARY")"
expect_eq 'aging WIP lists the open queue row only' Q1 \
	"$(awk '/aging WIP/ { aging = 1; next } aging { print $1 }' <<<"$SUMMARY" | paste -sd' ' -)"
expect_eq 'deferred row is parked, not aging' \
	'  parked in Deferred: 1 (excluded from aging WIP)' \
	"$(grep 'parked in Deferred' <<<"$SUMMARY")"

# --- shapes a positional `|` split gets wrong ---------------------------------

REPO2="$WORKDIR/repo2"
mkdir -p "$REPO2"
git -C "$REPO2" init -q -b trunk
git -C "$REPO2" config maintenance.auto false

# An escaped pipe inside a cell. Splitting the row on every `|` truncates the
# title at the escape and shifts every field after it, so the row's size is read
# out of the wrong cell.
PIPE_ROW='| <a id="Q7"></a>Q7 | [Item A \| B](plan/p.md) | infra | 🔲 | S | notes |'

{
	printf '# Project Status\n\n## Queue\n\n'
	printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
	printf '%s\n' "$PIPE_ROW"
	printf '\n## Deferred\n\n'
	printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
	printf '%s\n' "$(drow Q8)"
	# A fenced row is documentation about the format, not a parked item.
	# shellcheck disable=SC2016  # backticks are a Markdown fence, not a substitution
	printf '\nHow a Deferred row looks:\n\n```\n%s\n```\n' "$(drow Q9)"
} >"$REPO2/STATUS.md"
git -C "$REPO2" add STATUS.md
git -C "$REPO2" "${GIT_ID[@]}" commit -qm 'docs(status): add Q7, Q8'

EVENTS2="$("$METRICS" --events "$REPO2/STATUS.md")"
SUMMARY2="$("$METRICS" "$REPO2/STATUS.md")"

field() {
	awk -F'\t' -v id="$1" -v col="$2" '$1 == id { print $col; found = 1 }
		END { if (!found) print "-" }' <<<"$EVENTS2"
}

expect_eq 'escaped pipe keeps the title whole' 'Item A | B' "$(field Q7 8)"
expect_eq 'escaped pipe does not shift the size cell' S "$(field Q7 7)"
expect_eq 'a fenced example row is not parked in Deferred' \
	'  parked in Deferred: 1 (excluded from aging WIP)' \
	"$(grep 'parked in Deferred' <<<"$SUMMARY2")"
# The replay reads diff lines, which carry no document around them, so a fenced
# row still registers as an arrival there. Pinned so a change to that boundary
# is a deliberate one — every metric moves with it.
expect_eq 'the replay itself sees the fenced row (line-based by construction)' \
	open "$(field Q9 5)"

# --- the seam: one series across the storage move (Q889) ----------------------
#
# The two bulk commits at the cutover are storage, not flow, and each fails in
# its own direction if unsuppressed: the migration would re-file every live item
# on migration day (destroying every cycle time that spans the seam), and the
# deletion would resolve the whole open backlog on one day. Both are asserted
# here against a fixture carrying an item that is filed in one era and completed
# in the other, which is the only shape that can tell a bridged series from two
# concatenated ones.

REPO3="$WORKDIR/repo3"
mkdir -p "$REPO3/docs"
git -C "$REPO3" init -q -b trunk
git -C "$REPO3" config maintenance.auto false

# commit_on DATE SUBJECT — commit the worktree with both clocks pinned, so the
# cycle times below are the fixture's and not the day the suite runs.
commit_on() {
	local date="$1" subject="$2"
	GIT_AUTHOR_DATE="${date}T12:00:00" GIT_COMMITTER_DATE="${date}T12:00:00" \
		git -C "$REPO3" "${GIT_ID[@]}" commit -qam "$subject"
}

# item ID STATUS — one store file.
item() {
	printf -- '---\nid: %s\ntitle: Item %s\nstatus: %s\nsize: s\nrank: m\n---\n\nnotes\n' \
		"$1" "$1" "$2" >"$REPO3/docs/queue/$1.md"
}

status_md "" -- "$(qrow Q1)" "$(qrow Q2)" "$(qrow Q3)" -- >"$REPO3/docs/STATUS.md"
git -C "$REPO3" add docs/STATUS.md
commit_on 2020-01-01 'docs(status): add Q1, Q2, Q3'

status_md "" -- "$(qrow Q1)" "$(qrow Q3)" -- >"$REPO3/docs/STATUS.md"
commit_on 2020-01-11 'docs(status): complete Q2'

# The migration: every live item arrives under docs/queue/ in one commit.
mkdir -p "$REPO3/docs/queue"
item Q1 open
item Q3 deferred
git -C "$REPO3" add docs/queue
commit_on 2020-02-01 'docs(queue): migrate the backlog to the item store'

# The cutover: the table goes, with both remaining rows still in it.
git -C "$REPO3" rm -q docs/STATUS.md
commit_on 2020-02-02 'docs(queue): delete docs/STATUS.md'

item Q5 open
git -C "$REPO3" add docs/queue
commit_on 2020-02-05 'docs(queue): file Q5'

git -C "$REPO3" rm -q docs/queue/Q1.md
commit_on 2020-02-11 'docs(queue): complete Q1'

EVENTS3="$("$METRICS" --events "$REPO3/docs/STATUS.md")"
SUMMARY3="$("$METRICS" "$REPO3/docs/STATUS.md")"

col3() {
	awk -F'\t' -v id="$1" -v col="$2" '$1 == id { print $col; found = 1 }
		END { if (!found) print "-" }' <<<"$EVENTS3"
}

expect_eq 'the cutover deletion is not a removal' open "$(col3 Q3 5)"
expect_eq 'a table-era item keeps its filing date across the migration' \
	2020-01-01 "$(col3 Q1 2)"
# 2020-01-01 to 2020-02-11. The number is the whole point: a series that
# restarted at the migration would say 10 days (filed 2020-02-01), and one that
# stopped at the cutover would say open.
expect_eq 'an item filed before the seam and completed after it is one span' \
	41 "$(col3 Q1 4)"
expect_eq 'a store-era completion is booked' completed "$(col3 Q1 5)"
expect_eq 'a table-era item is attributed to the table era' table "$(col3 Q1 6)"
expect_eq 'a store-era arrival is attributed to the store era' store "$(col3 Q5 6)"
expect_eq 'a store-era arrival carries the date it was filed' \
	2020-02-05 "$(col3 Q5 2)"
expect_eq 'a store-era arrival takes its title from the item file' \
	'Item Q5' "$(col3 Q5 8)"
expect_eq 'the table era still completes its own removals' \
	completed "$(col3 Q2 5)"

expect_eq 'the summary marks where the storage changed' \
	'  ─── docs/STATUS.md: 3 filed ─── 2020-02-02 cutover ─── docs/queue/: 1 filed ───' \
	"$(grep 'cutover' <<<"$SUMMARY3")"
expect_eq 'both eras count into one completed total' \
	'  completed:       2' \
	"$(grep 'completed:' <<<"$SUMMARY3")"
expect_eq 'the whole open backlog is not resolved by the deletion' \
	'  open now:        2' \
	"$(grep 'open now:' <<<"$SUMMARY3")"
expect_eq 'a store item parked by frontmatter stays out of aging WIP' \
	'  parked in Deferred: 1 (excluded from aging WIP)' \
	"$(grep 'parked in Deferred' <<<"$SUMMARY3")"

if (( fails )); then
	printf '%d failure(s)\n' "$fails" >&2
	exit 1
fi
printf 'all backlog-metrics tests passed\n'
