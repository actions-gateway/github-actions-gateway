#!/usr/bin/env bash
#
# git-merge-plan-index.sh — a git merge driver for docs/plan/README.md that
# resolves its index tables by plan path, and falls back to ordinary conflict
# markers for anything it is not certain about. The docs/STATUS.md sibling is
# scripts/docs/git-merge-status.sh and the docs/roadmap.md one is
# git-merge-roadmap.sh; all three share scripts/lib/merge-keyed-records.awk.
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
ROWS_AWK="$SCRIPT_DIR/../lib/merge-keyed-records.awk"

DRIVER_NAME='planindex'
DRIVER_LOG='merge-plan-index'
DRIVER_PATH='scripts/docs/git-merge-plan-index.sh'
DRIVER_DESC='docs/plan/README.md: merge index rows by plan path, else conflict markers'
DRIVER_INSTALL_NOTE='  docs/plan/README.md conflicts now resolve by plan path during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_SELF="${BASH_SOURCE[0]}"
DEFAULT_PATH='docs/plan/README.md'

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_init "$@"

# split_tables FILE OUT_PREFIX — carve FILE into alternating prose and table-row
# segments: OUT_PREFIX.pre.N holds everything from the previous table's rows up
# to and including table N's header and `|---|` separator, OUT_PREFIX.rows.N
# holds that table's contiguous data rows, and OUT_PREFIX.post holds the tail
# after the last table. OUT_PREFIX.count records how many tables were found.
#
# Splitting on the whole file rather than on one named section is the difference
# from the STATUS.md driver: the plan index carries a table per topic and rows
# move between them, so a section-scoped split would see an archival as an
# unexplained deletion.
split_tables() {
	local file="$1" out="$2"
	awk -v out="$out" '
		{ line[NR] = $0 }
		END {
			sep = "^\\|[-:| \t]+\\|[ \t]*$"
			t = 0
			i = 1
			pre = ""
			while (i <= NR) {
				# A table starts at a header row whose next line is the column
				# separator. Anything else is prose, including a stray leading `|`.
				if (line[i] ~ /^\|/ && i < NR && line[i + 1] ~ sep) {
					t++
					f = sprintf("%s.pre.%d", out, t)
					printf "%s%s\n%s\n", pre, line[i], line[i + 1] > f
					close(f)
					pre = ""
					i += 2
					r = sprintf("%s.rows.%d", out, t)
					printf "" > r
					while (i <= NR && line[i] ~ /^\|/) {
						print line[i] > r
						i++
					}
					close(r)
				} else {
					pre = pre line[i] "\n"
					i++
				}
			}
			f = out ".post"
			printf "%s", pre > f
			close(f)
			f = out ".count"
			printf "%d\n", t > f
			close(f)
			if (t == 0) {
				printf "no Markdown tables found\n" > "/dev/stderr"
				exit 1
			}
		}
	' "$file"
}

# merge_text OURS BASE THEIRS OUT — a plain three-way merge of one prose
# segment, with the driver's conflict labels. Non-zero when it conflicts.
merge_text() {
	git merge-file -p --marker-size="$MARKER_SIZE" \
		-L "$LABEL_OURS" -L "$LABEL_BASE" -L "$LABEL_THEIRS" \
		"$1" "$2" "$3" >"$4" 2>/dev/null
}

for side_pair in "base:$BASE_FILE" "ours:$OURS_FILE" "theirs:$THEIRS_FILE"; do
	side="${side_pair%%:*}"
	if ! split_tables "${side_pair#*:}" "$WORKDIR/$side" 2>"$WORKDIR/split.err"; then
		merge_driver_fallback \
			"$side: $(merge_driver_first_line "$WORKDIR/split.err" 'the index tables could not be located')"
	fi
done

BASE_N="$(cat "$WORKDIR/base.count")"
OURS_N="$(cat "$WORKDIR/ours.count")"
THEIRS_N="$(cat "$WORKDIR/theirs.count")"

# A side that added or dropped a whole table has restructured the index, and the
# per-table pairing this driver depends on no longer holds.
if (( OURS_N != BASE_N || THEIRS_N != BASE_N )); then
	merge_driver_fallback \
		"the sides disagree on how many tables the index has (base $BASE_N, ours $OURS_N, theirs $THEIRS_N)"
fi

: >"$WORKDIR/result"
for (( t = 1; t <= BASE_N; t++ )); do
	if ! merge_text "$WORKDIR/ours.pre.$t" "$WORKDIR/base.pre.$t" \
		"$WORKDIR/theirs.pre.$t" "$WORKDIR/merged.pre.$t"; then
		merge_driver_fallback "the prose before table $t conflicts"
	fi
	if ! awk -v key_mode=link -f "$ROWS_AWK" \
		"$WORKDIR/base.rows.$t" "$WORKDIR/ours.rows.$t" "$WORKDIR/theirs.rows.$t" \
		>"$WORKDIR/merged.rows.$t" 2>"$WORKDIR/rows.err"; then
		merge_driver_fallback \
			"table $t: $(merge_driver_first_line "$WORKDIR/rows.err" 'the rows could not be merged by plan path')"
	fi
	cat "$WORKDIR/merged.pre.$t" "$WORKDIR/merged.rows.$t" >>"$WORKDIR/result"
done

if ! merge_text "$WORKDIR/ours.post" "$WORKDIR/base.post" \
	"$WORKDIR/theirs.post" "$WORKDIR/merged.post"; then
	merge_driver_fallback 'the prose after the last table conflicts'
fi
cat "$WORKDIR/merged.post" >>"$WORKDIR/result"

# Each table merged on its own, so nothing above can see a plan that ended up in
# two of them — the shape one branch archiving a plan while another relocates it
# produces. One plan, one row, whole file.
#
# Compared on the basename, because that is the plan's identity: `archive/x.md`
# and `x.md` are the same doc in two sections, and it is exactly that pair the
# per-table merge cannot rule out. check-plan-index.sh compares basenames against
# the disk tree for the same reason.
dupes="$(awk '
	/^\|/ {
		n = split($0, f, "|")
		if (n >= 3 && match(f[2], /\[[^]]*\]\([^)]+\)/)) {
			key = substr(f[2], RSTART, RLENGTH)
			sub(/^\[[^]]*\]\(/, "", key)
			sub(/\)$/, "", key)
			sub(/^archive\//, "", key)
			if (key in seen) print key
			seen[key] = 1
		}
	}
' "$WORKDIR/result")"
if [[ -n "$dupes" ]]; then
	merge_driver_fallback "the merged index lists a plan more than once: $(printf '%s' "$dupes" | tr '\n' ' ')"
fi

cp "$WORKDIR/result" "$OURS_FILE"
merge_driver_note "resolved $TARGET_PATH by plan path; review the row set before committing"
exit 0
