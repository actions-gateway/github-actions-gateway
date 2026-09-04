#!/usr/bin/env bash
#
# Unit tests for scripts/docs/git-merge-roadmap.sh — the docs/roadmap.md merge
# driver.
#
# The driver silently rewrites people's merges, so the fail-safe half matters as
# much as the resolving half: every assertion below is a real three-way merge in
# a throwaway repo, checked for BOTH the resulting bullet set and whether
# conflict markers were left. The cases that must resolve are the ones the gate
# workflow produces by construction (two gate PRs each deleting their own
# bullet); the cases that must produce markers are everything the driver cannot
# know.
#
# Deletion is asserted in both directions on purpose. A bullet carries no
# separator of its own, so the failure this file is built to catch is a driver
# that reads "the bullet before the deleted one is now the last" as an edit of
# that bullet, which turns a concurrent delete/delete into a delete/modify
# conflict — a losable resolution rather than a visible one.
#
# The page's real shape is asserted against docs/roadmap.md itself: an identity
# merge is byte-identical and a one-sided real deletion reproduces that file
# exactly. `make check` runs check-roadmap.sh over the real page, so those two
# together are what says a driver-resolved page is still one the gate accepts.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
DRIVER="$REPO_ROOT/scripts/docs/git-merge-roadmap.sh"
TARGET='docs/roadmap.md'

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Q820: the plan-index sibling dies under the parallel runner on a git temp-file
# error whose message names no command, and passes on rerun. This suite shares
# its scaffolding, so it gets the same instrumentation before a sighting rather
# than after one. errtrace is what reaches run_merge's two commits; the other
# four are at top level, where the trap fires without it.
set -o errtrace
trap 'printf "%s:%s: FAILED (rc=%s): %s\n" "${BASH_SOURCE[0]##*/}" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR

fails=0

# Assembled from a character class so this file never trips
# check-conflict-markers.sh, which scans tracked files for marker-shaped lines.
MARKER_RE='^([<]{7}|[>]{7}|[|]{7})( |$)|^[=]{7}$'

# Throwaway repos have no identity configured and must not borrow the
# developer's.
GIT_ID=(-c user.email=test@example.invalid -c user.name=test)

# --- fixtures -----------------------------------------------------------------

# bullet ID TEXT — one annotated roadmap bullet with a continuation line, the
# shape every real one has.
bullet() {
	printf -- '- **[Item %s](operations/%s.md)** <!-- q:%s --> %s\n  A second line, so a bullet is never one line.' \
		"$1" "$1" "$1" "$2"
}

# roadmap_page NEAR_BULLETS -- EXPLORING_BULLETS
# A page with two annotated lists and prose around and between them. The
# near-term list is blank-separated and the exploring list is tight, which is
# how the real page is spaced and what makes the separator handling load-bearing.
roadmap_page() {
	local seen=0 arg
	local -a near=() exploring=()
	for arg in "$@"; do
		if [[ "$arg" == '--' ]]; then
			seen=$((seen + 1))
			continue
		fi
		case "$seen" in
		0) near+=("$arg") ;;
		*) exploring+=("$arg") ;;
		esac
	done
	local b first=1
	printf -- '---\nhide:\n  - toc\n---\n\n'
	printf -- '# Roadmap\n\n'
	printf -- 'What the project does not do yet.\n\n'
	printf -- '## In progress / near-term\n\n'
	printf -- 'Committed to a named release.\n\n'
	for b in ${near+"${near[@]}"}; do
		(( first )) || printf -- '\n'
		first=0
		printf -- '%s\n' "$b"
	done
	printf -- '\n## Exploring / longer-term\n\n'
	printf -- 'Unscheduled directions.\n\n'
	for b in ${exploring+"${exploring[@]}"}; do
		printf -- '%s\n' "$b"
	done
	printf -- '\n## How priorities are set\n\n'
	printf -- 'Operator feedback drives the ordering above.\n'
}

# --- the merge harness --------------------------------------------------------

