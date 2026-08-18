#!/usr/bin/env bash
#
# check-metric-tiers-test.sh — assertions for check-metric-tiers.sh.
#
# The Go checker's own cases live in devtools/docs/metrictiers/main_test.go and
# cover what each of its checks means, against a synthetic source tree.
# What only this suite can cover is the entry point: that the gate defaults to
# the three shipped paths, that it reports the real tree as clean, and that it
# fails on a defect seeded into the real ledger rather than passing by checking
# nothing.
#
# Both directions are asserted. A gate that stopped finding anything fails as
# silently as one that finds everything, and the shipped-tree case is the one
# that would go quiet: this gate's whole job is to be the thing that notices, so
# a green run has to be shown to mean something.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-metric-tiers.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

METRICS_DOC="$REPO_ROOT/docs/operations/observability-metrics.md"
PARITY_DOC="$REPO_ROOT/docs/plan/v2-ga.md"
AGC_SRC="$REPO_ROOT/cmd/agc"

failures=0

ok() { printf 'ok   %s\n' "$1"; }
fail() {
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
}

# expect_rc NAME EXPECTED_RC -- command...
expect_rc() {
    local name="$1" want="$2" rc=0
    shift 3
    "$@" >/dev/null 2>&1 || rc=$?
    if ((rc == want)); then
        ok "$name (rc=$rc)"
    else
        fail "$name: expected rc=$want, got rc=$rc"
    fi
}

