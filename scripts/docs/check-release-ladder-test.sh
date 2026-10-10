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
#
# Every fixture is built in an assignment, never inside expect's argument list:
# an argument drops the substitution's status, so a builder that failed or was
# killed hands the checker an empty path and reads as its rc 2 (Q1145). As an
# assignment, errexit ends the suite with the builder's own status instead.
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
page="$(write_page evid "$TWO_ROWS" three "$BACK")"
store="$(write_store evid Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"
expect evidence-id 0 'an item cited as evidence is not counted as revived' \
	"$page" "$store"

# Q932's defect: a trigger fired, the item came back, the table still calls it
# punted. This is the state that stood for five days.
page="$(write_page rev "$TWO_ROWS" three "$BACK")"
store="$(write_store rev Q565:ready Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"
expect revived-still-punted 1 'a punted item that is no longer deferred fails' \
	"$page" "$store"

page="$(write_page pm "$TWO_ROWS" three "$BACK")"
store="$(write_store pm Q565:deferred Q566:deferred Q408:ready Q564:ready Q725:ready)"
expect punted-missing 1 'a punted item whose row is gone fails' \
	"$page" "$store"

# The other direction: re-parking is a one-word edit in the item file, nowhere
# near this page.
page="$(write_page rp "$TWO_ROWS" three "$BACK")"
store="$(write_store rp Q565:deferred Q566:deferred Q765:deferred Q408:deferred Q564:ready Q725:ready)"
expect reparked 1 'a revived item that was parked again fails' \
	"$page" "$store"

page="$(write_page rm "$TWO_ROWS" three "$BACK")"
store="$(write_store rm Q565:deferred Q566:deferred Q765:deferred Q564:ready Q725:ready)"
expect revived-missing 1 'a revived item whose row is gone fails' \
	"$page" "$store"

# The counts the prose states, each against what the sections actually hold.
page="$(write_page pc "$TWO_ROWS" four "$BACK")"
store="$(write_store pc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"
expect punted-count 1 'a punted count the table contradicts fails' \
	"$page" "$store"

ONE_BACK='**Two of the original five are back.** [Q408](../queue/Q408.md) waited on an ask, and it fired.'
page="$(write_page rc "$TWO_ROWS" three "$ONE_BACK")"
store="$(write_store rc Q565:deferred Q566:deferred Q765:deferred Q408:ready)"
expect revived-count 1 'a revived count the paragraph contradicts fails' \
	"$page" "$store"

TOTAL_BACK='**Two of the original nine are back.** [Q408](../queue/Q408.md) waited on an ask and [Q564](../queue/Q564.md) on demand, and both fired.'
page="$(write_page tc "$TWO_ROWS" three "$TOTAL_BACK")"
store="$(write_store tc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready)"
expect total-count 1 'an original total the two sections contradict fails' \
	"$page" "$store"

# The last revived item shipping empties the paragraph, which is a legal state
# rather than a shape change -- but only when the prose says so. Both directions,
# because a gate that accepts an empty paragraph unconditionally stops reading
# the half it exists for.
NONE_BACK='**None of the original three are back.** The last one that was has since shipped: [Q408](../queue/Q408.md) landed, leaving this accounting.'
page="$(write_page rn "$TWO_ROWS" three "$NONE_BACK")"
store="$(write_store rn Q565:deferred Q566:deferred Q765:deferred)"
expect revived-none 0 'a declared-empty revived paragraph passes' \
	"$page" "$store"

CLAIMED_BACK='**Two of the original five are back.** Both triggers fired, and neither is named here.'
page="$(write_page rce "$TWO_ROWS" three "$CLAIMED_BACK")"
store="$(write_store rce Q565:deferred Q566:deferred Q765:deferred)"
expect revived-claimed-empty 2 'a paragraph claiming revived items while naming none refuses' \
	"$page" "$store"

# A section down to one item takes a singular verb, which the gate used to reject
# outright: the only grammatical sentence was unmatchable, so the page had to say
# "One ... are back" to stay green (Q965). Both counts, because the punted aside
# carries the identical wording and would otherwise be half-fixed.
ONE_ROW='| Demand | [Q565](../queue/Q565.md) rate limiting |'
SINGULAR_BACK='**One of the original two is back.** [Q408](../queue/Q408.md) waited on an ask, and it fired.'
page="$(write_page sr "$ONE_ROW" one "$SINGULAR_BACK")"
store="$(write_store sr Q565:deferred Q408:ready)"
expect singular-revived 0 'a single revived item may take a singular verb' \
	"$page" "$store"

