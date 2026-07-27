#!/usr/bin/env bash
#
# Unit tests for scripts/lint-backlog.sh — the docs/STATUS.md format gate
# (vendored from the backlog skill).
#
# The caps rules (Notes ≤ 250 chars; > 200 chars must link a doc) decide
# whether a Queue row may drop context on the floor, so they are asserted
# against synthetic fixtures, alongside the counter / state / Deferred-trigger
# rules the new format adds. Runs under `make check` (via `make scripts-test`)
# and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
LINT="$REPO_ROOT/scripts/lint-backlog.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# repeat CHAR N — emit N copies of CHAR, with no trailing newline.
repeat() {
	local char="$1" n="$2"
	printf '%*s' "$n" '' | tr ' ' "$char"
}

# fixture ROW... — write a STATUS.md whose Queue holds the given rows (one
# argument per full `| ... |` line) and whose Deferred table holds any rows
# passed after a `--deferred` separator. Echoes the file path.
fixture() {
	local file="$WORKDIR/STATUS.md" in_deferred=0
	{
		printf '# Project Status\n\n'
		printf '## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n'
		printf '|---|---|---|---|---|---|\n'
		local row
		for row in "$@"; do
			if [[ "$row" == "--deferred" ]]; then
				printf '\n## Deferred\n\n'
				printf '| ID | Item | Labels | Sz | Trigger to revive |\n'
				printf '|---|---|---|---|---|\n'
				in_deferred=1
				continue
			fi
			printf '%s\n' "$row"
		done
		if (( ! in_deferred )); then
			printf '\n## Deferred\n'
		fi
	} >"$file"
	printf '%s\n' "$file"
}

# qrow ID ITEM ST NOTES — build a Queue row. The Labels cell is opaque to the
# linter, so it is left unadorned (backticks here would read as a command
# substitution to shellcheck).
qrow() {
	printf '| <a id="%s"></a>%s | %s | infra | %s | S | %s |' "$1" "$1" "$2" "$3" "$4"
}

# drow ID ITEM TRIGGER — build a Deferred row.
drow() {
	printf '| <a id="%s"></a>%s | %s | infra | S | %s |' "$1" "$1" "$2" "$3"
}

# expect NAME WANT_RC ROW... — run the linter on the fixture, assert exit code.
# WANT_RC is 0 (clean) or 1 (a rule fired).
expect() {
	local name="$1" want_rc="$2" file rc=0
	shift 2
	file="$(fixture "$@")"
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

# --- Notes caps (rules 3/4 of the old linter, carried forward) ---------------

# Link-threshold boundary: 200 chars is the last self-contained length.
expect 'caps: 200-char notes, no link   -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 200)")"
expect 'caps: 201-char notes, no link   -> fail' 1 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 201)")"

# Over the threshold, a link in either the Item or the Notes cell satisfies it.
expect 'caps: 240-char notes, Item link -> clean' 0 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat x 240)")"
expect 'caps: 240-char notes, Note link -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 220) [plan](plan/x.md)")"

# A same-file anchor is NOT an overflow target: the sibling row it points at is
# capped at 250 chars too, so it cannot hold the context this row dropped. This
# is the real-world miss — Q284 carried a memory-only governance decision behind
# a `[Q289](#Q289)` cross-reference and satisfied a naive "has a link" check.
expect 'caps: sibling-row anchor only   -> fail' 1 \
	"$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 220) see [Q2](#Q2)")" \
	"$(qrow Q2 "$PLAIN_ITEM" 🔲 'short')"
expect 'caps: anchor + real doc link    -> clean' 0 \
	"$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 190) [Q2](#Q2) [plan](plan/x.md)")" \
	"$(qrow Q2 "$PLAIN_ITEM" 🔲 'short')"
# A cross-document link that happens to carry a fragment still counts.
expect 'caps: doc link with fragment    -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 210) [g7](design/appendix-g.md#g7)")"

# The hard cap still applies — a link does not buy extra characters.
expect 'caps: 251-char notes, Item link -> fail' 1 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat x 251)")"
expect 'caps: 250-char notes, Item link -> clean' 0 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat x 250)")"

