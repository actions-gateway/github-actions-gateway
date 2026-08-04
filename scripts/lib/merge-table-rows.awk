# merge-table-rows.awk — three-way merge of one Markdown table's data rows by
# key set-semantics. Driven by scripts/docs/git-merge-status.sh (docs/STATUS.md's
# Queue table) and scripts/docs/git-merge-plan-index.sh (docs/plan/README.md's
# index tables); see those scripts' headers for the why, and
# docs/development/queue-id-allocation.md for the conflict classes this exists
# to absorb.
#
#   awk -v key_mode=anchor -f merge-table-rows.awk BASE.rows OURS.rows THEIRS.rows
#
# Each input holds only one table's data rows, already split out by the caller.
#
# key_mode selects how a row's stable key is read from its first cell, which is
# the only file-specific knowledge here:
#
#   anchor  the `<a id="QN"></a>QN` backlog anchor (default)
#   link    a Markdown link's target, e.g. `[foo.md](foo.md)` -> `foo.md`
#
# Either way the key comes from cell 1 alone, so an escaped `\|` in a later cell
# cannot shift it.
#
# Exit 0: the merged row block is on stdout and the result is certain.
# Exit 2: the merge is NOT certain; a one-line reason is on stderr and the
#         caller must fall back to ordinary conflict markers. Silence beats a
#         guess here — a wrongly resolved row loses backlog state, whereas a
#         conflict marker costs a minute.
#
# The rules, per key:
#   * deleted on either side (and unchanged on the other) -> deleted
#   * added on either side                                -> present
#   * changed on one side only                            -> that change
#   * changed identically on both sides                   -> that change
#   * changed differently on both sides                   -> uncertain
#   * deleted on one side, changed on the other           -> uncertain
#   * same new ID added on both sides with different text -> uncertain
#
# Row *order* is meaningful in these files, so it is reconstructed rather than
# assumed: the rows both sides kept form a skeleton, and each side's additions
# are spliced back in at the position that side put them. When the two sides
# order the shared rows differently, the side that still agrees with the base
# did not reorder, so the other side's order is the intended one. When both
# reordered, that is uncertain.

function fail(msg) {
	printf "merge-table-rows: %s\n", msg > "/dev/stderr"
}

function side_name(s) {
	return (s == 0) ? "base" : ((s == 1) ? "ours" : "theirs")
}

# row_id LINE — the key this row belongs to, or "" when the line is not a
# well-formed row. Dispatches on key_mode; the empty return is what makes an
# unparseable row a fallback rather than a guess.
function row_id(line) {
	return (key_mode == "link") ? row_key_link(line) : row_key_anchor(line)
}

# row_key_anchor LINE — the backlog ID. Mirrors scripts/docs/lint-backlog.sh's
# parse_id: the ID cell is field 2 of the pipe-split row, and its
# `<a id="QN"></a>` anchor must match the visible ID, because every
# cross-reference in the file resolves through the anchor.
function row_key_anchor(line,    n, f, cell, anchor, visible) {
	if (line !~ /^\|/) return ""
	n = split(line, f, "|")
	if (n < 3) return ""
	cell = f[2]
	if (!match(cell, "<a id=\"Q[0-9]+\"></a>")) return ""
	anchor = substr(cell, RSTART, RLENGTH)
	gsub(/[^0-9]/, "", anchor)
	visible = cell
	gsub(/<[^>]*>/, "", visible)
	gsub(/^[ \t]+/, "", visible)
	gsub(/[ \t]+$/, "", visible)
	if (visible != "Q" anchor) return ""
	return visible
}

