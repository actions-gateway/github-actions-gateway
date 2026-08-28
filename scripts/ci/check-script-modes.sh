#!/usr/bin/env bash
#
# check-script-modes.sh — fail when a script under scripts/ carries the wrong
# executable bit for how it is used (Q1013).
#
# The rule is positional, because position is what the tree already encodes:
#
#   scripts/**/lib/*.sh   sourced by a caller, never run — must NOT be executable
#   scripts/**/*-test.sh  a suite `make scripts-test` runs by path — executable
#   every other .sh       run as a command — must be executable
#
# Nothing held the tree to that, and six entrypoints had drifted to 644 by the
# time this gate was written: e2e-setup.sh, e2e-start.sh, e2e-stop.sh, start.sh,
# stop.sh and runner-build.sh, every one of them invoked bare by
# docs/operations/release.md. The bare form exits 126 with `Permission denied`
# before the script reads a single argument, so the runbook's own bring-up and
# teardown steps did not execute as written.
#
# The lib/ carve-out is not new here: check-errexit-prologue.sh already exempts
# `*/lib/*` on the same reasoning, that a sourced file runs under the caller's
# shell rather than its own. This gate reuses that split rather than inventing a
# second way to say which files are entrypoints.
#
# A `*-test.sh` is the one place position alone gets it wrong, and it outranks
# the lib/ marker. The repo convention is that a suite sits beside its subject
# (scripts/README.md), so a library gets its tests in the same directory — and
# `SCRIPTS_TESTS` runs every suite by path, so that file must be executable
# however it is filed. Position still decides the library beside it; what
# changes is that the suffix is read first.
#
# Both directions are checked. A one-way rule would let a library drift to 755
# and read as an entrypoint, and the reverse direction costs nothing here: all
# ten files under a lib/ directory are already 644, so the carve-out needs no
# allowlist and a new exception has to argue for itself in review.
#
# Mode is read from the index for a tracked file, because the index is what a
# clone gets and a local chmod that was never staged is exactly the drift this
# catches. An untracked-but-not-gitignored script is read from the filesystem
# instead, so a new script is checked by its own first `make check` rather than
# from the commit that tracks it — the same selection the shellcheck and
# script-docs gates use (Q432/Q619).
#
# Usage:
#   check-script-modes.sh [script.sh ...]
#
# With no scripts named, every present scripts/**/*.sh is checked. Exits 1 on
# any finding, and 2 when the selection returned nothing at all, which would
# otherwise let the gate pass by checking nothing.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

# git_candidates is cwd-sensitive by contract, and its pathspec is root-relative.
cd "$(git rev-parse --show-toplevel)"

# wants_executable PATH — 0 when PATH should carry the executable bit.
# A `lib/` path component is the source-only marker; anything else is a command.
# A `*-test.sh` is checked first, because `make scripts-test` runs one by path
# wherever it is filed, a library's own suite included.
wants_executable() {
	[[ "$1" == *-test.sh || "$1" != */lib/* ]]
}

# indexed_mode PATH — the file's mode in the git index, empty when untracked.
# `git ls-files -s` prints `<mode> <sha> <stage>\t<path>`, and git records only
# 100644 and 100755 for a regular file.
indexed_mode() {
	git ls-files -s -- "$1" | awk 'NR == 1 { print $1 }'
}

# is_executable PATH — 0 when the file is executable as a clone would get it:
# the index mode when tracked, the filesystem bit when not.
is_executable() {
	local path="$1" mode
	mode="$(indexed_mode "$path")"
	if [[ -n "$mode" ]]; then
		[[ "$mode" == 100755 ]]
	else
		[[ -x "$path" ]]
	fi
}

# select_scripts — every present scripts/**/*.sh, tracked or
# untracked-and-not-gitignored, one per line.
#
# Both halves come from common.sh rather than a local `git ls-files`, because
# each drops a file this gate would otherwise miscount. git_candidates sets
# core.quotePath=false, without which git C-quotes a non-ASCII path
# (`"scripts/ci/caf\303\251.sh"`); that name matches no file, so the entry is
# dropped by the reader while still counting toward the total this gate reports
# — the exact false green the empty-selection guard below exists to prevent,
# one file at a time. select_present_files then drops deleted-but-tracked paths
# and the duplicate stages `--cached` emits for an unmerged path, which would
# otherwise mode-check the same file three times.
select_scripts() {
	git_candidates 'scripts/*.sh' | select_present_files
}

main() {
	local -a scripts=()
	if (($# > 0)); then
		scripts=("$@")
	else
		mapfile -t scripts < <(select_scripts)
	fi

	if ((${#scripts[@]} == 0)); then
		echo "check-script-modes: selected no scripts — the gate would pass by checking nothing" >&2
		exit 2
	fi

	local path findings=0
	for path in "${scripts[@]}"; do
		if wants_executable "$path"; then
			if ! is_executable "$path"; then
				echo "check-script-modes: $path is run as a command but is not executable" >&2
				echo "  fix: git update-index --chmod=+x $path" >&2
				findings=$((findings + 1))
			fi
		else
			if is_executable "$path"; then
				echo "check-script-modes: $path is sourced, not run, but is executable" >&2
				echo "  fix: git update-index --chmod=-x $path" >&2
				findings=$((findings + 1))
			fi
		fi
	done

	if ((findings > 0)); then
		echo "check-script-modes: FAILED — ${findings} script(s) with the wrong mode" >&2
		exit 1
	fi
	echo "check-script-modes: ok (${#scripts[@]} scripts)"
}

[[ -n "${CHECK_SCRIPT_MODES_LIB_ONLY:-}" ]] || main "$@"
