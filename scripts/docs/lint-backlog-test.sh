#!/usr/bin/env bash
#
# Unit tests for scripts/docs/lint-backlog.sh — the docs/STATUS.md format gate
# (vendored from the backlog skill).
#
# The caps rules (Notes ≤ 250 chars; > 200 chars must link a doc) decide
# whether a Queue row may drop context on the floor, so they are asserted
# against synthetic fixtures, alongside the counter / state / Deferred-trigger
# rules the new format adds. Runs under `make check` (via `make scripts-test`)
# and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# LINT_BACKLOG_BIN points the whole suite at another implementation, which is
# how the Q613 rewrite was reconciled against the awk it replaced: every case
# below ran under both, and the only disagreements were the two defects the
# rewrite exists to close (escaped-pipe cases, and the rune-vs-byte cap).
LINT="${LINT_BACKLOG_BIN:-$REPO_ROOT/scripts/docs/lint-backlog.sh}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# repeat CHAR N — emit N copies of CHAR, with no trailing newline.
repeat() {
	local char="$1" n="$2"
	printf '%*s' "$n" '' | tr ' ' "$char"
}

# repeat_str STR N — emit N copies of STR. `tr` maps single bytes, so a
# multi-byte character needs this instead of repeat.
repeat_str() {
	local str="$1" n="$2" out='' i
	for ((i = 0; i < n; i++)); do out+="$str"; done
	printf '%s' "$out"
}