# The control: the plural still parses, so the change widened the wording rather
# than moving it.
PLURAL_BACK='**One of the original two are back.** [Q408](../queue/Q408.md) waited on an ask, and it fired.'
page="$(write_page ps "$ONE_ROW" one "$PLURAL_BACK")"
store="$(write_store ps Q565:deferred Q408:ready)"
expect plural-still-read 0 'the plural wording still parses' \
	"$page" "$store"

# And the verb is still required: dropping it entirely must refuse rather than
# let the count drift unread, which a wildcard in its place would have allowed.
VERBLESS_BACK='**One of the original two back.** [Q408](../queue/Q408.md) waited on an ask, and it fired.'
page="$(write_page vb "$ONE_ROW" one "$VERBLESS_BACK")"
store="$(write_store vb Q565:deferred Q408:ready)"
expect verbless 2 'a sentence with no verb at all still refuses' \
	"$page" "$store"

# Q1087: a gate label and its rung's scope ledger, each direction of the other.
# The ladder links each rung to a plan file beside the page, and the 2.0 rung's
# plan is deliberately not named for 2.0, so a filename template would miss it.
# write_ladder NAME — a page whose punted half is valid, with a two-rung ladder.
write_ladder() {
	local out="$FIXTURE_DIR/ladder.$1.md"
	{
		printf '# Release ladder\n\n## The ladder\n\n| Release | Carries | Gate |\n|---|---|---|\n'
		printf '| **1.9** | The overlap | [plan.%s.19.md](plan.%s.19.md) |\n' "$1" "$1"
		printf '| **2.0** | v2 GA | [plan.%s.ga.md](plan.%s.ga.md#phase-3) |\n\n' "$1" "$1"
		printf '## What is punted past `v2.0.0`\n\n| Waiting on | Items |\n|---|---|\n%s\n\n%s\n\n' "$ONE_ROW" "$SINGULAR_BACK"
		printf '## What this does not decide\n\nThe items moved to Deferred (one of them still is).\n'
	} > "$out"
	printf '%s\n' "$out"
}

# write_plan NAME RUNG LEDGER_ROWS — a plan with a scope ledger; an empty
# LEDGER_ROWS of "-" writes a plan with no ledger at all.
write_plan() {
	local out="$FIXTURE_DIR/plan.$1.$2.md"
	{
		printf '# Plan\n\n## Status\n\n| Phase | Status |\n|---|---|\n| 1 | [Q9](../queue/Q9.md) open |\n\n'
		if [[ "$3" != "-" ]]; then
			printf '## Scope ledger\n\n| Q-ID | Item | Gates? | Status |\n|---|---|---|---|\n%s\n' "$3"
			printf '| — | RC validated on dogfood | gates | 🔲 |\n\n'
		fi
		printf '## Definition of done\n\n1. Done.\n'
	} > "$out"
}

