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
# Two assertions, cheapest first:
#
#   1. Tag coverage. Every first-party .go file is compiled by SOME tag in
#      BUILD_TAGS. Asserted from `go list`'s IgnoredGoFiles, so introducing a
#      new build tag fails this gate until it is added to BUILD_TAGS below,
#      rather than silently carving another hole in the same shape as Q404.
#   2. Vet. One workspace-wide `go vet -tags "$BUILD_TAGS"` over every go.work
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
# Assertion 1 keeps this list honest in the add direction: a new tag makes the
# gate fail with instructions. GOOS/GOARCH/go1.x constraints do NOT belong here,
# because they are environment-controlled, and forcing e.g. `linux` on a darwin
# host would build files for the wrong platform.
BUILD_TAGS="integration,e2e,load,autoscaler,karpenter"

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

	assert_tags_cover_tree "$BUILD_TAGS" "${patterns[@]}"

	init_throttle
	[[ -n "$THROTTLE_JOBS" ]] && export GOMAXPROCS="$THROTTLE_JOBS"
	echo "==> go vet -tags $BUILD_TAGS ${patterns[*]}"
	# shellcheck disable=SC2086  # the throttle prefix word-splits intentionally
	$THROTTLE_PREFIX go vet -tags "$BUILD_TAGS" "${patterns[@]}"
}

# Run main only when executed directly, so go-vet-tags-test.sh can source this
# file to exercise assert_tags_cover_tree against a scratch module.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
