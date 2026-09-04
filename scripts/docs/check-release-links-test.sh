#!/usr/bin/env bash
#
# Assertions for scripts/docs/check-release-links.sh — the gate that resolves a
# release note's absolute site links against a local docs-site build (Q636).
#
# The gate's value is entirely in the failures it produces, so most cases below
# plant one: a resolver that quietly matches nothing passes a broken note exactly
# like a good one. Each case runs against a throwaway notes dir and a hand-built
# site tree, so nothing about the repo's real docs leaks into a verdict.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job. The
# gate itself is not in `make check` — its oracle is an mkdocs build — but these
# fixtures need no mkdocs, so the logic stays covered by the fast gate.

set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
readonly REPO_ROOT
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
readonly GATE="$REPO_ROOT/scripts/docs/check-release-links.sh"
readonly HOST="docs.example.test"

WORKDIR="$(mktemp -d)"
readonly WORKDIR
trap 'rm -rf "$WORKDIR"' EXIT

# A site tree shaped the way `mkdocs build` lays one out: directory URLs, each
# page an index.html whose headings carry id attributes.
SITE="$WORKDIR/site"
readonly SITE
mkdir -p "$SITE/operations/upgrade" "$SITE/operations/troubleshooting"
printf '<h1 id="home">Home</h1>\n' >"$SITE/index.html"
printf '<h1 id="upgrading">Upgrading</h1>\n<h2 id="gmc-rollback">GMC rollback</h2>\n' \
    >"$SITE/operations/upgrade/index.html"
printf '<h1 id="troubleshooting">Troubleshooting</h1>\n' \
    >"$SITE/operations/troubleshooting/index.html"

fails=0
n=0

# make_notes VERSION LINE... -> path to a notes dir holding one <VERSION>.md
make_notes() {
    local version="$1" dir
    shift
    n=$((n + 1))
    dir="$WORKDIR/notes-$n"
    mkdir -p "$dir"
    printf '%s\n' "$@" >"$dir/$version.md"
    printf '%s\n' "$dir"
}

# run NOTES_DIR [SITE_DIR] -> exit status in $status, combined output in $output
status=0
output=''
run() {
    status=0
    output="$(GAG_SITE_DIR="${2:-$SITE}" GAG_RELEASE_NOTES_DIR="$1" GAG_SITE_HOST="$HOST" \
        "$GATE" 2>&1)" || status=$?
}

# case NAME WANT_STATUS NOTES_DIR NEEDLE — NEEDLE keeps a case from passing on an
# unrelated failure, or on a success that checked nothing.
case_is() {
    local name="$1" want="$2" dir="$3" needle="$4"
    run "$dir"
    die_if_killed "$name" "$status" "$want"
    if (( status == want )) && [[ "$output" == *"$needle"* ]]; then
        printf 'ok   %s\n' "$name"
    else
        printf 'FAIL %s: wanted exit %d mentioning %q, got %d:\n%s\n' \
            "$name" "$want" "$needle" "$status" "$output" >&2
        fails=$((fails + 1))
    fi
}

