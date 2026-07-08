#!/usr/bin/env bash
#
# Unit tests for scripts/lint-status.sh — the docs/STATUS.md format gate.
#
# Rule 3 (Notes ≤ 250 chars) and rule 4 (a Notes cell over 200 chars must link
# to a doc) together decide whether a Queue row may drop context on the floor,
# so they are asserted here against synthetic fixtures. Runs under `make check`
# (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
LINT="$REPO_ROOT/scripts/lint-status.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# repeat CHAR N — emit N copies of CHAR, with no trailing newline.
repeat() {
	local char="$1" n="$2"
	printf '%*s' "$n" '' | tr ' ' "$char"
}

# fixture ITEM NOTES — write a minimal one-row STATUS.md, echo its path.
fixture() {
	local item="$1" notes="$2" file="$WORKDIR/STATUS.md"
	{
		printf '# Project Status\n\nLast touched: 2026-07-08\n\n'
		printf '## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n'
		printf '|---|---|---|---|---|---|\n'
		# The Labels cell is opaque to the linter, so it is left unadorned
		# (backticks here would read as a command substitution to shellcheck).
		printf '| <a id="Q1"></a>Q1 | %s | infra | 🔲 | S | %s |\n' "$item" "$notes"
		printf '\n## Deferred\n'
	} >"$file"
	printf '%s\n' "$file"
}

# expect NAME WANT_RC ITEM NOTES — run the linter on the fixture, assert exit code.
# WANT_RC is 0 (clean) or 1 (a rule fired).
expect() {
	local name="$1" want_rc="$2" item="$3" notes="$4" file rc=0
	file="$(fixture "$item" "$notes")"
	"$LINT" "$file" >/dev/null 2>&1 || rc=$?
	if [[ "$rc" == "$want_rc" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want rc=%s got rc=%s\n' "$name" "$want_rc" "$rc" >&2
		fails=$((fails + 1))
	fi
}

PLAIN_ITEM='Some item'
LINK_ITEM='[Some item](plan/some-item.md)'

# Rule 4 boundary: 200 chars is the last self-contained length; 201 is compression.
expect 'rule4: 200-char notes, no link  -> clean' 0 "$PLAIN_ITEM" "$(repeat x 200)"
expect 'rule4: 201-char notes, no link  -> fail' 1 "$PLAIN_ITEM" "$(repeat x 201)"

# Over the threshold, a link in either the Item or the Notes cell satisfies it.
expect 'rule4: 240-char notes, Item link -> clean' 0 "$LINK_ITEM" "$(repeat x 240)"
expect 'rule4: 240-char notes, Note link -> clean' 0 "$PLAIN_ITEM" "$(repeat x 220) [plan](plan/x.md)"

# A same-file anchor is NOT an overflow target: the sibling row it points at is
# capped at 250 chars too, so it cannot hold the context this row dropped. This
# is the real-world miss — Q284 carried a memory-only governance decision behind
# a `[Q289](#Q289)` cross-reference and satisfied a naive "has a link" check.
expect 'rule4: sibling-row anchor only   -> fail' 1 "$PLAIN_ITEM" "$(repeat x 220) see [Q289](#Q289)"
expect 'rule4: anchor + real doc link    -> clean' 0 "$PLAIN_ITEM" "$(repeat x 200) [Q289](#Q289) [plan](plan/x.md)"
expect 'rule4: linked Item + row anchor  -> clean' 0 "$LINK_ITEM" "$(repeat x 220) see [Q289](#Q289)"
# A cross-document link that happens to carry a fragment still counts.
expect 'rule4: doc link with fragment    -> clean' 0 "$PLAIN_ITEM" "$(repeat x 210) [g7](design/appendix-g.md#g7)"

# Rule 3 still caps total length — a link does not buy extra characters.
expect 'rule3: 251-char notes, Item link -> fail' 1 "$LINK_ITEM" "$(repeat x 251)"
expect 'rule3: 250-char notes, Item link -> clean' 0 "$LINK_ITEM" "$(repeat x 250)"

# Short rows stay free of the link requirement — most S items are self-contained.
expect 'rule4: short notes, no link     -> clean' 0 "$PLAIN_ITEM" 'Register into a fresh registry.'

# Thresholds are env-overridable (documented in maintaining-backlog.md). The
# assignment prefixes the external linter, not `expect` — a var prefixed onto a
# bash *function* leaks into the caller's scope and would taint the check below.
threshold_file="$(fixture "$PLAIN_ITEM" "$(repeat x 240)")"
rc=0
NOTES_LINK_CHARS=300 "$LINT" "$threshold_file" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then
	printf 'ok   rule4: threshold override relaxes\n'
else
	printf 'FAIL rule4: NOTES_LINK_CHARS=300 should admit a 240-char unlinked row (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

# The real file must pass every rule.
rc=0
"$LINT" "$REPO_ROOT/docs/STATUS.md" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then
	printf 'ok   docs/STATUS.md passes all rules\n'
else
	printf 'FAIL docs/STATUS.md does not pass lint-status.sh (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\nlint-status-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nlint-status-test: ok\n'
