#!/usr/bin/env bash
#
# Unit tests for scripts/docs/git-merge-status.sh — the docs/STATUS.md merge driver.
#
# The driver silently rewrites people's merges, so the fail-safe half matters as
# much as the resolving half: every assertion below is a real three-way merge in
# a throwaway repo, checked for BOTH the resulting row set and whether conflict
# markers were left. The cases that must resolve are the ones parallel dispatch
# produces by construction (adjacent adds and deletes at the top of the table);
# the cases that must produce markers are everything the driver cannot know.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
DRIVER="$REPO_ROOT/scripts/docs/git-merge-status.sh"
LINT="$REPO_ROOT/scripts/docs/lint-backlog.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# Assembled from a character class so this file never trips
# check-conflict-markers.sh, which scans tracked files for marker-shaped lines.
MARKER_RE='^([<]{7}|[>]{7}|[|]{7})( |$)|^[=]{7}$'

# Throwaway repos have no identity configured and must not borrow the
# developer's.
GIT_ID=(-c user.email=test@example.invalid -c user.name=test)

# --- fixtures -----------------------------------------------------------------

# qrow ID NOTES — one Queue row. Labels/St/Sz are opaque to the driver; NOTES is
# the cell the "modified" cases vary.
qrow() {
	printf '| <a id="%s"></a>%s | [Item %s](plan/p.md) | infra | 🔲 | S | %s |' \
		"$1" "$1" "$1" "$2"
}

