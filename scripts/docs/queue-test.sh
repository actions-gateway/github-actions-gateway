#!/usr/bin/env bash
#
# test-queue.sh — exercise queue.py against fixtures.
#
# Every checker case is paired: a clean store must pass, and a store carrying
# one introduced defect must fail. A linter never shown failing is not evidence
# that it checks anything.

set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
Q="$HERE/queue.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

expect_rc() {  # expect_rc <want> <name> <cmd...>
    local want="$1" name="$2" rc=0
    shift 2
    "$@" >"$TMP/out" 2>"$TMP/err" || rc=$?
    die_if_killed "$name" "$rc" "$want"
    if [[ "$rc" == "$want" ]]; then
        ok "$name"
    else
        bad "$name (rc=$rc want=$want)"
        sed 's/^/       /' "$TMP/err" | head -3
    fi
}

expect_eq() {  # expect_eq <want> <got> <name>
    if [[ "$2" == "$1" ]]; then ok "$3"; else bad "$3 (got '$2' want '$1')"; fi
}

expect_in() {  # expect_in <pattern> <file> <name>
    if grep -q "$1" "$2"; then ok "$3"; else bad "$3 (no match for '$1')"; fi
}

item() {  # item <dir> <id> <rank> <status> [notes]
    mkdir -p "$1"
    {
        printf -- '---\nid: %s\nrank: %s\nstatus: %s\nsize: S\n---\n\n# Title for %s\n' \
            "$2" "$3" "$4" "$2"
        if [[ -n "${5:-}" ]]; then printf '\n%s\n' "$5"; fi
    } > "$1/$2.md"
}

ids() {  # ids <store> [--all]
    local store="$1"
    shift
    python3 "$Q" --store "$store" render "$@" | awk '{print $2}' | tr '\n' ' '
}

# --- rank algebra ---------------------------------------------------------

if ! python3 - "$Q" <<'PY'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("q", sys.argv[1])
q = importlib.util.module_from_spec(spec)
spec.loader.exec_module(q)
fails = []

# Nested midpoints stay strictly inside, so an insert never reorders siblings.
a = q.rank_between(None, None)
b = q.rank_between(a, None)
for _ in range(200):
    mid = q.rank_between(a, b)
    if not a < mid < b:
        fails.append(f"between({a},{b}) -> {mid} is not strictly inside")
        break
    b = mid
if not fails:
    print("ok   rank: 200 nested midpoints stay ordered")

# Head insertion is the hot case — "file the new flake at the top" — and the
# magnitude head is what keeps it from growing a digit every few inserts.
keys = []
cur = q.rank_between(None, None)
for _ in range(5000):
    cur = q.rank_between(None, cur)
    keys.append(cur)
if any(keys[i] >= keys[i - 1] for i in range(1, len(keys))):
    fails.append("head inserts are not monotonically decreasing")
longest = max(len(k) for k in keys)
if longest > 6:
    fails.append(f"5000 head inserts grew a key to {longest} chars")
else:
    print(f"ok   rank: 5,000 head inserts stay <= {longest} chars")

# Tail insertion, the other open end.
cur = q.rank_between(None, None)
tail = []
for _ in range(5000):
    cur = q.rank_between(cur, None)
    tail.append(cur)
if tail != sorted(tail):
    fails.append("tail inserts are not monotonically increasing")
else:
    print(f"ok   rank: 5,000 tail inserts stay <= {max(len(k) for k in tail)} chars")

# Scale: a bulk import must not run into a wall.
for n in (1_300, 50_000, 1_000_000):
    s = q.rank_series(n)
    if len(s) != n or s != sorted(s) or len(set(s)) != n:
        fails.append(f"rank_series({n}) is not {n} distinct ordered keys")
    else:
        print(f"ok   rank: {n:>9,} items -> distinct ordered keys, "
              f"{len(s[-1])} chars")

# Every generated key must satisfy the checker, including the no-trailing-zero
# rule a midpoint could otherwise violate.
for k in keys[:200] + tail[:200]:
    try:
        q.check_rank(k)
    except ValueError as e:
        fails.append(f"generated key {k!r} fails check_rank: {e}")
        break
else:
    print("ok   rank: generated keys satisfy check_rank")

# The reserved bottom is fractional runway rather than a key. The 5,000-insert
# descent above stops nowhere near it, so its neighbourhood is checked here.
bottom = q.SMALLEST_INTEGER
for hi in (q.increment_integer(bottom), bottom + "1", bottom + "i"):
    got = q.rank_between(None, hi)
    if not got < hi:
        fails.append(f"between(None, {hi!r}) -> {got!r} is not below it")
        break
    try:
        q.check_rank(got)
    except ValueError as e:
        fails.append(f"between(None, {hi!r}) -> {got!r} fails check_rank: {e}")
        break
else:
    print("ok   rank: head inserts at the bottom of the space stay legal")

