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

# backlog ROW... — write an item-store fixture with the given items. Each ROW
# is "ID|Item cell|labels", unchanged from when this fixture wrote a STATUS.md
# table: only the shape it lands in changed, so every expectation below is
# still asserting the same thing about the same titles.
#
# A parked item and a flake-watch item come along every time, because the two
# are one `status: deferred` in the store and only the `flake` label separates
# them — the section a hit reports is derived from that, not read.
backlog() {
	local row id item labels title target
	rm -rf "$WORK/queue"
	mkdir -p "$WORK/queue"
	item_file() {  # item_file ID "[Title](target)" LABELS STATUS
		local iid=$1 cell=$2 lbls=$3 st=$4
		title=${cell#*[}
		title=${title%%]*}
		target=${cell#*](}
		target=${target%)*}
		{
			printf -- '---\nid: %s\nrank: a%s\nlabels:\n    - %s\nstatus: %s\nsize: S\n' \
				"$iid" "${iid#Q}" "$lbls" "$st"
			printf -- 'target: %s\n---\n\n# %s\n\nnotes\n' "$target" "$title"
		} >"$WORK/queue/$iid.md"
	}
	for row in "$@"; do
		IFS='|' read -r id item labels <<<"$row"
		item_file "$id" "$item" "$labels" ready
	done
	item_file Q501 "[A cancelled run's worker pod keeps running to completion](../plan/q501-cancel-relay.md)" bug deferred
	item_file Q549 "[Provisioner eviction tests flake under parallel load](../../cmd/agc/p_test.go)" flake deferred
}

# expect NAME WANT_ID -- ARGS... — run the search over the fixture and assert
# whether WANT_ID appears. WANT_ID of "" asserts the search stayed silent.
expect() {
	local name="$1" want="$2" got=0
	shift 3 # name, want, the literal --
	"$SEARCH" --store "$WORK/queue" "$@" >"$WORK/out" 2>&1 || got=$?

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

# Q509's case — a Progress anchor naming a plan rather than an item — has no
# equivalent here and is deliberately gone rather than ported. It existed
# because plan rows and item rows shared one file and only their shape told
# them apart; the store holds Q*.md items and nothing else, so the matcher has
# no foreign anchor to skip. What replaces it is the check that a non-item file
# in the directory is not read as one.
# The title has to be one the matcher would otherwise flag, or the case passes
# for the wrong reason: a two-word heading scores below MIN_SHARED whether it is
# indexed or not, so it would stay silent even with the store fully unguarded.
printf '# GMC CRD manifest drifts from the AGC types it embeds\n\nJust a page.\n' \
	>"$WORK/queue/README.md"
expect 'a non-item file in the store is never a candidate' '' -- \
	'GMC CRD manifest drifts from the AGC types it embeds'

# --- robustness: it must never be the thing that stops a filing -------------

expect 'a missing store is silent, not an error' '' -- \
	--store "$WORK/absent" 'anything at all'

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
got="$(run_status "$SEARCH" --store "$WORK/queue" --audit)"
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

got="$(run_status "$SEARCH" --store "$WORK/queue")"
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
