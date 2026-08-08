#!/usr/bin/env bash
#
# check-no-license-headers.sh — forbid per-file Apache license headers in
# first-party Go source (Q331).
#
# The repository's root LICENSE (Apache-2.0) is the canonical, sufficient license
# grant, so the per-file boilerplate header that Kubebuilder/controller-gen
# scaffolds onto Go files is redundant. Coverage of that scaffolded header was
# also inconsistent across the tree (cmd/gmc carried it, cmd/agc largely did
# not). Q331 stripped it from every first-party file and empties the codegen
# `hack/boilerplate.go.txt` header sources so regeneration adds none; this gate
# keeps it from creeping back in.
#
# It fails if any tracked, non-vendored .go file contains the Apache boilerplate
# marker line. Third-party code under vendor/ and tools/vendor/ keeps its
# headers (those notices are legally required) and is excluded.
#
# This is unrelated to the `license-notices` / THIRD-PARTY-NOTICES tooling, which
# aggregates *dependency* license notices for image distribution.
#
# Usage:
#   scripts/go/check-no-license-headers.sh

set -euo pipefail
shopt -s inherit_errexit

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

# The boilerplate's stable, distinctive line. Matching this (rather than the full
# block) keeps the check cheap and robust to minor whitespace differences.
marker='Licensed under the Apache License'

# Tracked first-party Go files, excluding the vendored trees.
mapfile -t files < <(
    git ls-files -- '*.go' \
        | grep -Ev '(^|/)(tools/)?vendor/' \
        | sort -u
)

if (( ${#files[@]} == 0 )); then
    printf 'check-no-license-headers: no first-party .go files found\n' >&2
    exit 2
fi

offenders=()
for file in "${files[@]}"; do
    if grep -qF "$marker" "$file"; then
        offenders+=("$file")
    fi
done

if (( ${#offenders[@]} != 0 )); then
    for file in "${offenders[@]}"; do
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s::first-party Go file carries a per-file Apache license header; remove it (root LICENSE is canonical)\n' "$file"
        else
            printf 'check-no-license-headers: %s: per-file Apache license header found\n' "$file" >&2
        fi
    done
    printf '\ncheck-no-license-headers: %d file(s) carry a redundant per-file license\n' "${#offenders[@]}" >&2
    printf 'header. The root LICENSE (Apache-2.0) is canonical; first-party files must\n' >&2
    printf 'not repeat it. Delete the leading `/* ... Licensed under the Apache License\n' >&2
    printf '... */` block. If a file is regenerated, ensure its module\n' >&2
    printf 'hack/boilerplate.go.txt is empty so controller-gen emits no header.\n' >&2
    exit 1
fi

printf 'check-no-license-headers: all %d first-party .go files are header-free\n' "${#files[@]}"
