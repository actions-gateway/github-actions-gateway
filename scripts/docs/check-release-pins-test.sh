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
        # Q820: no detached maintenance racing the next command in a fixture repo.
        git config maintenance.auto false
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

# A release with a candidate tagged and no stable tag yet may be pinned early, so
# the bump can land before the tag and reach that release's own published page.
# These cases need real tags, and the gate reads them from the tree it is run in,
# so each builds a throwaway repo and runs from inside it.
#
# Both directions matter here more than usual: accepting the prepared version is
# the new behaviour, but a gate that accepted anything would pass a stale tree,
# which is the bug this whole file exists for.
prepared_repo() {
    local dir="$WORKDIR/prep-$1"
    shift
    mkdir -p "$dir"
    git -C "$dir" init -q .
    git -C "$dir" config maintenance.auto false
    git -C "$dir" config user.email t@example.com
    git -C "$dir" config user.name t
    : >"$dir/seed"
    git -C "$dir" add seed
    git -C "$dir" -c commit.gpgsign=false commit -qm seed
    local t
    for t in "$@"; do git -C "$dir" tag "$t"; done
    printf '%s\n' "$dir"
}

# run_in DIR DOC — same as run, but from inside DIR so the gate reads its tags.
run_in() {
    status=0
    output="$(cd "$1" && GAG_RELEASE_TAG=v1.4.0 "$GATE" "$2" 2>&1)" || status=$?
}

prep="$(prepared_repo a v1.4.0 v1.5.0-rc.3)"
# shellcheck disable=SC2016 # literal backticks: the pin shape is markdown, and double quotes would run them
printf '> Pin `--version 1.5.0` here.\n' >"$prep/doc.md"
run_in "$prep" "$prep/doc.md"
if (( status == 0 )); then
    printf 'ok   %s\n' 'a candidate is tagged -> the prepared version may be pinned early'
else
    printf 'FAIL %s: expected exit 0, got %d:\n%s\n' 'prepared version accepted' "$status" "$output" >&2
    fails=$((fails + 1))
fi

# The summary names what the pins NAME, not what they were compared against. The
# two differ for exactly one window -- a candidate tagged, the bump landed, the
# stable tag not yet cut -- and that window is when a release engineer reads the
# line to confirm the bump. Printing the current tag there reported a v1.4.0 tree
# as correct after a correct bump to 1.5.0, so the line could not distinguish a
# bumped tree from an unbumped one.
if [[ "$output" == *'all naming v1.5.0, prepared'* ]]; then
    printf 'ok   %s\n' 'the summary names the prepared version the pins actually name'
else
    printf 'FAIL %s: summary should name v1.5.0, got:\n%s\n' 'summary names the pinned version' "$output" >&2
    fails=$((fails + 1))
fi

# The permissiveness is bounded: only the prepared version, not any version.
# shellcheck disable=SC2016 # literal backticks: the pin shape is markdown, and double quotes would run them
printf '> Pin `--version 1.3.0` here.\n' >"$prep/stale.md"
run_in "$prep" "$prep/stale.md"
if (( status == 1 )) && [[ "$output" == *"1.3.0"* ]]; then
    printf 'ok   %s\n' 'a candidate is tagged -> a genuinely stale pin still fails'
else
    printf 'FAIL %s: expected exit 1 naming 1.3.0, got %d:\n%s\n' 'stale pin still fails' "$status" "$output" >&2
    fails=$((fails + 1))
fi

# Once the stable tag lands the candidate is spent, so the early-pin allowance
# stops: only the released version is acceptable from then on.
released="$(prepared_repo b v1.4.0 v1.5.0-rc.3 v1.5.0)"
# shellcheck disable=SC2016 # literal backticks: the pin shape is markdown, and double quotes would run them
printf '> Pin `--version 1.6.0` here.\n' >"$released/doc.md"
run_in "$released" "$released/doc.md"
if (( status == 1 )); then
    printf 'ok   %s\n' 'the stable tag landed -> no unreleased version is accepted'
else
    printf 'FAIL %s: expected exit 1, got %d:\n%s\n' 'allowance ends at the stable tag' "$status" "$output" >&2
    fails=$((fails + 1))
fi

# Same spent-candidate case, but with a tag list that outgrows the 64 KiB pipe
# buffer. resolve_prepared_release asked "is the stable tag already cut?" through
# `printf | grep -q`, and `grep -q` exits on the match: the writer then takes
# SIGPIPE and `set -o pipefail` reports the dead pipeline as no-match. So a tag
# that IS present reads as absent, the spent candidate looks live, and the gate
# waves through a version that shipped weeks ago. Only a PRESENT value is
# falsified and only past the buffer, which is why every small fixture above
# passes either way -- the padding is what makes this case able to fail (Q982).
pad_tags() {
    local dir="$1" sha i
    sha="$(git -C "$dir" rev-parse HEAD)"
    # `vz-` so the padding sorts after the real tags in refname order (the match
    # must land early, with the writer still holding a buffer's worth to write),
    # and no `-` after a version triple so it is not read as a prerelease.
    for i in $(seq 1 400); do
        printf 'create refs/tags/vz-pad-%s-%0200d %s\n' "$i" "$i" "$sha"
    done | git -C "$dir" update-ref --stdin
}

padded="$(prepared_repo c v1.4.0 v1.5.0-rc.3 v1.5.0)"
pad_tags "$padded"
# shellcheck disable=SC2016 # literal backticks: the pin shape is markdown, and double quotes would run them
printf '> Pin `--version 1.5.0` here.\n' >"$padded/doc.md"
run_in "$padded" "$padded/doc.md"
if (( status == 1 )) && [[ "$output" == *"1.5.0"* ]]; then
    printf 'ok   %s\n' 'a spent candidate stays spent when the tag list outgrows the pipe buffer'
else
    printf 'FAIL %s: expected exit 1 naming 1.5.0, got %d:\n%s\n' \
        'spent candidate past the pipe buffer' "$status" "$output" >&2
    fails=$((fails + 1))
fi

if (( fails > 0 )); then
    printf '\n%d check-release-pins assertion(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\ncheck-release-pins-test: all assertions passed\n'