# Short rows stay free of the link requirement — most S items are self-contained.
expect 'caps: short notes, no link      -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 'Register into a fresh registry.')"

# The same caps apply to the Deferred trigger cell.
expect 'caps: deferred 251-char trigger -> fail' 1 --deferred "$(drow Q1 "$PLAIN_ITEM" "**Demand:** $(repeat x 240)")"
expect 'caps: deferred long + Item link -> clean' 0 --deferred "$(drow Q1 "$LINK_ITEM" "**Demand:** $(repeat x 220)")"

# --- Counter, states, anchors, blockers, Deferred triggers -------------------

# Old-format markers are rejected.
expect 'state: ▶ Started row            -> fail' 1 "$(qrow Q1 "$PLAIN_ITEM" ▶ 'in flight')"
expect 'state: 💤 row in the Queue      -> fail' 1 "$(qrow Q1 "$PLAIN_ITEM" 💤 'parked')"
expect 'state: 🚫 row                   -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🚫 'waits on an external sign-off')"

# `Blocked by [QN](#QN)` implies 🚫, and every (#QN) link must resolve.
expect 'block: Blocked-by prefix on 🔲  -> fail' 1 \
	"$(qrow Q1 "$PLAIN_ITEM" 🔲 'Blocked by [Q2](#Q2). Needs the fix first.')" \
	"$(qrow Q2 "$PLAIN_ITEM" 🔲 'short')"
expect 'block: Blocked-by prefix on 🚫  -> clean' 0 \
	"$(qrow Q1 "$PLAIN_ITEM" 🚫 'Blocked by [Q2](#Q2). Needs the fix first.')" \
	"$(qrow Q2 "$PLAIN_ITEM" 🔲 'short')"
expect 'block: dangling (#QN) reference -> fail' 1 "$(qrow Q1 "$PLAIN_ITEM" 🔲 'see [Q99](#Q99)')"

# Anchor and ID hygiene.
expect 'ids: anchor/visible mismatch    -> fail' 1 '| <a id="Q1"></a>Q2 | item | infra | 🔲 | S | notes |'
expect 'ids: duplicate ID               -> fail' 1 \
	"$(qrow Q1 "$PLAIN_ITEM" 🔲 'one')" \
	"$(qrow Q1 "$PLAIN_ITEM" 🔲 'two')"

# Deferred triggers must open with a source tag.
expect 'defer: untagged trigger         -> fail' 1 --deferred "$(drow Q1 "$PLAIN_ITEM" 'when someone asks')"
expect 'defer: **Event:** trigger       -> clean' 0 --deferred "$(drow Q1 "$PLAIN_ITEM" '**Event:** upstream ships the fix.')"

# The Next ID counter is old format: IDs now come from alloc-queue-id.sh, which
# claims a refs/queue-ids/QN ref. A file-local counter conflicts by construction
# under concurrent sessions (Q382), so its presence is a lint failure.
counter_file="$WORKDIR/counter.md"
{ printf '**Next ID:** Q101\n'; cat "$(fixture "$(qrow Q1 "$PLAIN_ITEM" 🔲 'notes')")"; } >"$counter_file"
rc=0; "$LINT" "$counter_file" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   old-format: Next ID counter     -> fail\n'; else
	printf 'FAIL old-format: a Next ID line should fail (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# A file with no counter at all is the current format.
expect 'counter: absent (current format) -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 'notes')"

lt_file="$WORKDIR/lasttouched.md"
{ cat "$(fixture "$(qrow Q1 "$PLAIN_ITEM" 🔲 'notes')")"; printf '\nLast touched: 2026-07-08\n'; } >"$lt_file"
rc=0; "$LINT" "$lt_file" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   old-format: Last touched line   -> fail\n'; else
	printf 'FAIL old-format: Last touched line should fail (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# Thresholds are env-overridable. The assignment prefixes the external linter,
