#!/usr/bin/env bash
#
# check-v2-api-sync.sh — assert every file adjacent v2 API packages share stays
# byte-identical, modulo the differences a Kubernetes API version is entitled to
# (Q345, widened in Q374, a second pair in Q413).
#
# api/v2alpha1, api/v2beta1 and api/v2 are served versions of one API, checked as
# two pairs: v2alpha1 against v2beta1, and v2beta1 against v2. Kubernetes requires
# the *versioned types* to be duplicated per version, but most of what sits beside
# them — the shared spec fragments, the scheduling knobs, the condition re-exports —
# is identical by contract, and a one-sided edit breaks the storage/hub conversion
# silently. Q345's original gate hardcoded a single pair of paths
# (conditions.go) and so guarded 332 of the ~2,550 identical lines; the rest drifted
# unwatched. This gate inverts the default: EVERY .go file present in both packages
# must match unless it is named in its pair's exemption list below with a reason. A
# new file added to both versions is covered the day it lands, with no edit here.
#
# What counts as a legitimate difference, and is normalised away before the diff:
#   - the `package v2alphaN` / `package v2betaN` / `package v2` clause
#   - a `// +kubebuilder:storageversion` marker (by definition only one version
#     carries it)
#   - a `// +kubebuilder:deprecatedversion` marker, with or without its
#     `:warning="..."` text (Q411: v2alpha1 is deprecated, v2beta1 is not, and the
#     warning text names the deprecated version and Kind, so it cannot be mirrored)
# Everything else must match byte for byte.
#
# Files present in only one version (a version-specific test, say) are reported but
# never fail: adding a test to one package is normal and this gate must not tax it.
#
# Usage:
#   scripts/go/check-v2-api-sync.sh                                  # the real check
#   scripts/go/check-v2-api-sync.sh OLDER_DIR NEWER_DIR [EXEMPT...]  # one pair (tests)
#
# Passing directories checks that one pair and replaces the exemption list too — it
# defaults to empty, so a caller can only make the check stricter, never weaker.

set -euo pipefail
shopt -s inherit_errexit

# Files that legitimately differ within a pair, each with the reason it cannot be
# held identical. Keep these lists SHORT and justified: every entry is a stretch of
# API surface nothing checks. A stale entry (a file no longer present in both
# packages of its pair) fails the gate, so the lists cannot rot silently.
# shellcheck disable=SC2034  # read through check_pair's nameref
declare -A EXEMPT_ALPHA_BETA=(
    [runnerset_types.go]="genuinely versioned: v2beta1 is ScaleSet-only and drops acquisitionProtocol/maxListeners (Q264 §5a-U7)"
    [conversion.go]="genuinely versioned: v2alpha1 is the spoke, v2beta1 the hub, so the conversion bodies are inverses"
    [groupversion_info.go]="genuinely versioned: per-version GroupVersion, SchemeBuilder, and served/storage markers"
    [types_test.go]="genuinely versioned: pins each version's own surface (v2beta1's dropped fields, v2alpha1's protocol enum)"
    [zz_generated.deepcopy.go]="controller-gen output derived from the versioned *_types.go; its cross-version identity is a consequence of today's field shapes, not a contract, and \`make codegen-check\` regenerates and diffs both copies (Q477)"
)
# shellcheck disable=SC2034  # read through check_pair's nameref
declare -A EXEMPT_BETA_GA=(
    [egressproxy_types.go]="genuinely versioned: v2 omits the deprecated CiliumFQDN/CalicoFQDN aliases (Q452); api/v2's TestEgressProxyShapeMatchesHub pins the rest of the shape to v2beta1's"
    [conversion.go]="genuinely versioned: v2beta1 is the hub, v2 a spoke"
    [groupversion_info.go]="genuinely versioned: per-version GroupVersion, SchemeBuilder, and package doc"
    [types_test.go]="genuinely versioned: v2beta1 asserts its hub markers, v2 that it is a spoke"
    [zz_generated.deepcopy.go]="controller-gen output derived from the versioned *_types.go; \`make codegen-check\` regenerates and diffs every copy (Q477)"
)
declare -A EXEMPT_FIXTURE=()

# PAIRS holds "OLDER_DIR NEWER_DIR EXEMPT_ARRAY_NAME" triples, checked in order.
PAIRS=(
    'api/v2alpha1 api/v2beta1 EXEMPT_ALPHA_BETA'
    'api/v2beta1 api/v2 EXEMPT_BETA_GA'
)

