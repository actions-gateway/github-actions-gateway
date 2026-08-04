#!/usr/bin/env bash
#
# Unit tests for scripts/ci/gate-list.sh (Q649). The gate is only worth having if
# it goes red on the drift it exists to catch, so every assertion here injects
# one defect into a healthy fixture and requires a failure: a gate that runs
# without a .PHONY or a `##` line, a heavy phase in the recipe that is not in
# CHECK_HEAVY_GATES, a gate hand-wired into the fan-out line, a target declared
# .PHONY twice, a STATUS_GATES member outside CHECK_FAST_GATES, and a doc that
# stopped pointing at `make list-gates`. Reading the Makefile predicts these;
# only running the checker measures them.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/gate-list.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/gate-list-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

DOC="$FIXTURE_DIR/doc.md"
printf 'Run make list-gates to see the set.\n' >"$DOC"
STALE_DOC="$FIXTURE_DIR/stale.md"
printf 'The gate runs alpha and beta.\n' >"$STALE_DOC"

fails=0

# write_makefile PATH [EXTRA_LINE] [FANOUT_SUFFIX] — a fixture with two fast
# gates and two heavy ones declared, whose `check:` recipe fans out over
# CHECK_FAST_GATES and then runs heavy-one only. Recipe lines need real tabs, so
# they are emitted rather than written inline.
# shellcheck disable=SC2016 # the recipe body is make source text, not shell expansions
write_makefile() {
	local path="$1" extra="${2-}" fanout_suffix="${3-}" target
	{
		for target in alpha beta heavy-one heavy-two; do
			printf '.PHONY: %s\n%s: ## %s does a thing\n' "$target" "$target" "$target"
			printf '\ttrue\n\n'
		done
		printf '%s\n' "$extra"
		printf 'CHECK_FAST_GATES := alpha beta\n'
		printf 'CHECK_HEAVY_GATES := heavy-one\n\n'
		printf '.PHONY: check\ncheck: ## The gate\n'
		printf '\tscripts/ci/run-parallel.sh $(foreach gate,$(CHECK_FAST_GATES),"$(gate):$(MAKE) $(gate)")%s\n' "$fanout_suffix"
		printf '\t$(MAKE) heavy-one\n'
	} >"$path"
}

# expect NAME WANT_RC ARGS... — run the checker and assert the exit code. The
# checker's own output lands in LAST_OUT for the assertions that inspect it.
LAST_OUT=""
expect() {
	local name="$1" want_rc="$2" got_rc=0
	shift 2
	LAST_OUT="$("$CHECKER" "$@" 2>&1)" || got_rc=$?
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-28s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-28s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

# assert_output NAME PATTERN — the last checker run named what it rejected.
assert_output() {
	if grep -q -- "$2" <<<"$LAST_OUT"; then
		printf 'ok   %-28s output names %s\n' "$1" "$2"
	else
		printf 'FAIL %-28s output does not name %s\n%s\n' "$1" "$2" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

MK="$FIXTURE_DIR/Makefile"
write_makefile "$MK"
# shellcheck disable=SC2016 # the suffix is make source text appended to the fan-out line
write_makefile "$FIXTURE_DIR/Makefile.wired" '' ' "gamma:$(MAKE) gamma"'
write_makefile "$FIXTURE_DIR/Makefile.dupe" '.PHONY: alpha beta'

# The healthy fixture passes — without this the red cases below prove nothing.
expect healthy 0 --check --makefile "$MK" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-one' --status 'alpha'

# A gate that runs but has no .PHONY and no `##` line: `make list-gates` would
# print it blank and make would look for a file by that name.
expect undeclared-gate 1 --check --makefile "$MK" --doc "$DOC" \
	--fast 'alpha beta gamma' --heavy 'heavy-one'
assert_output undeclared-gate gamma

# A heavy phase the recipe runs but CHECK_HEAVY_GATES omits — and the reverse.
expect heavy-recipe-drift 1 --check --makefile "$MK" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-one heavy-two'
assert_output heavy-recipe-drift CHECK_HEAVY_GATES
expect heavy-swapped 1 --check --makefile "$MK" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-two'

# A gate hand-wired into the fan-out line instead of into CHECK_FAST_GATES: it
# would run on every `make check` while never appearing in `make list-gates`.
expect fanout-hand-wired 1 --check --makefile "$FIXTURE_DIR/Makefile.wired" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-one'
assert_output fanout-hand-wired CHECK_FAST_GATES

# A target declared .PHONY twice — the bulk block coming back.
expect duplicate-phony 1 --check --makefile "$FIXTURE_DIR/Makefile.dupe" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-one'
assert_output duplicate-phony 'more than once'

# STATUS_GATES must stay a subset of CHECK_FAST_GATES, the claim its comment makes.
expect status-gates-not-subset 1 --check --makefile "$MK" --doc "$DOC" \
	--fast 'alpha beta' --heavy 'heavy-one' --status 'alpha heavy-one'
assert_output status-gates-not-subset 'heavy-one'

# The doc has to keep naming the target rather than re-transcribing the list.
expect doc-lost-pointer 1 --check --makefile "$MK" --doc "$STALE_DOC" \
	--fast 'alpha beta' --heavy 'heavy-one'
assert_output doc-lost-pointer 'list-gates'

# Mode and argument validation, so a malformed call fails loudly instead of
# reporting a clean list of nothing.
expect no-mode 2 --makefile "$MK" --fast 'alpha' --heavy 'heavy-one'
expect no-lists 2 --check --makefile "$MK"
expect unknown-arg 2 --check --makefile "$MK" --fast 'alpha' --heavy 'heavy-one' --bogus

# --list names every gate it was given, with each one's `##` description.
listing="$("$CHECKER" --list --makefile "$MK" --fast 'alpha beta' --heavy 'heavy-one')"
missing=""
for gate in alpha beta heavy-one; do
	grep -q "^  $gate " <<<"$listing" || missing="$missing $gate"
	grep -q "$gate does a thing" <<<"$listing" || missing="$missing $gate(desc)"
done
if [[ -n "$missing" ]]; then
	printf 'FAIL %-28s absent from --list output:%s\n' list-renders-every-gate "$missing" >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s every gate and description rendered\n' list-renders-every-gate
fi

if ((fails > 0)); then
	printf '\n%d gate-list assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ngate-list-test: all assertions passed\n'