# status_md PROGRESS_STATUS -- QUEUE_ROW... -- DEFERRED_ROW...
# A STATUS.md with a Progress table, a Queue table, and a Deferred table, so the
# regions outside the Queue rows are exercised too.
status_md() {
	local progress="$1" seen=0 arg
	shift
	local -a queue=() deferred=()
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then
			seen=$((seen + 1))
			continue
		fi
		if (( seen < 2 )); then queue+=("$arg"); else deferred+=("$arg"); fi
	done
	printf '# Project Status\n\n'
	printf 'Pick the next task from the top of the Queue.\n\n'
	printf '## Progress\n\n'
	printf '| Item | Labels | Status |\n'
	printf '|---|---|---|\n'
	printf '| [Some plan](plan/p.md) | infra | %s |\n\n' "$progress"
	printf '## Queue\n\n'
	printf 'Specific actionable items in priority order.\n\n'
	printf '| ID | Item | Labels | St | Sz | Notes |\n'
	printf '|---|---|---|---|---|---|\n'
	local row
	for row in ${queue+"${queue[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Deferred\n\n'
	printf '| ID | Item | Labels | Sz | Trigger to revive |\n'
	printf '|---|---|---|---|---|\n'
	for row in ${deferred+"${deferred[@]}"}; do printf '%s\n' "$row"; done
}

# --- the merge harness --------------------------------------------------------

# plain_repo NAME — a fresh repo carrying the committed half of the setup (the
# .gitattributes line) but NOT the per-clone driver config. Echoes its path.
# `trunk` keeps the throwaway repos off the protected branch names.
plain_repo() {
	local repo="$WORKDIR/$1"
	rm -rf "$repo"
	mkdir -p "$repo/docs"
	git -C "$repo" init -q -b trunk
	git -C "$repo" config user.email test@example.invalid
	git -C "$repo" config user.name test
	printf 'docs/STATUS.md merge=backlog\n' >"$repo/.gitattributes"
	printf '%s\n' "$repo"
}

# merge_repo NAME — plain_repo plus the driver, installed exactly the way
# `make merge-driver` installs it: a path relative to the working-tree root. That
# makes every merge below also a test of whether git resolves that relative path.
merge_repo() {
	local repo
	repo="$(plain_repo "$1")"
	mkdir -p "$repo/scripts/lib" "$repo/scripts/docs"
	cp "$DRIVER" "$repo/scripts/docs/git-merge-status.sh"
	cp "$REPO_ROOT/scripts/lib/merge-table-rows.awk" "$repo/scripts/lib/"
	cp "$REPO_ROOT/scripts/lib/merge-driver-common.sh" "$repo/scripts/lib/"
	chmod +x "$repo/scripts/docs/git-merge-status.sh"
	(cd "$repo" && ./scripts/docs/git-merge-status.sh --install >/dev/null)
	printf '%s\n' "$repo"
}

# run_merge REPO BASE OURS THEIRS — commit BASE on trunk, OURS on branch `ours`,
# THEIRS on branch `theirs`, then merge theirs into ours. Sets MERGE_RC and
# leaves the result at $REPO/docs/STATUS.md.
run_merge() {
	local repo="$1" base="$2" ours="$3" theirs="$4"
	local -a paths=(docs/STATUS.md .gitattributes)
	[[ -d "$repo/scripts" ]] && paths+=(scripts)
	cp "$base" "$repo/docs/STATUS.md"
	git -C "$repo" add "${paths[@]}"
	git -C "$repo" "${GIT_ID[@]}" commit -qm base

	# --allow-empty: a side that leaves the file untouched is a legitimate case
	# (only the other side edited it), and it still has to reach the merge.
	git -C "$repo" checkout -q -b theirs
	cp "$theirs" "$repo/docs/STATUS.md"
	git -C "$repo" "${GIT_ID[@]}" commit -qam theirs --allow-empty

	git -C "$repo" checkout -q -b ours trunk
	cp "$ours" "$repo/docs/STATUS.md"
	git -C "$repo" "${GIT_ID[@]}" commit -qam ours --allow-empty

	MERGE_RC=0
	git -C "$repo" "${GIT_ID[@]}" merge theirs >/dev/null 2>&1 || MERGE_RC=$?
}

# ids REPO — the Queue-table IDs of the merged file, in order, space-separated.
ids() {
	awk '
		/^## Queue/ { in_queue = 1; next }
		/^## /      { in_queue = 0 }
		in_queue && /^\|/ && match($0, "<a id=\"Q[0-9]+\"></a>") {
			id = substr($0, RSTART, RLENGTH)
			gsub(/[^0-9]/, "", id)
			printf "Q%s ", id
		}
	' "$1/docs/STATUS.md"
}

has_markers() {
	grep -qE "$MARKER_RE" "$1/docs/STATUS.md"
}

ok() { printf 'ok   %s\n' "$1"; }
bad() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# expect_resolved NAME WANT_IDS -- BASE_ROWS -- OURS_ROWS -- THEIRS_ROWS
# The merge must succeed, leave no markers, and produce exactly WANT_IDS in order.
# Set EXPECT_TEXT beforehand to also require a literal string in the result; it
# is consumed (reset) by the call, so it never leaks into the next assertion.
EXPECT_TEXT=''
expect_resolved() {
	local name="$1" want="$2" seen=0 arg
	shift 2
	local -a base=() ours=() theirs=()
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		1) base+=("$arg") ;;
		2) ours+=("$arg") ;;
		*) theirs+=("$arg") ;;
		esac
	done

	local repo
	repo="$(merge_repo "resolved-$RANDOM")"
	status_md ⚠️ -- ${base+"${base[@]}"} -- >"$WORKDIR/base.md"
	status_md ⚠️ -- ${ours+"${ours[@]}"} -- >"$WORKDIR/ours.md"
	status_md ⚠️ -- ${theirs+"${theirs[@]}"} -- >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"

	local got want_text="$EXPECT_TEXT"
	EXPECT_TEXT=''
	got="$(ids "$repo")"
	got="${got% }"
	if (( MERGE_RC != 0 )); then
		bad "$name: merge reported a conflict (rc=$MERGE_RC)"
	elif has_markers "$repo"; then
		bad "$name: conflict markers left behind"
	elif [[ "$got" != "$want" ]]; then
		bad "$name: rows are [$got], want [$want]"
	elif [[ -n "$want_text" ]] && ! grep -qF "$want_text" "$repo/docs/STATUS.md"; then
		bad "$name: the result does not carry the expected text: $want_text"
	else
		ok "$name"
	fi
}

# expect_conflict NAME -- BASE_ROWS -- OURS_ROWS -- THEIRS_ROWS
# The merge must fail AND leave ordinary conflict markers — never a silent pick.
expect_conflict() {
	local name="$1" seen=0 arg
	shift
	local -a base=() ours=() theirs=()
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		1) base+=("$arg") ;;
		2) ours+=("$arg") ;;
		*) theirs+=("$arg") ;;
		esac
	done

	local repo
	repo="$(merge_repo "conflict-$RANDOM")"
	status_md ⚠️ -- ${base+"${base[@]}"} -- >"$WORKDIR/base.md"
	status_md ⚠️ -- ${ours+"${ours[@]}"} -- >"$WORKDIR/ours.md"
	status_md ⚠️ -- ${theirs+"${theirs[@]}"} -- >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"

	if (( MERGE_RC == 0 )); then
		bad "$name: merge succeeded, but the outcome was not knowable"
	elif ! has_markers "$repo"; then
		bad "$name: conflict reported without conflict markers to resolve"
	else
		ok "$name"
	fi
}

