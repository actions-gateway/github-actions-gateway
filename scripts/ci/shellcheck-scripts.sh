#!/usr/bin/env bash
#
# Shellcheck every shell script present under scripts/. The git pathspec
# 'scripts/*.sh' matches recursively (git's default '*' spans '/'), so it covers
# every scripts/<group>/*.sh — and any group added later — without re-touching
# the gate.
#
# The file set is `git ls-files --cached --others --exclude-standard`: tracked
# files PLUS untracked ones that are not gitignored. `--cached` alone made a
# brand-new script invisible to its own first `make check` — a false green until
# the commit that tracked it (Q432). So a scratch script you don't want linted
# has to be *gitignored*, not merely left untracked: write it under the
# gitignored tmp/ at the repo root, per the repo temp-file convention.
#
# Backs `make shellcheck`, mirrored by the `shellcheck` job in unit-test.yml —
# CI pins shellcheck v0.11.0; install that version locally so verdicts match
# (shellcheck's heuristics drift between releases).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# --- file selection (asserted by shellcheck-scripts-test.sh) -----------------

# script_candidates — emit the git-known candidate paths under scripts/, one per
# line: tracked (--cached) plus untracked-and-not-gitignored (--others
# --exclude-standard). core.quotePath=false keeps a non-ASCII path literal
# rather than C-quoted, so it survives to shellcheck instead of being silently
# dropped by the existence filter below. Impure (queries git); the filtering
# lives in select_scripts.
script_candidates() {
	git -c core.quotePath=false ls-files --cached --others --exclude-standard -- 'scripts/*.sh'
}

# select_scripts — read candidate paths on stdin, emit the ones shellcheck can
# actually open, in input order. Two classes have to go:
#   * a deleted-but-tracked path — `--cached` still lists a file removed from
#     the worktree, and shellcheck exits non-zero on a path it cannot read;
#   * duplicates — `--cached` lists an unmerged path once per merge stage, and
#     linting the same file three times triples its findings.
select_scripts() {
	local path seen=$'\n'
	while IFS= read -r path; do
		[[ -n "$path" ]] || continue
		[[ -f "$path" ]] || continue
		[[ "$seen" == *$'\n'"$path"$'\n'* ]] && continue
		seen+="$path"$'\n'
		printf '%s\n' "$path"
	done
}

main() {
	cd "$REPO_ROOT"
	require_cmd shellcheck "https://github.com/koalaman/shellcheck#installing"

	# Command substitution, not a `while read < <(...)` loop: it keeps the
	# selection under `set -o pipefail`, so a failing `git ls-files` aborts the
	# gate instead of quietly reducing it to "no scripts to shellcheck".
	local selected files=()
	selected="$(script_candidates | select_scripts)"
	if [[ -n "$selected" ]]; then
		mapfile -t files <<<"$selected"
	fi

	if (( ${#files[@]} == 0 )); then
		echo "no scripts to shellcheck"
		return 0
	fi
	echo "==> shellcheck ${#files[@]} script(s) under scripts/"
	shellcheck "${files[@]}"
}

# Run main only when executed directly, so shellcheck-scripts-test.sh can source
# this file to exercise the pure selection helper without linting anything.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