# no_auto_maintenance REPO — stop this throwaway repo running background git.
#
# Q820: every commit otherwise spawns a detached `git maintenance run --auto`
# that outlives it and prunes while the next command writes to the same repo.
# A fixture repo has nothing to maintain, so the fix is not to start it.
no_auto_maintenance() {
	git -C "$1" config maintenance.auto false
}

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
	no_auto_maintenance "$repo"
	printf -- '%s merge=roadmap\n' "$TARGET" >"$repo/.gitattributes"
	printf -- '%s\n' "$repo"
}

# merge_repo NAME — plain_repo plus the driver, installed exactly the way
# `make merge-driver` installs it: a path relative to the working-tree root. That
# makes every merge below also a test of whether git resolves that relative path.
merge_repo() {
	local repo
	repo="$(plain_repo "$1")"
	mkdir -p "$repo/scripts/lib" "$repo/scripts/docs"
	cp "$DRIVER" "$repo/scripts/docs/git-merge-roadmap.sh"
	cp "$REPO_ROOT/scripts/lib/merge-keyed-records.awk" "$repo/scripts/lib/"
	cp "$REPO_ROOT/scripts/lib/merge-driver-common.sh" "$repo/scripts/lib/"
	chmod +x "$repo/scripts/docs/git-merge-roadmap.sh"
	(cd "$repo" && ./scripts/docs/git-merge-roadmap.sh --install >/dev/null)
	printf -- '%s\n' "$repo"
}

# run_merge REPO BASE OURS THEIRS — commit BASE on trunk, OURS on branch `ours`,
# THEIRS on branch `theirs`, then merge theirs into ours. Sets MERGE_RC and
# leaves the result at $REPO/$TARGET.
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
	git -C "$repo" "${GIT_ID[@]}" merge theirs >/dev/null 2>&1 || MERGE_RC=$?
}

# keys FILE — every bullet's annotated IDs, in file order, space-separated. The
# same annotation devtools/docs/roadmapcheck reads.
keys() {
	awk '
		/^- / {
			rest = $0
			while (match(rest, /<!--[ \t]*q:[^-]*-->/)) {
				m = substr(rest, RSTART, RLENGTH)
				rest = substr(rest, RSTART + RLENGTH)
				sub(/^<!--[ \t]*q:/, "", m)
				sub(/-->$/, "", m)
				gsub(/[ \t]/, "", m)
				printf "%s ", m
			}
		}
	' "$1"
}

has_markers() {
	grep -qE "$MARKER_RE" "$1/$TARGET"
}

ok() { printf -- 'ok   %s\n' "$1"; }
bad() {
	printf -- 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# expect_resolved NAME WANT_KEYS -- BASE -- OURS -- THEIRS
# Each of the three sides is a `roadmap_page` argument list. The merge must
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
	roadmap_page "${SIDE_BASE[@]}" >"$WORKDIR/base.md"
	roadmap_page "${SIDE_OURS[@]}" >"$WORKDIR/ours.md"
	roadmap_page "${SIDE_THEIRS[@]}" >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"
	die_if_killed "$name" "$MERGE_RC"

	local got want_text="$EXPECT_TEXT"
	EXPECT_TEXT=''
	got="$(keys "$repo/$TARGET")"
	got="${got% }"
	if (( MERGE_RC != 0 )); then
		bad "$name: merge reported a conflict (rc=$MERGE_RC)"
	elif has_markers "$repo"; then
		bad "$name: conflict markers left behind"
	elif [[ "$got" != "$want" ]]; then
		bad "$name: bullets are [$got], want [$want]"
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
	roadmap_page "${SIDE_BASE[@]}" >"$WORKDIR/base.md"
	roadmap_page "${SIDE_OURS[@]}" >"$WORKDIR/ours.md"
	roadmap_page "${SIDE_THEIRS[@]}" >"$WORKDIR/theirs.md"
	run_merge "$repo" "$WORKDIR/base.md" "$WORKDIR/ours.md" "$WORKDIR/theirs.md"

	if (( MERGE_RC == 0 )); then
		bad "$name: merge succeeded, but the outcome was not knowable"
	elif ! has_markers "$repo"; then
		bad "$name: conflict reported without conflict markers to resolve"
	else
		ok "$name"
	fi
}

