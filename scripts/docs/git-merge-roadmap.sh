#!/usr/bin/env bash
#
# git-merge-roadmap.sh — a git merge driver for docs/roadmap.md that resolves
# its annotated bullets by backlog ID, and falls back to ordinary conflict
# markers for anything it is not certain about. The siblings are
# scripts/docs/git-merge-script-index.sh, git-merge-plan-index.sh and
# scripts/ci/git-merge-gate-lists.sh; the merge itself is Go, in
# devtools/git/mergedriver, and this script is the entry point git config names.
#
# WHY. Every roadmap bullet is bound to a backlog row by a `<!-- q:QN -->`
# annotation, so shipping a gated item deletes that bullet — the same edit,
# concentrated in the same two sections, on every gate PR. Measured on
# docs/roadmap.md at 61cf54e7b: two branches each deleting their own bullet from
# the "In progress / near-term" list conflict under a plain three-way merge,
# while the same two deletions ten bullets apart merge clean. Q715's PR met that
# shape three times in one session, each an eviction followed by a hand-resolved
# rebase. This makes that rebase silent; it does not make the eviction rarer
# (see LIMITS).
#
# WHAT IT DOES. Each run of top-level bullets whose members all carry an
# annotation is merged as a set keyed on the annotation's normalized ID list —
# the same `<!-- q:QN[,QM…] -->` binding devtools/docs/roadmapcheck parses, so
# the driver and the gate read a bullet's identity the same way. A bullet
# deleted on either side is deleted, added on either side is present, changed on
# one side takes that change, and bullet order is reconstructed from whichever
# side reordered. The frontmatter, the headings and the prose between the lists
# merge exactly as git would have merged them.
#
# A bullet spans several lines, which the shared record merge does not model, so
# each one is encoded onto a single line with SOH standing in for the newline
# and decoded again afterwards. The blank lines *between* bullets are held to
# one side and rebuilt after the merge, because a bullet does not own the
# spacing around it: fold the trailing blank into the record and deleting a
# list's last bullet reads as an edit of its neighbour, which then collides with
# the other side deleting that neighbour — the merge this exists to resolve.
#
# WHAT IT REFUSES TO DO. Any uncertainty ends the same way: re-run the plain
# three-way merge and leave its conflict markers, with a one-line reason on
# stderr. A bullet changed on both sides, deleted on one side and edited on the
# other, the same binding added twice with different text, bullets reordered on
# both sides, a bullet the annotation parser cannot key, a side that added or
# removed a whole list, a source line that already contains the record
# separator, or a merged result in which one binding appears twice — all get
# markers. A conflict marker costs a minute; a wrong silent resolution drops a
# roadmap commitment.
#
# A run holding even one unannotated bullet is prose to this driver, not a
# mergeable list, so an ordinary bulleted paragraph elsewhere on the page merges
# the way it always did.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # installs every driver in this repo together
#
# .gitattributes already routes docs/roadmap.md to `merge=roadmap`, but git will
# not let a tracked file define the driver's command — that would be remote code
# execution on clone — so the `merge.roadmap.driver` config is per-clone and
# opt-in. Until you run the setup, the attribute names an undefined driver and
# git silently uses its built-in three-way merge: the pre-driver behaviour,
# exactly. Nothing about this repo requires it.
#
# LIMITS. It runs on local merges, rebases, cherry-picks and stash
# applications. It does not run anywhere GitHub does the merging — neither the
# squash-merge nor the merge queue's candidate build, which have no access to a
# clone's config — so two branches deleting adjacent bullets still conflict
# server-side and still get evicted. What this removes is the hand-resolution of
# the rebase that follows. An annotation inside a fenced code block is
# prose about the format to roadmapcheck but a binding to this driver; the page
# carries no such fence, and one would only cost a fallback.
#
# One consequence worth knowing: the PR that *adds* a routing line cannot
# benefit from it. git reads .gitattributes from the base during a rebase, so
# the routing is not in effect for the commit that introduces it, and that
# first rebase resolves by hand. Measured 2026-08-11 landing this driver.
#
# Usage (as configured by --install; git substitutes the placeholders):
#   git-merge-roadmap.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRIVER_SUBCOMMAND='roadmap'
DRIVER_NAME='roadmap'
DRIVER_PATH='scripts/docs/git-merge-roadmap.sh'
DRIVER_DESC='docs/roadmap.md: merge annotated bullets by backlog ID, else conflict markers'
DRIVER_INSTALL_NOTE='  docs/roadmap.md conflicts now resolve by <!-- q:QN --> binding during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_LOG='merge-roadmap'
DRIVER_SELF="${BASH_SOURCE[0]}"

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_exec "$@"
