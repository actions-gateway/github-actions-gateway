#!/usr/bin/env bash
#
# Unit tests for scripts/ci/gate-list.sh (Q649, Q671). The gate is only worth
# having if it goes red on the drift it exists to catch, so every assertion here
# injects one defect into a healthy fixture and requires a failure: a gate that
# runs without a .PHONY or a `##` line, a heavy phase in the recipe that is not in
# CHECK_HEAVY_GATES, a gate hand-wired into the fan-out line, a target declared
# .PHONY twice, a QUEUE_GATES member outside CHECK_FAST_GATES, a fast gate that
# selects a backlog item while QUEUE_GATES omits it, a gate whose file set is
# neither derivable nor declared, a gate no workflow runs, a gate whose only
# workflow the merge queue never evaluates, a doc that stopped pointing at the
# list targets, and a scripts/ suite on disk that SCRIPTS_TESTS omits — the one
# whose symptom is a green `make scripts-test` that never ran it. Reading the
# Makefile predicts these; only running the checker measures them.
#
# The DOCS_GATES pair (rules 9 and 10, Q920) is asserted in three directions
# rather than two. A subset assertion is satisfied by a list that is empty or
# misspelled, so proving it goes red on a bad member says nothing about whether
# it can be disarmed: the empty case must refuse as well, which is the defect
# the rules themselves are about, one level up.
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

# The gate scripts the QUEUE_GATES-completeness rule reads. Their pathspecs are
# resolved against the real repo, which is the point: a git glob crosses
# directory separators, so `docs/*.md` selects every item under docs/queue/
# there and the wide gate below is scoped the way em-dash-check and
# page-density-check are. That is not hypothetical — page-density-check was
# exactly this shape when the rule was repointed at the store (Q889).
printf "git_candidates '*.go' | select_present_files\n" >"$SCRIPTS/one/alpha.sh"
printf "git_candidates '*.go' ':!:vendor/*' | select_present_files\n" >"$SCRIPTS/one/beta.sh"
printf "git_candidates 'docs/*.md' | select_present_files\n" >"$SCRIPTS/one/wide.sh"
# Scoped to prose that is not the backlog store, so the DOCS_GATES-completeness
# rule fires on its own: `docs/development/*.md` reaches no docs/queue/ item, so
# the QUEUE_GATES rules stay quiet and the red below has one cause.
printf "git_candidates 'docs/development/*.md' | select_present_files\n" >"$SCRIPTS/one/prose.sh"
# The Q930 shapes: a gate whose subject is a constant hands git nothing, so the
# pathspec derivation above sees none of these. Each names a real tracked path,
# because the answer is resolved against this repo the way a pathspec is.
# shellcheck disable=SC2016 # the body is the fixture's shell source, not an expansion
printf 'STORE="${1:-docs/queue}"\n' >"$SCRIPTS/one/subject-queue.sh"
# shellcheck disable=SC2016 # same: a literal ${1:-...} default is the shape under test
printf 'PAGE="${1:-docs/development/testing.md}"\n' >"$SCRIPTS/one/subject-prose.sh"
printf "DEFAULT_FILES=('docs/development/testing.md')\n" >"$SCRIPTS/one/subject-array.sh"
# A script whose only path literals sit behind a command substitution. The
# tokenizer cannot follow the quoting through one, and what fell out of the
# attempt was `.` — which selects the whole tree and would report every gate in
# every scope. It must yield nothing instead.
# shellcheck disable=SC2016 # the command substitutions are the fixture's subject
printf 'root="$(git rev-parse --show-toplevel)"\nn="$(awk \047$1 == "docs/queue" { print }\047 docs/roadmap.md)"\n' \
	>"$SCRIPTS/one/subject-cmdsub.sh"

fails=0

# write_makefile PATH [EXTRA_LINE] [FANOUT_SUFFIX] [BETA_RECIPE] [BETA_MARKER] —
# a fixture with two fast gates and two heavy ones declared, whose `check:` recipe
# fans out over CHECK_FAST_GATES and then runs heavy-one only. Recipe lines need
# real tabs, so they are emitted rather than written inline. beta's recipe and the
# comment above its .PHONY are the two inputs the QUEUE_GATES-completeness rule
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
		# Not a gate: a target that runs beta's script, standing in for the way
		# `manifest-validate` runs the three chart-*-check scripts. A workflow
		# invoking this covers beta without naming it.
		printf '.PHONY: wrapper\nwrapper: ## runs beta'"'"'s script\n'
		printf '\tscripts/one/beta.sh\n\n'
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

