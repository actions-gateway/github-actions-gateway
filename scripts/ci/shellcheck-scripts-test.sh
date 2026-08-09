#!/usr/bin/env bash
#
# Unit tests for the file-selection helpers scripts/ci/shellcheck-scripts.sh
# runs on (Q432): select_present_files (existence filter, de-dupe, order) and
# the script_candidates + select_present_files pair end-to-end against a
# throwaway git repo. The regression they lock down is coverage, not style — the
# gate used to list `--cached` only, so a newly written script was skipped and
# its first `make check` was a false green.
#
# git_candidates and select_present_files live in scripts/lib/common.sh since
# Q619 swept the same query into the doc-link, conflict-marker and plan-ref
# gates, so these assertions now defend all four. Runs under `make check` (via
# `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its helpers; the BASH_SOURCE guard there
# keeps main() from running (and shellcheck from being required) on source.
# shellcheck source=scripts/ci/shellcheck-scripts.sh
source "$REPO_ROOT/scripts/ci/shellcheck-scripts.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/shellcheck-scripts-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# expect NAME WANT GOT — assert two newline-joined path lists match exactly.
expect() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-28s -> [%s]\n' "$name" "${got//$'\n'/ }"
	else
		printf 'FAIL %-28s want=[%s] got=[%s]\n' "$name" "${want//$'\n'/ }" "${got//$'\n'/ }" >&2
		fails=$((fails + 1))
	fi
}

# --- select_present_files: existence filter, de-dupe, order ------------------------

SELECT_DIR="$FIXTURE_DIR/select"
mkdir -p "$SELECT_DIR/sub"
: >"$SELECT_DIR/a.sh"
: >"$SELECT_DIR/sub/b.sh"
mkdir -p "$SELECT_DIR/adir.sh" # a directory, not a script

(
	cd "$SELECT_DIR"
	expect keeps-existing $'a.sh\nsub/b.sh' "$(printf 'a.sh\nsub/b.sh\n' | select_present_files)"
	# A deleted-but-tracked path is still listed by `git ls-files --cached`;
	# passing it to shellcheck would fail the gate on a file nobody can read.
	expect drops-missing 'a.sh' "$(printf 'a.sh\ngone.sh\n' | select_present_files)"
	# An unmerged path is listed once per merge stage.
	expect dedupes-merge-stages $'a.sh\nsub/b.sh' \
		"$(printf 'a.sh\na.sh\na.sh\nsub/b.sh\n' | select_present_files)"
	expect skips-blank-lines 'a.sh' "$(printf '\na.sh\n\n' | select_present_files)"
	expect drops-directories '' "$(printf 'adir.sh\n' | select_present_files)"
	expect empty-input '' "$(printf '' | select_present_files)"
	# Order is input order, so the shellcheck arg list stays reproducible.
	expect preserves-order $'sub/b.sh\na.sh' "$(printf 'sub/b.sh\na.sh\n' | select_present_files)"
)

# --- script_candidates + select_present_files against a real git repo --------------

# A throwaway repo covering every state the gate has to classify: tracked,
# tracked-in-a-subdir, untracked-and-not-ignored, untracked-but-gitignored,
# deleted-but-tracked, plus non-.sh and non-scripts/ paths that must not match.
GIT_DIR_FIXTURE="$FIXTURE_DIR/repo"
mkdir -p "$GIT_DIR_FIXTURE/scripts/lib"
(
	cd "$GIT_DIR_FIXTURE"
	git init -q -b main .
	printf 'scripts/scratch.sh\n' >.gitignore
	: >scripts/tracked.sh
	: >scripts/deleted.sh
	: >scripts/lib/nested.sh
	: >scripts/notes.txt
	: >toplevel.sh
	git add -A >/dev/null
	git -c user.name=test -c user.email=test@example.com commit -qm init --no-verify
	rm scripts/deleted.sh # tracked in HEAD, absent from the worktree
	: >scripts/untracked.sh
	: >scripts/scratch.sh # untracked AND gitignored
	: >scripts/lib/untracked-nested.sh
)

got="$( (cd "$GIT_DIR_FIXTURE" && script_candidates | select_present_files | LC_ALL=C sort) )"
want=$'scripts/lib/nested.sh\nscripts/lib/untracked-nested.sh\nscripts/tracked.sh\nscripts/untracked.sh'
expect git-repo-selection "$want" "$got"

# --- the real worktree: every tracked script stays covered ------------------

# Whatever else the new candidate query adds, it must not drop a path the old
# tracked-only query linted.
missing="$(
	comm -23 \
		<(git ls-files 'scripts/*.sh' | select_present_files | LC_ALL=C sort) \
		<(script_candidates | select_present_files | LC_ALL=C sort)
)"
expect real-tree-covers-tracked '' "$missing"

# A failing candidate query must surface, not degrade the gate to a silent "no
# scripts to shellcheck" pass — that is the same false-green shape as Q432
# itself. select_present_files drains all of stdin, so pipefail sees the real status
# instead of a SIGPIPE from an early return.
failing_candidates() {
	printf 'scripts/ci/shellcheck-scripts.sh\n'
	return 3
}
rc=0
(
	set -o pipefail
	failing_candidates | select_present_files
) >/dev/null || rc=$?
expect propagates-candidate-failure 3 "$rc"

if [[ -z "$(script_candidates | select_present_files)" ]]; then
	printf 'FAIL %-28s real worktree selected no scripts\n' real-tree-non-empty >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s -> %s script(s)\n' real-tree-non-empty \
		"$(script_candidates | select_present_files | wc -l | tr -d ' ')"
fi

if (( fails > 0 )); then
	echo "shellcheck-scripts-test: $fails failure(s)" >&2
	exit 1
fi
echo "shellcheck-scripts-test: all assertions passed"
