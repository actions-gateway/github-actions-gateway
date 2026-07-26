#!/usr/bin/env bash
#
# Run the claude-usage/ Python unit tests (Q437).
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
# needed for make_charts.py's matplotlib/numpy).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

readonly SUITE_DIR="claude-usage"

# python3 is an extended-tier prerequisite (scripts/check-tools.sh), not a
# required one, so a machine without it skips rather than fails — same contract
# as scripts/release-version-hook-test.sh. On CI it is a hard failure instead: a
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
	printf 'skip claude-usage-test: python3 not found (extended tier, scripts/check-tools.sh)\n'
	exit 0
fi

# unittest writes its progress and summary to stderr, so capture both streams
# and print them either way — the test count is the evidence this gate ran.
output=""
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
