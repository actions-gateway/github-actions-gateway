#!/usr/bin/env bash
#
# Unit tests for scripts/docs/git-merge-plan-index.sh — the docs/plan/README.md
# merge driver.
#
# The driver silently rewrites people's merges, so the fail-safe half matters as
# much as the resolving half: every assertion below is a real three-way merge in
# a throwaway repo, checked for BOTH the resulting row set and whether conflict
# markers were left. The cases that must resolve are the ones the plan workflow
# produces by construction (a new plan row and an archival landing in the same
# window); the cases that must produce markers are everything the driver cannot
# know.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
DRIVER="$REPO_ROOT/scripts/docs/git-merge-plan-index.sh"
INDEX_CHECK="$REPO_ROOT/scripts/docs/check-plan-index.sh"
TARGET='docs/plan/README.md'

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Q820: under the parallel runner this suite occasionally dies on a git
# temp-file error whose message names no command, and it passes on rerun, so the
# next occurrence is the only chance to see which call failed. errtrace carries
# the ERR trap into run_merge, whose two commits it would otherwise miss.
set -o errtrace

q820_dirstate() {
	if [[ -d "$1" ]]; then printf 'yes'; else printf 'no'; fi
}

# q820_report RC LINE COMMAND — the ERR trap. Names the failing call, then
# records whether the throwaway trees are still on disk. git reaches the Q820
# signature from its loose-object write path, which it enters long after
# validating the repository, so the message itself cannot say whether anything
# was removed; only a reading taken here can. RC/LINE/COMMAND are arguments
# because $?, $LINENO and $BASH_COMMAND would all read this function's frame.
q820_report() {
	local repo
	trap - ERR
	printf '%s:%s: FAILED (rc=%s): %s\n' "${BASH_SOURCE[0]##*/}" "$2" "$1" "$3" >&2
	printf 'Q820: WORKDIR=%s present=%s\n' "$WORKDIR" "$(q820_dirstate "$WORKDIR")" >&2
	for repo in "$WORKDIR"/*/; do
		[[ -d "$repo" ]] || continue
		printf 'Q820:   %s .git=%s objects=%s\n' "${repo%/}" \
			"$(q820_dirstate "$repo.git")" "$(q820_dirstate "$repo.git/objects")" >&2
	done
}
trap 'q820_report "$?" "$LINENO" "$BASH_COMMAND"' ERR

fails=0

# Assembled from a character class so this file never trips
# check-conflict-markers.sh, which scans tracked files for marker-shaped lines.
MARKER_RE='^([<]{7}|[>]{7}|[|]{7})( |$)|^[=]{7}$'

# Throwaway repos have no identity configured and must not borrow the
# developer's.
GIT_ID=(-c user.email=test@example.invalid -c user.name=test)

# --- fixtures -----------------------------------------------------------------

# prow NAME SCOPE STATUS — one active index row, keyed on the plan path.
prow() {
	printf '| [%s](%s) | %s | %s |' "$1" "$1" "$2" "$3"
}

# arow NAME CLOSED — one Archive row. The key carries the archive/ prefix, which
# is what makes archiving a delete in one table and an add in another.
arow() {
	printf '| [archive/%s](archive/%s) | scope of %s | %s |' "$1" "$1" "$1" "$2"
}

# plan_index CROSS_ROWS -- SECURITY_ROWS -- ARCHIVE_ROWS
# A README with three tables and prose between them, so the regions outside the
# rows are exercised too.
plan_index() {
	local seen=0 arg
	local -a cross=() security=() archive=()
	for arg in "$@"; do
		if [[ "$arg" == "--" ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		0) cross+=("$arg") ;;
		1) security+=("$arg") ;;
		*) archive+=("$arg") ;;
		esac
	done
	local row
	printf '# Plans\n\n'
	printf 'Topic-organized index of plan files.\n\n'
	printf '## Cross-cutting\n\n'
	printf '| Plan | Scope | Status |\n|---|---|---|\n'
	for row in ${cross+"${cross[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Security\n\n'
	printf '| Plan | Scope | Status |\n|---|---|---|\n'
	for row in ${security+"${security[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Archive\n\n'
	printf 'Plans whose work has fully landed.\n\n'
	printf '| Plan | Scope | Closed |\n|---|---|---|\n'
	for row in ${archive+"${archive[@]}"}; do printf '%s\n' "$row"; done
	printf '\n## Conventions\n\n'
	printf 'Add a row when creating, completing, or archiving a plan.\n'
}

# --- the merge harness --------------------------------------------------------

# no_auto_maintenance REPO — stop this throwaway repo running background git.
#
# Q820: every commit otherwise spawns `git maintenance run --auto --detach`, and
# roughly nine per suite run reach `git repack --cruft`, pruning the objects they
# packed and removing each fanout directory that empties. Detached, that gc
# outlives its commit and runs while the next command writes to the same repo; a
# prune landing between git's `mkdir` and the `open` under it fails the write
# with `unable to create temporary file`, exit 128. A fixture repo has nothing to
# maintain, so the fix is not to start it.
no_auto_maintenance() {
	git -C "$1" config maintenance.auto false
}

# plain_repo NAME — a fresh repo carrying the committed half of the setup (the
# .gitattributes line) but NOT the per-clone driver config. Echoes its path.
# `trunk` keeps the throwaway repos off the protected branch names.
plain_repo() {
	local repo="$WORKDIR/$1"
	rm -rf "$repo"
	mkdir -p "$repo/docs/plan"
	git -C "$repo" init -q -b trunk
	git -C "$repo" config user.email test@example.invalid
	git -C "$repo" config user.name test
	no_auto_maintenance "$repo"
	printf '%s merge=planindex\n' "$TARGET" >"$repo/.gitattributes"
	printf '%s\n' "$repo"
}

# merge_repo NAME — plain_repo plus the driver, installed exactly the way
# `make merge-driver` installs it: a path relative to the working-tree root. That
# makes every merge below also a test of whether git resolves that relative path.
merge_repo() {
	local repo
	repo="$(plain_repo "$1")"
	mkdir -p "$repo/scripts/lib" "$repo/scripts/docs"
	cp "$DRIVER" "$repo/scripts/docs/git-merge-plan-index.sh"
	cp "$REPO_ROOT/scripts/lib/merge-keyed-records.awk" "$repo/scripts/lib/"
	cp "$REPO_ROOT/scripts/lib/merge-driver-common.sh" "$repo/scripts/lib/"
	chmod +x "$repo/scripts/docs/git-merge-plan-index.sh"
	(cd "$repo" && ./scripts/docs/git-merge-plan-index.sh --install >/dev/null)
	printf '%s\n' "$repo"
}

MERGE_LOG="$WORKDIR/merge.log"

# run_merge REPO BASE OURS THEIRS — commit BASE on trunk, OURS on branch `ours`,
# THEIRS on branch `theirs`, then merge theirs into ours. Sets MERGE_RC, writes
# the merge's combined output to MERGE_LOG, and leaves the result at
# $REPO/$TARGET.
run_merge() {
	local repo="$1" base="$2" ours="$3" theirs="$4"
	local -a paths=("$TARGET" .gitattributes)
	[[ -d "$repo/scripts" ]] && paths+=(scripts)
	cp "$base" "$repo/$TARGET"
	git -C "$repo" add "${paths[@]}"
	git -C "$repo" "${GIT_ID[@]}" commit -qm base

	# --allow-empty: a side that leaves the file untouched is a legitimate case
	# (only the other side edited it), and it still has to reach the merge.
	git -C "$repo" checkout -q -b theirs
	cp "$theirs" "$repo/$TARGET"
	git -C "$repo" "${GIT_ID[@]}" commit -qam theirs --allow-empty

	git -C "$repo" checkout -q -b ours trunk
	cp "$ours" "$repo/$TARGET"
	git -C "$repo" "${GIT_ID[@]}" commit -qam ours --allow-empty

	MERGE_RC=0
	git -C "$repo" "${GIT_ID[@]}" merge theirs >"$MERGE_LOG" 2>&1 || MERGE_RC=$?
}

# keys FILE — every row's plan path, in file order, space-separated. This is the
# same column-1 link target scripts/docs/check-plan-index.sh reads.
keys() {
	awk '
		/^\|/ {
			n = split($0, f, "|")
			if (n >= 3 && match(f[2], /\[[^]]*\]\([^)]+\)/)) {
				k = substr(f[2], RSTART, RLENGTH)
				sub(/^\[[^]]*\]\(/, "", k)
				sub(/\)$/, "", k)
				printf "%s ", k
			}
		}
	' "$1"
}

has_markers() {
	grep -qE "$MARKER_RE" "$1/$TARGET"
}

ok() { printf 'ok   %s\n' "$1"; }
bad() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# diag LOG — the output a failing call produced, indented under the assertion.
# Interpolate it into a bad() message; the command substitution eats the
# trailing newline.
#
# Every assertion below reports this, because an exit code does not classify a
# git failure on its own. Measured on git 2.55.0: 1 is a conflict but also an
# unmergeable ref, 2 is the merge strategy failing (a failed object write, a
# dirty worktree), and 128 is git dying before the strategy runs (a held
# index.lock, a merge already in progress). Only the output separates them, and
# a conflict writes its report to stdout rather than stderr.
diag() {
	if [[ -s "$1" ]]; then
		printf '\n'
		sed 's/^/     | /' "$1"
	else
		printf ' (no output)'
	fi
}

# expect_resolved NAME WANT_KEYS -- BASE -- OURS -- THEIRS
# Each of the three sides is a `plan_index` argument list. The merge must
# succeed, leave no markers, and produce exactly WANT_KEYS in order. Set
# EXPECT_TEXT beforehand to also require a literal string in the result; it is
# consumed (reset) by the call, so it never leaks into the next assertion.
EXPECT_TEXT=''
split_sides() {
	SIDE_BASE=(); SIDE_OURS=(); SIDE_THEIRS=()
	local seen=0 arg
	for arg in "$@"; do
		if [[ "$arg" == '===' ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		1) SIDE_BASE+=("$arg") ;;
		2) SIDE_OURS+=("$arg") ;;
		*) SIDE_THEIRS+=("$arg") ;;
		esac
	done
}

expect_resolved() {
	local name="$1" want="$2"
	shift 2
	split_sides "$@"
	local repo
	repo="$(merge_repo "resolved-$RANDOM")"
	plan_index "${SIDE_BASE[@]}" >"$WORKDIR/base.md"
	plan_index "${SIDE_OURS[@]}" >"$WORKDIR/ours.md"
	plan_index "${SIDE_THEIRS[@]}" >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"

	local got want_text="$EXPECT_TEXT"
	EXPECT_TEXT=''
	got="$(keys "$repo/$TARGET")"
	got="${got% }"
	if (( MERGE_RC == 1 )); then
		bad "$name: merge conflicted (rc=1)$(diag "$MERGE_LOG")"
	elif (( MERGE_RC != 0 )); then
		bad "$name: git merge failed without conflicting (rc=$MERGE_RC)$(diag "$MERGE_LOG")"
	elif has_markers "$repo"; then
		bad "$name: conflict markers left behind"
	elif [[ "$got" != "$want" ]]; then
		bad "$name: rows are [$got], want [$want]"
	elif [[ -n "$want_text" ]] && ! grep -qF "$want_text" "$repo/$TARGET"; then
		bad "$name: the result does not carry the expected text: $want_text"
	else
		ok "$name"
	fi
}

# expect_conflict NAME -- BASE -- OURS -- THEIRS
# The merge must fail AND leave ordinary conflict markers — never a silent pick.
expect_conflict() {
	local name="$1"
	shift
	split_sides "$@"
	local repo
	repo="$(merge_repo "conflict-$RANDOM")"
	plan_index "${SIDE_BASE[@]}" >"$WORKDIR/base.md"
	plan_index "${SIDE_OURS[@]}" >"$WORKDIR/ours.md"
	plan_index "${SIDE_THEIRS[@]}" >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"

	if (( MERGE_RC == 0 )); then
		bad "$name: merge succeeded, but the outcome was not knowable"
	elif (( MERGE_RC != 1 )); then
		bad "$name: git merge failed instead of conflicting (rc=$MERGE_RC)$(diag "$MERGE_LOG")"
	elif ! has_markers "$repo"; then
		bad "$name: conflict reported without conflict markers to resolve$(diag "$MERGE_LOG")"
	else
		ok "$name"
	fi
}

A="$(prow a.md 'scope of a' '❌ Open')"
B="$(prow b.md 'scope of b' '⚠️ Partial')"
C="$(prow c.md 'scope of c' '✅ Done')"
D="$(prow d.md 'scope of d' '❌ Open')"
SEC="$(prow sec.md 'scope of sec' '✅ Done')"
ARCH="$(arow old.md '2026-07-01 — Q1')"

# --- what the plan workflow produces by construction ---------------------------

# Two branches each file a plan and append its row to the same table. Adjacent
# adds are the case a plain three-way merge cannot absorb, and the one the Q611
# history measurement found four times in a single day.
expect_resolved 'add/add adjacent rows            -> both present' \
	'a.md b.md c.md d.md sec.md archive/old.md' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH" \
	=== "$A" "$B" "$D" -- "$SEC" -- "$ARCH"

# One branch archives a plan while another files one. Archival is a delete in
# the active table and an add at the top of Archive; the two tables merge
# independently and neither half needs to know about the other.
expect_resolved 'archive vs file a new plan       -> both applied' \
	'a.md c.md sec.md archive/b.md archive/old.md' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" -- "$SEC" -- "$(arow b.md '2026-08-03 — Q2')" "$ARCH" \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH"

# Two branches archive two different plans in the same window: adjacent deletes
# in one table and adjacent adds at the top of another, at once.
expect_resolved 'archive/archive two plans        -> both moved' \
	'c.md sec.md archive/a.md archive/b.md archive/old.md' \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH" \
	=== "$B" "$C" -- "$SEC" -- "$(arow a.md '2026-08-03 — Q2')" "$ARCH" \
	=== "$A" "$C" -- "$SEC" -- "$(arow b.md '2026-08-03 — Q3')" "$ARCH"

# A row's status text updated on one side only, with a neighbour deleted on the
# other. EXPECT_TEXT asserts the surviving row's content, since the key set alone
# cannot tell an edit that survived from one that was reverted.
EXPECT_TEXT='✅ Done — everything shipped'
expect_resolved 'edit one side, delete neighbour  -> edit kept' \
	'b.md sec.md archive/old.md' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" "$(prow b.md 'scope of b' '✅ Done — everything shipped')" -- "$SEC" -- "$ARCH" \
	=== "$B" -- "$SEC" -- "$ARCH"

# A row moved within its table on one side, untouched on the other, keeps the
# move — reordering a section is an ordinary grooming edit here.
expect_resolved 'reorder one side only            -> move kept' \
	'c.md a.md b.md sec.md archive/old.md' \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH" \
	=== "$C" "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH"

# Rows added to two different tables in the same merge.
expect_resolved 'adds in two different tables     -> both present' \
	'a.md c.md sec.md d.md archive/old.md' \
	=== "$A" -- "$SEC" -- "$ARCH" \
	=== "$A" "$C" -- "$SEC" -- "$ARCH" \
	=== "$A" -- "$SEC" "$(prow d.md 'scope of d' '❌ Open')" -- "$ARCH"

# --- what the driver must refuse to guess -------------------------------------

# Both sides rewrote the same plan's status: two intents, one row.
expect_conflict 'edit/edit the same row           -> markers' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$(prow a.md 'scope of a' '✅ ours')" "$B" -- "$SEC" -- "$ARCH" \
	=== "$(prow a.md 'scope of a' '⚠️ theirs')" "$B" -- "$SEC" -- "$ARCH"

# One side archived a plan while the other rewrote its status in place. Taking
# the archival would drop the edit; taking the edit would un-archive the plan
# and fail `make plan-index-check`. Both are guesses.
expect_conflict 'archive vs edit the same row     -> markers' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" "$(prow b.md 'scope of b' '⚠️ still open, actually')" -- "$SEC" -- "$ARCH" \
	=== "$A" -- "$SEC" -- "$(arow b.md '2026-08-03 — Q2')" "$ARCH"

# The same plan filed on both sides with different scope text.
expect_conflict 'same plan added twice            -> markers' \
	=== "$A" -- "$SEC" -- "$ARCH" \
	=== "$A" "$(prow c.md 'ours scope' '❌ Open')" -- "$SEC" -- "$ARCH" \
	=== "$A" "$(prow c.md 'theirs scope' '❌ Open')" -- "$SEC" -- "$ARCH"

# Both sides reshuffled the same table differently: neither order is derivable.
expect_conflict 'reorder on both sides            -> markers' \
	=== "$A" "$B" "$C" -- "$SEC" -- "$ARCH" \
	=== "$C" "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$B" "$C" "$A" -- "$SEC" -- "$ARCH"

# A row whose first cell is not a link has no key, so the table is unparseable.
expect_conflict 'unkeyable row (no link)          -> markers' \
	=== "$A" "$B" -- "$SEC" -- "$ARCH" \
	=== "$A" '| a plan with no link | scope | ❌ Open |' -- "$SEC" -- "$ARCH" \
	=== "$B" -- "$SEC" -- "$ARCH"

# One side moved a plan to Archive while the other moved it to a different active
# table. Each table resolves on its own, so only the whole-file uniqueness check
# can see that the plan now has two rows. git's own merge is clean here (the two
# adds land in different regions), so the driver cannot leave markers — what it
# must do is refuse to claim the resolution and hand back exactly what git would
# have produced, which check-plan-index.sh then rejects for the moved file.
plan_index "$A" "$B" -- "$SEC" -- "$ARCH" >"$WORKDIR/dup-base.md"
plan_index "$A" -- "$SEC" "$B" -- "$ARCH" >"$WORKDIR/dup-ours.md"
plan_index "$A" -- "$SEC" -- "$(arow b.md '2026-08-03 — Q2')" "$ARCH" >"$WORKDIR/dup-theirs.md"
cp "$WORKDIR/dup-ours.md" "$WORKDIR/dup-driver.md"
rc=0
"$DRIVER" "$WORKDIR/dup-base.md" "$WORKDIR/dup-driver.md" "$WORKDIR/dup-theirs.md" \
	7 "$TARGET" 2>"$WORKDIR/dup.err" || rc=$?
cp "$WORKDIR/dup-ours.md" "$WORKDIR/dup-git.md"
git merge-file "$WORKDIR/dup-git.md" "$WORKDIR/dup-base.md" "$WORKDIR/dup-theirs.md" >/dev/null 2>&1 || true
if (( rc == 0 )) && grep -q 'lists a plan more than once: b.md' "$WORKDIR/dup.err" &&
	cmp -s "$WORKDIR/dup-driver.md" "$WORKDIR/dup-git.md"; then
	ok 'moved to two tables at once     -> refused, handed back to git'
else
	bad "one plan in two tables should fall back to git verbatim (rc=$rc, err=$(cat "$WORKDIR/dup.err"))"
fi

# --- a side that restructures the index ---------------------------------------

# Adding a whole section is rare but legal, and it breaks the per-table pairing
# the driver depends on. It must fall back rather than pair the wrong tables:
# with a genuinely independent edit on the other side, git's own merge is clean.
restructure="$(merge_repo restructure)"
plan_index "$A" -- "$SEC" -- "$ARCH" >"$WORKDIR/x-base.md"
{
	plan_index "$A" -- "$SEC" -- "$ARCH"
	printf '\n## Deployment\n\n| Plan | Scope | Status |\n|---|---|---|\n%s\n' "$C"
} >"$WORKDIR/x-ours.md"
plan_index "$A" "$B" -- "$SEC" -- "$ARCH" >"$WORKDIR/x-theirs.md"
run_merge "$restructure" "$WORKDIR/x-base.md" "$WORKDIR/x-ours.md" "$WORKDIR/x-theirs.md"
if (( MERGE_RC == 0 )) && ! has_markers "$restructure" &&
	[[ "$(keys "$restructure/$TARGET")" == 'a.md b.md sec.md archive/old.md c.md ' ]]; then
	ok 'a side adds a whole table       -> falls back to git, which merges it'
else
	bad "a new table should fall back to git's merge (rc=$MERGE_RC, keys=[$(keys "$restructure/$TARGET")])$(diag "$MERGE_LOG")"
fi

# --- regions outside the rows -------------------------------------------------

# Prose between the tables merges as plain text, and a genuine conflict there is
# reported as one, with markers.
prose_repo="$(merge_repo prose)"
plan_index "$A" -- "$SEC" -- "$ARCH" >"$WORKDIR/p-base.md"
sed 's/^Topic-organized index of plan files\./Our new intro./' \
	"$WORKDIR/p-base.md" >"$WORKDIR/p-ours.md"
sed 's/^Topic-organized index of plan files\./Their new intro./' \
	"$WORKDIR/p-base.md" >"$WORKDIR/p-theirs.md"
run_merge "$prose_repo" "$WORKDIR/p-base.md" "$WORKDIR/p-ours.md" "$WORKDIR/p-theirs.md"
if (( MERGE_RC == 1 )) && has_markers "$prose_repo"; then
	ok 'outside the rows: contested prose -> markers'
else
	bad "outside the rows: a contested intro line should conflict (rc=$MERGE_RC)$(diag "$MERGE_LOG")"
fi

# --- absence is a no-op -------------------------------------------------------

# A contributor who never ran `make merge-driver` gets git's built-in three-way
# merge: the attribute names an undefined driver, which git resolves by falling
# back, not by erroring. This is what makes the .gitattributes line safe to
# commit for everyone.
unconfigured="$(plain_repo unconfigured)"
plan_index "$A" "$B" "$C" "$D" -- "$SEC" -- "$ARCH" >"$WORKDIR/u-base.md"
plan_index "$A" "$B" "$C" -- "$SEC" -- "$ARCH" >"$WORKDIR/u-ours.md"
plan_index "$A" "$B" "$C" "$D" -- "$SEC" -- "$(arow old2.md '2026-08-03 — Q2')" "$ARCH" \
	>"$WORKDIR/u-theirs.md"
run_merge "$unconfigured" "$WORKDIR/u-base.md" "$WORKDIR/u-ours.md" "$WORKDIR/u-theirs.md"
if (( MERGE_RC == 0 )) && ! has_markers "$unconfigured"; then
	ok 'no driver configured: git merges distant edits as before'
else
	bad "no driver configured: the attribute must be a no-op, not an error (rc=$MERGE_RC)$(diag "$MERGE_LOG")"
fi

# Same repo, the adjacent case: without the driver this is the conflict that
# motivated it. Asserted so the tests above are known to be measuring the driver
# and not something git would have done anyway.
unconfigured2="$(plain_repo unconfigured2)"
plan_index "$A" "$B" -- "$SEC" -- "$ARCH" >"$WORKDIR/u2-base.md"
plan_index "$A" "$B" "$C" -- "$SEC" -- "$ARCH" >"$WORKDIR/u2-ours.md"
plan_index "$A" "$B" "$D" -- "$SEC" -- "$ARCH" >"$WORKDIR/u2-theirs.md"
run_merge "$unconfigured2" "$WORKDIR/u2-base.md" "$WORKDIR/u2-ours.md" "$WORKDIR/u2-theirs.md"
if (( MERGE_RC == 1 )); then
	ok 'no driver configured: adjacent adds still conflict (the motivating case)'
else
	bad "no driver configured: adjacent adds were expected to conflict (rc=$MERGE_RC)$(diag "$MERGE_LOG")"
fi

# --- rebase, which is where the pain actually is -------------------------------

# Branches rebase onto main; a rebase replays each commit as a merge with ours
# and theirs swapped relative to `git merge`, so the symmetry of the set rules is
# what makes this work.
rebase_repo="$(merge_repo rebase)"
plan_index "$A" "$B" -- "$SEC" -- "$ARCH" >"$WORKDIR/r-base.md"
cp "$WORKDIR/r-base.md" "$rebase_repo/$TARGET"
git -C "$rebase_repo" add "$TARGET" .gitattributes scripts
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qm base
git -C "$rebase_repo" checkout -q -b main
plan_index "$A" "$B" "$C" -- "$SEC" -- "$ARCH" >"$rebase_repo/$TARGET"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'file c'
git -C "$rebase_repo" checkout -q -b work trunk
plan_index "$A" "$B" "$D" -- "$SEC" -- "$ARCH" >"$rebase_repo/$TARGET"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'file d'
rc=0
git -C "$rebase_repo" "${GIT_ID[@]}" rebase main >"$WORKDIR/rebase.log" 2>&1 || rc=$?
if (( rc == 0 )) && [[ "$(keys "$rebase_repo/$TARGET")" == 'a.md b.md c.md d.md sec.md archive/old.md ' ]] &&
	! has_markers "$rebase_repo"; then
	ok 'rebase onto main: both new plan rows applied'
else
	bad "rebase onto main should resolve by plan path (rc=$rc, keys=[$(keys "$rebase_repo/$TARGET")])$(diag "$WORKDIR/rebase.log")"
	git -C "$rebase_repo" rebase --abort >/dev/null 2>&1 || true
fi

# --- the driver resolves from a subdirectory ----------------------------------

# `merge.planindex.driver` holds a path relative to the working-tree root, which
# is only correct if git runs the driver from there. A merge started in a
# subdirectory is the case that would expose it.
subdir_repo="$(merge_repo subdir)"
plan_index "$A" "$B" -- "$SEC" -- "$ARCH" >"$WORKDIR/s-base.md"
plan_index "$A" "$B" "$C" -- "$SEC" -- "$ARCH" >"$WORKDIR/s-ours.md"
plan_index "$A" "$B" "$D" -- "$SEC" -- "$ARCH" >"$WORKDIR/s-theirs.md"
cp "$WORKDIR/s-base.md" "$subdir_repo/$TARGET"
git -C "$subdir_repo" add "$TARGET" .gitattributes scripts
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qm base
git -C "$subdir_repo" checkout -q -b theirs
cp "$WORKDIR/s-theirs.md" "$subdir_repo/$TARGET"
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qam theirs
git -C "$subdir_repo" checkout -q -b ours trunk
cp "$WORKDIR/s-ours.md" "$subdir_repo/$TARGET"
git -C "$subdir_repo" "${GIT_ID[@]}" commit -qam ours
rc=0
(cd "$subdir_repo/docs/plan" && git "${GIT_ID[@]}" merge theirs >"$MERGE_LOG" 2>&1) || rc=$?
if (( rc == 0 )) && [[ "$(keys "$subdir_repo/$TARGET")" == 'a.md b.md c.md d.md sec.md archive/old.md ' ]]; then
	ok 'merge from a subdirectory resolves the relative driver path'
else
	bad "merge from a subdirectory failed (rc=$rc, keys=[$(keys "$subdir_repo/$TARGET")])$(diag "$MERGE_LOG")"
fi

# --- the real plan index ------------------------------------------------------

# The driver's table splitter has to agree with the real file's shape, not just
# the fixtures': a no-op merge of docs/plan/README.md against itself must
# reproduce it byte for byte.
real_out="$WORKDIR/real-ours.md"
cp "$REPO_ROOT/$TARGET" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/$TARGET" "$real_out" "$REPO_ROOT/$TARGET" 7 "$TARGET" \
	>"$WORKDIR/real-identity.log" 2>&1 || rc=$?
if (( rc == 0 )) && cmp -s "$real_out" "$REPO_ROOT/$TARGET"; then
	ok "$TARGET: an identity merge is byte-identical"
else
	bad "$TARGET: identity merge changed the file or failed (rc=$rc)$(diag "$WORKDIR/real-identity.log")"
fi

# One real deletion against the real file, to prove the splitter finds every real
# table and that nothing outside the rows moves.
first_key="$(keys "$REPO_ROOT/$TARGET" | awk '{ print $1 }')"
grep -vF "]($first_key)" "$REPO_ROOT/$TARGET" >"$WORKDIR/real-theirs.md"
cp "$REPO_ROOT/$TARGET" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/$TARGET" "$real_out" "$WORKDIR/real-theirs.md" 7 "$TARGET" \
	>"$WORKDIR/real-delete.log" 2>&1 || rc=$?
if (( rc == 0 )) && cmp -s "$real_out" "$WORKDIR/real-theirs.md"; then
	ok "$TARGET: deleting $first_key one-sided reproduces that file"
else
	bad "$TARGET: a one-sided real deletion did not apply cleanly (rc=$rc)$(diag "$WORKDIR/real-delete.log")"
fi

# --- agreement with the gate --------------------------------------------------

# check-plan-index.sh keys rows on the same column-1 link. If the two ever read
# the file differently the driver could produce an index the gate rejects, so the
# agreement is asserted on the real file, through a real merge: two branches each
# file a plan directly under the same table header — the neighbour position that
# conflicts by construction — and the gate runs over the merged tree.
#
# The fixture is a replica of this repo, so it has to carry whatever the gate
# reads. That set is not stable: Q889 changed it twice in one cutover, gaining
# docs/queue/ in phase 3 and losing docs/STATUS.md in phase 6, and both times
# the only thing that noticed was this suite going red -- a suite named for the
# driver, which nobody editing the gate has reason to open. So the set is read
# out of the gate rather than restated here.
#
# Read as text, never run: this suite asserts the driver, and asking the gate
# what it needs would let the fixture be defined by a script the same block
# then checks the merged tree against.
gate_inputs() {
	awk 'match($0, /^[a-z_]+="\$repo_root\/[^"]+"$/) {
		path = $0
		sub(/^[a-z_]+="\$repo_root\//, "", path)
		sub(/"$/, "", path)
		print path
	}' "$INDEX_CHECK" | sort -u
}

gate_repo="$WORKDIR/gate"
rm -rf "$gate_repo"
mkdir -p "$gate_repo/scripts/lib" "$gate_repo/scripts/docs"

mapfile -t GATE_INPUTS < <(gate_inputs)
# An awk that stopped matching reads no paths, copies nothing, and leaves the
# gate to fail for want of its inputs -- red, but attributed to the index rather
# than to this reader. The empty set is therefore its own assertion.
if (( ${#GATE_INPUTS[@]} == 0 )); then
	bad "gate inputs: no \$repo_root path found in ${INDEX_CHECK##*/}"
