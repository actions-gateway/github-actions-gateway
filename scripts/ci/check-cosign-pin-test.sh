#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-cosign-pin.sh (Q903): drift between the
# Makefile's COSIGN_VERSION and publish.yml's cosign-release fails, agreement
# passes, an installer step that lost its pin fails, and every shape that would
# leave the gate comparing nothing refuses with rc 2 instead of reporting green.
#
# Both directions are asserted because the gate's own failure mode is silence:
# a pattern that stopped matching would find no pins to compare and — without
# the rc-2 refusals below — call every tree in step.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-cosign-pin.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/cosign-pin-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# write_makefile NAME VERSION_LINES... — a Makefile fixture with the surrounding
# variables the real one carries, so the awk selection is exercised against
# neighbours rather than a lone line. Each fixture gets its own path: the
# in-step Makefile is reused by later cases, and a shared one would be rewritten
# under them.
write_makefile() {
	local out="$FIXTURE_DIR/Makefile.$1"
	shift
	{
		printf 'COSIGN         := .build/cosign\n'
		local line
		for line in "$@"; do
			printf '%s\n' "$line"
		done
		printf 'KIND_CLUSTER  ?= actions-gateway-e2e\n'
	} > "$out"
	printf '%s\n' "$out"
}

# write_workflow NAME STEP... — a publish.yml fixture, one argument per
# installer step, each either a version to pin or the literal "unpinned".
write_workflow() {
	local out="$FIXTURE_DIR/publish.$1.yml" step
	shift
	{
		printf 'jobs:\n  publish:\n    steps:\n'
		for step in "$@"; do
			printf '      - name: Install cosign\n'
			printf '        uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2\n'
			if [[ "$step" != unpinned ]]; then
				printf '        with:\n          cosign-release: %s\n' "$step"
			fi
		done
	} > "$out"
	printf '%s\n' "$out"
}

# expect NAME EXPECT_RC MAKEFILE WORKFLOW [SUBSTRING] — run the gate over the
# fixtures and assert the exit code, and that any SUBSTRING is reported.
expect() {
	local name="$1" want_rc="$2" makefile="$3" workflow="$4" want_text="${5:-}"
	local got_rc=0 out
	out="$("$CHECKER" "$makefile" "$workflow" 2>&1)" || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" != "$want_rc" ]]; then
		printf 'FAIL %-30s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$out" >&2
		fails=$((fails + 1))
		return
	fi
	if [[ -n "$want_text" && "$out" != *"$want_text"* ]]; then
		printf 'FAIL %-30s output does not mention %s\n%s\n' "$name" "$want_text" "$out" >&2
		fails=$((fails + 1))
		return
	fi
	printf 'ok   %-30s rc=%s\n' "$name" "$got_rc"
}

mk_in_step="$(write_makefile in-step 'COSIGN_VERSION ?= v2.5.2')"

# --- the comparison itself --------------------------------------------------

wf="$(write_workflow in-step v2.5.2 v2.5.2)"
expect in-step 0 "$mk_in_step" "$wf" 'v2.5.2'

# The defect Q903 names: the workflow signs with one cosign, `make
# verify-release` verifies with another.
wf="$(write_workflow drift-both v2.6.0 v2.6.0)"
expect drift-both-pins 1 "$mk_in_step" "$wf" 'v2.6.0'

# A partial bump is the likelier shape — two installer steps, one edited.
wf="$(write_workflow drift-one v2.5.2 v2.6.0)"
expect drift-one-pin 1 "$mk_in_step" "$wf" 'v2.6.0'

# --- drift arriving by omission ---------------------------------------------
#
# cosign-installer floats to the latest release with no cosign-release input, so
# a deleted pin diverges exactly like an edited one. Comparing only the pins
# that remain would report this tree in step.
wf="$(write_workflow half-pinned v2.5.2 unpinned)"
expect installer-lost-its-pin 1 "$mk_in_step" "$wf" 'cosign-release pin'

wf="$(write_workflow unpinned unpinned unpinned)"
expect no-pins-at-all 1 "$mk_in_step" "$wf" 'cosign-release pin'

# --- shapes that must refuse rather than pass -------------------------------
#
# Each of these leaves the gate with nothing to compare. Reporting green would
# be indistinguishable from a tree that is genuinely in step, which is the
# failure mode the gate exists to remove.
wf="$(write_workflow refusals v2.5.2 v2.5.2)"

expect makefile-pin-gone 2 "$(write_makefile no-pin 'COSIGN         := unrelated')" "$wf" 'COSIGN_VERSION'
expect makefile-pin-twice 2 \
	"$(write_makefile two-pins 'COSIGN_VERSION ?= v2.5.2' 'COSIGN_VERSION ?= v2.6.0')" "$wf" 'COSIGN_VERSION'

printf 'jobs:\n  publish:\n    steps:\n      - run: echo no cosign here\n' > "$FIXTURE_DIR/nocosign.yml"
expect workflow-installs-nothing 2 "$mk_in_step" "$FIXTURE_DIR/nocosign.yml" 'installs cosign nowhere'

expect makefile-missing 2 "$FIXTURE_DIR/no-such-Makefile" "$wf" 'does not exist'
expect workflow-missing 2 "$mk_in_step" "$FIXTURE_DIR/no-such.yml" 'does not exist'

# --- the tracked tree -------------------------------------------------------
#
# The default arguments are the real pins, so this is the gate as `make check`
# runs it.
rc=0
out="$("$CHECKER" 2>&1)" || rc=$?
die_if_killed tracked-tree-in-step "$rc"
if ((rc == 0)); then
	printf 'ok   %-30s rc=0\n' tracked-tree-in-step
else
	printf 'FAIL %-30s the tracked tree is out of step\n%s\n' tracked-tree-in-step "$out" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\ncheck-cosign-pin-test: %d failure(s)\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-cosign-pin-test: all checks passed\n'
