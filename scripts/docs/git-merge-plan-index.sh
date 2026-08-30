#!/usr/bin/env bash
#
# git-merge-plan-index.sh — a git merge driver for docs/plan/README.md that
# resolves its index tables by plan path, and falls back to ordinary conflict
# markers for anything it is not certain about. The siblings are
# scripts/docs/git-merge-script-index.sh, git-merge-roadmap.sh and
# scripts/ci/git-merge-gate-lists.sh; the merge itself is Go, in
# devtools/git/mergedriver, and this script is the entry point git config names.
#
# WHY. The plan index has STATUS.md's contention without STATUS.md's tooling.
# Every plan doc that lands adds one long row, every archival moves one to the
# top of the Archive table, and the topical sections concentrate those edits on
# the same few neighbours — so two branches routinely rewrite adjacent lines.
# Measured over the seven changes to the file that merged on 2026-08-03, four
# conflict when replayed from a common base (Q611).
#
# WHAT IT DOES. Each table's data rows are merged as a set keyed on the plan
# path in column 1 — the same cell scripts/docs/check-plan-index.sh reads, so
# the driver and the gate agree by construction. A row deleted on either side is
# deleted, a row added on either side is present, a row changed on one side
# takes that change, and row order is reconstructed from whichever side
# reordered. Prose between the tables merges exactly as git would have merged
# it.
#
# Archiving a plan is a delete in one table and an add in another, which the
# per-table set merge handles without either half knowing about the other.
#
# WHAT IT REFUSES TO DO. Any uncertainty ends the same way: re-run the plain
# three-way merge and leave its conflict markers, with a one-line reason on
# stderr. A row changed on both sides, a row deleted on one side and edited on
# the other, the same plan added twice with different text, rows reordered on
# both sides, a row whose first cell is not a link, a side that added or removed
# a whole table, or a merged result in which one plan appears in two tables —
# all get markers. A conflict marker costs a minute; a wrong silent resolution
# loses index state.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # installs this and the docs/STATUS.md driver together
#
# .gitattributes already routes docs/plan/README.md to `merge=planindex`, but
# git will not let a tracked file define the driver's command — that would be
# remote code execution on clone — so the `merge.planindex.driver` config is
# per-clone and opt-in. Until you run the setup, the attribute names an
# undefined driver and git silently uses its built-in three-way merge: the
# pre-driver behaviour, exactly. Nothing about this repo requires it.
#
# LIMITS. It runs on local merges, rebases, cherry-picks and stash
# applications. It does not run on GitHub's server-side squash-merge, which has
# no access to a clone's config.
#
# Usage (as configured by --install; git substitutes the placeholders):
#   git-merge-plan-index.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRIVER_SUBCOMMAND='planindex'
DRIVER_NAME='planindex'
DRIVER_PATH='scripts/docs/git-merge-plan-index.sh'
DRIVER_DESC='docs/plan/README.md: merge index rows by plan path, else conflict markers'
DRIVER_INSTALL_NOTE='  docs/plan/README.md conflicts now resolve by plan path during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_LOG='merge-plan-index'
DRIVER_SELF="${BASH_SOURCE[0]}"

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_exec "$@"