# expect_output NAME NEEDLE -- command...
expect_output() {
    local name="$1" needle="$2" out
    shift 3
    out="$("$@" 2>&1 || true)"
    if [[ "$out" == *"$needle"* ]]; then
        ok "$name"
    else
        fail "$name: output did not contain '$needle'; got: $out"
    fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The shipped tree is clean, with no arguments and with the paths named.
expect_rc "shipped tree passes with no arguments" 0 -- "$GATE"
expect_rc "shipped tree passes with the paths named" 0 -- "$GATE" "$AGC_SRC" "$METRICS_DOC" "$PARITY_DOC"

# A metric dropped from the ledger is the case the gate exists for: the row is
# what a tier-asymmetric series would never have gained in the first place.
awk '!/^\| `actions_gateway_renew_job_teardowns_total` \| Classic only/' \
    "$METRICS_DOC" > "$TMP/no-row.md"
expect_rc "a metric with no ledger row fails" 1 -- "$GATE" "$AGC_SRC" "$TMP/no-row.md" "$PARITY_DOC"
expect_output "and says which tier question is unanswered" "which acquisition tier emits it" -- \
    "$GATE" "$AGC_SRC" "$TMP/no-row.md" "$PARITY_DOC"

# The other direction: a ledger row the source refutes. active_sessions is written
# from the classic listener alone, so calling it scale-set-only contradicts the
# site the source actually has.
awk '{ if ($0 ~ /^\| `actions_gateway_active_sessions` \| Classic only \|/) \
         print "| `actions_gateway_active_sessions` | Scale-set only | Seeded defect. |"; \
       else print }' "$METRICS_DOC" > "$TMP/contradicted.md"
expect_output "a single-tier claim the source refutes fails" "is emitted here, and the ledger calls it" -- \
    "$GATE" "$AGC_SRC" "$TMP/contradicted.md" "$PARITY_DOC"

# An unknown tier value: the vocabulary is closed on purpose, so "mostly" is not
# an answer the ledger can carry.
awk '{ if ($0 ~ /^\| `actions_gateway_job_duration_seconds` \| Both \|/) \
         print "| `actions_gateway_job_duration_seconds` | Mostly | Seeded defect. |"; \
       else print }' "$METRICS_DOC" > "$TMP/bad-tier.md"
expect_output "an unknown tier value fails" "which is not one of" -- \
    "$GATE" "$AGC_SRC" "$TMP/bad-tier.md" "$PARITY_DOC"

# The label-value half (Q851). A series can be Both while one of its values is
# not, so dropping the row for a value the source proves single-tier must fail.
awk '!/^\| `actions_gateway_eviction_retries_total` \| `cause` \| `vanished`/' \
    "$METRICS_DOC" > "$TMP/no-value-row.md"
expect_output "a single-tier label value with no row fails" 'give it a "### Label-value reach" row' -- \
    "$GATE" "$AGC_SRC" "$TMP/no-value-row.md" "$PARITY_DOC"

# The other direction, as for the series table: a value row the source refutes.
awk '{ if ($0 ~ /^\| `actions_gateway_eviction_retries_total` \| `cause` \| `vanished`/) \
         print "| `actions_gateway_eviction_retries_total` | `cause` | `vanished` | Classic only | Seeded defect. |"; \
       else print }' "$METRICS_DOC" > "$TMP/flipped-value.md"
expect_output "a value row the source refutes fails" "is named here, and the ledger calls it" -- \
    "$GATE" "$AGC_SRC" "$TMP/flipped-value.md" "$PARITY_DOC"

# Q867: a series the excluded tier reaches through an adapter. message_poll_errors_total
# is written in runnercore, so neither listener holds an emission site in its own
# subtree and the site half of the contradiction check sees nothing — the scale-set
# arm is visible only as the origin of the reason values it names. Seeding it on the
# real tree is what this suite can do that the synthetic one cannot.
awk '{ if ($0 ~ /^\| `actions_gateway_message_poll_errors_total` \| Both \|/) \
         print "| `actions_gateway_message_poll_errors_total` | Classic only | Seeded defect. |"; \
       else print }' "$METRICS_DOC" > "$TMP/adapter-tier.md"
expect_output "a single-tier claim only a value origin refutes fails" \
    "is named here, and the ledger calls the series" -- \
    "$GATE" "$AGC_SRC" "$TMP/adapter-tier.md" "$PARITY_DOC"

# A claim the file layout cannot prove rests on a guard, and the row must cite it
# — otherwise the value table is the hand-kept list the ledger exists to replace.
awk '{ if ($0 ~ /^\| `actions_gateway_worker_pods_reaped_total` \| `reason` \| `job_abandoned`/) \
         print "| `actions_gateway_worker_pods_reaped_total` | `reason` | `job_abandoned` | Classic only | Seeded defect, uncited. |"; \
       else print }' "$METRICS_DOC" > "$TMP/uncited-value.md"
expect_output "an unprovable value row with no citation fails" "cite the .go file whose guard does" -- \
    "$GATE" "$AGC_SRC" "$TMP/uncited-value.md" "$PARITY_DOC"

# The ledger is the gate's only input for the tier question, so its absence must
# be an error rather than a green run over zero rows.
awk '!/^## Acquisition-tier reach$/' "$METRICS_DOC" > "$TMP/no-ledger.md"
expect_rc "a missing ledger section is an error, not a pass" 2 -- \
    "$GATE" "$AGC_SRC" "$TMP/no-ledger.md" "$PARITY_DOC"

# A source tree with no metrics in it looks exactly like a clean one, so it must
# refuse rather than report green.
mkdir -p "$TMP/empty-src"
expect_rc "a source tree defining no metrics is an error" 2 -- \
    "$GATE" "$TMP/empty-src" "$METRICS_DOC" "$PARITY_DOC"

# Argument handling: 0 or 3, never 1 or 2.
expect_rc "one argument is rejected" 2 -- "$GATE" "$AGC_SRC"
expect_rc "two arguments are rejected" 2 -- "$GATE" "$AGC_SRC" "$METRICS_DOC"
expect_rc "a missing doc is rejected" 2 -- "$GATE" "$AGC_SRC" "$TMP/nope.md" "$PARITY_DOC"
expect_rc "a missing source dir is rejected" 2 -- "$GATE" "$TMP/nope-src" "$METRICS_DOC" "$PARITY_DOC"

printf '\n'
if ((failures > 0)); then
    printf 'check-metric-tiers-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'check-metric-tiers-test: ok\n'
