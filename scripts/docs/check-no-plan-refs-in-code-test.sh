#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-no-plan-refs-in-code.sh — the gate that
# keeps code from citing plan docs by path.
#
# Reading the matcher predicts coverage; planting the defect measures it. Each
# case builds a throwaway git repo, plants one fixture, and asserts the gate's
# exit status — the red cases prove the gate sees a stale citation in a shell
# comment, a workflow comment and Go source, and the green cases pin the
# exceptions that must NOT trip it: a workflow `paths:` filter, a plan file a
# script opens as data, the inline opt-out marker, a bare directory reference
# and the plan index. A last group covers file *selection* rather than matching
# — an untracked file is scanned and a gitignored one is not (Q619).
#
# The fixtures are built with printf from the variables below rather than
# written as literal comment lines, because this file is itself a tracked shell
# script the gate scans: a literal `# ...<plan path>` line here would make the
# suite fail its own subject. Values are exempt by design, which is the same
# property case 11 asserts.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
GATE="$REPO_ROOT/scripts/docs/check-no-plan-refs-in-code.sh"

plan_ref='docs/plan/some-plan.md'
archived_ref='docs/plan/archive/some-plan.md'
index_ref='docs/plan/README.md'

fails=0
workdirs=()
WORK=""

# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11
# misses that whenever the script ends in an explicit `exit`.
cleanup() {
    local d
    for d in "${workdirs[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo — start a throwaway git repo and point WORK at it. The gate resolves
# its root with `git rev-parse --show-toplevel`, so running it from inside this
# directory scopes it to the fixture rather than the real tree.
new_repo() {
    WORK="$(mktemp -d)"
    workdirs+=("$WORK")
    git -C "$WORK" init -q
}

# run_gate NAME WANT — run the gate inside WORK against whatever is on disk and
# compare its exit status with WANT.
run_gate() {
    local name="$1" want="$2" got=0
    ( cd "$WORK" && "$GATE" ) >"$WORK/gate.out" 2>&1 || got=$?
    die_if_killed "$name" "$got" "$want"
    if [[ "$got" == "$want" ]]; then
        printf 'ok   %s\n' "$name"
        return
    fi
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$want" "$got"
    awk '{ print "    " $0 }' "$WORK/gate.out"
    fails=$(( fails + 1 ))
}

# expect NAME WANT — stage whatever the case planted, then run_gate.
expect() {
    git -C "$WORK" add -A
    run_gate "$@"
}

# --- red: the defect the gate exists to catch ------------------------------

new_repo
printf '#!/usr/bin/env bash\n# See %s Part F.\nset -euo pipefail\n' "$plan_ref" \
    >"$WORK/script.sh"
expect 'shell comment citing a plan file is rejected' 1

new_repo
mkdir -p "$WORK/.github/workflows"
printf 'name: w\n# Rationale: see %s.\non: push\n' "$archived_ref" \
    >"$WORK/.github/workflows/w.yml"
expect 'workflow comment citing an archived plan file is rejected' 1

new_repo
printf '#!/usr/bin/env bash\nfoo=1  # sizing rationale: %s\n' "$plan_ref" \
    >"$WORK/script.sh"
expect 'shell trailing comment citing a plan file is rejected' 1

new_repo
printf 'package p\n\n// Context: %s\n' 'docs/plan/' >"$WORK/a.go"
expect 'Go keeps the strict rule: a bare plan-tree path is rejected' 1

new_repo
printf 'package p\n\nconst planDoc = "%s"\n' "$plan_ref" >"$WORK/a.go"
expect 'Go string literal naming a plan doc is still rejected' 1

# The index carve-out must not become a hole: an archived plan reached through
# the index's own directory is still a plan file, so it stays rejected.
new_repo
printf 'package p\n\nconst planDoc = "docs/plan/archive/old.md"\n' >"$WORK/a.go"
expect 'Go: an archived plan doc is rejected despite the index exemption' 1

# --- green: the exceptions that must survive -------------------------------

# The plan index never moves, and the merge driver that resolves it is Go and
# names it as the file it merges. Both shapes a driver actually uses.
new_repo
printf 'package p\n\nconst target = "docs/plan/README.md"\n' >"$WORK/a.go"
expect 'Go naming the plan index as a value is allowed' 0

new_repo
printf 'package p\n\n// The index this driver merges: docs/plan/README.md\n' >"$WORK/a.go"
expect 'Go citing the plan index in a comment is allowed' 0

new_repo
mkdir -p "$WORK/.github/workflows"
{
    printf 'name: plan-hygiene\non:\n  pull_request:\n    paths:\n'
    printf "      - '%s'\n" 'docs/plan/**' "$index_ref" "$plan_ref"
} >"$WORK/.github/workflows/w.yml"
expect 'workflow paths: filter naming plan docs stays green' 0

new_repo
printf '#!/usr/bin/env bash\nMILESTONE="%s"\necho "updating %s"\n' \
    "$plan_ref" "$plan_ref" >"$WORK/script.sh"
expect 'shell value naming a plan file it opens stays green' 0

new_repo
printf '#!/usr/bin/env bash\n# Rewrites %s. no-plan-refs: a path this opens.\n' \
    "$plan_ref" >"$WORK/script.sh"
expect 'inline no-plan-refs marker exempts exactly that line' 0

new_repo
printf '#!/usr/bin/env bash\n# The release scope excludes %s entirely.\n' \
    'docs/plan/' >"$WORK/script.sh"
expect 'bare plan directory reference stays green' 0

new_repo
printf '#!/usr/bin/env bash\n# See the dogfood plan (indexed in %s).\n' \
    "$index_ref" >"$WORK/script.sh"
expect 'the plan index README stays green' 0

# The whole-string trap: a path that merely appears in prose or in a message
# string is not a citation the archival tax applies to, and the gate must not
# match text just because it names the path.
new_repo
printf '# Notes\n\nSee %s for the full log.\n' "$archived_ref" >"$WORK/notes.md"
printf '#!/usr/bin/env bash\nmsg="docs: move %s to the archive"\n' "$plan_ref" \
    >"$WORK/script.sh"
expect 'a doc or a message string naming the path stays green' 0

# --- file selection: untracked is scanned, gitignored is the opt-out ---------
#
# The cases above stage before running, so they only ever measured the tracked
# half. Both lists listed `--cached` only until Q619, which made a brand-new
# script, workflow or Go file invisible to its own first `make check` — the
# citation surfaced on the next run, after the gate had been reported green.

new_repo
printf '#!/usr/bin/env bash\n# See %s Part F.\nset -euo pipefail\n' "$plan_ref" \
    >"$WORK/script.sh"
run_gate 'untracked shell comment citing a plan file is rejected' 1

new_repo
printf 'package p\n\n// Context: %s\n' "$plan_ref" >"$WORK/a.go"
run_gate 'untracked Go file citing a plan file is rejected' 1

new_repo
printf 'scratch.sh\nscratch.go\n' >"$WORK/.gitignore"
printf '#!/usr/bin/env bash\n# See %s Part F.\n' "$plan_ref" >"$WORK/scratch.sh"
printf 'package p\n\n// Context: %s\n' "$plan_ref" >"$WORK/scratch.go"
run_gate 'a gitignored file is the opt-out and stays green' 0

if (( fails > 0 )); then
    printf '\ncheck-no-plan-refs-in-code-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-no-plan-refs-in-code-test: all assertions passed\n'
exit 0
