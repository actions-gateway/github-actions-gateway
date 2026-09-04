#!/usr/bin/env bash
#
# Unit tests for scripts/go/go-vet-tags.sh (Q404). Three properties, each
# asserted against a scratch fixture rather than the tracked tree so a case can
# actually be broken:
#
#   1. The tag-coverage guard fails on a build tag BUILD_TAGS does not list, and
#      names the file. Otherwise a newly-introduced tag would reopen exactly the
#      hole this gate closes.
#   2. `go vet -tags` catches a compile break in a tagged file that an untagged
#      vet, which is what `make lint` and `make test` see, reports clean. That is
#      the Q404 failure itself (an unused import in an `integration`-tagged test
#      passed `make check` and failed CI's integration leg) in permanent form.
#   3. The lint-sync guard fails when .golangci.yml's run.build-tags and
#      BUILD_TAGS disagree in either direction, and when the key is absent
#      altogether. That last case is the one worth the fixtures: a gate
#      comparing two lists is vacuously green the moment both read as empty,
#      which is the shape that let the lists drift unwatched (Q532).
#
# The gate is only worth having if it fails when it should, so these are the
# standing form of the invert-the-fix verification
# (docs/development/testing.md § Diagnosing failures). Runs under `make check`
# (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
# Source the script under test for BUILD_TAGS and assert_tags_cover_tree; the
# BASH_SOURCE guard there keeps main() from running (and from vetting the whole
# workspace) on source.
# shellcheck source=scripts/go/go-vet-tags.sh
source "$REPO_ROOT/scripts/go/go-vet-tags.sh"

FIXTURE_ROOT="$REPO_ROOT/tmp/go-vet-tags-test.$$"
trap 'rm -rf "$FIXTURE_ROOT"' EXIT INT TERM

fails=0

# fixture NAME TAG — create a scratch module whose pkg/ holds one untagged file
# (so the package always has something to build) plus one TAG-constrained file
# carrying an unused import, and echo the module root.
#
# The fixture carries its OWN go.work. `go` resolves the nearest go.work walking
# up from the working directory, so running inside the fixture picks that one up
# instead of the repo's, and the scratch module is a workspace of one. That is
# what keeps `go` from rejecting it as a module missing from the repo workspace,
# with no GOWORK env var to thread through the subshells below.
fixture() {
	local name="$1" tag="$2" go_version
	local root="$FIXTURE_ROOT/$name"
	go_version="$(go work edit -json | jq -r '.Go')"
	mkdir -p "$root/pkg"
	printf 'module q404scratch\n\ngo %s\n' "$go_version" >"$root/go.mod"
	printf 'go %s\n\nuse .\n' "$go_version" >"$root/go.work"
	printf 'package pkg\n\n// Untagged, always built.\nfunc Untagged() {}\n' >"$root/pkg/untagged.go"
	printf '//go:build %s\n\npackage pkg\n\nimport "os"\n\nfunc Tagged() {}\n' "$tag" >"$root/pkg/tagged.go"
	printf '%s' "$root"
}

# expect_coverage NAME WANT_RC ROOT — run the tag-coverage guard over the scratch
# module with the real BUILD_TAGS and assert its exit code. Output is captured so
# a later assertion can grep it.
expect_coverage() {
	local name="$1" want_rc="$2" root="$3" got_rc=0
	LAST_OUT="$(
		cd "$root"
		assert_tags_cover_tree "$BUILD_TAGS" ./... 2>&1
	)" || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-34s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-34s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

# expect_output NAME PATTERN — assert the last run's output mentions PATTERN.
expect_output() {
	local name="$1" pattern="$2"
	if grep -q -- "$pattern" <<<"$LAST_OUT"; then
		printf 'ok   %-34s reported %q\n' "$name" "$pattern"
	else
		printf 'FAIL %-34s output did not mention %q\n%s\n' "$name" "$pattern" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

# expect_vet NAME WANT_RC ROOT [TAGS] — run `go vet` over the scratch module,
# with TAGS when given and with the default tag set when not, and assert the exit
# code. Subshell-wrapped so the parent cwd and env cannot drift.
expect_vet() {
	local name="$1" want_rc="$2" root="$3" tags="${4:-}" got_rc=0
	local out
	out="$(
		cd "$root"
		if [[ -n "$tags" ]]; then
			go vet -tags "$tags" ./... 2>&1
		else
			go vet ./... 2>&1
		fi
	)" || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-34s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-34s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$out" >&2
		fails=$((fails + 1))
	fi
}

