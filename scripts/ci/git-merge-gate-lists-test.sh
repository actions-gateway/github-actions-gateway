#!/usr/bin/env bash
#
# Tests for scripts/ci/git-merge-gate-lists.sh.
#
# This driver rewrites part of the Makefile during a merge, so two properties
# are asserted on every resolution rather than just one: the merged entry set is
# exactly right, AND `make` still parses the file. A driver that produces a
# correct-looking list with a dropped backslash breaks every build downstream,
# and a set assertion alone would not see it.
#
# The zero-churn case is asserted too. An earlier revision re-rendered every
# managed list on every merge, which rewrapped 30 lines of a variable neither
# side had touched; that is noise in every future merge and it buries the real
# change during review.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
DRIVER="$REPO_ROOT/scripts/ci/git-merge-gate-lists.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/git-merge-gate-lists-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0
LAST_RC=0

ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1"
	[[ -n "${2:-}" ]] && printf '     %s\n' "$2"
	fails=$((fails + 1))
}

# makefile SUITES — a Makefile carrying all four managed variables, because the
# driver requires every one of them to be present on every side.
makefile() {
	cat <<EOF
# leading prose
CHECK_FAST_GATES := lint-backlog doc-links \\
                    em-dash-check

CHECK_HEAVY_GATES := build-tags-check lint cover-check

STATUS_GATES := lint-backlog doc-links

SCRIPTS_TESTS := $1

.PHONY: all
all:
	@echo hi
EOF
}

run_merge() {
	cp "$2" "$FIXTURE_DIR/out"
	set +e
	"$DRIVER" "$1" "$FIXTURE_DIR/out" "$3" 7 Makefile 2>"$FIXTURE_DIR/err"
	LAST_RC=$?
	set -e
}

# entries_of FILE VAR — read the variable back through make's own parser, so a
# broken continuation shows up as a parse failure rather than a passing string
# comparison. `make --eval` is not available on the make shipped with macOS, so
# a wrapper makefile includes the file under test.
#
# --no-print-directory is load-bearing, not tidiness. Under `make scripts-test`
# this suite is a sub-make, and GNU make then writes "Entering directory ..." to
# stdout, whose words land in the entry list and fail every set comparison. It
# passed locally at top level and failed on CI for exactly that reason.
entries_of() {
	local file="$1" var="$2"
	# The `$(...)` here is Make's expansion syntax and must reach the generated
	# makefile literally, so the single quotes are the point.
	# shellcheck disable=SC2016
	printf 'include %s\n__l:\n\t@echo $(%s)\n' "$file" "$var" >"$FIXTURE_DIR/wrap.mk"
	make --no-print-directory -f "$FIXTURE_DIR/wrap.mk" __l 2>"$FIXTURE_DIR/make.err" | tr ' ' '\n' | sed '/^$/d' | sort
}

expect_set() {
	local desc="$1" var="$2"
	shift 2
	if ((LAST_RC != 0)); then
		bad "$desc" "driver reported a conflict: $(head -1 "$FIXTURE_DIR/err")"
		return
	fi
	local got want
	got="$(entries_of "$FIXTURE_DIR/out" "$var")"
	if [[ -s "$FIXTURE_DIR/make.err" ]]; then
		bad "$desc" "make could not parse the merged file: $(head -1 "$FIXTURE_DIR/make.err")"
		return
	fi
	want="$(printf '%s\n' "$@" | sort)"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc" "got [$(echo "$got" | tr '\n' ' ')] want [$(echo "$want" | tr '\n' ' ')]"
	fi
}

expect_fallback() {
	local desc="$1" want="$2"
	if grep -qF "$want" "$FIXTURE_DIR/err"; then
		ok "$desc"
	else
		bad "$desc" "stderr did not mention '$want': $(head -1 "$FIXTURE_DIR/err")"
	fi
}

