#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-gate-needs.sh — the `*-gate` needs
# reconciliation (Q856).
#
# Each case writes a throwaway workflow directory, so the assertions are about
# the rule rather than about whichever jobs this repo happens to ship today.
#
# The red case and its control are the point. A job missing from the gate's
# `needs:` must fail; the same workflow with that job listed must pass. Without
# the control, a checker whose parse had stopped finding jobs at all would pass
# every green case and catch nothing — which is the exact failure mode this gate
# exists to prevent in the workflows themselves.
#
# The rest pin the shapes that would otherwise pass by checking nothing: an empty
# directory and a tree with no `-gate` job are refusals (exit 2) rather than clean
# runs, a lone scalar `needs:` is read the same as a sequence, and a second gate
# job is not itself required to be waited on.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/ci/check-gate-needs.sh"

fails=0
WORK="$(mktemp -d)"
# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# write_wf NAME — a workflow directory holding one file, read from stdin.
write_wf() {
	local dir="$WORK/$1"
	rm -rf "$dir"
	mkdir -p "$dir"
	cat >"$dir/wf.yml"
	printf '%s\n' "$dir"
}

# expect NAME WANT DIR — run the gate over DIR and compare its exit status.
expect() {
	local name="$1" want="$2" dir="$3" got=0
	"$GATE" --dir "$dir" >"$WORK/out" 2>&1 || got=$?
	die_if_killed "$name" "$got" "$want"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
		return
	fi
	printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
	awk '{ print "    " $0 }' "$WORK/out"
	fails=$((fails + 1))
}

expect_out() {
	local name="$1" pattern="$2"
	if grep -q -- "$pattern" "$WORK/out"; then
		printf 'ok   %s\n' "$name"
		return
	fi
	printf 'FAIL %s: no match for %s in:\n' "$name" "$pattern"
	awk '{ print "    " $0 }' "$WORK/out"
	fails=$((fails + 1))
}

# --- the red case, and the control that gives it meaning --------------------

dir="$(write_wf missing <<'YML'
name: t
on: [pull_request]
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  beta:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  t-gate:
    needs: [alpha]
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YML
)"
expect "a job missing from the gate's needs fails" 1 "$dir"
expect_out "the finding names the job and the gate" 'beta'

dir="$(write_wf complete <<'YML'
name: t
on: [pull_request]
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  beta:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  t-gate:
    needs: [alpha, beta]
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YML
)"
expect "the same workflow with every job listed passes" 0 "$dir"

# --- both spellings of `needs:` ---------------------------------------------

# A lone scalar is valid YAML for a single dependency, and reading only the
# sequence form would pass this by finding no needs to check against nothing.
dir="$(write_wf scalar <<'YML'
name: t
on: [pull_request]
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  t-gate:
    needs: alpha
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YML
)"
expect "a scalar needs is read like a sequence" 0 "$dir"

# --- a gate is not required to wait on another gate -------------------------

dir="$(write_wf twogates <<'YML'
name: t
on: [pull_request]
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  first-gate:
    needs: [alpha]
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  second-gate:
    needs: [alpha]
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YML
)"
expect "a gate need not wait on another gate" 0 "$dir"

# --- refusals: a gate that checked nothing would otherwise read as clean -----

dir="$WORK/empty"
rm -rf "$dir"
mkdir -p "$dir"
expect "a directory with no workflows is a refusal" 2 "$dir"

dir="$(write_wf nogate <<'YML'
name: t
on: [pull_request]
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YML
)"
expect "a tree with no gate job at all is a refusal" 2 "$dir"

expect "a directory that does not exist is a refusal" 2 "$WORK/nope"

if ((fails > 0)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall tests passed\n'
