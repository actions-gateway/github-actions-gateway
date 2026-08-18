#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-errexit-prologue.sh (Q733). Both directions
# are asserted: a script missing `shopt -s inherit_errexit` after its
# `set -euo pipefail` must fail the gate, and every legal shape must pass — a
# rule that stops matching fails as silently as one that matches everything,
# and this gate exists precisely because a silent pass is the bug class.
#
# The no-args selection is exercised in a throwaway repo, including the case the
# gate guards explicitly: a selection that returns nothing must fail rather than
# report success over zero files.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# File-wide: every fixture body is the source text of another script, so a
# `$(...)` in one must reach the fixture file unexpanded — single quotes are the
# point, not an oversight.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-errexit-prologue.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/errexit-prologue-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# expect NAME EXPECT_RC CONTENT [SUBDIR] [BASENAME] — write CONTENT to a fixture
# script, run the checker against it, and assert the exit code. BASENAME
# overrides the file name, which the hook exemption keys on.
expect() {
	local name="$1" want_rc="$2" content="$3" subdir="${4:-}" base="${5:-$1.sh}"
	local dir fixture got_rc=0
	dir="$FIXTURE_DIR${subdir:+/$subdir}"
	mkdir -p "$dir"
	fixture="$dir/$base"
	printf '%s\n' "$content" >"$fixture"
	"$CHECKER" "$fixture" >/dev/null 2>&1 || got_rc=$?
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-28s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-28s want rc=%s got rc=%s\n' "$name" "$want_rc" "$got_rc" >&2
		fails=$((fails + 1))
	fi
}

# The complete prologue passes.
expect full-prologue 0 '#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit

x="$(echo hi)"'

# The prologue without the shopt is the defect this gate exists for.
expect shopt-missing 1 '#!/usr/bin/env bash
set -euo pipefail

x="$(echo hi)"'

# Present but not adjacent: a later shopt leaves every substitution above it
# uncovered, which is the same hole in a shape that greps clean.
expect shopt-not-adjacent 1 '#!/usr/bin/env bash
set -euo pipefail

x="$(echo hi)"
shopt -s inherit_errexit'

# A script with no prologue at all.
expect no-prologue 1 '#!/usr/bin/env bash

x="$(echo hi)"'

# The shopt is required regardless of whether the script has a substitution
# today: the next edit adds one, and the gate must not have to re-classify.
expect no-substitution-still-required 1 '#!/usr/bin/env bash
set -euo pipefail

echo hi'

# A comment may sit between the two lines; neither a comment nor a blank can
# hold a substitution, so the protection still starts before any code runs.
expect comment-between 0 '#!/usr/bin/env bash
set -euo pipefail
# why this is here
shopt -s inherit_errexit

x="$(echo hi)"'

# ...but a line that executes must not.
expect code-between 1 '#!/usr/bin/env bash
set -euo pipefail
x="$(echo hi)"
shopt -s inherit_errexit'

# inherit_errexit is bash 4.4+, and stock macOS ships 3.2, where the shopt
# itself fails and set -e turns that into a non-zero exit. A Claude Code hook
# must never block a tool call, so it alone may swallow that failure.
expect failopen-hook-allowed 0 '#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit 2>/dev/null || true

x="$(echo hi)"' '' claude-go-throttle-hook.sh
# Anywhere else the same form is a silent opt-out of the protection.
expect failopen-elsewhere-rejected 1 '#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit 2>/dev/null || true

x="$(echo hi)"'

# Sourced libraries are exempt by path: they run under the caller''s shell
# options, and declaring the shopt there would switch it on for every caller.
expect sourced-lib 0 'helper() { echo hi; }' lib
# The exemption is by path, not a blanket pass for prologue-less files.
expect no-prologue-outside-lib 1 'helper() { echo hi; }' notlib
# A lib/ file that does carry a prologue is still held to the full rule.
expect lib-with-prologue 1 '#!/usr/bin/env bash
set -euo pipefail

x="$(echo hi)"' lib

# Later `set -euo pipefail` lines belong to heredoc-generated stub scripts; the
# script''s own prologue is the first one, and only it is checked.
expect heredoc-stub-ignored 0 '#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit

cat > stub <<STUB
#!/usr/bin/env bash
set -euo pipefail
echo stub
STUB'

# --- no-args selection ------------------------------------------------------
#
# Reading the pathspec predicts coverage; planting a script measures it.
SELECT_REPO="$FIXTURE_DIR/repo"
mkdir -p "$SELECT_REPO/scripts/ci"
(
	cd "$SELECT_REPO"
	git init -q -b main .
	# Q820: no detached maintenance racing the next command in a fixture repo.
	git config maintenance.auto false
	printf 'scripts/ignored.sh\n' >.gitignore
	printf '#!/usr/bin/env bash\nset -euo pipefail\nshopt -s inherit_errexit\n' >scripts/ci/clean.sh
	git add -A >/dev/null
	git -c user.name=test -c user.email=test@example.com commit -qm init --no-verify
)

# selection_case NAME EXPECT_RC [FILE CONTENT] — optionally plant FILE in the
# fixture repo, run the checker there in no-args mode, assert the exit code.
selection_case() {
	local name="$1" want_rc="$2" file="${3:-}" content="${4:-}" got_rc=0
	if [[ -n "$file" ]]; then
		printf '%s\n' "$content" >"$SELECT_REPO/$file"
	fi
	( cd "$SELECT_REPO" && "$CHECKER" ) >/dev/null 2>&1 || got_rc=$?
	if [[ -n "$file" ]]; then
		rm -f "$SELECT_REPO/$file"
	fi
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-28s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-28s want rc=%s got rc=%s\n' "$name" "$want_rc" "$got_rc" >&2
		fails=$((fails + 1))
	fi
}

# Baseline, so a red below is the planted script and not the fixture itself.
selection_case fixture-repo-clean 0
selection_case untracked-scanned 1 scripts/ci/bad.sh '#!/usr/bin/env bash
set -euo pipefail
echo hi'
# Gitignoring is the documented opt-out for scratch scripts, and must still work.
selection_case gitignored-skipped 0 scripts/ignored.sh '#!/usr/bin/env bash
set -euo pipefail
echo hi'

# An empty selection must fail, not report success over zero files — the exact
# shape of false green this gate exists to prevent.
EMPTY_REPO="$FIXTURE_DIR/empty"
mkdir -p "$EMPTY_REPO"
(
	cd "$EMPTY_REPO"
	git init -q -b main .
	git config maintenance.auto false
	printf 'placeholder\n' >README.md
	git add -A >/dev/null
	git -c user.name=test -c user.email=test@example.com commit -qm init --no-verify
)
empty_rc=0
( cd "$EMPTY_REPO" && "$CHECKER" ) >/dev/null 2>&1 || empty_rc=$?
if (( empty_rc != 0 )); then
	printf 'ok   %-28s rc=%s\n' empty-selection-fails "$empty_rc"
else
	printf 'FAIL %-28s empty selection reported success\n' empty-selection-fails >&2
	fails=$((fails + 1))
fi

# The real tree must pass in the no-args default mode.
if "$CHECKER" >/dev/null; then
	printf 'ok   %-28s\n' real-tree-clean
else
	printf 'FAIL %-28s tree is missing the prologue or the scan errored\n' real-tree-clean >&2
	fails=$((fails + 1))
fi

if (( fails > 0 )); then
	echo "check-errexit-prologue-test: $fails failure(s)" >&2
	exit 1
fi
echo "check-errexit-prologue-test: all assertions passed"