# expect_page NAME BASE OURS THEIRS WANT — the merged file must equal WANT byte
# for byte. The key set says which bullets survived; only this says the blank
# lines around them did.
expect_page() {
	local name="$1" repo
	repo="$(merge_repo "page-$RANDOM")"
	run_merge "$repo" "$2" "$3" "$4"
	if (( MERGE_RC != 0 )); then
		bad "$name: merge reported a conflict (rc=$MERGE_RC)"
	elif ! cmp -s "$repo/$TARGET" "$5"; then
		bad "$name: $(diff "$5" "$repo/$TARGET" | head -6 | tr '\n' '/')"
	else
		ok "$name"
	fi
}

A="$(bullet Q10 'Waits on an operator ask.')"
B="$(bullet Q11 'Waits on a measurement.')"
C="$(bullet Q12 'Waits on hardware.')"
D="$(bullet Q13 'Waits on demand.')"
E="$(bullet Q14 'Parked until someone asks.')"
F="$(bullet Q15 'Shelved with the rest.')"

# --- what the gate workflow produces by construction ---------------------------

# THE motivating case: two gate PRs each ship their item and delete its bullet.
# Adjacent deletions are what a plain three-way merge cannot absorb (asserted
# below, on the same fixture, with no driver configured).
expect_resolved 'delete/delete adjacent bullets   -> both gone' \
	'Q12 Q14 Q15' \
	=== "$A" "$B" "$C" -- "$E" "$F" \
	=== "$B" "$C" -- "$E" "$F" \
	=== "$A" "$C" -- "$E" "$F"

# The same, deleting the list's LAST bullet on one side. A bullet does not own
# the blank line after it, so this must not read as an edit of its neighbour.
expect_resolved 'delete the last bullet + its neighbour -> both gone' \
	'Q10 Q14 Q15' \
	=== "$A" "$B" "$C" -- "$E" "$F" \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$A" "$C" -- "$E" "$F"

# And on the tight list, where the same reasoning has to hold with no blank
# lines between bullets at all.
expect_resolved 'delete/delete in the tight list  -> both gone' \
	'Q10 Q11 Q12 Q15' \
	=== "$A" "$B" "$C" -- "$D" "$E" "$F" \
	=== "$A" "$B" "$C" -- "$D" "$F" \
	=== "$A" "$B" "$C" -- "$E" "$F"

# The tight list is where the two spacings differ: nothing separates two bullets
# but a blank line separates the list from what follows. Deleting the final
# bullet on one side while the other deletes its neighbour is therefore the case
# that fails the moment a bullet is made to own the blank line after it.
expect_resolved 'delete the tight list final bullet -> both gone' \
	'Q10 Q11 Q12 Q13' \
	=== "$A" "$B" "$C" -- "$D" "$E" "$F" \
	=== "$A" "$B" "$C" -- "$D" "$E" \
	=== "$A" "$B" "$C" -- "$D" "$F"

# The same two shapes again, asserted on the whole page. A list is separated
# from the heading after it by a blank line the last bullet does not own, so
# both of these are silently one blank line short if the driver rebuilds the
# spacing from the deleted bullet instead of from the list.
roadmap_page "$A" "$B" "$C" -- "$D" "$E" "$F" >"$WORKDIR/sp-base.md"
roadmap_page "$A" "$B" "$C" -- "$D" "$E" >"$WORKDIR/sp-ours.md"
roadmap_page "$A" "$B" "$C" -- "$D" "$F" >"$WORKDIR/sp-theirs.md"
roadmap_page "$A" "$B" "$C" -- "$D" >"$WORKDIR/sp-want.md"
expect_page 'tight list, final bullet gone   -> page is byte-exact' \
	"$WORKDIR/sp-base.md" "$WORKDIR/sp-ours.md" "$WORKDIR/sp-theirs.md" "$WORKDIR/sp-want.md"

roadmap_page "$A" "$B" "$C" -- "$E" "$F" >"$WORKDIR/sq-base.md"
roadmap_page "$A" "$B" -- "$E" "$F" >"$WORKDIR/sq-ours.md"
roadmap_page "$A" "$C" -- "$E" "$F" >"$WORKDIR/sq-theirs.md"
roadmap_page "$A" -- "$E" "$F" >"$WORKDIR/sq-want.md"
expect_page 'spaced list, two bullets gone   -> page is byte-exact' \
	"$WORKDIR/sq-base.md" "$WORKDIR/sq-ours.md" "$WORKDIR/sq-theirs.md" "$WORKDIR/sq-want.md"

