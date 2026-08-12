#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-plan-index.sh, the docs/plan/README.md
# consistency gate.
#
# The cases here cover invariant 3, the Status-cell/Queue-row rule (Q800): a
# QNNN in an active row's Status cell must be linked while its row is live and
# bare once it is gone. Both directions are asserted, because the bare form is
# the common one — 92 of the 96 QNNN mentions in the real file name closed rows
# — so a matcher that stopped firing would look exactly like a clean tree.
#
# Two cases pin things reading the source does not predict. The escaped-pipe
# case plants `\|` in the Scope cell and still requires red: without the
# protection before the column split, the Status cell shifts out of column 3
# and every check below it silently reads the wrong text. The Scope-cell and
# Archive-row cases pin what must stay green, which is where a rule this shape
# gets noisy first.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-plan-index.sh"

fails=0
workdirs=()
WORK=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo QUEUE_IDS DEFERRED_IDS — start a throwaway repo holding the plan
# files every fixture indexes, and a docs/STATUS.md whose Queue and Deferred
# tables anchor the given space-separated IDs. The Progress row keeps invariant
# 1 satisfied, so a case only ever fails on the rule it is testing.
new_repo() {
    local queue="$1" deferred="$2" id
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/docs/plan/archive"
    printf '# Alpha\n' >"$WORK/docs/plan/alpha.md"
    printf '# Zeta\n' >"$WORK/docs/plan/archive/zeta.md"
    # shellcheck disable=SC2016 # the backticks are a Markdown label cell, not substitution
    {
        printf '# Project Status\n\n## Progress\n\n| Plan | Labels | St |\n|---|---|---|\n'
        printf '| [Alpha](plan/alpha.md) | `docs` | 🔲 |\n'
        printf '\n## Queue\n\n| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
        for id in $queue; do
            printf '| <a id="%s"></a>%s | Thing | `docs` | 🔲 | S | note |\n' "$id" "$id"
        done
        printf '\n## Deferred\n\n| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
        for id in $deferred; do
            printf '| <a id="%s"></a>%s | Thing | `docs` | S | **Demand:** someone asks. |\n' "$id" "$id"
        done
    } >"$WORK/docs/STATUS.md"
}

# index ACTIVE_ROW [ARCHIVE_ROW] — write docs/plan/README.md with the given raw
# table rows. Both are passed whole so a case can vary any column, including
# dropping one.
index() {
    local active="$1"
    local archive="${2:-| [archive/zeta.md](archive/zeta.md) | Zeta scope | 2026-01-01, Q99 |}"
    {
        printf '# Plans\n\n## Implementation roadmap\n\n'
        printf '| Plan | Scope | Status |\n|---|---|---|\n'
        printf '%s\n' "$active"
        printf '\n## Archive\n\n| Plan | Scope | Closed |\n|---|---|---|\n'
        printf '%s\n' "$archive"
    } >"$WORK/docs/plan/README.md"
}

# expect NAME WANT [SUBSTRING] — run the gate inside WORK, compare its exit
# status with WANT, and require SUBSTRING in its output when given. The gate
# resolves its own root, so running it from the fixture scopes it there.
expect() {
    local name="$1" want="$2" want_text="${3:-}" got=0
    ( cd "$WORK" && "$GATE" ) >"$WORK/gate.out" 2>&1 || got=$?
    if [[ "$got" != "$want" ]]; then
        printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
        awk '{ print "    " $0 }' "$WORK/gate.out"
        fails=$(( fails + 1 ))
        return
    fi
    if [[ -n "$want_text" ]] && ! grep -qF "$want_text" "$WORK/gate.out"; then
        printf 'FAIL %s: exit %s as wanted, but output lacks %s\n' "$name" "$want" "$want_text"
        awk '{ print "    " $0 }' "$WORK/gate.out"
        fails=$(( fails + 1 ))
        return
    fi
    printf 'ok   %s\n' "$name"
}

# --- red: the staleness the rule exists to catch ---------------------------

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open. Q10 remains, then the reconciliation |'
expect 'a live Queue row named bare in a Status cell is rejected' 1 'names Q10'

new_repo '' 'Q10'
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open. Q10 remains, then the reconciliation |'
expect 'a live Deferred row named bare in a Status cell is rejected' 1 'names Q10'

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q99](../STATUS.md#Q99) |'
expect 'a Status cell linking a row that no longer exists is rejected' 1 'links Q99'

new_repo 'Q10 Q20' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q10](../STATUS.md#Q20) |'
expect 'a Status cell link labelling one row and targeting another is rejected' 1 'links Q10 -> #Q20'

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope with no status cell |'
expect 'a plan row missing its Status cell is rejected' 1 'exactly three columns'

# The escaped pipe belongs to the Scope cell. Split naively it becomes a column
# boundary, the Status cell reads as ` Alpha scope ` and the live bare ID below
# it goes unseen — so this case is green with the protection removed.
new_repo 'Q10' ''
# shellcheck disable=SC2016 # the backticks are Markdown inline code, not substitution
index '| [alpha.md](alpha.md) | Alpha scope `a`\|`b` | ⚠️ Open. Q10 remains |'
expect 'an escaped pipe in the Scope cell does not hide the Status cell' 1 'names Q10'

# --- green: what must not trip it ------------------------------------------

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done — shipped as Q99, measured 2026-01-01 |'
expect 'a closed row named bare in a Status cell is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q10](../STATUS.md#Q10) |'
expect 'a live row linked from a Status cell is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope: the Q10 defect | ✅ Done — shipped as Q99 |'
expect 'a live row named bare in the Scope cell is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |' \
    '| [archive/zeta.md](archive/zeta.md) | Zeta scope | 2026-01-01, Q10 and Q99 |'
expect 'a live row named bare in an Archive row is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Q99 landed 2026-01-01; [Q10](../STATUS.md#Q10) remains |'
expect 'a cell mixing a landed bare ID and a live linked one is accepted' 0

if (( fails > 0 )); then
    printf '\ncheck-plan-index-test: FAILED — %d case(s)\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-plan-index-test: ok\n'