Q1='| <a id="Q1"></a>Q1 | [Item Q1](plan/p.md) | infra | 🔲 | S | one |'
Q2='| <a id="Q2"></a>Q2 | [Item Q2](plan/p.md) | infra | 🔲 | S | two |'
Q3='| <a id="Q3"></a>Q3 | [Item Q3](plan/p.md) | infra | 🔲 | S | three |'
Q4='| <a id="Q4"></a>Q4 | [Item Q4](plan/p.md) | infra | 🔲 | S | four |'

# --- what dispatch produces by construction -----------------------------------

# Two workers each delete their own row from the top of the table. Adjacent
# deletes are the case a plain three-way merge cannot absorb.
expect_resolved 'delete/delete adjacent rows      -> both gone' 'Q3 Q4' \
	-- "$Q1" "$Q2" "$Q3" "$Q4" \
	-- "$Q2" "$Q3" "$Q4" \
	-- "$Q1" "$Q3" "$Q4"

# The same row deleted on both sides is one outcome, not two.
expect_resolved 'delete/delete the same row       -> gone once' 'Q2 Q3' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q2" "$Q3" \
	-- "$Q2" "$Q3"

# One worker files a new top-priority row while another deletes row 1.
expect_resolved 'add at top vs delete row 1       -> add kept, delete applied' 'Q4 Q2 Q3' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q4" "$Q1" "$Q2" "$Q3" \
	-- "$Q2" "$Q3"

# Both sides insert at the top: priority-on-entry plus flakes-first put every
# new row there, so this is the common add/add.
expect_resolved 'add/add both at the top          -> both present' 'Q3 Q4 Q1 Q2' \
	-- "$Q1" "$Q2" \
	-- "$Q3" "$Q1" "$Q2" \
	-- "$Q4" "$Q1" "$Q2"

# A row edited on one side only takes that edit, even with a neighbour deleted.
# EXPECT_TEXT additionally asserts the surviving row's content, since the ID set
# alone cannot tell an edit that survived from one that was reverted.
EXPECT_TEXT='two, reworded'
expect_resolved 'edit one side, delete neighbour  -> edit kept' 'Q2 Q3' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q1" "$(qrow Q2 'two, reworded')" "$Q3" \
	-- "$Q2" "$Q3"

# ...and symmetrically, when the edit is on the side being merged in.
EXPECT_TEXT='three, reworded'
expect_resolved 'their edit, our delete neighbour -> edit kept' 'Q2 Q3' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q2" "$Q3" \
	-- "$Q1" "$Q2" "$(qrow Q3 'three, reworded')"

# The reorder-over-delete case from maintaining-backlog.md: ours relocates rows
# while theirs deletes one. The deletion must survive the move — this is the
# merge that silently resurrects a done row without a driver.
expect_resolved 'reorder vs delete                -> no resurrection' 'Q3 Q2' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q3" "$Q1" "$Q2" \
	-- "$Q2" "$Q3"

# A row moved on one side while untouched on the other keeps the move.
expect_resolved 'reorder one side only            -> move kept' 'Q3 Q1 Q2' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q3" "$Q1" "$Q2" \
	-- "$Q1" "$Q2" "$Q3"

# Deleting every row is a legitimate (if unlikely) state, and an empty row block
# on one side must not shift the driver's idea of which file is which.
expect_resolved 'delete all vs delete one         -> empty' '' \
	-- "$Q1" "$Q2" \
	-- \
	-- "$Q1"

# --- what the driver must refuse to guess -------------------------------------

# Both sides reworded the same row: two intents, one row, no way to pick.
expect_conflict 'edit/edit the same row           -> markers' \
	-- "$Q1" "$Q2" \
	-- "$(qrow Q1 'ours rewording')" "$Q2" \
	-- "$(qrow Q1 'their rewording')" "$Q2"

# Delete versus edit. Deleting outright would drop the edit; keeping the row
# would resurrect work the other side finished. Both are guesses.
expect_conflict 'delete vs edit the same row      -> markers' \
	-- "$Q1" "$Q2" \
	-- "$(qrow Q1 'reworded while the other side shipped it')" "$Q2" \
	-- "$Q2"

