#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-queue-rules.py.
#
# Every rule is paired: a store that must pass, and the same store carrying one
# introduced violation that must fail. A rule never shown failing is not
# evidence that it checks anything, and each of these four guards a loss that
# is silent by construction, so a vacuous pass would look identical to a real
# one.
#
# The fixtures build a real repository and set refs/remotes/origin/main with
# update-ref rather than pushing: the checker keys on the merge base, and a
# push to a fixture remote is denied by this workstation's branch guard.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="$HERE/check-queue-rules.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

item() {  # item <repo> <id> <labels-inline> [target]
    mkdir -p "$1/docs/queue"
    {
        printf -- '---\nid: %s\nrank: a0\nlabels: [%s]\nstatus: ready\nsize: S\n' "$2" "$3"
        if [[ -n "${4:-}" ]]; then printf 'target: %s\n' "$4"; fi
        printf -- '---\n\n# Title for %s\n\nA note.\n' "$2"
    } > "$1/docs/queue/$2.md"
}

newrepo() {  # newrepo <dir>  -> a repo with a store, a vocabulary and a base ref
    local r="$1"
    mkdir -p "$r/docs/queue" "$r/docs/development" "$r/docs/plan"
    git init -q -b main "$r"
    # Q820: no detached maintenance racing the next command in a fixture repo.
    git -C "$r" config maintenance.auto false
    git -C "$r" config user.email t@e.com
    git -C "$r" config user.name T
    # shellcheck disable=SC2016  # the backticks are markdown code spans in the
    # vocabulary line, which is exactly the form the checker parses.
    printf '**Labels:** `flake` `ci` `docs` `debt`\n' > "$r/docs/queue/README.md"
    printf '# Retired flake watch\n\nNothing yet.\n' > "$r/docs/development/flake-watch-retired.md"
    printf '| Plan | Scope | Status |\n|---|---|---|\n' > "$r/docs/plan/README.md"
    # Rule 13 shells out to the matcher, which resolves from the repo root, so
    # a fixture needs the real one rather than a stub -- the rule's whole claim
    # is about what that script scores.
    mkdir -p "$r/scripts/docs"
    cp "$HERE/find-duplicate-rows.sh" "$r/scripts/docs/"
}

titled() {  # titled <repo> <id> <title> [target] — an item with a chosen title
    mkdir -p "$1/docs/queue"
    {
        printf -- '---\nid: %s\nrank: a0\nlabels: [docs]\nstatus: ready\nsize: S\n' "$2"
        if [[ -n "${4:-}" ]]; then printf 'target: %s\n' "$4"; fi
        printf -- '---\n\n# %s\n\nA note.\n' "$3"
    } > "$1/docs/queue/$2.md"
}

seal() {  # seal <repo> — commit the base and make it the merge base
    git -C "$1" add -A
    git -C "$1" commit -qm base
    git -C "$1" update-ref refs/remotes/origin/main HEAD
    git -C "$1" checkout -q -b claude/work
}

run() {  # run <repo> -> rc, output in $TMP/out
    local rc=0
    (cd "$1" && python3 "$CHECKER") > "$TMP/out" 2>&1 || rc=$?
    return "$rc"
}

expect() {  # expect <want-rc> <repo> <name> [pattern]
    local want="$1" repo="$2" name="$3" pat="${4:-}" rc=0
    run "$repo" || rc=$?
    if [[ "$rc" != "$want" ]]; then
        bad "$name (rc=$rc want=$want)"
        sed 's/^/       /' "$TMP/out" | head -3
        return
    fi
    if [[ -n "$pat" ]] && ! grep -q "$pat" "$TMP/out"; then
        bad "$name (rc matched but output lacks '$pat')"
        sed 's/^/       /' "$TMP/out" | head -3
        return
    fi
    ok "$name"
}

# --- rule 8: a flake item may not vanish ----------------------------------

