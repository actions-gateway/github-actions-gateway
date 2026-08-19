#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-tool-pins.sh (Q904): a `.build/` tool rule
# with no version-pin prerequisite fails, the two shapes that carry a
# prerequisite which cannot fire still fail, the two that can fire pass, and
# every read that would leave the gate checking nothing refuses with rc 2.
#
# Both directions are asserted because the gate's own failure mode is silence.
# It derives its rules from `make -pnq`, and a database it could not parse
# yields no tool rules at all — which, without the rc-2 refusals below, is
# indistinguishable from a tree where every rule is correctly pinned. The
# order-only and stamp cases are here for the same reason one rung down: both
# satisfy "declares a prerequisite" while leaving the Q857 defect in place, so a
# gate that only counted prerequisites would pass them.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# File-wide: every fixture line is make source text, so a `$(...)` in one must
# reach the makefile unexpanded — single quotes are the point, not an oversight.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-tool-pins.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/tool-pins-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# write_makefile NAME LINE... — a makefile fixture. Every fixture gets its own
# path so a later case cannot rewrite one an earlier case still names.
write_makefile() {
	local out="$FIXTURE_DIR/mk.$1"
	shift
	local line
	: > "$out"
	for line in "$@"; do
		printf '%s\n' "$line" >> "$out"
	done
	printf '%s\n' "$out"
}

# expect NAME WANT_RC DESCRIPTION MAKEFILE — run the gate and compare rc.
expect() {
	local name="$1" want="$2" desc="$3" mk="$4" rc=0 out
	out="$("$CHECKER" --makefile "$mk" 2>&1)" || rc=$?
	if ((rc != want)); then
		printf 'FAIL: %s — %s: expected rc %d, got %d\n' "$name" "$desc" "$want" "$rc" >&2
		printf '%s\n' "$out" | sed 's/^/       /' >&2
		((fails++)) || true
		return
	fi
	printf 'ok: %s — %s (rc %d)\n' "$name" "$desc" "$rc"
}

# The live Makefile is the gate's real subject, so it is asserted directly
# rather than only through fixtures: a rule shape the tree adopts that the
# parser cannot read would otherwise surface as a green fixture run.
expect real 0 'the tracked Makefile pins every .build/ tool rule' "$REPO_ROOT/Makefile"

# The Q857 shape: no prerequisite at all, so make takes an existing binary as up
# to date forever and a bump keeps serving the old tool.
expect no-prereq 1 'a tool rule with no prerequisite fails' \
	"$(write_makefile no-prereq \
		'TRIVY := .build/trivy' \
		'$(TRIVY):' \
		'	touch $@')"

# Order-only prerequisites never make a target out of date when they are newer,
# so this is the same defect wearing a prerequisite.
expect order-only 1 'a tool rule with only order-only prerequisites fails' \
	"$(write_makefile order-only \
		'TRIVY := .build/trivy' \
		'$(TRIVY): | tools/go.mod' \
		'	touch $@')"

# A sentinel touched once and never again: present, untracked, and unmoved by a
# bump. Counting prerequisites would pass it.
expect stamp 1 'a tool rule keyed on an unversioned stamp fails' \
	"$(write_makefile stamp \
		'TRIVY := .build/trivy' \
		'$(TRIVY): .build/trivy.stamp' \
		'	touch $@' \
		'.build/trivy.stamp:' \
		'	touch $@')"

# The shape the vendored Go tools use: a tracked manifest that moves on a bump.
expect tracked 0 'a tool rule keyed on a tracked manifest passes' \
	"$(write_makefile tracked \
		'TRIVY := .build/trivy' \
		'$(TRIVY): tools/go.mod' \
		'	touch $@')"

# The shape cosign uses since Q857: a sentinel whose name carries the resolved
# version, so the rule fires on a change and only on a change. The sentinel is
# itself a prerequisite, so the gate must not turn round and report it as a tool
# rule with no prerequisite of its own.
expect version-keyed 0 'a tool rule keyed on a version-named sentinel passes' \
	"$(write_makefile version-keyed \
		'TRIVY_VERSION ?= v0.1.0' \
		'TRIVY := .build/trivy' \
		'TRIVY_PIN := .build/trivy-$(TRIVY_VERSION).pin' \
		'$(TRIVY): $(TRIVY_PIN)' \
		'	touch $@' \
		'$(TRIVY_PIN):' \
		'	touch $@')"

# Two tools in one makefile, one pinned and one not: the pinned one must not
# stand in for the other. A gate reporting per file rather than per rule would
# pass this.
expect mixed 1 'a pinned rule does not cover an unpinned one beside it' \
	"$(write_makefile mixed \
		'TRIVY := .build/trivy' \
		'SYFT := .build/syft' \
		'$(TRIVY): tools/go.mod' \
		'	touch $@' \
		'$(SYFT):' \
		'	touch $@')"

# Refusals. Each is a read that leaves nothing to compare, and reporting green
# from one is the failure this suite exists to make impossible.
expect no-build-rule 2 'a makefile declaring no .build/ rule refuses' \
	"$(write_makefile no-build-rule \
		'all:' \
		'	@true')"

expect missing 2 'a makefile that does not exist refuses' "$FIXTURE_DIR/absent.mk"

# The one that matters most: make itself cannot read the file, so the database
# is empty. Left unrefused this reports as a tree where every rule is pinned.
expect unparseable 2 'a makefile make cannot parse refuses' \
	"$(write_makefile unparseable \
		'TRIVY := .build/trivy' \
		'ifeq (1,1)' \
		'$(TRIVY):' \
		'	touch $@')"

rc=0
"$CHECKER" --makefile "$REPO_ROOT/Makefile" --nonsense > /dev/null 2>&1 || rc=$?
if ((rc != 2)); then
	printf 'FAIL: unknown-arg — an unrecognized argument: expected rc 2, got %d\n' "$rc" >&2
	((fails++)) || true
else
	printf 'ok: unknown-arg — an unrecognized argument refuses (rc 2)\n'
fi

if ((fails > 0)); then
	printf '\n%d check-tool-pins assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-tool-pins: all assertions passed\n'
