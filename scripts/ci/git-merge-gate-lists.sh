#!/usr/bin/env bash
#
# git-merge-gate-lists.sh — a git merge driver for mk/gate-lists.mk that
# resolves its gate and suite list variables entry by entry, and falls back to
# ordinary conflict markers for anything it is not certain about. The Markdown siblings
# are scripts/docs/git-merge-status.sh, git-merge-plan-index.sh and
# git-merge-script-index.sh.
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
# WHAT IT DOES. Only the variables named in MANAGED_VARS are treated specially.
# Each side's assignment for such a variable is lifted out and replaced by a
# one-line sentinel, the rest of the Makefile is merged exactly as git would
# have merged it, and each lifted list is merged as a set of entries: an entry
# added on either side is present, an entry removed on either side is absent,
# and the surviving entries keep base's order with each side's additions
# appended in the order that side introduced them. The rendered block reuses the
# assignment operator, the continuation style and the indent already in ours, so
# the result reads like the file it came from.
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
# whose sentinel-substituted body still conflicts; a rendered block whose entry
# set does not match the merged set; anything unparseable — all get markers. A
# marker costs a minute; a wrong silent resolution can drop a test suite from
# the gate, which is exactly the failure the gate exists to prevent.
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
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRIVER_NAME='gatelists'
DRIVER_LOG='merge-gate-lists'
DRIVER_PATH='scripts/ci/git-merge-gate-lists.sh'
DRIVER_DESC='mk/gate-lists.mk: merge gate/suite list entries as a set, else conflict markers'
DRIVER_INSTALL_NOTE='  Makefile gate-list conflicts now resolve entry by entry during merge/rebase;
  anything else in the file still gets ordinary conflict markers.'
DRIVER_SELF="${BASH_SOURCE[0]}"
DEFAULT_PATH='mk/gate-lists.mk'

# The lists this driver owns. Every one is a whitespace-separated, order-
# insensitive set that PRs append to, and every one is reconciled by
# gate-lists-check. A variable not named here is merged by git alone.
#
# This array and the assignments in mk/gate-lists.mk must name the same set.
# lift_vars below hard-fails on a name it cannot find, so a list renamed or
# dropped there without this array following refuses every merge of that file;
# a list added there without this array following is merged by git alone, which
# is the line-position conflict the driver exists to remove.
# git-merge-gate-lists-test.sh reconciles the two, in both directions.
MANAGED_VARS=(CHECK_FAST_GATES CHECK_HEAVY_GATES QUEUE_GATES DOCS_GATES SCRIPTS_TESTS)

# shellcheck source=scripts/lib/merge-driver-common.sh
. "$SCRIPT_DIR/../lib/merge-driver-common.sh"

# --managed-vars — print the array above, one name per line, so the suite
# reconciles the value the driver runs on rather than re-deriving it from this
# source. Handled before merge_driver_init, which expects the git placeholders.
if [[ "${1:-}" == "--managed-vars" ]]; then
	printf '%s\n' "${MANAGED_VARS[@]}"
	exit 0
fi

merge_driver_init "$@"

# lift_vars FILE OUT_PREFIX — write OUT_PREFIX.body with each managed variable's
# assignment replaced by a sentinel line, and OUT_PREFIX.entries.<VAR> with that
# variable's entries, one per line. Fails when a managed variable is missing or
# assigned more than once, either of which breaks the pairing below.
lift_vars() {
	local file="$1" out="$2"
	awk -v out="$out" -v vars="${MANAGED_VARS[*]}" '
		BEGIN {
			n = split(vars, v, " ")
			for (i = 1; i <= n; i++) managed[v[i]] = 1
		}
		{
			# An assignment opens when a managed name starts the line, and runs
			# while lines end in a backslash continuation.
			if (!incont) {
				name = $0
				sub(/[ \t]*[:+?]?=.*$/, "", name)
				if (name in managed && $0 ~ /^[A-Z_]+[ \t]*[:+?]?=/) {
					if (name in seen) {
						printf "%s is assigned more than once\n", name > "/dev/stderr"
						exit 1
					}
					seen[name] = 1
					cur = name
					printf "#__GATE_LIST_SLOT_%s__\n", name > (out ".body")
					bf = sprintf("%s.block.%s", out, name)
					printf "" > bf
					print $0 > bf
					body = $0
					sub(/^[A-Z_]+[ \t]*[:+?]?=/, "", body)
					ef = sprintf("%s.entries.%s", out, name)
					printf "" > ef
					emit(body, ef)
					incont = ($0 ~ /\\[ \t]*$/)
                                        if (!incont) close(ef)
					next
				}
				print $0 > (out ".body")
				next
			}
			# Continuation line of the assignment opened above.
			ef = sprintf("%s.entries.%s", out, cur)
			bf = sprintf("%s.block.%s", out, cur)
			print $0 > bf
			emit($0, ef)
			incont = ($0 ~ /\\[ \t]*$/)
			if (!incont) { close(ef); close(bf) }
			next
		}
		function emit(text, f,   k, parts, j) {
			sub(/\\[ \t]*$/, "", text)
			k = split(text, parts, /[ \t]+/)
			for (j = 1; j <= k; j++)
				if (parts[j] != "") print parts[j] > f
		}
		END {
			if (incont) {
				printf "%s ends in an unterminated continuation\n", cur > "/dev/stderr"
				exit 1
			}
			n = split(vars, v, " ")
			for (i = 1; i <= n; i++)
				if (!(v[i] in seen)) {
					printf "%s is not assigned in this file\n", v[i] > "/dev/stderr"
					exit 1
				}
		}
	' "$file"
}

