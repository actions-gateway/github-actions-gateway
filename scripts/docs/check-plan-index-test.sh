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
# The next block covers invariant 4, the shipped-release rule (Q812). Its red
# cases replay the cell invariant 3 was green on for nine days, so they are also
# the assertion that the two rules do not overlap.
#
# The last block covers invariant 5, the plan doc's own Status paragraph (Q893).
# Its green cases carry the weight: the rule reads free prose rather than a table
# cell, so what must *not* trip it — a status paragraph below a heading, an ID
# under the blank line, an ID inside a link to somewhere else — is where a rule
# this shape goes noisy first, and every one of them is a real shape on `main`.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
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
# files every fixture indexes, and a docs/queue/ store carrying the given
# space-separated IDs. PLAN names the single active plan file, which the release
# cases point at a release-X.Y.md.
#
# Every item's body targets the plan, which keeps invariant 1 satisfied so a case
# only ever fails on the rule it is testing — the job the Progress row did before
# Q889 deleted it. A case testing invariant 1 itself calls unback to take it away.
new_repo() {
    local queue="$1" deferred="$2" plan="${3:-alpha.md}" id
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/docs/plan/archive" "$WORK/docs/queue"
    printf '# Alpha\n' >"$WORK/docs/plan/$plan"
    printf '# Zeta\n' >"$WORK/docs/plan/archive/zeta.md"
    for id in $queue $deferred; do
        {
            printf -- '---\nid: %s\nrank: a1\nlabels:\n    - docs\n' "$id"
            printf -- 'status: ready\nsize: S\n---\n\n# Thing\n\n'
            printf -- 'Tracked in [the plan](../plan/%s).\n' "$plan"
        } >"$WORK/docs/queue/$id.md"
    done
}