# Interop: the gateway's own live keys must validate and order under this port.
real = ["a0", "a1", "a2", "b0e", "b1r", "Zz"]
for k in real:
    try:
        q.check_rank(k)
    except ValueError as e:
        fails.append(f"live key {k!r} rejected: {e}")
if sorted(real) != ["Zz", "a0", "a1", "a2", "b0e", "b1r"]:
    fails.append("uppercase heads do not sort below lowercase")
else:
    print("ok   rank: live gateway keys validate and order")

# Malformed keys must be refused.
for bad, why in (("", "empty"), ("0a", "no head"), ("a", "too short for head"),
                 ("zz", "shorter than its head requires"),
                 ("a0z0", "trailing zero"), ("a0!", "non-base-36")):
    try:
        q.check_rank(bad)
        fails.append(f"check_rank accepted {bad!r} ({why})")
    except ValueError:
        pass
else:
    print("ok   rank: malformed keys are refused")

for f in fails:
    print(f"FAIL {f}")
sys.exit(1 if fails else 0)
PY
then
    fail=1
fi

# --- lint: a clean store passes, each defect fails ------------------------

S="$TMP/clean"
item "$S" Q1 a0 ready
item "$S" Q2 a1 ready
item "$S" Q3 a2 blocked "Blocked by Q1 until that lands."
expect_rc 0 "lint: clean store passes" python3 "$Q" --store "$S" lint

S="$TMP/dup"
item "$S" Q1 a0 ready
item "$S" Q2 a1 ready
sed -i.bak 's/^id: Q2/id: Q1/' "$S/Q2.md" && rm -f "$S/Q2.md.bak"
expect_rc 1 "lint: filename/id mismatch fails" python3 "$Q" --store "$S" lint

# Any non-empty base-36 string is a legal key, so the invalid case is a
# character outside the alphabet — uppercase sorts before every digit and
# would silently reorder the store.
S="$TMP/badrank"
item "$S" Q1 "0a" ready
expect_rc 1 "lint: rank with no magnitude head fails" python3 "$Q" --store "$S" lint

S="$TMP/heads"
item "$S" Q1 "Zz" ready
item "$S" Q2 "a0" ready
item "$S" Q3 "b0e" ready
expect_rc 0 "lint: mixed magnitude heads validate" python3 "$Q" --store "$S" lint
expect_eq "Q1 Q2 Q3 " "$(ids "$S")" "render: uppercase heads sort below lowercase"

# A blocked row is asked to open with what it waits on, not to name an item.
# The two are different questions, and answering the second with "contains any
# id" was wrong in both directions: three of this repo's four blocked rows
# escaped on ids they merely cite, while the one stating a non-item blocker in
# its first sentence was the only one flagged.
# One case per accepted opener. The bold form is what the deferred-trigger
# check next to this one already expects a structured opener to look like, so a
# note carrying it must not read as saying nothing.
i=0
for opener in "Blocked on an upstream release" "**Blocked by** the rename" \
              "Blocked by [Q2](Q2.md)"; do
    i=$((i + 1))
    S="$TMP/extblock-$i"
    item "$S" Q1 a0 blocked "$opener, with no item of ours to name."
    item "$S" Q2 a1 ready
    rc=0
    python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
    die_if_killed "lint: a blocked row opening '$opener'" "$rc"
    # "is blocked but", not the rest of the sentence: a grep for wording only
    # the current message carries cannot fail against the check this one
    # replaced, and an assertion that cannot fail is not evidence.
    if [[ $rc -eq 0 ]] && ! grep -q "is blocked but" "$TMP/e"; then
        ok "lint: a blocked row opening '$opener' is quiet"
    else
        bad "lint: a blocked row opening '$opener' still warned"
    fi
done

S="$TMP/vacuousblock"
# shellcheck disable=SC2016  # the backticks are the code span under test
item "$S" Q1 a0 blocked 'Still stuck. The `work Q123` example is unrelated.'
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a blocked row citing an unrelated id" "$rc"
if [[ $rc -eq 0 ]] && grep -q "is blocked but" "$TMP/e"; then
    ok "lint: a blocked row citing an unrelated id still warns"
else
    bad "lint: blocked-without-a-blocker note missing or it failed the store"
fi

# A reference is a link at an item's file. A bare id in prose is a mention of
# history that stays true forever, and warning on those made the store noisier
# with every item it cleared.
S="$TMP/mention"
item "$S" Q1 a0 ready "Found during the Q99 audit, which shipped long ago."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a bare id in prose is a mention" "$rc"
if [[ $rc -eq 0 ]] && ! grep -q "Q99" "$TMP/e"; then
    ok "lint: a bare id in prose is a mention, not a reference"
else
    bad "lint: a prose mention of a shipped item warned"
fi

