#!/usr/bin/env bash
#
# Assertions for this repo's foreground-guard slow-command patterns.
#
# `.claude/foreground-guard.json` registers regexes for commands too slow to
# finish inside a foreground Bash timeout, so an agent is asked to background
# them instead. The hook applies each one as `re.compile(pat).search(command)`
# against the whole command string — which is why a pattern written as a bare
# path catches every command that merely NAMES the script: `git show`, `grep`,
# `ls`, a diff. That shipped once and made reading the file a permission prompt.
#
# Both directions are asserted here, because both are silent: a pattern that
# stopped matching lets an hour-long gate run in the foreground and be killed
# mid-run, and one that matches too much turns ordinary reads into prompts.
#
# Python because the hook is Python: bash and `re` disagree about enough
# (\s, \b, alternation) that testing these regexes in bash would assert a
# different language than the one that runs them.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

python3 - <<'PY'
import json
import re
import sys

CONFIG = ".claude/foreground-guard.json"
with open(CONFIG) as fh:
    patterns = json.load(fh)["slow"]["commands"]

# (should_match, command). The must-match cases are the invocation shapes an
# agent or a runbook actually uses; the must-not cases are the read-only
# commands that name the same paths.
CASES = [
    (True, "make test-race"),
    (True, "make -C cmd/agc test-integration"),
    (True, "make e2e"),
    (True, "make e2e SUITE=single-node"),
    (True, "make e2e-up"),
    (True, "make e2e-cluster e2e-images"),
    (True, "make e2e-github-cleanup"),
    (True, "go test -race ./..."),
    (True, "bash scripts/dogfood/release-sentinel.sh"),
    (True, "scripts/dogfood/release-sentinel.sh"),
    (True, 'bash "/Users/someone/repo/scripts/dogfood/release-sentinel.sh"'),
    (True, "PROJECT=x CLUSTER=y bash scripts/dogfood/validate-release.sh v1.3.0-rc.1"),
    (True, "cd /repo && bash scripts/dogfood/validate-release.sh v1.3.0-rc.1"),
    (True, "nohup scripts/dogfood/release-sentinel.sh &"),
    (False, "git show origin/main:scripts/dogfood/release-sentinel.sh"),
    (False, "grep -n progress_status_json scripts/dogfood/release-sentinel.sh"),
    (False, "git log --oneline -- scripts/dogfood/validate-release.sh"),
    (False, "git diff origin/main -- scripts/dogfood/validate-release.sh"),
    (False, "ls -l scripts/dogfood/release-sentinel.sh scripts/dogfood/validate-release.sh"),
    (False, "shellcheck scripts/dogfood/validate-release.sh"),
    # The fast neighbours of the registered scripts must stay unregistered, or
    # `make scripts-test` itself starts asking.
    (False, "bash scripts/dogfood/validate-release-test.sh"),
    (False, "bash scripts/dogfood/release-sentinel-test.sh"),
    (False, "make test"),
    # A target name mentioned in a quoted argument is not an invocation. The
    # e2e pattern was `make .*\be2e`, whose `.*` reached past the target into
    # the argument, so allocating a backlog ID about e2e was denied as an e2e
    # run (upstream karlkfi/claude-foreground-guard#21).
    (False, 'make queue-id TITLE="No per-spec filter on the e2e make target"'),
    (False, 'make -n help NOTE="a note mentioning e2e somewhere"'),
]

fails = 0
for want, cmd in CASES:
    hit = next((p for p in patterns if re.search(p, cmd)), None)
    got = hit is not None
    if got == want:
        print(f"ok   match={str(got):5} {cmd}")
    else:
        fails += 1
        print(f"FAIL want={want} got={got} {cmd}", file=sys.stderr)
        if hit:
            print(f"       caught by: {hit}", file=sys.stderr)

# This file only runs inside `make scripts-test`, which CI reaches through the
# `scripts` path filter. CONFIG has to be in that filter or a config-only change
# skips this test and an over-matching pattern ships unasserted.
WORKFLOW = ".github/workflows/unit-test.yml"
with open(WORKFLOW) as fh:
    block = re.search(r"\n(\s+)scripts:\n((?:\1  .*\n)+)", fh.read())
if not block:
    print(f"FAIL no `scripts` filter found in {WORKFLOW}", file=sys.stderr)
    fails += 1
elif f"'{CONFIG}'" not in block.group(2):
    fails += 1
    print(f"FAIL {CONFIG} missing from the `scripts` filter in {WORKFLOW};", file=sys.stderr)
    print("       a config-only change would skip this test", file=sys.stderr)
else:
    print(f"ok   {CONFIG} is in the `scripts` filter of {WORKFLOW}")

print()
if fails:
    print(f"{fails} case(s) wrong", file=sys.stderr)
    sys.exit(1)
print("all assertions passed")
PY