R="$TMP/r8"; newrepo "$R"
item "$R" Q1 "flake, ci"
item "$R" Q2 "docs"
seal "$R"

git -C "$R" rm -q docs/queue/Q1.md
git -C "$R" commit -qm "delete the flake item"
expect 1 "$R" "rule 8: deleting a flake item fails" "rule 8: Q1"

# The ledger is the intended exit, so it must actually clear the rule.
printf '\n- Q1 retired after a 50-run soak.\n' >> "$R/docs/development/flake-watch-retired.md"
git -C "$R" commit -qam "retire Q1 to the ledger"
expect 0 "$R" "rule 8: the ledger clears it"

# A non-flake item deleting freely is the control: without it the rule above is
# equally consistent with the checker refusing every deletion.
R="$TMP/r8b"; newrepo "$R"
item "$R" Q1 "flake"
item "$R" Q2 "docs"
seal "$R"
git -C "$R" rm -q docs/queue/Q2.md
git -C "$R" commit -qm "complete an ordinary item"
expect 0 "$R" "rule 8 control: an ordinary item deletes freely"

# --- rule 9: the last item of a plan flips its index row -------------------

R="$TMP/r9"; newrepo "$R"
item "$R" Q1 "ci" "../plan/thing.md"
item "$R" Q2 "docs"
printf '| [thing.md](thing.md) | A thing | ⚠️ Open |\n' >> "$R/docs/plan/README.md"
seal "$R"

git -C "$R" rm -q docs/queue/Q1.md
git -C "$R" commit -qm "complete the plan's last item"
expect 1 "$R" "rule 9: an open plan row after its last item goes fails" "rule 9: Q1"

python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/plan/README.md"
p.write_text(p.read_text().replace("⚠️ Open", "✅ Done"))
PY
git -C "$R" commit -qam "flip the plan row"
expect 0 "$R" "rule 9: flipping the row clears it"

# A plan keeping another live item must not fire — otherwise the rule would
# demand a flip every time any targeting item closed.
R="$TMP/r9b"; newrepo "$R"
item "$R" Q1 "ci" "../plan/thing.md"
item "$R" Q2 "ci" "../plan/thing.md"
printf '| [thing.md](thing.md) | A thing | ⚠️ Open |\n' >> "$R/docs/plan/README.md"
seal "$R"
git -C "$R" rm -q docs/queue/Q1.md
git -C "$R" commit -qm "complete one of two"
expect 0 "$R" "rule 9 control: a plan with an item left stays open"

# An anchored target still points at the plan, so a live item carrying one keeps
# the row legal. Q408 closing while Q539 and Q540 targeted its section 6 is the
# case: raw string comparison made those two invisible.
R="$TMP/r9c"; newrepo "$R"
item "$R" Q1 "ci" "../plan/thing.md"
item "$R" Q2 "ci" "../plan/thing.md#6-follow-on"
printf '| [thing.md](thing.md) | A thing | \u26a0\ufe0f Open |\n' >> "$R/docs/plan/README.md"
seal "$R"
git -C "$R" rm -q docs/queue/Q1.md
git -C "$R" commit -qm "complete the item with the unanchored target"
expect 0 "$R" "rule 9 control: an anchored target still counts as live"

# The other direction: the anchor must not excuse a plan whose last item goes.
R="$TMP/r9d"; newrepo "$R"
item "$R" Q1 "ci" "../plan/thing.md#6-follow-on"
item "$R" Q2 "docs"
printf '| [thing.md](thing.md) | A thing | \u26a0\ufe0f Open |\n' >> "$R/docs/plan/README.md"
seal "$R"
git -C "$R" rm -q docs/queue/Q1.md
git -C "$R" commit -qm "complete the plan's last item, anchored"
expect 1 "$R" "rule 9: an anchored last item still obliges the flip" "rule 9: Q1"

# --- rule 11: the label vocabulary is closed -------------------------------

