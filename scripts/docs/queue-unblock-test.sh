#!/usr/bin/env bash
#
# Unit tests for scripts/docs/queue-unblock.sh.
#
# The script had no suite while it read the Queue table. It gets one now
# because moving to the item store changed how a blocker is written: the table
# linked `[Q12](#Q12)` and the store links `[Q12](Q12.md)`, so a clause taken
# as "Blocked by" up to the next period now stops inside the first link. That
# is silent — the first blocker still matches, and every later one in a list
# disappears — so the multi-blocker case below is the reason this file exists.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

# The script resolves its store from its own location, so the fixture is a
# throwaway tree with the script copied into the same relative position.
mkdir -p "$TMP/scripts/docs" "$TMP/docs/queue"
cp "$HERE/queue-unblock.sh" "$TMP/scripts/docs/queue-unblock.sh"
RUN="$TMP/scripts/docs/queue-unblock.sh"

item() {  # item <id> <title> [note-line]
    {
        printf -- '---\nid: %s\nrank: a0\nlabels:\n    - ci\nstatus: ready\nsize: S\n---\n\n' "$1"
        printf -- '# %s\n\n%s\n' "$2" "${3:-A note.}"
    } > "$TMP/docs/queue/$1.md"
}

expect() {  # expect <want-rc> <id-arg> <name> [pattern]
    local want="$1" arg="$2" name="$3" pat="${4:-}" rc=0
    "$RUN" "$arg" > "$TMP/out" 2>&1 || rc=$?
    if [[ "$rc" != "$want" ]]; then
        bad "$name (rc=$rc want=$want)"; sed 's/^/       /' "$TMP/out" | head -3; return
    fi
    if [[ -n "$pat" ]] && ! grep -q "$pat" "$TMP/out"; then
        bad "$name (rc matched but output lacks '$pat')"; sed 's/^/       /' "$TMP/out" | head -3; return
    fi
    ok "$name"
}

item Q1 "The blocker"
item Q2 "The other blocker"
item Q10 "A single-blocker dependent" 'Blocked by [Q1](Q1.md).'
item Q11 "A two-blocker dependent"    'Blocked by [Q1](Q1.md), [Q2](Q2.md).'
item Q12 "An unrelated item"          'Mentions Q1 in passing, but waits on nothing.'
item Q125 "A prefix collision"        'Blocked by [Q99](Q99.md).'

expect 0 1  "a single blocker is found" "Q10"
expect 0 Q1 "the Q-prefixed form works the same" "Q10"

# The reason this suite exists: the second blocker in a list must be found.
expect 0 2  "a later blocker in a comma-separated list is found" "Q11"

# Controls. Without these the rule above is equally consistent with a script
# that prints every item carrying the words "Blocked by".
"$RUN" 1 > "$TMP/out" 2>&1 || true
if grep -q "Q12" "$TMP/out"; then
    bad "control: a bare mention outside the clause must not match"
else
    ok "control: a bare mention outside the clause must not match"
fi

expect 0 9   "control: Q9 does not match Q99" "No backlog items"
expect 0 999 "control: an ID nothing waits on reports none" "No backlog items"

# Bad input and a missing store are refusals, not empty results.
expect 1 "" "an empty id is refused"     "must be numeric"
expect 1 "abc" "a non-numeric id is refused" "must be numeric"

rm -rf "$TMP/docs/queue"
expect 1 1 "an absent store is an error, not 'nothing blocked'" "not found"

if (( fail )); then
    printf '\nqueue-unblock-test: FAILED\n'
    exit 1
fi
printf '\nqueue-unblock-test: ok\n'
