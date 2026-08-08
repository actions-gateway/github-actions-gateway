#!/usr/bin/env bash
#
# check-v2-api-sync.sh — assert every file the two v2 API packages share stays
# byte-identical, modulo the differences a Kubernetes API version is entitled to
# (Q345, widened in Q374).
#
# api/v2alpha1 and api/v2beta1 are two served versions of one API. Kubernetes
# requires the *versioned types* to be duplicated per version, but most of what sits
# beside them — the shared spec fragments, the scheduling knobs, the condition
# re-exports — is identical by contract, and a one-sided edit breaks the storage/hub
# conversion silently. Q345's original gate hardcoded a single pair of paths
# (conditions.go) and so guarded 332 of the ~2,550 identical lines; the rest drifted
# unwatched. This gate inverts the default: EVERY .go file present in both packages
# must match unless it is named in EXEMPT below with a reason. A new file added to
# both versions is covered the day it lands, with no edit here.
#
# What counts as a legitimate difference, and is normalised away before the diff:
#   - the `package v2alphaN` / `package v2betaN` clause
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
#   scripts/go/check-v2-api-sync.sh ALPHA_DIR BETA_DIR [EXEMPT...]   # fixtures (tests)
#
# Passing directories replaces the exemption list too — it defaults to empty, so a
# caller can only make the check stricter, never weaker.

set -euo pipefail
shopt -s inherit_errexit

# Files that legitimately differ between the two versions, each with the reason it
# cannot be held identical. Keep this list SHORT and justified: every entry is a
# stretch of API surface nothing checks. A stale entry (a file no longer present in
# both packages) fails the gate, so the list cannot rot silently.
declare -A EXEMPT=(
    [runnerset_types.go]="genuinely versioned: v2beta1 is ScaleSet-only and drops acquisitionProtocol/maxListeners (Q264 §5a-U7)"
    [conversion.go]="genuinely versioned: v2alpha1 is the spoke, v2beta1 the hub, so the conversion bodies are inverses"
    [groupversion_info.go]="genuinely versioned: per-version GroupVersion, SchemeBuilder, and served/storage markers"
    [types_test.go]="genuinely versioned: pins each version's own surface (v2beta1's dropped fields, v2alpha1's protocol enum)"
    [zz_generated.deepcopy.go]="controller-gen output derived from the versioned *_types.go; its cross-version identity is a consequence of today's field shapes, not a contract, and \`make codegen-check\` regenerates and diffs both copies (Q477)"
)

alpha_dir='api/v2alpha1'
beta_dir='api/v2beta1'

if (($# > 0)); then
    if (($# < 2)); then
        printf 'usage: %s [ALPHA_DIR BETA_DIR [EXEMPT_BASENAME...]]\n' "$0" >&2
        exit 2
    fi
    alpha_dir="$1"
    beta_dir="$2"
    shift 2
    EXEMPT=()
    for basename in "$@"; do
        EXEMPT["$basename"]='exempted by the caller'
    done
else
    cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
fi

for dir in "$alpha_dir" "$beta_dir"; do
    if [[ ! -d "$dir" ]]; then
        printf 'check-v2-api-sync: %s is not a directory\n' "$dir" >&2
        exit 2
    fi
done

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
        /^package v2[a-z0-9]+$/            { print "package v2SYNC"; next }
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

mapfile -t alpha_files < <(go_files "$alpha_dir")
mapfile -t beta_files < <(go_files "$beta_dir")

declare -A in_beta=()
for file in "${beta_files[@]}"; do in_beta["$file"]=1; done

failed=0
checked=0
checked_lines=0
declare -a unpaired=() skipped=() diverged=()

for file in "${alpha_files[@]}"; do
    if [[ -z "${in_beta[$file]:-}" ]]; then
        unpaired+=("$alpha_dir/$file")
        continue
    fi
    unset "in_beta[$file]"
    if [[ -n "${EXEMPT[$file]:-}" ]]; then
        skipped+=("$file — ${EXEMPT[$file]}")
        continue
    fi

    checked=$((checked + 1))
    checked_lines=$((checked_lines + $(wc -l <"$alpha_dir/$file")))
    if ! normalize "$alpha_dir/$file" >"$NORM_TMP/alpha" ||
        ! normalize "$beta_dir/$file" >"$NORM_TMP/beta"; then
        failed=1
        printf 'check-v2-api-sync: could not read %s in both versions — trouble, not drift\n' "$file" >&2
        continue
    fi
    if diff_out="$(diff -u --label "$alpha_dir/$file" --label "$beta_dir/$file" \
        "$NORM_TMP/alpha" "$NORM_TMP/beta")"; then
        continue
    fi
    failed=1
    diverged+=("$file")
    if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
        printf '::error file=%s/%s::diverges from %s/%s; the two v2 API packages must hold this file identically\n' \
            "$beta_dir" "$file" "$alpha_dir" "$file"
    fi
    printf '\ncheck-v2-api-sync: %s diverges between %s and %s.\n' "$file" "$alpha_dir" "$beta_dir" >&2
    printf '%s\n' "$diff_out" >&2
done

# Whatever is left in in_beta is beta-only.
for file in "${!in_beta[@]}"; do
    unpaired+=("$beta_dir/$file")
done

# A stale exemption is a silent coverage hole: the file it names is gone or no longer
# paired, so the entry buys nothing and hides the next file that takes its place.
for file in "${!EXEMPT[@]}"; do
    if [[ ! -f "$alpha_dir/$file" || ! -f "$beta_dir/$file" ]]; then
        failed=1
        printf 'check-v2-api-sync: stale exemption %q — not present in both %s and %s; drop it from EXEMPT\n' \
            "$file" "$alpha_dir" "$beta_dir" >&2
    fi
done

if ((${#skipped[@]} > 0)); then
    printf 'check-v2-api-sync: exempt (versioned by design):\n'
    printf '  - %s\n' "${skipped[@]}"
fi
if ((${#unpaired[@]} > 0)); then
    mapfile -t unpaired < <(printf '%s\n' "${unpaired[@]}" | sort)
    printf 'check-v2-api-sync: present in one version only (not checked):\n'
    printf '  - %s\n' "${unpaired[@]}"
fi

if ((failed == 0)); then
    printf 'check-v2-api-sync: %d shared file(s), %d lines, in sync across %s and %s\n' \
        "$checked" "$checked_lines" "$alpha_dir" "$beta_dir"
    exit 0
fi

if ((${#diverged[@]} > 0)); then
    printf '\nThe v2 API packages must hold these files identically: %s\n' "${diverged[*]}" >&2
    printf 'A one-sided edit breaks the storage/hub conversion contract silently. Mirror the\n' >&2
    printf 'edit into the other version so the files differ only in their package clause:\n' >&2
    printf '  awk '\''NR==1{print "package v2beta1"; next}{print}'\'' %s/FILE > %s/FILE\n' \
        "$alpha_dir" "$beta_dir" >&2
    printf 'If the divergence is deliberate and permanent, add the file to EXEMPT in\n' >&2
    printf '%s with the reason — an unexplained gap is how the last one grew.\n' "$0" >&2
fi
exit 1