# The ID allocator makes this near-impossible, but if one ID does arrive twice
# with different content the driver must not pick a winner.
expect_conflict 'same new ID, different content   -> markers' \
	-- "$Q1" \
	-- "$Q1" "$(qrow Q9 'ours')" \
	-- "$Q1" "$(qrow Q9 'theirs')"

# Both sides reshuffled the same rows differently: neither order is derivable.
expect_conflict 'reorder on both sides            -> markers' \
	-- "$Q1" "$Q2" "$Q3" \
	-- "$Q3" "$Q1" "$Q2" \
	-- "$Q2" "$Q3" "$Q1"

# A row with no anchor cannot be keyed, so the whole table is unparseable.
expect_conflict 'unparseable row (no anchor)      -> markers' \
	-- "$Q1" "$Q2" \
	-- '| Q1 | [Item Q1](plan/p.md) | infra | 🔲 | S | anchor dropped |' "$Q2" \
	-- "$Q2"

# An anchor that disagrees with the visible ID is the same class of ambiguity the
# linter rejects: cross-references resolve through the anchor.
expect_conflict 'anchor/visible ID mismatch       -> markers' \
	-- "$Q1" "$Q2" \
	-- '| <a id="Q1"></a>Q7 | [Item](plan/p.md) | infra | 🔲 | S | mismatched |' "$Q2" \
	-- "$Q2"

# --- regions outside the Queue rows -------------------------------------------

# The driver claims no knowledge of the Progress or Deferred tables: they merge
# as plain text. Independent edits on either side still merge cleanly...
progress_repo="$(merge_repo progress)"
DROW='| <a id="Q8"></a>Q8 | [Parked](plan/p.md) | infra | S | **Demand:** an operator asks. |'
status_md ⚠️ -- "$Q1" "$Q2" -- "$DROW" >"$WORKDIR/p-base.md"
status_md ✅ -- "$Q2" -- "$DROW" >"$WORKDIR/p-ours.md"
status_md ⚠️ -- "$Q1" -- "$DROW" \
	'| <a id="Q9"></a>Q9 | [Also parked](plan/p.md) | infra | S | **Event:** upstream ships it. |' \
	>"$WORKDIR/p-theirs.md"
run_merge "$progress_repo" "$WORKDIR/p-base.md" "$WORKDIR/p-ours.md" "$WORKDIR/p-theirs.md"
if (( MERGE_RC == 0 )) && ! has_markers "$progress_repo" &&
	grep -q '| \[Some plan\](plan/p.md) | infra | ✅ |' "$progress_repo/docs/STATUS.md" &&
	grep -q '<a id="Q9"></a>' "$progress_repo/docs/STATUS.md" &&
	[[ "$(ids "$progress_repo")" == "" ]]; then
	ok 'outside the rows: Progress flip + Deferred add merge'
else
	bad "outside the rows: independent Progress/Deferred edits should merge (rc=$MERGE_RC, ids=[$(ids "$progress_repo")])"
fi

# ...and a genuine conflict there is reported as one, with markers.
outside_repo="$(merge_repo outside)"
status_md ⚠️ -- "$Q1" -- >"$WORKDIR/o-base.md"
status_md ✅ -- "$Q1" -- >"$WORKDIR/o-ours.md"
status_md 🚫 -- "$Q1" -- >"$WORKDIR/o-theirs.md"
run_merge "$outside_repo" "$WORKDIR/o-base.md" "$WORKDIR/o-ours.md" "$WORKDIR/o-theirs.md"
if (( MERGE_RC != 0 )) && has_markers "$outside_repo"; then
	ok 'outside the rows: contested Progress cell -> markers'
else
	bad "outside the rows: a contested Progress cell should conflict (rc=$MERGE_RC)"
fi

# --- absence is a no-op -------------------------------------------------------

# A contributor who never ran `make merge-driver` gets git's built-in three-way
# merge: the attribute names an undefined driver, which git resolves by falling
# back, not by erroring. This is what makes the .gitattributes line safe to
# commit for everyone.
unconfigured="$(plain_repo unconfigured)"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" "$Q4" -- >"$WORKDIR/u-base.md"
status_md ⚠️ -- "$Q2" "$Q3" "$Q4" -- >"$WORKDIR/u-ours.md"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" -- >"$WORKDIR/u-theirs.md"
run_merge "$unconfigured" "$WORKDIR/u-base.md" "$WORKDIR/u-ours.md" "$WORKDIR/u-theirs.md"
if (( MERGE_RC == 0 )) && ! has_markers "$unconfigured"; then
	ok 'no driver configured: git merges non-adjacent deletes as before'
