#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-conflict-markers.sh (Q379): the four marker
# forms are rejected, near-misses (setext underlines, mid-line mentions,
# six/eight-char runs) stay legal, the no-args file selection covers untracked
# files while honouring the gitignore opt-out (Q619), and the current tree
# passes. The marker strings are assembled at runtime so this file never
# contains one.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-conflict-markers.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/conflict-markers-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# Marker strings, assembled so they never appear literally in this file.
lt7="$(printf '<%.0s' {1..7})"
gt7="$(printf '>%.0s' {1..7})"
eq7="$(printf '=%.0s' {1..7})"
pipe7="$(printf '|%.0s' {1..7})"

# expect NAME EXPECT_RC CONTENT — write CONTENT to a fixture file, run the
# checker against it, and assert the exit code matches EXPECT_RC.
expect() {
	local name="$1" want_rc="$2" content="$3" fixture got_rc=0
	fixture="$FIXTURE_DIR/$name.txt"
	printf '%s\n' "$content" >"$fixture"
	"$CHECKER" "$fixture" >/dev/null 2>&1 || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-24s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-24s want rc=%s got rc=%s\n' "$name" "$want_rc" "$got_rc" >&2
		fails=$((fails + 1))
	fi
}

# The four marker forms are rejected.
expect ours-marker 1 "$lt7 HEAD"
expect theirs-marker 1 "$gt7 abc1234 (some subject)"
expect bare-ours-marker 1 "$lt7"
expect separator-marker 1 "$eq7"
expect diff3-marker 1 "$pipe7 merged common ancestors"
expect marker-mid-file 1 "clean line
$lt7 HEAD
more content"

# Near-misses stay legal.
expect setext-six 0 'Heading
======'
expect setext-eight 0 'Heading
========'
expect midline-mention 0 "docs may mention \`$lt7 HEAD\` inline"
expect clean-file 0 'nothing to see here'

# Multiple args: one dirty file fails the run and is named in the output.
dirty="$FIXTURE_DIR/dirty.txt" clean="$FIXTURE_DIR/clean.txt"
printf '%s\n' "$gt7 theirs" >"$dirty"
printf '%s\n' 'fine' >"$clean"
if out="$("$CHECKER" "$clean" "$dirty" 2>&1)"; then
	printf 'FAIL %-24s expected failure\n' names-dirty-file >&2
	fails=$((fails + 1))
elif grep -q "dirty.txt" <<<"$out"; then
	printf 'ok   %-24s dirty file named\n' names-dirty-file
else
	printf 'FAIL %-24s dirty file not named in output\n' names-dirty-file >&2
	fails=$((fails + 1))
fi

# --- file selection in the no-args default mode -----------------------------
#
# Reading the pathspec predicts coverage; planting the marker measures it. The
# gate listed `--cached` only until Q619, so a brand-new file's markers were
# invisible to its own first `make check` and only surfaced once the commit that
# tracked it had landed. A throwaway repo scopes the gate — it resolves its root
# with `git rev-parse --show-toplevel`.
SELECT_REPO="$FIXTURE_DIR/repo"
mkdir -p "$SELECT_REPO"
(
	cd "$SELECT_REPO"
	git init -q -b main .
	# Q820: no detached maintenance racing the next command in a fixture repo.
	git config maintenance.auto false
	printf 'ignored.txt\n' >.gitignore
	printf 'clean\n' >tracked.txt
	printf 'clean\n' >doomed.txt
	git add -A >/dev/null
	git -c user.name=test -c user.email=test@example.com commit -qm init --no-verify
)

# selection_case NAME EXPECT_RC [FILE CONTENT] — optionally plant CONTENT at
# FILE inside the fixture repo, run the checker there in its no-args mode, and
# assert the exit code. The planted file is removed afterwards so cases don't
# leak into each other.
selection_case() {
	local name="$1" want_rc="$2" file="${3:-}" content="${4:-}" got_rc=0
	if [[ -n "$file" ]]; then
		printf '%s\n' "$content" >"$SELECT_REPO/$file"
	fi
	( cd "$SELECT_REPO" && "$CHECKER" ) >/dev/null 2>&1 || got_rc=$?
	if [[ -n "$file" ]]; then
		rm -f "$SELECT_REPO/$file"
	fi
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-24s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-24s want rc=%s got rc=%s\n' "$name" "$want_rc" "$got_rc" >&2
		fails=$((fails + 1))
	fi
}

# Baseline, so a red below is the planted marker and not the fixture itself.
selection_case fixture-repo-clean 0
selection_case untracked-scanned 1 untracked.txt "$gt7 theirs"
# Gitignoring is the documented opt-out, and it must still work.
selection_case gitignored-skipped 0 ignored.txt "$gt7 theirs"
# A deleted-but-tracked path is still listed by `--cached`; grep failing to open
# it would be swallowed by the gate's `|| true` and report a partial scan clean.
# The scan must survive it and still see a marker in a file that IS present.
rm "$SELECT_REPO/doomed.txt"
selection_case deleted-tracked-ok 0
selection_case deleted-tracked-still-scans 1 untracked.txt "$lt7 HEAD"

# The current tree must itself be clean (the no-args default mode).
if "$CHECKER" >/dev/null; then
	printf 'ok   %-24s\n' real-tree-clean
else
	printf 'FAIL %-24s tree has markers or scan errored\n' real-tree-clean >&2
	fails=$((fails + 1))
fi

if (( fails > 0 )); then
	echo "check-conflict-markers-test: $fails failure(s)" >&2
	exit 1
fi
echo "check-conflict-markers-test: all assertions passed"
