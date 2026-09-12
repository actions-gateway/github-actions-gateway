#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-tier-claims.sh — the prose tier-claim stamp
# gate (Q848).
#
# Each case builds a throwaway repo holding a summary page and the canonical page
# it paraphrases, so the assertions are about the rules rather than about whatever
# the shipped pages happen to say today.
#
# The case that matters is the third: it replays the Q766 tier move — the measured
# failure this gate exists to catch — against a stamp taken before it, and requires
# red. Its control is the fourth, a non-tier edit to the same section, which must
# stay green. A digest gate that fired on every edit to a long section would be
# re-stamped without anyone re-reading the paraphrase, which is the same nothing
# the upkeep comment already was.
#
# The first two cases pin the second direction: a tier claim with no stamp is a
# finding, so deleting a stamp cannot quietly turn the gate off. That direction is
# why this suite exists at all — the gate passed its own first run because an
# upkeep comment and the paragraph under it are one block when no blank line
# separates them, and a "skip blocks that open with a comment" exemption swallowed
# exactly the paragraph being watched.
#
# The refusals pin the shapes that would otherwise pass by checking nothing: a
# stamp naming a file that is not there, and one naming an anchor that matches no
# heading.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-tier-claims.sh"

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

# new_repo — start a throwaway repo with the canonical source page in place. The
# summary page is written afterwards by summary().
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
    mkdir -p "$WORK/docs/operations"
    cat >"$WORK/docs/operations/canonical.md" <<'MD'
# Canonical

## Which Disruptions Auto-Re-Run

Some preamble that names no tier at all.
All but the last work on **both acquisition tiers**; the `vanished` row is `ScaleSet`-only.

| Disruption | Detected by |
|---|---|
| Kubelet eviction | pod Failed |

## A Later Section

This is outside the section above.
MD
}

# summary — write docs/why-gag.md from stdin.
summary() {
    mkdir -p "$WORK/docs"
    cat >"$WORK/docs/why-gag.md"
}

# digest — print the current digest of the canonical section, as the gate computes
# it. Taken from the gate's own code rather than restated, so the fixture cannot
# drift from the implementation and quietly assert nothing.
digest() {
    (cd "$WORK" && python3 - "$REPO_ROOT/scripts/docs/check-tier-claims.py" <<'PY'
import importlib.util, pathlib, sys
spec = importlib.util.spec_from_file_location("tc", sys.argv[1])
tc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tc)
src = pathlib.Path("docs/operations/canonical.md").read_text()
print(tc.tier_digest(tc.section_lines(src, "which-disruptions-auto-re-run")))
PY
    )
}

# run_gate NAME WANT — run the gate inside the throwaway repo and compare its exit
# status, which is what `make check` and the docs workflow consume.
run_gate() {
    local name="$1" want="$2" got=0
    (cd "$WORK" && "$GATE" docs/why-gag.md) >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# expect_out NAME PATTERN — assert the last run's output matched PATTERN.
expect_out() {
    local name="$1" pattern="$2"
    if grep -q -- "$pattern" "$WORK/gate.out"; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: no match for %s in:\n' "$name" "$pattern"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$((fails + 1))
}

# --- direction 2: a tier claim must be stamped ------------------------------

new_repo
summary <<'MD'
# Why

Eviction and preemption both come back, on both acquisition tiers.
MD
run_gate "an unstamped tier claim fails" 1
expect_out "the finding names the missing annotation" 'tier-source'

# The block shape that made the gate's own first run pass by checking nothing: an
# upkeep comment with no blank line before the paragraph it governs.
new_repo
summary <<'MD'
# Why

<!-- Some upkeep note about this paragraph. -->
Eviction and preemption both come back, on both acquisition tiers.
MD
run_gate "a claim sharing a block with a comment still fails" 1

# --- direction 1: the measured failure, and its control ---------------------

new_repo
STAMP="$(digest)"
summary <<MD
# Why

<!-- tier-source: docs/operations/canonical.md#which-disruptions-auto-re-run sha=${STAMP} -->
Eviction and preemption both come back, on both acquisition tiers.
MD
run_gate "a current stamp passes" 0

# Replay a tier move in the canonical section, the Q766 shape: the stamp is now
# stale and the paraphrase must be re-read.
awk '{ sub(/on \*\*both acquisition tiers\*\*/, "on the **classic tier** only"); print }' \
    "$WORK/docs/operations/canonical.md" >"$WORK/canonical.new"
mv "$WORK/canonical.new" "$WORK/docs/operations/canonical.md"
run_gate "a tier move in the source fails the stamp" 1
expect_out "the finding says how to re-stamp" 'tier-claims-check-write'

# The control: an edit to the same section that moves no tier must stay green,
# or the gate is noise and its stamps get refreshed unread.
new_repo
STAMP="$(digest)"
summary <<MD
# Why

<!-- tier-source: docs/operations/canonical.md#which-disruptions-auto-re-run sha=${STAMP} -->
Eviction and preemption both come back, on both acquisition tiers.
MD
awk '{ sub(/\| Kubelet eviction \| pod Failed \|/, "| Kubelet eviction (memory) | pod Failed |"); print }' \
    "$WORK/docs/operations/canonical.md" >"$WORK/canonical.new"
mv "$WORK/canonical.new" "$WORK/docs/operations/canonical.md"
run_gate "a non-tier edit to the same section passes" 0

# --- refusals ---------------------------------------------------------------

new_repo
summary <<'MD'
# Why

<!-- tier-source: docs/operations/gone.md#which-disruptions-auto-re-run sha=00000000 -->
Eviction and preemption both come back, on both acquisition tiers.
MD
run_gate "a stamp naming a missing file is a refusal" 2

new_repo
summary <<'MD'
# Why

<!-- tier-source: docs/operations/canonical.md#no-such-heading sha=00000000 -->
Eviction and preemption both come back, on both acquisition tiers.
MD
run_gate "a stamp naming a missing anchor is a refusal" 2

if ((fails > 0)); then
    printf '\n%d test(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\nall tests passed\n'
