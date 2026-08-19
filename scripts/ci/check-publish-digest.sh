#!/usr/bin/env bash
#
# check-publish-digest.sh — hold publish.yml's retried image build to the one
# digest it actually published (Q899).
#
# The release build is `no-cache: true`, so a second attempt rebuilds every
# layer and its index digest does NOT match the first attempt's. Four steps bind
# the release to that digest — the signed SLSA provenance subject, the per-arch
# SBOM resolution, the cosign sign/attest, and the run-summary pin operators
# copy into chart values — and a signature over the superseded index would be
# cryptographically valid over an image the tag no longer serves. Publish runs
# only on a v* tag, so no PR ever executes this job: a static gate is the only
# check that can fail before a release rather than during one.
#
# What this asserts:
#   1. Both attempts exist, the retry is gated on the first attempt's outcome,
#      and only the first attempt is `continue-on-error`. A retry that ran
#      unconditionally would republish every release; a second
#      `continue-on-error` would let a wholly failed build reach the sign steps.
#   2. The two attempts' `with:` blocks are identical. They are hand-kept copies,
#      so a build-arg or a `no-cache: true` changed on one side alone means the
#      retry publishes something the first attempt was not allowed to build.
#   3. No step outside the resolver reads an attempt's digest. This is the whole
#      point: with the reads centralized, a superseded digest has nowhere to
#      reach, and a new consumer copying the pattern from a sibling step copies
#      the resolved output because no other example remains in the file.
#   4. The resolver reads both attempts, and something downstream reads the
#      resolver. Either half missing is a gate comparing nothing.
#
# Usage:
#   check-publish-digest.sh [path/to/publish.yml]
#
# Exits 1 on a violation, and 2 when a subject the gate compares is absent.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

WORKFLOW="${1:-.github/workflows/publish.yml}"

# The step ids this gate is written against. Renaming one is a real change to
# the invariant, so the gate names them rather than discovering them.
FIRST_ID="build"
RETRY_ID="rebuild"
RESOLVER_ID="image"

if [[ ! -f "$WORKFLOW" ]]; then
	printf 'publish-digest: %s does not exist, so this gate would compare nothing\n' "$WORKFLOW" >&2
	exit 2
fi

