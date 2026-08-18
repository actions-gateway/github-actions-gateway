# Q820 — `git-merge-plan-index-test` dies on a temp-file error under the parallel scripts-test runner

[`scripts/docs/git-merge-plan-index-test.sh`](../../scripts/docs/git-merge-plan-index-test.sh) builds throwaway git repos under a `mktemp -d` and runs the [`docs/plan/README.md` merge driver](../../scripts/docs/git-merge-plan-index.sh) against them.
Under [`make scripts-test`](../../scripts/ci/run-parallel.sh), which launches every `scripts/**/*-test.sh` suite at once, it occasionally died on a git temp-file error and passed on rerun with no code change.

**Status: cause named and fixed 2026-08-18.** The remover was git itself.
Every commit in a fixture repo spawned a **detached** `git maintenance run --auto`, and about nine per suite run went on to repack and prune, removing each object fanout directory they emptied.
Detached, that gc outlives the command that spawned it and runs while the *next* command writes to the same repo.
When it removed a fanout directory in the window between git's `mkdir` and the `open` beneath it, the write failed with `unable to create temporary file` and exit 128.

Everything below was measured on macOS 15.5 (Darwin 25.5.0, arm64, 18 cores), git 2.55.0, bash 5.3.15, unless it names CI.

## The signature

Identical in sightings 1 to 3, and reproduced locally:

```
[git-merge-plan-index-test] error: unable to create temporary file: No such file or directory
[git-merge-plan-index-test] error: docs/plan/README.md: failed to insert into database
[git-merge-plan-index-test] error: unable to index file 'docs/plan/README.md'
[git-merge-plan-index-test] fatal: updating files failed
[run-parallel] FAILED: git-merge-plan-index-test (exit 128)
```

A shorter variant loses the middle two lines and names the commit object instead of a blob:

```
error: unable to create temporary file: Invalid argument
fatal: failed to write commit object
```

Both are the same fault; the difference is only which object the prune happened to race.

## The mechanism

Taken with a `DYLD_INSERT_LIBRARIES` interposer on `open`, `mkdir`, `rmdir` and `unlink`, writing to a file so a detached process is visible.
Two pids, one repository, interleaved:

```
pid=76221 unlink .git/objects/ca/657e4a4f73bf76…      gc prunes a packed loose object
pid=76221 rmdir  .git/objects/ca                       and removes the fanout it emptied
pid=76343 mkdir  .git/objects/cc            rc=0       the committing git creates its fanout
pid=76221 rmdir  .git/objects/cc                       gc removes it
pid=76343 open   .git/objects/cc/tmp_obj_…  ENOENT     fatal
```

pid 76221 is a garbage collection: it writes `pack-*.mtimes`, `.bitmap` and `.promisor`, then unlinks the loose objects it packed and rmdirs each emptied fanout directory. git's ordinary path for a new fanout directory is `open` failing `ENOENT`, then `mkdir`, then a second `open`; four such sequences completed in pid 76343 moments earlier.
The fifth lost the race.

At the fatal `open` the interposer also recorded the working directory valid, `.git` present and `.git/objects` present, which is why nothing about the repository looked wrong.

**Counts, from one clean suite run traced end to end:** 64 processes spawned as `git maintenance run --auto --quiet --detach`, of which 9 reached `git repack -d -l --cruft --cruft-expiration=2.weeks.ago --quiet --write-midx`.
So the gc is not rare; only the collision is.

**Why the fan-out matters.** The window is the microseconds between one process's `mkdir` and its own next `open`.
Contention lengthens both the gc and the descheduled commit, which is why `make check` reproduces it and a quiet `make scripts-test` does not.

## The fix

`plain_repo` and the gate-agreement repo now set `maintenance.auto false` on every fixture repo, which stops the spawn at source (measured: `gc.auto=0` also works; `maintenance.auto` is the knob that names the intent).
A fixture repo has nothing to maintain, so the fix is to not start it.