R="$TMP/r11"; newrepo "$R"
item "$R" Q1 "ci"
seal "$R"
item "$R" Q2 "documentation"          # a typo for `docs`
git -C "$R" add -A && git -C "$R" commit -qm "file an item with an undeclared label"
expect 1 "$R" "rule 11: an undeclared label fails" "rule 11: Q2"

python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/queue/README.md"
p.write_text("**Labels:** `flake` `ci` `docs` `debt` `documentation`\n")
PY
git -C "$R" commit -qam "declare the label"
expect 0 "$R" "rule 11: declaring the label clears it"

# The block-list form is the one `queue.py migrate` actually writes, so a
# suite that only ever files inline labels never exercises the real store's
# shape. Both arms, because a parser returning nothing passes rule 11 for the
# same reason a correct one does.
R="$TMP/r11block"; newrepo "$R"
item "$R" Q1 "ci"
seal "$R"
{
    printf -- '---\nid: Q2\nrank: a1\nlabels:\n    - ci\n    - documentation\n'
    printf -- 'status: ready\nsize: S\n---\n\n# Title for Q2\n\nA note.\n'
} > "$R/docs/queue/Q2.md"
git -C "$R" add -A && git -C "$R" commit -qm "file a block-list item"
expect 1 "$R" "rule 11: an undeclared label in block form fails" "rule 11: Q2"

python3 - "$R" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1]) / "docs/queue/Q2.md"
p.write_text(p.read_text().replace("    - documentation\n", "    - docs\n"))
PY
git -C "$R" commit -qam "use a declared label"
expect 0 "$R" "rule 11 control: block-form labels that are declared pass"

# --- reads that could not be taken are not verdicts ------------------------

R="$TMP/novocab"; newrepo "$R"
item "$R" Q1 "ci"
rm "$R/docs/queue/README.md"
seal "$R"
expect 2 "$R" "an absent vocabulary exits unmeasurable, not ok" "refusing to guess"

# --- an absent store says so rather than passing quietly -------------------

R="$TMP/empty"; newrepo "$R"
rm -f "$R/docs/queue/README.md"
seal "$R"
expect 0 "$R" "an absent store reports 0 checked" "0 checked"

# --- rule 13: a filed item answers the near-duplicate search ---------------
#
# The pair is the one that bought the rule. Q987 was filed while the matcher
# scored Q922 at 0.50 and named it zero times, so three rows described one
# defect and two sessions collided (Q1045). These are those two titles.
#
# Every case commits: the checker reads HEAD, so an uncommitted file is not an
# added item and the red case would pass for the wrong reason.
# shellcheck disable=SC2016  # the backticks are markdown code spans in the
# titles as filed, and the scorer's stemmer sees them, so expanding them
# would score a different pair than the one this replays.
Q922T='Docs name the retired `lint-backlog.sh` as the live backlog linter'
# shellcheck disable=SC2016  # as above.
Q987T='Point live `lint-backlog.sh` references at `lint-queue.sh`'

R="$TMP/r13"
newrepo "$R"
titled "$R" Q1 "$Q922T" ../development/maintaining-backlog.md
seal "$R"
titled "$R" Q2 "$Q987T" ../development/maintaining-backlog.md
git -C "$R" add -A && git -C "$R" commit -qm "file the second row"
expect 1 "$R" "rule 13: a filed item naming no flagged candidate fails" "rule 13: Q2"

# The fix the message asks for: name it. Nothing else about the store changes,
# so a pass here is the citation and nothing else.
printf 'Distinct from [Q1](Q1.md): a narrower scope.\n' >> "$R/docs/queue/Q2.md"
git -C "$R" commit -qam "cite the candidate"
expect 0 "$R" "rule 13: citing the candidate clears it"

