#!/usr/bin/env bash
#
# Unit tests for scripts/docs/find-duplicate-rows.sh — the near-duplicate search
# `make queue-id` runs before it claims an ID.
#
# Reading a matcher predicts its coverage; running it measures it. The red cases
# replay the three duplicates the "search before you file" rule actually failed
# to stop, using the two rows' real titles and Item links:
#
#   Q456 vs Q440 — same Item link, 3 shared content words
#   Q635 vs Q619 — same Item link, 4 shared content words
#   Q511 vs Q500 — different links, 5 shared content words
#
# The green cases are the ones that decide whether anyone keeps reading the
# output: a genuinely novel row, and a row sharing two incidental words with a
# short title. A matcher that fires on those is worse than no matcher.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SEARCH="$REPO_ROOT/scripts/docs/find-duplicate-rows.sh"
ALLOC="$REPO_ROOT/scripts/docs/alloc-queue-id.sh"

fails=0
WORK="$(mktemp -d)"

# Row titles and label cells are full of Markdown code spans, and SC2016 reads a
# literal backtick in a single-quoted string as legacy command substitution.
# Carrying one in a variable keeps the fixtures faithful — the tokenizer has to
# strip them — without the false positive. (A comment cannot open with the
# linter's own name either: that parses as a directive, SC1073.)
bt="$(printf '\140')"

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "$WORK"
}
trap cleanup EXIT

# backlog ROW... — write a STATUS.md fixture with the given Queue rows. Each ROW
# is "ID|Item cell|labels"; the surrounding shape (Progress table above, Deferred
# and Flake watch below) matches the real file so section scoping is exercised.
backlog() {
	local row id item labels
	{
		printf '# Project Status\n\n## Progress\n\n| Item | Labels | Status |\n|---|---|---|\n'
		# A Progress anchor is a plan, not an item (Q509). Its shape differs —
		# no visible ID after the anchor — and the matcher must skip it.
		printf '| <a id="Q248"></a>[GMC CRD manifest drift plan](plan/p.md) | %s | ✅ |\n' "${bt}infra${bt}"
		printf '\n## Queue\n\n| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
		for row in "$@"; do
			IFS='|' read -r id item labels <<<"$row"
			printf '| <a id="%s"></a>%s | %s | %s | 🔲 | S | notes |\n' \
				"$id" "$id" "$item" "${bt}${labels}${bt}"
		done
		printf '\n## Deferred\n\n| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
		printf '| <a id="Q501"></a>Q501 | [%s](plan/q501-cancel-relay.md) | %s | S | **Event:** next live run |\n' \
			"A cancelled run's worker pod keeps running to completion" "${bt}bug${bt}"
		printf '\n### Flake watch\n\n| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
		printf '| <a id="Q549"></a>Q549 | [Provisioner eviction tests flake under parallel load](../cmd/agc/p_test.go) | %s | S | **Event:** recurs on main |\n' "${bt}flake${bt}"
	} >"$WORK/STATUS.md"
}

# expect NAME WANT_ID -- ARGS... — run the search over the fixture and assert
# whether WANT_ID appears. WANT_ID of "" asserts the search stayed silent.
expect() {
	local name="$1" want="$2" got=0
	shift 3 # name, want, the literal --
	"$SEARCH" --file "$WORK/STATUS.md" "$@" >"$WORK/out" 2>&1 || got=$?

	if ((got != 0)); then
		printf 'FAIL %s: the search must never fail a filing, got exit %s\n' "$name" "$got"
		awk '{ print "    " $0 }' "$WORK/out"
		fails=$((fails + 1))
		return
	fi
	if [[ -z "$want" ]]; then
		if [[ ! -s "$WORK/out" ]]; then
			printf 'ok   %s\n' "$name"
			return
		fi
		printf 'FAIL %s: want no candidates, got:\n' "$name"
	else
		if grep -q "^  ${want} " "$WORK/out"; then
			printf 'ok   %s\n' "$name"
			return
		fi
		printf 'FAIL %s: want %s among the candidates, got:\n' "$name" "$want"
	fi
	awk '{ print "    " $0 }' "$WORK/out"
	fails=$((fails + 1))
}

crd_link='../cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml'
doclinks_link='../scripts/docs/check-doc-links.sh'

# --- red: the three duplicates that reached the Queue ----------------------

backlog "Q440|[GMC CRD manifest drifts from the AGC types it embeds]($crd_link)|infra"
expect 'Q456 surfaces Q440 (same link, 3 shared words)' Q440 -- \
	--target "$crd_link" 'The GMC CRD manifests are stale and no gate notices'

backlog "Q619|[Three gates scan tracked files only, so a new file misses its own \`make check\`]($doclinks_link)|ci"
expect 'Q635 surfaces Q619 (same link, 4 shared words)' Q619 -- \
	--target "$doclinks_link" \
	"${bt}doc-links${bt} never reads a new doc's own links until it is staged, so it passes on a file it did not scan"

backlog 'Q500|[Two concurrent live-GitHub runs collide on the fixture repo](plan/q459.md)|tests'
expect 'Q511 surfaces Q500 (different links, 5 shared words)' Q500 -- \
	--target 'development/testing.md' \
	'Two live-GitHub runs collide invisibly, and a killed one poisons the next'

# The link is a second route, not the only one: Q456/Q440 clears the text bar
# on its own, so losing the link must not lose the candidate.
backlog "Q440|[GMC CRD manifest drifts from the AGC types it embeds]($crd_link)|infra"
expect 'Q456 still surfaces Q440 with no link supplied' Q440 -- \
	'The GMC CRD manifests are stale and no gate notices'