Verification:

| | Before | After |
|---|---|---|
| `git maintenance run --auto … --detach` spawned per suite run | 64 | 0 |
| `git repack … --cruft` spawned per suite run | 9 | 0 |

The suite carries an assertion for it, written against behaviour rather than the config key that currently delivers it: a fixture commit under `GIT_TRACE=1` must spawn nothing matching `maintenance run`.
Proved able to fail by deleting the mechanism: with the `no_auto_maintenance` call removed from `plain_repo`, it reports `FAIL a fixture commit spawned background maintenance: git maintenance run --auto --quiet --detach` and the suite exits 1.

## Sightings

| # | Date | Where | Notes |
|---|---|---|---|
| 1 | 2026-08-12 | local, under `make check` | Host temp pressure suspected at the time |
| 2 | 2026-08-14 | CI, PR #1511 | Ephemeral runner |
| 3 | 2026-08-15 | CI, PR #1534, run [31884780048](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31884780048) | Ephemeral runner; green on rerun of the same commit |
| 4 | 2026-08-15 | local, under `make check` | First sighting with the tree read. Every repository intact; `EINVAL`; commit object |
| 5 | 2026-08-18 | local, reproduction harness | `EINVAL`, commit object, every repository intact |
| 6 | 2026-08-18 | local, harness | `ENOENT`, blob: the CI signature, on macOS |
| 7 | 2026-08-18 | local, harness, instrumented | The syscall sequence above, naming the gc |

**Reading sighting 3 costs a step-level look.** The failing check is named `shellcheck` and shellcheck itself passed: the one that failed is the `scripts behavioural tests (make scripts-test)` step.
`unit-test-gate` also reported failure because it aggregates the gating jobs' results.
Read the step conclusions:

```bash
gh api repos/actions-gateway/github-actions-gateway/actions/jobs/95012397955 --jq '.steps[] | "\(.number) \(.conclusion) \(.name)"'
```

## Where the earlier rounds went wrong

Three readings in this file pointed away from the answer, and each is worth keeping.

**The errno split was read as possibly two bugs.** Sighting 4's `EINVAL` on a commit object against sightings 1 to 3's `ENOENT` on a blob looked like it might split by platform.
Both variants reproduced on one macOS host, so it never did: it is one fault, and the errno only records which object lost the race.

**"Every repository intact" was read as excluding a removal.** It does exclude the family this file spent three rounds on, where something removes the repo or its `.git`.
It does not exclude a remover that only takes *empty fanout directories*, which is what a prune does and what leaves every repository intact by construction.
The reading was correct; the inference from it was too broad.

**The remover was looked for outside git.** Every candidate considered was a shell `rm -rf`: the suite's EXIT trap, `plain_repo`'s, a sibling suite's, the runner's `SIGTERM` path.
The actual remover is a `git` process that the suite never invokes and that no line of the suite mentions, spawned by `git commit` itself.
A trace that only watched shell removals would never have found it.

**And the instrument that found it nearly reported a false negative twice.** `/usr/bin/env` is SIP-`restricted`, so `#!/usr/bin/env bash` clears every `DYLD_*` variable before bash starts and the whole process tree runs uninstrumented; the suite has to be invoked as `"$(command -v bash)" <script>` instead.
And a detached child has no stderr, so traces must go to a file or the one process that mattered writes into the void.
Both were caught only by a load proof, a constructor that prints one line per process, because without it "no anomalies logged" and "the library never loaded" are the same output.

## What stayed ruled out