# One branch ships an item while another adds a bullet next to it.
expect_resolved 'delete vs add adjacent           -> both applied' \
	'Q11 Q13 Q14 Q15' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$B" -- "$E" "$F" \
	=== "$A" "$B" "$D" -- "$E" "$F"

# Two branches each add a bullet to the same list.
expect_resolved 'add/add adjacent bullets         -> both present' \
	'Q10 Q11 Q12 Q13 Q14 Q15' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$A" "$B" "$C" -- "$E" "$F" \
	=== "$A" "$B" "$D" -- "$E" "$F"

# A bullet reworded on one side, its neighbour deleted on the other. EXPECT_TEXT
# asserts the surviving text, since the key set alone cannot tell an edit that
# survived from one that was reverted.
EXPECT_TEXT='Now waits on the 1.6 release.'
expect_resolved 'edit one side, delete neighbour  -> edit kept' \
	'Q11 Q14 Q15' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$A" "$(bullet Q11 'Now waits on the 1.6 release.')" -- "$E" "$F" \
	=== "$B" -- "$E" "$F"

# An item promoted out of "Exploring" into "In progress" on one side is a delete
# in one list and an add in the other; the two lists merge independently and
# neither half needs to know about the other. Two branches promote two different
# items in the same window.
expect_resolved 'promote across lists             -> both halves applied' \
	'Q10 Q14 Q13 Q15' \
	=== "$A" -- "$D" "$E" "$F" \
	=== "$A" "$E" -- "$D" "$F" \
	=== "$A" "$D" -- "$E" "$F"

# A bullet reordered within its list on one side, untouched on the other, keeps
# the move.
expect_resolved 'reorder one side only            -> move kept' \
	'Q12 Q10 Q11 Q14 Q15' \
	=== "$A" "$B" "$C" -- "$E" "$F" \
	=== "$C" "$A" "$B" -- "$E" "$F" \
	=== "$A" "$B" "$C" -- "$E" "$F"

# Additions to two different lists in the same merge.
expect_resolved 'adds in two different lists      -> both present' \
	'Q10 Q12 Q14 Q15 Q13' \
	=== "$A" -- "$E" "$F" \
	=== "$A" "$C" -- "$E" "$F" \
	=== "$A" -- "$E" "$F" "$D"

# --- what the driver must refuse to guess -------------------------------------

# Both sides reworded the same bullet: two intents, one commitment.
expect_conflict 'edit/edit the same bullet        -> markers' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$(bullet Q10 'Ours: waits on a cluster.')" "$B" -- "$E" "$F" \
	=== "$(bullet Q10 'Theirs: waits on a customer.')" "$B" -- "$E" "$F"

# One side shipped the item and deleted its bullet while the other reworded it.
# Taking the deletion would drop the rewording; taking the rewording would
# re-promise shipped work. Both are guesses.
expect_conflict 'delete vs edit the same bullet   -> markers' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$B" -- "$E" "$F" \
	=== "$(bullet Q10 'Theirs: still open, actually.')" "$B" -- "$E" "$F"

# The same backlog binding added on both sides with different prose.
expect_conflict 'same binding added twice         -> markers' \
	=== "$A" -- "$E" "$F" \
	=== "$A" "$(bullet Q12 'Ours: waits on hardware.')" -- "$E" "$F" \
	=== "$A" "$(bullet Q12 'Theirs: waits on a plan.')" -- "$E" "$F"

# Both sides reshuffled the same list differently: neither order is derivable.
expect_conflict 'reorder on both sides            -> markers' \
	=== "$A" "$B" "$C" -- "$E" "$F" \
	=== "$C" "$A" "$B" -- "$E" "$F" \
	=== "$B" "$C" "$A" -- "$E" "$F"

