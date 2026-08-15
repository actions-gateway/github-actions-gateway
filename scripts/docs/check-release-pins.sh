#!/usr/bin/env bash
#
# check-release-pins.sh — fail when an install/upgrade page still pins a release
# the project has moved past (Q638).
#
# A release publishes a chart version, an image tag, and a release-notes URL, and
# the adopter-facing pages transcribe all three by hand. Nothing bumped them: after
# v1.3.0 shipped, README.md, docs/index.md and install.md had been fixed by hand
# while upgrade.md and gitops.md still told operators to install 1.2.0 — a reader
# following either page installed a release behind, digests and all. The runbook
# step said "X.Y.Z" and named no files, so the bump was remembered, not run.
#
# The pin-bearing set below is the whole answer to "which pages tell a reader
# which version to install". It is deliberately small: this gate asserts that
# *every* release-version literal in those files names the current release, which
# is only tractable because their noise floor is two literals (both exempted
# below). Do not widen it to docs/ at large — troubleshooting.md and release.md
# are full of legitimate "before v1.3.0" history, and an exemption list that size
# would hide the drift it exists to catch.
#
# The current release is the highest stable vX.Y.Z tag, resolved from local tags,
# then from the origin remote (a shallow CI checkout has no tags), then from
# $GAG_RELEASE_TAG. Prerelease tags never publish as "the release operators run",
# so they are excluded — the same test hooks/release_version.py applies to the
# docs-site announce bar.
#
# Usage:
#   check-release-pins.sh [file...]      # defaults to the pin-bearing set
#   GAG_RELEASE_TAG=v9.9.9 check-release-pins.sh
#
# Runs under `make release-pins-check` (part of `make check`) and the doc-links.yml
# CI workflow. Assertions: scripts/docs/check-release-pins-test.sh.

set -euo pipefail
shopt -s inherit_errexit

# The library is resolved from this script's own location, not from the git root
# below: the root is whatever tree the gate is pointed at, which the test suite
# scopes to a throwaway repo that has no scripts/lib/.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$SCRIPT_DIR/../lib/common.sh"

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

# Every page that tells a reader which version to install. A new one belongs
# here the day it grows its first pin.
DEFAULT_PIN_FILES=(
    README.md
    docs/index.md
    docs/operations/install.md
    docs/operations/upgrade.md
    docs/operations/gitops.md
)

# Versions in the pin-bearing set that are deliberately not the current release,
# and the extractor that finds every literal — both in scripts/lib/common.sh, so
# verify-published-docs.sh means the same thing by "a pin" when it reads the
# rendered pages this gate's files publish as (Q784). Lines whose versions record
# a measurement rather than a pin are skipped there too.
EXEMPT_VERSIONS_RE="$(release_pin_exempt_versions_regexp)"

if (( $# > 0 )); then
    pin_files=("$@")
else
    pin_files=()
    for f in "${DEFAULT_PIN_FILES[@]}"; do
        pin_files+=("$repo_root/$f")
    done
fi

for f in "${pin_files[@]}"; do
    if [[ ! -f "$f" ]]; then
        printf 'check-release-pins: file not found: %s\n' "$f" >&2
        exit 2
    fi
done

# The highest stable vX.Y.Z tag, and where it was read from — resolve_release_tag
# in scripts/lib/common.sh, shared with check-roadmap.sh so both gates mean the
# same thing by "the current release".
IFS=$'\t' read -r release_tag tag_source < <(resolve_release_tag "$repo_root") || true

if [[ -z "${release_tag:-}" ]]; then
    printf 'check-release-pins: SKIP — no stable vX.Y.Z tag locally or on origin, so there is\n'
    printf '                    no released version to pin (expected on a fresh fork). Nothing\n'
    printf '                    was checked. Set GAG_RELEASE_TAG to check against a version.\n'
    exit 0
fi

release_version="${release_tag#v}"
release_minor="${release_version%.*}"

# A release with a candidate tagged and no stable tag yet may be pinned in
# advance, so the bump can land BEFORE the tag. The site builds each version from
# its own tag, so a bump landing after it never reaches that release's published
# page — which is how three of the four releases since 1.0.0 published the
# previous version's install command as their landing page.
#
# Both versions are accepted while a candidate is outstanding rather than only
# the prepared one: requiring the bump the instant an RC is tagged would redden
# main during the freeze, which is the same problem moved earlier. What forces
# the bump is the pre-tag check in the release runbook, where being wrong is
# still cheap; this gate's job is catching a genuinely stale pin.
prepared_tag="$(resolve_prepared_release "$repo_root")"
prepared_version="${prepared_tag#v}"
prepared_minor="${prepared_version%.*}"

if [[ -n "$prepared_tag" ]]; then
    printf 'check-release-pins: current release %s (chart version %s), from %s; %s is prepared and may be pinned early\n' \
        "$release_tag" "$release_version" "$tag_source" "$prepared_tag"
else
    printf 'check-release-pins: current release %s (chart version %s), from %s\n' \
        "$release_tag" "$release_version" "$tag_source"
fi

fail=0
total=0

for f in "${pin_files[@]}"; do
    rel="${f#"$repo_root"/}"
    found=0
    while IFS=$'\t' read -r line_no tok kind; do
        [[ -n "$tok" ]] || continue
        if [[ "$tok" =~ $EXEMPT_VERSIONS_RE ]]; then
            continue
        fi
        found=$((found + 1))
        if [[ "$kind" == "patchline" ]]; then
            want="${release_minor}.z"
            [[ "${tok#v}" == "$want" ]] && continue
            [[ -n "$prepared_tag" && "${tok#v}" == "${prepared_minor}.z" ]] && continue
            printf "check-release-pins: %s:%s: patch-line hint \`%s\` names an old release; the current line is \`%s\`%s\n" \
                "$rel" "$line_no" "$tok" "$want" \
                "${prepared_tag:+ (or \`${prepared_minor}.z\` for the prepared ${prepared_tag})}" >&2
        else
            [[ "${tok#v}" == "$release_version" ]] && continue
            [[ -n "$prepared_tag" && "${tok#v}" == "$prepared_version" ]] && continue
            printf 'check-release-pins: %s:%s: pins %s, but the current release is %s%s\n' \
                "$rel" "$line_no" "$tok" "$release_tag" \
                "${prepared_tag:+ (or ${prepared_tag}, prepared)}" >&2
        fi
        fail=1
    done < <(release_version_literals "$f")

    # An empty result cannot tell "this page has no stale pin" from "the pin
    # moved and my scan no longer sees it", so a page that yields nothing is a
    # failure, not a pass.
    if (( found == 0 )); then
        printf 'check-release-pins: %s: no release-version literal found. This page is in the\n' "$rel" >&2
        printf '                    pin-bearing set because it pins a version — if it genuinely\n' >&2
        printf '                    no longer does, drop it from DEFAULT_PIN_FILES.\n' >&2
        fail=1
        continue
    fi
    total=$((total + found))
    printf 'check-release-pins: %s: %d pin(s)\n' "$rel" "$found"
done

if (( fail )); then
    printf '\ncheck-release-pins: the docs advertise a release the project has moved past.\n' >&2
    printf "Bump every site above to %s (charts drop the leading \`v\`), per\n" "$release_tag" >&2
    printf 'docs/operations/release.md#7-bump-the-pinned-release-in-the-docs.\n' >&2
    exit 1
fi

printf 'check-release-pins: ok (%d pin(s) across %d page(s), all naming %s)\n' \
    "$total" "${#pin_files[@]}" "$release_tag"
