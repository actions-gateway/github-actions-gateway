#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-fixture-maintenance.sh — the gate holding every
# scripts/ suite to "a fixture repo must not run background git" (Q820, Q921).
#
# Both directions, because a gate that has stopped matching fails exactly as
# silently as one that matches everything: this one reads traces, and an empty
# read is indistinguishable from a clean tier unless something demands evidence.
# So each green case is paired with the red one that proves the assertion is
# live, and the three refusals are asserted as rc 2 rather than as a pass.
#
# The traces are not hand-written. Every fixture here comes from a real git
# command under a real GIT_TRACE, so a change in git's trace vocabulary reddens
# this suite instead of quietly turning the gate into a no-op — which is the
# failure mode a canned string would hide (verify-claims: an instrument whose
# question has drifted from the claim).
#
# Runs under `make scripts-test` and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
GATE="$REPO_ROOT/scripts/ci/check-fixture-maintenance.sh"

WORK="$REPO_ROOT/tmp/check-fixture-maintenance-test.$$"
rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

fails=0
out=""
rc=0

# run DIR — invoke the gate, capturing merged output and exit code.
run() {
	rc=0
	out="$("$GATE" "$@" 2>&1)" || rc=$?
}

want_rc() {
	local name="$1" want="$2"
	die_if_killed "$name" "$rc" "$want"
	if [[ "$rc" == "$want" ]]; then
		printf 'ok   %-52s rc=%s\n' "$name" "$rc"
	else
		printf 'FAIL %-52s want rc=%s got rc=%s\n%s\n' "$name" "$want" "$rc" "$out" >&2
		fails=$((fails + 1))
	fi
}

want() {
	local name="$1" pattern="$2"
	if grep -Eq -- "$pattern" <<<"$out"; then
		printf 'ok   %-52s\n' "$name"
	else
		printf 'FAIL %-52s no match for /%s/\n%s\n' "$name" "$pattern" "$out" >&2
		fails=$((fails + 1))
	fi
}

# trace_of REPO_DIR OUT [spawning] — commit once under GIT_TRACE and leave the
# trace at OUT. `spawning` re-enables maintenance.auto for that ONE command,
# which is how the dirty fixture is made dirty.
#
# This suite is the only place in the repo that must spawn background git, since
# a control that cannot spawn cannot prove the gate sees one. That makes it the
# one suite at risk of tripping the tier gate it belongs to, so the spawn is kept
# as narrow as it can be: the repo itself keeps `maintenance.auto false`, so the
# seed commit and every other git here stay silent, and only this command, under
# its own GIT_TRACE, reaches the trace the gate reads.
#
# A blanket redirect of the suite's GIT_TRACE would also have silenced it, and
# would have exempted every future git added here along with it. Q878 set the
# same precedent, re-enabling per command to verify each assertion could fail.
trace_of() {
	local repo="$1" out_file="$2"
	local -a cfg=()
	if [[ "${3:-}" == "spawning" ]]; then
		cfg=(-c maintenance.auto=true)
	fi
	git -C "$repo" add -A
	GIT_TRACE="$out_file" git -C "$repo" "${cfg[@]+"${cfg[@]}"}" commit -qm "next-$RANDOM"
}

# new_repo DIR — a fixture repo with one commit already in it, and no background
# maintenance: the seed commit runs under the tier's inherited GIT_TRACE, so a
# spawn here would be filed against this suite by the very gate it tests.
new_repo() {
	local dir="$1"
	mkdir -p "$dir"
	git -C "$dir" init -q
	git -C "$dir" config maintenance.auto false
	git -C "$dir" config user.email t@example.com
	git -C "$dir" config user.name t
	printf 'seed\n' >"$dir/f"
	git -C "$dir" add -A
	git -C "$dir" commit -qm seed >/dev/null 2>&1
}

# --- The two fixtures the whole suite rests on -------------------------------
#
# Built first and asserted against each other: if the "dirty" one carried no
# spawn, every red case below would pass for the wrong reason.

