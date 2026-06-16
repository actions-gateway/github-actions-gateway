#!/usr/bin/env bash
#
# check-go-version.sh — enforce a single Go version across the workspace.
#
# CLAUDE.md requires that "all go modules in the repo must use the same Go
# version", but the rule was unenforced: the two go.work.gen files (consumed by
# `make manifests` via GOWORK=) silently drifted off the repo `go` directive,
# which breaks code generation. This gate asserts the `go X.Y.Z` directive is
# identical across go.work, every (non-vendored) go.mod, and every go.work.gen,
# failing with a message naming the offending files.
#
# The canonical version is the one declared in go.work; every other file must
# match it. This check only compares declared versions — it never bumps them.
# Bumping the Go version is a deliberate, coupled change (GOTOOLCHAIN=local is
# pinned to the golang base-image digest; see reference docs), so this gate
# deliberately does not normalise — it only reports drift for a human to fix.
#
# Usage:
#   scripts/check-go-version.sh
#
# Operates on the tracked go.work / go.mod / go.work.gen files under the repo
# root, excluding vendored copies (vendor/, tools/vendor/).

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$repo_root"

# Collect the workspace's tracked module/workspace files, excluding the vendored
# trees (which carry upstream dependencies' own, unrelated `go` directives).
mapfile -t files < <(
    git ls-files -- 'go.work' '**/go.mod' 'go.mod' '**/go.work.gen' 'go.work.gen' \
        | grep -Ev '(^|/)vendor/' \
        | sort -u
)

if (( ${#files[@]} == 0 )); then
    printf 'check-go-version: no go.work/go.mod/go.work.gen files found\n' >&2
    exit 2
fi

# Extract the version token from the first `go X[.Y[.Z]]` directive in a file.
go_directive() {
    local file="$1"
    awk '/^go [0-9]/ { print $2; exit }' "$file"
}

canonical="$(go_directive go.work)"
if [[ -z "$canonical" ]]; then
    printf 'check-go-version: go.work has no go directive\n' >&2
    exit 2
fi

exit_code=0
offenders=()

fail() {
    local file="$1" msg="$2"
    if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
        printf '::error file=%s::%s\n' "$file" "$msg"
    else
        printf 'check-go-version: %s: %s\n' "$file" "$msg" >&2
    fi
    exit_code=1
}

for file in "${files[@]}"; do
    version="$(go_directive "$file")"
    if [[ -z "$version" ]]; then
        fail "$file" "no \`go\` directive found"
        offenders+=("$file")
        continue
    fi
    if [[ "$version" != "$canonical" ]]; then
        fail "$file" "go directive is '$version'; expected '$canonical' (canonical, from go.work)"
        offenders+=("$file")
    fi
done

if (( exit_code != 0 )); then
    printf '\ncheck-go-version: Go version drift detected. All go.work, go.mod, and\n' >&2
    printf 'go.work.gen files must declare the same go directive as go.work (%s).\n' "$canonical" >&2
    printf 'Offending files: %s\n' "${offenders[*]}" >&2
    printf 'Align the drifted file(s) to %s — do not bump go.work up to match them\n' "$canonical" >&2
    printf '(a version bump is a coupled change; see CLAUDE.md / reference docs).\n' >&2
    exit 1
fi

printf 'check-go-version: all %d files declare go %s\n' "${#files[@]}" "$canonical"