fi
for gate_input in ${GATE_INPUTS+"${GATE_INPUTS[@]}"}; do
	if [[ -d "$REPO_ROOT/$gate_input" ]]; then
		mkdir -p "$gate_repo/$gate_input"
		cp -R "$REPO_ROOT/$gate_input/." "$gate_repo/$gate_input/"
	elif [[ -f "$REPO_ROOT/$gate_input" ]]; then
		mkdir -p "$gate_repo/${gate_input%/*}"
		cp "$REPO_ROOT/$gate_input" "$gate_repo/$gate_input"
	else
		bad "gate inputs: ${INDEX_CHECK##*/} reads $gate_input, absent from this repo"
	fi
done
cp "$DRIVER" "$gate_repo/scripts/docs/git-merge-plan-index.sh"
cp "$INDEX_CHECK" "$gate_repo/scripts/docs/check-plan-index.sh"
cp "$REPO_ROOT/scripts/lib/merge-keyed-records.awk" "$gate_repo/scripts/lib/"
cp "$REPO_ROOT/scripts/lib/merge-driver-common.sh" "$gate_repo/scripts/lib/"
# The gate sources common.sh for resolve_release_tag (Q812), and resolves it from
# its own location, so the copy has to come along or it dies before it checks
# anything — which the baseline below reads as a red tree.
cp "$REPO_ROOT/scripts/lib/common.sh" "$gate_repo/scripts/lib/"
chmod +x "$gate_repo/scripts/docs/git-merge-plan-index.sh" "$gate_repo/scripts/docs/check-plan-index.sh"
printf '%s merge=planindex\n' "$TARGET" >"$gate_repo/.gitattributes"
git -C "$gate_repo" init -q -b trunk
git -C "$gate_repo" config user.email test@example.invalid
git -C "$gate_repo" config user.name test
no_auto_maintenance "$gate_repo"
(cd "$gate_repo" && ./scripts/docs/git-merge-plan-index.sh --install >/dev/null)
git -C "$gate_repo" add -A
git -C "$gate_repo" "${GIT_ID[@]}" commit -qm base

