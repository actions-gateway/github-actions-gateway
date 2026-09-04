#!/usr/bin/env bash
#
# check-rung-order-test.sh — assertions for check-rung-order.sh.
#
# The Go checker's own cases live in devtools/docs/rungorder/main_test.go and
# cover what each check means against synthetic inputs. What only this suite can
# cover is the entry point: that the gate defaults to the two shipped paths, that
# it reports the real tree as clean, and that it fails on a defect seeded into the
# real doc rather than passing by checking nothing.
#
# Both directions are asserted. A gate that stopped finding anything fails as
# silently as one that finds everything, and the shipped-tree case is the one that
# would go quiet: this gate's whole job is to be the thing that notices, so a
# green run has to be shown to mean something.
#
# The seeded defect is the exact drift that shipped — Rate listed ahead of Ceiling
# — so this suite fails if the gate ever stops catching the regression it was
# written for.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-rung-order.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

ADMISSION_SRC="$REPO_ROOT/cmd/agc/internal/provisioner/admission.go"
FLOWS_DOC="$REPO_ROOT/docs/design/04-operational-flows.md"

WORK="$REPO_ROOT/tmp/check-rung-order-test"
rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

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
    die_if_killed "$name" "$rc" "$want"
    if ((rc == want)); then
        ok "$name (rc=$rc)"
    else
        fail "$name: expected rc=$want, got rc=$rc"
    fi
}

# The shipped tree must be clean, both by default and with the paths named.
expect_rc "shipped tree is clean (default paths)" 0 -- "$GATE"
expect_rc "shipped tree is clean (explicit paths)" 0 -- "$GATE" "$ADMISSION_SRC" "$FLOWS_DOC"

# Seed the regression: swap Ceiling and Rate in the doc's ladder, which is the
# order that shipped from Q717 until Q972. The swap is asserted to have applied
# before the gate is run on it — an edit that silently matched nothing would
# leave the doc clean, and the gate would then pass for the wrong reason, which
# is the one outcome this case cannot distinguish from success on its own.
drifted="$WORK/flows-drifted.md"
python3 - "$FLOWS_DOC" "$drifted" <<'PY'
import re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src, encoding="utf-8").read()
ceiling = re.search(r"^ *\*\*Ceiling:\*\*.*\n", text, re.M)
rate = re.search(r"^ *\*\*Rate:\*\*.*\n", text, re.M)
if not ceiling or not rate:
    sys.exit("seed: could not find both rung lines to swap")
if ceiling.start() > rate.start():
    sys.exit("seed: doc already lists Rate before Ceiling; nothing to seed")
swapped = text[:ceiling.start()] + rate.group(0) + ceiling.group(0) + text[rate.end():]
if swapped == text:
    sys.exit("seed: swap produced an identical file")
open(dst, "w", encoding="utf-8").write(swapped)
PY

if ! grep -q '\*\*Rate:\*\*' "$drifted"; then
    fail "seeded doc lost its Rate rung; the mutation did not apply as intended"
fi

expect_rc "seeded rung-order drift is caught" 1 -- "$GATE" "$ADMISSION_SRC" "$drifted"

# A doc whose ladder block has moved is a read failure (rc=2), never a pass. A
# check whose subject is absent has verified nothing, and the failure mode this
# pins is a gate that goes permanently green after an unrelated reword.
missing="$WORK/flows-missing.md"
sed 's/\*\*Admit (/**Reworded (/' "$FLOWS_DOC" > "$missing"
if grep -q '\*\*Admit (' "$missing"; then
    fail "anchor removal did not apply; the missing-block case would pass vacuously"
fi
expect_rc "a missing ladder block refuses rather than passing" 2 -- "$GATE" "$ADMISSION_SRC" "$missing"

# Same guard on the code side.
renamed="$WORK/admission-renamed.go"
sed 's/func (p \*Provisioner) Admit(/func (p *Provisioner) Renamed(/' "$ADMISSION_SRC" > "$renamed"
if grep -q 'func (p \*Provisioner) Admit(' "$renamed"; then
    fail "Admit rename did not apply; the missing-Admit case would pass vacuously"
fi
expect_rc "a missing Admit refuses rather than passing" 2 -- "$GATE" "$renamed" "$FLOWS_DOC"

# A nonexistent path is a usage failure, not a finding.
expect_rc "a nonexistent doc refuses" 2 -- "$GATE" "$ADMISSION_SRC" "$WORK/no-such-file.md"
expect_rc "a wrong argument count refuses" 2 -- "$GATE" "$ADMISSION_SRC"

if ((failures > 0)); then
    printf '\ncheck-rung-order-test: %d failure(s)\n' "$failures" >&2
    exit 1
fi
printf '\ncheck-rung-order-test: ok\n'