else
	bad "no driver configured: the attribute must be a no-op, not an error (rc=$MERGE_RC)"
fi

# Same repo, the adjacent case: without the driver this is the conflict that
# motivated it. Asserted so the tests above are known to be measuring the driver
# and not something git would have done anyway.
unconfigured2="$(plain_repo unconfigured2)"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" "$Q4" -- >"$WORKDIR/u2-base.md"
status_md ⚠️ -- "$Q2" "$Q3" "$Q4" -- >"$WORKDIR/u2-ours.md"
status_md ⚠️ -- "$Q1" "$Q3" "$Q4" -- >"$WORKDIR/u2-theirs.md"
run_merge "$unconfigured2" "$WORKDIR/u2-base.md" "$WORKDIR/u2-ours.md" "$WORKDIR/u2-theirs.md"
if (( MERGE_RC != 0 )); then
	ok 'no driver configured: adjacent deletes still conflict (the motivating case)'
else
	bad 'no driver configured: adjacent deletes were expected to conflict'
fi

# --- rebase, which is where the pain actually is -------------------------------

# Dispatch rebases branches onto main; a rebase replays each commit as a merge
# with ours and theirs swapped relative to `git merge`, so the symmetry of the
# set rules is what makes this work. Two workers each delete their own row from
# the top: without the driver this is the guaranteed conflict.
rebase_repo="$(merge_repo rebase)"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" -- >"$WORKDIR/r-base.md"
cp "$WORKDIR/r-base.md" "$rebase_repo/docs/STATUS.md"
git -C "$rebase_repo" add docs/STATUS.md .gitattributes scripts
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qm base
git -C "$rebase_repo" checkout -q -b main
status_md ⚠️ -- "$Q2" "$Q3" -- >"$rebase_repo/docs/STATUS.md"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'complete Q1'
git -C "$rebase_repo" checkout -q -b work trunk
status_md ⚠️ -- "$Q1" "$Q3" -- >"$rebase_repo/docs/STATUS.md"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'complete Q2'
rc=0
git -C "$rebase_repo" "${GIT_ID[@]}" rebase main >/dev/null 2>&1 || rc=$?
if (( rc == 0 )) && [[ "$(ids "$rebase_repo")" == "Q3 " ]] && ! has_markers "$rebase_repo"; then
	ok 'rebase onto main: both top-of-queue deletes applied'
else
	bad "rebase onto main should resolve by ID (rc=$rc, ids=[$(ids "$rebase_repo")])"
	git -C "$rebase_repo" rebase --abort >/dev/null 2>&1 || true
fi

# --- the driver resolves from a subdirectory ----------------------------------

# `merge.backlog.driver` holds a path relative to the working-tree root, which is
# only correct if git runs the driver from there. A merge started in a
# subdirectory is the case that would expose it.
subdir_repo="$(merge_repo subdir)"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" -- >"$WORKDIR/s-base.md"
status_md ⚠️ -- "$Q2" "$Q3" -- >"$WORKDIR/s-ours.md"
status_md ⚠️ -- "$Q1" "$Q3" -- >"$WORKDIR/s-theirs.md"
cp "$WORKDIR/s-base.md" "$subdir_repo/docs/STATUS.md"
git -C "$subdir_repo" add docs/STATUS.md .gitattributes scripts
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qm base
git -C "$subdir_repo" checkout -q -b theirs
cp "$WORKDIR/s-theirs.md" "$subdir_repo/docs/STATUS.md"
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qam theirs
git -C "$subdir_repo" checkout -q -b ours trunk
cp "$WORKDIR/s-ours.md" "$subdir_repo/docs/STATUS.md"
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qam ours
rc=0
(cd "$subdir_repo/docs" && git "${GIT_ID[@]}" merge theirs >/dev/null 2>&1) || rc=$?
if (( rc == 0 )) && [[ "$(ids "$subdir_repo")" == "Q3 " ]]; then
	ok 'merge from a subdirectory resolves the relative driver path'
else
	bad "merge from a subdirectory failed (rc=$rc, ids=[$(ids "$subdir_repo")])"
fi

# --- interaction with lint-backlog rule 10 ------------------------------------