case_is 'a page and anchor the site serves' 0 \
    "$(make_notes v1.3.0 "See [rollback](https://$HOST/1.3.0/operations/upgrade/#gmc-rollback).")" \
    'ok (1 link(s) into 1.3.0 resolved'

case_is 'an anchor no heading carries' 1 \
    "$(make_notes v1.3.0 "See [x](https://$HOST/1.3.0/operations/upgrade/#no-such-heading).")" \
    'dead site anchor'

case_is 'a page the build does not hold' 1 \
    "$(make_notes v1.3.0 "See [x](https://$HOST/1.3.0/operations/imaginary/).")" \
    'dead site link'

# The control that keeps the gate honest about its own reach: a third-party URL
# has no local oracle, so even a plainly bogus anchor on one must not fail.
case_is 'a third-party URL is reported, never failed' 0 \
    "$(make_notes v1.3.0 \
        "See [spec](https://kubernetes.example/docs/#not-a-real-anchor)." \
        "And [ok](https://$HOST/1.3.0/operations/upgrade/#gmc-rollback).")" \
    '1 link(s) to other hosts not checked'

case_is 'the version root resolves to the site index' 0 \
    "$(make_notes v1.3.0 "The [docs](https://$HOST/1.3.0/) for this release.")" \
    'ok (1 link(s)'

# MkDocs serves directory URLs, so a missing trailing slash is a redirect rather
# than a 404. Resolve it the same way instead of reporting a phantom break.
case_is 'a directory URL without its trailing slash still resolves' 0 \
    "$(make_notes v1.3.0 "See [x](https://$HOST/1.3.0/operations/troubleshooting).")" \
    'ok (1 link(s)'

# site/ is built from this tree, which publishes exactly one version. Links to
# any other one are unresolvable here — skipped, but never silently.
case_is 'a link to another version is skipped out loud' 0 \
    "$(make_notes v1.3.0 \
        "Old [x](https://$HOST/1.2.0/operations/upgrade/#gmc-rollback)." \
        "New [y](https://$HOST/1.3.0/operations/upgrade/#gmc-rollback).")" \
    '1 link(s) to version 1.2.0 skipped'

case_is 'an unversioned site link is reported as unresolvable' 0 \
    "$(make_notes v1.3.0 \
        "Float [x](https://$HOST/operations/upgrade/)." \
        "Pin [y](https://$HOST/1.3.0/operations/upgrade/).")" \
    'no version prefix, not resolvable'

# A URL inside a code block is sample text, not a link an operator can follow.
case_is 'a URL in a fenced block is not a link' 0 \
    "$(make_notes v1.3.0 \
        'Run:' '```bash' "curl https://$HOST/1.3.0/operations/imaginary/" '```' \
        "See [x](https://$HOST/1.3.0/operations/upgrade/).")" \
    'ok (1 link(s)'

case_is 'a URL in an inline code span is not a link' 0 \
    "$(make_notes v1.3.0 \
        "Set the base to \`https://$HOST/1.3.0/operations/imaginary/\` first." \
        "See [x](https://$HOST/1.3.0/operations/upgrade/).")" \
    'ok (1 link(s)'

# The reconciliation guard. Notes that yield no site link at all mean the
# extractor stopped matching, which reads identically to a clean tree.
case_is 'notes with no site link are a failure, not a pass' 1 \
    "$(make_notes v1.3.0 'This release changed nothing worth linking.')" \
    'the extractor matching nothing'

# Version selection: the newest note names the version site/ stands in for, and
# newest is by version order — lexically, v1.9.0 would beat v1.10.0.
multi="$WORKDIR/notes-multi"
mkdir -p "$multi"
printf 'Old [x](https://%s/1.9.0/operations/upgrade/#gmc-rollback).\n' "$HOST" >"$multi/v1.9.0.md"
printf 'New [y](https://%s/1.10.0/operations/upgrade/#gmc-rollback).\n' "$HOST" >"$multi/v1.10.0.md"
case_is 'the newest note by version order picks the resolvable version' 0 \
    "$multi" 'resolved against'
case_is 'and the older note is the one skipped' 0 \
    "$multi" '1 link(s) to version 1.9.0 skipped'

# Every link naming some other version leaves nothing checked. Legitimate — the
# author cannot fix it — but it must never read as a clean pass.
older="$WORKDIR/notes-older"
mkdir -p "$older"
printf 'Old [x](https://%s/1.2.0/operations/upgrade/).\n' "$HOST" >"$older/v1.3.0.md"
case_is 'nothing resolvable warns rather than reporting a clean run' 0 \
    "$older" 'WARNING — 0 of 1 site link(s) were resolvable'

# Oracle integrity. An explicit GAG_SITE_DIR is a tree the caller named, so a
# missing or unbuilt one is an error — building a different tree, or passing,
# would both be lies about what was checked.
good_notes="$(make_notes v1.3.0 "See [x](https://$HOST/1.3.0/operations/upgrade/).")"
readonly good_notes

run "$good_notes" "$WORKDIR/no-such-site"
die_if_killed "a missing GAG_SITE_DIR is an error" "$status"
if (( status == 2 )) && [[ "$output" == *'Nothing was checked'* ]]; then
    printf 'ok   %s\n' 'a missing GAG_SITE_DIR is an error, not a skip'
else
    printf 'FAIL %s: wanted exit 2, got %d:\n%s\n' \
        'a missing GAG_SITE_DIR is an error' "$status" "$output" >&2
    fails=$((fails + 1))
fi

mkdir -p "$WORKDIR/empty-site"
run "$good_notes" "$WORKDIR/empty-site"
die_if_killed "a site dir with no index.html is an error" "$status"
if (( status == 2 )) && [[ "$output" == *'not a docs-site build'* ]]; then
    printf 'ok   %s\n' 'a site dir with no index.html is an error'
else
    printf 'FAIL %s: wanted exit 2, got %d:\n%s\n' \
        'a site dir with no index.html is an error' "$status" "$output" >&2
    fails=$((fails + 1))
fi

# No notes at all is a fresh fork, not a broken tree.
mkdir -p "$WORKDIR/no-notes"
run "$WORKDIR/no-notes"
die_if_killed "no release notes -> skip" "$status"
if (( status == 0 )) && [[ "$output" == *SKIP* ]]; then
    printf 'ok   %s\n' 'no release notes -> skip, not fail'
else
    printf 'FAIL %s: wanted a skip, got %d:\n%s\n' 'no release notes -> skip' "$status" "$output" >&2
    fails=$((fails + 1))
fi

if (( fails > 0 )); then
    printf '\n%d check-release-links assertion(s) failed\n' "$fails" >&2
    exit 1
fi
printf '\ncheck-release-links-test: all assertions passed\n'
