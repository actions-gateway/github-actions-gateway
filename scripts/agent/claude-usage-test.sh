#!/usr/bin/env bash
#
# Byte-compile claude-usage/ and run its Python unit tests (Q437).
#
# claude-usage/ is the committed record of the project's Claude Code usage: the
# CSVs under data/ are the only surviving copy of days whose session transcripts
# have been archived, and compute_metrics.py's merge rule is what decides that a
# re-run can never revise a recorded day downward or collapse two machines'
# shares of one day. test_compute_metrics.py pins exactly that, but the module
# sits outside every dorny/paths-filter in .github/workflows/ — it is neither Go
# code nor a shell script — so the suite ran only when someone remembered to run
# it by hand. PR #841 shipped a model-mapping fix that no gate would have caught.
#
# Backs `make claude-usage-test` (part of `make check`) and the
# `claude-usage-test` job in .github/workflows/unit-test.yml, so the local and CI
# verdicts come from one implementation.
#
# The suite is stdlib-only — no venv, no requirements.txt install (that is only
# needed for make_charts.py's matplotlib/numpy). make_charts.py itself has no
# tests, so this gate byte-compiles the module first: parsing it is coverage no
# other gate provides, and it needs none of those unpinned dependencies.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

readonly SUITE_DIR="claude-usage"
# Directories that may sit under the module but are not its source: a venv a
# contributor created in place (the README's flow puts it at the repo root, but
# not everyone follows that) and any previous bytecode cache. Matched as a regex
# against each path by `compileall -x`.
readonly NON_SOURCE_RE='(^|/)(\.venv|venv|__pycache__)/'

# python3 is an extended-tier prerequisite (scripts/ci/check-tools.sh), not a
# required one, so a machine without it skips rather than fails — same contract
# as scripts/docs/release-version-hook-test.sh. On CI it is a hard failure instead: a
# gate that reports green by skipping is the false negative this gate exists to
# remove. (Consequence for the documented `CI=1 make check` throttle opt-out: on
# a python3-less machine that invocation fails here. Drop the CI=1 or install
# python3.)
if ! command -v python3 >/dev/null 2>&1; then
	if [[ -n "${CI:-}" ]]; then
		echo "claude-usage-test: python3 not found on PATH — install: https://www.python.org/downloads/" >&2
		echo "  refusing to skip on CI: a skipped gate reports green without testing anything" >&2
		exit 1
	fi
	printf 'skip claude-usage-test: python3 not found (extended tier, scripts/ci/check-tools.sh)\n'
	exit 0
fi

# Keep the gate's bytecode out of the worktree: PYTHONPYCACHEPREFIX (3.8+)
# redirects every __pycache__ this run would write into a throwaway tree. The
# caches are gitignored, so this is tidiness rather than correctness — but a gate
# that leaves build output in the tree it is checking invites the next reader to
# wonder whether it matters.
PYTHONPYCACHEPREFIX="$(mktemp -d)"
export PYTHONPYCACHEPREFIX
trap 'rm -rf "$PYTHONPYCACHEPREFIX"' EXIT

output=""

# Byte-compile the whole module before running the suite. make_charts.py has no
# tests of its own and imports matplotlib/numpy, so nothing else here would even
# parse it — a syntax error in it would reach main untouched by any gate. This
# does not import the file, so the unpinned matplotlib/numpy are not needed;
# equally, it proves only that the source parses, not that the charts render.
if ! output="$(python3 -m compileall -q -x "$NON_SOURCE_RE" "$SUITE_DIR" 2>&1)"; then
	printf '%s\n' "$output"
	echo "claude-usage-test: $SUITE_DIR/ does not byte-compile" >&2
	exit 1
fi

# unittest writes its progress and summary to stderr, so capture both streams
# and print them either way — the test count is the evidence this gate ran.
if ! output="$(python3 -m unittest discover -s "$SUITE_DIR" 2>&1)"; then
	printf '%s\n' "$output"
	echo "claude-usage-test: $SUITE_DIR tests failed" >&2
	exit 1
fi
printf '%s\n' "$output"

# `unittest discover` exits 0 on an empty run, so a suite renamed off the
# test*.py pattern (or moved out of the directory) would leave this gate passing
# while testing nothing — the same green-by-skipping hole in a smaller shape.
if [[ "$output" == *"Ran 0 tests"* ]]; then
	echo "claude-usage-test: discovered no tests under $SUITE_DIR/" >&2
	echo "  unittest discover only collects files matching test*.py — rename the suite back," >&2
	echo "  or point this gate at wherever it moved" >&2
	exit 1
fi

echo "claude-usage-test: ok"
