# Q820 — `git-merge-plan-index-test` dies on a temp-file error under the parallel scripts-test runner

[`scripts/docs/git-merge-plan-index-test.sh`](../../scripts/docs/git-merge-plan-index-test.sh) builds throwaway git repos under a `mktemp -d` and runs the [`docs/plan/README.md` merge driver](../../scripts/docs/git-merge-plan-index.sh) against them.
Under [`make scripts-test`](../../scripts/ci/run-parallel.sh), which launches every `scripts/**/*-test.sh` suite at once, it occasionally dies on a git temp-file error and passes on rerun with no code change.

**Status:** watching.
The failure *family* is reproduced (a `rm -rf` racing a live git in the same repository gives the exact signature), but **no candidate remover exists** in the suite or in the fan-out around it, so the mechanism is not established.
This file exists so a fourth sighting starts from what has already been excluded rather than re-deriving it.
Nothing has been changed in the suite.
A speculative fix was deliberately not written, because every fix that follows from the reproduction would target a removal nothing has been shown to perform.

## The signature

Identical in all three sightings:

```
[git-merge-plan-index-test] error: unable to create temporary file: No such file or directory
[git-merge-plan-index-test] error: docs/plan/README.md: failed to insert into database
[git-merge-plan-index-test] error: unable to index file 'docs/plan/README.md'
[git-merge-plan-index-test] fatal: updating files failed
[run-parallel] FAILED: git-merge-plan-index-test (exit 128)
```

## Sightings

| # | Date | Where | Notes |
|---|---|---|---|
| 1 | 2026-08-12 | local, under `make check` | Host temp pressure suspected at the time |
| 2 | 2026-08-14 | CI, PR #1511 | Ephemeral runner |
| 3 | 2026-08-15 | CI, PR #1534, run [31884780048](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31884780048) | Ephemeral runner; green on rerun of the same commit, no code change |

Two different ephemeral runners on two different pull requests, plus one local run, is what rules out the host temp pressure sighting 1 suspected: an ephemeral runner starts clean.
Sighting 3's job ran on GitHub-hosted `ubuntu-latest` (`runner_group_name: GitHub Actions`), not on a self-hosted dogfood runner.

**Reading sighting 3 costs a step-level look.** The failing check is named `shellcheck` and shellcheck itself passed: the job runs several steps, and the one that failed is `scripts behavioural tests (make scripts-test)`. `unit-test-gate` also reported failure because it aggregates the gating jobs' results.
One root cause, two red checks, and neither check name points at the suite.
Read the step conclusions:

```bash
gh api repos/actions-gateway/github-actions-gateway/actions/jobs/95012397955 --jq '.steps[] | "\(.number) \(.conclusion) \(.name)"'
```

## What is ruled out

Measured 2026-08-15 on git 2.55.0 and bash 5.3.15 (macOS arm64).
The probe scripts are not committed; each is a few lines and is described below well enough to rewrite.

### A trap firing on a subshell exit is not the remover

The suspicion was that the suite's `trap 'rm -rf "$WORKDIR"' EXIT` fires when one of its `$( )` helpers or the `(cd "$repo" && … --install)` subshell exits, removing a tree a later case still uses.

**It cannot.** Bash resets traps in subshells.
A script setting that trap, then running a `$( )` command substitution and a failing `( exit 3 )`, still has its `WORKDIR` present after both, and the trap fires exactly once, at script exit.
The same reasoning kills the variant where a `merge_repo` failure takes the tree with it.

This also holds for the near-identical siblings [`git-merge-status-test.sh`](../../scripts/docs/git-merge-status-test.sh) and [`git-merge-roadmap-test.sh`](../../scripts/docs/git-merge-roadmap-test.sh), which share the shape.

### The tree was not already gone when the git command started

The Queue row's original wording, "the repo gone mid-merge", is wrong as literally written.
Every removal staged **before** a git command fails earlier, and differently:

| What was removed first | What git says |
|---|---|
| `.git/objects` | `fatal: not a git repository (or any of the parent directories): .git` |
| `.git/objects`, with `GIT_DIR` pinned so git cannot search upward | `fatal: not a git repository: '<path>/.git'` |
| the whole repo directory | `fatal: cannot change to '<path>': No such file or directory` |
| `.git/objects`, then `git merge` rather than `git add` | `fatal: not a git repository (or any of the parent directories): .git` |

None is the signature.
The signature comes from git's loose-object write path, which is only reached **after** git has validated the repository and read the worktree file.
Whatever goes wrong therefore lands inside a live git process's window, not before it.

**Side finding worth keeping:** git treats a repository whose `.git/objects` is missing as invalid and **searches upward** for an enclosing one.
A first version of this probe put its throwaway repos inside the worktree and silently operated on the real repository instead: `git -C "$broken_repo" add` reported that `tmp/q820` is gitignored.
Any probe of this class belongs in a `mktemp -d` outside the repo tree.

### No cross-suite path collision is visible in the source

