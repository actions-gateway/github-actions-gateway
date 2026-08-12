#!/usr/bin/env bash
#
# Unit tests for scripts/ci/gate-list.sh (Q649, Q671). The gate is only worth
# having if it goes red on the drift it exists to catch, so every assertion here
# injects one defect into a healthy fixture and requires a failure: a gate that
# runs without a .PHONY or a `##` line, a heavy phase in the recipe that is not in
# CHECK_HEAVY_GATES, a gate hand-wired into the fan-out line, a target declared
# .PHONY twice, a STATUS_GATES member outside CHECK_FAST_GATES, a fast gate that
# scans docs/STATUS.md while STATUS_GATES omits it, a gate whose file set is
# neither derivable nor declared, a doc that
# stopped pointing at the list targets, and a scripts/ suite on disk that
# SCRIPTS_TESTS omits — the one whose symptom is a green `make scripts-test` that
# never ran it. Reading the Makefile predicts these; only running the checker
# measures them.
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/ci/gate-list.sh"

FIXTURE_DIR="$REPO_ROOT/tmp/gate-list-test.$$"
mkdir -p "$FIXTURE_DIR"
trap 'rm -rf "$FIXTURE_DIR"' EXIT INT TERM

DOC="$FIXTURE_DIR/doc.md"
printf 'Run make list-gates and make list-script-tests to see the sets.\n' >"$DOC"
STALE_DOC="$FIXTURE_DIR/stale.md"
printf 'The gate runs alpha and beta.\n' >"$STALE_DOC"

# A scripts/ tree standing in for the real one: two suites, plus a runner whose
# name ends in -test.sh but which is not a suite. NON_SUITE_TESTS is keyed to the
# real tree, so the fixture reuses one of its entries to assert the exemption.
SCRIPTS="$FIXTURE_DIR/scripts"
mkdir -p "$SCRIPTS/one" "$SCRIPTS/go"
touch "$SCRIPTS/one/first-test.sh" "$SCRIPTS/one/second-test.sh" "$SCRIPTS/go/go-test.sh"
SUITES='one/first-test one/second-test'

# The gate scripts the STATUS_GATES-completeness rule reads. Their pathspecs are
# resolved against the real repo, which is the point: `docs/*.md` selects
# docs/STATUS.md there, so the wide gate below is scoped the way em-dash-check
# and page-density-check are.
printf "git_candidates '*.go' | select_present_files\n" >"$SCRIPTS/one/alpha.sh"
printf "git_candidates '*.go' ':!:vendor/*' | select_present_files\n" >"$SCRIPTS/one/beta.sh"
printf "git_candidates 'docs/*.md' | select_present_files\n" >"$SCRIPTS/one/wide.sh"

fails=0

# write_makefile PATH [EXTRA_LINE] [FANOUT_SUFFIX] [BETA_RECIPE] [BETA_MARKER] —
# a fixture with two fast gates and two heavy ones declared, whose `check:` recipe
# fans out over CHECK_FAST_GATES and then runs heavy-one only. Recipe lines need
# real tabs, so they are emitted rather than written inline. beta's recipe and the
# comment above its .PHONY are the two inputs the STATUS_GATES-completeness rule
# reads: which files it selects, and whether it declares itself out of scope.
# shellcheck disable=SC2016 # the recipe body is make source text, not shell expansions
write_makefile() {
	local path="$1" extra="${2-}" fanout_suffix="${3-}" target
	local beta_recipe="${4-scripts/one/beta.sh}" beta_marker="${5-}"
	{
		for target in alpha beta heavy-one heavy-two; do
			if [[ "$target" == beta && -n "$beta_marker" ]]; then
				printf '%s\n' "$beta_marker"
			fi
			printf '.PHONY: %s\n%s: ## %s does a thing\n' "$target" "$target" "$target"
			case "$target" in
			alpha) printf '\tscripts/one/alpha.sh\n\n' ;;
			beta) printf '\t%s\n\n' "$beta_recipe" ;;
			*) printf '\ttrue\n\n' ;;
			esac
		done
		printf '%s\n' "$extra"
		printf 'CHECK_FAST_GATES := alpha beta\n'
		printf 'CHECK_HEAVY_GATES := heavy-one\n\n'
		printf '.PHONY: check\ncheck: ## The gate\n'
		printf '\tscripts/ci/run-parallel.sh $(foreach gate,$(CHECK_FAST_GATES),"$(gate):$(MAKE) $(gate)")%s\n' "$fanout_suffix"
		printf '\t$(MAKE) heavy-one\n'
	} >"$path"
}

# expect NAME WANT_RC ARGS... — run the checker and assert the exit code. Every
# --check case reads the fixture scripts/ tree and the healthy suite list unless
# it overrides them, so a case aimed at a gate rule cannot fail on a suite rule.
# The checker's own output lands in LAST_OUT for the assertions that inspect it.
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
write_makefile "$FIXTURE_DIR/Makefile.wide" '' '' 'scripts/one/wide.sh'
write_makefile "$FIXTURE_DIR/Makefile.opaque" '' '' 'true'
write_makefile "$FIXTURE_DIR/Makefile.marked" '' '' 'true' \
	'# status-scope: none - beta reads no Markdown'

# expect_check NAME WANT_RC ARGS... — a --check run over the healthy fixture,
# with ARGS appended. The parser takes the last occurrence of an option, so a
# case overrides a default just by naming it again.
expect_check() {
	local name="$1" want_rc="$2"
	shift 2
	expect "$name" "$want_rc" --check --makefile "$MK" --doc "$DOC" \
		--scripts-dir "$SCRIPTS" --suites "$SUITES" "$@"
}

