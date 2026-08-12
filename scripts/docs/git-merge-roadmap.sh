#!/usr/bin/env bash
#
# git-merge-roadmap.sh — a git merge driver for docs/roadmap.md that resolves
# its annotated bullets by backlog ID, and falls back to ordinary conflict
# markers for anything it is not certain about. The siblings are
# scripts/docs/git-merge-status.sh (docs/STATUS.md) and
# scripts/docs/git-merge-plan-index.sh (docs/plan/README.md); all three share
# scripts/lib/merge-keyed-records.awk.
#
# WHY. Every roadmap bullet is bound to a backlog row by a `<!-- q:QN -->`
# annotation, so shipping a gated item deletes that bullet — the same edit,
# concentrated in the same two sections, on every gate PR. Measured on
# docs/roadmap.md at 61cf54e7b: two branches each deleting their own bullet from
# the "In progress / near-term" list conflict under a plain three-way merge,
# while the same two deletions ten bullets apart merge clean. Q715's PR was
# queue-evicted three times in one session on exactly that shape.
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
# removed a whole list, a source line that already contains SEP, or a merged
# result in which one binding appears twice — all get markers. A conflict marker
# costs a minute; a wrong silent resolution drops a roadmap commitment.
#
# A run holding even one unannotated bullet is prose to this driver, not a
# mergeable list, so an ordinary bulleted paragraph elsewhere on the page merges
# the way it always did.
#
# ONE-TIME SETUP, PER CLONE:
#
#   make merge-driver     # installs this and the other two drivers together
#
# .gitattributes already routes docs/roadmap.md to `merge=roadmap`, but git will
# not let a tracked file define the driver's command — that would be remote code
# execution on clone — so the `merge.roadmap.driver` config is per-clone and
# opt-in. Until you run the setup, the attribute names an undefined driver and
# git silently uses its built-in three-way merge: the pre-driver behaviour,
# exactly. Nothing about this repo requires it.
#
# LIMITS. It runs on local merges, rebases, cherry-picks and stash
# applications. It does not run on GitHub's server-side squash-merge, which has
# no access to a clone's config. An annotation inside a fenced code block is
# prose about the format to roadmapcheck but a binding to this driver; the page
# carries no such fence, and one would only cost a fallback.
#
# Usage (as configured by --install; git substitutes the placeholders):
#   git-merge-roadmap.sh %O %A %B %L %P %S %X %Y
#     %O base   %A ours (the result is written here)   %B theirs
#     %L conflict-marker size   %P the real pathname
#     %S %X %Y conflict labels (git >= 2.44; older git is handled)
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECORDS_AWK="$SCRIPT_DIR/../lib/merge-keyed-records.awk"

DRIVER_NAME='roadmap'
DRIVER_LOG='merge-roadmap'
DRIVER_PATH='scripts/docs/git-merge-roadmap.sh'
DRIVER_DESC='docs/roadmap.md: merge annotated bullets by backlog ID, else conflict markers'
DRIVER_INSTALL_NOTE='  docs/roadmap.md conflicts now resolve by <!-- q:QN --> binding during merge/rebase;
  anything ambiguous still gets ordinary conflict markers.'
DRIVER_SELF="${BASH_SOURCE[0]}"
DEFAULT_PATH='docs/roadmap.md'

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

merge_driver_init "$@"

