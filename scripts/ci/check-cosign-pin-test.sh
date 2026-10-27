#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-cosign-pin.sh (Q903, Q1166): drift between
# the Makefile's COSIGN_VERSION and scripts/ci/pins.env's fails, agreement
# passes, an installer step whose cosign-release is not the pins reference fails
# (a literal, or no input at all), a v3 signing call without the v2-format
# flags fails (Q1218), and every shape that would leave the gate comparing
# nothing refuses with rc 2 instead of reporting green.
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

# The literal workflow text, unexpanded.
# shellcheck disable=SC2016
REF='${{ steps.pins.outputs.COSIGN_VERSION }}'

# write_pins NAME LINE... — a pins.env fixture with a neighbouring pin, so the
# awk selection is exercised against more than a lone line.
write_pins() {
	local out="$FIXTURE_DIR/pins.$1.env" line
	shift
	{
		printf 'KIND_VERSION=v0.32.0\n'
		for line in "$@"; do
			printf '%s\n' "$line"
		done
	} > "$out"
	printf '%s\n' "$out"
}

# write_workflow NAME STEP... — a publish.yml fixture, one argument per
# installer step, each either the cosign-release value or the literal "unpinned".
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

# expect NAME EXPECT_RC MAKEFILE WORKFLOW PINS [SUBSTRING] — run the gate over
# the fixtures and assert the exit code, and that any SUBSTRING is reported.
expect() {
	local name="$1" want_rc="$2" makefile="$3" workflow="$4" pins="$5" want_text="${6:-}"
	local got_rc=0 out
	out="$("$CHECKER" "$makefile" "$workflow" "$pins" 2>&1)" || got_rc=$?
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
pins_in_step="$(write_pins in-step 'COSIGN_VERSION=v2.5.2')"
wf_in_step="$(write_workflow in-step "$REF" "$REF")"

# --- the comparison itself --------------------------------------------------

expect in-step 0 "$mk_in_step" "$wf_in_step" "$pins_in_step" 'v2.5.2'

# The defect Q903 names: the workflow signs with one cosign, `make
# verify-release` verifies with another.
expect pins-ahead-of-makefile 1 "$mk_in_step" "$wf_in_step" \
	"$(write_pins ahead 'COSIGN_VERSION=v2.6.0')" 'v2.6.0'
expect makefile-ahead-of-pins 1 "$(write_makefile ahead 'COSIGN_VERSION ?= v2.6.0')" \
	"$wf_in_step" "$pins_in_step" 'v2.6.0'

# A literal stops moving with the pin, so even one that agrees today is refused:
# the next bump would leave it behind.
wf="$(write_workflow literal-agrees "$REF" v2.5.2)"
expect literal-version-in-workflow 1 "$mk_in_step" "$wf" "$pins_in_step" 'must read'
# shellcheck disable=SC2016
wf="$(write_workflow env-ref "$REF" '${{ env.COSIGN_VERSION }}')"
expect env-reference-in-workflow 1 "$mk_in_step" "$wf" "$pins_in_step" 'must read'

# --- drift arriving by omission ---------------------------------------------
#
# cosign-installer floats to the latest release with no cosign-release input, so
# a deleted input diverges exactly like an edited one. Comparing only the inputs
# that remain would report this tree in step.
wf="$(write_workflow half-pinned "$REF" unpinned)"
expect installer-lost-its-pin 1 "$mk_in_step" "$wf" "$pins_in_step" 'cosign-release input'

wf="$(write_workflow unpinned unpinned unpinned)"
expect no-pins-at-all 1 "$mk_in_step" "$wf" "$pins_in_step" 'cosign-release input'

# --- shapes that must refuse rather than pass -------------------------------
#
# Each of these leaves the gate with nothing to compare. Reporting green would
# be indistinguishable from a tree that is genuinely in step, which is the
# failure mode the gate exists to remove.
expect makefile-pin-gone 2 "$(write_makefile no-pin 'COSIGN         := unrelated')" \
	"$wf_in_step" "$pins_in_step" 'COSIGN_VERSION'
expect makefile-pin-twice 2 \
	"$(write_makefile two-pins 'COSIGN_VERSION ?= v2.5.2' 'COSIGN_VERSION ?= v2.6.0')" \
	"$wf_in_step" "$pins_in_step" 'COSIGN_VERSION'
expect pins-env-pin-gone 2 "$mk_in_step" "$wf_in_step" "$(write_pins no-pin)" 'COSIGN_VERSION='
expect pins-env-pin-twice 2 "$mk_in_step" "$wf_in_step" \
	"$(write_pins two-pins 'COSIGN_VERSION=v2.5.2' 'COSIGN_VERSION=v2.6.0')" 'COSIGN_VERSION='

printf 'jobs:\n  publish:\n    steps:\n      - run: echo no cosign here\n' > "$FIXTURE_DIR/nocosign.yml"
expect workflow-installs-nothing 2 "$mk_in_step" "$FIXTURE_DIR/nocosign.yml" "$pins_in_step" 'installs cosign nowhere'

expect makefile-missing 2 "$FIXTURE_DIR/no-such-Makefile" "$wf_in_step" "$pins_in_step" 'does not exist'
expect workflow-missing 2 "$mk_in_step" "$FIXTURE_DIR/no-such.yml" "$pins_in_step" 'does not exist'
expect pins-env-missing 2 "$mk_in_step" "$wf_in_step" "$FIXTURE_DIR/no-such.env" 'does not exist'

# --- the v2 signing format, from cosign v3 (Q1218) --------------------------
#
# write_signing_workflow NAME LINE... — one pinned installer step and a run
# block holding each LINE verbatim, so wrapped calls and comments can be built.
write_signing_workflow() {
	local out="$FIXTURE_DIR/publish.$1.yml" line
	shift
	{
		printf 'jobs:\n  publish:\n    steps:\n'
		printf '      - name: Install cosign\n'
		printf '        uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2\n'
		printf '        with:\n          cosign-release: %s\n' "$REF"
		printf '      - run: |\n'
		for line in "$@"; do
			printf '          %s\n' "$line"
		done
	} > "$out"
	printf '%s\n' "$out"
}

mk_v3="$(write_makefile v3 'COSIGN_VERSION ?= v3.1.4')"
pins_v3="$(write_pins v3 'COSIGN_VERSION=v3.1.4')"
F='--new-bundle-format=false --use-signing-config=false'

# The fixture lines are literal workflow text: a trailing backslash and an
# unexpanded ${IMAGE} are the point.
# shellcheck disable=SC1003,SC2016
wf="$(write_signing_workflow v3-flagged \
	"retry.sh cosign sign --yes $F --recursive \"\${IMAGE}\"" \
	'retry.sh cosign attest --yes --type spdxjson \' \
	"  $F --predicate sbom.json \"\${IMAGE}\"" \
	"retry.sh cosign sign-blob --yes $F \\" \
	'  --bundle out.bundle out' \
	'# `cosign sign` fails with UNAUTHORIZED without a docker login')"
expect v3-every-call-flagged 0 "$mk_v3" "$wf" "$pins_v3" '3 v2-format signing call(s)'

wf="$(write_signing_workflow v3-half-flagged \
	"retry.sh cosign sign --yes $F \"\${ref}\"" \
	'retry.sh cosign sign-blob --yes --new-bundle-format=false --bundle b out')"
expect v3-call-missing-one-flag 1 "$mk_v3" "$wf" "$pins_v3" '--use-signing-config=false'

# shellcheck disable=SC1003,SC2016
wf="$(write_signing_workflow v3-wrapped-unflagged \
	"retry.sh cosign sign --yes $F \"\${ref}\"" \
	'retry.sh cosign attest --yes --type spdxjson \' \
	'  --predicate sbom.json "${IMAGE}"')"
expect v3-wrapped-call-unflagged 1 "$mk_v3" "$wf" "$pins_v3" 'cosign attest --yes --type spdxjson'

wf="$(write_signing_workflow v3-no-calls 'echo nothing signed')"
expect v3-signs-nothing 2 "$mk_v3" "$wf" "$pins_v3" 'compared nothing'

# cosign v2 rejects both flags as unknown, so an unflagged v2 signer is correct.
# shellcheck disable=SC2016
wf="$(write_signing_workflow v2-unflagged 'retry.sh cosign sign --yes "${ref}"')"
expect v2-unflagged-call 0 "$mk_in_step" "$wf" "$pins_in_step" '0 v2-format signing call(s)'

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