S="$TMP/danglink"
item "$S" Q1 a0 ready "Superseded by [Q99](Q99.md), which is not in the store."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a link at an item not in the store" "$rc"
if [[ $rc -eq 0 ]] && grep -q "links Q99" "$TMP/e"; then
    ok "lint: a link at an item not in the store warns without failing"
else
    bad "lint: dangling-link note missing or it failed the store"
fi

S="$TMP/quotedlink"
# shellcheck disable=SC2016  # the backticks are the code span under test
item "$S" Q1 a0 ready 'The form is `[Q99](Q99.md)`, quoted rather than used.'
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a link inside backticks is quoted syntax" "$rc"
if [[ $rc -eq 0 ]] && ! grep -q "Q99" "$TMP/e"; then
    ok "lint: a link inside backticks is quoted syntax, not a reference"
else
    bad "lint: a backticked link was read as a reference"
fi

S="$TMP/badstatus"
item "$S" Q1 a0 pending
expect_rc 1 "lint: unknown status fails" python3 "$Q" --store "$S" lint

S="$TMP/nofm"
mkdir -p "$S"
printf '# No frontmatter\n' > "$S/Q1.md"
expect_rc 1 "lint: missing frontmatter fails" python3 "$Q" --store "$S" lint

# A title has no page of its own to overflow into: it renders whole in every
# index row and in the kickoff prompt, so the cap is an error where the note's
# is not a cap at all.
S="$TMP/longtitle"
mkdir -p "$S"
long_title="$(python3 -c 'print("T" * 73)')"
printf -- '---\nid: Q1\nrank: a0\nstatus: ready\n---\n\n# %s\n' "$long_title" > "$S/Q1.md"
expect_rc 1 "lint: a title over the cap fails" python3 "$Q" --store "$S" lint

S="$TMP/boundary"
mkdir -p "$S"
at_cap="$(python3 -c 'print("T" * 72)')"
printf -- '---\nid: Q1\nrank: a0\nstatus: ready\n---\n\n# %s\n' "$at_cap" > "$S/Q1.md"
expect_rc 0 "lint: a title exactly at the cap passes" python3 "$Q" --store "$S" lint

# The note is deliberately uncapped — that is the whole point of the layout.
S="$TMP/longnote2"
item "$S" Q1 a0 ready "$(python3 -c 'print("word " * 400)')"
expect_rc 0 "lint: a very long note passes, since its page has no cap" \
    python3 "$Q" --store "$S" lint

S="$TMP/notitle"
mkdir -p "$S"
printf -- '---\nid: Q1\nrank: a0\nstatus: ready\n---\n\nno heading\n' > "$S/Q1.md"
expect_rc 1 "lint: missing title fails" python3 "$Q" --store "$S" lint

S="$TMP/notrigger"
item "$S" Q1 a0 deferred "Parked for now."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: deferred without a trigger" "$rc"
if [[ $rc -eq 0 ]] && grep -q "names no trigger" "$TMP/e"; then
    ok "lint: deferred without a trigger warns without failing"
else
    bad "lint: deferred-without-trigger note missing or it failed the store"
fi

# One case per trigger word the skill documents. Checking only one of the three
# is how `**Decision:**` — the documented form for "a choice gets made" — came
# to warn as though it named no trigger at all.
for word in Demand Event Decision; do
    S="$TMP/withtrigger-$word"
    item "$S" Q1 a0 deferred "**$word:** the condition the skill names."
    python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e"
    if grep -q "names no trigger" "$TMP/e"; then
        bad "lint: a stated **$word:** trigger still warned"
    else
        ok "lint: a stated **$word:** trigger is quiet"
    fi
done

# A store with nothing in it passes, and must say so loudly enough that a
# --store pointed at the wrong directory is not read as a clean bill of health.
S="$TMP/emptystore"
mkdir -p "$S"
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: an empty store passes with a note" "$rc"
if [[ $rc -eq 0 ]] && grep -q "no Q\*.md under" "$TMP/e"; then
    ok "lint: an empty store passes with a note naming the directory"
else
    bad "lint: empty-store note missing or it failed the store"
fi

S="$TMP/fileref"
item "$S" Q1 a0 ready "The defect is at nosuch/thing.go:120 today."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: an unresolvable file:line reference" "$rc"
if [[ $rc -eq 0 ]] && grep -q "nosuch/thing.go:120" "$TMP/e"; then
    ok "lint: an unresolvable file:line reference warns without failing"
else
    bad "lint: file:line note missing or it failed the store"
fi

# A leading `.` is not a word character, so a `\b` anchor never holds before
# `.github/` and the path is captured a character in. Both directions matter:
# the note must name the whole path, and a citation that does resolve must
# still go quiet rather than warn on everything.
S="$TMP/dotref"
item "$S" Q1 a0 ready "The defect is at .github/workflows/nosuch.yml:12 today."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a dot-directory citation" "$rc"
if [[ $rc -eq 0 ]] && grep -q "cites \.github/workflows/nosuch\.yml:12" "$TMP/e"; then
    ok "lint: a dot-directory citation warns with its leading dot intact"
