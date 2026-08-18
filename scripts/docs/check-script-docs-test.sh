#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-script-docs.sh (Q688) — the shell half:
# file selection, the --readme-relative argument contract, and the exit
# statuses. What counts as a mention is asserted in Go beside the package, where
# the fenced-code and filename-boundary cases live.
#
# The selection assertions are the ones that earn their keep. A coverage gate
# that silently checks nothing passes a drifted tree exactly like a clean one,
# and the specific way this one could stop checking is by missing the file that
# was just added — an untracked script is precisely the shape of a script whose
# README entry was forgotten (Q432/Q619). So both directions are planted in
# throwaway repos: an untracked script must be caught, a gitignored one must not.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECK="$REPO_ROOT/scripts/docs/check-script-docs.sh"

# A fixed path under the repo's gitignored tmp/ rather than `mktemp -d`, per the
# repo temp-file convention: the throwaway repos below are invisible to the real
# tree's own file selection, which the last assertion exercises.
WORKDIR="$REPO_ROOT/tmp/check-script-docs-test"
rm -rf "$WORKDIR"
mkdir -p "$WORKDIR"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# commit DIR — stage and commit everything in a throwaway repo.
commit() {
    git -C "$1" add -A
    git -C "$1" -c user.email=t@t -c user.name=t commit -qm fixture
}

# repo NAME — build a throwaway git repo holding a scripts/ tree with one
# documented script, and echo its path. Callers plant the defect on top.
repo() {
    local dir="$WORKDIR/$1"
    mkdir -p "$dir/scripts/go"
    git -C "$dir" init -q
    # Q820: no detached maintenance racing the next command in a fixture repo.
    git -C "$dir" config maintenance.auto false
    printf '#!/usr/bin/env bash\ntrue\n' >"$dir/scripts/go/go-lint.sh"
    # shellcheck disable=SC2016 # backticks are Markdown code spans, not shell
    {
        printf '# scripts/\n\n## `go/`\n\n'
        printf '| Script | Purpose |\n|---|---|\n'
        printf '| [go-lint.sh](go/go-lint.sh) | Lint. Backs `make lint`. |\n'
    } >"$dir/scripts/README.md"
    commit "$dir"
    printf '%s\n' "$dir"
}

# expect NAME EXPECTED_RC CWD NEEDLE [ARG...] — run the gate from CWD and assert
# its exit code, and that its output mentions NEEDLE. NEEDLE is positional
# rather than an environment prefix: bash keeps an assignment that prefixes a
# *function* call in scope after it returns, so one would leak into the next
# case and assert nothing there.
expect() {
    local name="$1" want_rc="$2" cwd="$3" needle="$4"
    shift 4
    local out rc=0
    out="$(cd "$cwd" && "$CHECK" "$@" 2>&1)" || rc=$?
    if ((rc != want_rc)); then
        printf 'FAIL %-46s rc=%d, want %d\n' "$name" "$rc" "$want_rc"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    if ! grep -qF -- "$needle" <<<"$out"; then
        printf 'FAIL %-46s missing %q in output\n' "$name" "$needle"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    printf 'ok   %-46s rc=%d\n' "$name" "$rc"
}

# --- default selection, in a throwaway tree ---------------------------------

clean="$(repo clean)"
expect "documented tree passes" 0 "$clean" 'all mentioned in'

tracked="$(repo tracked)"
printf '#!/usr/bin/env bash\ntrue\n' >"$tracked/scripts/go/go-vet-tags.sh"
commit "$tracked"
expect "tracked script with no entry fails" 1 "$tracked" 'go-vet-tags.sh'

# The false green this gate is most exposed to: a script is added, its README
# entry is forgotten, and the gate does not see the file until the commit that
# tracks it — by which point `make check` has already gone green on it.
untracked="$(repo untracked)"
printf '#!/usr/bin/env bash\ntrue\n' >"$untracked/scripts/go/go-vet-tags.sh"
expect "untracked script with no entry fails" 1 "$untracked" 'go-vet-tags.sh'

# The opt-out the file-selection contract promises: gitignoring is how a scratch
# script stays out of the gate, not merely leaving it untracked.
ignored="$(repo ignored)"
printf '#!/usr/bin/env bash\ntrue\n' >"$ignored/scripts/go/scratch.sh"
printf 'scripts/go/scratch.sh\n' >"$ignored/.gitignore"
expect "gitignored script is not checked" 0 "$ignored" 'all mentioned in'

# A script mentioned only in its subject's row, with no row and no link of its
# own, is the *-test.sh convention and must pass through the entry point too.
sibling="$(repo sibling)"
printf '#!/usr/bin/env bash\ntrue\n' >"$sibling/scripts/go/go-lint-scope-test.sh"
# shellcheck disable=SC2016 # backticks are Markdown code spans, not shell
printf '| [go-lint.sh](go/go-lint.sh) | Scoping asserted by `go-lint-scope-test.sh` under `make scripts-test`. |\n' \
    >>"$sibling/scripts/README.md"
expect "mention in a sibling row passes" 0 "$sibling" 'all mentioned in'

# --- explicit arguments -----------------------------------------------------

expect "named script overrides selection" 1 "$clean" 'go-vet-tags.sh' scripts/go/go-vet-tags.sh
expect "named documented script passes" 0 "$clean" 'all mentioned in' scripts/go/go-lint.sh

# --- hard errors, never a silent pass ---------------------------------------

drifted="$(repo drifted)"
printf '# scripts/\n\nProse only, no tables.\n' >"$drifted/scripts/README.md"
expect "README with no tables is a hard error" 2 "$drifted" 'cannot judge'

empty="$WORKDIR/empty"
mkdir -p "$empty"
git -C "$empty" init -q
git -C "$empty" config maintenance.auto false
printf 'placeholder\n' >"$empty/README.md"
commit "$empty"
expect "empty file set is a hard error" 2 "$empty" 'no scripts to check'

expect "unknown flag is a hard error" 2 "$clean" 'unknown argument' --nope

# --- the reconciliation this gate exists for --------------------------------
#
# The real tree, checked by the real selection. This is the assertion that goes
# red when a script is added without an entry, which is the drift Q688 recorded.

expect "this repo's own scripts/ tree is documented" 0 "$REPO_ROOT" 'all mentioned in'

if ((fails)); then
    printf '\ncheck-script-docs-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-script-docs-test: all assertions passed\n'
