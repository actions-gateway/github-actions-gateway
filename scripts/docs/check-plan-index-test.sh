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
# The last block covers invariant 4, the shipped-release rule (Q812). Its red
# cases replay the cell invariant 3 was green on for nine days, so they are also
# the assertion that the two rules do not overlap.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-plan-index.sh"

fails=0
workdirs=()
WORK=""
# The release the gate resolves for the case about to run. Empty means "no tag",
# which a fixture repo is anyway: it has no tags and no origin to read them from.
TAG=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo QUEUE_IDS DEFERRED_IDS [PLAN] — start a throwaway repo holding the plan
# files every fixture indexes, and a docs/STATUS.md whose Queue and Deferred
# tables anchor the given space-separated IDs. The Progress row keeps invariant
# 1 satisfied, so a case only ever fails on the rule it is testing. PLAN names
# the single active plan file, which the release cases point at a release-X.Y.md.
new_repo() {
    local queue="$1" deferred="$2" plan="${3:-alpha.md}" id
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/docs/plan/archive"
    printf '# Alpha\n' >"$WORK/docs/plan/$plan"
    printf '# Zeta\n' >"$WORK/docs/plan/archive/zeta.md"
    # shellcheck disable=SC2016 # the backticks are a Markdown label cell, not substitution
    {
        printf '# Project Status\n\n## Progress\n\n| Plan | Labels | St |\n|---|---|---|\n'
        printf '| [Alpha](plan/%s) | `docs` | 🔲 |\n' "$plan"
        printf '\n## Queue\n\n| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
        for id in $queue; do
            printf '| <a id="%s"></a>%s | Thing | `docs` | 🔲 | S | note |\n' "$id" "$id"
        done
        printf '\n## Deferred\n\n| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
        for id in $deferred; do
            printf '| <a id="%s"></a>%s | Thing | `docs` | S | **Demand:** someone asks. |\n' "$id" "$id"
        done
    } >"$WORK/docs/STATUS.md"
    # Invariant 3 reads the live IDs from the store, invariant 1 still reads the
    # table for its Progress row (Q889). Both fixtures, because the checker
    # genuinely reads both.
    mkdir -p "$WORK/docs/queue"
    for id in $queue $deferred; do
        {
            printf -- '---\nid: %s\nrank: a1\nlabels:\n    - docs\n' "$id"
            printf -- 'status: ready\nsize: S\n---\n\n# Thing\n\nnote\n'
        } >"$WORK/docs/queue/$id.md"
    done
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
# resolves its own root, so running it from the fixture scopes it there, and
# $TAG hands it the release: an empty value falls through to the fixture's own
# (absent) tags, which is how the pre-Q812 cases keep meaning what they did.
expect() {
    local name="$1" want="$2" want_text="${3:-}" got=0
    ( cd "$WORK" && GAG_RELEASE_TAG="$TAG" "$GATE" ) >"$WORK/gate.out" 2>&1 || got=$?
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
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q99](../queue/Q99.md) |'
expect 'a Status cell linking a row that no longer exists is rejected' 1 'links Q99'

new_repo 'Q10 Q20' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q10](../queue/Q20.md) |'
expect 'a Status cell link labelling one row and targeting another is rejected' 1 'links Q10 -> Q20'

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
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q10](../queue/Q10.md) |'
expect 'a live row linked from a Status cell is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope: the Q10 defect | ✅ Done — shipped as Q99 |'
expect 'a live row named bare in the Scope cell is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |' \
    '| [archive/zeta.md](archive/zeta.md) | Zeta scope | 2026-01-01, Q10 and Q99 |'
expect 'a live row named bare in an Archive row is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Q99 landed 2026-01-01; [Q10](../queue/Q10.md) remains |'
expect 'a cell mixing a landed bare ID and a live linked one is accepted' 0

# --- invariant 4: a shipped release must not read as open (Q812) ------------

# The head of the real cell at ed8160c48^, which sat on `main` for the nine days
# after v1.3.0 shipped. Q484 is bare and no row anchors it, so invariant 3 holds
# and this fixture is green with invariant 4 removed — which is the point.
TAG=v1.3.0
new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | ❌ Open — one gate left from the pre-release API review, Q484 |'
expect 'a shipped release still marked ❌ is rejected' 1 'but the project has released v1.3.0'

new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | 🔲 Open. Every gate item landed |'
expect 'a shipped release still marked 🔲 is rejected' 1 'read as open for a release that has shipped'

# Released past the line, not merely at it.
TAG=v1.5.0
new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | 🚧 In progress |'
expect 'a release the project has moved past, marked 🚧, is rejected' 1 'is 🚧'

TAG=v1.4.0
new_repo 'Q10' '' 'release-1.5.md'
index '| [release-1.5.md](release-1.5.md) | The 1.5 release gate | 🔲 Open. All four gate items landed |'
expect 'an unshipped release marked open is accepted' 0

# ⚠️ stays legal after the tag: a residual Queue row is a real state, and the
# rule would be unusable if shipping forced ✅.
TAG=v1.3.0
new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | ⚠️ Shipped 2026-08-03; one residual remains |'
expect 'a shipped release marked ⚠️ is accepted' 0

new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | ✅ Done — 1.3 SHIPPED 2026-08-03 |'
expect 'a shipped release marked ✅ is accepted' 0

# A plan that is not a release makes no claim a tag can refute.
new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ❌ Open, nothing built |'
expect 'a non-release plan marked ❌ is accepted' 0

# No tag resolves in a fresh fork, so the check reports a skip rather than a
# verdict — and says so, because a silent skip reads exactly like a pass.
TAG=
new_repo 'Q10' '' 'release-1.3.md'
index '| [release-1.3.md](release-1.3.md) | The 1.3 release gate | ❌ Open |'
expect 'a tagless tree skips the release-row check' 0 'release-row check SKIPPED'

if (( fails > 0 )); then
    printf '\ncheck-plan-index-test: FAILED — %d case(s)\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-plan-index-test: ok\n'