# merge_entries VAR — three-way set merge of one variable, printed one entry per
# line. base order first for survivors, then ours-only, then theirs-only.
merge_entries() {
	local var="$1"
	awk '
		FNR == 1 { side++ }
		side == 1 { base[$0] = 1; border[++bn] = $0; next }
		side == 2 { ours[$0] = 1; oorder[++on] = $0; next }
		{ theirs[$0] = 1; torder[++tn] = $0 }
		END {
			for (i = 1; i <= bn; i++) {
				e = border[i]
				# Removed on either side wins over kept on the other: a suite
				# deleted deliberately must not come back.
				if ((e in ours) && (e in theirs)) print e
			}
			for (i = 1; i <= on; i++) {
				e = oorder[i]
				if (!(e in base) && !seen[e]++) print e
			}
			for (i = 1; i <= tn; i++) {
				e = torder[i]
				if (!(e in base) && !seen[e]++) print e
			}
		}
	' "$WORKDIR/base.entries.$var" "$WORKDIR/ours.entries.$var" "$WORKDIR/theirs.entries.$var"
}

# render_block VAR ENTRIES_FILE — rebuild the assignment the way ours wrote it:
# same operator, same indent on continuation lines, and the same rough line
# width, so the merged file does not churn the whole block into one shape.
render_block() {
	local var="$1" entries="$2"
	local op indent width
	op="$(awk -v v="$var" '$0 ~ "^"v"[ \t]*[:+?]?=" { match($0, /[:+?]?=/); print substr($0, RSTART, RLENGTH); exit }' "$OURS_FILE")"
	indent="$(awk -v v="$var" '
		found && /^[ \t]+/ { match($0, /^[ \t]+/); print substr($0, RSTART, RLENGTH); exit }
		$0 ~ "^"v"[ \t]*[:+?]?=" { found = 1; if ($0 !~ /\\[ \t]*$/) exit }
	' "$OURS_FILE")"
	width="$(awk -v v="$var" '$0 ~ "^"v"[ \t]*[:+?]?=" { print length($0) + 4; exit }' "$OURS_FILE")"
	[[ -n "$indent" ]] || indent="                 "
	[[ "$width" =~ ^[0-9]+$ ]] && ((width >= 60)) || width=100

	awk -v var="$var" -v op="${op:-:=}" -v indent="$indent" -v width="$width" '
		{ e[++n] = $0 }
		END {
			line = var " " op
			out = ""
			for (i = 1; i <= n; i++) {
				cand = line " " e[i]
				if (length(cand) > width && line != var " " op) {
					out = out line " \\\n"
					line = indent e[i]
				} else {
					line = cand
				}
			}
			printf "%s%s\n", out, line
		}
	' "$entries"
}

for side_pair in "base:$BASE_FILE" "ours:$OURS_FILE" "theirs:$THEIRS_FILE"; do
	side="${side_pair%%:*}"
	if ! lift_vars "${side_pair#*:}" "$WORKDIR/$side" 2>"$WORKDIR/lift.err"; then
		merge_driver_fallback \
			"$side: $(merge_driver_first_line "$WORKDIR/lift.err" 'the gate lists could not be located')"
	fi
done

# The Makefile minus the managed assignments, merged by git alone. A conflict
# here is an ordinary Makefile conflict and is none of this driver's business.
if ! git merge-file -p --marker-size="$MARKER_SIZE" \
	-L "$LABEL_OURS" -L "$LABEL_BASE" -L "$LABEL_THEIRS" \
	"$WORKDIR/ours.body" "$WORKDIR/base.body" "$WORKDIR/theirs.body" \
	>"$WORKDIR/merged.body" 2>/dev/null; then
	merge_driver_fallback 'the Makefile conflicts outside the gate lists'
fi