# Print the `with:` block of the step carrying `id: <1>`, comments and blank
# lines dropped, so two hand-kept copies compare as the values they set.
step_with() {
	awk -v id="$1" '
		$0 ~ "^[[:space:]]+id:[[:space:]]*" id "[[:space:]]*$" { in_step = 1; next }
		!in_step { next }
		# A `with:` at step-key depth opens the block; the next key at that
		# depth, or the next step, closes both it and the step.
		/^[[:space:]]{8}with:[[:space:]]*$/ { in_with = 1; next }
		/^[[:space:]]{6}- / { exit }
		in_with && /^[[:space:]]{0,8}[^[:space:]]/ { exit }
		in_with {
			line = $0
			sub(/^[[:space:]]*#.*$/, "", line)
			if (line ~ /^[[:space:]]*$/) next
			print line
		}
	' "$WORKFLOW"
}

# Print every non-comment line of the step carrying `id: <1>`, from the id line
# to the start of the next step.
step_body() {
	awk -v id="$1" '
		$0 ~ "^[[:space:]]+id:[[:space:]]*" id "[[:space:]]*$" { in_step = 1; print; next }
		!in_step { next }
		/^[[:space:]]{6}- / { exit }
		!/^[[:space:]]*#/ { print }
	' "$WORKFLOW"
}

fail=0

# --- 1. the two attempts, and how each is gated ------------------------------
first_body="$(step_with "$FIRST_ID")"
retry_body="$(step_with "$RETRY_ID")"
if [[ -z "$first_body" ]]; then
	printf 'publish-digest: no step with id: %s and a with: block in %s\n' "$FIRST_ID" "$WORKFLOW" >&2
	exit 2
fi
if [[ -z "$retry_body" ]]; then
	printf 'publish-digest: no step with id: %s and a with: block in %s\n' "$RETRY_ID" "$WORKFLOW" >&2
	printf 'the build retry is what Q899 added; without it there is no second digest to get wrong\n' >&2
	exit 2
fi

first_full="$(step_body "$FIRST_ID")"
retry_full="$(step_body "$RETRY_ID")"

if ! grep -q '^[[:space:]]*continue-on-error:[[:space:]]*true[[:space:]]*$' <<<"$first_full"; then
	printf 'publish-digest: the %s attempt is not continue-on-error: true, so the retry below it can never run\n' "$FIRST_ID" >&2
	fail=1
fi
if grep -q '^[[:space:]]*continue-on-error:' <<<"$retry_full"; then
	printf 'publish-digest: the %s retry is continue-on-error, so a build that failed twice would still reach the sign steps\n' "$RETRY_ID" >&2
	fail=1
fi
if ! grep -qF "if: steps.${FIRST_ID}.outcome == 'failure'" <<<"$retry_full"; then
	printf 'publish-digest: the %s retry is not gated on steps.%s.outcome, so it would republish every release\n' \
		"$RETRY_ID" "$FIRST_ID" >&2
	fail=1
fi

# --- 2. the attempts build the same thing ------------------------------------
if [[ "$first_body" != "$retry_body" ]]; then
	printf 'publish-digest: the %s and %s attempts set different with: values\n' "$FIRST_ID" "$RETRY_ID" >&2
	printf 'they are hand-kept copies: the retry must build exactly what the first attempt was building\n' >&2
	diff <(printf '%s\n' "$first_body") <(printf '%s\n' "$retry_body") >&2 || true
	fail=1
fi
if ! grep -q '^[[:space:]]*no-cache:[[:space:]]*true[[:space:]]*$' <<<"$retry_body"; then
	printf 'publish-digest: the %s retry is not no-cache: true (Q127): a cached retry can serve PR-populated layers into a signed release\n' \
		"$RETRY_ID" >&2
	fail=1
fi

# --- 3./4. one reader of the attempts, and it is the resolver ----------------
resolver_reads=0
for attempt in "$FIRST_ID" "$RETRY_ID"; do
	# Every line reading this attempt's digest, wherever it sits in the file.
	mapfile -t reads < <(grep -n "steps\.${attempt}\.outputs\.digest" "$WORKFLOW" | grep -v '^[0-9]*:[[:space:]]*#' || true)
	if ((${#reads[@]} == 0)); then
		printf 'publish-digest: nothing reads steps.%s.outputs.digest, so the resolver is not resolving it\n' "$attempt" >&2
		fail=1
		continue
	fi
	for read_line in "${reads[@]}"; do
		# The resolver owns them all, and it reads them through `env:` so the
		# expression never lands in a `run:` body.
		if [[ "$read_line" =~ _DIGEST:[[:space:]]*\$\{\{[[:space:]]*steps\.${attempt}\.outputs\.digest ]]; then
			resolver_reads=$((resolver_reads + 1))
			continue
		fi
		printf 'publish-digest: %s reads an attempt digest outside the resolver:\n  %s\n' "$WORKFLOW" "$read_line" >&2
		printf 'a retried build supersedes that digest; read steps.%s.outputs.digest instead\n' "$RESOLVER_ID" >&2
		fail=1
	done
done
if ((resolver_reads < 2)); then
	printf 'publish-digest: the resolver reads %d attempt digest(s), expected both\n' "$resolver_reads" >&2
	fail=1
fi

consumers="$(grep -c "steps\.${RESOLVER_ID}\.outputs\.digest" "$WORKFLOW" || true)"
if ((consumers == 0)); then
	printf 'publish-digest: nothing reads steps.%s.outputs.digest, so this gate would be checking an unused step\n' "$RESOLVER_ID" >&2
	exit 2
fi

if ((fail)); then
	exit 1
fi

printf 'publish-digest: ok (%s: 2 attempts kept in step, %d consumer(s) bound to steps.%s.outputs.digest)\n' \
	"$WORKFLOW" "$consumers" "$RESOLVER_ID"