# row_key_link LINE — the first cell's Markdown link target, which is what keys
# docs/plan/README.md: one row per plan file, and the path is the identity the
# rest of the tooling already uses (scripts/docs/check-plan-index.sh reads the
# same cell). A row whose first cell is not a link has no key.
function row_key_link(line,    n, f, cell, target) {
	if (line !~ /^\|/) return ""
	n = split(line, f, "|")
	if (n < 3) return ""
	cell = f[2]
	if (!match(cell, /\[[^]]*\]\([^)]+\)/)) return ""
	target = substr(cell, RSTART, RLENGTH)
	sub(/^\[[^]]*\]\(/, "", target)
	sub(/\)$/, "", target)
	gsub(/^[ \t]+/, "", target)
	gsub(/[ \t]+$/, "", target)
	return target
}

# seq_equal A NA B NB — 1 when the two 1-indexed ID sequences are identical.
function seq_equal(a, na, b, nb,    i) {
	if (na != nb) return 0
	for (i = 1; i <= na; i++)
		if (a[i] != b[i]) return 0
	return 1
}

# push ID — append ID's resolved row to the output, once. Ignores IDs that did
# not survive the set merge, so callers can sweep a whole side's sequence.
function push(id) {
	if (!(id in keep)) return
	if (id in emitted) return
	emitted[id] = 1
	out[++n_out] = keep[id]
}

