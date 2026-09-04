#!/usr/bin/env bash
#
# Unit tests for scripts/go/check-v2-api-sync.sh (Q374): a body divergence in any shared
# file fails, the entitled differences (the package clause, a
# +kubebuilder:storageversion marker, a +kubebuilder:deprecatedversion marker) do not,
# an exempt file may diverge freely, a stale exemption fails, and a file present in one
# version only is reported without failing. Also runs the real check against the
# tracked tree.
#
# The gate is only worth having if it fails when it should, so these assertions are
# the permanent form of the invert-the-fix verification
# (docs/development/testing.md § Diagnosing failures). Runs under `make check` (via
# `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"
CHECKER="$REPO_ROOT/scripts/go/check-v2-api-sync.sh"

FIXTURE_ROOT="$REPO_ROOT/tmp/v2-api-sync-test.$$"
trap 'rm -rf "$FIXTURE_ROOT"' EXIT INT TERM

fails=0

# fixture NAME — create a fresh alpha/beta package pair holding one identical file,
# and echo the pair's root. Callers then mutate one side to build the case.
fixture() {
    local name="$1"
    local root="$FIXTURE_ROOT/$name"
    mkdir -p "$root/v2alpha1" "$root/v2beta1"
    printf 'package v2alpha1\n\nconst Shared = "x"\n' >"$root/v2alpha1/shared_types.go"
    printf 'package v2beta1\n\nconst Shared = "x"\n' >"$root/v2beta1/shared_types.go"
    printf '%s' "$root"
}

# expect NAME EXPECT_RC ROOT [EXEMPT...] — run the checker over the fixture pair and
# assert the exit code. Output is captured so a later assertion can grep it.
expect() {
    local name="$1" want_rc="$2" root="$3" got_rc=0
    shift 3
    LAST_OUT="$("$CHECKER" "$root/v2alpha1" "$root/v2beta1" "$@" 2>&1)" || got_rc=$?
    die_if_killed "$name" "$got_rc" "$want_rc"
    if [[ "$got_rc" == "$want_rc" ]]; then
        printf 'ok   %-34s rc=%s\n' "$name" "$got_rc"
    else
        printf 'FAIL %-34s want rc=%s got rc=%s\n%s\n' "$name" "$want_rc" "$got_rc" "$LAST_OUT" >&2
        fails=$((fails + 1))
    fi
}

# expect_output NAME PATTERN — assert the last run's output mentions PATTERN.
expect_output() {
    local name="$1" pattern="$2"
    if grep -q -- "$pattern" <<<"$LAST_OUT"; then
        printf 'ok   %-34s reported %q\n' "$name" "$pattern"
    else
        printf 'FAIL %-34s output did not mention %q\n%s\n' "$name" "$pattern" "$LAST_OUT" >&2
        fails=$((fails + 1))
    fi
}

# An identical pair differing only in the package clause is in sync.
root="$(fixture identical)"
expect identical-pair 0 "$root"

# A body divergence in a shared file fails and names the file. This is the case the
# old two-path gate missed for every file but conditions.go.
root="$(fixture body-drift)"
printf 'const OnlyInBeta = "y"\n' >>"$root/v2beta1/shared_types.go"
expect body-drift 1 "$root"
expect_output body-drift-names-file 'shared_types.go'

# A one-sided +kubebuilder:storageversion marker is the storage version's badge, not
# drift: exactly one version carries it, always.
root="$(fixture storageversion-marker)"
printf '\n// +kubebuilder:storageversion\ntype Thing struct{}\n' >>"$root/v2beta1/shared_types.go"
printf '\ntype Thing struct{}\n' >>"$root/v2alpha1/shared_types.go"
expect storageversion-marker-ignored 0 "$root"

# A one-sided +kubebuilder:deprecatedversion marker is the deprecated version's badge
# (Q411), with or without warning text. The text names the version and Kind, so it can
# never be mirrored into the other package.
root="$(fixture deprecatedversion-marker)"
printf '\n// +kubebuilder:deprecatedversion:warning="v2alpha1 Thing is deprecated; use v2beta1."\ntype Thing struct{}\n' >>"$root/v2alpha1/shared_types.go"
printf '\ntype Thing struct{}\n' >>"$root/v2beta1/shared_types.go"
expect deprecatedversion-marker-ignored 0 "$root"

