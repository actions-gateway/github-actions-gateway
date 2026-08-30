#!/usr/bin/env bash
#
# Unit tests for scripts/lib/merge-keyed-records.awk — the set-semantics merge
# the three Markdown merge drivers share.
#
# Why key_mode is asserted here rather than left to the driver suites. Each
# driver passes its own mode and only ever exercises that one, so nothing they
# run can see the case this file's contract turns on: a fourth driver added
# without `-v key_mode=`. That used to fall through to the backlog anchor
# reader, dead since Q889 retired the Queue table, so the merge keyed every row
# to "" and refused with "not a well-formed record" — a message naming the rows
# rather than the missing argument. It is now refused up front (Q1043), and a
# default silently reappearing would be invisible to every other suite.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
AWK_SCRIPT="${REPO_ROOT}/scripts/lib/merge-keyed-records.awk"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

fails=0

check() {
	local name="$1" want="$2" got="$3"
	if [[ "${want}" == "${got}" ]]; then
		echo "ok   ${name}"
	else
		echo "FAIL ${name}: want '${want}', got '${got}'" >&2
		fails=$((fails + 1))
	fi
}

# merge MODE — run the awk over the three side files with MODE passed verbatim,
# capturing stdout, stderr and the exit status. An empty MODE stands for the
# caller that forgot the argument altogether.
merge() {
	local mode="$1" rc=0
	local -a args=()
	if [[ -n "${mode}" ]]; then args=(-v "key_mode=${mode}"); fi
	awk "${args[@]}" -f "${AWK_SCRIPT}" \
		"${WORKDIR}/base" "${WORKDIR}/ours" "${WORKDIR}/theirs" \
		>"${WORKDIR}/out" 2>"${WORKDIR}/err" || rc=$?
	echo "${rc}"
}

# --- key_mode is required ----------------------------------------------------

# Bullets, not table rows: a record only one mode can read is refused either
# way, so it cannot tell the mode check from a fall-through to whichever reader
# comes last. These parse cleanly under `marker`, so a merge that succeeds here
# is one that picked a mode the caller never asked for.
cat >"${WORKDIR}/base" <<'ROWS'
- a bullet <!-- q:Q1 -->
ROWS
cp "${WORKDIR}/base" "${WORKDIR}/ours"
cp "${WORKDIR}/base" "${WORKDIR}/theirs"

check 'an unset key_mode is refused' '2' "$(merge '')"
check 'and the reason names key_mode' \
	'merge-keyed-records: key_mode must be link or marker' \
	"$(cat "${WORKDIR}/err")"
check 'a retired mode is refused, not silently accepted' '2' "$(merge anchor)"
check 'a misspelled mode is refused' '2' "$(merge links)"
check 'the mode check leaves nothing on stdout' '' "$(cat "${WORKDIR}/out")"

# And the same for records only `link` can read, so neither reader can stand in
# for the missing argument.
cat >"${WORKDIR}/base" <<'ROWS'
| [a.md](a.md) | first |
ROWS
cp "${WORKDIR}/base" "${WORKDIR}/ours"
cp "${WORKDIR}/base" "${WORKDIR}/theirs"
check 'an unset key_mode is refused for table rows too' '2' "$(merge '')"

# --- the live modes still merge ----------------------------------------------

cat >"${WORKDIR}/ours" <<'ROWS'
| [a.md](a.md) | first |
| [b.md](b.md) | ours added |
ROWS
cat >"${WORKDIR}/theirs" <<'ROWS'
| [a.md](a.md) | first |
| [c.md](c.md) | theirs added |
ROWS
check 'link mode merges both additions' '0' "$(merge link)"
check 'link mode keeps every row' \
	'| [a.md](a.md) | first |
| [b.md](b.md) | ours added |
| [c.md](c.md) | theirs added |' \
	"$(cat "${WORKDIR}/out")"

cat >"${WORKDIR}/base" <<'ROWS'
- a bullet <!-- q:Q1 -->
ROWS
cat >"${WORKDIR}/ours" <<'ROWS'
- a bullet <!-- q:Q1 -->
- another <!-- q:Q2 -->
ROWS
cp "${WORKDIR}/base" "${WORKDIR}/theirs"
check 'marker mode merges an addition' '0' "$(merge marker)"
check 'marker mode keeps both bullets' \
	'- a bullet <!-- q:Q1 -->
- another <!-- q:Q2 -->' \
	"$(cat "${WORKDIR}/out")"

if ((fails)); then
	echo "${fails} assertion(s) failed" >&2
	exit 1
fi
echo
echo "all merge-keyed-records.awk assertions passed"
