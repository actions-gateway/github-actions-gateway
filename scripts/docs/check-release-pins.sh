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

# Lines whose versions record what was measured, not what to install. Bumping
# one falsifies the record, so they are matched by shape rather than by value.
EXEMPT_LINE_RE='^Measured on kind '

# Versions in the pin-bearing set that are deliberately not the current release.
# Both are v-prefixed; chart pins are written without the `v`, so a bare `2.0.0`
# still fails.
EXEMPT_VERSIONS_RE='^v2\.0\.0$'   # the announced v1alpha1/v2alpha1 removal release

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

# The highest stable vX.Y.Z tag. Prerelease tags (-rc.N, -alpha, -beta) and 0.x
# are excluded: neither is a release an operator installs.
stable_tags() {
    awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ && !/^v0\./' | sort -V | tail -1
}

release_tag="${GAG_RELEASE_TAG:-}"
tag_source="\$GAG_RELEASE_TAG"
if [[ -z "$release_tag" ]]; then
    release_tag="$(git -C "$repo_root" tag --list 'v*' | stable_tags)"
    tag_source="local tags"
fi
if [[ -z "$release_tag" ]]; then
    # A shallow CI checkout carries no tags. Read them off the remote rather than
    # concluding there is no release — an unfetched tag list and a tagless repo
    # look identical locally.
    # No `origin`, or no network, is "cannot tell" rather than an error here —
    # the skip below reports it.
    release_tag="$({ git -C "$repo_root" ls-remote --tags --refs origin 'v*' 2>/dev/null || true; } |
        awk -F/ '{ print $NF }' | stable_tags)"
    tag_source="origin remote"
fi

if [[ -z "$release_tag" ]]; then
    printf 'check-release-pins: SKIP — no stable vX.Y.Z tag locally or on origin, so there is\n'
    printf '                    no released version to pin (expected on a fresh fork). Nothing\n'
    printf '                    was checked. Set GAG_RELEASE_TAG to check against a version.\n'
    exit 0
fi

release_version="${release_tag#v}"
release_minor="${release_version%.*}"

printf 'check-release-pins: current release %s (chart version %s), from %s\n' \
    "$release_tag" "$release_version" "$tag_source"

# Emit one `<line>\t<literal>\t<kind>` record per release-version literal.
#
# `kind` is `semver` for a full X.Y.Z (optionally v-prefixed) or `patchline` for
# the `X.Y.z` shorthand the install callout uses for "newer patch releases".
# A match flanked by a digit — or by a dot followed by a digit — is part of a
# longer dotted run (a four-part version, a dotted-quad address) and is not a
# release version.
literals() {
    awk -v exempt_line="$EXEMPT_LINE_RE" '
        function flanked(before, after, after2) {
            if (before ~ /[0-9]/) return 1
            if (before == "." ) return 1
            if (after ~ /[0-9]/) return 1
            if (after == "." && after2 ~ /[0-9]/) return 1
            return 0
        }
        $0 ~ exempt_line { next }
        {
            rest = $0
            offset = 0
            while (match(rest, /v?[0-9]+\.[0-9]+\.([0-9]+|z)/)) {
                tok = substr(rest, RSTART, RLENGTH)
                before = (RSTART + offset > 1) ? substr($0, RSTART + offset - 1, 1) : ""
                end = RSTART + RLENGTH
                after  = substr(rest, end, 1)
                after2 = substr(rest, end + 1, 1)
                offset += end - 1
                rest = substr(rest, end)
                if (flanked(before, after, after2)) continue
                printf "%d\t%s\t%s\n", NR, tok, (tok ~ /z$/ ? "patchline" : "semver")
            }
        }
    ' "$1"
}

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
            printf "check-release-pins: %s:%s: patch-line hint \`%s\` names an old release; the current line is \`%s\`\n" \
                "$rel" "$line_no" "$tok" "$want" >&2
        else
            [[ "${tok#v}" == "$release_version" ]] && continue
            printf 'check-release-pins: %s:%s: pins %s, but the current release is %s\n' \
                "$rel" "$line_no" "$tok" "$release_tag" >&2
        fi
        fail=1
    done < <(literals "$f")

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
