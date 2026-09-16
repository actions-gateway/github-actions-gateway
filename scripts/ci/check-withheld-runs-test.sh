#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-withheld-runs.sh (Q1052). Covers the pure half:
# selecting withheld PRs from recorded PR+runs JSON. The querying half needs a
# live repo and is exercised by the watch workflow's own run.
#
# Selection is the whole risk here. A withheld run has no signature of its own at
# the PR level - statusCheckRollup is empty and mergeStateStatus reads BLOCKED,
# which is what an unpushed branch and a failing check read - so the check keys on
# the one field that differs, and every neighbouring run state has to be shown NOT
# to select. The three that would otherwise pass for withheld are all here and all
# real: a queued run and an in_progress run have no conclusion at all, and a
# skipped run is `completed` with a conclusion, like a withheld one.
#
# Every run shape below is transcribed from this repo's own API output on
# 2026-09-16: PR #1877's superseded head 88ebcaa9 (16 runs, all withheld) and its
# current head dfb9e23a, released minutes earlier and running at attempt 2.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
SCRIPT="$REPO_ROOT/scripts/ci/check-withheld-runs.sh"

fails=0

# run EVENT STATUS CONCLUSION - one run object.
run() {
	printf '{"event":"%s","status":"%s","conclusion":%s}' "$1" "$2" \
		"$([[ "$3" == null ]] && echo null || printf '"%s"' "$3")"
}

# expect_select NAME WANT JSON - assert --select prints WANT for JSON on stdin.
expect_select() {
	local name="$1" want="$2" json="$3" got
	got="$("$SCRIPT" --select <<<"$json")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-26s %s\n' "$name" "${got:-<no findings>}"
	else
		printf 'FAIL %-26s want [%s] got [%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

WITHHELD_SHA='88ebcaa9ff33d45e401701340ae63d5ba57f5f38'
RELEASED_SHA='dfb9e23a4278227b5a623b825e82e8b724771cf8'
CLEAN_SHA='15fbfe0eff80d5bce7f738ea7a01f1bc5277bb97'

# pr NUMBER SHA RUNS... - one PR object carrying the given runs.
pr() {
	local number="$1" sha="$2"
	shift 2
	local IFS=','
	printf '{"number":%s,"headRefOid":"%s","runs":[%s]}' "$number" "$sha" "$*"
}

# --- The positive. Without this line every negative below is unreadable: a
# selector that matches nothing passes all of them. #1877's superseded head, as
# the API returns it, all 16 runs withheld at attempt 1.
held=()
for _ in {1..16}; do held+=("$(run pull_request completed action_required)"); done
expect_select withheld-head "1877 $WITHHELD_SHA 16/16" \
	"[$(pr 1877 "$WITHHELD_SHA" "${held[@]}")]"

# --- The three neighbours that must not select.
#
# Released and re-running: the same PR minutes later, after "Approve and run".
# This is the discriminator the whole check turns on - a queued run and a
# withheld run are both "no result yet" to every PR-level instrument.
expect_select released-attempt-2 '' \
	"[$(pr 1877 "$RELEASED_SHA" \
		"$(run pull_request queued null)" \
		"$(run pull_request in_progress null)" \
		"$(run pull_request completed success)")]"

# Never held: a Dependabot head nobody pushed to, every run green at attempt 1.
expect_select never-held '' \
	"[$(pr 1943 "$CLEAN_SHA" \
		"$(run pull_request completed success)" \
		"$(run pull_request completed skipped)")]"

# Skipped alone: `completed` WITH a conclusion, the same shape as withheld, and a
# path-gated workflow produces it on every PR in this repo.
expect_select skipped-only '' \
	"[$(pr 1943 "$CLEAN_SHA" "$(run pull_request completed skipped)")]"

# --- The documented blind spot, asserted so it stays deliberate. A PR carrying
# no runs reads identically to a branch pushed seconds ago, so it is not a
# finding. Changing that means changing this line.
expect_select no-runs-at-all '' "[$(pr 1943 "$CLEAN_SHA")]"

# --- Event scoping. The github-pages environment can also hold a run at
# action_required, on push rather than pull_request; a PR-scoped finding must not
# quote it.
expect_select push-event-ignored '' \
	"[$(pr 1943 "$CLEAN_SHA" \
		"$(run push completed action_required)" \
		"$(run pull_request completed success)")]"

# --- A partial hold blocks the PR as completely as a full one and has no other
# reporter, so ANY withheld run selects. The ratio names how many.
expect_select partial-hold "1878 $WITHHELD_SHA 1/4" \
	"[$(pr 1878 "$WITHHELD_SHA" \
		"$(run pull_request completed success)" \
		"$(run pull_request completed action_required)" \
		"$(run pull_request completed skipped)" \
		"$(run pull_request queued null)")]"

# --- Several PRs at once: the report is per-PR, and a clean PR beside a held one
# must not suppress it.
expect_select mixed-population "1877 $WITHHELD_SHA 16/16" \
	"[$(pr 1943 "$CLEAN_SHA" "$(run pull_request completed success)"),
	  $(pr 1877 "$WITHHELD_SHA" "${held[@]}"),
	  $(pr 1880 "$RELEASED_SHA" "$(run pull_request queued null)")]"