BEGIN {
	if (ARGC != 4) {
		fail("usage: awk -f merge-table-rows.awk BASE OURS THEIRS")
		exit 2
	}

	# --- read the three row blocks -------------------------------------------
	# Read explicitly rather than through awk's main loop: awk skips a
	# zero-length file entirely, so an empty table on one side would shift every
	# later file's side index.
	for (s = 0; s <= 2; s++) {
		file = ARGV[s + 1]
		rc = 0
		while ((rc = (getline line < file)) > 0) {
			if (line ~ /^[ \t]*$/) continue
			id = row_id(line)
			if (id == "") {
				fail(sprintf("%s: not a well-formed table row: %.60s", side_name(s), line))
				exit 2
			}
			if ((s SUBSEP id) in text) {
				fail(sprintf("%s: %s appears twice in the table", side_name(s), id))
				exit 2
			}
			text[s, id] = line
			seq[s, ++count[s]] = id
			pos[s, id] = count[s]
			all[id] = 1
		}
		close(file)
		if (rc < 0) {
			fail(sprintf("cannot read %s rows (%s)", side_name(s), file))
			exit 2
		}
	}

	# --- which rows survive, and with what text ------------------------------
	for (id in all) {
		hb = ((0 SUBSEP id) in text)
		ho = ((1 SUBSEP id) in text)
		ht = ((2 SUBSEP id) in text)
		if (hb) {
			if (ho && ht) {
				if (text[1, id] == text[2, id])
					keep[id] = text[1, id]
				else if (text[1, id] == text[0, id])
					keep[id] = text[2, id]
				else if (text[2, id] == text[0, id])
					keep[id] = text[1, id]
				else {
					fail(id " was changed differently on both sides")
					exit 2
				}
			} else if (ho) {
				# theirs deleted it. Only an untouched row may go quietly: a row
				# deleted on one side and edited on the other is the classic
				# delete/modify, and which intent wins is not ours to pick.
				if (text[1, id] != text[0, id]) {
					fail(id " was deleted on one side and changed on the other")
					exit 2
				}
			} else if (ht) {
				if (text[2, id] != text[0, id]) {
					fail(id " was deleted on one side and changed on the other")
					exit 2
				}
			}
			# else: both deleted it — the agreed-on outcome.
		} else {
			if (ho && ht) {
				if (text[1, id] == text[2, id])
					keep[id] = text[1, id]
				else {
					fail(id " was filed on both sides with different content")
					exit 2
				}
			} else if (ho)
				keep[id] = text[1, id]
			else
				keep[id] = text[2, id]
		}
	}

	# --- order: the rows both sides kept form the skeleton -------------------
	ns = 0
	for (n = 1; n <= count[1]; n++) {
		id = seq[1, n]
		if ((id in keep) && ((2 SUBSEP id) in text)) shared_ours[++ns] = id
	}
	nt = 0
	for (n = 1; n <= count[2]; n++) {
		id = seq[2, n]
		if ((id in keep) && ((1 SUBSEP id) in text)) shared_theirs[++nt] = id
	}

	nsk = 0
	if (seq_equal(shared_ours, ns, shared_theirs, nt)) {
		# Both sides agree on the shared rows' order: nothing was reordered, or
		# both reordered the same way.
		for (n = 1; n <= ns; n++) sk[++nsk] = shared_ours[n]
	} else {
		# They disagree, so at least one side reordered. Compare each side's
		# order of the shared-and-in-base rows against the base's: the side that
		# still matches the base is the one that did not reorder.
		nbo = 0
		for (n = 1; n <= ns; n++)
			if ((0 SUBSEP shared_ours[n]) in text) base_in_ours[++nbo] = shared_ours[n]
		nbt = 0
		for (n = 1; n <= nt; n++)
			if ((0 SUBSEP shared_theirs[n]) in text) base_in_theirs[++nbt] = shared_theirs[n]
		nbb = 0
		for (n = 1; n <= count[0]; n++) {
			id = seq[0, n]
			if ((id in keep) && ((1 SUBSEP id) in text) && ((2 SUBSEP id) in text))
				base_order[++nbb] = id
		}
		ours_kept_base_order = seq_equal(base_in_ours, nbo, base_order, nbb)
		theirs_kept_base_order = seq_equal(base_in_theirs, nbt, base_order, nbb)
		if (ours_kept_base_order && !theirs_kept_base_order) {
			for (n = 1; n <= nt; n++) sk[++nsk] = shared_theirs[n]
		} else if (theirs_kept_base_order && !ours_kept_base_order) {
			for (n = 1; n <= ns; n++) sk[++nsk] = shared_ours[n]
		} else {
			fail("rows were reordered on both sides")
			exit 2
		}
	}

	# --- emit: skeleton in order, each side's additions at its own position ---
	# Only additions are spliced in ahead of a skeleton entry. Sweeping every
	# earlier row of a side would drag skeleton rows forward out of their agreed
	# order, since the two sides' positions for them need not line up.
	for (k = 1; k <= nsk; k++) in_skeleton[sk[k]] = 1
	oi = 1
	ti = 1
	n_out = 0
	for (k = 1; k <= nsk; k++) {
		while (oi < pos[1, sk[k]]) {
			id = seq[1, oi++]
			if (!(id in in_skeleton)) push(id)
		}
		while (ti < pos[2, sk[k]]) {
			id = seq[2, ti++]
			if (!(id in in_skeleton)) push(id)
		}
		push(sk[k])
		oi = pos[1, sk[k]] + 1
		ti = pos[2, sk[k]] + 1
	}
	# Additions past the last skeleton entry, in each side's own order — plus the
	# ones stepped over above, which happens when the skeleton follows one side's
	# order and the other side's positions are therefore not monotonic.
	for (n = 1; n <= count[1]; n++) {
		id = seq[1, n]
		if (!(id in in_skeleton)) push(id)
	}
	for (n = 1; n <= count[2]; n++) {
		id = seq[2, n]
		if (!(id in in_skeleton)) push(id)
	}
	# Completeness backstop. push() is idempotent, so this cannot duplicate a
	# row; it exists so that no surviving row can be dropped even if the ordering
	# pass above ever misses one — losing backlog state is the one outcome worse
	# than a conflict marker.
	for (n = 1; n <= count[1]; n++) push(seq[1, n])
	for (n = 1; n <= count[2]; n++) push(seq[2, n])

	# Defensive: every surviving row must appear exactly once. A mismatch would
	# mean silently dropping backlog state, which is the one outcome worse than
	# a conflict marker.
	n_keep = 0
	for (id in keep) n_keep++
	if (n_out != n_keep) {
		fail(sprintf("internal: emitted %d of %d surviving rows", n_out, n_keep))
		exit 2
	}

	for (n = 1; n <= n_out; n++) print out[n]
}