# fixture ROW... — write a STATUS.md whose Queue holds the given rows (one
# argument per full `| ... |` line) and whose Deferred table holds any rows
# passed after a `--deferred` separator. Echoes the file path.
#
# The vocabulary line declares `flake` because rule 11 fires on any backticked
# label with no declaration, and a case built around a flake row is about rule 8.
# Rows built by qrow wear a bare `infra`, which carries no vocabulary at all.
fixture() {
	local file="$WORKDIR/STATUS.md" in_deferred=0
	{
		printf '# Project Status\n\n'
		# shellcheck disable=SC2016  # the backticks are markdown, not a subshell
		printf '**Labels:** `flake`\n\n'
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

# qrow ID ITEM ST NOTES — build a Queue row. Its Labels cell is left unadorned:
# only backticked tokens are vocabulary (rule 11), so a bare word carries none
# and keeps these fixtures focused on the rule under test.
qrow() {
	printf '| <a id="%s"></a>%s | %s | %s | %s | S | %s |' "$1" "$1" "$2" "${5:-infra}" "$3" "$4"
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
# --- Rule 10: a row the baseline deleted may not come back --------------------

# Reproduces the failure this rule exists for. A row is filed, ships, and its
# row is deleted on main; a branch then brings it back. In the real incident the
# branch had merely *reordered* rows around the deleted one, which merges with
# no conflict at all — so nothing but this check would have caught it.
res_repo="$WORKDIR/resurrect"
mkdir -p "$res_repo/docs"
git -C "$res_repo" init -q
# Throwaway repo: it has no user identity configured, and must not read the
# developer's.
git_id=(-c user.email=test@example.invalid -c user.name=test)

commit_backlog() {
	local msg="$1"
	shift
	cp "$(fixture "$@")" "$res_repo/docs/STATUS.md"
	git -C "$res_repo" add docs/STATUS.md
	git -C "$res_repo" "${git_id[@]}" commit -qm "$msg"
}

Q1_ROW="$(qrow Q1 "$PLAIN_ITEM" 🔲 'one')"
Q2_ROW="$(qrow Q2 "$PLAIN_ITEM" 🔲 'two')"

commit_backlog 'file Q1 and Q2' "$Q1_ROW" "$Q2_ROW"
commit_backlog 'Q1 ships; delete its row' "$Q2_ROW"
git -C "$res_repo" update-ref refs/remotes/origin/main HEAD

run_res() { rc=0; (cd "$res_repo" && "$LINT" "$res_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?; }

# Q1 is back, and reordered below Q2 the way a rebase would leave it.
cp "$(fixture "$Q2_ROW" "$Q1_ROW")" "$res_repo/docs/STATUS.md"
run_res
if [[ "$rc" == 1 ]]; then printf 'ok   rule 10: resurrected done row     -> fail\n'; else
	printf 'FAIL rule 10: a row main deleted came back and was not flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# Same file, but the re-open is deliberate.
rc=0; (cd "$res_repo" && BACKLOG_ALLOW_RESURRECT=Q1 "$LINT" "$res_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   rule 10: BACKLOG_ALLOW_RESURRECT -> pass\n'; else
	printf 'FAIL rule 10: the escape hatch should allow a deliberate re-open (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# A genuinely new ID is absent from the baseline too, and must NOT be flagged —
# this is the case the manual `comm` check cannot tell apart.
cp "$(fixture "$Q2_ROW" "$(qrow Q9 "$PLAIN_ITEM" 🔲 'new')")" "$res_repo/docs/STATUS.md"
run_res
if [[ "$rc" == 0 ]]; then printf 'ok   rule 10: newly filed row          -> pass\n'; else
	printf 'FAIL rule 10: a new ID was mistaken for a resurrection (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# A branch that merely predates the deletion is NOT resurrecting anything: its
# HEAD does not carry the deleting commit, and a rebase will apply it. This case
# is common (main moves while a branch is open) so a false positive here would
# make the rule noise. HEAD is moved back to before Q1 was deleted.
git -C "$res_repo" checkout -q -- docs/STATUS.md # drop the fixture; checkout needs a clean tree
git -C "$res_repo" checkout -q HEAD~1
cp "$(fixture "$Q1_ROW" "$Q2_ROW")" "$res_repo/docs/STATUS.md"
run_res
if [[ "$rc" == 0 ]]; then printf 'ok   rule 10: branch behind main       -> pass\n'; else
	printf 'FAIL rule 10: a branch merely behind main was flagged as resurrecting (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- Rule 8: the same staleness trap ------------------------------------------

# A flake row filed on main after this branch opened is absent from the branch's
# file, but the branch never deleted it — it simply predates it. Before the
# ancestry check, rule 8 reported that as a vanished flake row on every stale
# branch.
flake_repo="$WORKDIR/flake"
mkdir -p "$flake_repo/docs"
git -C "$flake_repo" init -q
# shellcheck disable=SC2016  # the backticks are markdown; rule 8 matches /`flake`/
FLAKE_ROW="$(qrow Q3 "$PLAIN_ITEM" 🔲 'flaky' '`flake`')"

cp "$(fixture "$Q2_ROW")" "$flake_repo/docs/STATUS.md"
git -C "$flake_repo" add docs/STATUS.md
git -C "$flake_repo" "${git_id[@]}" commit -qm 'branch point'
git -C "$flake_repo" update-ref refs/heads/branch HEAD

# main then files a flake row; the branch stays at the older commit.
cp "$(fixture "$Q2_ROW" "$FLAKE_ROW")" "$flake_repo/docs/STATUS.md"
git -C "$flake_repo" add docs/STATUS.md
git -C "$flake_repo" "${git_id[@]}" commit -qm 'file a flake row'
git -C "$flake_repo" update-ref refs/remotes/origin/main HEAD
git -C "$flake_repo" checkout -q branch
cp "$(fixture "$Q2_ROW")" "$flake_repo/docs/STATUS.md"

rc=0; (cd "$flake_repo" && "$LINT" "$flake_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   rule 8: branch behind main       -> pass\n'; else
	printf 'FAIL rule 8: a branch predating a flake row was flagged for deleting it (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- Escaped pipes: a cell boundary, not a field separator (Q613) ------------

# Splitting a row on a literal `|` shifts every field after an escaped pipe, so
# the rules downstream read the wrong cell — silently, in both directions. Each
# case below is paired with a control that differs only by the escape, so a
# disagreement is attributable to the shape and not to the row.
#
# shellcheck disable=SC2016  # `\|` is markdown, not a shell escape

# Direction 1: a rule that should FIRE and does not. The Notes cell is over the
# hard cap, but everything after the escaped pipe lands in a later field, so a
# positional split measures only the stub before it.
LONG_NOTES="$(repeat a 40) \\| $(repeat b 215)" # 258 chars
expect 'pipe: over-cap notes with \| -> fail' 1 "$(qrow Q1 "$LINK_ITEM" 🔲 "$LONG_NOTES")"
expect 'pipe: control, same length no \| -> fail' 1 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat a 258)")"

# Direction 2: a rule that should NOT fire and does. The escape sits in the Item
# cell, so a positional split reads St from the Labels cell and rejects a
# perfectly good row.
expect 'pipe: \| in Item, St still 🔲   -> clean' 0 \
	"$(qrow Q1 'Item with a \| pipe' 🔲 'short note')"
expect 'pipe: control, same Item no \|  -> clean' 0 \
	"$(qrow Q1 'Item with a pipe' 🔲 'short note')"

# The escape costs the two characters it is written as: the cap is counted over
# the cell's source form, which is what an author sees while writing the row.
expect 'pipe: \| counts as two chars   -> fail' 1 \
	"$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat a 249)\\|")"

# --- Cell length is runes, not bytes (Q613) ----------------------------------

# `awk`'s length() counts bytes under BWK awk and mawk but runes under gawk in a
# UTF-8 locale, so a row of multi-byte characters near the cap passed or failed
# depending on which awk ran the gate. 250 em dashes are 250 characters and 750
# bytes; the cap is on characters.
expect 'runes: 250 multi-byte chars    -> clean' 0 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat_str — 250)")"
expect 'runes: 251 multi-byte chars    -> fail' 1 "$(qrow Q1 "$LINK_ITEM" 🔲 "$(repeat_str — 251)")"
# The link threshold reads the same scale.
expect 'runes: 201 multi-byte, no link -> fail' 1 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat_str — 201)")"
expect 'runes: 200 multi-byte, no link -> clean' 0 "$(qrow Q1 "$PLAIN_ITEM" 🔲 "$(repeat_str — 200)")"

# --- Rule 8: a deleted flake row is the failure this rule exists for ---------

# The staleness case below asserts the rule stays quiet on a branch that merely
# predates the row. This asserts the other half: a branch that actually deletes
# a flake row main carries must fail, or the rule is decoration.
flake_del_repo="$WORKDIR/flake-delete"
mkdir -p "$flake_del_repo/docs"
git -C "$flake_del_repo" init -q
# shellcheck disable=SC2016  # the backticks are markdown; rule 8 matches /`flake`/
DEL_FLAKE_ROW="$(qrow Q3 "$PLAIN_ITEM" 🔲 'flaky' '`flake`')"

cp "$(fixture "$Q2_ROW" "$DEL_FLAKE_ROW")" "$flake_del_repo/docs/STATUS.md"
git -C "$flake_del_repo" add docs/STATUS.md
git -C "$flake_del_repo" "${git_id[@]}" commit -qm 'file a flake row'
git -C "$flake_del_repo" update-ref refs/remotes/origin/main HEAD
cp "$(fixture "$Q2_ROW")" "$flake_del_repo/docs/STATUS.md"

rc=0; (cd "$flake_del_repo" && "$LINT" "$flake_del_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   rule 8: deleted flake row         -> fail\n'; else
	printf 'FAIL rule 8: a deleted flake row was not flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

rc=0
(cd "$flake_del_repo" && BACKLOG_ALLOW_FLAKE_DELETE=Q3 "$LINT" "$flake_del_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   rule 8: BACKLOG_ALLOW_FLAKE_DELETE -> pass\n'; else
	printf 'FAIL rule 8: the escape hatch should admit a retired row (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# Moving the row to Deferred — what a shipped mitigation actually does — keeps
# it, so the rule stays quiet without the escape hatch.
cp "$(fixture "$Q2_ROW" --deferred "$(drow Q3 "$PLAIN_ITEM" '**Event:** recurs on main after the fix.')")" \
	"$flake_del_repo/docs/STATUS.md"
rc=0; (cd "$flake_del_repo" && "$LINT" "$flake_del_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 0 ]]; then printf 'ok   rule 8: moved to Flake watch     -> pass\n'; else
	printf 'FAIL rule 8: a row moved to Deferred should satisfy the rule (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- Structural rules --------------------------------------------------------

# A visible ID with no anchor cannot be cross-referenced.
expect 'ids: visible ID, no anchor      -> fail' 1 '| Q1 | item | infra | 🔲 | S | notes |'

# A file with no Queue section at all is not a backlog.
no_queue_file="$WORKDIR/no-queue.md"
printf '# Project Status\n\n## Deferred\n' >"$no_queue_file"
rc=0; "$LINT" "$no_queue_file" >/dev/null 2>&1 || rc=$?
if [[ "$rc" == 1 ]]; then printf 'ok   structure: no ## Queue section  -> fail\n'; else
	printf 'FAIL structure: a file with no Queue section should fail (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- The real file must pass every rule ---------------------------------------

# --- Rule 11: the declared label vocabulary ---------------------------------
#
# Q592 was filed wearing `infra` from a branch cut before that label was split
# into ci/dogfood/debt, and merged clean — the two edits touched different rows,
# so nothing conflicted and nothing checked. The Progress table is covered too:
# its Labels cell sits one field earlier, so a check written for the Queue shape
# alone reads the wrong column and silently passes.

# labelled_fixture DECL QUEUE_LABELS PROGRESS_LABELS — write a STATUS.md
# carrying all three tables. Echoes the file path.
labelled_fixture() {
	local decl="$1" queue_labels="$2" progress_labels="$3" file="$WORKDIR/labels.md"
	{
		printf '# Project Status\n\n'
		printf '%s\n\n' "$decl"
		printf '## Progress\n\n'
		printf '| Item | Labels | Status |\n'
		printf '|---|---|---|\n'
		printf '| [A plan](plan/a.md) | %s | ✅ |\n\n' "$progress_labels"
		printf '## Queue\n\n'
		printf '| ID | Item | Labels | St | Sz | Notes |\n'
		printf '|---|---|---|---|---|---|\n'
		printf '| <a id="Q1"></a>Q1 | %s | %s | 🔲 | S | short |\n\n' "$PLAIN_ITEM" "$queue_labels"
		printf '## Deferred\n'
	} >"$file"
	printf '%s\n' "$file"
}

# expect_file NAME WANT_RC FILE — assert the linter's exit code on a prebuilt file.
expect_file() {
	local name="$1" want_rc="$2" file="$3" rc=0
	"$LINT" "$file" >/dev/null 2>&1 || rc=$?
	if [[ "$rc" == "$want_rc" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want rc=%s got rc=%s\n' "$name" "$want_rc" "$rc" >&2
		fails=$((fails + 1))
	fi
}

# lbl WORD... — render bare words as a backticked Labels cell. Escaped backticks
# inside double quotes keep the label syntax as data, so shellcheck does not read
# it as command substitution.
lbl() {
	local out='' word
	for word in "$@"; do out="$out\`$word\` "; done
	printf '%s' "${out% }"
}

DECL="**Labels:** $(lbl tests docs ci)"

expect_file 'labels: all declared            -> clean' 0 \
	"$(labelled_fixture "$DECL" "$(lbl tests docs)" "$(lbl ci)")"
expect_file 'labels: undeclared in Queue     -> fail' 1 \
	"$(labelled_fixture "$DECL" "$(lbl tests infra)" "$(lbl ci)")"
expect_file 'labels: undeclared in Progress  -> fail' 1 \
	"$(labelled_fixture "$DECL" "$(lbl tests)" "$(lbl infra)")"
expect_file 'labels: labels but no DECL line -> fail' 1 \
	"$(labelled_fixture 'Conventions go here.' "$(lbl tests)" "$(lbl ci)")"

# A -gate label's parenthetical gloss carries its own backticked link text. That
# text names a release, not a label, so it must not enter the vocabulary.
GATE_DECL="**Labels:** $(lbl tests 2.0-gate) (blocks the [$(lbl v2.0.0)](plan/v2-ga.md) tag)"
expect_file 'labels: gate label declared     -> clean' 0 \
	"$(labelled_fixture "$GATE_DECL" "$(lbl 2.0-gate)" "$(lbl tests)")"
expect_file 'labels: gloss link text as label-> fail' 1 \
	"$(labelled_fixture "$GATE_DECL" "$(lbl v2.0.0)" "$(lbl tests)")"

# --- Rule 12: a new row's ID must hold a claim on the remote ------------------
#
# Reproduces Q656: a row was committed carrying an ID nobody had reserved, a
# second session then claimed that ID legitimately, and the collision surfaced
# only at rebase. The fixture origin is a real bare repo, so `git ls-remote`
# answers from it rather than from the network.
claim_repo="$WORKDIR/claims"
claim_origin="$WORKDIR/claims-origin.git"
mkdir -p "$claim_repo/docs"
git -C "$claim_repo" init -q
git init -q --bare "$claim_origin"

CLAIMED_ROW="$(qrow Q402 "$PLAIN_ITEM" 🔲 'claimed')"
UNCLAIMED_ROW="$(qrow Q401 "$PLAIN_ITEM" 🔲 'never reserved')"
LEGACY_ROW="$(qrow Q300 "$PLAIN_ITEM" 🔲 'predates the allocator')"

cp "$(fixture "$(qrow Q400 "$PLAIN_ITEM" 🔲 'base')")" "$claim_repo/docs/STATUS.md"
git -C "$claim_repo" add docs/STATUS.md
git -C "$claim_repo" "${git_id[@]}" commit -qm 'branch point'
git -C "$claim_repo" remote add origin "$claim_origin"
git -C "$claim_repo" push -q origin HEAD:refs/heads/main
git -C "$claim_repo" update-ref refs/remotes/origin/main HEAD

# The namespace starts at Q400, so Q300 is below the floor and Q402 is taken.
claim_sha="$(git -C "$claim_repo" rev-parse HEAD)"
git -C "$claim_origin" update-ref refs/queue-ids/Q400 "$claim_sha"
git -C "$claim_origin" update-ref refs/queue-ids/Q402 "$claim_sha"

run_claim() {
	rc=0
	(cd "$claim_repo" && "$@" "$LINT" "$claim_repo/docs/STATUS.md") >/dev/null 2>&1 || rc=$?
}

cp "$(fixture "$(qrow Q400 "$PLAIN_ITEM" 🔲 'base')" "$UNCLAIMED_ROW")" "$claim_repo/docs/STATUS.md"
run_claim env
if [[ "$rc" == 1 ]]; then printf 'ok   rule 12: new row, no claim        -> fail\n'; else
	printf 'FAIL rule 12: an unreserved new ID was not flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

run_claim env BACKLOG_ALLOW_UNCLAIMED_ID=Q401
if [[ "$rc" == 0 ]]; then printf 'ok   rule 12: BACKLOG_ALLOW_UNCLAIMED -> pass\n'; else
	printf 'FAIL rule 12: the escape hatch should allow an ID claimed elsewhere (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# A longer ID sharing Q401's prefix is not Q401's claim. An unanchored match
# would read this as reserved and pass, which is the dangerous direction.
git -C "$claim_origin" update-ref refs/queue-ids/Q4010 "$claim_sha"
run_claim env
if [[ "$rc" == 1 ]]; then printf 'ok   rule 12: prefix of a longer claim  -> fail\n'; else
	printf 'FAIL rule 12: a claim on Q4010 was accepted as a claim on Q401 (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi
git -C "$claim_origin" update-ref -d refs/queue-ids/Q4010

cp "$(fixture "$(qrow Q400 "$PLAIN_ITEM" 🔲 'base')" "$CLAIMED_ROW")" "$claim_repo/docs/STATUS.md"
run_claim env
if [[ "$rc" == 0 ]]; then printf 'ok   rule 12: new row holding a claim  -> pass\n'; else
	printf 'FAIL rule 12: a properly allocated ID was flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# IDs below the namespace's lowest claim predate the allocator and hold no ref.
cp "$(fixture "$(qrow Q400 "$PLAIN_ITEM" 🔲 'base')" "$LEGACY_ROW")" "$claim_repo/docs/STATUS.md"
run_claim env
if [[ "$rc" == 0 ]]; then printf 'ok   rule 12: ID below the namespace   -> pass\n'; else
	printf 'FAIL rule 12: an ID predating the allocator was flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# Only *new* IDs are checked: a row the baseline already carried is settled, and
# re-litigating it would fail every branch that merely touches the file.
cp "$(fixture "$UNCLAIMED_ROW")" "$claim_repo/docs/STATUS.md"
git -C "$claim_repo" add docs/STATUS.md
git -C "$claim_repo" "${git_id[@]}" commit -qm 'unclaimed row already in the baseline'
git -C "$claim_repo" update-ref refs/remotes/origin/main HEAD
run_claim env
if [[ "$rc" == 0 ]]; then printf 'ok   rule 12: unclaimed but not new    -> pass\n'; else
	printf 'FAIL rule 12: a pre-existing unclaimed row was flagged (rc=%s)\n' "$rc" >&2; fails=$((fails + 1)); fi

# --- The baseline is the merge base, not main's tip (Q684) --------------------
#
# Every git-baseline rule asks what THIS branch changed, which is a question
# about the branch point. Measured against origin/main's tip, a row main deleted
# while the branch was behind read as added here, and rule 12 demanded an ID for
# a row another session had already finished — the observed failure was
# `Q526 is a new row here but holds no refs/queue-ids/Q526 claim` on a stale
# worktree, which is indistinguishable from a genuinely broken main.
stale_repo="$WORKDIR/stale"
stale_origin="$WORKDIR/stale-origin.git"
mkdir -p "$stale_repo/docs"
git -C "$stale_repo" init -q
git init -q --bare "$stale_origin"
git -C "$stale_repo" remote add origin "$stale_origin"

BASE_ROW="$(qrow Q500 "$PLAIN_ITEM" 🔲 'base')"
DONE_ROW="$(qrow Q501 "$PLAIN_ITEM" 🔲 'ships on main while this branch is open')"
NEW_ROW="$(qrow Q502 "$PLAIN_ITEM" 🔲 'filed here, never reserved')"
# shellcheck disable=SC2016  # the backticks are markdown; rule 8 matches /`flake`/
STALE_FLAKE_ROW="$(qrow Q503 "$PLAIN_ITEM" 🔲 'flaky' '`flake`')"
MAIN_ROW="$(qrow Q504 "$PLAIN_ITEM" 🔲 'filed on main after the branch point')"

# The branch point.
cp "$(fixture "$BASE_ROW" "$DONE_ROW" "$STALE_FLAKE_ROW")" "$stale_repo/docs/STATUS.md"
git -C "$stale_repo" add docs/STATUS.md
git -C "$stale_repo" "${git_id[@]}" commit -qm 'branch point'
git -C "$stale_repo" update-ref refs/heads/branch HEAD

# main moves on: Q501 ships and its row is deleted, Q504 is filed.
cp "$(fixture "$BASE_ROW" "$STALE_FLAKE_ROW" "$MAIN_ROW")" "$stale_repo/docs/STATUS.md"
git -C "$stale_repo" add docs/STATUS.md
git -C "$stale_repo" "${git_id[@]}" commit -qm 'Q501 ships; file Q504'
git -C "$stale_repo" push -q origin HEAD:refs/heads/main
git -C "$stale_repo" update-ref refs/remotes/origin/main HEAD

# Rule 12 only checks IDs at or above the namespace's lowest claim. The claim
# points at a commit the bare origin actually carries.
stale_sha="$(git -C "$stale_repo" rev-parse HEAD)"
git -C "$stale_origin" update-ref refs/queue-ids/Q500 "$stale_sha"

# The branch is now both behind main and ahead of the branch point, which is
# what an open PR looks like. The extra commit touches another file so the
# backlog stays exactly as it was at the branch point.
git -C "$stale_repo" checkout -q branch
printf 'notes\n' >"$stale_repo/docs/NOTES.md"
git -C "$stale_repo" add docs/NOTES.md
git -C "$stale_repo" "${git_id[@]}" commit -qm 'branch work'

run_stale() {
	rc=0
	(cd "$stale_repo" && "$LINT" "$stale_repo/docs/STATUS.md") >"$WORKDIR/stale.out" 2>&1 || rc=$?
}

# The defect. The branch carries the rows it always had; Q501 is absent from
# main's tip only because main deleted it.
cp "$(fixture "$BASE_ROW" "$DONE_ROW" "$STALE_FLAKE_ROW")" "$stale_repo/docs/STATUS.md"
run_stale
if [[ "$rc" == 0 ]]; then printf 'ok   baseline: row main deleted        -> pass\n'; else
	printf 'FAIL baseline: a row main deleted was read as added by the branch (rc=%s)\n' "$rc" >&2
	cat "$WORKDIR/stale.out" >&2
	fails=$((fails + 1))
fi

# Control: rule 12 must still bite on the same stale branch. A row the branch
# genuinely adds is absent from the merge base too, and holds no claim.
cp "$(fixture "$BASE_ROW" "$DONE_ROW" "$STALE_FLAKE_ROW" "$NEW_ROW")" "$stale_repo/docs/STATUS.md"
run_stale
if [[ "$rc" == 1 ]] && grep -q 'Q502 is a new row here' "$WORKDIR/stale.out"; then
	printf 'ok   baseline: genuinely new row       -> fail\n'
else
	printf 'FAIL rule 12: an unreserved new row on a stale branch was not flagged (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

# Control: a deletion the branch itself makes is still the branch's deletion,
# even though the merge base is no longer main's tip.
cp "$(fixture "$BASE_ROW" "$DONE_ROW")" "$stale_repo/docs/STATUS.md"
run_stale
if [[ "$rc" == 1 ]] && grep -q 'Q503 was a flake-labelled Queue row' "$WORKDIR/stale.out"; then
	printf 'ok   baseline: flake row deleted here  -> fail\n'
else
	printf 'FAIL rule 8: a flake row the branch deleted was not flagged (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

# --staged asks a different question — what this COMMIT changes — so its baseline
# stays the pre-commit tree and no merge base enters it. On the same stale
# branch: the staged row is flagged, and the row main deleted is not, because
# HEAD still carries it.
cp "$(fixture "$BASE_ROW" "$DONE_ROW" "$STALE_FLAKE_ROW" "$NEW_ROW")" "$stale_repo/docs/STATUS.md"
git -C "$stale_repo" add docs/STATUS.md
rc=0
(cd "$stale_repo" && "$LINT" --staged) >"$WORKDIR/stale.out" 2>&1 || rc=$?
if [[ "$rc" == 1 ]] && grep -q 'Q502 is a new row here' "$WORKDIR/stale.out" &&
	! grep -q 'Q501 is a new row here' "$WORKDIR/stale.out"; then
	printf 'ok   baseline: --staged reads HEAD     -> fail\n'
else
	printf 'FAIL --staged: want only the staged row flagged (rc=%s)\n' "$rc" >&2
	fails=$((fails + 1))
fi

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