- Every suite in the fan-out that needs scratch space uses `mktemp -d`, which is atomic and unique.
- No suite's `rm -rf` targets anything outside its own scratch tree (`grep -rn 'rm -rf' scripts --include='*-test.sh'`).
- No script under `scripts/` sets `TMPDIR` or `HOME`.
- [`merge-driver-common.sh`](../../scripts/lib/merge-driver-common.sh) gives each driver invocation its own `mktemp -d "${TMPDIR:-/tmp}/${DRIVER_NAME}-merge.XXXXXX"`, so two drivers running at once cannot share one.

## What is confirmed: the failure family

A removal racing a **live** git process in the same repository reproduces the signature exactly.
Racing `rm -rf "$repo/.git"` against a `git add -A` over 1,500 files, at delays stepped between 10 ms and 55 ms, hit it on 2 of 60 attempts:

```
error: unable to create temporary file: No such file or directory
error: docs/plan/f0.md: failed to insert into database
error: unable to index file 'docs/plan/f0.md'
```

The window is narrow, which fits a flake seen three times in four days rather than one seen every run.

Two qualifications matter for the next occurrence.

**What is removed decides the message.** Racing `rm -rf "$repo"`, the whole tree, gives `fatal: unable to stat 'docs/plan/f1097.md'` instead, because `rm` reaches the worktree files before git finishes writing objects.
The racing `rm` itself then errors `Directory not empty`, because git keeps creating object fanout directories under it.
Only a removal that takes `.git` first lands on the signature.

**The last line names the failing command, and it is `git commit -a`.** A plain `git add -A` ends `fatal: adding files failed`, not the `fatal: updating files failed` all three sightings carry.
Racing the same removal against each candidate separately settles it: `git commit -qam` reproduces the full four-line signature, last line included, on 1 of 40 attempts, while `git checkout -q -b` and `git merge` each reproduced nothing in 40.
Treat those two zeros as weak on their own, since the positive case only landed once in 40; the last line is the stronger discriminator.

That narrows the failing call in the suite to one of its six `commit -qam` invocations: the two in `run_merge`, the two staging the rebase case, and the two staging the subdirectory case.
The `git add` and `commit -qm` pairs elsewhere in the suite, including the gate-agreement setup, would have failed with a different last line.

### But no candidate remover exists

This is where it stops.
The family is confirmed, and nothing in the suite or the fan-out can supply the concurrent `rm -rf`:

- The suite's merges are strictly sequential, so no git is still running when the next `plain_repo` call removes a tree.
- `plain_repo`'s `rm -rf "$repo"` only collides with an earlier repo when `$RANDOM` repeats (`merge_repo "resolved-$RANDOM"`), and even then the earlier repo's git has already exited.
- The suite's own `trap 'rm -rf "$WORKDIR"' EXIT` runs after the last command, and cannot fire early, per the subshell finding above.
- A `SIGTERM` from [`run-parallel.sh`](../../scripts/ci/run-parallel.sh)'s `cleanup` would fit the shape, since the EXIT trap would remove the tree while a git child kept running, but that path exits **143** and all three sightings report **128**.

## Reproducing locally

14 consecutive `make scripts-test` runs on an 18-core machine, at load averages of 40 to 55, did not hit it.
The sightings were on GitHub-hosted `ubuntu-latest`, which is much smaller, so the local fan-out may simply not be contended enough to be the right load.

**Do not edit tracked docs while the loop runs.** The suite's gate-agreement case copies the *live working tree* (`cp -R "$REPO_ROOT/docs/plan/." …` and `cp "$REPO_ROOT/docs/STATUS.md" …`), so a plan file added without its [`docs/plan/README.md`](README.md) row yet fails the case's own baseline with `FAIL gate agreement: base=1 …` and exit **1**.
That is a self-inflicted transient rather than this flake, and the exit status tells them apart: Q820 exits **128**.

## What a fourth sighting should capture

1. **The step-level conclusions**, per the `gh api` call above.
   The check name will again say `shellcheck`.
2. **Which of the six `commit -qam` calls failed.** The last line narrows it to that set but not to one call.
   Running the suite under `set -x` with a `PS4` carrying `$LINENO` would turn the next occurrence into a pointer at one line, and that single fact is what the exclusion list above is missing.
3. **Whether the racing `rm` also errored.** A `rm: … Directory not empty` line anywhere in the suite's output is the remover announcing itself, since that is what a `rm -rf` racing a live git produces.
   None of the three sightings has one, which is the main reason the concurrent-removal family, though reproducible, still has no owner.
4. **Whether a sibling suite failed in the same run.** [Q826](../STATUS.md#Q826) is a sibling flake in [`git-merge-gate-lists-test.sh`](../../scripts/ci/git-merge-gate-lists-test.sh) with a different signature, and [Q822](../STATUS.md#Q822) tracks unrelated suites failing under concurrent load.
   A shared window would reframe all three as one contention problem rather than three defects.
