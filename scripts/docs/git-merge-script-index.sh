#!/usr/bin/env bash
#
# git-merge-script-index.sh — a git merge driver for scripts/README.md that
# resolves its per-group tables by script path, and falls back to ordinary
# conflict markers for anything it is not certain about. The siblings are
# scripts/docs/git-merge-status.sh, git-merge-plan-index.sh and
# git-merge-roadmap.sh; all four share scripts/lib/merge-keyed-records.awk.
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
ROWS_AWK="$SCRIPT_DIR/../lib/merge-keyed-records.awk"

DRIVER_NAME='scriptindex'
DRIVER_LOG='merge-script-index'
DRIVER_PATH='scripts/docs/git-merge-script-index.sh'
DRIVER_DESC='scripts/README.md: merge registry rows by script path, else conflict markers'
DRIVER_INSTALL_NOTE='  scripts/README.md conflicts now resolve by script path during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_SELF="${BASH_SOURCE[0]}"
DEFAULT_PATH='scripts/README.md'

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_init "$@"

# split_tables FILE OUT_PREFIX — carve FILE into alternating prose and table-row
# segments: OUT_PREFIX.pre.N holds everything from the previous table's rows up
# to and including table N's header and `|---|` separator, OUT_PREFIX.rows.N
# holds that table's contiguous data rows, and OUT_PREFIX.post holds the tail
# after the last table. OUT_PREFIX.count records how many tables were found.
#
# Whole-file rather than one named section, like the plan-index driver: this
# page carries a table per script group, and a row can move between groups when
# a script is regrouped.
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
			"$side: $(merge_driver_first_line "$WORKDIR/split.err" 'the registry tables could not be located')"
	fi
done

BASE_N="$(cat "$WORKDIR/base.count")"
OURS_N="$(cat "$WORKDIR/ours.count")"
THEIRS_N="$(cat "$WORKDIR/theirs.count")"

# A side that added or dropped a whole table has restructured the registry, and
# the per-table pairing this driver depends on no longer holds.
if ((OURS_N != BASE_N || THEIRS_N != BASE_N)); then
	merge_driver_fallback \
		"the sides disagree on how many tables the registry has (base $BASE_N, ours $OURS_N, theirs $THEIRS_N)"
fi

: >"$WORKDIR/result"
for ((t = 1; t <= BASE_N; t++)); do
	if ! merge_text "$WORKDIR/ours.pre.$t" "$WORKDIR/base.pre.$t" \
		"$WORKDIR/theirs.pre.$t" "$WORKDIR/merged.pre.$t"; then
		merge_driver_fallback "the prose before table $t conflicts"
	fi
	if ! awk -v key_mode=link -f "$ROWS_AWK" \
		"$WORKDIR/base.rows.$t" "$WORKDIR/ours.rows.$t" "$WORKDIR/theirs.rows.$t" \
		>"$WORKDIR/merged.rows.$t" 2>"$WORKDIR/rows.err"; then
		merge_driver_fallback \
			"table $t: $(merge_driver_first_line "$WORKDIR/rows.err" 'the rows could not be merged by script path')"
	fi
	cat "$WORKDIR/merged.pre.$t" "$WORKDIR/merged.rows.$t" >>"$WORKDIR/result"
done

if ! merge_text "$WORKDIR/ours.post" "$WORKDIR/base.post" \
	"$WORKDIR/theirs.post" "$WORKDIR/merged.post"; then
	merge_driver_fallback 'the prose after the last table conflicts'
fi
cat "$WORKDIR/merged.post" >>"$WORKDIR/result"

# Each table merged on its own, so nothing above can see a script that ended up
# in two of them — the shape one branch regrouping a script while another edits
# its old row produces. One script, one row, whole page.
dupes="$(awk '
	/^\|/ {
		n = split($0, f, "|")
		if (n >= 3 && match(f[2], /\[[^]]*\]\([^)]+\)/)) {
			key = substr(f[2], RSTART, RLENGTH)
			sub(/^\[[^]]*\]\(/, "", key)
			sub(/\)$/, "", key)
			# Group rows in the summary table link to in-page anchors, not to
			# files; they share no namespace with script paths.
			if (key ~ /^#/) next
			if (key in seen) print key
			seen[key] = 1
		}
	}
' "$WORKDIR/result")"
if [[ -n "$dupes" ]]; then
	merge_driver_fallback "the merged registry lists a script more than once: $(printf '%s' "$dupes" | tr '\n' ' ')"
fi

cp "$WORKDIR/result" "$OURS_FILE"
merge_driver_note "resolved $TARGET_PATH by script path; review the row set before committing"
exit 0
