#!/usr/bin/env bash
#
# Typecheck and vet the build-tagged Go files that no other fast gate compiles
# (Q404). `make lint` (golangci-lint) and `make test` both build the workspace
# with the DEFAULT tag set, so every `//go:build integration|e2e|load` file is
# invisible to them: a refactor can leave an unused import or a stale call
# signature in an envtest suite, `make check` passes green, and the break only
# surfaces on CI's integration/e2e leg, a path-gated tier that may not even run
# on the PR that introduced it. `go vet` typechecks the packages it analyses, so
# a tagged-tree compile break fails this gate.
#
# Three assertions, cheapest first:
#
#   1. Lint sync. .golangci.yml's run.build-tags names the same tags as
#      BUILD_TAGS. golangci-lint reads its own list, so a tag added here and
#      not there leaves every linter blind to the tagged tree — assertion 2
#      cannot see that, because it reads the Go tree rather than the lint
#      config (Q532).
#   2. Tag coverage. Every first-party .go file is compiled by SOME tag in
#      BUILD_TAGS. Asserted from `go list`'s IgnoredGoFiles, so introducing a
#      new build tag fails this gate until it is added to BUILD_TAGS below,
#      rather than silently carving another hole in the same shape as Q404.
#   3. Vet. One workspace-wide `go vet -tags "$BUILD_TAGS"` over every go.work
#      module.
#
# Needs no envtest assets, no cluster, and no test execution: this compiles and
# analyses, it does not run anything. `make test-integration` still owns actually
# running the tagged suites. Backs `make build-tags-check` (part of `make check`)
# and the `lint` job in .github/workflows/unit-test.yml.
#
# Applies the local throttle (GOMAXPROCS cap and a low-priority QoS prefix) on a
# GUI dev shell; a no-op on CI/headless, see scripts/agent/local-throttle.sh.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

# Every custom build tag first-party Go files constrain on, comma-separated for
# `go`'s -tags flag. Enabling them all at once is sound here because every tagged
# file ADDS to a build rather than replacing part of one: no file is constrained
# on the negation of another's tag, so no combination conflicts. Most select
# whole package trees of their own (envtest suites, e2e suites, the load
# harness); `autoscaler` and `karpenter` instead add live tests (plus their
# shared plumbing, constrained on either tag) to a package the default build
# already compiles, which is fine for the same reason — they only have to not
# collide with the identifiers already there. Keep it that way. If a tag ever
# needs a `!tag` counterpart file, this one-shot strategy stops working and the
# gate has to vet each tag separately.
#
# Assertion 2 keeps this list honest in the add direction: a new tag makes the
# gate fail with instructions. Assertion 1 then propagates it to .golangci.yml,
# which reads its own copy. GOOS/GOARCH/go1.x constraints do NOT belong here,
# because they are environment-controlled, and forcing e.g. `linux` on a darwin
# host would build files for the wrong platform.
BUILD_TAGS="integration,e2e,load,autoscaler,karpenter"

# The golangci-lint config whose run.build-tags must name the same tags. Both
# gates build the tagged trees, and each reads its own list.
GOLANGCI_CONFIG="$REPO_ROOT/.golangci.yml"