- **A trap firing on a subshell exit is not the remover.** Bash resets traps in subshells, so `trap 'rm -rf "$WORKDIR"' EXIT` cannot fire when a `$( )` helper or the `(cd "$repo" && … --install)` subshell exits.
- **The tree was not already gone when the git command started.** Every removal staged *before* a git command fails earlier and differently (`fatal: not a git repository`, `fatal: cannot change to …`), never with this signature, which is only reached after git has validated the repository.
- **No cross-suite path collision.** Every scratch root under `scripts/` is a `mktemp -d` or a path under the gitignored `tmp/`; no script sets `TMPDIR` or `HOME`; 100 consecutive suite runs recorded 100 distinct workdirs.
- **The host filesystem does not fail under this churn.** A git-free probe doing the same create-and-delete churn (16 processes, 153,600 file creations) had zero failures.
  Its question is narrower than "the filesystem never fails", since it did not reproduce git's renames and fsyncs, but it kills the churn theory.
- **`SIGTERM` from the runner.** That path exits 143, and every Q820 sighting is 128.

**Side finding worth keeping:** git treats a repository whose `.git/objects` is missing as invalid and searches *upward* for an enclosing one, so a probe of this class placed inside the worktree silently operates on the real repository.
Put throwaway repos under `mktemp -d`.

## Neighbouring defects, not this one

**A merge failure is reported as a conflict, with the message discarded.** `run_merge` runs `git merge … >/dev/null 2>&1`, so any non-conflict failure reaches the assertion as `merge reported a conflict (rc=…)` with nothing to read.
Two `main` runs on 2026-08-18 (32131116724, 32131423171) failed that way at `rc=128` with no diagnosable output.
Measured: an ordinary conflict exits **1** and a merge whose object write fails exits **2**, so `rc=128` is neither, and this suite cannot currently say what it was.
Tracked separately; this fix does not touch it, and the abort path it fixes exits 128 from the *suite*, which is a different thing from the inner merge's 128.

**Two exit statuses are noise once the machine is oversubscribed.** 137 is the OOM killer and 143 a `SIGTERM` from outside; both appeared in the reproduction harness at high worker counts and neither is this flake.
Q837 (shipped) labels them `KILLED` rather than as a gate failure and pins 128 as `FAILED`, so this signature is unchanged by it.

## Reproducing, if it recurs

Run the suite itself 16 to 32 wide in a loop, each copy redirecting to its own log, keeping only the logs of runs that exited non-zero, with a `make check` alongside it for contention.
That reproduced it at roughly 1 run in 100 to 300; the harness is a dozen lines and was not committed.
Watch the syscalls with an interposer on `open`/`mkdir`/`rmdir`/`unlink`, remembering both instrumentation traps above.

**Do not edit tracked docs while the loop runs.** The gate-agreement case copies the live `docs/plan/` and `docs/queue/` trees, so a plan file added without its [`docs/plan/README.md`](README.md) row fails that case's baseline with exit **1**, which is self-inflicted rather than this flake.

## If it recurs anyway

The fix removes the only concurrent writer found in these repositories, so a recurrence means a different one.
Capture, in order: the failing call from the `ERR` trap, the `Q820:` tree reading, the errno and which object, and whether a sibling suite failed in the same run.
[Q826](../queue/Q826.md) is a sibling flake in [`git-merge-gate-lists-test.sh`](../../scripts/ci/git-merge-gate-lists-test.sh) with a different signature, and [Q822](../queue/Q822.md) tracks unrelated suites failing under concurrent load.

**This mechanism does not reach Q826**, measured 2026-08-18 while sweeping the fix across the tier.
That fixture holds about six objects against `gc.auto`'s default 6700, so its three detached maintenance runs return without repacking anything, and there is no prune to race — the trace shows the `maintenance run` line followed straight by the next suite command, where this suite's shows `git repack -d -l --cruft` under it.
A merge driver exiting non-zero reproduces Q826's line verbatim, and [`merge-driver-common.sh`](../../scripts/lib/merge-driver-common.sh)'s `ERR` trap turns any internal failure into a fallback conflict, so the driver's own exit is where to look.

The same fixture-repo defect existed wherever a suite commits in a throwaway repo; Q878 swept it, and every fixture repo in the tree now sets the key ([the rule](../development/testing.md#a-fixture-repo-must-not-run-background-git)).
