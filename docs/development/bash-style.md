# Bash style

Conventions for every shell script in this repo — `scripts/`, `.githooks/`, and (where practical) inline `run:` blocks in `.github/workflows/`.
Every tracked script under `scripts/` is linted against these by `make shellcheck`, which is part of `make check` (see [testing.md § The shellcheck gate](testing.md#the-shellcheck-gate)).

## Rules

- Every script must start with `set -euo pipefail`, plus `shopt -s inherit_errexit` in any script that assigns from a command substitution: `set -e` does not reach inside one (see [§ `set -e` stops at a command substitution](#set--e-stops-at-a-command-substitution)).
- Use `local` for all variables inside functions.
- Use `[[ ]]` for conditionals and `(( ))` for arithmetic — never `[ ]`.
- Quote all variable expansions (`"$var"`, `"${arr[@]}"`) unless word-splitting is explicitly intended — annotate that intent with a comment.
- When background processes need cleanup, register a `trap cleanup EXIT INT TERM` function that kills tracked PIDs.
- Every loop must be able to stop itself: a bounded iteration count or a stop-file it polls, never an unbounded loop that an external kill has to end (see [§ A loop must be able to stop itself](#a-loop-must-be-able-to-stop-itself)).
- Prefer `awk -v name="$value" '...'` over `sed` for substitutions involving variables — `sed` delimiter and metacharacter (`/`, `&`, `\`) issues are a common source of bugs.
- When capturing the exit code of a pipeline via `wait`, wrap it in a subshell (`( cmd | other ) &`) so `$!` is the subshell's PID and `wait` reflects the pipeline result under `pipefail`, not just the last process's exit code.

## A loop must be able to stop itself

An unbounded loop can only be ended from outside, and that external kill is the problem: it gets issued as a `pkill -f <pattern>`, the pattern matches every parallel worktree, and it carries no record of whose run it is hitting ([testing.md § Stopping a run](testing.md#stopping-a-run-name-the-target-never-the-program)).
Give the loop its own stop condition and the kill is never needed.

- A **bounded iteration count** when the work is countable: `for i in $(seq 1 40)`.
- A **stop-file the loop polls** when it is not: `while [[ ! -f "${stop_file}" ]]`.
  Touching the file ends the run through the normal exit path, so the cleanup `trap` still runs.

**Do not reach for `timeout`.** It ships with GNU coreutils and is absent from a stock macOS, where neither `timeout` nor `gtimeout` is on `PATH`.
A script that depends on it runs unbounded on a dev Mac while looking correct.
That is the same silent-absence trap that broke Q690's load harness, which backgrounded its generators with `setsid`: also absent on macOS, so all three died instantly and 40 samples passed against an idle machine, reading as strong evidence of no flake.
`timeout` is also still a kill, just one on a timer, so the signal can skip the cleanup `trap` that a self-terminating loop unwinds through.

This applies hardest to throwaway harnesses under the gitignored `tmp/`.
Being a throwaway is exactly why such a script gets none of the review a tracked one does, and it is where every unbounded loop in this repo's history has actually come from.

## `set -e` stops at a command substitution

`set -euo pipefail` does not cover `x="$(build_fixture)"`.
Inside the substitution a failing command neither aborts nor propagates, and the assignment takes the status of the builder's *last* command, so a fixture that broke on its third step reports success.
Measured on bash 5.3, with a builder whose first two steps fail:

| Form | Exit status | Steps run after the first failure |
|---|---|---|
| `repo="$(build)"` | 0 | 2 |
| `repo=$(build)` | 0 | 2 |
| `local repo; repo="$(build)"` | 0 | 2 |
| `local repo="$(build)"` | 0 | 2 |
| `repo="$(set -e; build)"` | 1 | 0 |
| `shopt -s inherit_errexit` then `repo="$(build)"` | 1 | 0 |
| `shopt -s inherit_errexit` then `local repo="$(build)"` | **0** | 0 |
| `build` called directly, no substitution | 1 | 0 |

`shopt -s inherit_errexit` is the remedy to reach for: one line at the top of the file covers every *plain* substitution in it, where the other two working forms have to be repeated per call site.
It is required in every executable script under `scripts/`, and [`make errexit-prologue-check`](../../scripts/ci/check-errexit-prologue.sh) enforces that.
Sourced `lib/` files are exempt and must not declare it: they run under the caller's shell options, a caller's shopt already covers the functions they define, and declaring it in a sourced file would switch the option on for every caller instead.

**It does not cover a declaration builtin.** The last-but-one row above is the trap: `local`, `declare`, `export` and `readonly` return their *own* status, which replaces the substitution's, so `local repo="$(build)"` stays exit 0 even with the shopt set.
The shopt still stops the builder at its first failure, which makes this the worst of the three states: a value truncated *and* a clean exit.
Split the declaration from the assignment, which is the one form that reports:

```bash
local repo
repo="$(build)"
```

shellcheck's SC2155 already rejects the combined form, and `make shellcheck` runs in the same `make check`, so this half of the class is closed by a gate that predates the rule.
The prologue gate deliberately leaves it alone rather than half-duplicating it.

### bash 4.4 is a declared host prerequisite

Requiring the shopt everywhere makes the shell version it needs a hard dependency, and until Q751 nothing said so.
Measured on 2026-08-11: 175 of the 185 scripts under `scripts/` carry the strict line, 3 carry the tolerant form below, and 7 are sourced `lib/` files that inherit their caller's options.

bash is therefore a `required`-tier entry in [`check-tools.sh`](../../scripts/ci/check-tools.sh) with a declared minimum of 4.4, and a prerequisite in [`CONTRIBUTING.md`](../../CONTRIBUTING.md#the-bash-floor).
The registry gained a seventh field for that minimum; leave it empty for a tool with no floor the project actually depends on, because it costs a `--version` fork per check.

`check-tools.sh` is itself one of the scripts that needs 4.4, so its own floor check sits **above** its prologue in 3.2-safe syntax.
Without that the tool meant to diagnose an old bash would die of the old bash first, reporting `invalid shell option name` like everything else.
Both directions are asserted by [`check-tools-test.sh`](../../scripts/ci/check-tools-test.sh), which stubs a `bash` on `PATH` to fake the version and, where the host has a real pre-4.4 bash to measure with, runs the checker under it.

### The Claude Code hooks swallow the shopt's own failure

`inherit_errexit` arrived in bash 4.4, and stock macOS still ships 3.2 at `/bin/bash`.
There the `shopt` itself fails, and `set -e` turns that into a non-zero exit before the script does anything.
For the three `PreToolUse` hooks in [`scripts/agent/`](../../scripts/agent/) that breaks a contract that outranks this one: a hook must never block a tool call, on any bash it happens to run under.
They alone carry the tolerant form, and the gate rejects it everywhere else:

```bash
set -euo pipefail
shopt -s inherit_errexit 2>/dev/null || true
```

On bash 4.4+ this is identical to the strict line; on 3.2 it gives up the coverage rather than the hook.
Only the since-retired `claude-piped-gate-hook-test.sh` exercised this, because it stripped `PATH` (to simulate a missing Go toolchain) and so was the one suite reaching the system bash at all; `check-tools-test.sh` is now the only suite that stubs `PATH` this way.

What this costs when it goes unnoticed is a misattributed failure, not just a late one.
A fixture builder that keeps running after its setup broke turns one root failure into a cascade of downstream errors, and the last line in the log belongs to the *subject* rather than the fixture.
In Q703, `release-delta-test`'s fixture repository lost a commit object partway through `build_repo`; the suite ran seven more failing `git commit` calls and ended on `release-delta: 'HEAD' is not a commit-ish in this repo`, which reads as a defect in the report under test.
With `inherit_errexit` the same injected fault stops at the first `git` fatal and exits 128, git's own status, naming the fixture as the thing that broke.

The hole was in 12 of 59 `scripts/**/*-test.sh` suites when it was found, including [`check-v2-api-sync-test.sh`](../../scripts/go/check-v2-api-sync-test.sh), whose own flake (Q596) was undiagnosable for the same reason: a failure that is not distinguished from a legitimate verdict.
All 12 carry the shopt now.

The gates were left behind by that pass, and Q733 closed them: 154 more scripts, of which 75 had at least one substitution running an in-script function, the shape that actually swallows a failure.
The worst was [`go-lint.sh`](../../scripts/go/go-lint.sh), whose `scoped_module_dirs` feeds the lint scope.
With a fault injected into it the gate announced a full sweep, linted 1 module instead of 11, swallowed four `command not found` 127s and exited 0; with the shopt the same fault exits 127 at the first one.
Q670 is the same hole reached without an injected fault, and its fix addressed the scope computation rather than the swallowing.

Adding the shopt is a behaviour change, which is the point: it turns a silent pass into a failure.
It found one, in `windowserver_reports` in [`validate-throttle.sh`](../../scripts/agent/validate-throttle.sh), which counted with `[[ -e "$f" ]] && (( count++ ))`.
Post-increment evaluates to the *old* value, so the first match returns 1, and as the last command in an `&&` list that aborts the function.
The count was right only because errexit could not reach it.

## Shared helpers and Makefile wiring

Before writing a new helper function, check [`scripts/lib/common.sh`](../../scripts/lib/common.sh) — `require_cmd`, `workspace_modules`, `die_if_killed`, and the throttle setup already live there.

**A test suite that compares a captured exit status guards it with `die_if_killed` first.** A status above 128 is 128+n from a signal: the command was killed before it could answer, so comparing it against a wanted status blames the subject for the kill, and a contended `make check` reports `expected rc=1, got rc=143` as a defect in the gate under test (Q1023).
The guard exits with that same status instead, which is what lets [`run-parallel.sh`](../../scripts/ci/run-parallel.sh) file the suite under KILLED rather than FAILED, the distinction it already draws and explains.

```bash
out="$("$@" 2>&1)" || rc=$?
die_if_killed "$name" "$rc" "$want"
if ((rc == want)); then
```

Two things about it are load-bearing.
The split is `> 128`, not `>= 128`: git spends 128 on any fatal error, so 128 is not a signal death (Q837, where `run-parallel.sh` drew the same line).
And the third argument is the *wanted* status, which a suite deliberately asserting a kill needs: [`run-parallel-test.sh`](../../scripts/ci/run-parallel-test.sh) expects 137 and 143 from the runner's own KILLED path, and a want-blind guard would exit before those assertions ran.
Pass it whenever the wanted status is a variable; omit it for a literal, which can never be a signal.
A helper that discards the status outright (`|| true`) has the same defect in a quieter form: the command is dead before it prints, so the assertion reports a missing needle and blames the gate's wording.

**A suite whose own harness kills the subject gets no guard at all, and WANT cannot rescue it.** Where a stub bounds a loop with `kill -KILL "$PPID"`, the 128+n it produces is how a *regression* fails the case, and the wanted status is 0, so a guard would file a real failure as KILLED and the third argument has nothing to match.
[`progress-watch-test.sh`](../../scripts/e2e/progress-watch-test.sh) and [`release-sentinel-test.sh`](../../scripts/dogfood/release-sentinel-test.sh) are the two in the tree; both say so at the capture, because the absence is otherwise indistinguishable from a site the roll-out missed (Q1055).
Grep a suite for `kill -` before guarding it.

The root `Makefile` keeps recipes as thin target→script wiring so the logic stays shellcheck-covered; see [`scripts/README.md`](../../scripts/README.md) for the script inventory and parameter conventions.

**A new script goes in a `scripts/<group>/` directory, never at the top level.** The groups name the gate that consumes the script, which is what lets every CI path filter be a prefix glob rather than an enumeration; the map is in [`scripts/README.md`](../../scripts/README.md) and the rule is explained in [testing.md § `scripts/` is grouped by blast radius](testing.md#scripts-is-grouped-by-blast-radius).
Put a `*-test.sh` beside its subject.

**Commit an entry point executable, and a sourced `lib/` file not.** Docs and runbooks invoke these scripts bare, so a script committed `100644` exits 126 `Permission denied` before it reads a single env var.
Six dogfood entry points had accumulated that way, four of them in the five bring-up/teardown commands [release.md](../operations/release.md) prescribes (Q1013).
[`make script-modes-check`](../../scripts/ci/check-script-modes.sh) enforces both directions.
It reads the mode from the git index rather than the worktree, which is the reading the machine that introduced the defect cannot take: a local `chmod +x` that never reached the index runs for its author and is broken for every fresh clone.
Set the bit with `git update-index --chmod=+x <path>` when a `chmod` alone leaves the index behind.

## When not to write the script in shell

A script that sequences `kubectl`/`helm`/`gcloud` calls belongs here however long it gets.
A script that parses a structured format into fields and reasons over them wants a real parser and a test suite, and belongs in `devtools/` as a Go program with a thin `scripts/` entry point.
The criterion is parsing density, not length; it, its corroborating signals, and what the rewrite costs are in [technical-debt.md § A shell gate becomes a Go devtool on parsing density, not length](technical-debt.md#a-shell-gate-becomes-a-go-devtool-on-parsing-density-not-length).

**A Claude Code `PreToolUse` hook is not an exception**, however much its must-never-block contract makes it feel like one: a hook that scans the Bash command string is parsing shell grammar, and fail-open survives the move to Go via the build seam that section documents.
Read it before hand-rolling quote state, heredoc bodies, or command position in `case`.

## Accepted shellcheck findings

A finding that is accepted rather than fixed carries a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (example: the dynamic-name `read`/`export` in [`scripts/dev/probe-investigations-cd.sh`](../../scripts/dev/probe-investigations-cd.sh)).
Everything else is fixed to match the rules above.

### SC2329 on a `trap`-invoked cleanup function

shellcheck 0.11 reports `SC2329 (info): This function is never invoked` for a function reached only through `trap`, but **only when the script's final statement is an unconditional `exit`**.
The same script ending in an ordinary command is clean, and a conditional `exit` inside an `if` does not trigger it either — so most scripts here never see it, and the ones that do look identical to the ones that do not.

It is a false positive: the function *is* invoked, by the `EXIT` trap.
Keep the `exit` if the script needs a meaningful status, and disable the check where the function is defined:

```bash
# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "${work_dir}"
}
trap cleanup EXIT
```

Do not delete the trailing `exit` or restructure the cleanup to silence it — the exit status is the useful thing and the warning is the wrong one.
Live example: [`scripts/e2e/vap-param-informer-check.sh`](../../scripts/e2e/vap-param-informer-check.sh).
