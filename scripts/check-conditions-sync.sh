#!/usr/bin/env bash
#
# check-conditions-sync.sh — assert the two v2 conditions.go files stay in sync
# (Q345).
#
# The v2 status/condition contract is uniform across all five v2 kinds, so the
# canonical condition types and reasons are declared identically in the v2alpha1
# and v2beta1 API packages. The two files
#   api/v2alpha1/conditions.go
#   api/v2beta1/conditions.go
# must be byte-identical except for their `package` declaration line. That parity
# was manual discipline: a condition or reason added to only one file would drift
# silently and break the storage/hub conversion contract. This gate fails fast on
# any divergence, naming the offending lines so a contributor knows to sync them.
#
# The check normalises each file's `package v2...` line to a common token, then
# diffs the two. Only the package line is ignored; every other byte must match.
#
# Usage:
#   scripts/check-conditions-sync.sh

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

alpha='api/v2alpha1/conditions.go'
beta='api/v2beta1/conditions.go'

for file in "$alpha" "$beta"; do
    if [[ ! -f "$file" ]]; then
        printf 'check-conditions-sync: %s not found\n' "$file" >&2
        exit 2
    fi
done

# Replace the sole `package v2alphaN/v2betaN` declaration with a fixed token so
# the legitimate package-name difference is ignored while every other line is
# compared verbatim. awk (not sed) per the repo bash style.
normalize() {
    awk '/^package v2[a-z0-9]+$/ { print "package v2SYNC"; next } { print }' "$1"
}

if diff_out="$(diff -u <(normalize "$alpha") <(normalize "$beta"))"; then
    printf 'check-conditions-sync: %s and %s are in sync\n' "$alpha" "$beta"
    exit 0
fi

# diff exited non-zero: the bodies diverge. Emit the offending hunk.
if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    printf '::error file=%s::diverges from %s (ignoring the package line); sync the two v2 conditions.go files\n' "$beta" "$alpha"
fi
printf 'check-conditions-sync: %s and %s diverge (ignoring the package line).\n' "$alpha" "$beta" >&2
printf 'The v2 condition/reason set must be identical in both v2 API packages;\n' >&2
printf 'a one-sided add breaks the storage/hub conversion contract. Sync them so\n' >&2
printf 'the files differ only in their package declaration. Divergent lines:\n\n' >&2
printf '%s\n' "$diff_out" >&2
exit 1
