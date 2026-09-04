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
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
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
	die_if_killed "$name" "$rc" "$want"
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

# The parser against a GNU make 4.x database. This box has only 3.81, which
# prints a target-specific variable as a comment; 4.x prints it in rule
# position, and read naively that makes `TOOL_PKG`, `:=` and the package path
# three prerequisites of .build/mdreflow. Every Go tool rule failed that way on
# CI run 32275796483 while this same suite passed locally, so the shape is
# asserted here rather than left to CI.
#
# Reconstructed from that run's output, not captured: no make 4.x is available
# here and installing one is not this gate's business. It therefore pins the
# defect CI found and cannot vouch for the rest of 4.x's formatting, which is
# why the gate also runs in CI.
db="$FIXTURE_DIR/make4.db"
{
	printf '# Files\n'
	printf '\n'
	printf '%s/.build/mdreflow: TOOL_PKG := github.com/jbeda/mdreflow/cmd/mdreflow\n' "$REPO_ROOT"
	printf '#  Implicit rule search has not been done.\n'
	printf '\n'
	printf '%s/.build/mdreflow: %s/tools/go.mod %s/tools/vendor/modules.txt\n' \
		"$REPO_ROOT" "$REPO_ROOT" "$REPO_ROOT"
	printf '#  Implicit rule search has not been done.\n'
} > "$db"

rules="$("$CHECKER" --database "$db" --print-rules)"
if grep -qE 'TOOL_PKG|:=' <<<"$rules"; then
	printf 'FAIL: make4-parse — a target-specific variable was read as prerequisites\n' >&2
	printf '%s\n' "$rules" | sed 's/^/       /' >&2
	((fails++)) || true
elif [[ "$rules" != ".build/mdreflow|tools/go.mod tools/vendor/modules.txt" ]]; then
	printf 'FAIL: make4-parse — expected the real rule alone, got:\n' >&2
	printf '%s\n' "$rules" | sed 's/^/       /' >&2
	((fails++)) || true
else
	printf 'ok: make4-parse — a make 4.x target-specific variable is not read as prerequisites\n'
fi

rc=0
"$CHECKER" --database "$db" > /dev/null 2>&1 || rc=$?
die_if_killed print-rules-guard "$rc" 2
if ((rc != 2)); then
	printf 'FAIL: print-rules-guard — --database without --print-rules: expected rc 2, got %d\n' "$rc" >&2
	((fails++)) || true
else
	printf 'ok: print-rules-guard — --database alone is not a check (rc 2)\n'
fi

rc=0
"$CHECKER" --makefile "$REPO_ROOT/Makefile" --nonsense > /dev/null 2>&1 || rc=$?
die_if_killed unknown-arg "$rc" 2
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
