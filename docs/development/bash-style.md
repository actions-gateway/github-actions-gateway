# Bash style

Conventions for every shell script in this repo — `scripts/`, `.githooks/`, and (where practical) inline `run:` blocks in `.github/workflows/`. Every tracked script under `scripts/` is linted against these by `make shellcheck`, which is part of `make check` (see [testing.md § The shellcheck gate](testing.md#the-shellcheck-gate)).

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

An unbounded loop can only be ended from outside, and that external kill is the problem: it gets issued as a `pkill -f <pattern>`, the pattern matches every parallel worktree, and it carries no record of whose run it is hitting ([testing.md § Stopping a run](testing.md#stopping-a-run-name-the-target-never-the-program)). Give the loop its own stop condition and the kill is never needed.

- A **bounded iteration count** when the work is countable: `for i in $(seq 1 40)`.
- A **stop-file the loop polls** when it is not: `while [[ ! -f "${stop_file}" ]]`. Touching the file ends the run through the normal exit path, so the cleanup `trap` still runs.

**Do not reach for `timeout`.** It ships with GNU coreutils and is absent from a stock macOS, where neither `timeout` nor `gtimeout` is on `PATH`. A script that depends on it runs unbounded on a dev Mac while looking correct. That is the same silent-absence trap that broke Q690's load harness, which backgrounded its generators with `setsid`: also absent on macOS, so all three died instantly and 40 samples passed against an idle machine, reading as strong evidence of no flake. `timeout` is also still a kill, just one on a timer, so the signal can skip the cleanup `trap` that a self-terminating loop unwinds through.

This applies hardest to throwaway harnesses under the gitignored `tmp/`. Being a throwaway is exactly why such a script gets none of the review a tracked one does, and it is where every unbounded loop in this repo's history has actually come from.

## `set -e` stops at a command substitution

`set -euo pipefail` does not cover `x="$(build_fixture)"`. Inside the substitution a failing command neither aborts nor propagates, and the assignment takes the status of the builder's *last* command, so a fixture that broke on its third step reports success. Measured on bash 5.3, with a builder whose first two steps fail:

| Form | Exit status | Steps run after the first failure |
|---|---|---|
| `repo="$(build)"` | 0 | 2 |
| `repo=$(build)` | 0 | 2 |
| `local repo; repo="$(build)"` | 0 | 2 |
| `repo="$(set -e; build)"` | 1 | 0 |
| `shopt -s inherit_errexit` then `repo="$(build)"` | 1 | 0 |
| `build` called directly, no substitution | 1 | 0 |

`shopt -s inherit_errexit` is the remedy to reach for: one line at the top of the file covers every substitution in it, where the other two working forms have to be repeated per call site.

What this costs when it goes unnoticed is a misattributed failure, not just a late one. A fixture builder that keeps running after its setup broke turns one root failure into a cascade of downstream errors, and the last line in the log belongs to the *subject* rather than the fixture. In Q703, `release-delta-test`'s fixture repository lost a commit object partway through `build_repo`; the suite ran seven more failing `git commit` calls and ended on `release-delta: 'HEAD' is not a commit-ish in this repo`, which reads as a defect in the report under test. With `inherit_errexit` the same injected fault stops at the first `git` fatal and exits 128, git's own status, naming the fixture as the thing that broke.

The hole was in 12 of 59 `scripts/**/*-test.sh` suites when it was found, including [`check-v2-api-sync-test.sh`](../../scripts/go/check-v2-api-sync-test.sh), whose own flake (Q596) was undiagnosable for the same reason: a failure that is not distinguished from a legitimate verdict. All 12 carry the shopt now.

## Shared helpers and Makefile wiring

Before writing a new helper function, check [`scripts/lib/common.sh`](../../scripts/lib/common.sh) — `require_cmd`, `workspace_modules`, and the throttle setup already live there. The root `Makefile` keeps recipes as thin target→script wiring so the logic stays shellcheck-covered; see [`scripts/README.md`](../../scripts/README.md) for the script inventory and parameter conventions.

**A new script goes in a `scripts/<group>/` directory, never at the top level.** The groups name the gate that consumes the script, which is what lets every CI path filter be a prefix glob rather than an enumeration; the map is in [`scripts/README.md`](../../scripts/README.md) and the rule is explained in [testing.md § `scripts/` is grouped by blast radius](testing.md#scripts-is-grouped-by-blast-radius). Put a `*-test.sh` beside its subject.

## When not to write the script in shell

A script that sequences `kubectl`/`helm`/`gcloud` calls belongs here however long it gets. A script that parses a structured format into fields and reasons over them wants a real parser and a test suite, and belongs in `devtools/` as a Go program with a thin `scripts/` entry point. The criterion is parsing density, not length; it, its corroborating signals, and what the rewrite costs are in [technical-debt.md § A shell gate becomes a Go devtool on parsing density, not length](technical-debt.md#a-shell-gate-becomes-a-go-devtool-on-parsing-density-not-length).

**A Claude Code `PreToolUse` hook is not an exception**, however much its must-never-block contract makes it feel like one: a hook that scans the Bash command string is parsing shell grammar, and fail-open survives the move to Go via the build seam that section documents. Read it before hand-rolling quote state, heredoc bodies, or command position in `case`.

## Accepted shellcheck findings

A finding that is accepted rather than fixed carries a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (example: the dynamic-name `read`/`export` in [`scripts/dev/probe-investigations-cd.sh`](../../scripts/dev/probe-investigations-cd.sh)). Everything else is fixed to match the rules above.

### SC2329 on a `trap`-invoked cleanup function

shellcheck 0.11 reports `SC2329 (info): This function is never invoked` for a function reached only through `trap`, but **only when the script's final statement is an unconditional `exit`**. The same script ending in an ordinary command is clean, and a conditional `exit` inside an `if` does not trigger it either — so most scripts here never see it, and the ones that do look identical to the ones that do not.

It is a false positive: the function *is* invoked, by the `EXIT` trap. Keep the `exit` if the script needs a meaningful status, and disable the check where the function is defined:

```bash
# shellcheck disable=SC2329 # invoked by `trap cleanup EXIT`; shellcheck 0.11 misses
# that whenever the script ends in an explicit `exit`.
cleanup() {
	rm -rf "${work_dir}"
}
trap cleanup EXIT
```

Do not delete the trailing `exit` or restructure the cleanup to silence it — the exit status is the useful thing and the warning is the wrong one. Live example: [`scripts/e2e/vap-param-informer-check.sh`](../../scripts/e2e/vap-param-informer-check.sh).
