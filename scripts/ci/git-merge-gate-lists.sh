#!/usr/bin/env bash
#
# git-merge-gate-lists.sh — a git merge driver for mk/gate-lists.mk that
# resolves its gate and suite list variables entry by entry, and falls back to
# ordinary conflict markers for anything it is not certain about. The Markdown
# siblings are scripts/docs/git-merge-roadmap.sh, git-merge-plan-index.sh and
# git-merge-script-index.sh; the merge itself is Go, in
# devtools/git/mergedriver, and this script is the entry point git config names.
#
# WHY. Adding a gated script is not optional work in this repo: gate-lists-check
# fails when SCRIPTS_TESTS disagrees with the scripts/**/*-test.sh files on
# disk, so a PR shipping one must append to a list here. Those lists are
# backslash-continued, so two PRs appending entries land on adjacent lines.
# Measured 2026-08-11 by replaying the real file: two branches each appending
# one suite entry conflict under a plain three-way merge, and resolve cleanly
# under this driver.
#
# What that buys is a trivial rebase instead of a hand-resolved one. It does NOT
# reduce merge-queue evictions, and no local driver can: GitHub performs the
# queue's merge itself and never runs a per-clone driver. Serializing the PRs
# that touch one registry is what prevents an eviction.
#
# WHAT IT DOES. Only the variables named in the driver's own managed list are
# treated specially — `--managed-vars` prints it. Each side's assignment for
# such a variable is lifted out and replaced by a one-line sentinel, the rest of
# the Makefile is merged exactly as git would have merged it, and each lifted
# list is merged as a set of entries: an entry added on either side is present,
# an entry removed on either side is absent, and the surviving entries keep
# base's order with each side's additions appended. The rendered block reuses
# the assignment operator, the continuation style and the indent already in
# ours, so the result reads like the file it came from.
#
# The entry set merge is the same devtools/git/keyedrecords the Markdown drivers
# use, reached with an identity key because an entry here is a bare word. It
# runs under that package's BaseThenAdditions order: a Makefile list is a set
# make expands, so nothing reads anything into its order, and the row-order
# reconstruction the Markdown registries need would refuse a merge over a
# difference that means nothing here.
#
# Confining the clever part to a sentinel is the whole safety argument: a
# conflict anywhere else in the Makefile never reaches this driver's list logic,
# it reaches git's ordinary merge and gets ordinary markers.
#
# The lists live in their own file rather than in the Makefile precisely so
# this driver can own the routed path outright: .gitattributes routes per file,
# and routing the whole Makefile would make every ordinary change to it count
# as driver-owned wherever that matters.
#
# WHAT IT REFUSES TO DO. Any uncertainty ends the same way: re-run the plain
# three-way merge and leave its conflict markers, with a one-line reason on
# stderr. A managed variable missing from a side, assigned twice on a side, or
# whose sentinel-substituted body still conflicts; an entry listed twice within
# one side; a rendered block whose entry set does not match the merged set;
# anything unparseable — all get markers. A marker costs a minute; a wrong
# silent resolution can drop a test suite from the gate, which is exactly the
# failure the gate exists to prevent.
#
# gate-lists-check is the backstop underneath this driver: it reconciles every
# managed list against the files on disk in both directions, so a resolution
# that silently drops or invents an entry fails `make check` rather than
# passing quietly.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # installs every driver in this repo together
#
# .gitattributes already routes mk/gate-lists.mk to `merge=gatelists`, but git
# will not let a tracked file define the driver's command — that would be remote
# code execution on clone — so the `merge.gatelists.driver` config is per-clone
# and opt-in. Until you run the setup, the attribute names an undefined driver and
# git silently uses its built-in three-way merge: the pre-driver behaviour,
# exactly.
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
#   git-merge-gate-lists.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
#
#   git-merge-gate-lists.sh --managed-vars
#     the lists this driver owns, one per line, so a caller reconciles the value
#     the driver runs on rather than re-deriving it from source.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRIVER_SUBCOMMAND='gatelists'
DRIVER_NAME='gatelists'
DRIVER_PATH='scripts/ci/git-merge-gate-lists.sh'
DRIVER_DESC='mk/gate-lists.mk: merge gate/suite list entries as a set, else conflict markers'
DRIVER_INSTALL_NOTE='  Makefile gate-list conflicts now resolve entry by entry during merge/rebase;
  anything else in the file still gets ordinary conflict markers.'
DRIVER_LOG='merge-gate-lists'
DRIVER_SELF="${BASH_SOURCE[0]}"

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_exec "$@"