else
    bad "lint: dot-directory citation note missing, or its path lost the dot"
fi

# The same anchor governs a `../` citation, which resolves from neither base
# once its prefix survives.
S="$TMP/dotdotref"
item "$S" Q1 a0 ready "The defect is at ../nosuch/thing.go:7 today."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a ../ citation" "$rc"
if [[ $rc -eq 0 ]] && grep -q "cites \.\./nosuch/thing\.go:7" "$TMP/e"; then
    ok "lint: a ../ citation warns with its prefix intact"
else
    bad "lint: ../ citation note missing, or its path lost the prefix"
fi

# The quiet half. The file has to exist under the store's parent for the
# resolve branch to be the reason for the silence.
S="$TMP/dotok"
mkdir -p "$TMP/.github/workflows"
: > "$TMP/.github/workflows/real.yml"
item "$S" Q1 a0 ready "The defect is at .github/workflows/real.yml:12 today."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a dot-directory citation that resolves" "$rc"
if [[ $rc -eq 0 ]] && ! grep -q "real\.yml" "$TMP/e"; then
    ok "lint: a dot-directory citation that resolves stays quiet"
else
    bad "lint: a resolving dot-directory citation warned or failed the store"
fi

# And the pattern must still want an extension it knows. The list is a
# whitelist, so an unknown suffix and a bare colon-number both stay quiet;
# broadening the alternation surfaces here rather than in the store.
S="$TMP/notaref"
item "$S" Q1 a0 ready "The window is 10:30 wide and nosuch/notes.txt:4 is not one."
rc=0
python3 "$Q" --store "$S" lint >"$TMP/o" 2>"$TMP/e" || rc=$?
die_if_killed "lint: a colon-number that is not a file reference" "$rc"
if [[ $rc -eq 0 ]] && ! grep -q "which does not" "$TMP/e"; then
    ok "lint: a colon-number that is not a file reference stays quiet"
else
    bad "lint: a non-citation colon-number warned or failed the store"
fi

# --- metrics --------------------------------------------------------------
# Needs real history, since it reads git rather than the files on disk.

G="$TMP/repo"
mkdir -p "$G/docs/queue"
git -C "$G" init -q
# Q820: no detached maintenance racing the next command in a fixture repo.
git -C "$G" config maintenance.auto false
git -C "$G" config user.email t@example.com
git -C "$G" config user.name Test
item "$G/docs/queue" Q1 a0 ready
item "$G/docs/queue" Q2 a1 ready
git -C "$G" add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:00" GIT_COMMITTER_DATE="2026-01-01T00:00:00" \
    git -C "$G" commit -qm "docs(queue): file Q1 and Q2"
rm "$G/docs/queue/Q1.md"
git -C "$G" add -A
GIT_AUTHOR_DATE="2026-01-11T00:00:00" GIT_COMMITTER_DATE="2026-01-11T00:00:00" \
    git -C "$G" commit -qm "docs(queue): complete Q1"

python3 "$Q" --store "$G/docs/queue" metrics >"$TMP/m" 2>"$TMP/me"
if grep -q "^filed        2" "$TMP/m" && grep -q "^completed    1" "$TMP/m" \
   && grep -q "^open         1" "$TMP/m"; then
    ok "metrics: counts filed, completed and open from history"
else
    bad "metrics: wrong counts"
    sed 's/^/       /' "$TMP/m" "$TMP/me" | head -8
fi
if grep -q "cycle time   median 10d" "$TMP/m"; then
    ok "metrics: cycle time spans the file's own history"
else
    bad "metrics: cycle time wrong"; grep cycle "$TMP/m" | sed 's/^/       /'
fi
python3 "$Q" --store "$G/docs/queue" metrics --events >"$TMP/ev" 2>/dev/null
# BSD grep has no -P, so match the fields rather than a tab-escaped pattern.
if awk -F'\t' '$1=="Q1" && $2=="2026-01-01" && $3=="2026-01-11" && $4=="10" && $5=="complete"{f=1} END{exit !f}' "$TMP/ev"; then
    ok "metrics: --events emits a per-item row with its removal verb"
else
    bad "metrics: events row wrong"; sed 's/^/       /' "$TMP/ev" | head -4
fi

# --- claims ---------------------------------------------------------------
# Needs a branch and a remote, since it checks the ids a branch adds against
# the claims on the remote rather than anything in the files themselves.