# refute_output NAME PATTERN — the last checker run did NOT name PATTERN. Rule
# 11 needs the negative half: a gate reported once and a gate reported twice for
# one absence are both red, so only asserting the absence tells them apart.
refute_output() {
	if grep -q -- "$2" <<<"$LAST_OUT"; then
		printf 'FAIL %-28s output names %s\n%s\n' "$1" "$2" "$LAST_OUT" >&2
		fails=$((fails + 1))
	else
		printf 'ok   %-28s output does not name %s\n' "$1" "$2"
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
	'# queue-scope: none - beta reads no Markdown'
write_makefile "$FIXTURE_DIR/Makefile.prose" '' '' 'scripts/one/prose.sh'
write_makefile "$FIXTURE_DIR/Makefile.docs-marked" '' '' 'true' \
	'# docs-scope: none - beta reads no Markdown'
write_makefile "$FIXTURE_DIR/Makefile.ci-marked" '' '' 'true' \
	'# ci-scope: none - beta is a local-only convenience'
write_makefile "$FIXTURE_DIR/Makefile.mq-marked" '' '' 'scripts/one/beta.sh' \
	'# merge-queue-scope: none - beta is advisory on the candidate merge'
write_makefile "$FIXTURE_DIR/Makefile.subject-queue" '' '' 'scripts/one/subject-queue.sh'
write_makefile "$FIXTURE_DIR/Makefile.subject-queue-marked" '' '' 'scripts/one/subject-queue.sh' \
	'# queue-scope: none - beta reads the store path to name a file, not to check one'
write_makefile "$FIXTURE_DIR/Makefile.subject-prose" '' '' 'scripts/one/subject-prose.sh'
write_makefile "$FIXTURE_DIR/Makefile.subject-prose-marked" '' '' 'scripts/one/subject-prose.sh' \
	'# docs-scope: none - beta names the page as an instrument, not a subject'
write_makefile "$FIXTURE_DIR/Makefile.subject-array" '' '' 'scripts/one/subject-array.sh'
write_makefile "$FIXTURE_DIR/Makefile.subject-cmdsub" '' '' 'scripts/one/subject-cmdsub.sh'
write_makefile "$FIXTURE_DIR/Makefile.wide-marked" '' '' 'scripts/one/wide.sh' \
	'# queue-scope: none - a declaration that must not silence a pathspec'

# Workflow trees for rules 8 and 11. Every gate has to reach CI somehow, so the
# default tree runs all three by name and the variants each remove one route.
# The default `on:` declares merge_group, so a case aimed at rule 8 cannot trip
# rule 11 as well.
# write_workflow PATH BODY [ON_BLOCK]
MQ_ON='on:\n  pull_request:\n  merge_group:\n'
write_workflow() {
	local dir="$1"
	mkdir -p "$dir"
	printf '%bjobs:\n  gate:\n    steps:\n%b' "${3-$MQ_ON}" "$2" >"$dir/ci.yml"
}

# write_extra_workflow DIR NAME ON_BLOCK BODY — a second file in an existing
# tree, so a case can split coverage between a workflow the merge queue
# evaluates and one it does not.
write_extra_workflow() {
	printf '%bjobs:\n  gate:\n    steps:\n%b' "$3" "$4" >"$1/$2.yml"
}

WF_ALL="$FIXTURE_DIR/wf-all"
write_workflow "$WF_ALL" '      - run: make alpha\n      - run: make beta\n      - run: make heavy-one\n'
# beta reaches no workflow: the Q831 shape, green under every other rule.
WF_NO_BETA="$FIXTURE_DIR/wf-no-beta"
write_workflow "$WF_NO_BETA" '      - run: make alpha\n      - run: make heavy-one\n'
# beta named only in a comment. A gate a workflow merely mentions gates nothing,
# and these files describe themselves in prose that names their own targets.
WF_COMMENT="$FIXTURE_DIR/wf-comment"
write_workflow "$WF_COMMENT" '      # run: make beta\n      - run: make alpha\n      - run: make heavy-one\n'
# beta covered by its script rather than its target — the status-lint shape,
# which runs lint-queue.sh without going through make.
WF_SCRIPT="$FIXTURE_DIR/wf-script"
write_workflow "$WF_SCRIPT" '      - run: scripts/one/beta.sh\n      - run: make alpha\n      - run: make heavy-one\n'
# beta covered through another make target that runs its script.
WF_WRAPPER="$FIXTURE_DIR/wf-wrapper"
write_workflow "$WF_WRAPPER" '      - run: make wrapper\n      - run: make alpha\n      - run: make heavy-one\n'
# beta covered only by a workflow the merge queue never evaluates: rule 8's
# question is answered and the candidate merge is held to alpha and heavy-one
# alone. Split across two files because that is the shape in the tree — four
# gates each sat in a `pull_request`-only workflow of their own (Q942).
WF_NO_MQ="$FIXTURE_DIR/wf-no-mq"
write_workflow "$WF_NO_MQ" '      - run: make alpha\n      - run: make heavy-one\n'
write_extra_workflow "$WF_NO_MQ" pr-only 'on:\n  pull_request:\n' '      - run: make beta\n'
# The same tree with merge_group present only as a comment in that `on:` block.
# These files explain their own triggers in prose, so a trigger the workflow
# merely names must not count — the rule 8 shape one question over.
WF_MQ_COMMENT="$FIXTURE_DIR/wf-mq-comment"
write_workflow "$WF_MQ_COMMENT" '      - run: make alpha\n      - run: make heavy-one\n'
write_extra_workflow "$WF_MQ_COMMENT" pr-only \
	'on: # merge_group is not declared here (Q942)\n  pull_request:\n  # merge_group:\n' \
	'      - run: make beta\n'

# expect_check NAME WANT_RC ARGS... — a --check run over the healthy fixture,
# with ARGS appended. The parser takes the last occurrence of an option, so a
# case overrides a default just by naming it again.
expect_check() {
	local name="$1" want_rc="$2"
	shift 2
	expect "$name" "$want_rc" --check --makefile "$MK" --doc "$DOC" \
		--scripts-dir "$SCRIPTS" --suites "$SUITES" --workflows "$WF_ALL" \
		--docs 'alpha' "$@"
}

# The healthy fixture passes — without this the red cases below prove nothing.
expect_check healthy 0 --fast 'alpha beta' --heavy 'heavy-one' --queue 'alpha'

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

# QUEUE_GATES must stay a subset of CHECK_FAST_GATES, the claim its comment makes.
expect_check queue-gates-not-subset 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha heavy-one'
assert_output queue-gates-not-subset 'heavy-one'

# And the direction rule 4 cannot see: a fast gate that selects a backlog item
# while QUEUE_GATES omits it. This is Q749's defect — em-dash-check and
# page-density-check both selected the file from the day each was written — so
# the fixture gate is scoped the same way, through a `docs/*.md` pathspec.
expect_check queue-gates-incomplete 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.wide"
assert_output queue-gates-incomplete 'docs/queue/'
assert_output queue-gates-incomplete beta
expect_check queue-gates-complete 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha beta' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.wide"

# A gate running no scripts/ file has no pathspec to derive from, so it must say
# which side it is on rather than being assumed out of scope.
expect_check queue-scope-underivable 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.opaque"
assert_output queue-scope-underivable 'queue-scope'
expect_check queue-scope-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.marked"

# Rule 7's second derivation (Q930): a gate whose subject is a hardcoded literal
# rather than a pathspec. Nothing in the tree is in this state — the only real
# gate the derivation reaches declares itself out — so the fixture is the whole
# assertion, and it has to go red on its own.
expect_check queue-subject-incomplete 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-queue"
assert_output queue-subject-incomplete 'as its subject'
assert_output queue-subject-incomplete beta
expect_check queue-subject-complete 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha beta' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-queue"
# A subject is weaker evidence than a pathspec, so it can be declared away.
expect_check queue-subject-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-queue-marked"

# And the half that keeps Q749 intact: the same declaration must NOT silence a
# real pathspec. Without this, adding the hatch would quietly hand every gate a
# way out of the rule that caught em-dash-check and page-density-check.
expect_check queue-pathspec-not-declarable 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.wide-marked"
assert_output queue-pathspec-not-declarable 'pathspec'

# A path literal behind a command substitution is not a subject. The tokenizer
# cannot follow the quoting through one and its fragments included `.`, which
# selects the tree — so this fixture is green only while that guard holds.
expect_check subject-cmdsub-ignored 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--queue 'alpha' --docs 'alpha' --makefile "$FIXTURE_DIR/Makefile.subject-cmdsub"

# DOCS_GATES must stay a subset of CHECK_FAST_GATES (rule 9). It was not:
# release-notes-check sat in DOCS_GATES and in neither gate list, so
# `make docs-gates` ran a gate `make check` did not, for the whole life of the
# list — nothing passed it in to notice (Q920).
expect_check docs-gates-not-subset 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha heavy-one'
assert_output docs-gates-not-subset 'heavy-one'

# And the direction rule 9 cannot see (rule 10): a fast gate that selects a page
# under docs/ while DOCS_GATES omits it. conflict-markers-check was exactly this
# — it scans the whole tree — so the fixture gate is scoped to prose, and to
# prose that is not the backlog store, so this red has one cause and not two.
expect_check docs-gates-incomplete 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --makefile "$FIXTURE_DIR/Makefile.prose"
assert_output docs-gates-incomplete 'docs/'
assert_output docs-gates-incomplete beta
expect_check docs-gates-complete 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.prose"

# A gate running no scripts/ file has no pathspec to derive from here either, so
# it declares which side it is on rather than being assumed out of scope.
expect_check docs-scope-underivable 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.opaque"
assert_output docs-scope-underivable 'docs-scope'
expect_check docs-scope-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.docs-marked"

# Rule 10's second derivation, where it earns its keep: every page-scoped gate
# in the tree hardcodes its page, and seven were invisible to rule 10 at once
# (Q930). Both spellings a real gate uses — an argument default and an array.
expect_check docs-subject-incomplete 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-prose"
assert_output docs-subject-incomplete 'as its subject'
assert_output docs-subject-incomplete beta
expect_check docs-subject-complete 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha beta' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-prose"
expect_check docs-subject-array 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-array"
assert_output docs-subject-array 'as its subject'
expect_check docs-subject-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--docs 'alpha' --queue 'alpha beta' --makefile "$FIXTURE_DIR/Makefile.subject-prose-marked"

# The half the two cases above cannot reach: both prove the rules fire on a list
# that is wrong, and neither can prove they fire on a list that is not there. An
# empty DOCS_GATES — a renamed variable, a dropped argument — satisfies rule 9
# with no members to test, which is the disarmed-gate shape these rules exist to
# catch. It must refuse rather than report agreement.
expect docs-empty-refused 2 --check --makefile "$MK" --doc "$DOC" \
	--scripts-dir "$SCRIPTS" --suites "$SUITES" --workflows "$WF_ALL" \
	--fast 'alpha beta' --heavy 'heavy-one' --docs ''
assert_output docs-empty-refused 'non-empty --docs'
expect docs-absent-refused 2 --check --makefile "$MK" --doc "$DOC" \
	--scripts-dir "$SCRIPTS" --suites "$SUITES" --workflows "$WF_ALL" \
	--fast 'alpha beta' --heavy 'heavy-one'

# Rule 8 (Q831): a gate can join CHECK_FAST_GATES and be run by no workflow,
# which every rule above reports as healthy — `make check` enforces it locally
# while every PR merges without it. Five gates were in that state when this was
# written, and the one that made it visible (md-reflow-check) had let four
# unformatted files onto main.
expect_check gate-not-in-ci 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_NO_BETA"
assert_output gate-not-in-ci beta
assert_output gate-not-in-ci 'not in CI'

# A gate named only in a workflow comment is not wired. This is the direction a
# whole-file grep gets wrong: these workflows explain themselves in prose that
# names their own targets, so matching anywhere would call every gate covered.
expect_check gate-in-ci-comment-only 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_COMMENT"
assert_output gate-in-ci-comment-only beta

# The two indirect routes that are real coverage: the gate's script invoked
# directly (status-lint runs check-queue-rules.sh), and another make target running
# it (manifest-validate runs the three chart-*-check scripts).
expect_check gate-in-ci-via-script 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_SCRIPT"
expect_check gate-in-ci-via-wrapper 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_WRAPPER"

# A gate that runs no scripts/ file has no indirect route to be covered by, so
# an unwired one must say which side it is on rather than pass by default.
expect_check ci-scope-underivable 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--makefile "$FIXTURE_DIR/Makefile.opaque" --workflows "$WF_NO_BETA" \
	--queue 'alpha beta' --docs 'alpha beta'
assert_output ci-scope-underivable 'ci-scope'
expect_check ci-scope-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--makefile "$FIXTURE_DIR/Makefile.ci-marked" --workflows "$WF_NO_BETA" \
	--queue 'alpha beta' --docs 'alpha beta'

# Rule 11 (Q942): a gate whose only workflow the merge queue never evaluates.
# Rule 8 reports it healthy — some workflow does run it — while the candidate
# merge, the one commit that carries the merge result, is never held to it.
# Four gates were in that state when this was written.
expect_check gate-not-in-merge-queue 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_NO_MQ"
assert_output gate-not-in-merge-queue beta
assert_output gate-not-in-merge-queue 'merge queue'

# merge_group named only in a comment inside the `on:` block is not a trigger.
expect_check merge-group-comment-only 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_MQ_COMMENT"
assert_output merge-group-comment-only beta
assert_output merge-group-comment-only 'merge queue'

# A gate deliberately kept off the candidate merge declares it, the shape rules
# 7, 8 and 10 already use.
expect_check merge-queue-scope-declared 0 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_NO_MQ" --makefile "$FIXTURE_DIR/Makefile.mq-marked"

# A gate no workflow runs at all has one defect, not two. Both rules read the
# same coverage question, so rule 11 has to stay quiet where rule 8 already
# spoke — and only the absence distinguishes that from reporting it twice.
expect_check gate-not-in-ci-reports-once 1 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$WF_NO_BETA"
refute_output gate-not-in-ci-reports-once 'merge queue'

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
# An unreadable workflow tree must refuse rather than report every gate unwired.
expect_check no-workflow-tree 2 --fast 'alpha beta' --heavy 'heavy-one' \
	--workflows "$FIXTURE_DIR/absent"
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