# The healthy fixture passes — without this the red cases below prove nothing.
expect_check healthy 0 --fast 'alpha beta' --heavy 'heavy-one' --status 'alpha'

# A gate that runs but has no .PHONY and no `##` line: `make list-gates` would
# print it blank and make would look for a file by that name.
expect_check undeclared-gate 1 --fast 'alpha beta gamma' --heavy 'heavy-one'
assert_output undeclared-gate gamma

# A heavy phase the recipe runs but CHECK_HEAVY_GATES omits — and the reverse.
expect_check heavy-recipe-drift 1 --fast 'alpha beta' --heavy 'heavy-one heavy-two'
assert_output heavy-recipe-drift CHECK_HEAVY_GATES
expect_check heavy-swapped 1 --fast 'alpha beta' --heavy 'heavy-two'

# A gate hand-wired into the fan-out line instead of into CHECK_FAST_GATES: it
# would run on every `make check` while never appearing in `make list-gates`.
expect_check fanout-hand-wired 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--makefile "$FIXTURE_DIR/Makefile.wired"
assert_output fanout-hand-wired CHECK_FAST_GATES

# A target declared .PHONY twice — the bulk block coming back.
expect_check duplicate-phony 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--makefile "$FIXTURE_DIR/Makefile.dupe"
assert_output duplicate-phony 'more than once'

# STATUS_GATES must stay a subset of CHECK_FAST_GATES, the claim its comment makes.
expect_check status-gates-not-subset 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--status 'alpha heavy-one'
assert_output status-gates-not-subset 'heavy-one'

# And the direction rule 4 cannot see: a fast gate that scans docs/STATUS.md
# while STATUS_GATES omits it. This is Q749's defect — em-dash-check and
# page-density-check both selected the file from the day each was written — so
# the fixture gate is scoped the same way, through a `docs/*.md` pathspec.
expect_check status-gates-incomplete 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--status 'alpha' --makefile "$FIXTURE_DIR/Makefile.wide"
assert_output status-gates-incomplete 'docs/STATUS.md'
assert_output status-gates-incomplete beta
expect_check status-gates-complete 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--status 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.wide"

# A gate running no scripts/ file has no pathspec to derive from, so it must say
# which side it is on rather than being assumed out of scope.
expect_check status-scope-underivable 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--status 'alpha' --makefile "$FIXTURE_DIR/Makefile.opaque"
assert_output status-scope-underivable 'status-scope'
expect_check status-scope-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--status 'alpha' --makefile "$FIXTURE_DIR/Makefile.marked"

# The doc has to keep naming both targets rather than re-transcribing the lists.
expect_check doc-lost-pointer 1 --fast 'alpha beta' --heavy 'heavy-one' --doc "$STALE_DOC"
assert_output doc-lost-pointer 'list-gates'
printf 'Run make list-gates to see the set.\n' >"$FIXTURE_DIR/half.md"
expect_check doc-lost-suites-pointer 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--doc "$FIXTURE_DIR/half.md"
assert_output doc-lost-suites-pointer 'list-script-tests'

# The suite rules (Q671). A *-test.sh on disk that SCRIPTS_TESTS omits is the
# case that matters: `make scripts-test` reports green having never run it, so
# the failure is invisible from the gate's own output.
expect_check suite-on-disk-unlisted 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--suites 'one/first-test'
assert_output suite-on-disk-unlisted 'one/second-test'
assert_output suite-on-disk-unlisted 'never runs them'

# And the reverse: a listed suite whose file is gone would fail the fan-out on a
# missing path, so the gate names it first.
expect_check suite-listed-missing 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--suites "$SUITES one/third-test"
assert_output suite-listed-missing 'one/third-test'

# go/go-test.sh sits in the fixture tree and is exempt, so the healthy case above
# already proves NON_SUITE_TESTS is honoured. Claiming an exemption *and* listing
# it is the contradiction that must fail rather than resolve silently.
expect_check non-suite-also-listed 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--suites "$SUITES go/go-test"
assert_output non-suite-also-listed 'cannot be both'

# Mode and argument validation, so a malformed call fails loudly instead of
# reporting a clean list of nothing.
expect no-mode 2 --makefile "$MK" --fast 'alpha' --heavy 'heavy-one'
expect no-lists 2 --check --makefile "$MK"
expect no-suites 2 --check --makefile "$MK" --fast 'alpha' --heavy 'heavy-one'
expect list-suites-no-suites 2 --list-suites
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

# --list-suites is what the `scripts-test` help line now points at, so it has to
# name every suite it was given and count them.
suite_listing="$("$CHECKER" --list-suites --suites "$SUITES" --scripts-dir "$SCRIPTS")"
missing=""
for suite in $SUITES; do
	grep -qF "    $SCRIPTS/$suite.sh" <<<"$suite_listing" || missing="$missing $suite"
done
grep -q 'runs 2 scripts/ suites' <<<"$suite_listing" || missing="$missing (count)"
if [[ -n "$missing" ]]; then
	printf 'FAIL %-28s absent from --list-suites output:%s\n%s\n' \
		list-renders-every-suite "$missing" "$suite_listing" >&2
	fails=$((fails + 1))
else
	printf 'ok   %-28s every suite rendered\n' list-renders-every-suite
fi

if ((fails > 0)); then
	printf '\n%d gate-list assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ngate-list-test: all assertions passed\n'