C="$TMP/claims"
mkdir -p "$C"
git init -q --bare "$C/origin.git"
git -C "$C/origin.git" config maintenance.auto false
git -C "$C/origin.git" symbolic-ref HEAD refs/heads/main
git init -q "$C/work"
git -C "$C/work" config maintenance.auto false
git -C "$C/work" config user.email t@example.com
git -C "$C/work" config user.name Test
CS="$C/work/docs/queue"
# Filed before the allocator existed, so it holds no claim and never will —
# the state the whole live backlog is in, and the reason the rule keys on what
# a branch adds rather than on every id in the store.
item "$CS" Q1 a0 ready
git -C "$C/work" add -A
git -C "$C/work" commit -qm seed
git -C "$C/work" remote add origin "$C/origin.git"
git -C "$C/work" push -q origin HEAD:refs/heads/main
git -C "$C/work" fetch -q origin

expect_rc 0 "claims: an unclaimed id already on main is not this branch's" \
    python3 "$Q" --store "$CS" claims --strict

# Uncommitted, which is where the gate meets a hand-picked id: `make check`
# runs over the written row before the commit that would file it.
item "$CS" Q2 a1 ready
expect_rc 1 "claims: an id the branch adds with no claim fails" \
    python3 "$Q" --store "$CS" claims
rm "$CS/Q2.md"

# The two scripts agree or the rule is unenforceable, and they encode the ref
# namespace separately. Asserted by running the real allocator rather than by
# comparing the two spellings, which would flag every legal difference too.
#
# This repo's allocator claims through `gh api -X POST repos/<slug>/git/refs`
# rather than the `git push` form, which is the path the skill names as the one
# proven here at 460+ live claims. A local bare remote cannot serve it, so the
# fixture below reaches the wrong half of the mechanism and the allocator
# correctly emits nothing. The case reports that it did not run rather than
# passing vacuously, which is the behaviour to keep; what it cannot do here is
# assert the agreement. Both sides still encode `refs/queue-ids/` and the
# namespace is asserted directly below, so the gap is the cross-script call,
# not the convention.
got=""
if [[ "${QUEUE_TEST_ALLOCATOR_FIXTURE:-}" == "push" ]]; then
    got="$(cd "$C/work" && bash "$HERE/alloc-queue-id.sh" 'A properly claimed item' 2>/dev/null)"
fi
if [[ -z "$got" ]]; then
    ok "claims: cross-script case skipped, the allocator here needs a forge API"
fi
if [[ "$got" =~ ^Q[0-9]+$ ]]; then  # only under the push-form fixture above
    item "$CS" "$got" a1 ready
    expect_rc 0 "claims: an id from alloc-queue-id.sh is accepted" \
        python3 "$Q" --store "$CS" claims --strict
    # Delete the claim under the same store: without this the pass above is
    # equally consistent with the id never having been checked at all.
    git -C "$C/origin.git" update-ref -d "refs/queue-ids/$got"
    expect_rc 1 "claims: the same id fails once its claim is removed" \
        python3 "$Q" --store "$CS" claims --strict
    rm "$CS/$got.md"
elif [[ "${QUEUE_TEST_ALLOCATOR_FIXTURE:-}" == "push" ]]; then
    # Only a failure when the fixture was asked to exercise the push form. The
    # skip above already reported the forge-API case, and reporting it twice
    # would read as a defect rather than an un-runnable arrangement.
    bad "claims: the allocator emitted '$got', so the cross-script case never ran"
fi

# The escape hatch, for an id whose claim lives somewhere this remote cannot
# see. Both spellings, because the env var is the one that reaches a run
# through `make`.
item "$CS" Q900 a2 ready
expect_rc 0 "claims: --allow excuses an id claimed elsewhere" \
    python3 "$Q" --store "$CS" claims --allow Q900 --strict
expect_rc 0 "claims: QUEUE_CLAIMS_ALLOW is the same hatch through make" \
    env QUEUE_CLAIMS_ALLOW=Q900 python3 "$Q" --store "$CS" claims --strict
rm "$CS/Q900.md"

# Merge base, not tip. main completes Q1 and deletes it while this branch is
# behind; the branch still carries the row, and it did not add it.
git clone -q -b main "$C/origin.git" "$C/other"
git -C "$C/other" config maintenance.auto false
git -C "$C/other" config user.email t@example.com
git -C "$C/other" config user.name Test
git -C "$C/other" rm -q docs/queue/Q1.md
git -C "$C/other" commit -qm 'complete Q1'
git -C "$C/other" push -q origin HEAD:main
git -C "$C/work" fetch -q origin
if git -C "$C/work" ls-tree -r --name-only origin/main -- docs/queue | grep -q Q1; then
    bad "claims: fixture wrong — main still carries Q1, so the tip case is untested"
else
    ok "claims: main has dropped Q1, so a tip-keyed check would demand a claim"
fi
expect_rc 0 "claims: a row main deleted while the branch was behind is not added" \
    python3 "$Q" --store "$CS" claims --strict