# A bullet with no annotation has no key. The run stops being a mergeable list
# on that side alone, the per-list pairing breaks, and the driver hands the file
# back to git — which conflicts here, because the other side deleted the
# neighbour it was inserted next to.
expect_conflict 'unannotated bullet added         -> markers' \
	=== "$A" "$B" -- "$E" "$F" \
	=== "$A" '- **An item with no backlog row.** Nothing keys this.' "$B" -- "$E" "$F" \
	=== "$B" -- "$E" "$F"

# Two branches write a bullet for the same backlog row into two different lists:
# one calls it near-term, the other parks it under Exploring. Each list resolves
# on its own, so only the whole-page uniqueness check can see that the binding
# now has two bullets. git's own merge is clean here (the two adds land in
# different regions), so the driver cannot leave markers — what it must do is
# refuse to claim the resolution and hand back exactly what git would have
# produced.
roadmap_page "$A" -- "$F" >"$WORKDIR/dup-base.md"
roadmap_page "$A" "$E" -- "$F" >"$WORKDIR/dup-ours.md"
roadmap_page "$A" -- "$F" "$E" >"$WORKDIR/dup-theirs.md"
cp "$WORKDIR/dup-ours.md" "$WORKDIR/dup-driver.md"
rc=0
"$DRIVER" "$WORKDIR/dup-base.md" "$WORKDIR/dup-driver.md" "$WORKDIR/dup-theirs.md" \
	7 "$TARGET" 2>"$WORKDIR/dup.err" || rc=$?
	die_if_killed "one bullet in two lists" "$rc"
cp "$WORKDIR/dup-ours.md" "$WORKDIR/dup-git.md"
git merge-file "$WORKDIR/dup-git.md" "$WORKDIR/dup-base.md" "$WORKDIR/dup-theirs.md" >/dev/null 2>&1 || true
if (( rc == 0 )) && grep -q 'binding more than once: Q14' "$WORKDIR/dup.err" &&
	cmp -s "$WORKDIR/dup-driver.md" "$WORKDIR/dup-git.md"; then
	ok 'one bullet in two lists         -> refused, handed back to git'
else
	bad "a bullet in two lists should fall back to git verbatim (rc=$rc, err=$(cat "$WORKDIR/dup.err"))"
fi

# --- a side that restructures the page ----------------------------------------

# Adding a whole section is rare but legal, and it breaks the per-list pairing
# the driver depends on. It must fall back rather than pair the wrong lists:
# with a genuinely independent edit on the other side, git's own merge is clean.
restructure="$(merge_repo restructure)"
roadmap_page "$A" -- "$E" >"$WORKDIR/x-base.md"
{
	roadmap_page "$A" -- "$E"
	printf -- '\n## Recently shipped\n\n%s\n' "$C"
} >"$WORKDIR/x-ours.md"
roadmap_page "$A" "$B" -- "$E" >"$WORKDIR/x-theirs.md"
run_merge "$restructure" "$WORKDIR/x-base.md" "$WORKDIR/x-ours.md" "$WORKDIR/x-theirs.md"
die_if_killed "a side adds a whole list" "$MERGE_RC"
if (( MERGE_RC == 0 )) && ! has_markers "$restructure" &&
	[[ "$(keys "$restructure/$TARGET")" == 'Q10 Q11 Q14 Q12 ' ]]; then
	ok 'a side adds a whole list        -> falls back to git, which merges it'
else
	bad "a new list should fall back to git's merge (rc=$MERGE_RC, keys=[$(keys "$restructure/$TARGET")])"
fi

# --- regions outside the bullets ----------------------------------------------

# Prose between the lists merges as plain text, and a genuine conflict there is
# reported as one, with markers.
prose_repo="$(merge_repo prose)"
roadmap_page "$A" -- "$E" >"$WORKDIR/p-base.md"
awk '{ sub(/^Unscheduled directions\.$/, "Our new intro."); print }' \
	"$WORKDIR/p-base.md" >"$WORKDIR/p-ours.md"
awk '{ sub(/^Unscheduled directions\.$/, "Their new intro."); print }' \
	"$WORKDIR/p-base.md" >"$WORKDIR/p-theirs.md"
run_merge "$prose_repo" "$WORKDIR/p-base.md" "$WORKDIR/p-ours.md" "$WORKDIR/p-theirs.md"
die_if_killed "outside the bullets: contested prose" "$MERGE_RC"
if (( MERGE_RC != 0 )) && has_markers "$prose_repo"; then
	ok 'outside the bullets: contested prose -> markers'