# The resurrected-row guard is the backstop, and the two must agree. A merge in
# which `theirs` (standing in for main) deleted a row must leave a file the guard
# accepts; hand-restoring that same row must still fail it, so the guard is
# demonstrably able to see through a driver-produced merge.
guard_repo="$(merge_repo guard)"
status_md ⚠️ -- "$Q1" "$Q2" "$Q3" -- >"$WORKDIR/g-base.md"
cp "$WORKDIR/g-base.md" "$guard_repo/docs/STATUS.md"
git -C "$guard_repo" add docs/STATUS.md .gitattributes scripts
git -C "$guard_repo" "${GIT_ID[@]}" commit -qm base

# "main" ships Q1 and deletes its row.
git -C "$guard_repo" checkout -q -b main
status_md ⚠️ -- "$Q2" "$Q3" -- >"$guard_repo/docs/STATUS.md"
git -C "$guard_repo" "${GIT_ID[@]}" commit -qam 'complete Q1'
git -C "$guard_repo" update-ref refs/remotes/origin/main HEAD

# Our branch deletes the adjacent row and merges main in — the driver's case.
git -C "$guard_repo" checkout -q -b work trunk
status_md ⚠️ -- "$Q1" "$Q3" -- >"$guard_repo/docs/STATUS.md"
git -C "$guard_repo" "${GIT_ID[@]}" commit -qam 'complete Q2'
rc=0
git -C "$guard_repo" "${GIT_ID[@]}" merge main >/dev/null 2>&1 || rc=$?
lint_rc=0
(cd "$guard_repo" && "$LINT" docs/STATUS.md) >/dev/null 2>&1 || lint_rc=$?
if (( rc == 0 )) && [[ "$(ids "$guard_repo")" == "Q3 " ]] && (( lint_rc == 0 )); then
	ok 'rule 10: a driver-resolved merge does not resurrect the deleted row'
else
	bad "rule 10: driver-resolved merge should be clean and lint-clean (merge rc=$rc, ids=[$(ids "$guard_repo")], lint rc=$lint_rc)"
fi

# And the guard is not blind here: restoring Q1 by hand on the merged branch is
# exactly the resurrection it exists to catch.
status_md ⚠️ -- "$Q3" "$Q1" -- >"$guard_repo/docs/STATUS.md"
lint_rc=0
(cd "$guard_repo" && "$LINT" docs/STATUS.md) >/dev/null 2>&1 || lint_rc=$?
if (( lint_rc == 1 )); then
	ok 'rule 10: a hand-restored row is still caught after the merge'
else
	bad "rule 10: the resurrection guard should still fire post-merge (rc=$lint_rc)"
fi

# --- the real backlog file ----------------------------------------------------

# The driver's table splitter has to agree with the real file's shape, not just
# the fixtures': a no-op merge of docs/STATUS.md against itself must reproduce it
# byte for byte.
real_out="$WORKDIR/real-ours.md"
cp "$REPO_ROOT/docs/STATUS.md" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/docs/STATUS.md" "$real_out" "$REPO_ROOT/docs/STATUS.md" 7 docs/STATUS.md \
	>/dev/null 2>&1 || rc=$?
if (( rc == 0 )) && cmp -s "$real_out" "$REPO_ROOT/docs/STATUS.md"; then
	ok 'docs/STATUS.md: an identity merge is byte-identical'
else
	bad "docs/STATUS.md: identity merge changed the file or failed (rc=$rc)"
fi

# One real deletion against the real file, to prove the splitter finds the real
# table and that nothing outside the Queue rows moves.
first_id="$(ids "$REPO_ROOT" | awk '{ print $1 }')"
awk -v id="$first_id" '$0 !~ "<a id=\"" id "\"></a>"' "$REPO_ROOT/docs/STATUS.md" \
	>"$WORKDIR/real-theirs.md"
cp "$REPO_ROOT/docs/STATUS.md" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/docs/STATUS.md" "$real_out" "$WORKDIR/real-theirs.md" 7 docs/STATUS.md \
	>/dev/null 2>&1 || rc=$?
if (( rc == 0 )) && cmp -s "$real_out" "$WORKDIR/real-theirs.md"; then
	ok "docs/STATUS.md: deleting $first_id one-sided reproduces that file"
else
	bad "docs/STATUS.md: a one-sided real deletion did not apply cleanly (rc=$rc)"
fi

if (( fails > 0 )); then
	printf '\ngit-merge-status-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\ngit-merge-status-test: ok\n'