# ... and the link genuinely lowers the bar for wording that would not clear it.
# Two shared words against a five-word title is below the text bar by design;
# the shared link is the whole reason this pair surfaces.
kata_link='plan/q408-untrusted-pr-egress.md#6-follow-on-validations'
backlog "Q540|[Milestone: Kata + Dragonfly (node layer) + pull-through cache (guest layer)]($kata_link)|feature"
expect 'a shared link surfaces a pair the text bar rejects' Q540 -- \
	--target "$kata_link" 'Validate Kata + Dragonfly as the mirror backend'
expect 'the same pair without the link stays silent' '' -- \
	'Validate Kata + Dragonfly as the mirror backend'

# --- green: what must stay silent ------------------------------------------

# The control: Q634 as filed, against the rows that were live at the time.
backlog "Q619|[Three gates scan tracked files only, so a new file misses its own \`make check\`]($doclinks_link)|ci" \
	'Q650|[Em-dash density in the design and operations docs](development/documentation-standards.md)|docs' \
	'Q500|[Two concurrent live-GitHub runs collide on the fixture repo](plan/q459.md)|tests'
expect 'a genuinely novel row surfaces nothing' '' -- \
	--target 'operations/troubleshooting.md' \
	"Five condition reasons an operator can see are documented nowhere in ${bt}docs/operations/${bt}"

# Containment divides by the shorter title, so two incidental words against a
# five-word row score 0.40 — the shared-word floor is what rejects it.
backlog 'Q650|[Em-dash density in the design and operations docs](development/documentation-standards.md)|docs'
expect 'two incidental shared words are not a candidate' '' -- \
	"Five condition reasons an operator can see are documented nowhere in ${bt}docs/operations/${bt}"

# --- scope: which tables are searched --------------------------------------

backlog 'Q595|[The cancel runbook manual pod delete may re-run the job GitHub cancelled](operations/troubleshooting.md)|bug'
expect 'Deferred rows are searched too' Q501 -- \
	'A cancelled run'"'"'s worker pod keeps running to completion'

backlog 'Q595|[Unrelated row](operations/troubleshooting.md)|bug'
expect 'Flake watch rows are searched too' Q549 -- \
	'Provisioner eviction tests flake under parallel load'

# A Progress anchor names a plan, not an item; matching it would offer a
# candidate that cannot be a duplicate of anything.
backlog 'Q595|[Unrelated row](operations/troubleshooting.md)|bug'
expect 'Progress-table anchors are never candidates' '' -- \
	'GMC CRD manifest drift plan'

# --- robustness: it must never be the thing that stops a filing -------------

expect 'a missing STATUS.md is silent, not an error' '' -- \
	--file "$WORK/absent.md" 'anything at all'

run_status() {
	local got=0
	"$@" >"$WORK/out" 2>&1 || got=$?
	printf '%s' "$got"
}

# --- the audit: the noise claim stays measurable ---------------------------
#
# The thresholds are a claim about how often the matcher fires against a real
# backlog, and that claim decays as the backlog grows. --audit re-measures it
# through the same scoring path the search uses, so the two cannot drift.

backlog "Q440|[GMC CRD manifest drifts from the AGC types it embeds]($crd_link)|infra" \
	"Q456|[The GMC CRD manifests are stale and no gate notices]($crd_link)|tests" \
	'Q650|[Em-dash density in the design and operations docs](development/documentation-standards.md)|docs'
got="$(run_status "$SEARCH" --file "$WORK/STATUS.md" --audit)"
# 5 rows (3 Queue + Deferred + Flake watch) -> 10 pairs, of which only the
# planted Q440/Q456 duplicate may flag.
if [[ "$got" == 0 ]] &&
	grep -qx 'rows=5 pairs=10 flagged=1' "$WORK/out" &&
	grep -qx "$(printf 'Q456\tQ440')" "$WORK/out"; then
	printf 'ok   %s\n' 'the audit counts every pair and flags only the planted duplicate'
else
	printf 'FAIL %s: got exit %s and:\n' \
		'the audit counts every pair and flags only the planted duplicate' "$got"
	awk '{ print "    " $0 }' "$WORK/out"
	fails=$((fails + 1))
fi

got="$(run_status "$SEARCH" --file "$WORK/STATUS.md")"
if [[ "$got" == 1 ]] && grep -q 'wants the title' "$WORK/out"; then
	printf 'ok   %s\n' 'no title is a usage error, not a silent pass'
else
	printf 'FAIL %s: want exit 1 and a usage message, got exit %s\n' \
		'no title is a usage error, not a silent pass' "$got"
	fails=$((fails + 1))
fi

# --- the chokepoint: allocation refuses to run untitled --------------------
#
# Both assertions land in argument parsing, before any network call, so this
# needs no `gh` and claims no ID.

got="$(run_status "$ALLOC")"
if [[ "$got" == 1 ]] && grep -q 'wants the title' "$WORK/out"; then
	printf 'ok   %s\n' 'alloc-queue-id refuses to claim an ID with no title'
else
	printf 'FAIL %s: want exit 1 and a usage message, got exit %s\n' \
		'alloc-queue-id refuses to claim an ID with no title' "$got"
	fails=$((fails + 1))
fi

# `-n 3` was the way to claim IDs without naming a single row. It has to be
# gone, not merely discouraged, or the search is a gate with a door beside it.
got="$(run_status "$ALLOC" -n 3)"
if [[ "$got" == 1 ]] && grep -q 'unknown argument' "$WORK/out"; then
	printf 'ok   %s\n' 'the untitled -n batch form is rejected'
else
	printf 'FAIL %s: want exit 1 and "unknown argument", got exit %s\n' \
		'the untitled -n batch form is rejected' "$got"
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\nfind-duplicate-rows-test: %d assertion(s) failed\n' "$fails" >&2
	exit 1
fi

printf '\nfind-duplicate-rows-test: all assertions passed\n'
exit 0
