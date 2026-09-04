#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-path-filters.sh (Q429). The gate exists to fail on
# a filter that has drifted from the repo, so every case here breaks a fixture and
# asserts it is caught — the standing form of the invert-the-fix verification
# (docs/development/testing.md § Diagnosing failures). Fixtures rather than the
# tracked workflows, because the tracked ones are (and must stay) correct.
#
# Seven groups:
#
#   1. parse_filters reads the nested YAML string faithfully — trailing comments,
#      comment-only lines, and the dedent that ends the block.
#   2. pattern_covers_dir counts only patterns that match a module's WHOLE tree.
#      Its false negatives are fixable failures; a false positive would silently
#      reopen Q400, so the partial-cover cases are the important ones.
#   3. literal_prefix extracts what a pattern actually pins.
#   4. The assertions fail on a module a workspace-covering filter omits, on an
#      unregistered filter, and on a pattern whose path no longer exists — each
#      naming what to fix.
#   5. Two filters gating one reusable workflow are held to the same scripts/
#      patterns, comparing only those and ignoring order (Q571).
#   6. parse_push_paths reads `on.push.paths` out of real YAML, and a workflow's
#      push list is held equal to its changes filter — the drift Q571 itself
#      shipped, which no assertion then covered.
#   7. A `**` sits where picomatch still expands it. Both sides are pinned: the
#      sound shapes must not be flagged (a false positive fails the tracked
#      tree, which carries '**.go') and the degraded one must be (Q659).
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
# Source the script under test for its helpers and assertions; the BASH_SOURCE
# guard there keeps main() from running against the tracked tree on source.
# shellcheck source=scripts/ci/check-path-filters.sh
source "$REPO_ROOT/scripts/ci/check-path-filters.sh"

FIXTURE_ROOT="$REPO_ROOT/tmp/check-path-filters-test.$$"
trap 'rm -rf "$FIXTURE_ROOT"' EXIT INT TERM

fails=0