# write_labelled_store NAME "ID:label,label" ... — every item is ready, and the
# punted and revived items the ladder names are present in the right states.
# Each body also carries a `labels:` list naming 2.0-gate: a label in prose is
# not frontmatter, and gate-bound fails if the reader takes Q3's for one.
write_labelled_store() {
	local dir="$FIXTURE_DIR/store.$1" spec id labels l
	shift
	mkdir -p "$dir"
	printf -- '---\nid: Q565\nlabels:\n    - debt\nstatus: deferred\n---\n\n# Q565\n' > "$dir/Q565.md"
	printf -- '---\nid: Q408\nstatus: ready\n---\n\n# Q408\n' > "$dir/Q408.md"
	for spec in "$@"; do
		id="${spec%%:*}"
		labels="${spec#*:}"
		{
			printf -- '---\nid: %s\nlabels:\n' "$id"
			for l in ${labels//,/ }; do printf '    - %s\n' "$l"; done
			printf 'status: ready\n---\n\n# %s\n\nlabels:\n    - 2.0-gate\n' "$id"
		} > "$dir/$id.md"
	done
	printf '%s\n' "$dir"
}

LEDGER_GA='| [Q1](../queue/Q1.md) | The removal | `2.0-gate` | 🔲 |
| Q2 | Closed already | `2.0-gate` | ✅ shipped |
| [Q3](../queue/Q3.md) | Rides along | rides | 🔲 |'
LEDGER_19='| [Q4](../queue/Q4.md) | Serve v2 | `1.9-gate` | 🔲 |'

write_plan bound 19 "$LEDGER_19"
write_plan bound ga "$LEDGER_GA"
page="$(write_ladder bound)"
store="$(write_labelled_store bound Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
expect gate-bound 0 'labels and ledgers that agree pass, and a closed ledger row is skipped' \
	"$page" "$store"

# The row's first inversion: a label no plan names as gating.
write_plan unnamed 19 "$LEDGER_19"
write_plan unnamed ga "$LEDGER_GA"
page="$(write_ladder unnamed)"
store="$(write_labelled_store unnamed Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate Q5:ci,2.0-gate)"
expect label-unnamed 1 'a gate-labelled row its ledger does not name fails' \
	"$page" "$store"

# The second inversion: a row the ledger calls gating that carries no label.
# Q264 and Q273 stood this way until 2026-09-08.
write_plan unlabelled 19 "$LEDGER_19"
write_plan unlabelled ga "$LEDGER_GA"
page="$(write_ladder unlabelled)"
store="$(write_labelled_store unlabelled Q1:debt Q3:debt Q4:milestone,1.9-gate)"
expect ledger-unlabelled 1 'a ledger gating row whose item lacks the label fails' \
	"$page" "$store"

# A label on a row the ledger lists as riding is the two claims disagreeing.
write_plan rides 19 "$LEDGER_19"
write_plan rides ga "$LEDGER_GA"
page="$(write_ladder rides)"
store="$(write_labelled_store rides Q1:bug,2.0-gate Q3:debt,2.0-gate Q4:milestone,1.9-gate)"
expect label-rides 1 'a gate-labelled row its ledger marks as riding fails' \
	"$page" "$store"

# The audit's third gap: one release's plan owing a row another's label names.
write_plan cross 19 "$LEDGER_19
| [Q1](../queue/Q1.md) | The removal | \`2.0-gate\` | 🔲 |"
write_plan cross ga "$LEDGER_GA"
page="$(write_ladder cross)"
store="$(write_labelled_store cross Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
expect cross-rung 1 'a ledger marking another rung'"'"'s label fails' \
	"$page" "$store"

write_plan norung 19 "$LEDGER_19"
write_plan norung ga "$LEDGER_GA"
page="$(write_ladder norung)"
store="$(write_labelled_store norung Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate Q6:ci,3.0-gate)"
expect label-no-rung 1 'a gate label whose version is no ladder rung fails' \
	"$page" "$store"

write_plan noledger 19 "$LEDGER_19"
write_plan noledger ga -
page="$(write_ladder noledger)"
store="$(write_labelled_store noledger Q1:bug,2.0-gate Q4:milestone,1.9-gate)"
expect label-no-ledger 1 'a gate label whose rung'"'"'s plan has no scope ledger fails' \
	"$page" "$store"

# queue-lint accepts a quoted item and an inline list, so both must read as
# labels. Each fixture adds a row the ledger does not name: read, it fails.
write_plan inline 19 "$LEDGER_19"
write_plan inline ga "$LEDGER_GA"
INLINE_STORE="$(write_labelled_store inline Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
printf -- '---\nid: Q7\nlabels: [ci, "2.0-gate"]\nstatus: ready\n---\n\n# Q7\n' > "$INLINE_STORE/Q7.md"
page="$(write_ladder inline)"
expect label-inline 1 'an inline-list gate label its ledger does not name fails' \
	"$page" "$INLINE_STORE"

write_plan quoted 19 "$LEDGER_19"
write_plan quoted ga "$LEDGER_GA"
QUOTED_STORE="$(write_labelled_store quoted Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
printf -- "---\nid: Q8\nlabels:\n    - ci\n    - '2.0-gate'\nstatus: ready\n---\n\n# Q8\n" > "$QUOTED_STORE/Q8.md"
page="$(write_ladder quoted)"
expect label-quoted 1 'a quoted block-list gate label its ledger does not name fails' \
	"$page" "$QUOTED_STORE"

write_plan scalar 19 "$LEDGER_19"
write_plan scalar ga "$LEDGER_GA"
SCALAR_STORE="$(write_labelled_store scalar Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
printf -- '---\nid: Q9\nlabels: 2.0-gate\nstatus: ready\n---\n\n# Q9\n' > "$SCALAR_STORE/Q9.md"
page="$(write_ladder scalar)"
expect label-scalar 1 'a scalar gate label its ledger does not name fails' \
	"$page" "$SCALAR_STORE"

# The control: the same forms, named by the ledger, pass, so the red above is
# the ledger check reading them and not a parse failure.
write_plan forms 19 "$LEDGER_19"
write_plan forms ga "$LEDGER_GA
| Q7 | Inline | \`2.0-gate\` | 🔲 |
| Q8 | Quoted | \`2.0-gate\` | 🔲 |
| Q9 | Scalar | \`2.0-gate\` | 🔲 |"
FORMS_STORE="$(write_labelled_store forms Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate)"
printf -- '---\nid: Q7\nlabels: [ci, "2.0-gate"]\nstatus: ready\n---\n\n# Q7\n' > "$FORMS_STORE/Q7.md"
printf -- "---\nid: Q8\nlabels:\n    - ci\n    - '2.0-gate'\nstatus: ready\n---\n\n# Q8\n" > "$FORMS_STORE/Q8.md"
printf -- '---\nid: Q9\nlabels: 2.0-gate\nstatus: ready\n---\n\n# Q9\n' > "$FORMS_STORE/Q9.md"
page="$(write_ladder forms)"
expect label-forms-named 0 'inline, quoted and scalar gate labels the ledger names pass' \
	"$page" "$FORMS_STORE"

# A qualifier in the Gates? cell must not turn a gating row into a non-gating
# one: read loosely, Q5 below is unlabelled and nothing would say so.
write_plan qual 19 "$LEDGER_19"
write_plan qual ga "$LEDGER_GA
| Q5 | Qualified | \`2.0-gate\` (from 1.9) | 🔲 |"
page="$(write_ladder qual)"
store="$(write_labelled_store qual Q1:bug,2.0-gate Q3:debt Q4:milestone,1.9-gate Q5:ci)"
expect gates-cell-qualified 1 'a Gates? cell naming a gate with extra text fails' \
	"$page" "$store"

NOCOL_PLAN="$FIXTURE_DIR/plan.nocol.ga.md"
write_plan nocol 19 "$LEDGER_19"
printf '# Plan\n\n## Scope ledger\n\n| Q-ID | Item | Status |\n|---|---|---|\n| Q1 | The removal | 🔲 |\n' > "$NOCOL_PLAN"
page="$(write_ladder nocol)"
store="$(write_labelled_store nocol Q1:bug,2.0-gate Q4:milestone,1.9-gate)"
expect ledger-no-gates-column 2 'a scope ledger with no Gates? column refuses' \
	"$page" "$store"

# Refusals: a page whose shape moved must not report every claim in it verified.
page="$(write_page np '| Waiting on | nothing yet |' three "$BACK")"
store="$(write_store np Q408:ready Q564:ready Q725:ready)"
expect no-punted 2 'a page whose punted table names no item refuses' \
	"$page" "$store"

page="$(write_page nr "$TWO_ROWS" three 'Nothing came back yet.')"
store="$(write_store nr Q565:deferred Q566:deferred Q765:deferred)"
expect no-revived 2 'a page with no revived paragraph refuses' \
	"$page" "$store"

NO_COUNTS_PAGE="$FIXTURE_DIR/page.nc.md"
{
	printf '# Release ladder\n\n## What is punted past `v2.0.0`\n\n'
	printf '| Waiting on | Items |\n|---|---|\n%s\n\n' "$TWO_ROWS"
	printf '%s\n\n' "$BACK"
	printf '## What this does not decide\n\nThe punted items moved to Deferred.\n'
} > "$NO_COUNTS_PAGE"
store="$(write_store nc Q565:deferred Q566:deferred Q765:deferred Q408:ready Q564:ready Q725:ready)"
expect no-counts 2 'a page that stopped stating its counts refuses' \
	"$NO_COUNTS_PAGE" "$store"

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
