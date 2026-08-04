#!/usr/bin/env bash
#
# git-merge-status.sh — a git merge driver for docs/STATUS.md that resolves
# Queue-table conflicts by ID set-semantics, and falls back to ordinary conflict
# markers for anything it is not certain about.
#
# WHY. Almost every session edits the backlog, and the process concentrates the
# edits: pick from the top, insert at the priority the item deserves, flakes
# first. Adjacent-row edits are the one thing a plain three-way merge cannot
# absorb, so a four-worker dispatch batch that deletes rows 1-4 conflicts by
# construction (docs/development/queue-id-allocation.md § What this fixes, and
# what it does not). Resolving a row conflict is mechanical, but a *botched*
# resolution silently re-opens finished work, which is the expensive failure.
# Deciding it by ID rather than by line position removes both.
#
# WHAT IT DOES. The Queue table's data rows are merged as a set keyed on the
# row's `<a id="QN"></a>QN` anchor: a row deleted on either side is deleted, a
# row added on either side is present, a row changed on one side takes that
# change. Row order is reconstructed from whichever side reordered. Everything
# outside those rows — the header, the Progress table, the Deferred table — is
# merged exactly as git would have merged it, with no set-semantics applied.
# The rules and the ordering rebuild live in scripts/lib/merge-table-rows.awk;
# docs/plan/README.md gets the same treatment from git-merge-plan-index.sh.
#
# WHAT IT REFUSES TO DO. Any uncertainty ends the same way: re-run the plain
# three-way merge and leave its conflict markers. A row changed on both sides, a
# row deleted on one side and edited on the other, one ID filed twice with
# different text, a table it cannot parse, a conflict outside the Queue rows —
# all of them get markers and a one-line reason on stderr. A conflict marker
# costs a minute; a wrong silent resolution loses backlog state.
#
# It cannot resurrect a row that the other side deleted: a deletion either wins
# outright or produces markers, never a re-add. `make lint-backlog` (rule 10)
# remains the independent check on that, and still runs afterwards.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # or: scripts/docs/git-merge-status.sh --install
#
# .gitattributes already routes docs/STATUS.md to `merge=backlog`, but git will
# not let a tracked file define the driver's command — that would be remote code
# execution on clone — so the `merge.backlog.driver` config is per-clone and
# opt-in. Until you run the setup, the attribute names an undefined driver and
# git silently uses its built-in three-way merge: the pre-driver behaviour,
# exactly. Nothing about this repo requires the driver to be installed.
#
# LIMITS. It runs on local merges, rebases, cherry-picks and stash applications.
# It does not run on GitHub's server-side squash-merge, which has no access to
# a clone's config.
#
# Usage (as configured by --install; git substitutes the placeholders):
#   git-merge-status.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROWS_AWK="$SCRIPT_DIR/../lib/merge-table-rows.awk"

DRIVER_NAME='backlog'
DRIVER_LOG='merge-status'
DRIVER_PATH='scripts/docs/git-merge-status.sh'
DRIVER_DESC='docs/STATUS.md: merge Queue rows by ID set-semantics, else conflict markers'
DRIVER_INSTALL_NOTE='  docs/STATUS.md Queue conflicts now resolve by ID during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_SELF="${BASH_SOURCE[0]}"
DEFAULT_PATH='docs/STATUS.md'

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_init "$@"

# split_status FILE OUT_PREFIX — carve FILE into OUT_PREFIX.{prefix,rows,suffix}:
# everything up to and including the Queue table's `|---|` separator, then the
# table's contiguous data rows, then the rest of the file. Non-zero when the
# Queue table is not where it is supposed to be, which is a fallback case.
split_status() {
	local file="$1" out="$2"
	: >"$out.prefix"
	: >"$out.rows"
	: >"$out.suffix"
	awk -v prefix="$out.prefix" -v rows="$out.rows" -v suffix="$out.suffix" '
		state == 0 {
			print > prefix
			if ($0 ~ /^## Queue/) state = 1
			next
		}
		# In the section, walking the intro prose to the table header row.
		state == 1 {
			if ($0 ~ /^## /) { bad = "the ## Queue section holds no table"; exit 1 }
			print > prefix
			if ($0 ~ /^\|/) state = 2
			next
		}
		# The line after the header row must be the column separator.
		state == 2 {
			if ($0 !~ /^\|[-:| \t]+\|[ \t]*$/) {
				bad = "the Queue table header is not followed by a |---| separator"
				exit 1
			}
			print > prefix
			state = 3
			next
		}
		state == 3 {
			if ($0 ~ /^\|/) { print > rows; next }
			state = 4
		}
		state == 4 {
			if ($0 ~ /^## Queue/) { bad = "more than one ## Queue section"; exit 1 }
			print > suffix
		}
		END {
			if (bad == "" && state < 3) bad = "no ## Queue table found"
			if (bad != "") {
				printf "%s\n", bad > "/dev/stderr"
				exit 1
			}
		}
	' "$file"
}

for side_pair in "base:$BASE_FILE" "ours:$OURS_FILE" "theirs:$THEIRS_FILE"; do
	side="${side_pair%%:*}"
	if ! split_status "${side_pair#*:}" "$WORKDIR/$side" 2>"$WORKDIR/split.err"; then
		merge_driver_fallback "$side: $(merge_driver_first_line "$WORKDIR/split.err" 'the Queue table could not be located')"
	fi
done

# The Queue rows, by ID set-semantics.
if ! awk -v key_mode=anchor -f "$ROWS_AWK" \
	"$WORKDIR/base.rows" "$WORKDIR/ours.rows" "$WORKDIR/theirs.rows" \
	>"$WORKDIR/merged.rows" 2>"$WORKDIR/rows.err"; then
	merge_driver_fallback "$(merge_driver_first_line "$WORKDIR/rows.err" 'the Queue rows could not be merged by ID')"
fi

# Everything else is merged as plain text — this driver claims no special
# knowledge of the Progress or Deferred tables, so they behave as before.
for part in prefix suffix; do
	if ! git merge-file -p --marker-size="$MARKER_SIZE" \
		-L "$LABEL_OURS" -L "$LABEL_BASE" -L "$LABEL_THEIRS" \
		"$WORKDIR/ours.$part" "$WORKDIR/base.$part" "$WORKDIR/theirs.$part" \
		>"$WORKDIR/merged.$part" 2>/dev/null; then
		merge_driver_fallback "the file outside the Queue rows conflicts (${part})"
	fi
done

cat "$WORKDIR/merged.prefix" "$WORKDIR/merged.rows" "$WORKDIR/merged.suffix" \
	>"$WORKDIR/result"
cp "$WORKDIR/result" "$OURS_FILE"
merge_driver_note "resolved the $TARGET_PATH Queue table by ID; review the row set before committing"
exit 0