ok() { printf 'ok   %-42s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-42s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# expect_eq NAME WANT GOT
expect_eq() {
	local name="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$name" "= $(printf '%q' "$got")"
	else
		bad "$name" "want $(printf '%q' "$want") got $(printf '%q' "$got")"
	fi
}

# expect_covers NAME PATTERN DIR — the pattern gates DIR's whole tree.
expect_covers() {
	local name="$1" pattern="$2" dir="$3"
	if pattern_covers_dir "$pattern" "$dir"; then
		ok "$name" "'$pattern' covers $dir"
	else
		bad "$name" "'$pattern' should cover $dir"
	fi
}

# expect_no_cover NAME PATTERN DIR — the pattern leaves part of DIR ungated.
expect_no_cover() {
	local name="$1" pattern="$2" dir="$3"
	if pattern_covers_dir "$pattern" "$dir"; then
		bad "$name" "'$pattern' must NOT count as covering $dir"
	else
		ok "$name" "'$pattern' does not cover $dir"
	fi
}

# expect_assertion NAME WANT_FAILURES PATTERN COMMAND... — run an assertion from
# the script under test, then check how many problems it reported and that its
# output names PATTERN (empty to skip). Problems are counted from the report
# rather than the script's `failures` global: the assertion runs in a command
# substitution, so an increment there would not reach this shell, and the report
# is the contract a developer actually reads.
expect_assertion() {
	local name="$1" want="$2" pattern="$3"
	shift 3
	local out got
	out="$("$@" 2>&1)" || true
	got="$(grep -c '^ERROR: ' <<<"$out" || true)"
	if [[ "$got" != "$want" ]]; then
		bad "$name" "want $want failure(s) got $got"$'\n'"$out"
		return
	fi
	if [[ -n "$pattern" ]] && ! grep -q -- "$pattern" <<<"$out"; then
		bad "$name" "output did not mention $(printf '%q' "$pattern")"$'\n'"$out"
		return
	fi
	ok "$name" "$want failure(s)"
}

# --- 1. parsing ---------------------------------------------------------------

mkdir -p "$FIXTURE_ROOT/workflows"

# A block exercising every shape the real workflows use: a trailing comment after
# a quoted pattern, a comment-only line, two filters, and a dedent back to the
# `filters:` indentation that must end the block (the `- name:` step below it is
# a sibling key, not a pattern).
cat >"$FIXTURE_ROOT/workflows/parse.yml" <<'YAML'
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v4
        with:
          filters: |
            code:
              # A comment line is not a pattern.
              - 'api/**'
              - 'cmd/**'          # trailing comments are not part of the pattern
            docs:
              - 'docs/**'
      - name: not a pattern
        run: echo hi
YAML

expect_eq parse-code-patterns "api/** cmd/**" \
	"$(parse_filters "$FIXTURE_ROOT/workflows/parse.yml" | awk -F'\t' '$1=="code"{printf "%s%s", sep, $2; sep=" "}')"
expect_eq parse-docs-patterns "docs/**" \
	"$(parse_filters "$FIXTURE_ROOT/workflows/parse.yml" | awk -F'\t' '$1=="docs"{print $2}')"
# The dedented step must not leak into the last filter, or the gate would try to
# stat a YAML key as a path.
expect_eq parse-stops-at-dedent 3 \
	"$(parse_filters "$FIXTURE_ROOT/workflows/parse.yml" | wc -l | tr -d ' ')"

# --- 2. coverage semantics ----------------------------------------------------

expect_covers covers-module-root 'api/**' 'api'
expect_covers covers-ancestor 'cmd/**' 'cmd/agc'
expect_covers covers-nested-module 'test/**' 'test/fakegithub'
expect_covers covers-everything '**' 'cmd/agc'

# A bare directory path matches the literal path and nothing beneath it: picomatch
# does not expand a directory to its tree, so this is NOT coverage.
expect_no_cover no-cover-bare-dir 'api' 'api'
# The Q400 shape: a filter naming a subdirectory leaves the rest of the module
# ungated. manifest-validate.yml uses exactly this pattern, deliberately.
expect_no_cover no-cover-subdir 'api/config/**' 'api'
# A sibling module is not covered by another's glob.
expect_no_cover no-cover-sibling 'broker/**' 'api'
# A prefix of the name, not of the path: 'scale/**' must not match 'scaleset'.
expect_no_cover no-cover-name-prefix 'scale/**' 'scaleset'
# An extglob prefix has an unknowable covered set, so it counts as no coverage
# rather than being assumed to match. No tracked filter uses one since Q571
# regrouped scripts/, but picomatch still accepts them.
expect_no_cover no-cover-extglob 'scripts/!(dogfood)/**' 'scripts/dogfood'

# --- 3. literal prefixes ------------------------------------------------------

expect_eq prefix-recursive-glob 'api' "$(literal_prefix 'api/**')"
expect_eq prefix-single-glob 'scripts' "$(literal_prefix 'scripts/*')"
expect_eq prefix-extglob 'scripts' "$(literal_prefix 'scripts/!(dogfood)/**')"
expect_eq prefix-literal-file 'go.work' "$(literal_prefix 'go.work')"
expect_eq prefix-nested-literal 'charts/actions-gateway/templates' "$(literal_prefix 'charts/actions-gateway/templates/**')"
# Leading-** patterns pin no path at all, so there is nothing to verify.
expect_eq prefix-leading-glob '' "$(literal_prefix '**/go.mod')"
expect_eq prefix-suffix-glob '' "$(literal_prefix '**.go')"
# A glob inside a single segment pins no whole segment either.
expect_eq prefix-partial-segment '' "$(literal_prefix 'Makefile*')"

# --- 4. the assertions fail when they should ----------------------------------

# Point the assertions at a fixture workflow dir and registry. Both are plain
# globals in the script under test, so a test can substitute them without the
# production script carrying a test-only seam.
cat >"$FIXTURE_ROOT/workflows/gate.yml" <<'YAML'
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v4
        with:
          filters: |
            code:
              - 'api/**'
              - 'go.work'
            extra:
              - 'scripts/definitely-not-a-real-script.sh'
YAML

WORKFLOW_DIR="$FIXTURE_ROOT/workflows"
WORKSPACE_FILTERS=('gate.yml:code')
NARROW_FILTERS=('gate.yml:extra')

# The Q429 failure itself: a module in the workspace that a workspace-covering
# filter does not list. One failure per uncovered module, naming the module and
# the pattern to add.
expect_assertion coverage-fails-on-missing-module 1 "does not cover go.work module 'broker'" \
	assert_module_coverage api broker
expect_assertion coverage-names-the-fix 1 "- 'broker/\*\*'" \
	assert_module_coverage broker
# A module the filter does list passes.
expect_assertion coverage-passes-when-covered 0 '' assert_module_coverage api
# Coverage is per (filter, module), so two gaps report two failures rather than
# stopping at the first — one run lists everything to add.
expect_assertion coverage-reports-every-gap 2 '' assert_module_coverage broker githubapp

# An unregistered filter must fail: that is what stops a new workflow from
# quietly shipping a filter nobody classified.
expect_assertion registry-fails-on-unregistered 1 "does not know about" \
	assert_registry_complete 'gate.yml:code' 'gate.yml:extra' 'gate.yml:surprise'
# A stale registry entry fails too, so a renamed filter cannot leave the registry
# asserting coverage on a filter that no longer exists.
expect_assertion registry-fails-on-stale 1 'declares no such filter' \
	assert_registry_complete 'gate.yml:code'
expect_assertion registry-passes-when-exact 0 '' \
	assert_registry_complete 'gate.yml:code' 'gate.yml:extra'

# A pattern whose path is gone matches nothing, silently narrowing its gate.
expect_assertion paths-fails-on-dead-path 1 'does not exist' assert_paths_live 'gate.yml:extra'
# 'api/**' and 'go.work' both resolve in this repo.
expect_assertion paths-passes-on-live-paths 0 '' assert_paths_live 'gate.yml:code'

# --- 5. shared lanes must list the same scripts/ patterns ---------------------

# Two lanes over one reusable workflow. `kindnet` names three script groups, and
# each `calico-*` fixture differs from it in exactly one way.
cat >"$FIXTURE_ROOT/workflows/lanes.yml" <<'YAML'
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v4
        with:
          filters: |
            kindnet:
              - 'cmd/**'
              - 'scripts/e2e/**'
              - 'scripts/fetch/**'
              - 'scripts/lib/**'
            calico-same:
              - 'cmd/gmc/**'
              - 'scripts/lib/**'
              - 'scripts/e2e/**'
              - 'scripts/fetch/**'
            calico-short:
              - 'cmd/gmc/**'
              - 'scripts/e2e/**'
YAML

SHARED_LANE_FILTERS=('lanes.yml:kindnet|lanes.yml:calico-same')
# Agreement is on the scripts/ patterns as a SET: listing order differs above,
# and the non-scripts patterns differ deliberately — that is what makes the
# calico lane narrower than the kindnet one everywhere but scripts/.
expect_assertion lanes-pass-when-sets-match 0 '' assert_shared_lanes_agree

# The Q571 shape: the second lane names a subset, so a scripts/fetch/ change runs
# one lane and skips the other.
SHARED_LANE_FILTERS=('lanes.yml:kindnet|lanes.yml:calico-short')
expect_assertion lanes-fail-on-subset 1 'different scripts/ patterns' assert_shared_lanes_agree
expect_assertion lanes-name-the-missing-pattern 1 'scripts/fetch/\*\*' assert_shared_lanes_agree
# Asymmetry is a failure in both directions — an extra pattern runs a lane on a
# change it does not exercise, which is how the two lists drift apart again.
SHARED_LANE_FILTERS=('lanes.yml:calico-short|lanes.yml:kindnet')
expect_assertion lanes-fail-on-superset 1 'different scripts/ patterns' assert_shared_lanes_agree

# --- 6. push-trigger paths must match the changes filter ----------------------

# The real shape: `pull_request:` bare (so `gate` always reports), a push leg
# scoped by paths, and a `changes` filter classifying PRs. `on.push.paths` is
# real YAML, not the nested string `filters:` uses, so it needs its own parser —
# these cases pin what it must and must not pick up.
cat >"$FIXTURE_ROOT/workflows/push.yml" <<'YAML'
on:
  pull_request:
  push:
    branches: [main]
    paths:
      - 'cmd/**'
      # A comment is not a path.
      - 'scripts/e2e/**'
  workflow_dispatch:
    inputs:
      runner:
        description: 'not a path'
        type: string

jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v4
        with:
          filters: |
            same:
              - 'scripts/e2e/**'
              - 'cmd/**'
            short:
              - 'cmd/**'
            extra:
              - 'cmd/**'
              - 'scripts/e2e/**'
              - 'scripts/fetch/**'
YAML

expect_eq push-paths-parsed "cmd/** scripts/e2e/**" \
	"$(parse_push_paths "$FIXTURE_ROOT/workflows/push.yml" | tr '\n' ' ' | sed 's/ $//')"
# workflow_dispatch's inputs sit deeper than `paths:` and must not leak in, and
# `branches:` is a sibling key rather than a list item.
expect_eq push-paths-count 2 \
	"$(parse_push_paths "$FIXTURE_ROOT/workflows/push.yml" | wc -l | tr -d ' ')"
# A workflow with no push-paths list yields nothing rather than erroring.
expect_eq push-paths-absent '' "$(parse_push_paths "$FIXTURE_ROOT/workflows/parse.yml")"

WORKFLOW_DIR="$FIXTURE_ROOT/workflows"
PUSH_TRIGGER_FILTERS=('push.yml:same')
# Order need not agree — the filter lists the same two paths reversed.
expect_assertion push-passes-when-sets-match 0 '' assert_push_paths_match_filter

# The Q571 regression itself: the filter gained paths the push list did not, so
# merging one of them skips the post-merge leg while every PR looked correct.
PUSH_TRIGGER_FILTERS=('push.yml:extra')
expect_assertion push-fails-on-filter-only 1 'differ from filter' assert_push_paths_match_filter
expect_assertion push-names-the-missing-path 1 'scripts/fetch/\*\*' assert_push_paths_match_filter
# And the mirror: a push-only path runs the post-merge leg on a change the PR
# leg never classified as relevant.
PUSH_TRIGGER_FILTERS=('push.yml:short')
expect_assertion push-fails-on-push-only 1 'differ from filter' assert_push_paths_match_filter

# --- 7. `**` must sit where picomatch still expands it ------------------------

# The boundary is the whole content of this assertion, so it is pinned from both
# sides. Sound shapes first — a false positive here would fail the tracked tree,
# and '**.go' is what plan-hygiene.yml's `plan` filter carries today.
expect_globstar_ok() {
	local name="$1" pattern="$2"
	if broken_globstar "$pattern"; then
		bad "$name" "'$pattern' must NOT be flagged"
	else
		ok "$name" "'$pattern' globstars"
	fi
}

# expect_globstar_broken NAME PATTERN WANT_FIX
expect_globstar_broken() {
	local name="$1" pattern="$2" want="$3"
	if ! broken_globstar "$pattern"; then
		bad "$name" "'$pattern' should be flagged"
		return
	fi
	expect_eq "$name" "$want" "$(globstar_fix "$pattern")"
}

expect_globstar_ok globstar-whole-segment 'cmd/**/*.go'
expect_globstar_ok globstar-leading-segment '**/*.go'
expect_globstar_ok globstar-leading-suffix '**.go'
expect_globstar_ok globstar-leading-named '**/go.mod'
expect_globstar_ok globstar-trailing 'api/**'
expect_globstar_ok globstar-bare '**'
expect_globstar_ok globstar-nested 'charts/**/templates/**'
# No `**` at all cannot degrade one. A lone `*` matches nothing useful here, but
# that is a different defect and not this assertion's business.
expect_globstar_ok globstar-absent '*.go'
expect_globstar_ok globstar-literal 'go.work'

# The Q659 shape: a mid-pattern `**` beside non-'/' characters.
expect_globstar_broken globstar-mid-pattern 'cmd/**.go' 'cmd/**/*.go'
expect_globstar_broken globstar-deep 'cmd/agc/**.go' 'cmd/agc/**/*.go'
# A prefix on the globstar degrades it just the same, leading or not.
expect_globstar_broken globstar-mid-prefixed 'cmd/x**/y' 'cmd/x**/*/y'
expect_globstar_broken globstar-leading-prefixed 'x**/y' 'x**/*/y'

# And the assertion itself, over a fixture holding one degraded pattern beside
# three sound controls — the injected defect must be the only thing it reports.
cat >"$FIXTURE_ROOT/workflows/globstar.yml" <<'YAML'
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v4
        with:
          filters: |
            sound:
              - 'cmd/**/*.go'
              - '**/*.go'
              - '**.go'
              - '*.go'
            degraded:
              - 'cmd/**.go'
YAML

WORKFLOW_DIR="$FIXTURE_ROOT/workflows"
expect_assertion globstar-passes-sound-patterns 0 '' \
	assert_globstar_placement 'globstar.yml:sound'
expect_assertion globstar-fails-on-degraded 1 'does not globstar' \
	assert_globstar_placement 'globstar.yml:degraded'
expect_assertion globstar-names-the-fix 1 "cmd/\*\*/\*\.go" \
	assert_globstar_placement 'globstar.yml:degraded'

# --- 8. the tracked workflows pass -------------------------------------------

# End-to-end against the real tree, in a subshell so the fixture globals above
# cannot leak into it. This is the same verdict `make path-filters-check` gives;
# it is cheap enough to assert here too.
#
# Unlike the fixtures above, this reads live state, so its failure need not be
# the condition asserted. Keep the report: "re-run it yourself" is no help in CI,
# and none at all if the cause was transient (Q596).
tracked_rc=0
tracked_out="$( (scripts/ci/check-path-filters.sh 2>&1) )" || tracked_rc=$?
die_if_killed tracked-workflows-pass "$tracked_rc"
if ((tracked_rc == 0)); then
	ok tracked-workflows-pass 'make path-filters-check is green'
else
	bad tracked-workflows-pass "scripts/ci/check-path-filters.sh exited $tracked_rc"
	printf '%s\n' "$tracked_out" >&2
fi

if ((fails > 0)); then
	printf '\ncheck-path-filters-test: %d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-path-filters-test: all assertions passed\n'
