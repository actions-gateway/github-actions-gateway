#!/usr/bin/env bash
#
# check-comparison-stamps-test.sh — both directions for every branch of the
# stamp rule, plus the two silent-pass shapes that make a gate like this
# worthless: a table the parser stops recognising, and a header with no body
# rows. Both must exit 2 rather than report a clean table.
#
# The last case points the gate at the real docs/why-gag.md. A rule asserted only
# against fixtures can drift away from the page it governs and stay green.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-comparison-stamps.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK="$REPO_ROOT/tmp/comparison-stamps-test"

rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

check() {
    local name="$1" want="$2"
    shift 2
    local got=0
    "$GATE" "$@" >"$WORK/out.log" 2>&1 || got=$?
    if [[ "$got" == "$want" ]]; then
        pass=$((pass + 1))
        printf 'ok   %s\n' "$name"
    else
        fail=$((fail + 1))
        printf 'FAIL %s (want exit %s, got %s)\n' "$name" "$want" "$got"
        sed 's/^/       /' "$WORK/out.log"
    fi
}

# grep the last run's output, for the cases where the exit code alone cannot
# tell one finding from another.
saw() {
    local name="$1" pattern="$2"
    if grep -qE "$pattern" "$WORK/out.log"; then
        pass=$((pass + 1))
        printf 'ok   %s\n' "$name"
    else
        fail=$((fail + 1))
        printf 'FAIL %s (output did not match /%s/)\n' "$name" "$pattern"
        sed 's/^/       /' "$WORK/out.log"
    fi
}

# table BODY... — a minimal page carrying the comparison table's exact shape.
table() {
    local out="$1"
    shift
    {
        printf '# Page\n\nProse above the table.\n\n'
        printf '| Capability | ARC (scale-set mode) | GitHub Actions Gateway |\n'
        printf '| --- | --- | --- |\n'
        printf '%s\n' "$@"
        printf '\nProse below the table.\n'
    } >"$out"
}

STAMP='<span class="gag-asof">0.14.2 · 2026-08-12</span>'
YES=':material-check-circle:{ .gag-yes } yes'
NO=':material-close-circle:{ .gag-no } no'
UNV=':material-help-circle:{ .gag-unverified } *unverified*'

# --- the two states, rendered correctly ------------------------------------

table "$WORK/ok.md" \
    "| Quota safety | $NO<br>$STAMP | $YES |" \
    "| Ephemeral pods | $YES<br>$STAMP | $YES |"
check "a verdict with a version and a date passes" 0 "$WORK/ok.md"

table "$WORK/unverified.md" \
    "| Quota safety | $UNV<br><span class=\"gag-cont\">source inspection only</span> | $YES |"
check "the unverified state with no stamp passes" 0 "$WORK/unverified.md"

table "$WORK/mixed.md" \
    "| Quota safety | $NO<br>$STAMP | $YES |" \
    "| Throttling | $UNV | $YES |"
check "a stamped verdict beside an unverified cell passes" 0 "$WORK/mixed.md"

# --- a verdict without its measurement -------------------------------------

table "$WORK/nostamp.md" "| Quota safety | $NO | $YES |"
check "a verdict with no stamp fails" 1 "$WORK/nostamp.md"
saw "  and names the capability" 'Quota safety.*0 measurement'

table "$WORK/twostamps.md" "| Quota safety | $NO<br>$STAMP$STAMP | $YES |"
check "a verdict with two stamps fails" 1 "$WORK/twostamps.md"

table "$WORK/nodate.md" \
    "| Quota safety | $NO<br><span class=\"gag-asof\">0.14.2</span> | $YES |"
check "a stamp with a version and no date fails" 1 "$WORK/nodate.md"

table "$WORK/noversion.md" \
    "| Quota safety | $NO<br><span class=\"gag-asof\">2026-08-12</span> | $YES |"
check "a stamp with a date and no version fails" 1 "$WORK/noversion.md"