# The override, for a filing that read the warning and rejected it.
titled "$R" Q2 "$Q987T" ../development/maintaining-backlog.md
git -C "$R" commit -qam "drop the citation again"
expect 1 "$R" "rule 13: dropping the citation fails again" "rule 13: Q2"
QUEUE_ALLOW_UNCITED_DUPLICATE=Q2 expect 0 "$R" "rule 13: the override excuses one id"

# Control: an unrelated title must not be flagged, or the rule would demand a
# citation from every filing and the store would learn to set the override.
R="$TMP/r13-control"
newrepo "$R"
titled "$R" Q1 "$Q922T" ../development/maintaining-backlog.md
seal "$R"
titled "$R" Q2 'Worker pods leak when the reaper races a drain' ../design/05-security.md
git -C "$R" add -A && git -C "$R" commit -qm "file an unrelated row"
expect 0 "$R" "rule 13 control: an unrelated filing is not flagged"

# The frontmatter is metadata, not an answer: a `target:` naming the flagged
# item must not clear the rule. Targets are chosen for where the work lands, and
# one that happens to name a candidate would satisfy the gate with nothing read.
#
# The flag has to come from shared words here, not from the target, because the
# target is itself a matcher signal: pointing it at the candidate to test the
# evasion would stop the pair being flagged at all.
R="$TMP/r13-frontmatter"
newrepo "$R"
titled "$R" Q1 "$Q922T" ../development/maintaining-backlog.md
seal "$R"
titled "$R" Q2 'Docs name the retired backlog linter as a live linter' ../queue/Q1.md
git -C "$R" add -A && git -C "$R" commit -qm "file a row whose target names the candidate"
expect 1 "$R" "rule 13: a target naming the candidate does not clear it" "rule 13: Q2"

# It shells out, so a matcher it cannot run is a read it cannot take. That is
# exit 2 and never a pass: a rule that silently returns "no candidates" when its
# instrument is missing reports exactly what a clean store reports.
R="$TMP/r13-unreadable"
newrepo "$R"
titled "$R" Q1 "$Q922T" ../development/maintaining-backlog.md
seal "$R"
titled "$R" Q2 "$Q987T" ../development/maintaining-backlog.md
git -C "$R" add -A && git -C "$R" commit -qm "file a row the matcher would flag"
rm -f "$R/scripts/docs/find-duplicate-rows.sh"
expect 2 "$R" "rule 13: an absent matcher refuses rather than passing"

# Two mutually-duplicate rows filed in ONE branch stay green, because the score
# runs against the base rather than against what the branch is adding. That is
# the retro case -- a session filing three findings at once -- so a regression
# here reddens every retro rather than a rare filing. Without this the base
# scoping is untested: dropping `--store base_dir` and letting the matcher read
# the live store leaves the rest of this suite fully green.
R="$TMP/r13-together"
newrepo "$R"
titled "$R" Q1 'An unrelated row that anchors the store' ../design/05-security.md
seal "$R"
titled "$R" Q2 "$Q922T" ../development/maintaining-backlog.md
titled "$R" Q3 "$Q987T" ../development/maintaining-backlog.md
# shellcheck disable=SC2016  # a markdown code span, as in the filed title.
titled "$R" Q4 'Point live `lint-backlog.sh` references at the store gates' ../development/maintaining-backlog.md
git -C "$R" add -A && git -C "$R" commit -qm "file three findings at once"
expect 0 "$R" "rule 13: rows filed together are not scored against each other"

# Control: only what this branch ADDS is scored, so pairs already on main cannot
# redden it. Same two titles as the red case, both present at the base.
R="$TMP/r13-base"
newrepo "$R"
titled "$R" Q1 "$Q922T" ../development/maintaining-backlog.md
titled "$R" Q2 "$Q987T" ../development/maintaining-backlog.md
seal "$R"
expect 0 "$R" "rule 13 control: a pair already at the base is not this branch's"

if (( fail )); then
    printf '\ncheck-queue-rules-test: FAILED\n'
    exit 1
fi
printf '\ncheck-queue-rules-test: ok\n'
