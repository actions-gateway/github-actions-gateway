#!/usr/bin/env bash
#
# gen-third-party-notices.sh - assemble THIRD-PARTY-NOTICES from the vendor/ tree.
#
# Description:
#   The compiled Go binaries statically link the third-party modules committed
#   under the repo-root vendor/ tree. Their licenses (MIT/BSD/Apache/ISC/...)
#   require reproducing the copyright/notice text in any distribution — and a
#   container image IS a distribution (Apache-2.0 §4(d), the MIT/BSD
#   reproduce-the-notice clauses). This script concatenates every vendored
#   module's license/notice files into a single THIRD-PARTY-NOTICES file at the
#   repo root, each block headed by the module import path and version.
#
#   The file is generate-and-committed so reviewers see the content in the diff
#   and CI catches staleness; `--check` regenerates into a temp file and fails
#   if it differs from the committed copy (the drift gate). This mirrors the
#   repo's tidy/generate-and-verify pattern.
#
#   Source of truth is the committed, version-pinned vendor/ tree ONLY — the
#   script never hits the network or the module cache, so it is fully offline
#   and reproducible.
#
# Usage:
#   ./gen-third-party-notices.sh           # (re)generate THIRD-PARTY-NOTICES
#   ./gen-third-party-notices.sh --check   # fail if THIRD-PARTY-NOTICES is stale

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

VENDOR_DIR="vendor"
MODULES_TXT="$VENDOR_DIR/modules.txt"
OUT="THIRD-PARTY-NOTICES"

CHECK=false
if [[ "${1:-}" == "--check" ]]; then
    CHECK=true
elif [[ -n "${1:-}" ]]; then
    echo "usage: $0 [--check]" >&2
    exit 2
fi

if [[ ! -f "$MODULES_TXT" ]]; then
    echo "ERROR: $MODULES_TXT not found — run from a checkout with a vendored workspace." >&2
    exit 1
fi

# 1. Vendored module paths (and versions). A module-header line looks like
#    "# <path> <version>"; workspace-internal modules carry a "=> ./dir" replace
#    directive and have no vendored copy, so skip them. The package lines (no
#    leading "#") and the "## explicit" annotation lines are ignored.
mapfile -t modules < <(awk '/^# / && $0 !~ /=>/ { print $2 }' "$MODULES_TXT" | sort -u)

declare -A mod_version
while IFS=' ' read -r path version; do
    [[ -n "$path" ]] && mod_version["$path"]="$version"
done < <(awk '/^# / && $0 !~ /=>/ { print $2, $3 }' "$MODULES_TXT")

# 2. Every license/notice file under vendor/, sorted for determinism.
mapfile -t license_files < <(find "$VENDOR_DIR" -type f \
    \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
    | sort)

# 3. Attribute each file to the longest module path that is a directory prefix of
#    it. Longest-prefix matters for nested modules (e.g. .../json-patch vs
#    .../json-patch/v5), so a sub-module's license is filed under the sub-module.
declare -A mod_files
for f in "${license_files[@]}"; do
    rel="${f#"$VENDOR_DIR"/}"   # e.g. github.com/foo/bar/LICENSE
    dir="${rel%/*}"             # e.g. github.com/foo/bar
    best=""
    for m in "${modules[@]}"; do
        if [[ "$dir" == "$m" || "$dir" == "$m"/* ]]; then
            (( ${#m} > ${#best} )) && best="$m"
        fi
    done
    # Fall back to the file's own directory if it sits outside every known module
    # (should not happen for a tidy vendor tree).
    [[ -z "$best" ]] && best="$dir"
    mod_files["$best"]+="$rel"$'\n'
done

# 4. Emit the consolidated file. Walk modules in sorted order; within a module,
#    walk its license files in sorted order. Anything without a license file is
#    silently skipped (go mod vendor only copies a LICENSE when the upstream
#    module ships one at or above the imported package directory).
RULE="================================================================================"

generate() {
    cat <<'HEADER'
THIRD-PARTY-NOTICES
===================

This file aggregates the license and notice texts of the third-party Go modules
that are statically linked into the github-actions-gateway binaries (agc, gmc,
proxy, worker). Each block reproduces the upstream module's own license/notice
files verbatim, as required by those licenses when the binaries are
redistributed — including inside the container images, which ship this file at
/licenses/THIRD-PARTY-NOTICES alongside the project LICENSE and NOTICE.

GENERATED FILE — DO NOT EDIT BY HAND.
Regenerate with `make third-party-notices` (scripts/gen-third-party-notices.sh),
which reads only the committed, version-pinned vendor/ tree.

HEADER

    local m version rel
    local -a files
    for m in "${modules[@]}"; do
        [[ -z "${mod_files[$m]:-}" ]] && continue
        version="${mod_version[$m]:-}"
        mapfile -t files < <(printf '%s' "${mod_files[$m]}" | sort -u)
        for rel in "${files[@]}"; do
            [[ -z "$rel" ]] && continue
            echo "$RULE"
            if [[ -n "$version" ]]; then
                echo "Module: $m ($version)"
            else
                echo "Module: $m"
            fi
            echo "License file: $rel"
            echo "$RULE"
            echo
            cat "$VENDOR_DIR/$rel"
            echo
        done
    done
}

if [[ "$CHECK" == true ]]; then
    mkdir -p tmp
    tmp_out="$(mktemp tmp/third-party-notices.XXXXXX)"
    trap 'rm -f "$tmp_out"' EXIT
    generate > "$tmp_out"
    if ! diff -u "$OUT" "$tmp_out" >/dev/null 2>&1; then
        echo "ERROR: $OUT is stale relative to vendor/." >&2
        echo "       Run 'make third-party-notices' and commit the result." >&2
        diff -u "$OUT" "$tmp_out" || true
        exit 1
    fi
    echo "$OUT is up to date."
else
    generate > "$OUT"
    echo "wrote $OUT ($(wc -l < "$OUT" | tr -d ' ') lines)"
fi