# --- 1. tag coverage ---------------------------------------------------------

# A tag BUILD_TAGS lists leaves nothing excluded: the guard passes.
listed="$(fixture listed-tag integration)"
expect_coverage listed-tag-covered 0 "$listed"

# A tag BUILD_TAGS does not list leaves the file uncompiled, which is the Q404
# hole in a new shape. Fail, and name the file so the fix is obvious.
unlisted="$(fixture unlisted-tag q404unlisted)"
expect_coverage unlisted-tag-fails 1 "$unlisted"
expect_output unlisted-tag-names-file 'tagged.go'
expect_output unlisted-tag-explains 'BUILD_TAGS'

# --- 2. vet sees tagged files ------------------------------------------------

# The default tag set, which is what `make lint` and `make test` build, cannot
# see the broken file at all, so it reports clean. This is why Q404 reached CI.
expect_vet untagged-vet-misses-break 0 "$listed"
# With the tag applied, the same tree fails to typecheck.
expect_vet tagged-vet-catches-break 1 "$listed" "$BUILD_TAGS"

# --- 3. lint config tag sync -------------------------------------------------

# lint_config NAME TAG... — write a scratch golangci-lint config whose
# run.build-tags holds TAGs (none writes the key with an empty list, which YAML
# reads as null), and echo its path. A decoy `build-tags:` sits under a
# different top-level key on either side of `run:`, so every fixture also proves
# the parser is anchored on `run:` rather than taking the first list it sees or
# the last.
lint_config() {
	local name="$1"
	shift
	local path="$FIXTURE_ROOT/$name.yml" tag
	mkdir -p "$FIXTURE_ROOT"
	{
		printf 'version: "2"\n'
		printf 'formatters:\n  build-tags:\n    - q532decoybefore\n'
		printf 'run:\n  timeout: 10m\n  build-tags:\n'
		for tag in "$@"; do
			printf '    - %s\n' "$tag"
		done
		printf 'linters:\n  default: none\n  build-tags:\n    - q532decoyafter\n'
	} >"$path"
	printf '%s' "$path"
}

# expect_lint_sync NAME WANT_RC CONFIG [TAGS] — run the lint-sync guard against
# CONFIG, with TAGS when given and the real BUILD_TAGS when not, and assert its
# exit code. Output is captured so expect_output can grep it.
expect_lint_sync() {
	local name="$1" want_rc="$2" config="$3" tags="${4-$BUILD_TAGS}" got_rc=0
	LAST_OUT="$(assert_lint_tags_match "$tags" "$config" 2>&1)" || got_rc=$?
	die_if_killed "$name" "$got_rc" "$want_rc"
	if [[ "$got_rc" == "$want_rc" ]]; then
		printf 'ok   %-34s rc=%s\n' "$name" "$got_rc"
	else
		printf 'FAIL %-34s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$LAST_OUT" >&2
		fails=$((fails + 1))
	fi
}

# BUILD_TAGS as an array, so the fixtures below are built from the real list and
# cannot drift from it.
IFS=',' read -r -a build_tag_list <<<"$BUILD_TAGS"

# The same set in the same order: nothing to report.
in_sync_config="$(lint_config in-sync "${build_tag_list[@]}")"
expect_lint_sync in-sync 0 "$in_sync_config"

# Order is not meaning in YAML, and a reordered list must not read as drift —
# otherwise the gate fails on an edit that changed nothing.
reordered=("${build_tag_list[@]:1}" "${build_tag_list[0]}")
expect_lint_sync reordered-still-in-sync 0 "$(lint_config reordered "${reordered[@]}")"

