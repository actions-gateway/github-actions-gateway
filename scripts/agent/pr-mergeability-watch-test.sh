#!/usr/bin/env bash
#
# Unit tests for scripts/agent/pr-mergeability-watch.py, exercised through its
# own module rather than through the wrapper: the runner and the sleeper are
# injected, so no case touches the network or a real clock.
#
# The budget counts what the stub sleeper was asked to sleep. That single-clock
# property is what makes the timeout assertable at all, and it is why the shell
# predecessor's tests could not be kept when Q889 adopted the Python: they
# stubbed the `sleep` *binary*, which `time.sleep` never calls, so every case
# would have slept for real.
#
# Both directions, because both fail silently: it must exit on DIRTY, BEHIND and
# a closed PR, and must **not** exit on CLEAN, BLOCKED or UNKNOWN. BLOCKED is
# the one upstream's suite does not cover — it means checks failing or a review
# outstanding, which is not a conflict, and a watch that woke on it would fire
# on every red PR in a batch.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
W="$HERE/pr-mergeability-watch.py"

if python3 - "$W" <<'PY'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("w", sys.argv[1])
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)

fails = []


def check(name, got, want):
    if got == want:
        print(f"ok   {name}")
    else:
        fails.append(f"{name}: got {got!r} want {want!r}")


def replies(*payloads):
    """A runner that returns each payload in turn, then repeats the last."""
    seq = list(payloads)

    def run():
        item = seq.pop(0) if len(seq) > 1 else seq[0]
        if isinstance(item, Exception):
            raise item
        return item
    return run


def counting_sleeper():
    calls = []
    return calls, calls.append


OPEN_CLEAN = {"state": "OPEN", "mergeStateStatus": "CLEAN", "baseRefName": "main"}

# --- the events -----------------------------------------------------------

ev, _ = w.watch(1, replies({**OPEN_CLEAN, "mergeStateStatus": "DIRTY"}),
                lambda s: None)
check("DIRTY wakes as conflict", ev, "conflict")

ev, _ = w.watch(1, replies({**OPEN_CLEAN, "mergeStateStatus": "BEHIND"}),
                lambda s: None)
check("BEHIND wakes as conflict", ev, "conflict")

ev, detail = w.watch(1, replies({**OPEN_CLEAN, "state": "MERGED"}),
                     lambda s: None)
check("a merged PR exits closed", ev, "closed")
check("closed names the state", "MERGED" in detail, True)

# UNKNOWN is GitHub still computing, not a conflict — treating it as one would
# wake the orchestrator for every freshly pushed PR.
slept, sleeper = counting_sleeper()
ev, _ = w.watch(1, replies({**OPEN_CLEAN, "mergeStateStatus": "UNKNOWN"}),
                sleeper, interval=60, budget=120)
check("UNKNOWN does not wake as conflict", ev, "timeout")
check("UNKNOWN keeps polling until the budget", len(slept), 2)

# BLOCKED means a failing check or an outstanding review, which the owning
# worker is already handling. Waking on it would fire for every red PR.
slept, sleeper = counting_sleeper()
ev, _ = w.watch(1, replies({**OPEN_CLEAN, "mergeStateStatus": "BLOCKED"}),
                sleeper, interval=60, budget=120)
check("BLOCKED does not wake as conflict", ev, "timeout")
check("BLOCKED keeps polling until the budget", len(slept), 2)

# --- the budget is counted in slept time, not wall clock ------------------

slept, sleeper = counting_sleeper()
ev, _ = w.watch(1, replies(OPEN_CLEAN), sleeper, interval=30, budget=90)
check("clean PR times out on budget", ev, "timeout")
check("budget counts the stub's sleeps", sum(slept), 90)

# --- transport failures ---------------------------------------------------

slept, sleeper = counting_sleeper()
ev, detail = w.watch(1, replies(w.GhError("boom")), sleeper, interval=1,
                     budget=10_000)
check("repeated gh failure exits error", ev, "error")
check("error reports the last output", "boom" in detail, True)
check("it retries to the cap first", len(slept), w.MAX_CONSECUTIVE_FAILURES - 1)

# A failure that recovers must not count toward the cap.
ev, _ = w.watch(1, replies(w.GhError("blip"),
                           {**OPEN_CLEAN, "mergeStateStatus": "DIRTY"}),
                lambda s: None, interval=1)
check("a recovered failure resets the counter", ev, "conflict")

# --- the base branch ------------------------------------------------------

_, detail = w.watch(1, replies({"state": "OPEN", "mergeStateStatus": "DIRTY",
                                "baseRefName": "main"}), lambda s: None)
check("targeting trunk says rebase onto it", "origin/main" in detail, True)
check("targeting trunk does not claim stacked", "stacked" not in detail, True)

_, detail = w.watch(1, replies({"state": "OPEN", "mergeStateStatus": "DIRTY",
                                "baseRefName": "claude/base-pr"}),
                    lambda s: None)
check("a stacked PR is named as stacked", "stacked" in detail, True)
check("stacked names its own base", "origin/claude/base-pr" in detail, True)
check("stacked carries the already-merged clause",
      "already merged" in detail, True)

# An unreadable base must drop to the branchless wording rather than being
# interpolated — this is the injection boundary, not a formatting nicety.
for bad in ("main; rm -rf /", "-flag", "", "a b"):
    _, detail = w.watch(1, replies({"state": "OPEN", "mergeStateStatus": "DIRTY",
                                    "baseRefName": bad}), lambda s: None)
    if bad and bad in detail:
        fails.append(f"unsafe base {bad!r} reached the wake text")
check("an unusable base is refused, not interpolated", True, True)

# --- the field boundary ---------------------------------------------------

check("only three fields are ever requested", set(w.FIELDS),
      {"state", "mergeStateStatus", "baseRefName"})
for forbidden in ("body", "comments", "reviews", "title"):
    if forbidden in w.FIELDS:
        fails.append(f"{forbidden} is readable, which is an injection channel")

# read_fields must not crash on a payload missing everything.
check("a missing payload degrades rather than raising",
      w.read_fields({}), ("", "", ""))

for f in fails:
    print(f"FAIL {f}")
sys.exit(1 if fails else 0)
PY
then
    printf '\npr-mergeability-watch-test: ok\n'
else
    printf '\npr-mergeability-watch-test: FAILED\n'
    exit 1
fi