table "$WORK/twoversions.md" \
    "| Quota safety | $NO<br><span class=\"gag-asof\">0.14.2 / 0.13.1 · 2026-08-12</span> | $YES |"
check "a stamp naming two versions fails" 1 "$WORK/twoversions.md"

table "$WORK/future.md" \
    "| Quota safety | $NO<br><span class=\"gag-asof\">0.14.2 · 2999-01-01</span> | $YES |"
check "a stamp dated in the future fails" 1 "$WORK/future.md"

# --- the unverified state, misused -----------------------------------------

table "$WORK/unvstamped.md" "| Quota safety | $UNV<br>$STAMP | $YES |"
check "an unverified cell carrying a stamp fails" 1 "$WORK/unvstamped.md"

table "$WORK/both.md" "| Quota safety | $NO $UNV<br>$STAMP | $YES |"
check "a cell that is both a verdict and unverified fails" 1 "$WORK/both.md"

table "$WORK/neither.md" "| Quota safety | it depends | $YES |"
check "a cell that is neither fails" 1 "$WORK/neither.md"

# --- scope: only the competitor column -------------------------------------
#
# The GAG column is this repo's own behavior, which a test already gates. A
# stamp requirement there would be noise, and a gate that quietly checked it
# would make every GAG cell unfixable without a citation nobody can produce.

table "$WORK/gagcol.md" "| Quota safety | $NO<br>$STAMP | $YES with no stamp at all |"
check "an unstamped verdict in the GAG column passes" 0 "$WORK/gagcol.md"

# --- shapes the parser must survive ----------------------------------------

table "$WORK/escapedpipe.md" \
    "| Quota safety | $NO<br><span class=\"gag-cont\">\`a \\| b\`</span>$STAMP | $YES |"
check "a cell holding an escaped pipe is still parsed" 0 "$WORK/escapedpipe.md"

# --- the silent passes, which must not be silent ---------------------------

cat >"$WORK/notable.md" <<'EOF'
# Page

No table here at all, just prose.
EOF
check "a file with no comparison table exits 2" 2 "$WORK/notable.md"
saw "  and says the parser lost the table" 'no comparison table found'

cat >"$WORK/emptytable.md" <<'EOF'
# Page

| Capability | ARC (scale-set mode) | GitHub Actions Gateway |
| --- | --- | --- |

Prose below.
EOF
check "a header with no body rows exits 2" 2 "$WORK/emptytable.md"

cat >"$WORK/renamed.md" <<'EOF'
# Page

| Capability | Actions Runner Controller | GitHub Actions Gateway |
| --- | --- | --- |
| Quota safety | no | yes |
EOF
check "a header whose second column stops naming ARC exits 2" 2 "$WORK/renamed.md"

check "a missing file exits 2" 2 "$WORK/does-not-exist.md"

# --- the report ------------------------------------------------------------

table "$WORK/report.md" \
    "| Newer | $NO<br><span class=\"gag-asof\">0.14.2 · 2026-08-12</span> | $YES |" \
    "| Older | $NO<br><span class=\"gag-asof\">0.13.1 · 2025-12-23</span> | $YES |"
check "--report passes on a valid table" 0 --report "$WORK/report.md"
saw "  and reports the older stamp" '2025-12-23.*0\.13\.1.*Older'
older_at="$(grep -n -m1 '2025-12-23' "$WORK/out.log" | cut -d: -f1)"
newer_at="$(grep -n -m1 '2026-08-12' "$WORK/out.log" | cut -d: -f1)"
if ((older_at < newer_at)); then
    pass=$((pass + 1))
    printf 'ok     and puts it before the newer one\n'
else
    fail=$((fail + 1))
    printf 'FAIL   and puts it before the newer one (line %s vs %s)\n' "$older_at" "$newer_at"
    sed 's/^/       /' "$WORK/out.log"
fi

# --- the page the rule governs ---------------------------------------------

check "the shipped docs/why-gag.md passes" 0 "$REPO_ROOT/docs/why-gag.md"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