# Skips, and what --strict does to each. An added id is needed first, or the
# check returns before it ever reaches for the remote.
item "$CS" Q901 a3 ready
expect_rc 0 "claims: an unreachable remote skips" \
    python3 "$Q" --store "$CS" claims --remote "$C/nope.git"
expect_rc 1 "claims: --strict turns the unreachable-remote skip into a failure" \
    python3 "$Q" --store "$CS" claims --remote "$C/nope.git" --strict
expect_rc 0 "claims: a base that does not resolve skips" \
    python3 "$Q" --store "$CS" claims --base origin/nope
expect_rc 1 "claims: --strict turns the missing-base skip into a failure" \
    python3 "$Q" --store "$CS" claims --base origin/nope --strict
expect_rc 0 "claims: a store outside a git repository skips" \
    python3 "$Q" --store "$TMP/clean" claims

# --- render and next ------------------------------------------------------

S="$TMP/order"
item "$S" Q9 a2 ready
item "$S" Q4 a0 ready
item "$S" Q7 a1 deferred
expect_eq "Q4 Q9 " "$(ids "$S")" "render: rank order, deferred hidden"
expect_eq "Q4 Q7 Q9 " "$(ids "$S" --all)" "render: --all includes deferred"
expect_eq "Q4: Title for Q4" "$(python3 "$Q" --store "$S" next --title --no-pr-check)" \
    "next: picks the top ready item"

# --- next: the open-PR check (Q990) ---------------------------------------
#
# A stub gh rather than the real one: the check reaches the network by design,
# and this suite runs in a gate that has none. FAKE_GH_CLAIMED is the set of
# ids an open PR names; FAKE_GH_FAIL makes the call fail the way an offline or
# unauthenticated gh does; FAKE_GH_FLOOD pads the list past the read limit.
#
# Each claimed id gets a PR that names it in the *body* only. That is the shape
# the check has to catch and the one a title match would miss, so a stub that
# put the id in the title would pass a matcher that only ever read titles.
GHBIN="$TMP/ghbin"
mkdir -p "$GHBIN"
cat > "$GHBIN/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
shopt -s inherit_errexit
if [[ -n "${FAKE_GH_FAIL:-}" ]]; then
    printf 'gh: %s\n' "$FAKE_GH_FAIL" >&2
    exit 1
fi
if [[ -n "${FAKE_GH_SLEEP:-}" ]]; then sleep "$FAKE_GH_SLEEP"; fi
n=0
printf '['
sep=""
for qid in ${FAKE_GH_CLAIMED:-}; do
    n=$(( n + 1 ))
    printf '%s{"number":%d,"title":"feat: something","url":"https://x.invalid/%d",' \
        "$sep" "$n" "$n"
    printf '"body":"Closes %s in passing."}' "$qid"
    sep=","
done
for (( i = n + 1; i <= ${FAKE_GH_FLOOD:-0}; i++ )); do
    printf '%s{"number":%d,"title":"chore: pad","url":"https://x.invalid/%d","body":""}' \
        "$sep" "$i" "$i"
    sep=","
done
printf ']\n'
STUB
chmod +x "$GHBIN/gh"

# QUEUE_GH_TIMEOUT so the stubbed cases do not ride on host scheduling: the
# product default is 20s, and a loaded box starves even a local stub past it.
# Seen at load 64 on 18 cores, where three assertions went red on a timeout.
withgh() {  # withgh <claimed-ids> <cmd...>
    local claimed="$1"
    shift
    env PATH="$GHBIN:$PATH" QUEUE_GH_TIMEOUT=600 FAKE_GH_CLAIMED="$claimed" "$@"
}

expect_rc 0 "next: an unclaimed top item is handed out" \
    withgh "" python3 "$Q" --store "$S" next
expect_in "^Q4:" "$TMP/out" "next: ... and it is the top one"
expect_in "no open PR named it" "$TMP/out" \
    "next: the prompt says the check ran, rather than telling the session to"

expect_rc 0 "next: a claimed top item is skipped" \
    withgh "Q4" python3 "$Q" --store "$S" next
expect_in "^Q9:" "$TMP/out" "next: ... and the next ready item is handed out"
expect_in "skipping Q4: #1 is open" "$TMP/err" \
    "next: ... loudly, naming the PR so a false hit can be judged"

# --title is a second process in the invocation the docs teach, so it must see
# the same skip: a title that checked nothing would name the session after one
# item while the prompt handed out another.
expect_eq "Q9: Title for Q9" \
    "$(withgh "Q4" python3 "$Q" --store "$S" next --title 2>/dev/null)" \
    "next: --title runs the check too"

expect_rc 0 "next: --allow takes an id an open PR only cites" \
    withgh "Q4" python3 "$Q" --store "$S" next --allow Q4
expect_in "^Q4:" "$TMP/out" "next: ... the overridden id, not the one below it"
expect_in "check for an open PR first" "$TMP/out" \
    "next: ... and the prompt claims no clean check, because there was none"

