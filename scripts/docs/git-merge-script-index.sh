#!/usr/bin/env bash
#
# git-merge-script-index.sh — a git merge driver for scripts/README.md that
# resolves its per-group tables by script path, and falls back to ordinary
# conflict markers for anything it is not certain about. The siblings are
# scripts/docs/git-merge-plan-index.sh, git-merge-roadmap.sh and
# scripts/ci/git-merge-gate-lists.sh; the merge itself is Go, in
# devtools/git/mergedriver, and this script is the entry point git config names.
#
# WHY. scripts/README.md is a registry every new gated script must edit:
# check-script-docs.sh fails a script that has no row, so a PR adding one has no
# choice but to touch this file. Two such PRs land rows in the same group table,
# usually within a line or two of each other. Measured 2026-08-11 by replaying
# the real file: two branches each adding one row conflict under a plain
# three-way merge, and resolve cleanly under this driver.
#
# What that buys is a trivial rebase instead of a hand-resolved one. It does NOT
# reduce merge-queue evictions, and no local driver can: GitHub performs the
# queue's merge itself and never runs a per-clone driver. docs/STATUS.md is the
# standing demonstration — it has had an ID-keyed driver since Q611, local
# rebases resolve it silently, and PRs still go CONFLICTING on GitHub.
#
# WHAT IT DOES. Each table's data rows are merged as a set keyed on the link in
# column 1 — the same cell check-script-docs.sh reads, so the driver and the
# gate agree by construction. A row added on either side is present, a row
# deleted on either side is deleted, a row changed on one side takes that
# change, and row order is reconstructed from whichever side reordered. Prose
# between the tables merges exactly as git would have merged it.
#
# WHAT IT REFUSES TO DO. Any uncertainty ends the same way: re-run the plain
# three-way merge and leave its conflict markers, with a one-line reason on
# stderr. A row changed on both sides, a row deleted on one side and edited on
# the other, the same script added twice with different text, rows reordered on
# both sides, a row whose first cell is not a link, a side that added or removed
# a whole table, or a merged result listing one script twice — all get markers.
# A conflict marker costs a minute; a wrong silent resolution loses the registry
# row that makes a script discoverable.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # installs every driver in this repo together
#
# .gitattributes already routes scripts/README.md to `merge=scriptindex`, but
# git will not let a tracked file define the driver's command — that would be
# remote code execution on clone — so the `merge.scriptindex.driver` config is
# per-clone and opt-in. Until you run the setup, the attribute names an
# undefined driver and git silently uses its built-in three-way merge: the
# pre-driver behaviour, exactly.
#
# LIMITS. It runs on local merges, rebases, cherry-picks and stash
# applications. It does not run on GitHub's server-side squash-merge, which has
# no access to a clone's config — so it removes the rebase cost, not the
# merge-queue one.
#
# One consequence worth knowing: the PR that *adds* a routing line cannot
# benefit from it. git reads .gitattributes from the base during a rebase, so
# the routing is not in effect for the commit that introduces it, and that
# first rebase resolves by hand. Measured 2026-08-11 landing this driver.
#
# Usage (as configured by --install; git substitutes the placeholders):
#   git-merge-script-index.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRIVER_SUBCOMMAND='scriptindex'
DRIVER_NAME='scriptindex'
DRIVER_PATH='scripts/docs/git-merge-script-index.sh'
DRIVER_DESC='scripts/README.md: merge registry rows by script path, else conflict markers'
DRIVER_INSTALL_NOTE='  scripts/README.md conflicts now resolve by script path during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_LOG='merge-script-index'
DRIVER_SELF="${BASH_SOURCE[0]}"

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_exec "$@"
