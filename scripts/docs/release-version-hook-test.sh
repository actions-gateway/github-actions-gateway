#!/usr/bin/env bash
#
# Unit tests for hooks/release_version.py — the MkDocs hook that derives the
# docs-site announce bar's release from the git tags (Q393). The banner is
# published permanently under a stable tag, so the tag it names has to be right
# the first time; this asserts that selection in hermetic throwaway repos so no
# assumption about the caller's tree leaks in. Runs under `make check` (via
# `make scripts-test`) and the CI shellcheck job.
#
# python3 is an extended-tier prerequisite (scripts/ci/check-tools.sh), not a
# required one, so this skips rather than fails when it is absent. CI runners
# always have it.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly HOOK="$REPO_ROOT/hooks/release_version.py"

if ! command -v python3 >/dev/null 2>&1; then
	printf 'skip release-version-hook-test: python3 not found (extended tier, scripts/ci/check-tools.sh)\n'
	exit 0
fi

WORKDIR="$(mktemp -d)"
readonly WORKDIR
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# A throwaway repo carrying exactly the tags a case needs. Git identity is set
# locally (never --global) so the harness branch-guard is happy.
setup_repo() {
	local d="$WORKDIR/repo" tag
	rm -rf "$d"
	mkdir -p "$d"
	(
		cd "$d"
		git init -q -b main
		git config user.email t@t.t
		git config user.name t
		git commit -q --allow-empty -m base
		for tag in "$@"; do
			git tag "$tag"
		done
	)
	printf '%s\n' "$d"
}

# expect NAME WANT TAG...  — resolve in a repo carrying TAG..., compare to WANT.
expect() {
	local name="$1" want="$2" d got
	shift 2
	d="$(setup_repo "$@")"
	got="$(GAG_DOCS_RELEASE='' python3 "$HOOK" "$d")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: want=%q got=%q\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

expect 'single stable tag                -> that tag' v1.2.0 v1.2.0
expect 'stable beats its own prerelease  -> stable' v1.2.0 v1.2.0-rc.1 v1.2.0
expect 'newest of several stable tags' v1.3.0 v1.1.0 v1.3.0 v1.2.0
# Lexical sort would pick v1.9.0 here; the hook compares numerically.
expect 'v1.10.0 outranks v1.9.0' v1.10.0 v1.9.0 v1.10.0
expect 'major ordering is numeric too' v10.0.0 v9.9.9 v10.0.0
# The prerelease test mirrors publish.yml/pages.yml: 0.x core, or any '-' suffix.
expect 'prereleases only                 -> empty' '' v1.0.0-rc.1 v2.0.0-beta v3.0.0-alpha.2
expect '0.x is a prerelease              -> empty' '' v0.9.0 v0.10.0
expect 'no tags at all                   -> empty' ''
expect 'non-release tags are ignored' v1.0.0 v1.0.0 nightly v1.2 v1.2.3.4 release-2

# $GAG_DOCS_RELEASE wins over the tags: the escape hatch for a build with no git
# history, and how the template is exercised by hand.
d="$(setup_repo v1.2.0)"
got="$(GAG_DOCS_RELEASE=' v9.9.9 ' python3 "$HOOK" "$d")"
if [[ "$got" == "v9.9.9" ]]; then
	printf 'ok   %s\n' 'GAG_DOCS_RELEASE overrides the tags (and is trimmed)'
else
	printf 'FAIL %s: want=%q got=%q\n' 'GAG_DOCS_RELEASE overrides the tags' v9.9.9 "$got" >&2
	fails=$((fails + 1))
fi

# A path that is not a git repository at all must degrade to "", not raise: the
# template drops the version claim rather than guessing.
got="$(GAG_DOCS_RELEASE='' python3 "$HOOK" "$WORKDIR/does-not-exist")"
if [[ -z "$got" ]]; then
	printf 'ok   %s\n' 'no git repository -> empty'
else
	printf 'FAIL %s: want empty got=%q\n' 'no git repository' "$got" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\n%d release-version-hook assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nrelease-version-hook-test: all assertions passed\n'