# append_entries VAR ADDS_FILE — ours' block verbatim with ADDS appended as new
# continuation lines. Keeping ours' existing lines untouched is what holds the
# merge diff down to the entries that actually arrived; a full re-render churns
# every wrapped line of a 30-line list and buries the real change.
append_entries() {
	local var="$1" adds="$2"
	local indent width
	indent="$(awk 'NR > 1 { match($0, /^[ \t]+/); if (RSTART) { print substr($0, RSTART, RLENGTH); exit } }' "$WORKDIR/ours.block.$var")"
	[[ -n "$indent" ]] || indent="                 "
	width="$(awk 'NR == 1 { print length($0) + 4; exit }' "$WORKDIR/ours.block.$var")"
	[[ "$width" =~ ^[0-9]+$ ]] && ((width >= 60)) || width=100

	awk -v indent="$indent" -v width="$width" -v addsfile="$adds" '
		{ line[++n] = $0 }
		END {
			for (i = 1; i < n; i++) print line[i]
			last = line[n]
			while ((getline a < addsfile) > 0) adds[++m] = a
			close(addsfile)
			if (m == 0) { print last; exit }
			# The old last line now continues.
			sub(/[ \t]*$/, "", last)
			print last " \\"
			cur = indent
			out = ""
			for (i = 1; i <= m; i++) {
				cand = (cur == indent) ? cur adds[i] : cur " " adds[i]
				if (length(cand) > width && cur != indent) {
					out = out cur " \\\n"
					cur = indent adds[i]
				} else {
					cur = cand
				}
			}
			printf "%s%s\n", out, cur
		}
	' "$WORKDIR/ours.block.$var"
}

for var in "${MANAGED_VARS[@]}"; do
	merge_entries "$var" >"$WORKDIR/merged.entries.$var"
	comm -13 <(sort "$WORKDIR/ours.entries.$var") <(sort "$WORKDIR/merged.entries.$var") >"$WORKDIR/adds.$var"
	comm -23 <(sort "$WORKDIR/ours.entries.$var") <(sort "$WORKDIR/merged.entries.$var") >"$WORKDIR/dels.$var"

	if [[ ! -s "$WORKDIR/adds.$var" && ! -s "$WORKDIR/dels.$var" ]]; then
		# Nothing changed for this list. Reuse ours byte for byte, so an
		# untouched variable contributes no diff at all.
		cp "$WORKDIR/ours.block.$var" "$WORKDIR/block.$var"
	elif [[ ! -s "$WORKDIR/dels.$var" ]]; then
		append_entries "$var" "$WORKDIR/adds.$var" >"$WORKDIR/block.$var"
	else
		# A removal has to rewrite the wrapped lines, so this is the one case
		# that re-renders the whole block.
		render_block "$var" "$WORKDIR/merged.entries.$var" >"$WORKDIR/block.$var"
	fi

	# The render is the one step that could silently corrupt a list, so read it
	# back and require the entry set it actually produces to equal the set that
	# went in. Order is not compared; membership is the invariant.
	if ! lift_vars "$WORKDIR/block.$var" "$WORKDIR/check.$var" 2>/dev/null; then
		# lift_vars needs every managed var present, so a single-variable block
		# only round-trips through the entry extraction below.
		awk -v v="$var" '
			BEGIN { incont = 0 }
			{
				text = $0
				if (!incont) sub("^" v "[ \t]*[:+?]?=", "", text)
				incont = (text ~ /\\[ \t]*$/)
				sub(/\\[ \t]*$/, "", text)
				k = split(text, parts, /[ \t]+/)
				for (j = 1; j <= k; j++) if (parts[j] != "") print parts[j]
			}
		' "$WORKDIR/block.$var" >"$WORKDIR/roundtrip.$var"
	else
		cp "$WORKDIR/check.$var.entries.$var" "$WORKDIR/roundtrip.$var"
	fi
	if ! diff -q <(sort "$WORKDIR/merged.entries.$var") <(sort "$WORKDIR/roundtrip.$var") >/dev/null; then
		merge_driver_fallback "the rebuilt $var did not round-trip to the entries it was given"
	fi
done

# Substitute each rendered block back for its sentinel.
cp "$WORKDIR/merged.body" "$WORKDIR/result"
for var in "${MANAGED_VARS[@]}"; do
	if ! grep -q "^#__GATE_LIST_SLOT_${var}__\$" "$WORKDIR/result"; then
		merge_driver_fallback "the $var placeholder did not survive the merge"
	fi
	awk -v slot="#__GATE_LIST_SLOT_${var}__" -v blockfile="$WORKDIR/block.$var" '
		$0 == slot {
			while ((getline l < blockfile) > 0) print l
			close(blockfile)
			next
		}
		{ print }
	' "$WORKDIR/result" >"$WORKDIR/result.next"
	mv "$WORKDIR/result.next" "$WORKDIR/result"
done

cp "$WORKDIR/result" "$OURS_FILE"
merge_driver_note "resolved $TARGET_PATH gate lists entry by entry; review the list contents before committing"
exit 0