# not a bash function — a var prefixed onto a function leaks into the caller's
# scope and would taint later checks.
threshold_file="$(fixture "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat x 240)")")"
rc=0
NOTES_LINK_CHARS=300 "$LINT" "$threshold_file" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then
	printf 'ok   caps: threshold override relaxes\n'
else
	printf 'FAIL caps: NOTES_LINK_CHARS=300 should admit a 240-char unlinked row (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

# --- --staged mode: commit isolation ------------------------------------------

# In a throwaway repo, staging the backlog file alongside another file must be
# rejected; staging it alone must pass; a commit without it is untouched.
staged_repo="$WORKDIR/repo"
mkdir -p "$staged_repo/docs"
git -C "$staged_repo" init -q
cp "$(fixture "$(qrow Q1 "$PLAIN_ITEM" 🔲 'notes')")" "$staged_repo/docs/STATUS.md"
printf 'x\n' >"$staged_repo/other.txt"

git -C "$staged_repo" add docs/STATUS.md other.txt
rc=0; (cd "$staged_repo" && "$LINT" --staged) >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   staged: backlog + other file    -> fail\n'; else
	printf 'FAIL staged: mixed staging should fail (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

git -C "$staged_repo" reset -q other.txt
rc=0; (cd "$staged_repo" && "$LINT" --staged) >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   staged: backlog alone           -> clean\n'; else
	printf 'FAIL staged: isolated staging should pass (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

git -C "$staged_repo" reset -q
git -C "$staged_repo" add other.txt
rc=0; (cd "$staged_repo" && "$LINT" --staged) >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   staged: backlog not staged      -> no-op\n'; else
	printf 'FAIL staged: non-backlog commit should pass untouched (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- Rule 9: re-derive the Progress row when the last Queue row goes ----------

# Deleting the last Queue row that points at a plan makes that plan ✅ (deferred
# residuals don't count), and the flip must land in the same edit. The real miss:
# Q398 was the last Queue row on runner-sizing-profiles.md, its removal left the
# Progress row at ⚠️, and a follow-up PR (#857) had to correct it.
#
# progress_fixture STATUS ROW... — a STATUS.md carrying a Progress table whose
# single row links plan/some-item.md with the given status, plus the Queue rows.
progress_fixture() {
	local status="$1" file="$WORKDIR/progress-STATUS.md"
	shift
	{
		printf '# Project Status\n\n'
		printf '## Progress\n\n'
		printf '| Item | Labels | Status |\n'
		printf '|---|---|---|\n'
		printf '| [Some plan](plan/some-item.md) | infra | %s |\n\n' "$status"
		printf '## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n'
		printf '|---|---|---|---|---|---|\n'
		local row
		for row in "$@"; do printf '%s\n' "$row"; done
		printf '\n## Deferred\n\n'
		printf '| ID | Item | Labels | Sz | Trigger to revive |\n'
		printf '|---|---|---|---|---|\n'
		printf '| <a id="Q9"></a>Q9 | [Residual](plan/some-item.md) | infra | S | **Demand:** an operator reports it. |\n'
	} >"$file"
	printf '%s\n' "$file"
}

