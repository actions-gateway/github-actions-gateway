#!/usr/bin/env bash
# check-gates-green-test.sh — pins the classification the gate check turns on.
#
# The defect this script was rewritten to fix is a false GREEN: a run whose heavy
# job was path-skipped still concludes `success`, so a run-level reading called a
# commit validated when nothing had run on it. The cases below therefore assert the
# *skip* direction as hard as the pass direction — a classifier that only ever
# answered "ok" would have shipped the original bug again.
#
# The pure helpers are sourced rather than driven through the network, so the suite
# stays offline and deterministic under `make scripts-test`.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-gates-green.sh"

pass=0
fail=0
ok() {
	printf '[check-gates-green-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-gates-green-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

expect() {
	local desc="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc"
		printf '       want: %s\n       got:  %s\n' "$want" "$got" >&2
	fi
}

# Usage errors are exit 2, distinct from a real finding, so a broken invocation
# cannot be read as "the gates are red".
rc=0
"$SUBJECT" a b >/dev/null 2>&1 || rc=$?
expect "too many arguments is a usage error" 2 "$rc"

rc=0
"$SUBJECT" definitely-not-a-ref >/dev/null 2>&1 || rc=$?
expect "a ref that is not a commit is exit 2" 2 "$rc"

# Not followed by shellcheck: the library guard returns early, so following it
# makes every case below look unreachable (SC2317) when they all run.
# shellcheck source=/dev/null
CHECK_GATES_GREEN_LIB=1 source "$SUBJECT"

# --- skipped_jobs: the direction that matters -------------------------------
#
# This fixture is the real shape measured on 050f1e80e: `changes` and the always()
# gate job succeed while the heavy job is skipped, and the run concludes success.
run_with_skip='{"jobs":[
	{"name":"changes","conclusion":"success"},
	{"name":"e2e-gate","conclusion":"success"},
	{"name":"e2e","conclusion":"skipped"}]}'
expect "a path-skipped heavy job is reported, not swallowed" \
	"e2e" "$(skipped_jobs "$run_with_skip")"

run_full='{"jobs":[
	{"name":"changes","conclusion":"success"},
	{"name":"e2e-gate","conclusion":"success"},
	{"name":"e2e","conclusion":"success"}]}'
expect "a run that executed in full reports no skips" \
	"" "$(skipped_jobs "$run_full")"

multi='{"jobs":[
	{"name":"promql","conclusion":"skipped"},
	{"name":"validate","conclusion":"skipped"}]}'
expect "every skipped job is named" \
	"promql, validate" "$(skipped_jobs "$multi")"

# --- successful_runs: any lane may answer, and all are offered --------------
#
# The merge queue is where a commit is validated, so a merge_group success counts
# even when the push lane for the same SHA is still pending.
runs_queue='[{"event":"push","status":"queued","conclusion":null,"databaseId":1},
	{"event":"merge_group","status":"completed","conclusion":"success","databaseId":2}]'
expect "a merge_group success answers for the commit" \
	"merge_group	2" "$(successful_runs "$runs_queue")"

# Both lanes are handed back, because they disagree about what they execute: a
# merge_group run path-gates its heavy job where the push lane runs it outright.
# Returning only the newest would make the verdict depend on list order.
runs_both='[{"event":"push","status":"completed","conclusion":"success","databaseId":9},
	{"event":"merge_group","status":"completed","conclusion":"success","databaseId":8}]'
expect "every successful lane is offered, not just the newest" \
	"push	9
merge_group	8" "$(successful_runs "$runs_both")"

runs_red='[{"event":"merge_group","status":"completed","conclusion":"failure","databaseId":3}]'
expect "a failed lane is not offered" "" "$(successful_runs "$runs_red")"
expect "a failing lane is described, not just counted" \
	"merge_group:completed/failure" "$(run_state "$runs_red")"
expect "an empty lane list says so" \
	"no run found for this commit" "$(run_state '[]')"

# --- workflow_for: the derivation, and its fail-safe ------------------------
expect "a gate context resolves to the workflow declaring it" \
	"e2e-calico.yml" "$(workflow_for e2e-calico-gate)"

# The case that killed the obvious implementation: a context whose stem is not its
# filename. Deriving `<name>-gate` -> `<name>.yml` sends this to `e2e.yml`, which
# does not exist, and the e2e lane silently leaves the pre-flight.
expect "a context whose stem is not its filename still resolves" \
	"e2e-test.yml" "$(workflow_for e2e-gate)"

expect "a context no workflow declares maps to nothing" \
	"" "$(workflow_for no-such-thing-gate)"

# The derivation is only worth anything if it still matches the repository. Every
# context the ruleset requires must resolve, or the pre-flight under-covers exactly
# the way it did when e2e-calico was absent from a hand-kept list.
ctxs="$(required_contexts || true)"
if [[ -n "$ctxs" ]]; then
	bad_ctx=""
	while IFS= read -r c; do
		[[ -n "$c" ]] || continue
		[[ -n "$(workflow_for "$c")" ]] || bad_ctx+="${c} "
	done <<<"$ctxs"
	expect "every required context resolves to a workflow" "" "${bad_ctx% }"
else
	printf '[check-gates-green-test] SKIP ruleset case (rulesets unreadable — offline or no permission)\n'
fi

printf '[check-gates-green-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