# A tag BUILD_TAGS carries and the lint config drops: every linter goes blind to
# the files behind it while `go vet` still covers them, which is why the
# coverage guard cannot report it. This is Q532 itself.
expect_lint_sync lint-missing-a-tag 1 "$(lint_config missing "${build_tag_list[@]:1}")"
expect_output lint-missing-names-tag "${build_tag_list[0]}"
expect_output lint-missing-explains 'missing from the lint config'

# The other direction: a tag the lint config lists and BUILD_TAGS does not. Vet
# then skips files golangci-lint builds, and BUILD_TAGS is the list to fix.
expect_lint_sync lint-has-extra-tag 1 "$(lint_config extra "${build_tag_list[@]}" q532extra)"
expect_output lint-extra-names-tag 'q532extra'
expect_output lint-extra-explains 'listed only in the lint config'

# The vacuous case: no entries at all. Two empty lists compare equal, so a guard
# written the obvious way passes here and reports nothing for as long as the key
# stays gone.
expect_lint_sync lint-empty-list-fails 1 "$(lint_config empty)"
expect_output lint-empty-explains 'no run.build-tags entries'

# An unreadable config is the same hole reached by a different route: the tag
# set golangci-lint applies is unknown, which is not the same as matching.
expect_lint_sync lint-unreadable-fails 1 "$FIXTURE_ROOT/does-not-exist.yml"
expect_output lint-unreadable-explains 'cannot read'

# The same hole from the other side: an empty BUILD_TAGS against a populated
# config. Each list needs its own emptiness guard, because one comparison of two
# empty sets is exactly the answer that reports nothing.
expect_lint_sync empty-build-tags-fails 1 "$in_sync_config" ''
expect_output empty-build-tags-explains 'BUILD_TAGS is empty'

# --- 4. the tracked tree passes ----------------------------------------------

# Coverage only: the full workspace vet is `make build-tags-check`, and running
# it here would make `make scripts-test` a heavy build.
tree_patterns=()
while IFS= read -r module_dir; do
	tree_patterns+=("$module_dir/...")
done < <(workspace_modules)
#
# Unlike the fixtures above, this reads the live tree, so its failure need not be
# the condition asserted. Keep the report: "re-run it yourself" is no help in CI,
# and none at all if the cause was transient (Q596).
tree_rc=0
tree_out="$(assert_tags_cover_tree "$BUILD_TAGS" "${tree_patterns[@]}" 2>&1)" || tree_rc=$?
die_if_killed tree-fully-covered "$tree_rc"
if ((tree_rc == 0)); then
	printf 'ok   %-34s tracked workspace, -tags %s\n' tree-fully-covered "$BUILD_TAGS"
else
	printf 'FAIL %-34s a tracked first-party .go file is excluded even with -tags %s (rc=%d); run %s\n' \
		tree-fully-covered "$BUILD_TAGS" "$tree_rc" "$REPO_ROOT/scripts/go/go-vet-tags.sh" >&2
	printf '%s\n' "$tree_out" >&2
	fails=$((fails + 1))
fi

# The live .golangci.yml against the live BUILD_TAGS — the assertion the gate
# actually makes on every run. Unlike the fixtures above this reads the tracked
# tree, so keep its output: "re-run it yourself" is no help in CI (Q596).
tracked_rc=0
tracked_out="$(assert_lint_tags_match "$BUILD_TAGS" "$GOLANGCI_CONFIG" 2>&1)" || tracked_rc=$?
die_if_killed tracked-lint-config-in-sync "$tracked_rc"
if ((tracked_rc == 0)); then
	printf 'ok   %-34s %s\n' tracked-lint-config-in-sync "$GOLANGCI_CONFIG"
else
	printf 'FAIL %-34s rc=%d\n' tracked-lint-config-in-sync "$tracked_rc" >&2
	printf '%s\n' "$tracked_out" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\ngo-vet-tags-test: %d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ngo-vet-tags-test: all assertions passed\n'
