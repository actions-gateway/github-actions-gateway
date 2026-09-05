#!/usr/bin/env bash
#
# rank-vectors-test.sh — run queue.py's rank algebra against rank-vectors.tsv.
#
# The vectors are derived from the scheme rather than from any implementation,
# so this suite is evidence that queue.py agrees with the definition, not merely
# with itself. That argument rests on where the vectors come from, so it holds
# with queue.py as the only consumer: nothing else runs this fixture.
#
# Each expectation is checked three ways: the key matches, it orders strictly
# between its neighbours, and it satisfies check_rank. The last two catch a
# wrong vector as well as a wrong implementation.

set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
Q="$HERE/queue.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

vectors() {  # vectors <fixture>
    python3 - "$Q" "$1" <<'PY'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("q", sys.argv[1])
q = importlib.util.module_from_spec(spec)
spec.loader.exec_module(q)

OPEN, REFUSE = "-", "!"
fails, ran = [], 0

with open(sys.argv[2], encoding="utf-8") as fh:
    for n, line in enumerate(fh, 1):
        line = line.rstrip("\n")
        if not line or line.startswith("#"):
            continue
        fields = line.split("\t", 3)
        if len(fields) != 4:
            # A space where a tab belongs is the likely hand-edit.
            fails.append(f"line {n}: want 4 tab-separated fields, got {len(fields)}")
            continue
        lo, hi, want, note = fields
        lo = None if lo == OPEN else lo
        hi = None if hi == OPEN else hi
        ran += 1
        try:
            got = q.rank_between(lo, hi)
        except ValueError:
            if want != REFUSE:
                fails.append(f"{note}: between({lo!r}, {hi!r}) refused, want {want!r}")
            else:
                print(f"ok   vector: {note}")
            continue
        if want == REFUSE:
            fails.append(f"{note}: between({lo!r}, {hi!r}) -> {got!r}, want a refusal")
        elif got != want:
            fails.append(f"{note}: between({lo!r}, {hi!r}) -> {got!r}, want {want!r}")
        elif (lo is not None and lo >= got) or (hi is not None and got >= hi):
            fails.append(f"{note}: {got!r} is not strictly between {lo!r} and {hi!r}")
        else:
            try:
                q.check_rank(got)
            except ValueError as e:
                fails.append(f"{note}: generated {got!r} fails check_rank: {e}")
            else:
                print(f"ok   vector: {note}")

if not ran:
    fails.append(f"{sys.argv[2]} yielded no vectors")
for f in fails:
    print(f"FAIL {f}")
sys.exit(1 if fails else 0)
PY
}

vectors "$HERE/rank-vectors.tsv" || fail=1

# A suite never shown failing is not evidence that it checks anything, so each
# arm of the comparison gets a fixture carrying one planted defect.
{
    cat "$HERE/rank-vectors.tsv"
    printf 'a1\ta2\ta1z\tplanted: a key the scheme does not generate\n'
} > "$TMP/wrong-key.tsv"
if vectors "$TMP/wrong-key.tsv" >/dev/null 2>&1; then
    bad "control: a wrong expected key passes"
else
    ok "control: a wrong expected key fails"
fi

{
    cat "$HERE/rank-vectors.tsv"
    printf 'a1\ta2\t!\tplanted: a call that succeeds, marked as a refusal\n'
} > "$TMP/wrong-refusal.tsv"
if vectors "$TMP/wrong-refusal.tsv" >/dev/null 2>&1; then
    bad "control: a missing refusal passes"
else
    ok "control: a missing refusal fails"
fi

printf 'a1 a2 a1i not tab-separated\n' > "$TMP/malformed.tsv"
if vectors "$TMP/malformed.tsv" >/dev/null 2>&1; then
    bad "control: a malformed vector line passes"
else
    ok "control: a malformed vector line fails"
fi

exit "$fail"