else
	bad "outside the bullets: a contested intro line should conflict (rc=$MERGE_RC)"
fi

# --- absence is a no-op -------------------------------------------------------

# A contributor who never ran `make merge-driver` gets git's built-in three-way
# merge: the attribute names an undefined driver, which git resolves by falling
# back, not by erroring. This is what makes the .gitattributes line safe to
# commit for everyone.
unconfigured="$(plain_repo unconfigured)"
roadmap_page "$A" "$B" -- "$E" "$F" >"$WORKDIR/u-base.md"
roadmap_page "$A" -- "$E" "$F" >"$WORKDIR/u-ours.md"
roadmap_page "$A" "$B" -- "$E" >"$WORKDIR/u-theirs.md"
run_merge "$unconfigured" "$WORKDIR/u-base.md" "$WORKDIR/u-ours.md" "$WORKDIR/u-theirs.md"
die_if_killed "no driver configured: distant edits" "$MERGE_RC"
if (( MERGE_RC == 0 )) && ! has_markers "$unconfigured"; then
	ok 'no driver configured: git merges distant edits as before'
else
	bad "no driver configured: the attribute must be a no-op, not an error (rc=$MERGE_RC)"
fi

# Same repo, the adjacent case: without the driver this is the conflict that
# motivated it. Asserted so the tests above are known to be measuring the driver
# and not something git would have done anyway.
unconfigured2="$(plain_repo unconfigured2)"
roadmap_page "$A" "$B" "$C" -- "$E" "$F" >"$WORKDIR/u2-base.md"
roadmap_page "$B" "$C" -- "$E" "$F" >"$WORKDIR/u2-ours.md"
roadmap_page "$A" "$C" -- "$E" "$F" >"$WORKDIR/u2-theirs.md"
run_merge "$unconfigured2" "$WORKDIR/u2-base.md" "$WORKDIR/u2-ours.md" "$WORKDIR/u2-theirs.md"
die_if_killed "no driver configured: adjacent deletes" "$MERGE_RC"
if (( MERGE_RC != 0 )); then
	ok 'no driver configured: adjacent deletes still conflict (the motivating case)'
else
	bad 'no driver configured: adjacent deletes were expected to conflict'
fi

# --- rebase, which is where the pain actually is -------------------------------

# Branches rebase onto main; a rebase replays each commit as a merge with ours
# and theirs swapped relative to `git merge`, so the symmetry of the set rules is
# what makes this work.
rebase_repo="$(merge_repo rebase)"
roadmap_page "$A" "$B" "$C" -- "$E" "$F" >"$WORKDIR/r-base.md"
cp "$WORKDIR/r-base.md" "$rebase_repo/$TARGET"
git -C "$rebase_repo" add "$TARGET" .gitattributes scripts
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qm base
git -C "$rebase_repo" checkout -q -b main
roadmap_page "$B" "$C" -- "$E" "$F" >"$rebase_repo/$TARGET"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'ship Q10'
git -C "$rebase_repo" checkout -q -b work trunk
roadmap_page "$A" "$C" -- "$E" "$F" >"$rebase_repo/$TARGET"
git -C "$rebase_repo" "${GIT_ID[@]}" commit -qam 'ship Q11'
rc=0
git -C "$rebase_repo" "${GIT_ID[@]}" rebase main >/dev/null 2>&1 || rc=$?
die_if_killed "rebase onto main" "$rc"
if (( rc == 0 )) && [[ "$(keys "$rebase_repo/$TARGET")" == 'Q12 Q14 Q15 ' ]] &&
	! has_markers "$rebase_repo"; then
	ok 'rebase onto main: both shipped bullets removed'
else
	bad "rebase onto main should resolve by backlog ID (rc=$rc, keys=[$(keys "$rebase_repo/$TARGET")])"
	git -C "$rebase_repo" rebase --abort >/dev/null 2>&1 || true
fi

# --- the driver resolves from a subdirectory ----------------------------------

