#!/usr/bin/env bash
#
# Assertions for scripts/docs/check-release-pins.sh — the gate that keeps the
# install/upgrade pages naming the current release (Q638).
#
# The gate's whole value is that it fires when a page goes stale, so the cases
# below are mostly planted failures: a checker that silently matches nothing
# passes a stale tree exactly like a clean one. Each fixture is written to a
# throwaway tree so no assumption about the caller's docs leaks in.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
readonly GATE="$REPO_ROOT/scripts/docs/check-release-pins.sh"

WORKDIR="$(mktemp -d)"
readonly WORKDIR
trap 'rm -rf "$WORKDIR"' EXIT

fails=0
n=0

# fixture CONTENT... -> path to a doc holding those lines
fixture() {
    local path
    n=$((n + 1))
    path="$WORKDIR/doc-$n.md"
    printf '%s\n' "$@" >"$path"
    printf '%s\n' "$path"
}

# run FIXTURE -> exit status in $status, combined output in $output
status=0
output=''
run() {
    status=0
    output="$(GAG_RELEASE_TAG=v1.3.0 "$GATE" "$1" 2>&1)" || status=$?
}

pass_case() {
    local name="$1" doc="$2"
    run "$doc"
    if (( status == 0 )); then
        printf 'ok   %s\n' "$name"
    else
        printf 'FAIL %s: expected exit 0, got %d:\n%s\n' "$name" "$status" "$output" >&2
        fails=$((fails + 1))
    fi
}

# fail_case NAME DOC NEEDLE — must exit 1 and say NEEDLE, so a case cannot pass
# on an unrelated failure.
fail_case() {
    local name="$1" doc="$2" needle="$3"
    run "$doc"
    if (( status == 1 )) && [[ "$output" == *"$needle"* ]]; then
        printf 'ok   %s\n' "$name"
    else
        printf 'FAIL %s: expected exit 1 mentioning %q, got %d:\n%s\n' \
            "$name" "$needle" "$status" "$output" >&2
        fails=$((fails + 1))
    fi
}

pass_case 'a page pinning the current release' \
    "$(fixture "> **Current release — \`v1.3.0\`.** Pin \`--version 1.3.0\`.")"

fail_case 'a stale chart pin' \
    "$(fixture "Pin \`--version 1.2.0\`.")" \
    'pins 1.2.0, but the current release is v1.3.0'

fail_case 'a stale release-notes URL' \
    "$(fixture "[notes](https://github.com/o/r/releases/tag/v1.2.0) and \`--version 1.3.0\`")" \
    'pins v1.2.0'

fail_case 'a stale Argo targetRevision' \
    "$(fixture '    targetRevision: 1.2.0' '    tag: "1.3.0"')" \
    ':1: pins 1.2.0'

# The one shape that must survive a release bump: a line recording what was
# actually installed during a measurement. Bumping it would falsify the record.
pass_case 'a measurement line keeps its old version' \
    "$(fixture "Pin \`--version 1.3.0\`." \
               "Measured on kind v1.36.1: install \`v1.2.0\`, upgrade to this chart.")"

# ...and the exemption is anchored to that line shape, not to the version, so
# the same version elsewhere still fails.
fail_case 'the measurement exemption does not leak to other lines' \
    "$(fixture "Measured on kind v1.36.1: install \`v1.2.0\`." "Pin \`--version 1.2.0\`.")" \
    ':2: pins 1.2.0'

# v2.0.0 is the announced v1alpha1/v2alpha1 removal release — a future version
# these pages name on purpose.
pass_case 'the announced removal release is exempt' \
    "$(fixture "Removed in \`v2.0.0\`. Pin \`--version 1.3.0\`.")"

# ...but the exemption is v-prefixed only, because a chart pin never carries the
# `v`. An unprefixed 2.0.0 is a pin, and a wrong one.
fail_case 'a bare 2.0.0 is still a pin' \
    "$(fixture "Pin \`--version 2.0.0\`.")" \
    'pins 2.0.0'

fail_case 'a stale patch-line hint' \
    "$(fixture "Pin \`--version 1.3.0\`; newer patch releases publish as \`1.2.z\`.")" \
    "the current line is \`1.3.z\`"

pass_case 'a current patch-line hint' \
    "$(fixture "Pin \`--version 1.3.0\`; newer patch releases publish as \`1.3.z\`.")"

# The reconciliation guard: a page in the pin-bearing set that yields no pin at
# all means the scan stopped seeing it, which reads identically to "clean".
fail_case 'a page with no pin at all is a failure, not a pass' \
    "$(fixture 'This page pins nothing.')" \
    'no release-version literal found'

# Dotted runs longer than three parts are not release versions. Without this the
# gate would report every CIDR and four-part version as a stale pin.
pass_case 'dotted-quad addresses and four-part versions are not pins' \
    "$(fixture 'Reserve 169.254.169.254 and 10.96.0.0/12; runner 2.335.1.4 is fine.' \
               "Pin \`--version 1.3.0\`.")"

# Tag selection, in throwaway repos carrying exactly the tags a case needs: the
# gate is only as right as the version it compares against.
# setup_repo PIN TAG... -> a repo carrying TAG... and one doc pinning PIN
setup_repo() {
    local pin="$1" d="$WORKDIR/repo" tag
    shift
    rm -rf "$d"
    mkdir -p "$d"
    (
        cd "$d"
        git init -q -b main
        git config user.email t@t.t
        git config user.name t
        git commit -q --allow-empty -m base
        for tag in "$@"; do
            git tag "$tag"
        done
    )
    printf "Pin \`--version %s\`.\n" "${pin#v}" >"$d/doc.md"
    printf '%s\n' "$d"
}

# resolves NAME WANT TAG... — a doc pinning WANT must pass in a repo carrying
# TAG..., which holds only if the gate picked WANT as the current release.
resolves() {
    local name="$1" want="$2" d st=0 out
    shift 2
    d="$(setup_repo "$want" "$@")"
    out="$( (cd "$d" && GAG_RELEASE_TAG='' "$GATE" "$d/doc.md") 2>&1 )" || st=$?
    if (( st == 0 )) && [[ "$out" == *"current release $want "* ]]; then
        printf 'ok   %s\n' "$name"
    else
        printf 'FAIL %s: expected %q to resolve, got %d:\n%s\n' "$name" "$want" "$st" "$out" >&2
        fails=$((fails + 1))
    fi
}

resolves 'newest of several stable tags' v1.3.0 v1.1.0 v1.3.0 v1.2.0
# Lexical sort would pick v1.9.0 here.
resolves 'v1.10.0 outranks v1.9.0' v1.10.0 v1.9.0 v1.10.0
resolves 'a prerelease never becomes the current release' v1.2.0 v1.2.0 v1.3.0-rc.1
resolves '0.x is not a release operators install' v1.0.0 v0.9.0 v1.0.0

# No stable tag anywhere is a fresh fork, not a stale doc: skip, do not fail.
d="$(setup_repo v1.3.0 v0.1.0)"
st=0
out="$( (cd "$d" && GAG_RELEASE_TAG='' "$GATE" "$d/doc.md") 2>&1 )" || st=$?
if (( st == 0 )) && [[ "$out" == *SKIP* ]]; then
    printf 'ok   %s\n' 'no stable tag -> skip, not fail'
else
    printf 'FAIL %s: expected a skip, got %d:\n%s\n' 'no stable tag -> skip' "$st" "$out" >&2
    fails=$((fails + 1))
fi

if (( fails > 0 )); then
    printf '\n%d check-release-pins assertion(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\ncheck-release-pins-test: all assertions passed\n'
