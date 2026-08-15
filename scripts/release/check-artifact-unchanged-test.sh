#!/usr/bin/env bash
# check-artifact-unchanged-test.sh — asserts both directions of the surface check.
#
# A leftover-detection check that only ever answers "changed" would pass every
# release and catch nothing, so each case here pins one direction against a fixture
# repo whose answer is known by construction.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/check-artifact-unchanged.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

pass=0
fail=0
ok() {
	printf '[check-artifact-unchanged-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[check-artifact-unchanged-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

# The subject derives the surface from publish.yml, so the fixture is this repo at
# two of its own commits rather than a synthetic tree: a synthetic repo would have
# no publish.yml to derive from, and stubbing one would test the stub.
run_case() {
	local desc="$1" want="$2" from="$3" to="$4"
	local out rc
	out="$(cd "$REPO_ROOT" && "$SUBJECT" "$from" "$to" 2>&1)" && rc=0 || rc=$?
	if [[ "$rc" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc (want exit $want, got $rc)"
		printf '       %s\n' "$out" >&2
	fi
}

# Usage errors are exit 2, distinct from a real finding, so a caller cannot read a
# broken invocation as "the artifact changed".
run_case "no arguments is a usage error"        2 "" ""
run_case "a ref that is not a commit is exit 2" 2 "definitely-not-a-ref" "HEAD"

# Same commit both ends: an empty diff cannot contain a shipped file.
head_sha="$(cd "$REPO_ROOT" && git rev-parse HEAD)"
run_case "an empty window is unchanged" 0 "$head_sha" "$head_sha"

# has_parent COMMIT — is this commit's parent actually in the clone?
#
# CI checks out at depth 1 (actions/checkout defaults there), so a commit's
# parent is usually absent and `COMMIT^` does not resolve. The two cases below
# need a real two-commit window, so they say they were skipped rather than
# failing on the clone's shape or, worse, passing for the wrong reason.
has_parent() {
	git -C "$REPO_ROOT" rev-parse --verify --quiet "${1}^{commit}" >/dev/null 2>&1 &&
		git -C "$REPO_ROOT" rev-parse --verify --quiet "${1}^^{commit}" >/dev/null 2>&1
}

# Direction that matters most: a markdown file that ships. charts/ is a packaged
# tree, so a chart README is on the surface while carrying no semver weight — the
# case that separates this check from the semver floor.
chart_readme_commit="$(cd "$REPO_ROOT" && git log -1 --format=%H -- charts/actions-gateway/README.md 2>/dev/null || true)"
if [[ -n "$chart_readme_commit" ]] && has_parent "$chart_readme_commit"; then
	run_case "a chart README counts as shipped" 1 "${chart_readme_commit}^" "$chart_readme_commit"
else
	printf '[check-artifact-unchanged-test] SKIP chart README case (commit or its parent absent — shallow clone)\n'
fi

# And the negative: a docs page outside every packaged tree must not trip it.
doc_commit="$(cd "$REPO_ROOT" && git log -1 --format=%H -- docs/development/testing.md 2>/dev/null || true)"
if [[ -n "$doc_commit" ]] && has_parent "$doc_commit"; then
	changed="$(cd "$REPO_ROOT" && git diff --name-only "${doc_commit}^..${doc_commit}")"
	if [[ "$(printf '%s\n' "$changed" | grep -cv '^docs/' || true)" == "0" ]]; then
		run_case "a docs-only commit is unchanged" 0 "${doc_commit}^" "$doc_commit"
	else
		printf '[check-artifact-unchanged-test] SKIP docs-only case (newest such commit is mixed)\n'
	fi
fi

printf '[check-artifact-unchanged-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
