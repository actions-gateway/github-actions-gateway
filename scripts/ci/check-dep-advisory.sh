#!/usr/bin/env bash
#
# check-dep-advisory.sh - Print a one-line reminder when a change touches Go
# dependency files, and stay silent otherwise.
#
# `make check` is the fast pre-review gate, but it deliberately does NOT run the
# three dependency-drift gates — vendor-check and tidy-check can hit the network
# on a cold cache, and all three already run as their own CI jobs (unit-test.yml
# vendor-check/tidy-check, license-notices.yml). The consequence is a genuine
# surprise: a change that edits go.mod/go.sum/vendor/go.work* can pass a green
# `make check` locally and then fail unit-test.yml on push, one round-trip later.
#
# This script closes that gap the cheap way — advice, not a gate. It inspects the
# working tree, the index, and this branch's commits that aren't on the base for
# any dependency-file change, and if it finds one, prints a single line pointing
# at `make vendor-sync` (the one-shot remedy) and the CI gates that will judge it.
# It NEVER fails: it is invoked as the last step of `make check` and must not turn
# a green gate red. All git calls are read-only and offline (no fetch).
#
# Usage: scripts/ci/check-dep-advisory.sh   (invoked by `make check`)
set -euo pipefail
shopt -s inherit_errexit

# Dependency files whose drift the fast gate can't see but CI will. Extended-glob
# alternation, matched against repo-relative paths in a `case` (see below).
#
# Two `case`-matching subtleties this pattern is written around:
#   - `**` has no special meaning in `case` (globstar governs pathname expansion
#     only), so `**/go.mod` collapses to `*/go.mod` and would MISS a repo-root
#     `go.mod`. `?(*/)go.mod` matches both root and nested — `?(*/)` is an
#     optional directory prefix, and `*` matches embedded slashes in a `case`.
#   - `*` matching slashes is why `vendor/*` covers `vendor/a/b/c`, no `**` needed.
shopt -s extglob
DEP_GLOB='@(go.work|go.work.sum|vendor/modules.txt|THIRD-PARTY-NOTICES|?(*/)go.mod|?(*/)go.sum|?(*/)go.work.gen|vendor/*|tools/vendor/*)'

# Bail out silently on anything unexpected — not a git repo, detached HEAD, no
# base ref. The advisory is a nicety; its absence must never break a build.
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	exit 0
fi

# Collect candidate changed paths from four offline sources:
#   1. unstaged working-tree edits            (git diff)
#   2. staged edits                           (git diff --cached)
#   3. untracked, non-ignored files           (git ls-files --others) — a brand
#      new module's go.mod is a real dep change git diff can't see yet
#   4. commits on this branch not on the base (git diff <base>...HEAD)
# The base is origin/main when that ref exists locally (it usually does after the
# fetch every session's workflow starts with); we never fetch here. If it is
# absent, sources 1-3 alone still catch the common "uncommitted dep edit" case.
collect() {
	git diff --name-only 2>/dev/null || true
	git diff --cached --name-only 2>/dev/null || true
	git ls-files --others --exclude-standard 2>/dev/null || true
	local base
	for base in origin/main origin/master; do
		if git rev-parse --verify --quiet "$base" >/dev/null 2>&1; then
			git diff --name-only "${base}...HEAD" 2>/dev/null || true
			break
		fi
	done
}

# Deduplicate, then keep only paths matching the dependency glob.
changed_dep_files=()
while IFS= read -r path; do
	[[ -n "$path" ]] || continue
	# shellcheck disable=SC2254  # DEP_GLOB is an intentional extglob pattern.
	case "$path" in
		$DEP_GLOB) changed_dep_files+=("$path") ;;
	esac
done < <(collect | sort -u)

(( ${#changed_dep_files[@]} > 0 )) || exit 0

# One advisory line (plus the offending files), to stderr so it can't be mistaken
# for a gate's machine-readable stdout. The backticks are literal `make ...`
# names, not command substitutions.
# shellcheck disable=SC2016
{
	printf '\n'
	printf 'note: this change touches Go dependency files, which `make check` does NOT gate.\n'
	printf '      Run `make vendor-sync` before pushing — CI runs vendor-check, tidy-check,\n'
	printf '      and license-notices (outside the fast gate); a green `make check` does not\n'
	printf '      imply they pass. Files:\n'
	printf '        %s\n' "${changed_dep_files[@]}"
} >&2

exit 0