# unback — strip the plan target from every item, leaving the store with no item
# that names the plan. Invariant 1 then rests entirely on what the Status cell
# itself links, which is the half these cases vary.
unback() {
    local f
    for f in "$WORK"/docs/queue/Q*.md; do
        [[ -e "$f" ]] || continue
        printf -- '---\nid: %s\nrank: a1\nlabels:\n    - docs\nstatus: ready\nsize: S\n---\n\n# Thing\n\nnote\n' \
            "$(basename "$f" .md)" >"$f"
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

# plan_body LINES... — overwrite the active plan file with an H1 and the given
# lines, so a case can vary the Status paragraph invariant 5 reads. new_repo
# writes a bare H1, which is what keeps every case above this block out of the
# rule: no `**Status` line, no preamble, nothing charged.
plan_body() {
    {
        printf '# Alpha\n\n'
        printf '%s\n' "$@"
    } >"$WORK/docs/plan/alpha.md"
}

# expect NAME WANT [SUBSTRING] — run the gate inside WORK, compare its exit
# status with WANT, and require SUBSTRING in its output when given. The gate
# resolves its own root, so running it from the fixture scopes it there, and
# $TAG hands it the release: an empty value falls through to the fixture's own
# (absent) tags, which is how the pre-Q812 cases keep meaning what they did.
expect() {
    local name="$1" want="$2" want_text="${3:-}" got=0
    ( cd "$WORK" && GAG_RELEASE_TAG="$TAG" "$GATE" ) >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
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

# --- invariant 1: a row claiming open work must be backed by a live item ----
#
# Untested before Q889, because the fixture's Progress row satisfied the old
# form of this rule in every case. Both directions are asserted: a claim with
# nothing behind it is rejected, and each of the three ways a row can be legal
# is accepted on its own, so a rule that stopped reading any one of them fails
# here rather than passing quietly.

new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, more to do |'
expect 'an open-marked row no item carries is rejected' 1 'claim open work no backlog item carries'

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, more to do |'
expect 'an open-marked row an item targets is accepted' 0

new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope | ⚠️ Open, gated on [Q10](../queue/Q10.md) |'
expect 'an open-marked row linking a live item is accepted' 0

new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done — nothing left |'
expect 'a done row needs no backing item' 0

new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope | ⓘ Ongoing strategy, ⚠️ revisited each release |'
expect 'an ⓘ row is exempt even carrying an open marker' 0

# The marker is read from the Status cell alone. A Scope cell describing what a
# plan is about can carry one in prose, and charging the row for it would gate
# the description rather than the claim.
new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope: why ❌ was the wrong default | ✅ Done |'
expect 'an open marker in the Scope cell does not charge the row' 0

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


# --- invariant 5: the plan doc's own Status paragraph (Q893) ----------------
#
# 5a is invariant 3's rule one file lower, so its cases mirror that block. 5b
# has no counterpart there: it charges a paragraph that pins its claim to no
# live row at all, which is the only half that could have caught Q893's own
# instance — merge-drivers-go.md read "Phase 1 in progress" for four days after
# Phase 1 merged, under an index cell invariant 1 was correctly green on.

# Each 5a case links a live row as well, so 5b is satisfied and the only thing
# that can turn the case red is the rule it names. Without that the fixture goes
# red either way and the substring is the whole assertion.
new_repo 'Q10 Q20' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** phase 1 is [Q20](../queue/Q20.md); phase 2 tracked by Q10.'
expect 'a live Queue row named bare in a Status paragraph is rejected' 1 'names Q10'

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** phase 1 is [Q10](../queue/Q10.md); phase 2 was [Q99](../queue/Q99.md).'
expect 'a Status paragraph linking a row that no longer exists is rejected' 1 'links Q99'

new_repo 'Q10 Q20' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** phase 1 is [Q20](../queue/Q20.md); phase 2 is [Q10](../queue/Q20.md).'
expect 'a Status paragraph link labelling one row and targeting another is rejected' 1 'links Q10 -> Q20'

# Q893's own shape: the claim names no row, so nothing dies when the work lands.
new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | 🚧 In progress |'
plan_body '**Status:** filed 2026-08-30.' 'Phase 1 in progress.'
expect 'a Status paragraph pinned to no live row is rejected' 1 'pin their claim to no live Queue row'

# --- green: what must not trip it ------------------------------------------

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | 🚧 In progress |'
plan_body '**Status:** phase 1 shipped; phase 2 is [Q10](../queue/Q10.md).'
expect 'a Status paragraph linking the live row is accepted' 0

# 5b fires on the store backing the plan, not on the prose. With nothing naming
# the plan there is no work in flight for the paragraph to go stale about.
new_repo 'Q10' ''
unback
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** complete, shipped as Q99.'
expect 'a Status paragraph on a plan no item names is accepted' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** ⓘ informational — read-only analysis, tracked by Q10.'
expect 'an ⓘ Status paragraph is exempt even naming a live row bare' 0

# The paragraph ends at the blank line. Below it the doc states its scope and
# method, which describe the work rather than claim its state — the same cut
# invariant 3 makes when it reads column 3 and leaves the Scope cell alone.
# v2-api-gap-analysis.md names Q273 that way in its Goal sentence.
new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** phase 1 is [Q10](../queue/Q10.md).' '' 'Goal: the thing Q10 was about.'
expect 'a live ID below the Status paragraph is not charged' 0

# A `**Status` line under a heading is not the rollup. security.md carries one
# at line 1035, a thousand lines below anything a reader takes as the plan state.
new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body 'Intro.' '' '## Workstream 2' '' '**Status:** open, tracked by Q10.'
expect 'a **Status line below the first heading is not a preamble' 0

# An ID inside a link to somewhere else is already pinned to something the
# reader can follow. q224-fanout-dispatch-lever-spike.md writes exactly this.
new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** blocked on [Option E / Q10](beta.md), see [Q10](../queue/Q10.md).'
expect 'an ID inside a link to a plan doc is not charged as bare' 0

new_repo 'Q10' ''
index '| [alpha.md](alpha.md) | Alpha scope | ✅ Done |'
plan_body '**Status:** complete, shipped as Q99; residual is [Q10](../queue/Q10.md).'
expect 'a closed row named bare beside a live linked one is accepted' 0

if (( fails > 0 )); then
    printf '\ncheck-plan-index-test: FAILED — %d case(s)\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-plan-index-test: ok\n'