# The bare form (no warning text) is normalised too.
root="$(fixture deprecatedversion-bare)"
printf '\n// +kubebuilder:deprecatedversion\ntype Thing struct{}\n' >>"$root/v2alpha1/shared_types.go"
printf '\ntype Thing struct{}\n' >>"$root/v2beta1/shared_types.go"
expect deprecatedversion-bare-ignored 0 "$root"

# Normalising the marker must not blind the gate to the line it sits above: a real
# divergence in the deprecated type's body still fails.
root="$(fixture deprecatedversion-hides-nothing)"
printf '\n// +kubebuilder:deprecatedversion:warning="v2alpha1 Thing is deprecated."\ntype Thing struct{ A string }\n' >>"$root/v2alpha1/shared_types.go"
printf '\ntype Thing struct{ B string }\n' >>"$root/v2beta1/shared_types.go"
expect deprecatedversion-hides-nothing 1 "$root"

# An exempt file may diverge freely, and the run says which files it skipped.
root="$(fixture exempt-drift)"
printf 'package v2alpha1\n\nconst A = "alpha"\n' >"$root/v2alpha1/runnerset_types.go"
printf 'package v2beta1\n\nconst A = "beta"\n' >"$root/v2beta1/runnerset_types.go"
expect exempt-file-may-diverge 0 "$root" runnerset_types.go
expect_output exempt-file-reported 'runnerset_types.go'

# Without the exemption the same divergence fails — the exemption is what allows it,
# not a quirk of the file name.
expect unexempt-file-must-match 1 "$root"

# A stale exemption (naming a file no longer paired) is a silent coverage hole.
root="$(fixture stale-exemption)"
expect stale-exemption 1 "$root" not_a_file.go
expect_output stale-exemption-explained 'stale exemption'

# A file present in one version only is reported, never failed: adding a test to one
# package is normal and must not tax the contributor.
root="$(fixture unpaired)"
printf 'package v2alpha1\n\n// alpha-only helper test\n' >"$root/v2alpha1/extra_test.go"
expect unpaired-file-passes 0 "$root"
expect_output unpaired-file-reported 'extra_test.go'

# A missing directory is a usage error (rc 2), distinct from a divergence (rc 1).
root="$(fixture missing-dir)"
rm -rf "$root/v2beta1"
expect missing-directory 2 "$root"

# A shared file the checker cannot read is trouble, not drift. Left unchecked,
# awk's failure empties that side and the diff blames every line on an edit
# nobody made — the shape a transient read failure takes (Q596).
#
# Gated on whether the mode bits actually bite rather than on $EUID: a job on a
# self-hosted runner may read a 000 file, and asserting anyway would make this
# suite the next flake.
root="$(fixture unreadable)"
chmod 000 "$root/v2beta1/shared_types.go"
if cat "$root/v2beta1/shared_types.go" >/dev/null 2>&1; then
    printf 'skip %-34s uid %s reads a mode-000 file\n' unreadable-file "$(id -u)"
else
    expect unreadable-file 1 "$root"
    expect_output unreadable-file-explained 'trouble, not drift'
fi
chmod 644 "$root/v2beta1/shared_types.go"

# The tracked tree is in sync. This is the one assertion reading the live tree,
# so it is also the one whose failure can be something other than a divergence.
# Keep the checker's own output and its exit code: discarding them is what left
# Q596's single occurrence undiagnosable.
tree_rc=0
tree_out="$("$CHECKER" 2>&1)" || tree_rc=$?
die_if_killed tree-in-sync "$tree_rc"
if ((tree_rc == 0)); then
    printf 'ok   %-34s tracked api/v2alpha1 vs api/v2beta1\n' tree-in-sync
else
    if ((tree_rc == 1)); then
        printf 'FAIL %-34s tracked v2 API packages diverge; run %s\n' tree-in-sync "$CHECKER" >&2
    else
        printf 'FAIL %-34s %s exited %d — not a divergence\n' tree-in-sync "$CHECKER" "$tree_rc" >&2
    fi
    printf '%s\n' "$tree_out" >&2
    fails=$((fails + 1))
fi

if ((fails > 0)); then
    printf '\ncheck-v2-api-sync-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\ncheck-v2-api-sync-test: all assertions passed\n'