# Baseline: the gate must pass on the untouched copy, or the assertion below
# proves nothing.
base_gate_rc=0
(cd "$gate_repo" && ./scripts/docs/check-plan-index.sh) >"$WORKDIR/gate-base.log" 2>&1 || base_gate_rc=$?

# file_plan REPO NAME — add a plan file plus its README row directly under the
# first table's separator. ⓘ so invariant 1 (must stay STATUS-referenced) does
# not apply; the file on disk satisfies invariant 2.
file_plan() {
	printf '# %s\n\nScope.\n' "$2" >"$1/docs/plan/$2.md"
	awk -v name="$2" '
		!placed && /^\|---\|---\|---\|$/ {
			print
			printf "| [%s.md](%s.md) | a newly filed plan | \342\223\230 Design reference |\n", name, name
			placed = 1
			next
		}
		{ print }
	' "$1/$TARGET" >"$1/$TARGET.new"
	mv "$1/$TARGET.new" "$1/$TARGET"
}

git -C "$gate_repo" checkout -q -b theirs
file_plan "$gate_repo" zz-their-plan
git -C "$gate_repo" add -A
git -C "$gate_repo" "${GIT_ID[@]}" commit -qm 'file their plan'

git -C "$gate_repo" checkout -q -b ours trunk
file_plan "$gate_repo" zz-our-plan
git -C "$gate_repo" add -A
git -C "$gate_repo" "${GIT_ID[@]}" commit -qm 'file our plan'