# split_bullets FILE OUT_PREFIX — carve FILE into alternating prose and
# bullet-list segments: OUT_PREFIX.pre.N holds everything up to the line before
# list N starts, OUT_PREFIX.rows.N holds that list's bullets one encoded record
# per line, OUT_PREFIX.seps.N holds `BLANKS<TAB>RECORD` for each of them, and
# OUT_PREFIX.post holds the tail after the last list. OUT_PREFIX.count records
# how many lists were found.
#
# A list is a maximal run of top-level `- ` bullets, and it owns the blank lines
# that trail it — so the run reaches all the way to the next prose line, and the
# last bullet has a separator like every other. A run is only a list when every
# bullet in it is annotated; anything else is prose, which is how an ordinary
# bulleted paragraph keeps git's own merge.
#
# The separators are held beside the records rather than inside them because a
# bullet is not responsible for the spacing around it: fold the trailing blank
# into the record and deleting a list's last bullet reads as an *edit* of its
# neighbour, which collides with the other side deleting that neighbour — the
# exact merge this driver exists to resolve.
split_bullets() {
	local file="$1" out="$2"
	awk -v out="$out" '
		function is_blank(s) { return s ~ /^[ \t]*$/ }
		{ line[NR] = $0 }
		END {
			sep = sprintf("%c", 1)
			# The same payload class merge-keyed-records.awk keys on, so a run
			# is a list here exactly when every bullet in it is keyable there.
			annot = "<!--[ \t]*q:[^-" sep "]*-->"
			t = 0
			i = 1
			pre = ""
			while (i <= NR) {
				if (line[i] !~ /^- /) {
					pre = pre line[i] "\n"
					i++
					continue
				}
				# The run extends over every bullet, continuation and blank line
				# that follows, and stops at the first column-0 line that is
				# neither.
				j = i + 1
				while (j <= NR && (is_blank(line[j]) || line[j] ~ /^[ \t]/ || line[j] ~ /^- /))
					j++

				n = 0
				pend = ""
				pendn = 0
				plain = 1
				for (k = i; k < j; k++) {
					if (index(line[k], sep)) {
						printf "a source line already contains the record separator (line %d)\n", k > "/dev/stderr"
						exit 1
					}
					if (line[k] ~ /^- /) {
						if (n > 0) blanks[n] = pendn
						pend = ""
						pendn = 0
						rec[++n] = line[k]
					} else if (is_blank(line[k])) {
						# Undecided: a blank absorbed into the record if a
						# continuation follows, a separator if a bullet does.
						pend = pend sep line[k]
						pendn++
						if (line[k] != "") plain = 0
					} else {
						rec[n] = rec[n] pend sep line[k]
						pend = ""
						pendn = 0
					}
				}
				blanks[n] = pendn
				tailn = pendn

				# Checked on the assembled record rather than the first line: a
				# bullet whose title wraps can carry its annotation further down.
				# A separator that is not an empty line would not survive being
				# rebuilt from a count, so it disqualifies the run too.
				keyed = plain
				for (k = 1; k <= n; k++)
					if (rec[k] !~ annot) keyed = 0

				if (!keyed) {
					for (k = i; k < j; k++) pre = pre line[k] "\n"
				} else {
					t++
					f = sprintf("%s.pre.%d", out, t)
					printf "%s", pre > f
					close(f)
					pre = ""
					f = sprintf("%s.rows.%d", out, t)
					printf "" > f
					for (k = 1; k <= n; k++) print rec[k] > f
					close(f)
					f = sprintf("%s.seps.%d", out, t)
					printf "" > f
					for (k = 1; k <= n; k++) printf "%d\t%s\n", blanks[k], rec[k] > f
					close(f)
					f = sprintf("%s.tail.%d", out, t)
					printf "%d\n", tailn > f
					close(f)
				}
				i = j
			}
			f = out ".post"
			printf "%s", pre > f
			close(f)
			f = out ".count"
			printf "%d\n", t > f
			close(f)
			if (t == 0) {
				printf "no annotated bullet lists found\n" > "/dev/stderr"
				exit 1
			}
		}
	' "$file"
}