# --- resolves what it is certain about ------------------------------------

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test b-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "adjacent appends from both sides both survive" SCRIPTS_TESTS \
	a-test b-test ours-test theirs-test

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "an entry deleted on one side stays deleted" SCRIPTS_TESTS \
	a-test theirs-test

# The plain three-way merge already handles a one-sided change; what matters is
# that routing the file through this driver does not lose the other side's work.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_set "a one-sided append is kept" SCRIPTS_TESTS a-test ours-test

# --- churn ----------------------------------------------------------------

makefile "a-test b-test" >"$FIXTURE_DIR/base"
makefile "a-test b-test ours-test" >"$FIXTURE_DIR/ours"
makefile "a-test b-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
if diff <(grep -A2 '^CHECK_FAST_GATES' "$FIXTURE_DIR/base") \
	<(grep -A2 '^CHECK_FAST_GATES' "$FIXTURE_DIR/out") >/dev/null; then
	ok "a list neither side touched is left byte for byte"
else
	bad "a list neither side touched is left byte for byte" \
		"$(diff <(grep -A2 '^CHECK_FAST_GATES' "$FIXTURE_DIR/base") <(grep -A2 '^CHECK_FAST_GATES' "$FIXTURE_DIR/out") | head -4 | tr '\n' ' ')"
fi

# --- refuses what it is not ------------------------------------------------

# A conflict elsewhere in the Makefile is an ordinary Makefile conflict.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test" | sed 's/^# leading prose/# ours prose/' >"$FIXTURE_DIR/ours"
makefile "a-test" | sed 's/^# leading prose/# theirs prose/' >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_fallback "a conflict outside the gate lists is refused, not resolved" \
	"conflicts outside the gate lists"

# A side missing a managed variable breaks the pairing the driver depends on.
makefile "a-test" >"$FIXTURE_DIR/base"
makefile "a-test" | grep -v '^STATUS_GATES' >"$FIXTURE_DIR/ours"
makefile "a-test theirs-test" >"$FIXTURE_DIR/theirs"
run_merge "$FIXTURE_DIR/base" "$FIXTURE_DIR/ours" "$FIXTURE_DIR/theirs"
expect_fallback "a side missing a managed variable is refused" "is not assigned"

# --- the wiring, end to end ------------------------------------------------

REPO="$FIXTURE_DIR/e2e"
mkdir -p "$REPO"
(
	cd "$REPO"
	git init -q -b main
	git config user.email t@example.com
	git config user.name Test
	git config "merge.gatelists.name" 'test'
	git config "merge.gatelists.driver" "$DRIVER %O %A %B %L %P %S %X %Y"
	printf 'Makefile merge=gatelists\n' >.gitattributes
	makefile "a-test b-test" >Makefile
	git add -A
	git commit -qm base
	git checkout -q -b topic
	makefile "a-test b-test topic-test" >Makefile
	git commit -qam "topic appends"
	git checkout -q main
	makefile "a-test b-test main-test" >Makefile
	git commit -qam "main appends"
	git checkout -q topic
) >/dev/null 2>&1

set +e
(cd "$REPO" && git rebase main) >"$FIXTURE_DIR/rebase.log" 2>&1
rebase_rc=$?
set -e
if ((rebase_rc == 0)); then
	got="$(entries_of "$REPO/Makefile" SCRIPTS_TESTS | tr '\n' ' ')"
	if [[ "$got" == *topic-test* && "$got" == *main-test* ]]; then
		ok "a real git rebase resolves adjacent appends through .gitattributes"
	else
		bad "a real git rebase resolves adjacent appends through .gitattributes" "got [$got]"
	fi
else
	bad "a real git rebase resolves adjacent appends through .gitattributes" \
		"rc=$rebase_rc; $(tail -2 "$FIXTURE_DIR/rebase.log" | tr '\n' ' ')"
fi

if ((fails > 0)); then
	echo "git-merge-gate-lists-test: ${fails} failure(s)"
	exit 1
fi
echo "git-merge-gate-lists-test: all assertions passed"