# expect_progress NAME WANT_RC BEFORE_STATUS AFTER_STATUS -- BEFORE_ROWS -- AFTER_ROWS
# Commits BEFORE as the baseline, stages AFTER, runs the linter in --staged mode.
expect_progress() {
	local name="$1" want_rc="$2" before_st="$3" after_st="$4"
	shift 4
	local -a before=() after=(); local seen_sep=0 arg
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then seen_sep=$((seen_sep + 1)); continue; fi
		if (( seen_sep < 2 )); then before+=("$arg"); else after+=("$arg"); fi
	done

	local repo="$WORKDIR/progress-repo" rc=0
	rm -rf "$repo"; mkdir -p "$repo/docs"
	git -C "$repo" init -q
	cp "$(progress_fixture "$before_st" ${before+"${before[@]}"})" "$repo/docs/STATUS.md"
	git -C "$repo" add docs/STATUS.md
	git -C "$repo" -c user.email=t@t -c user.name=t commit -qm base
	cp "$(progress_fixture "$after_st" ${after+"${after[@]}"})" "$repo/docs/STATUS.md"
	git -C "$repo" add docs/STATUS.md
	(cd "$repo" && "$LINT" --staged) >/dev/null 2>&1 || rc=$?
	if [[ "$rc" == "$want_rc" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want rc=%s got rc=%s\n' "$name" "$want_rc" "$rc" >&2
		fails=$((fails + 1))
	fi
}

PLAN_ITEM='[Some item](plan/some-item.md)'
OTHER_ITEM='[Other item](plan/other.md)'

# The miss: last row on the plan deleted, Progress row left at ⚠️.
expect_progress 'progress: last row gone, still ⚠️ -> fail' 1 ⚠️ ⚠️ \
	-- "$(qrow Q1 "$PLAN_ITEM" 🔲 'notes')" -- "$(qrow Q2 "$OTHER_ITEM" 🔲 'notes')"

# The same edit, done right: the flip rides along.
expect_progress 'progress: last row gone, flipped ✅ -> clean' 0 ⚠️ ✅ \
	-- "$(qrow Q1 "$PLAN_ITEM" 🔲 'notes')" -- "$(qrow Q2 "$OTHER_ITEM" 🔲 'notes')"

# A deferred residual does NOT hold the plan at ⚠️ — the Deferred row linking
# plan/some-item.md is present in every fixture above, and the clean case proves
# it is correctly ignored. Conversely, another *Queue* row on the same plan does
# hold it: deleting one of two rows owes nothing.
expect_progress 'progress: another Queue row remains -> clean' 0 ⚠️ ⚠️ \
	-- "$(qrow Q1 "$PLAN_ITEM" 🔲 'notes')" "$(qrow Q2 "$PLAN_ITEM" 🔲 'notes')" \
	-- "$(qrow Q2 "$PLAN_ITEM" 🔲 'notes')"

# Steady state must stay silent: the rule only looks at plans whose last row just
# vanished, so the many rows that merely cite a completed (✅) plan never trip it.
expect_progress 'progress: unrelated edit, ✅ + citation -> clean' 0 ✅ ✅ \
	-- "$(qrow Q1 "$PLAN_ITEM" 🔲 'notes')" -- "$(qrow Q1 "$PLAN_ITEM" 🔲 'edited notes')"

# The escape hatch, for a vanished row that was only citing the plan. Rebuild the
# failing state from the first case, then prove the env var admits it — asserting
# against a state known to fail, not a vacuously clean one.
progress_allow_repo="$WORKDIR/progress-allow"
rm -rf "$progress_allow_repo"; mkdir -p "$progress_allow_repo/docs"
git -C "$progress_allow_repo" init -q
cp "$(progress_fixture ⚠️ "$(qrow Q1 "$PLAN_ITEM" 🔲 'notes')")" "$progress_allow_repo/docs/STATUS.md"
git -C "$progress_allow_repo" add docs/STATUS.md
git -C "$progress_allow_repo" -c user.email=t@t -c user.name=t commit -qm base
cp "$(progress_fixture ⚠️ "$(qrow Q2 "$OTHER_ITEM" 🔲 'notes')")" "$progress_allow_repo/docs/STATUS.md"
git -C "$progress_allow_repo" add docs/STATUS.md

rc=0; (cd "$progress_allow_repo" && "$LINT" --staged) >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   progress: escape-hatch baseline does fail\n'; else
	printf 'FAIL progress: escape-hatch baseline should fail first (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

rc=0
(cd "$progress_allow_repo" && BACKLOG_ALLOW_PROGRESS_STALE='plan/some-item.md' "$LINT" --staged) >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   progress: BACKLOG_ALLOW_PROGRESS_STALE -> clean\n'; else
	printf 'FAIL progress: the escape hatch should admit the stale row (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- The real file must pass every rule ---------------------------------------

rc=0
"$LINT" "$REPO_ROOT/docs/STATUS.md" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then
	printf 'ok   docs/STATUS.md passes all rules\n'
else
	printf 'FAIL docs/STATUS.md does not pass lint-backlog.sh (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\nlint-backlog-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\nlint-backlog-test: ok\n'
