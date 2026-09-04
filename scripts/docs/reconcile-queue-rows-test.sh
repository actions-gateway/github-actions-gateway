#!/usr/bin/env bash
#
# Unit tests for scripts/docs/reconcile-queue-rows.sh.
#
# Every case is paired: a resolution that must pass and the same resolution
# carrying one lost item that must fail. The tool exists because the loss it
# reports is silent by construction — a rebase that drops the branch's own rows
# prints `Successfully rebased` and leaves no marker — so a pass that was never
# shown failing would look exactly like a real one.
#
# The fixtures drive real `git rebase` and `git merge` conflicts rather than
# simulating the state, because what the tool reads differs per state: mid
# operation the resolution is in the index and `HEAD` is the replay so far. A
# fixture that staged a tree by hand would agree with an implementation reading
# either one, which is the distinction under test.
#
# refs/remotes/origin/main is set with update-ref rather than pushed: a push to
# a fixture remote is denied by this workstation's branch guard.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
TOOL="$HERE/reconcile-queue-rows.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

item() {  # item <repo> <id>
    mkdir -p "$1/docs/queue"
    printf -- '---\nid: %s\nrank: a0\nstatus: ready\n---\n\n# Title for %s\n' "$2" "$2" \
        > "$1/docs/queue/$2.md"
}

# newrepo <dir> — a repo whose base holds Q1 and Q2, then a main that closes Q1
# and files Q10, then a branch off the base that files Q20 and Q21. Both sides
# edit shared.txt, so the rebase and the merge below always conflict there and
# never inside the store: the store's row set is collateral of a hunk elsewhere,
# which is the shape #1471 met.
newrepo() {
    local r="$1"
    mkdir -p "$r"
    git init -q -b main "$r"
    # Q820: no detached maintenance racing the next command in a fixture repo.
    git -C "$r" config maintenance.auto false
    git -C "$r" config user.email t@e.com
    git -C "$r" config user.name T
    item "$r" Q1
    item "$r" Q2
    printf '# Queue\n' > "$r/docs/queue/README.md"
    printf 'shared\n' > "$r/shared.txt"
    git -C "$r" add -A
    git -C "$r" commit -qm base
    git -C "$r" rm -q "docs/queue/Q1.md"
    item "$r" Q10
    printf 'main-side\n' > "$r/shared.txt"
    git -C "$r" add -A
    git -C "$r" commit -qm "main: close Q1, file Q10"
    git -C "$r" update-ref refs/remotes/origin/main HEAD
    git -C "$r" checkout -q -B claude/work HEAD~1
    item "$r" Q20
    item "$r" Q21
    printf 'branch-side\n' > "$r/shared.txt"
    git -C "$r" add -A
    git -C "$r" commit -qm "branch: file Q20 and Q21"
}

drop() {  # drop <repo> <id>... — remove items from the in-progress resolution
    local r="$1"
    shift
    local id
    for id in "$@"; do
        git -C "$r" rm -q --cached "docs/queue/$id.md"
        rm -f "$r/docs/queue/$id.md"
    done
}

run() {  # run <repo> [args...] -> rc, output in $TMP/out
    local r="$1" rc=0
    shift
    (cd "$r" && bash "$TOOL" "$@") > "$TMP/out" 2>&1 || rc=$?
    return "$rc"
}

