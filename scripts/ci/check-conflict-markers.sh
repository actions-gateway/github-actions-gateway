#!/usr/bin/env bash
#
# Fail when any present file contains a leftover merge-conflict marker line
# (the seven-char <<<<<<< / ======= / >>>>>>> forms, or diff3's |||||||).
# An edit-based conflict resolution can miss a marker sitting just outside
# the text it replaced, and format-aware linters skip lines they don't
# parse — exactly that combination let a stray marker merge to main via
# PR #724 (Q379; fixed same day in PR #730). Backs `make
# conflict-markers-check` and the `conflict-markers` CI workflow.
#
# Usage: check-conflict-markers.sh [file...]
#   With no args, scans every present file — tracked, or untracked and not
#   gitignored, so a brand-new file's markers are caught by its own first `make
#   check` (Q619) — except the vendored trees (vendor/, tools/vendor/ —
#   third-party files may legitimately contain marker-shaped fixture lines).
#   Explicit file args override the file set (used by
#   check-conflict-markers-test.sh).
set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which the test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Assembled from character classes so this script's own source never trips the
# scan. A marker is exactly seven repeated chars at line start: <<<<<<< and
# >>>>>>> carry a " label" (or end the line), ======= stands alone — an eighth
# "=" is a Markdown setext underline and stays legal — and ||||||| is diff3's
# common-ancestor divider.
readonly MARKER_RE='^([<]{7}|[>]{7}|[|]{7})( |$)|^[=]{7}$'

matches=""
if (( $# > 0 )); then
	matches="$(grep -HInE "$MARKER_RE" -- "$@" || true)"
else
	# The candidate list goes through select_present_files before xargs: a
	# deleted-but-tracked path is still listed by `--cached`, and grep failing to
	# open it would be swallowed by the trailing `|| true` — a partial scan
	# reported as a clean one. NUL-delimited into xargs, since the tracked tree
	# is far past ARG_MAX.
	matches="$(git_candidates . ':(exclude)vendor' ':(exclude)tools/vendor' |
		select_present_files | tr '\n' '\0' |
		xargs -0 grep -HInE "$MARKER_RE" -- || true)"
fi

if [[ -n "$matches" ]]; then
	echo "conflict-markers: leftover merge-conflict marker(s) found:"
	echo "$matches"
	echo "resolve the conflict fully (git diff --check flags these per-file), then retry"
	exit 1
fi
echo "conflict-markers: ok (no leftover merge markers in the scanned files)"