# decode_records TB TO TT BASE_SEPS OURS_SEPS THEIRS_SEPS MERGED_ROWS — turn one
# merged block of encoded records back into Markdown lines on stdout, restoring
# the blank lines around them. TB/TO/TT are the three sides' counts of blank
# lines trailing the whole list.
#
# A record is looked up by its own text, which is exactly what survived the set
# merge, so no key has to be recomputed here. The spacing after a record follows
# the same three-way rule the records themselves do, and a side that respaced a
# bullet the other side also respaced differently is uncertain like anything
# else. Exit 2 for the caller to fall back on.
#
# The last surviving record takes the list trailing count rather than its own
# recorded separator, because that separator described the bullet that used to
# follow it. Without the override, deleting a tight list's final bullet would
# leave its predecessor welded to the next heading.
decode_records() {
	awk -v tb="$1" -v to="$2" -v tt="$3" '
		function fail(msg) {
			printf "merge-roadmap-spacing: %s\n", msg > "/dev/stderr"
		}
		BEGIN {
			sep = sprintf("%c", 1)
			for (s = 0; s <= 2; s++) {
				while ((getline ln < ARGV[s + 1]) > 0) {
					p = index(ln, "\t")
					if (p == 0) continue
					have[s, substr(ln, p + 1)] = substr(ln, 1, p - 1)
				}
				close(ARGV[s + 1])
			}
			n = 0
			while ((getline ln < ARGV[4]) > 0) rec[++n] = ln
			close(ARGV[4])

			if (to == tt) tail = to
			else if (to == tb) tail = tt
			else if (tt == tb) tail = to
			else {
				fail("the blank lines after the list were changed on both sides")
				exit 2
			}

			for (k = 1; k <= n; k++) {
				r = rec[k]
				ho = ((1 SUBSEP r) in have)
				ht = ((2 SUBSEP r) in have)
				hb = ((0 SUBSEP r) in have)
				if (ho && ht) {
					if (have[1, r] == have[2, r]) v = have[1, r]
					else if (hb && have[1, r] == have[0, r]) v = have[2, r]
					else if (hb && have[2, r] == have[0, r]) v = have[1, r]
					else {
						fail("a bullet was respaced differently on both sides")
						exit 2
					}
				} else if (ho) v = have[1, r]
				else if (ht) v = have[2, r]
				else if (hb) v = have[0, r]
				# Unreachable while every side records a separator for every
				# record it holds, and a blank line if it ever is not.
				else v = 1
				if (k == n) v = tail
				s = r
				gsub(sep, "\n", s)
				print s
				for (b = 0; b < v; b++) print ""
			}
		}
	' "$4" "$5" "$6" "$7"
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
	if ! split_bullets "${side_pair#*:}" "$WORKDIR/$side" 2>"$WORKDIR/split.err"; then
		merge_driver_fallback \
			"$side: $(merge_driver_first_line "$WORKDIR/split.err" 'the bullet lists could not be located')"
	fi
done

BASE_N="$(cat "$WORKDIR/base.count")"
OURS_N="$(cat "$WORKDIR/ours.count")"
THEIRS_N="$(cat "$WORKDIR/theirs.count")"

# A side that added or dropped a whole list has restructured the page, and the
# per-list pairing this driver depends on no longer holds.
if (( OURS_N != BASE_N || THEIRS_N != BASE_N )); then
	merge_driver_fallback \
		"the sides disagree on how many annotated lists the page has (base $BASE_N, ours $OURS_N, theirs $THEIRS_N)"
fi

: >"$WORKDIR/result"
for (( t = 1; t <= BASE_N; t++ )); do
	if ! merge_text "$WORKDIR/ours.pre.$t" "$WORKDIR/base.pre.$t" \
		"$WORKDIR/theirs.pre.$t" "$WORKDIR/merged.pre.$t"; then
		merge_driver_fallback "the prose before list $t conflicts"
	fi
	if ! awk -v key_mode=marker -f "$RECORDS_AWK" \
		"$WORKDIR/base.rows.$t" "$WORKDIR/ours.rows.$t" "$WORKDIR/theirs.rows.$t" \
		>"$WORKDIR/merged.rows.$t" 2>"$WORKDIR/rows.err"; then
		merge_driver_fallback \
			"list $t: $(merge_driver_first_line "$WORKDIR/rows.err" 'the bullets could not be merged by backlog ID')"
	fi
	if ! decode_records \
		"$(cat "$WORKDIR/base.tail.$t")" "$(cat "$WORKDIR/ours.tail.$t")" \
		"$(cat "$WORKDIR/theirs.tail.$t")" \
		"$WORKDIR/base.seps.$t" "$WORKDIR/ours.seps.$t" \
		"$WORKDIR/theirs.seps.$t" "$WORKDIR/merged.rows.$t" \
		>"$WORKDIR/decoded.rows.$t" 2>"$WORKDIR/seps.err"; then
		merge_driver_fallback \
			"list $t: $(merge_driver_first_line "$WORKDIR/seps.err" 'the spacing around the bullets could not be rebuilt')"
	fi
	cat "$WORKDIR/merged.pre.$t" "$WORKDIR/decoded.rows.$t" >>"$WORKDIR/result"
done

if ! merge_text "$WORKDIR/ours.post" "$WORKDIR/base.post" \
	"$WORKDIR/theirs.post" "$WORKDIR/merged.post"; then
	merge_driver_fallback 'the prose after the last list conflicts'
fi
cat "$WORKDIR/merged.post" >>"$WORKDIR/result"

# Each list merged on its own, so nothing above can see a bullet that ended up
# in two of them — the shape one branch parking an item under "Exploring"
# produces while another moves it somewhere else. One binding, one bullet, whole
# page.
dupes="$(awk '
	/^- / {
		ids = ""
		rest = $0
		while (match(rest, /<!--[ \t]*q:[^-]*-->/)) {
			m = substr(rest, RSTART, RLENGTH)
			rest = substr(rest, RSTART + RLENGTH)
			sub(/^<!--[ \t]*q:/, "", m)
			sub(/-->$/, "", m)
			ids = ids "," m
		}
		# Normalized exactly as merge-keyed-records.awk normalizes its key, so a
		# binding is compared here the same way it was merged.
		gsub(/[ \t]/, "", ids)
		key = ""
		n = split(ids, f, ",")
		for (i = 1; i <= n; i++) {
			if (f[i] == "") continue
			key = (key == "") ? f[i] : key "," f[i]
		}
		if (key == "") next
		if (key in seen) print key
		seen[key] = 1
	}
' "$WORKDIR/result")"
if [[ -n "$dupes" ]]; then
	merge_driver_fallback "the merged page lists a backlog binding more than once: $(printf '%s' "$dupes" | tr '\n' ' ')"
fi

cp "$WORKDIR/result" "$OURS_FILE"
merge_driver_note "resolved $TARGET_PATH by backlog ID; review the bullet set before committing"
exit 0