# lint_build_tags CONFIG — print CONFIG's run.build-tags entries, one per line.
# Anchored on the `run:` block: a top-level key ends it, so a build-tags list
# belonging to some other section is not mistaken for this one. Prints nothing
# when the key is absent, which the caller treats as a failure rather than as an
# empty list — two empty lists compare equal, which is how a gate of this shape
# goes vacuously green.
lint_build_tags() {
	awk '
		/^[^[:space:]#]/ { in_run = ($0 ~ /^run:[[:space:]]*$/); in_tags = 0; next }
		in_run && /^[[:space:]]+build-tags:[[:space:]]*$/ { in_tags = 1; next }
		in_tags {
			if ($0 ~ /^[[:space:]]*(#|$)/) { next }
			if ($0 !~ /^[[:space:]]+-[[:space:]]+/) { in_tags = 0; next }
			sub(/^[[:space:]]+-[[:space:]]+/, "")
			sub(/[[:space:]]*#.*$/, "")
			gsub(/[[:space:]"\047]/, "")
			if (length($0) > 0) { print }
		}
	' "$1"
}

# assert_lint_tags_match TAGS CONFIG — fail unless CONFIG's run.build-tags names
# exactly the comma-separated TAGS. golangci-lint compiles the tagged trees from
# its own list (Q430), so a tag added to BUILD_TAGS and not to CONFIG leaves
# gosec/errcheck/staticcheck/unused blind to every file behind it — the Q404
# blind spot in the linters rather than in vet. assert_tags_cover_tree cannot
# report it: it reads the Go tree, where the tag is now covered.
#
# Both lists are required non-empty. A comparison of two empty sets is the
# failure mode this gate is most likely to have, because it succeeds silently
# (Q532).
assert_lint_tags_match() {
	local tags="$1" config="$2" want got missing extra
	want="$(tr ',' '\n' <<<"$tags" | awk 'NF' | sort)"
	if [[ -z "$want" ]]; then
		echo "ERROR: BUILD_TAGS is empty in ${BASH_SOURCE[0]}, so this gate would vet and lint nothing." >&2
		exit 1
	fi
	if [[ ! -r "$config" ]]; then
		echo "ERROR: cannot read $config, so the build tags golangci-lint applies are unknown." >&2
		exit 1
	fi
	got="$(lint_build_tags "$config" | sort)"
	if [[ -z "$got" ]]; then
		echo "ERROR: no run.build-tags entries found in $config." >&2
		echo >&2
		echo "golangci-lint builds the workspace with the default tag set unless run.build-tags" >&2
		echo "lists them, so every //go:build-constrained file would go unlinted (Q430). Restore" >&2
		echo "the list to match BUILD_TAGS in scripts/go/go-vet-tags.sh:" >&2
		tr ',' '\n' <<<"$tags" | awk '{ printf "    - %s\n", $0 }' >&2
		exit 1
	fi
	[[ "$want" == "$got" ]] && return 0

	missing="$(comm -23 <(printf '%s\n' "$want") <(printf '%s\n' "$got") | paste -sd, -)"
	extra="$(comm -13 <(printf '%s\n' "$want") <(printf '%s\n' "$got") | paste -sd, -)"
	echo "ERROR: $config run.build-tags does not match BUILD_TAGS in scripts/go/go-vet-tags.sh:" >&2
	[[ -n "$missing" ]] && echo "  missing from the lint config: $missing" >&2
	[[ -n "$extra" ]] && echo "  listed only in the lint config:  $extra" >&2
	echo >&2
	echo "golangci-lint reads its own list, so a tag it omits leaves gosec/errcheck/" >&2
	echo "staticcheck/unused blind to every file behind that tag (Q430). Add the tag to" >&2
	echo "run.build-tags, or drop it from BUILD_TAGS if the tag is gone." >&2
	exit 1
}

# assert_tags_cover_tree TAGS PATTERN... — fail unless TAGS make every first-party
# .go file under PATTERNs buildable. go list reports the files a package's build
# constraints excluded in IgnoredGoFiles; with the full tag set applied that list
# must be empty everywhere, so anything it names is a file this gate would skip.
assert_tags_cover_tree() {
	local tags="$1"
	shift
	local ignored
	ignored="$(go list -tags "$tags" \
		-f '{{if .IgnoredGoFiles}}{{.ImportPath}}: {{.IgnoredGoFiles}}{{end}}' "$@")"
	[[ -z "$ignored" ]] && return 0
	echo "ERROR: build constraints exclude first-party Go files even with -tags $tags:" >&2
	echo "$ignored" >&2
	echo >&2
	echo "Every first-party .go file must be compiled by this gate (Q404). Either add the" >&2
	echo "new tag to BUILD_TAGS in scripts/go/go-vet-tags.sh, or, if the constraint is" >&2
	echo "environment-controlled (GOOS/GOARCH/go1.x) and cannot be forced on, exempt the" >&2
	echo "file here with a comment saying why." >&2
	exit 1
}

main() {
	# Serialize against a concurrent heavy build on this machine (no-op on
	# CI/headless) so sibling runs queue instead of saturating the cores; re-execs
	# self under a machine-wide lock.
	serialize_heavy_build "$@"

	# One `./<module>/...` pattern per go.work module. A repo-root `./...` does
	# not work in a workspace, but explicit per-module patterns do, and a single
	# invocation lets Go schedule the whole workspace as one build graph (the
	# same trick scripts/go/go-test.sh uses).
	local patterns=() dir
	for dir in $(workspace_modules); do
		patterns+=("$dir/...")
	done

	assert_lint_tags_match "$BUILD_TAGS" "$GOLANGCI_CONFIG"
	assert_tags_cover_tree "$BUILD_TAGS" "${patterns[@]}"

	init_throttle
	[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
	echo "==> go vet -tags $BUILD_TAGS ${patterns[*]}"
	# shellcheck disable=SC2086  # the throttle prefix word-splits intentionally
	$THROTTLE_PREFIX go vet -tags "$BUILD_TAGS" "${patterns[@]}"
}

# Run main only when executed directly, so go-vet-tags-test.sh can source this
# file to exercise assert_tags_cover_tree and assert_lint_tags_match against
# scratch fixtures.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
