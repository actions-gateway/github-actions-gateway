#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-release-ladder.sh (Q932): a punted item that
# revived fails, a revived item that was re-parked fails, the prose counts are
# held to what the sections hold, and every read that would leave the gate
# verifying nothing refuses with rc 2.
#
# Both directions are asserted because each half of this gate is the other's
# blind spot. Checking only that punted items are Deferred passes a page whose
# revived paragraph still names one of them, which is the five-day state Q932
# was filed for; checking only the revived half passes the table going stale.
#
# The evidence-ID case is a regression, not a hypothetical. The revived
# paragraph cites another item as the demand behind a trigger, and reading the
# whole paragraph collected it as a third revived item — caught by this gate
# failing its own page on first run.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# File-wide: every fixture line is markdown the page owns, so a `$` in one must
# reach the fixture unexpanded — single quotes are the point, not an oversight.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/docs/check-release-ladder.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/release-ladder-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# write_store NAME "ID:status" ... — an item store fixture.
write_store() {
	local dir="$FIXTURE_DIR/store.$1" spec id status
	shift
	mkdir -p "$dir"
	for spec in "$@"; do
		id="${spec%%:*}"
		status="${spec#*:}"
		printf -- '---\nid: %s\nstatus: %s\n---\n\n# %s\n' "$id" "$status" "$id" > "$dir/$id.md"
	done
	printf '%s\n' "$dir"
}

# write_page NAME PUNTED_ROWS STILL_WORD BACK_SENTENCE — a ladder page fixture.
# The surrounding headings are real, so the section scan is exercised against
# neighbours rather than against a lone table.
write_page() {
	local out="$FIXTURE_DIR/page.$1.md"
	{
		printf '# Release ladder\n\n## The ladder\n\n| Release | Carries |\n|---|---|\n| **2.0** | v2 GA |\n\n'
		printf '## What is punted past `v2.0.0`\n\nNot scheduled.\n\n'
		printf '| Waiting on | Items |\n|---|---|\n'
		printf '%s' "$2"
		printf '\n%s\n\n' "$4"
		printf '## What this does not decide\n\n'
		printf 'The seven punted items moved to Deferred (%s of them still are), and the labels publish it.\n' "$3"
	} > "$out"
	printf '%s\n' "$out"
}

# expect NAME WANT_RC DESC PAGE STORE
expect() {
	local name="$1" want="$2" desc="$3" page="$4" store="$5" rc=0 out
	out="$("$CHECKER" --page "$page" --store "$store" 2>&1)" || rc=$?
	die_if_killed "$name" "$rc" "$want"
	if ((rc != want)); then
		printf 'FAIL: %s — %s: expected rc %d, got %d\n' "$name" "$desc" "$want" "$rc" >&2
		printf '%s\n' "$out" | sed 's/^/       /' >&2
		((fails++)) || true
		return
	fi
	printf 'ok: %s — %s (rc %d)\n' "$name" "$desc" "$rc"
}

TWO_ROWS='| Demand | [Q565](../queue/Q565.md) rate limiting, [Q566](../queue/Q566.md) TLS |
| Hardware | [Q765](../queue/Q765.md) GHES validation |'
BACK='**Two of the original five are back.** [Q408](../queue/Q408.md) waited on an ask and [Q564](../queue/Q564.md) on demand, and both fired: the maintainer is the operator, and Q564'"'"'s demand is [Q725](../queue/Q725.md), which sat in the Queue.'

# The tracked page is the gate's real subject, so it is asserted directly.
expect real 0 'the tracked release ladder is consistent with the store' \
	"docs/plan/release-ladder.md" "docs/queue"

# The evidence ID after the colon is not a revived item. Reading the whole
# paragraph makes Q725 a third one and the counts stop adding up.
expect evidence-id 0 'an item cited as evidence is not counted as revived' \
	"$(write_page evid "$TWO_ROWS" three "$BACK")" \
	"$(write_store evid Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"

# Q932's defect: a trigger fired, the item came back, the table still calls it
# punted. This is the state that stood for five days.
expect revived-still-punted 1 'a punted item that is no longer deferred fails' \
	"$(write_page rev "$TWO_ROWS" three "$BACK")" \
	"$(write_store rev Q565:ready Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"

expect punted-missing 1 'a punted item whose row is gone fails' \
	"$(write_page pm "$TWO_ROWS" three "$BACK")" \
	"$(write_store pm Q565:deferred Q566:deferred Q408:ready Q564:ready Q725:ready)"

