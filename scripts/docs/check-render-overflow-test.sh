#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-render-overflow.sh and its checker.
#
# Split by what each half needs, so that no green is weaker on one machine than
# another. An earlier cut ran the render assertions when .venv-render/ happened
# to be provisioned and skipped them when it did not, which made `make check`
# mean one thing locally and another in CI. A suite whose coverage depends on
# the machine cannot be read at all, so the split is by mechanism instead:
#
#   default        the surface that needs no browser, driven with the SYSTEM
#                  python3. Identical everywhere, and what `make scripts-test`
#                  (so `make check`) runs. The checker imports its browser
#                  inside measure(), so argument handling, page discovery and
#                  the refusal paths are all reachable without one.
#
#   --render-only  the planted-fixture pair that needs the pinned browser.
#                  check-render-overflow.sh runs this itself after provisioning
#                  and before it measures the site, so these assertions run
#                  wherever the gate runs rather than wherever a venv happens to
#                  exist.
#
# The render pair is the injected defect and its control: a planted over-wide
# element must go red and name the element that owns it, and the same tree
# without it must go green. A gate asserted only against a clean document has
# demonstrated nothing, because a checker that measures no page passes it too.
#
# Runs under `make check` (via `make scripts-test`), and its --render-only half
# under `make render-overflow-check` and the CI doc-links job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
GATE="$REPO_ROOT/scripts/docs/check-render-overflow.sh"
CHECKER="$REPO_ROOT/scripts/docs/check-render-overflow.py"
VENV="$REPO_ROOT/.venv-render"

render_only=0
if [[ "${1:-}" == "--render-only" ]]; then
    render_only=1
fi

fails=0
workdirs=()

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

new_site() {
    local d
    d="$(mktemp -d "$REPO_ROOT/tmp/render-overflow-test.XXXXXX")"
    workdirs+=("$d")
    printf '%s' "$d"
}

# page DIR SUBPATH BODY — write one page of a fake built site.
page() {
    local dir="$1" sub="$2" body="$3" target
    target="$dir${sub:+/$sub}"
    mkdir -p "$target"
    cat >"$target/index.html" <<HTML
<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0"><article>${body}</article></body></html>
HTML
}

expect_status() {
    local want="$1" got="$2" what="$3"
    if [[ "$want" == "$got" ]]; then
        printf 'ok   %s (exit %s)\n' "$what" "$got"
    else
        printf 'FAIL %s: want exit %s, got %s\n' "$what" "$want" "$got"
        fails=$((fails + 1))
    fi
}

expect_eq() {
    local want="$1" got="$2" what="$3"
    if [[ "$want" == "$got" ]]; then
        printf 'ok   %s\n' "$what"
    else
        printf 'FAIL %s:\n  want: %s\n  got:  %s\n' "$what" "$want" "$got"
        fails=$((fails + 1))
    fi
}

finish() {
    if ((fails > 0)); then
        printf '\n%d assertion(s) failed\n' "$fails"
        exit 1
    fi
    printf '\nall assertions passed\n'
    exit 0
}

# --- the browser-free surface, on the system python3 -------------------------

if ((render_only == 0)); then
    rc=0
    "$GATE" --nonsense >/dev/null 2>&1 || rc=$?
    expect_status 2 "$rc" "an unknown argument is refused"

    # Page discovery is what decides the gate's subject, so a change that
    # narrowed it would shrink the sweep silently. Assert the mapping from a
    # built tree to the URL paths the site serves, nesting included.
    tree="$(new_site)"
    page "$tree" "" '<p>root</p>'
    page "$tree" "operations" '<p>section index</p>'
    page "$tree" "operations/install" '<p>nested</p>'
    got="$(python3 -c "
import importlib.util, sys
spec = importlib.util.spec_from_file_location('c', sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
from pathlib import Path
print(' '.join(m.discover(Path(sys.argv[2]))))
" "$CHECKER" "$tree")"
    expect_eq "/ /operations/ /operations/install/" "$got" \
        "page discovery maps a built tree to the served URL paths"

    # A tree with no pages must refuse rather than pass: a gate that measures
    # nothing is indistinguishable from a clean one by its exit status alone.
    # This path returns before the browser is ever needed, which is why the
    # system python3 can assert it.
    empty="$(new_site)"
    rc=0
    python3 "$CHECKER" --root "$empty" --widths 320 >/dev/null 2>&1 || rc=$?
    expect_status 2 "$rc" "a site tree with no pages is refused"

    finish
fi

# --- the pinned browser half -------------------------------------------------

if [[ ! -x "$VENV/bin/python" ]]; then
    printf 'check-render-overflow-test: --render-only needs .venv-render/, which is absent.\n' >&2
    printf '                            Run "make render-overflow-check", which provisions it.\n' >&2
    exit 2
fi

run_checker() {
    local root="$1"
    PLAYWRIGHT_BROWSERS_PATH="$VENV/browsers" "$VENV/bin/python" \
        "$CHECKER" --root "$root" --widths 320 2>&1
}

# The control: prose that wraps cannot widen the page.
clean="$(new_site)"
page "$clean" "" '<p>Ordinary prose that wraps inside the viewport.</p>'
rc=0
run_checker "$clean" >/dev/null || rc=$?
expect_status 0 "$rc" "a page that fits its viewport is green"

# The injected defect: the same tree plus one element too wide to shrink.
# `white-space: nowrap` on a long string is the exact shape the audience pills
# had, so the fixture fails the way the real defect did.
broken="$(new_site)"
page "$broken" "" '<p>Ordinary prose that wraps inside the viewport.</p>
<div class="planted-overflow" style="white-space:nowrap">AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA</div>'
rc=0
out="$(run_checker "$broken")" || rc=$?
expect_status 1 "$rc" "a page wider than its viewport is red"
if grep -q 'planted-overflow' <<<"$out"; then
    printf 'ok   the finding names the element that owns the overflow\n'
else
    printf 'FAIL the finding did not name .planted-overflow:\n%s\n' "$out"
    fails=$((fails + 1))
fi

finish