expect() {  # expect <want-rc> <repo> <name> [pattern] [tool-args...]
    local want="$1" repo="$2" name="$3" pat="${4:-}" rc=0
    if (($# >= 4)); then shift 4; else shift $#; fi
    run "$repo" "$@" || rc=$?
    die_if_killed "$name" "$rc" "$want"
    if [[ "$rc" != "$want" ]]; then
        bad "$name (rc=$rc want=$want)"
        sed 's/^/       /' "$TMP/out" | head -5
        return
    fi
    if [[ -n "$pat" ]] && ! grep -q "$pat" "$TMP/out"; then
        bad "$name (rc matched but output lacks '$pat')"
        sed 's/^/       /' "$TMP/out" | head -5
        return
    fi
    ok "$name"
}

refute() {  # refute <repo> <name> <pattern> — the last run must NOT have said it
    if grep -q "$3" "$TMP/out"; then
        bad "$2 (output should not contain '$3')"
        return
    fi
    ok "$2"
}

start_rebase() {  # start_rebase <repo> — conflict, then resolve shared.txt only
    git -C "$1" rebase origin/main > /dev/null 2>&1 || true
    printf 'resolved\n' > "$1/shared.txt"
    git -C "$1" add shared.txt
}

start_merge() {  # start_merge <repo> — same, as a merge
    git -C "$1" merge origin/main > /dev/null 2>&1 || true
    printf 'resolved\n' > "$1/shared.txt"
    git -C "$1" add shared.txt
}

# --- rebase in progress -------------------------------------------------------

r="$TMP/rebase-clean"; newrepo "$r"; start_rebase "$r"
expect 0 "$r" "rebase: a clean resolution passes" "every difference is one side closing"
expect 0 "$r" "rebase: main's closure of Q1 is named, not flagged" "Q1       closed on"

# The reason the tool reads the index. `HEAD` mid-rebase is the replay so far and
# excludes the commit being resolved, so a HEAD-based reading reports the
# branch's own new rows as casualties. Assert both directions: the tool stays
# quiet about them, and the HEAD reading really would have named them.
refute "$r" "rebase: the branch's own rows are not reported lost" "Q20 "
head_says="$(git -C "$r" ls-tree --name-only HEAD docs/queue/ | grep -c 'Q20\.md' || true)"
if [[ "$head_says" == "0" ]]; then
    ok "rebase: control — Q20 really is absent from HEAD, so the index read is load-bearing"
else
    bad "rebase: control — Q20 was in HEAD, so this fixture cannot show the difference"
fi

r="$TMP/rebase-lost-theirs"; newrepo "$r"; start_rebase "$r"; drop "$r" Q10
expect 1 "$r" "rebase: dropping the row main added fails" "Q10      COLLATERAL"

r="$TMP/rebase-lost-ours"; newrepo "$r"; start_rebase "$r"; drop "$r" Q20 Q21
expect 1 "$r" "rebase: dropping the branch's own rows fails" "Q20      COLLATERAL"
expect 1 "$r" "rebase: both lost rows are counted" "2 item(s) lost"

# The hint is only useful if it works. Run it and require the tree to come clean.
hint="$(grep -m1 'git checkout' "$TMP/out" | sed 's/^ *//')"
if [[ -z "$hint" ]]; then
    bad "rebase: a casualty prints a recovery command"
else
    ok "rebase: a casualty prints a recovery command"
    (cd "$r" && eval "$hint") > /dev/null 2>&1 || true
    (cd "$r" && eval "${hint/Q20/Q21}") > /dev/null 2>&1 || true
    expect 0 "$r" "rebase: running the printed recovery clears the finding" "ok —"
fi

# --- merge in progress --------------------------------------------------------

r="$TMP/merge-clean"; newrepo "$r"; start_merge "$r"
expect 0 "$r" "merge: a clean resolution passes" "reconcile-queue-rows (merge)"

r="$TMP/merge-lost"; newrepo "$r"; start_merge "$r"; drop "$r" Q10
expect 1 "$r" "merge: dropping the row the other side added fails" "Q10      COLLATERAL"

# --- settled, after the operation finished ------------------------------------

r="$TMP/settled-clean"; newrepo "$r"; start_rebase "$r"
GIT_EDITOR=true git -C "$r" rebase --continue > /dev/null 2>&1
expect 0 "$r" "settled: a completed clean rebase passes" "reconcile-queue-rows (settled)"

r="$TMP/settled-lost"; newrepo "$r"; start_rebase "$r"; drop "$r" Q20 Q21
GIT_EDITOR=true git -C "$r" rebase --continue > /dev/null 2>&1
expect 1 "$r" "settled: a completed lossy rebase still fails" "COLLATERAL"

# --- refusals: a read it cannot take is never a verdict -----------------------

# A completed rebase whose ORIG_HEAD a later operation overwrote: the reconcile
# is still possible, but only if the caller names the pre-rebase tip by hand.
r="$TMP/no-orig-head"; newrepo "$r"
pre="$(git -C "$r" rev-parse claude/work)"
start_rebase "$r"
GIT_EDITOR=true git -C "$r" rebase --continue > /dev/null 2>&1
git -C "$r" update-ref -d ORIG_HEAD 2>/dev/null || true
expect 2 "$r" "settled: no ORIG_HEAD refuses rather than passing" "ORIG_HEAD is unset"
expect 0 "$r" "settled: --base names the tip ORIG_HEAD would have" "ok —" --base "$pre"

# A branch that never merged the other side is not a lossy resolution; it is a
# branch that is simply behind, and answering would call every row the other
# side filed since the fork a casualty.
r="$TMP/never-merged"; newrepo "$r"
expect 2 "$r" "settled: a branch that never merged refuses" "no resolution of the" \
    --base "claude/work"

# A conflict inside the store itself, left unresolved. Both sides file the same
# ID with different content, which is an add/add on one path and the one shape
# that leaves an item unmerged. It is built standalone rather than on newrepo,
# whose shared.txt conflict stops the rebase on an earlier commit — the store
# commit never replays, so nothing under it is unmerged and the tool is right to
# answer. That fixture passed this case for the wrong reason.
r="$TMP/unmerged"
mkdir -p "$r"
git init -q -b main "$r"
git -C "$r" config maintenance.auto false
git -C "$r" config user.email t@e.com
git -C "$r" config user.name T
item "$r" Q1
git -C "$r" add -A && git -C "$r" commit -qm base
printf -- '---\nid: Q30\nrank: zz\nstatus: ready\n---\n\n# Main files Q30\n' \
    > "$r/docs/queue/Q30.md"
git -C "$r" add -A && git -C "$r" commit -qm "main files Q30"
git -C "$r" update-ref refs/remotes/origin/main HEAD
git -C "$r" checkout -q -B claude/work HEAD~1
item "$r" Q30
git -C "$r" add -A && git -C "$r" commit -qm "branch files Q30 differently"
git -C "$r" rebase origin/main > /dev/null 2>&1 || true
if [[ -n "$(git -C "$r" ls-files -u -- docs/queue)" ]]; then
    ok "control — the fixture really does leave an item unmerged"
else
    bad "control — the fixture left no unmerged item, so the refusal is untested"
fi
expect 2 "$r" "an unmerged item refuses rather than reading a half-resolved store" "still unmerged"

r="$TMP/badref"; newrepo "$r"
expect 2 "$r" "an unreadable ref refuses" "refusing to guess" --base "no/such/ref"

# --- the store's own boundary -------------------------------------------------

r="$TMP/nonitems"; newrepo "$r"; start_rebase "$r"
mkdir -p "$r/docs/queue/archive"
printf 'not an item\n' > "$r/docs/queue/archive/Q99.md"
printf 'not an item\n' > "$r/docs/queue/notes.md"
git -C "$r" add -A
expect 0 "$r" "a nested path and a non-Q file are not items" "4 item(s) yours"

printf '\n'
if ((fail)); then
    printf 'reconcile-queue-rows-test: FAILED\n'
    exit 1
fi
printf 'reconcile-queue-rows-test: all checks passed\n'