merge_rc=0
git -C "$gate_repo" "${GIT_ID[@]}" merge theirs >"$MERGE_LOG" 2>&1 || merge_rc=$?
gate_rc=0
(cd "$gate_repo" && ./scripts/docs/check-plan-index.sh) >"$WORKDIR/gate-merged.log" 2>&1 || gate_rc=$?
if (( base_gate_rc == 0 )) && (( merge_rc == 0 )) && (( gate_rc == 0 )) &&
	grep -q '](zz-our-plan.md)' "$gate_repo/$TARGET" &&
	grep -q '](zz-their-plan.md)' "$gate_repo/$TARGET"; then
	ok 'check-plan-index.sh accepts a driver-resolved adjacent-add merge'
else
	# Three calls, three logs: which one is non-zero says which to read, and a
	# baseline that failed means the merge assertion proved nothing.
	bad "gate agreement: base=$base_gate_rc merge=$merge_rc gate=$gate_rc
   baseline gate:$(diag "$WORKDIR/gate-base.log")
   merge:$(diag "$MERGE_LOG")
   gate after merge:$(diag "$WORKDIR/gate-merged.log")"
fi

# --- no background git in a fixture repo --------------------------------------

# Q820's cause, asserted on behaviour rather than on the config key that
# currently delivers it: a commit in a fixture repo must spawn nothing that
# outlives it. Dropping the no_auto_maintenance call turns this red.
maint_repo="$(plain_repo maintenance)"
printf 'x\n' >"$maint_repo/$TARGET"
git -C "$maint_repo" add "$TARGET" .gitattributes
git -C "$maint_repo" "${GIT_ID[@]}" commit -qm base
printf 'y\n' >"$maint_repo/$TARGET"
maint_trace="$WORKDIR/maintenance-trace.log"
GIT_TRACE=1 git -C "$maint_repo" "${GIT_ID[@]}" commit -qam next >"$maint_trace" 2>&1
if grep -q 'maintenance run' "$maint_trace"; then
	bad "a fixture commit spawned background maintenance: $(grep -m1 -o 'git maintenance run.*' "$maint_trace")"
else
	ok 'a fixture commit spawns no detached maintenance'
fi

if (( fails > 0 )); then
	printf '\ngit-merge-plan-index-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\ngit-merge-plan-index-test: ok\n'
