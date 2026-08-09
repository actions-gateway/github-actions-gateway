#!/usr/bin/env bash
#
# Unit tests for scripts/e2e/e2e-github-cleanup.sh's runner filter (Q511).
#
# The filter decides what a destructive sweep against a real GitHub repo touches, so
# both directions matter and neither is the obvious one:
#
#   - Too narrow and the sweep leaves a registration behind. The suite's preflight
#     blocks on that registration and names this script as the remedy, so the operator
#     gets a loop: run the cleanup, watch it report success, watch the next run refuse
#     to start on the same runner.
#   - Too wide and it deregisters a runner this suite never created.
#
# The positive cases are real names from the 2026-07-29 collision (real-ag-e2e-6d8749c-0)
# rather than invented ones, so the shape under test is the shape GitHub actually saw.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its helpers; the BASH_SOURCE guard there keeps
# main() from running — which would reach out to GitHub — on source.
# shellcheck source=scripts/e2e/e2e-github-cleanup.sh
source "$REPO_ROOT/scripts/e2e/e2e-github-cleanup.sh"

fails=0

ok() { printf 'ok   %-46s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-46s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# expect_selected NAME GATEWAY LINE — the filter keeps LINE.
expect_selected() {
	local name="$1" gateway="$2" line="$3" got
	got=$(printf '%s\n' "$line" | select_suite_runners "$gateway")
	if [[ "$got" == "$line" ]]; then
		ok "$name" "kept $(printf '%q' "$line")"
	else
		bad "$name" "should have kept $(printf '%q' "$line"), got $(printf '%q' "$got")"
	fi
}

# expect_rejected NAME GATEWAY LINE — the filter drops LINE.
expect_rejected() {
	local name="$1" gateway="$2" line="$3" got
	got=$(printf '%s\n' "$line" | select_suite_runners "$gateway")
	if [[ -z "$got" ]]; then
		ok "$name" "dropped $(printf '%q' "$line")"
	else
		bad "$name" "should have dropped $(printf '%q' "$line"), got $(printf '%q' "$got")"
	fi
}

echo '== runners this suite owns are selected =='
expect_selected 'v1 agent, index 0' real-ag '42 real-ag-e2e-6d8749c-0 online busy=false'
expect_selected 'v1 agent, index 1' real-ag '43 real-ag-e2e-6d8749c-1 offline busy=false'
expect_selected 'v2 RunnerSet agent' real-ag '44 rs-real-ag-e2e-6d8749c-0 online busy=true'
expect_selected 'a different gateway name' probe-ag '45 probe-ag-e2e-1a2b3c4-0 online busy=false'
# The Q422 sibling gateway is named by EXTENDING the suite's gateway name, so that one
# prefix reaches both. A sibling named independently would strand runners here.
expect_selected 'the Q422 sibling gateway' real-ag '51 real-ag-sib-e2e-9f3c1a2-0 online busy=false'

echo
echo '== runners this suite does not own are left alone =='
expect_rejected 'another tenant on the same repo' real-ag '46 other-ag-e2e-6d8749c-0 online busy=false'
expect_rejected 'a gateway name that merely ends in ours' real-ag '47 not-real-ag-e2e-6d8749c-0 online busy=false'
expect_rejected 'the gateway name as a bare prefix' real-ag '48 real-agent-farm-0 online busy=false'
expect_rejected 'a foreign rs- registration' real-ag '49 rs-other-ag-e2e-6d8749c-0 online busy=false'
expect_rejected 'the gateway name in a later field' real-ag '50 someone-else-0 online busy=false real-ag-'

echo
echo '== the whole listing is filtered, not just its first line =='
listing=$(printf '%s\n' \
	'46 other-ag-e2e-6d8749c-0 online busy=false' \
	'42 real-ag-e2e-6d8749c-0 online busy=false' \
	'44 rs-real-ag-e2e-6d8749c-0 online busy=true')
want=$(printf '%s\n' \
	'42 real-ag-e2e-6d8749c-0 online busy=false' \
	'44 rs-real-ag-e2e-6d8749c-0 online busy=true')
got=$(printf '%s\n' "$listing" | select_suite_runners real-ag)
if [[ "$got" == "$want" ]]; then
	ok 'mixed listing' 'kept both of ours, dropped the foreign one'
else
	bad 'mixed listing' "want $(printf '%q' "$want") got $(printf '%q' "$got")"
fi

echo
if ((fails > 0)); then
	echo "$fails assertion(s) failed" >&2
	exit 1
fi
echo 'all assertions passed'