# `merge.roadmap.driver` holds a path relative to the working-tree root, which is
# only correct if git runs the driver from there. A merge started in a
# subdirectory is the case that would expose it.
subdir_repo="$(merge_repo subdir)"
roadmap_page "$A" "$B" "$C" -- "$E" "$F" >"$WORKDIR/s-base.md"
roadmap_page "$B" "$C" -- "$E" "$F" >"$WORKDIR/s-ours.md"
roadmap_page "$A" "$C" -- "$E" "$F" >"$WORKDIR/s-theirs.md"
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
(cd "$subdir_repo/docs" && git "${GIT_ID[@]}" merge theirs >/dev/null 2>&1) || rc=$?
die_if_killed "merge from a subdirectory" "$rc"
if (( rc == 0 )) && [[ "$(keys "$subdir_repo/$TARGET")" == 'Q12 Q14 Q15 ' ]]; then
	ok 'merge from a subdirectory resolves the relative driver path'
else
	bad "merge from a subdirectory failed (rc=$rc, keys=[$(keys "$subdir_repo/$TARGET")])"
fi

# --- the real roadmap ----------------------------------------------------------

# The bullet splitter has to agree with the real page's shape, not just the
# fixtures': a no-op merge of docs/roadmap.md against itself must reproduce it
# byte for byte, including its mixed blank-line spacing and its frontmatter.
real_out="$WORKDIR/real-ours.md"
cp "$REPO_ROOT/$TARGET" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/$TARGET" "$real_out" "$REPO_ROOT/$TARGET" 7 "$TARGET" \
	>/dev/null 2>&1 || rc=$?
	die_if_killed "an identity merge is byte-identical" "$rc"
if (( rc == 0 )) && cmp -s "$real_out" "$REPO_ROOT/$TARGET"; then
	ok "$TARGET: an identity merge is byte-identical"
else
	bad "$TARGET: identity merge changed the file or failed (rc=$rc)"
fi

# A real one-sided deletion of the real page's LAST near-term bullet: the case
# whose spacing the driver has to rebuild, asserted byte for byte against the
# file a person would have written by hand.
real_last="$(keys "$REPO_ROOT/$TARGET" | awk '{ print $2 }')"
awk -v id="$real_last" '
	/^- / { drop = (index($0, "q:" id " ") > 0) }
	/^[^ \t-]/ && !/^- / { drop = 0 }
	!drop { print }
' "$REPO_ROOT/$TARGET" >"$WORKDIR/real-theirs.md"
cp "$REPO_ROOT/$TARGET" "$real_out"
rc=0
"$DRIVER" "$REPO_ROOT/$TARGET" "$real_out" "$WORKDIR/real-theirs.md" 7 "$TARGET" \
	>/dev/null 2>&1 || rc=$?
	die_if_killed "a one-sided real deletion" "$rc"
if (( rc == 0 )) && cmp -s "$real_out" "$WORKDIR/real-theirs.md"; then
	ok "$TARGET: deleting $real_last one-sided reproduces that file"
else
	bad "$TARGET: a one-sided real deletion did not apply cleanly (rc=$rc)"
fi

# --- no background git in a fixture repo --------------------------------------

# Q820's cause, asserted on behaviour rather than on the config key that
# currently delivers it: a commit in a fixture repo must spawn nothing that
# outlives it. Dropping the no_auto_maintenance call turns this red.
maint_repo="$(plain_repo maintenance)"
printf -- 'x\n' >"$maint_repo/$TARGET"
git -C "$maint_repo" add "$TARGET" .gitattributes
git -C "$maint_repo" "${GIT_ID[@]}" commit -qm base
printf -- 'y\n' >"$maint_repo/$TARGET"
maint_trace="$WORKDIR/maintenance-trace.log"
GIT_TRACE=1 git -C "$maint_repo" "${GIT_ID[@]}" commit -qam next >"$maint_trace" 2>&1
if grep -q 'maintenance run' "$maint_trace"; then
	bad "a fixture commit spawned background maintenance: $(grep -m1 -o 'git maintenance run.*' "$maint_trace")"
else
	ok 'a fixture commit spawns no detached maintenance'
fi

if (( fails > 0 )); then
	printf -- '\ngit-merge-roadmap-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf -- '\ngit-merge-roadmap-test: ok\n'
