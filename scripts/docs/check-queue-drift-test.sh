#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-queue-drift.py.
#
# The gate's whole value is failing on an edit that lands on one side only,
# so every case here is paired with the passing tree it was derived from. The
# controls matter as much as the failures: this check must be blind to how
# prose is wrapped (the store is reflowed, the table is not) while still
# seeing a one-character title change, and those two claims pull against each
# other.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="$HERE/check-queue-drift.py"
QUEUE="$HERE/queue.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

newrepo() {  # newrepo <dir> -> a repo holding a table and the store it migrates to
    local r="$1"
    mkdir -p "$r/docs" "$r/scripts/docs"
    git init -q -b main "$r"
    git -C "$r" config user.email t@e.com
    git -C "$r" config user.name T
    cp "$QUEUE" "$r/scripts/docs/queue.py"
    cp "$CHECKER" "$r/scripts/docs/check-queue-drift.py"
    # shellcheck disable=SC2016  # the backticks are markdown code spans in the
    # label cells, which is exactly the form the migration parses.
    {
        printf '# Project Status\n\n## Queue\n\n'
        printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
        printf '| <a id="Q1"></a>Q1 | [First thing](plan/one.md) | `ci` | 🔲 | S | A note. |\n'
        printf '| <a id="Q2"></a>Q2 | [Second thing](plan/two.md) | `docs` | 🔲 | S | Another note. |\n'
        printf '| <a id="Q3"></a>Q3 | [Third thing](plan/three.md) | `debt` | 🚫 | M | Blocked by Q1. |\n'
        printf '\n## Deferred\n\n'
        printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
        printf '| <a id="Q4"></a>Q4 | [Parked thing](plan/four.md) | `flake` | S | **Event:** it recurs. |\n'
    } > "$r/docs/STATUS.md"
    (cd "$r" && python3 scripts/docs/queue.py --store docs/queue migrate docs/STATUS.md) > /dev/null
}

expect() {  # expect <want-rc> <repo> <name> [pattern]
    local want="$1" repo="$2" name="$3" pat="${4:-}" rc=0
    (cd "$repo" && python3 scripts/docs/check-queue-drift.py) > "$TMP/out" 2>&1 || rc=$?
    if [[ "$rc" != "$want" ]]; then
        bad "$name (rc=$rc want=$want)"
        sed 's/^/       /' "$TMP/out" | head -4
        return
    fi
    if [[ -n "$pat" ]] && ! grep -q "$pat" "$TMP/out"; then
        bad "$name (rc matched but output lacks '$pat')"
        sed 's/^/       /' "$TMP/out" | head -4
        return
    fi
    ok "$name"
}

# --- the tree the migration just produced agrees with itself ---------------

R="$TMP/clean"; newrepo "$R"
expect 0 "$R" "a freshly migrated store agrees with its table" "4 item(s) agree"

# --- an item on one side only ---------------------------------------------

R="$TMP/gone"; newrepo "$R"
rm "$R/docs/queue/Q2.md"
expect 1 "$R" "an item deleted from the store is caught" "Q2 is in docs/STATUS.md but not"

R="$TMP/extra"; newrepo "$R"
cp "$R/docs/queue/Q2.md" "$R/docs/queue/Q9.md"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/queue/Q9.md"
p.write_text(p.read_text().replace("id: Q2", "id: Q9"))
PY
expect 1 "$R" "an item added to the store alone is caught" "Q9 is in docs/queue but not"

# --- a field edited on one side only --------------------------------------

R="$TMP/title"; newrepo "$R"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/STATUS.md"
p.write_text(p.read_text().replace("Second thing", "Second thing, reworded"))
PY
expect 1 "$R" "a title reworded in the table alone is caught" "title differs"

R="$TMP/label"; newrepo "$R"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/queue/Q1.md"
p.write_text(p.read_text().replace("    - ci\n", "    - ci\n    - security\n"))
PY
expect 1 "$R" "a label added to the store alone is caught" "labels differs"

R="$TMP/status"; newrepo "$R"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/STATUS.md"
p.write_text(p.read_text().replace("| `debt` | 🚫 |", "| `debt` | 🔲 |"))
PY
expect 1 "$R" "an unblock applied to the table alone is caught" "status differs"

# --- priority, which is the thing a count cannot see ----------------------

R="$TMP/order"; newrepo "$R"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/STATUS.md"
lines = p.read_text().splitlines(keepends=True)
i = next(n for n, l in enumerate(lines) if 'id="Q1"' in l)
j = next(n for n, l in enumerate(lines) if 'id="Q3"' in l)
lines[i], lines[j] = lines[j], lines[i]
p.write_text("".join(lines))
PY
expect 1 "$R" "a reprioritized table with the same items is caught" "priority order differs"

# --- controls: what the gate must stay blind to ---------------------------

# The store is reflowed to one sentence per line and the table is not, so a
# gate comparing text rather than meaning would fail on every real tree.
R="$TMP/wrapped"; newrepo "$R"
python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/queue/Q2.md"
p.write_text(p.read_text().replace("Another note.", "Another\nnote."))
PY
expect 0 "$R" "control: rewrapping a note in the store is not drift"

# A file the store adds that is not an item must not read as one.
R="$TMP/readme"; newrepo "$R"
printf '# Backlog\n\nAnything at all.\n' > "$R/docs/queue/README.md"
expect 0 "$R" "control: a README in the store is not an item"

# --- nothing to compare, and reads that could not be taken ---------------

R="$TMP/nostore"; newrepo "$R"
rm -rf "$R/docs/queue"
expect 0 "$R" "an absent store says nothing to compare" "has not been created yet"

R="$TMP/notable"; newrepo "$R"
rm "$R/docs/STATUS.md"
expect 0 "$R" "an absent table retires the gate rather than failing" "can be retired"

R="$TMP/noqueue"; newrepo "$R"
rm "$R/scripts/docs/queue.py"
expect 2 "$R" "a missing queue.py exits unmeasurable, not ok" "refusing to guess"

if (( fail )); then
    printf '\ncheck-queue-drift-test: FAILED\n'
    exit 1
fi
printf '\ncheck-queue-drift-test: ok\n'
