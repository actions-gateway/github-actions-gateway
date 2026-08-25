#!/usr/bin/env bash
#
# check-fixture-maintenance.sh — hold every scripts/ suite to the invariant that
# a throwaway fixture repo never spawns background git (Q820, Q921).
#
# Usage:
#   scripts/ci/check-fixture-maintenance.sh <trace-dir>
#
# <trace-dir> holds one GIT_TRACE file per suite, written by run-parallel.sh
# under RUN_PARALLEL_GIT_TRACE_DIR. Reading a run that already happens is the
# whole point: the suites are already `make check`'s slowest fast gate, and a
# second pass over them to measure this would double that for one assertion.
#
# Git spawns `git maintenance run --auto --detach` from commit, merge, fetch, am
# and pull unless `maintenance.auto false` is set on the repo. Detached, it
# outlives the command that started it and runs while the next one writes to the
# same fixture; once the fixture crosses gc.auto it reaches a prune that removes
# an object fanout directory between the next command's mkdir and the open under
# it — `unable to create temporary file`, exit 128, green on rerun (Q820).
#
# The assertion is on behaviour, never on the config key: a suite that sets the
# key and greps for the key has tested its own setup. It also reaches the one
# suite no text query can: pr-requeue-eligible-test.sh drives git from embedded
# Python as git(["init", "-q", "-b", "main"], repo), so no search for a command
# string sees its fixture (Q878).
#
# Refuses with rc 2 rather than reporting a clean tier whenever the evidence
# cannot bear the verdict: a missing directory, or no trace carrying any git at
# all. A loop over an empty directory reports zero spawns and exits 0, which is
# exactly what a gate whose wiring has silently come undone looks like.

set -euo pipefail
shopt -s inherit_errexit

if (( $# != 1 )); then
	printf 'usage: %s <trace-dir>\n' "${0##*/}" >&2
	exit 2
fi

trace_dir="$1"

if [[ ! -d "$trace_dir" ]]; then
	printf '%s: no trace directory at %s; is RUN_PARALLEL_GIT_TRACE_DIR still wired into the fan-out?\n' \
		"${0##*/}" "$trace_dir" >&2
	exit 2
fi

# Every spawn writes three trace lines; `run_command: git maintenance run` is
# the one that appears exactly once per spawn, so it is what gets counted.
SPAWN='run_command: git maintenance run'
# The evidence check. A suite that never runs git writes no trace file, and a
# tier where none of them did means the trace never reached any child.
EVIDENCE='trace: built-in: git'

traces=()
while IFS= read -r f; do
	traces+=("$f")
done < <(find "$trace_dir" -type f -name '*.trace' -print | sort)

if (( ${#traces[@]} == 0 )); then
	printf '%s: %s holds no *.trace file; the fan-out ran no child under GIT_TRACE\n' \
		"${0##*/}" "$trace_dir" >&2
	exit 2
fi

with_git=0
offenders=()
for f in "${traces[@]}"; do
	label="${f##*/}"
	label="${label%.trace}"
	if grep -qF -- "$EVIDENCE" "$f"; then
		with_git=$((with_git + 1))
	fi
	n="$(grep -cF -- "$SPAWN" "$f" || true)"
	if (( n > 0 )); then
		offenders+=("$label ($n spawn(s))")
	fi
done

if (( with_git == 0 )); then
	printf '%s: %d trace file(s), none recording a git command; GIT_TRACE reached no git\n' \
		"${0##*/}" "${#traces[@]}" >&2
	exit 2
fi

if (( ${#offenders[@]} > 0 )); then
	printf '%s: a fixture repo spawned background git maintenance\n' "${0##*/}" >&2
	printf '  %s\n' "${offenders[@]}" >&2
	printf 'Set git -C <repo> config maintenance.auto false at fixture creation in each suite above.\n' >&2
	printf 'Why it matters: docs/development/testing.md#a-fixture-repo-must-not-run-background-git\n' >&2
	exit 1
fi

printf '%s: no background git maintenance across %d suite(s), %d of which ran git\n' \
	"${0##*/}" "${#traces[@]}" "$with_git"
