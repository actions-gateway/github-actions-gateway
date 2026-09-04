#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-publish-digest.sh (Q899): a release build
# whose retry can supersede the digest a later step signs must fail, and the
# tracked publish.yml must pass.
#
# Both directions are asserted because the gate's own failure mode is silence.
# Every rule here is a grep over a workflow: a pattern that stopped matching
# would find no violation and call a tree that signs the wrong image clean.
# Every shape that would leave the gate comparing nothing therefore refuses with
# rc 2 rather than reporting green, and every rule is exercised against a
# fixture that violates it alone.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/check-publish-digest.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/publish-digest-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

fails=0

# write_workflow NAME [MUTATION...] — a publish.yml-shaped fixture carrying the
# real file's step layout (a retried build, a resolver, and consumers binding to
# it). With no mutation it is the compliant shape, so each case below differs
# from a passing tree by exactly the rule it exercises.
# The fixtures emit GitHub Actions `${{ ... }}` expressions literally, which is
# exactly what the single quotes are for -- the gate greps for them as text.
# shellcheck disable=SC2016
write_workflow() {
	local out="$FIXTURE_DIR/publish.$1.yml"
	shift
	local m
	local first_coe=1 retry=1 retry_if=1 retry_coe=0 retry_extra=0
	local retry_cache=1 leaky=0 resolver_first=1 consumers=1
	for m in "$@"; do
		case "$m" in
		no-first-coe) first_coe=0 ;;
		no-retry) retry=0 ;;
		no-retry-if) retry_if=0 ;;
		retry-coe) retry_coe=1 ;;
		divergent-with) retry_extra=1 ;;
		retry-cached) retry_cache=0 ;;
		leaky-consumer) leaky=1 ;;
		no-resolver-first) resolver_first=0 ;;
		no-consumers) consumers=0 ;;
		*)
			printf 'unknown mutation %s\n' "$m" >&2
			exit 2
			;;
		esac
	done
	{
		printf 'jobs:\n  publish:\n    steps:\n'
		printf '      - name: Build and push\n        id: build\n'
		((first_coe)) && printf '        continue-on-error: true\n'
		printf '        uses: docker/build-push-action@abc # v7.3.0\n'
		printf '        with:\n'
		printf '          # a comment, which is not part of the comparison\n'
		printf '          context: .\n          push: true\n          no-cache: true\n'
		if ((retry)); then
			printf "      - name: Back off\n        if: steps.build.outcome == 'failure'\n"
			printf '        run: sleep 30\n'
			printf '      - name: Rebuild and push\n        id: rebuild\n'
			((retry_if)) && printf "        if: steps.build.outcome == 'failure'\n"
			((retry_coe)) && printf '        continue-on-error: true\n'
			printf '        uses: docker/build-push-action@abc # v7.3.0\n'
			printf '        with:\n          context: .\n          push: true\n'
			((retry_cache)) && printf '          no-cache: true\n'
			((retry_extra)) && printf '          build-args: |\n            DRIFT=1\n'
		fi
		printf '      - name: Resolve the published index digest\n        id: image\n'
		printf '        env:\n'
		((resolver_first)) && printf '          FIRST_DIGEST: ${{ steps.build.outputs.digest }}\n'
		printf '          RETRY_DIGEST: ${{ steps.rebuild.outputs.digest }}\n'
		printf '        run: |\n          echo "digest=x" >> "${GITHUB_OUTPUT}"\n'
		printf '      - name: Attest build provenance\n        with:\n'
		if ((consumers)); then
			printf '          subject-digest: ${{ steps.image.outputs.digest }}\n'
		else
			printf '          subject-digest: unresolved\n'
		fi
		if ((leaky)); then
			printf '      - name: Record published digest\n        env:\n'
			printf '          IMAGE: ghcr.io/o/n@${{ steps.build.outputs.digest }}\n'
			printf '        run: echo "${IMAGE}"\n'
		fi
	} > "$out"
	printf '%s\n' "$out"
}

# expect NAME EXPECT_RC WORKFLOW [SUBSTRING] — run the gate and assert the exit
# code, and that any SUBSTRING is reported.
expect() {
	local name="$1" want_rc="$2" workflow="$3" want_text="${4:-}"
	local got_rc=0 out
	out="$("$CHECKER" "$workflow" 2>&1)" || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" != "$want_rc" ]]; then
		printf 'FAIL %-32s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$out" >&2
		fails=$((fails + 1))
		return
	fi
	if [[ -n "$want_text" && "$out" != *"$want_text"* ]]; then
		printf 'FAIL %-32s output does not mention %s\n%s\n' "$name" "$want_text" "$out" >&2
		fails=$((fails + 1))
		return
	fi
	printf 'ok   %-32s rc=%s\n' "$name" "$got_rc"
}

# --- the compliant shape ----------------------------------------------------
#
# Asserted first: every failing case below is this fixture with one rule broken,
# so a fixture that could not pass at all would make the rest vacuous.
expect compliant 0 "$(write_workflow compliant)" 'publish-digest: ok'

# --- the defect Q899 names --------------------------------------------------
#
# A later step reading the first attempt's digest. After a retry that digest
# names an index the tag no longer serves, so the signature would be valid over
# an image nobody can pull by tag.
expect consumer-reads-attempt 1 "$(write_workflow leaky leaky-consumer)" 'outside the resolver'

# --- the retry's own wiring -------------------------------------------------
expect first-attempt-unmasked 1 "$(write_workflow nocoe no-first-coe)" 'can never run'
expect retry-masked-too 1 "$(write_workflow retrycoe retry-coe)" 'failed twice'
expect retry-runs-always 1 "$(write_workflow noif no-retry-if)" 'republish every release'

# --- the attempts must build the same thing ---------------------------------
#
# Hand-kept copies: a build-arg added to one side alone means the retry
# publishes something the first attempt was not building.
expect attempts-diverged 1 "$(write_workflow divergent divergent-with)" 'set different with: values'

# `no-cache: true` is the Q127 supply-chain property, and dropping it from the
# retry alone is the shape that reads as a harmless copy-paste slip.
expect retry-lost-no-cache 1 "$(write_workflow cached retry-cached)" 'no-cache'

# --- shapes that must refuse rather than pass -------------------------------
#
# Each leaves the gate with nothing to compare, so green would be
# indistinguishable from a compliant tree.
expect retry-absent 2 "$(write_workflow noretry no-retry)" 'id: rebuild'
expect nothing-reads-resolver 2 "$(write_workflow noconsumers no-consumers)" 'unused step'
expect workflow-missing 2 "$FIXTURE_DIR/no-such.yml" 'does not exist'

# A resolver that stopped reading an attempt is silent in the other direction:
# the leak rule finds no violation, and the retry's digest would never be
# selected.
expect resolver-drops-an-attempt 1 "$(write_workflow halfresolver no-resolver-first)" 'nothing reads'

# --- the tracked tree -------------------------------------------------------
#
# The default argument is the real publish.yml, so this is the gate exactly as
# `make check` runs it.
rc=0
out="$("$CHECKER" 2>&1)" || rc=$?
die_if_killed tracked-workflow "$rc"
if ((rc != 0)); then
	printf 'FAIL %-32s the tracked publish.yml does not pass its own gate (rc=%s)\n%s\n' \
		tracked-workflow "$rc" "$out" >&2
	fails=$((fails + 1))
else
	printf 'ok   %-32s rc=0\n' tracked-workflow
fi

if ((fails)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall check-publish-digest.sh tests passed\n'
