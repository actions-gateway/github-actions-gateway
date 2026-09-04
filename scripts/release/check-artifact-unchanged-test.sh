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
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

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
	die_if_killed "$desc" "$rc" "$want"
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
else
	printf '[check-artifact-unchanged-test] SKIP docs-only case (commit or its parent absent — shallow clone)\n'
fi

# A tool that could not run is exit 2, never 1. Exit 1 means "the released surface
# moved" and callers act on it — the freeze watch retires the candidate it names —
# so a crash reported as a finding would retire one nothing ever measured. Posed
# with a clone that has no working tree, where semverfloor cannot read publish.yml.
# The clone is `--shared`, so it costs no object copy; a shallow CI clone may
# refuse it, which is a skip rather than a failure.
# The window must be non-empty: an empty diff never reaches the tool, so it would
# pass for the wrong reason.
nowt="$(mktemp -d)/nowt"
if has_parent "$head_sha" &&
	git clone --shared --quiet --no-checkout --no-tags "$REPO_ROOT" "$nowt" 2>/dev/null; then
	git -C "$nowt" config maintenance.auto false
	nowt_rc=0
	nowt_out="$( (cd "$nowt" && "$SUBJECT" "${head_sha}^" "$head_sha" 2>&1) )" || nowt_rc=$?
	die_if_killed "a semverfloor that cannot run" "$nowt_rc"
	if [[ "$nowt_rc" -eq 2 && "$nowt_out" == *"semverfloor -ships failed"* ]]; then
		ok "a semverfloor that cannot run is exit 2, not a finding"
	else
		bad "a semverfloor that cannot run is exit 2, not a finding (got exit $nowt_rc)"
		printf '       %s\n' "$nowt_out" >&2
	fi
	rm -rf "$(dirname "$nowt")"
else
	printf '[check-artifact-unchanged-test] SKIP tool-failure case (shallow clone: no parent, or clone refused)\n'
fi

# The other exit-2 branch: the tool never builds at all. Posed with a `go` on
# PATH that refuses, which is the half a no-working-tree clone cannot reach —
# there the build succeeds from the real checkout and it is `-ships` that fails.
# Asserted on the message, not the code, because both branches exit 2 and only
# the message says which one ran.
#
# Deliberately unguarded, and an empty window on purpose. The build runs before
# the diff is taken, so it is reached whatever the window holds; the `-ships`
# case above needs a real two-commit window only because an empty diff never
# reaches the tool at all. Guarding this one on `has_parent` as well — the
# obvious move, since it sits next to a case that needs it — skips it on CI's
# depth-1 clone, which is the checkout where the exit-2 class most needs a
# verdict and the one where a caller acts on the answer.
stub_bin="$(mktemp -d)/bin"
mkdir -p "$stub_bin"
printf '#!/usr/bin/env bash\nexit 1\n' >"$stub_bin/go"
chmod +x "$stub_bin/go"
build_case_rc=0
build_case_out="$( (cd "$REPO_ROOT" && PATH="$stub_bin:$PATH" \
	"$SUBJECT" "$head_sha" "$head_sha" 2>&1) )" || build_case_rc=$?
	die_if_killed "a semverfloor that cannot build" "$build_case_rc"
if [[ "$build_case_rc" -eq 2 && "$build_case_out" == *"could not build semverfloor"* ]]; then
	ok "a semverfloor that cannot build is exit 2, not a finding"
else
	bad "a semverfloor that cannot build is exit 2, not a finding (got exit $build_case_rc)"
	printf '       %s\n' "$build_case_out" >&2
fi
rm -rf "$(dirname "$stub_bin")"

printf '[check-artifact-unchanged-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