# --- An empty population is not a finding, and must not be an error either: the
# watch runs on a schedule and most runs see no open PRs in this state.
expect_select empty-population '' '[]'

# ---------------------------------------------------------------------------
# The exit-code contract, which is what withheld-runs-watch.yml branches on: 0
# nothing held, 1 findings, 2 could not measure. It reads anything above 1 as
# "this check is broken" and everything else as a finding, so a refusal that
# escapes as some other non-zero code opens an issue naming no PRs.
#
# It escaped once. Collecting and selecting were one pipeline, and under pipefail
# a pipeline yields its RIGHTMOST failing status, so a refusal surfaced as jq's
# exit 5 over the truncated JSON, with jq's parse error printed on top of the
# refusal's own message.
# ---------------------------------------------------------------------------

STUB_DIR="$REPO_ROOT/tmp/check-withheld-runs-test.$$"
mkdir -p "$STUB_DIR"
trap 'rm -rf "$STUB_DIR"' EXIT INT TERM

# stub_gh PR_LIST_JSON RUNS_JSON - a gh that answers `pr list` with the first and
# every `api` call with the second. RUNS_JSON is post-`--jq` output, the array the
# script's own filter produces, because applying that filter is gh's job: a stub
# that ignored --jq and returned the raw envelope measured itself rather than the
# script, and read as a defect for two probes before that showed.
stub_gh() {
	cat >"$STUB_DIR/gh" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "pr" && "\$2" == "list" ]]; then
	printf '%s\n' '$1'
	exit 0
fi
printf '%s\n' '$2'
EOF
	chmod +x "$STUB_DIR/gh"
}

# expect_rc NAME WANT PR_LIST_JSON RUNS_JSON
expect_rc() {
	local name="$1" want="$2" got=0
	stub_gh "$3" "$4"
	PATH="$STUB_DIR:$PATH" "$SCRIPT" >"$STUB_DIR/out" 2>&1 || got=$?
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %-26s rc=%s\n' "$name" "$got"
	else
		printf 'FAIL %-26s want rc=%s got rc=%s: %s\n' "$name" "$want" "$got" \
			"$(tr '\n' ' ' <"$STUB_DIR/out")" >&2
		fails=$((fails + 1))
	fi
}

HELD_RUN='{"event":"pull_request","status":"completed","conclusion":"action_required"}'
PASS_RUN='{"event":"pull_request","status":"completed","conclusion":"success"}'

expect_rc rc-nothing-held 0 "[{\"number\":1,\"headRefOid\":\"$CLEAN_SHA\"}]" "[$PASS_RUN]"
expect_rc rc-no-open-prs 0 '[]' '[]'
expect_rc rc-findings 1 "[{\"number\":1,\"headRefOid\":\"$WITHHELD_SHA\"}]" "[$HELD_RUN,$PASS_RUN]"
# The two reads that must refuse rather than report "nothing withheld": an empty
# head SHA would reach --head_sha and list every run in the repository, and a PR
# list that is not an array is a degraded API, not an empty one.
expect_rc rc-empty-head-sha 2 '[{"number":1,"headRefOid":""}]' '[]'
expect_rc rc-malformed-pr-list 2 'null' '[]'

# expect_out NAME WANT_SUBSTRING PR_LIST_JSON RUNS_JSON
# Asserts on what the run PRINTED, not on its status. The defect these cover
# exited 0 correctly and said the wrong thing.
expect_out() {
	local name="$1" want="$2"
	stub_gh "$3" "$4"
	PATH="$STUB_DIR:$PATH" "$SCRIPT" >"$STUB_DIR/out" 2>&1 || true
	if grep -qF "$want" "$STUB_DIR/out"; then
		printf 'ok   %-26s %s\n' "$name" "$want"
	else
		printf 'FAIL %-26s want %s in: %s\n' "$name" "$want" \
			"$(tr '\n' ' ' <"$STUB_DIR/out")" >&2
		fails=$((fails + 1))
	fi
}

# A clean scan must state its denominator. Without it a scan over PRs and a scan
# over none printed the same line and the watch workflow closed its issue on
# either, asserting every PR had been released when none had been looked at.
# Deleting the count from report()'s zero-findings branch reddens both.
expect_out examined-two '(2 open PR(s) examined)' \
	"[{\"number\":1,\"headRefOid\":\"$CLEAN_SHA\"},{\"number\":2,\"headRefOid\":\"$CLEAN_SHA\"}]" \
	"[$PASS_RUN]"
expect_out examined-none '(0 open PR(s) examined)' '[]' '[]'

# A PR list filled exactly to the limit cannot be told from one the limit
# truncated, so it refuses. gh reports no truncation flag of its own.
SATURATED="$(jq -nc --arg sha "$CLEAN_SHA" \
	'[range(500) | {number: (. + 1), headRefOid: $sha}]')"
expect_rc rc-pr-list-at-limit 2 "$SATURATED" '[]'

if ((fails > 0)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
echo
echo 'All check-withheld-runs.sh selection tests passed.'