DIRTY_REPO="$WORK/dirty-repo"
CLEAN_REPO="$WORK/clean-repo"
new_repo "$DIRTY_REPO"
new_repo "$CLEAN_REPO"
printf 'x\n' >>"$DIRTY_REPO/f"
printf 'x\n' >>"$CLEAN_REPO/f"
DIRTY_TRACE="$WORK/dirty.trace"
CLEAN_TRACE="$WORK/clean.trace"
trace_of "$DIRTY_REPO" "$DIRTY_TRACE" spawning
trace_of "$CLEAN_REPO" "$CLEAN_TRACE"

if grep -qF 'run_command: git maintenance run' "$DIRTY_TRACE"; then
	printf 'ok   %-52s\n' 'fixture: an unset repo really does spawn maintenance'
else
	printf 'FAIL %-52s no spawn in the dirty fixture; every red case below is vacuous\n' \
		'fixture: an unset repo really does spawn maintenance' >&2
	fails=$((fails + 1))
fi
if grep -qF 'run_command: git maintenance run' "$CLEAN_TRACE"; then
	printf 'FAIL %-52s the clean fixture spawned maintenance\n' \
		'fixture: maintenance.auto false really suppresses it' >&2
	fails=$((fails + 1))
else
	printf 'ok   %-52s\n' 'fixture: maintenance.auto false really suppresses it'
fi

# --- Green: a tier where no fixture spawned anything -------------------------

GREEN="$WORK/green"
mkdir -p "$GREEN"
cp "$CLEAN_TRACE" "$GREEN/suite-a.trace"
cp "$CLEAN_TRACE" "$GREEN/suite-b.trace"
run "$GREEN"
want_rc 'a clean tier passes' 0
want 'a clean tier reports what it read' 'across 2 suite\(s\), 2 of which ran git'

# --- Red: one suite spawned, and the finding names it ------------------------

RED="$WORK/red"
mkdir -p "$RED"
cp "$CLEAN_TRACE" "$RED/suite-a.trace"
cp "$DIRTY_TRACE" "$RED/offending-suite.trace"
run "$RED"
want_rc 'one spawning suite fails the tier' 1
want 'the finding names the offending suite' 'offending-suite \(1 spawn\(s\)\)'
want 'the finding names the fix' 'maintenance\.auto false'
if grep -Eq 'suite-a \([0-9]+ spawn' <<<"$out"; then
	printf 'FAIL %-52s a clean sibling was reported as an offender\n%s\n' \
		'a clean sibling is not reported' "$out" >&2
	fails=$((fails + 1))
else
	printf 'ok   %-52s\n' 'a clean sibling is not reported'
fi

# The count is spawns, not trace lines: each spawn writes three of them, so a
# gate counting the wrong line would report 3 here and read as three defects.
MULTI="$WORK/multi"
mkdir -p "$MULTI"
cat "$DIRTY_TRACE" "$DIRTY_TRACE" >"$MULTI/twice.trace"
run "$MULTI"
want_rc 'two spawns still fail' 1
want 'the count is spawns, not trace lines' 'twice \(2 spawn\(s\)\)'

# --- Refusals: evidence that cannot bear a verdict ---------------------------
#
# Each of these is a shape where a naive loop reports zero spawns and exits 0 —
# the wiring coming undone, in three different places.

run "$WORK/absent"
want_rc 'a missing trace dir refuses, never passes' 2
want 'the refusal names the wiring' 'RUN_PARALLEL_GIT_TRACE_DIR'

EMPTY="$WORK/empty"
mkdir -p "$EMPTY"
run "$EMPTY"
want_rc 'a trace dir with no traces refuses' 2
want 'the refusal says no child ran under GIT_TRACE' 'no \*\.trace file'

# A trace file that exists but recorded no git at all: GIT_TRACE reached the
# child and the child never ran git, which cannot distinguish a clean tier from
# a trace variable that git rejected.
NOGIT="$WORK/nogit"
mkdir -p "$NOGIT"
printf 'not a git trace\n' >"$NOGIT/suite-a.trace"
run "$NOGIT"
want_rc 'traces recording no git refuse' 2
want 'the refusal says GIT_TRACE reached no git' 'GIT_TRACE reached no git'

# --- Usage -------------------------------------------------------------------

run
want_rc 'no argument refuses' 2
run "$GREEN" extra
want_rc 'a second argument refuses' 2

if (( fails > 0 )); then
	echo "check-fixture-maintenance-test: $fails failure(s)" >&2
	exit 1
fi
echo "check-fixture-maintenance-test: all assertions passed"