expect_rc 1 "next: every ready item claimed is a failure, not a pick" \
    withgh "Q4 Q9" python3 "$Q" --store "$S" next

# The two constraints the row names. Neither may end with a pick on stdout: an
# unverified pick reads exactly like a verified one once it is in the prompt.
expect_rc 1 "next: a gh that cannot answer fails loud" \
    env PATH="$GHBIN:$PATH" QUEUE_GH_TIMEOUT=600 \
    FAKE_GH_FAIL="could not reach github.com" \
    python3 "$Q" --store "$S" next
expect_eq "" "$(cat "$TMP/out")" "next: ... and hands out nothing"
expect_in "could not reach github.com" "$TMP/err" "next: ... quoting gh's reason"

# A full page means the rest went unread, so "nothing claims this" is a read
# that never happened — the silent pass the row forbids.
expect_rc 1 "next: a truncated PR list fails rather than reporting nothing" \
    env PATH="$GHBIN:$PATH" QUEUE_GH_TIMEOUT=600 FAKE_GH_FLOOD=200 \
    python3 "$Q" --store "$S" next
expect_in "went unread" "$TMP/err" "next: ... saying the read was incomplete"
expect_eq "" "$(cat "$TMP/out")" "next: ... and handing out nothing"

# The deadline knob is wired, and the message names the deadline that actually
# applied — without this the cases above could be green because 600 was never
# read, which is indistinguishable from a stub that simply answered in time.
expect_rc 1 "next: a gh that outlives its deadline fails loud" \
    env PATH="$GHBIN:$PATH" QUEUE_GH_TIMEOUT=1 FAKE_GH_SLEEP=5 \
    python3 "$Q" --store "$S" next
expect_in "within 1s" "$TMP/err" "next: ... naming the deadline that applied"

NOGH="$TMP/nogh"
mkdir -p "$NOGH"
ln -sf "$(command -v python3)" "$NOGH/python3"
if PATH="$NOGH" command -v gh >/dev/null 2>&1; then
    bad "next: fixture wrong — gh is still on PATH, so the absent case is untested"
else
    expect_rc 1 "next: an absent gh fails loud" \
        env PATH="$NOGH" python3 "$Q" --store "$S" next
    expect_in "gh is not on PATH" "$TMP/err" "next: ... saying which tool is missing"
fi

expect_rc 0 "next: --no-pr-check takes the top item with no network" \
    env PATH="$NOGH" python3 "$Q" --store "$S" next --no-pr-check
expect_in "^Q4:" "$TMP/out" "next: ... the top one"
expect_in "nothing asked whether Q4" "$TMP/err" \
    "next: ... and says the check was skipped, so the pick is not read as clean"

# Equal ranks break by id, so two sessions that never saw each other commute.
S="$TMP/tie"
item "$S" Q20 a0 ready
item "$S" Q3 a0 ready
expect_eq "Q3 Q20 " "$(ids "$S")" "render: equal ranks break by id"

S="$TMP/empty"
mkdir -p "$S"
expect_rc 1 "next: empty store fails" python3 "$Q" --store "$S" next

# --- migrate --------------------------------------------------------------

SRC="$TMP/STATUS.md"
cat > "$SRC" <<'EOF'
# Project Status