# The other direction: re-parking is a one-word edit in the item file, nowhere
# near this page.
expect reparked 1 'a revived item that was parked again fails' \
	"$(write_page rp "$TWO_ROWS" three "$BACK")" \
	"$(write_store rp Q565:deferred Q566:deferred Q765:deferred Q408:deferred Q564:ready Q725:ready)"

expect revived-missing 1 'a revived item whose row is gone fails' \
	"$(write_page rm "$TWO_ROWS" three "$BACK")" \
	"$(write_store rm Q565:deferred Q566:deferred Q765:deferred Q564:ready Q725:ready)"

# The counts the prose states, each against what the sections actually hold.
expect punted-count 1 'a punted count the table contradicts fails' \
	"$(write_page pc "$TWO_ROWS" four "$BACK")" \
	"$(write_store pc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"

ONE_BACK='**Two of the original five are back.** [Q408](../queue/Q408.md) waited on an ask, and it fired.'
expect revived-count 1 'a revived count the paragraph contradicts fails' \
	"$(write_page rc "$TWO_ROWS" three "$ONE_BACK")" \
	"$(write_store rc Q565:deferred Q566:deferred Q765:deferred Q408:ready)"

TOTAL_BACK='**Two of the original nine are back.** [Q408](../queue/Q408.md) waited on an ask and [Q564](../queue/Q564.md) on demand, and both fired.'
expect total-count 1 'an original total the two sections contradict fails' \
	"$(write_page tc "$TWO_ROWS" three "$TOTAL_BACK")" \
	"$(write_store tc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready)"

# The last revived item shipping empties the paragraph, which is a legal state
# rather than a shape change -- but only when the prose says so. Both directions,
# because a gate that accepts an empty paragraph unconditionally stops reading
# the half it exists for.
NONE_BACK='**None of the original three are back.** The last one that was has since shipped: [Q408](../queue/Q408.md) landed, leaving this accounting.'
expect revived-none 0 'a declared-empty revived paragraph passes' \
	"$(write_page rn "$TWO_ROWS" three "$NONE_BACK")" \
	"$(write_store rn Q565:deferred Q566:deferred Q765:deferred)"

CLAIMED_BACK='**Two of the original five are back.** Both triggers fired, and neither is named here.'
expect revived-claimed-empty 2 'a paragraph claiming revived items while naming none refuses' \
	"$(write_page rce "$TWO_ROWS" three "$CLAIMED_BACK")" \
	"$(write_store rce Q565:deferred Q566:deferred Q765:deferred)"

# Refusals: a page whose shape moved must not report every claim in it verified.
expect no-punted 2 'a page whose punted table names no item refuses' \
	"$(write_page np '| Waiting on | nothing yet |' three "$BACK")" \
	"$(write_store np Q408:ready Q564:ready Q725:ready)"

expect no-revived 2 'a page with no revived paragraph refuses' \
	"$(write_page nr "$TWO_ROWS" three 'Nothing came back yet.')" \
	"$(write_store nr Q565:deferred Q566:deferred Q765:deferred)"

NO_COUNTS_PAGE="$FIXTURE_DIR/page.nc.md"
{
	printf '# Release ladder\n\n## What is punted past `v2.0.0`\n\n'
	printf '| Waiting on | Items |\n|---|---|\n%s\n\n' "$TWO_ROWS"
	printf '%s\n\n' "$BACK"
	printf '## What this does not decide\n\nThe punted items moved to Deferred.\n'
} > "$NO_COUNTS_PAGE"
expect no-counts 2 'a page that stopped stating its counts refuses' \
	"$NO_COUNTS_PAGE" \
	"$(write_store nc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"

expect missing-page 2 'a page that does not exist refuses' \
	"$FIXTURE_DIR/absent.md" "docs/queue"

expect missing-store 2 'a store that does not exist refuses' \
	"docs/plan/release-ladder.md" "$FIXTURE_DIR/absent-store"

rc=0
"$CHECKER" --nonsense > /dev/null 2>&1 || rc=$?
die_if_killed unknown-arg "$rc" 2
if ((rc != 2)); then
	printf 'FAIL: unknown-arg — an unrecognized argument: expected rc 2, got %d\n' "$rc" >&2
	((fails++)) || true
else
	printf 'ok: unknown-arg — an unrecognized argument refuses (rc 2)\n'
fi

if ((fails > 0)); then
	printf '\n%d check-release-ladder assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-release-ladder: all assertions passed\n'
