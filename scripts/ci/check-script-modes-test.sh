#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-script-modes.sh (Q1013).
#
# Why it is tested: a mode gate is exposed to the same false green as any
# reconciliation gate — a selection that quietly returns nothing reports "ok"
# over an unchecked tree, and a one-way rule reports "ok" over the half it never
# looks at. So both directions get a fixture that must go red, the empty
# selection gets its own exit status, and the last case reconciles against this
# repo's real scripts/ tree, which is the only assertion that would notice the
# gate and the tree disagreeing after a refactor.
#
# The index-versus-filesystem split gets its own case each, because they are
# what makes the gate catch a mode that a clone would get: a local `chmod +x`
# that was never staged must still fail, and that is invisible to `test -x`.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECK="$REPO_ROOT/scripts/ci/check-script-modes.sh"

# A fixed path under the repo's gitignored tmp/, per the repo temp-file
# convention: the throwaway repos below must be invisible to the real tree's own
# file selection, which the final assertion exercises.
WORKDIR="$REPO_ROOT/tmp/check-script-modes-test"
rm -rf "$WORKDIR"
mkdir -p "$WORKDIR"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

commit() {
	git -C "$1" add -A
	git -C "$1" -c user.email=t@t -c user.name=t commit -qm fixture
}

# repo NAME — a throwaway repo with one correct entrypoint (755) and one
# correct library (644), and echo its path. Callers plant the defect on top.
repo() {
	local dir="$WORKDIR/$1"
	mkdir -p "$dir/scripts/dogfood/lib"
	git -C "$dir" init -q
	# Q820: no detached maintenance racing the next command in a fixture repo.
	git -C "$dir" config maintenance.auto false
	printf '#!/usr/bin/env bash\ntrue\n' >"$dir/scripts/dogfood/go.sh"
	chmod +x "$dir/scripts/dogfood/go.sh"
	printf '# sourced, not run\ntrue\n' >"$dir/scripts/dogfood/lib/pool.sh"
	commit "$dir"
	printf '%s\n' "$dir"
}

# expect NAME EXPECTED_RC CWD NEEDLE — run the gate from CWD and assert its exit
# code and that its output mentions NEEDLE. NEEDLE is positional rather than an
# environment prefix so it cannot leak into the next case.
expect() {
	local name="$1" want_rc="$2" cwd="$3" needle="$4"
	local out rc=0
	out="$(cd "$cwd" && "$CHECK" 2>&1)" || rc=$?
	if ((rc != want_rc)); then
		printf 'FAIL %-52s rc=%d, want %d\n' "$name" "$rc" "$want_rc"
		printf '%s\n' "$out" | awk '{ print "       " $0 }'
		fails=$((fails + 1))
		return
	fi
	if ! grep -qF -- "$needle" <<<"$out"; then
		printf 'FAIL %-52s missing %q in output\n' "$name" "$needle"
		printf '%s\n' "$out" | awk '{ print "       " $0 }'
		fails=$((fails + 1))
		return
	fi
	printf 'ok   %-52s rc=%d\n' "$name" "$rc"
}

echo "scripts/ci/check-script-modes-test.sh"

# --- a correct tree passes ---------------------------------------------------

clean="$(repo clean)"
expect "a correct tree passes" 0 "$clean" 'ok ('

# --- direction 1: an entrypoint that lost its bit ----------------------------
#
# The Q1013 shape: six of these were invoked bare by release.md and exited 126.

demoted="$(repo demoted)"
chmod -x "$demoted/scripts/dogfood/go.sh"
commit "$demoted"
expect "an entrypoint without the bit fails" 1 "$demoted" 'go.sh'
expect "and says it is run as a command" 1 "$demoted" 'run as a command'
expect "and names the fix" 1 "$demoted" 'update-index --chmod=+x'

# --- direction 2: a library that gained one ----------------------------------
#
# Checked because a one-way rule would report ok over this half forever, and a
# library reading as an entrypoint is how the convention erodes in the other
# direction.

promoted="$(repo promoted)"
chmod +x "$promoted/scripts/dogfood/lib/pool.sh"
commit "$promoted"
expect "a library with the bit fails" 1 "$promoted" 'lib/pool.sh'
expect "and says it is sourced" 1 "$promoted" 'sourced, not run'
expect "and names the fix" 1 "$promoted" 'update-index --chmod=-x'

# --- the index is what a clone gets, so it outranks the filesystem -----------
#
# An unstaged `chmod +x` leaves the working tree executable and the index 644,
# which is exactly the drift that ships. `test -x` alone would call this clean.

# Neither case commits: a commit would reconcile the index and the filesystem,
# which is the exact disagreement under test.

unstaged="$(repo unstaged)"
chmod -x "$unstaged/scripts/dogfood/go.sh"
expect "an unstaged chmod is judged by the index" 0 "$unstaged" 'ok ('

staged="$(repo staged)"
git -C "$staged" update-index --chmod=-x scripts/dogfood/go.sh
expect "a staged 644 fails though the file is executable" 1 "$staged" 'go.sh'

# --- an untracked script is checked by its own first run ---------------------
#
# Otherwise the gate does not see a new script until the commit that tracks it,
# by which point `make check` has already gone green on it (Q432/Q619).

untracked="$(repo untracked)"
printf '#!/usr/bin/env bash\ntrue\n' >"$untracked/scripts/dogfood/new.sh"
expect "an untracked non-executable script fails" 1 "$untracked" 'new.sh'

# --- a non-ASCII path is checked, not silently skipped -----------------------
#
# git C-quotes such a path unless core.quotePath=false, and a quoted name
# matches no file. Selected with a bare `git ls-files` the entry survives into
# the total while being dropped by the reader, so the gate reports it as covered
# and never checks its mode: a false green of exactly the class the
# empty-selection case below guards, arriving one file at a time instead of all
# at once. common.sh's git_candidates is what sets the flag.

unicode="$(repo unicode)"
printf '#!/usr/bin/env bash\ntrue\n' >"$unicode/scripts/dogfood/café.sh"
chmod -x "$unicode/scripts/dogfood/café.sh"
commit "$unicode"
expect "a non-ASCII entrypoint is caught, not skipped" 1 "$unicode" 'café.sh'

# --- an empty selection is a failure, not a pass -----------------------------
#
# The false green a reconciliation gate hides best: a selection that matches
# nothing walks nothing and finds nothing wrong with it.

empty="$WORKDIR/empty"
mkdir -p "$empty"
git -C "$empty" init -q
git -C "$empty" config maintenance.auto false
expect "an empty selection exits 2 rather than passing" 2 "$empty" 'checking nothing'

# --- and the real tree agrees with the rule ----------------------------------

expect "this repo's own scripts/ tree passes" 0 "$REPO_ROOT" 'ok ('

if ((fails > 0)); then
	echo "${fails} failure(s)" >&2
	exit 1
fi
echo "all check-script-modes.sh tests passed"