if (($# > 0)); then
    if (($# < 2)); then
        printf 'usage: %s [OLDER_DIR NEWER_DIR [EXEMPT_BASENAME...]]\n' "$0" >&2
        exit 2
    fi
    PAIRS=("$1 $2 EXEMPT_FIXTURE")
    shift 2
    for basename in "$@"; do
        # shellcheck disable=SC2034  # read through check_pair's nameref
        EXEMPT_FIXTURE["$basename"]='exempted by the caller'
    done
else
    cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
fi

# Normalised copies land here rather than in a process substitution: awk's exit
# status is unobservable through <(...), so a read that fails leaves that side
# empty and the diff reports every line as deleted — a false divergence naming an
# edit nobody made (Q596).
NORM_TMP="$(mktemp -d)"
trap 'rm -rf "$NORM_TMP"' EXIT INT TERM

# Normalise the entitled differences so the diff sees only real divergence. awk
# (not sed) per docs/development/bash-style.md.
normalize() {
    awk '
        /^package v2[a-z0-9]*$/            { print "package v2SYNC"; next }
        /^[[:space:]]*\/\/ \+kubebuilder:storageversion[[:space:]]*$/ { next }
        /^[[:space:]]*\/\/ \+kubebuilder:deprecatedversion(:warning=.*)?[[:space:]]*$/ { next }
                                           { print }
    ' "$1"
}

# go_files DIR — the basenames of the .go files in DIR, sorted. A directory with no
# .go files yields nothing rather than a literal glob.
go_files() {
    local dir="$1" path
    for path in "$dir"/*.go; do
        [[ -e "$path" ]] || continue
        basename "$path"
    done | sort
}

# check_pair OLDER_DIR NEWER_DIR EXEMPT_ARRAY_NAME — diff every file the two
# packages share, report what was checked, and return 1 on any divergence or stale
# exemption.
check_pair() {
    local older_dir="$1" newer_dir="$2"
    local -n exempt="$3"
    local dir file diff_out failed=0 checked=0 checked_lines=0
    local -a older_files=() newer_files=() unpaired=() skipped=() diverged=()
    local -A in_newer=()

    for dir in "$older_dir" "$newer_dir"; do
        if [[ ! -d "$dir" ]]; then
            printf 'check-v2-api-sync: %s is not a directory\n' "$dir" >&2
            exit 2
        fi
    done

    mapfile -t older_files < <(go_files "$older_dir")
    mapfile -t newer_files < <(go_files "$newer_dir")
    for file in "${newer_files[@]}"; do in_newer["$file"]=1; done

    for file in "${older_files[@]}"; do
        if [[ -z "${in_newer[$file]:-}" ]]; then
            unpaired+=("$older_dir/$file")
            continue
        fi
        unset "in_newer[$file]"
        if [[ -n "${exempt[$file]:-}" ]]; then
            skipped+=("$file — ${exempt[$file]}")
            continue
        fi

        checked=$((checked + 1))
        checked_lines=$((checked_lines + $(wc -l <"$older_dir/$file")))
        if ! normalize "$older_dir/$file" >"$NORM_TMP/older" ||
            ! normalize "$newer_dir/$file" >"$NORM_TMP/newer"; then
            failed=1
            printf 'check-v2-api-sync: could not read %s in both versions — trouble, not drift\n' "$file" >&2
            continue
        fi
        if diff_out="$(diff -u --label "$older_dir/$file" --label "$newer_dir/$file" \
            "$NORM_TMP/older" "$NORM_TMP/newer")"; then
            continue
        fi
        failed=1
        diverged+=("$file")
        if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
            printf '::error file=%s/%s::diverges from %s/%s; the v2 API packages must hold this file identically\n' \
                "$newer_dir" "$file" "$older_dir" "$file"
        fi
        printf '\ncheck-v2-api-sync: %s diverges between %s and %s.\n' "$file" "$older_dir" "$newer_dir" >&2
        printf '%s\n' "$diff_out" >&2
    done

    # Whatever is left in in_newer is present in the newer package only.
    for file in "${!in_newer[@]}"; do
        unpaired+=("$newer_dir/$file")
    done

    # A stale exemption is a silent coverage hole: the file it names is gone or no
    # longer paired, so the entry buys nothing and hides the next file that takes its
    # place.
    for file in "${!exempt[@]}"; do
        if [[ ! -f "$older_dir/$file" || ! -f "$newer_dir/$file" ]]; then
            failed=1
            printf 'check-v2-api-sync: stale exemption %q — not present in both %s and %s; drop it from %s\n' \
                "$file" "$older_dir" "$newer_dir" "$3" >&2
        fi
    done

    if ((${#skipped[@]} > 0)); then
        printf 'check-v2-api-sync: %s vs %s exempt (versioned by design):\n' "$older_dir" "$newer_dir"
        printf '  - %s\n' "${skipped[@]}"
    fi
    if ((${#unpaired[@]} > 0)); then
        mapfile -t unpaired < <(printf '%s\n' "${unpaired[@]}" | sort)
        printf 'check-v2-api-sync: present in one version only (not checked):\n'
        printf '  - %s\n' "${unpaired[@]}"
    fi

    if ((failed == 0)); then
        printf 'check-v2-api-sync: %d shared file(s), %d lines, in sync across %s and %s\n' \
            "$checked" "$checked_lines" "$older_dir" "$newer_dir"
        return 0
    fi

    if ((${#diverged[@]} > 0)); then
        printf '\nThe v2 API packages must hold these files identically: %s\n' "${diverged[*]}" >&2
        printf 'A one-sided edit breaks the storage/hub conversion contract silently. Mirror the\n' >&2
        printf 'edit into the other version so the files differ only in their package clause:\n' >&2
        printf '  awk '\''NR==1{print "package %s"; next}{print}'\'' %s/FILE > %s/FILE\n' \
            "$(basename "$newer_dir")" "$older_dir" "$newer_dir" >&2
        printf 'If the divergence is deliberate and permanent, add the file to %s in\n' "$3" >&2
        printf '%s with the reason — an unexplained gap is how the last one grew.\n' "$0" >&2
    fi
    return 1
}

status=0
for pair in "${PAIRS[@]}"; do
    read -r older newer exempt_name <<<"$pair"
    check_pair "$older" "$newer" "$exempt_name" || status=1
done
exit "$status"