## Queue

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q11"></a>Q11 | [First thing](plan/a.md) | `ci` `debt` | 🔲 | S | A note with an escaped \| pipe. |
| <a id="Q12"></a>Q12 | Second thing | `bug` | 🚫 | M | Blocked by [Q11](#Q11). |

## Deferred

| ID | Item | Labels | Sz | Trigger |
|---|---|---|---|---|
| <a id="Q13"></a>Q13 | Parked thing | `feature` | L | **Event:** something ships. |

## Queue

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q14"></a>Q14 | Prefix [First thing](plan/a.md) suffix | `ci` | 🔲 | S | An inline link mid-cell. |
EOF

mkdir -p "$TMP/docs/plan" "$TMP/docs/queue"
printf '# a\n' > "$TMP/docs/plan/a.md"
OUT="$TMP/docs/queue"

expect_rc 0 "migrate: converts a table" python3 "$Q" --store "$OUT" migrate "$SRC"
if [[ -f "$OUT/Q11.md" && -f "$OUT/Q12.md" && -f "$OUT/Q13.md" ]]; then
    ok "migrate: one file per row"
else
    bad "migrate: missing item files"
fi
expect_in 'target: \.\./plan/a.md' "$OUT/Q11.md" \
    "migrate: re-bases the item link one directory down"
# The link is usually part of the cell, not the whole of it. Taking only the
# link text drops the words around it, and a truncated title still reads as one.
expect_in '# Prefix First thing suffix' "$OUT/Q14.md" \
    "migrate: keeps the words around an inline link in the title"
expect_in 'status: blocked' "$OUT/Q12.md" "migrate: maps a blocked marker"
expect_in 'status: deferred' "$OUT/Q13.md" "migrate: deferred rows carry status"
expect_in 'escaped | pipe' "$OUT/Q11.md" "migrate: unescapes a pipe in notes"
# A Notes cell citing a sibling row by table anchor must become a sibling page:
# the anchor exists only in the table, so it resolves to nothing once the table
# is deleted, and nothing looks broken until someone clicks.
expect_in 'Q11.md' "$OUT/Q12.md" "migrate: re-bases a #QNNN anchor in notes to a sibling page"
if grep -q '(#Q11)' "$OUT/Q12.md"; then
    bad "migrate: left a bare table anchor in notes"
else
    ok "migrate: no bare table anchor survives in notes"
fi
expect_eq "Q11 Q12 Q13 Q14 " "$(ids "$OUT" --all)" "migrate: preserves table order"
expect_rc 0 "migrate: output passes lint" python3 "$Q" --store "$OUT" lint

# The pre-counter format keeps ✅/▶/💤 in the Queue's status column, and the
# status test here reads only 🚫 — so a shipped row and a parked row both come
# out `status: ready`, which resurrects completed work into the backlog. Refuse
# instead, and refuse before writing anything.
OLD="$TMP/STATUS-old.md"
cat > "$OLD" <<'EOF'
# Project Status

## Queue

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q1"></a>Q1 | Shipped thing | `bug` | ✅ | S | done. |
| <a id="Q2"></a>Q2 | Parked thing | `docs` | 💤 | S | waiting. |
| <a id="Q3"></a>Q3 | Real work | `bug` | 🔲 | S | do it. |
EOF
OLDOUT="$TMP/oldout"
expect_rc 1 "migrate: refuses a table carrying old-format states" \
    python3 "$Q" --store "$OLDOUT" migrate "$OLD"
if compgen -G "$OLDOUT/Q*.md" >/dev/null; then
    bad "migrate: wrote items despite refusing"
else
    ok "migrate: writes nothing when it refuses"
fi

# Rendering the store rebuilds the row it was imported from.
python3 "$Q" --store "$OUT" render --all --format table > "$TMP/rendered.md"
expect_in '| \[Q11\](Q11.md) | \[First thing\](\.\./plan/a.md) |' "$TMP/rendered.md" \
    "render: table form rebuilds the row, id linked to its page"

# The skill says to render the table on demand and never commit it, because a
# tracked copy is the one file every completing session edits. A subcommand
# that writes one is how that rule gets undone, so assert it stays gone.
if python3 "$Q" --store "$OUT" readme >/dev/null 2>&1; then
    bad "queue.py accepts 'readme' again; a committed index reintroduces the merge conflict"
else
    ok "queue.py has no index-writing subcommand, so the table stays on demand"
fi

# A browser sizes table columns by content, so one long cell claims the width
# and every other column wraps into a ribbon. The full text is on the page.
S="$TMP/longnote"
long="$(python3 -c 'print("Sentence one is short. " + "then a great deal more text " * 30)')"
item "$S" Q1 a0 ready "$long"
python3 "$Q" --store "$S" render --format table > "$TMP/wide.md"
cell_len=$(awk -F'|' '/^\| \[Q1\]/ {gsub(/^ +| +$/,"",$7); print length($7)}' "$TMP/wide.md")
if [[ "$cell_len" -le 160 ]]; then
    ok "render: a long note is summarized in the table ($cell_len chars)"
else
    bad "render: table cell is $cell_len chars, so one row sets every column width"
fi
if grep -q 'Sentence one is short\.' "$TMP/wide.md"; then
    ok "render: the summary keeps the first sentence"
else
    bad "render: the summary dropped the opening sentence"
fi
# The page itself must keep everything: truncating the source would be data loss.
page_len=$(python3 -c "import sys;print(len(' '.join(open(sys.argv[1]).read().split('---',2)[-1].split())))" "$S/Q1.md")
if [[ "$page_len" -gt 400 ]]; then
    ok "render: the item page keeps the full note ($page_len chars)"
else
    bad "render: the item page lost text ($page_len chars)"
fi
# On-demand rendering only works if two runs agree; a consumer regenerating the
# table into a job summary or a site build has nothing else to compare against.
if diff -q <(python3 "$Q" --store "$OUT" render --all --format table) \
        "$TMP/rendered.md" >/dev/null; then
    ok "render: the table is reproducible run to run"
else
    bad "render: the table is not reproducible, so no consumer can rely on it"
fi
expect_in 'escaped \\| pipe' "$TMP/rendered.md" "render: re-escapes a pipe in notes"

if (( fail )); then
    printf '\nqueue-test: FAILED\n'
    exit 1
fi
printf '\nqueue-test: all checks passed\n'
