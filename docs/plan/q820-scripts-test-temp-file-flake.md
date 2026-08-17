# Q820 — `git-merge-plan-index-test` dies on a temp-file error under the parallel scripts-test runner

[`scripts/docs/git-merge-plan-index-test.sh`](../../scripts/docs/git-merge-plan-index-test.sh) builds throwaway git repos under a `mktemp -d` and runs the [`docs/plan/README.md` merge driver](../../scripts/docs/git-merge-plan-index.sh) against them.
Under [`make scripts-test`](../../scripts/ci/run-parallel.sh), which launches every `scripts/**/*-test.sh` suite at once, it occasionally dies on a git temp-file error and passes on rerun with no code change.

**Status:** watching, and pointed somewhere new.
Sighting 4 caught the suite failing with every throwaway repository still intact, so **the concurrent-removal family this file spent three rounds on is not what is happening**.
The family remains reproducible on demand and still produces the signature, which is what made it convincing; it is a coincidental match.
No fix has been written, because nothing here yet names a cause.
The suite carries instrumentation only: an `ERR` trap that names the failing line and reads the trees at the moment of failure.

Three things changed on 2026-08-15.
Sighting 4 took the reading above, on the trap's first run after it was added.
The candidate set of failing calls was **six, and is eight**: the last line discriminates on what was staged rather than on which command staged it, so the gate-agreement case's two `git add -A` calls belong in the set that had excluded them.
And the absence of a `rm` error from the sightings' output, previously read as the main argument against the family, turns out not to exclude it, which no longer matters much now that the reading does.

## The signature

Identical in sightings 1 to 3.
Sighting 4 shares the first line and the exit status but differs below that, as its own section sets out:

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
| 4 | 2026-08-15 | local, under `make check` | The first sighting taken with the tree read. Every repository intact; different errno; died on the commit object rather than a blob |

Two different ephemeral runners on two different pull requests, plus two local runs, is what rules out the host temp pressure sighting 1 suspected: an ephemeral runner starts clean.
Sighting 3's job ran on GitHub-hosted `ubuntu-latest` (`runner_group_name: GitHub Actions`), not on a self-hosted dogfood runner.

**Reading sighting 3 costs a step-level look.** The failing check is named `shellcheck` and shellcheck itself passed: the job runs several steps, and the one that failed is `scripts behavioural tests (make scripts-test)`.
`unit-test-gate` also reported failure because it aggregates the gating jobs' results.
One root cause, two red checks, and neither check name points at the suite.
Read the step conclusions:

```bash
gh api repos/actions-gateway/github-actions-gateway/actions/jobs/95012397955 --jq '.steps[] | "\(.number) \(.conclusion) \(.name)"'
```

## Sighting 4: nothing was removed

The `ERR` trap's tree reading landed on its first run after being added, under `make check` on an 18-core machine:

```
error: unable to create temporary file: Invalid argument
fatal: failed to write commit object
git-merge-plan-index-test.sh:158: FAILED (rc=128): git -C "$repo" "${GIT_ID[@]}" commit -qam theirs --allow-empty
Q820: WORKDIR=/var/folders/cs/.../T/tmp.pzy5nrmARw present=yes
Q820:   /var/folders/cs/.../T/tmp.pzy5nrmARw/resolved-21074 .git=yes objects=yes
Q820:   /var/folders/cs/.../T/tmp.pzy5nrmARw/resolved-23703 .git=yes objects=yes
```

`objects=yes` on every repository, taken at the moment of failure.
**Nothing had been removed**, which is the one thing the concurrent-removal family requires.
The suite passed on rerun with no code change, and the two repositories present are the count the second `expect_resolved` case would have built, so the run was where the line number says it was.

Read this as evidence against the family rather than as its refutation, because two things differ from the three CI sightings:

| | Sightings 1 to 3 | Sighting 4 |
|---|---|---|
| errno | `No such file or directory` (ENOENT) | `Invalid argument` (EINVAL) |
| Object being written | a blob (`failed to insert into database`, `unable to index file`) | the commit object (`failed to write commit object`) |
| Lines | four | two |

What they share is the whole rest of the shape: this suite, `unable to create temporary file` as the first line, exit **128**, a `commit -qam` as the failing call, and the parallel fan-out.

The reading that matters is that git failed to create a temporary file in an object directory that **was still there**.
That is not a removal, so it is not the family, and no mechanism yet proposed in this file explains it.
A transient failure of temporary-file creation under fan-out load fits every sighting and assumes no remover; it is a shape, not yet a cause, and the errno split is the part it does not explain.

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
- No suite's `rm -rf` targets anything outside its own scratch tree.
  Re-measured 2026-08-15 over all of `scripts/`, not just `*-test.sh` as the first pass did: every scratch root in the tree is either a `mktemp -d` or a path under the repository's gitignored `tmp/`, and the handful of fixed in-repo ones (`check-page-density-test.sh`, `check-comparison-stamps-test.sh`, `check-script-docs-test.sh`) cannot reach the host temp tree this suite's `WORKDIR` lives in.
  Nothing under `scripts/` removes anything in the host temp tree outside its own `mktemp -d`.
- No script under `scripts/` sets `TMPDIR` or `HOME`.
- [`merge-driver-common.sh`](../../scripts/lib/merge-driver-common.sh) gives each driver invocation its own `mktemp -d "${TMPDIR:-/tmp}/${DRIVER_NAME}-merge.XXXXXX"`, so two drivers running at once cannot share one.

## The failure family, kept as a look-alike

**Superseded by sighting 4**, which failed with every repository intact.
This section is kept because the family is real, reproducible on demand, and produces the signature, which is exactly what makes it a trap for the next reader: a reproduction that matches the symptom is not a diagnosis.
Everything below still holds on its own terms; none of it is evidence about this flake.

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
The racing `rm` may also error `Directory not empty`, when git creates an object fanout directory under one `rm` has already passed; a traversal that outlasts git's last write takes the tree silently instead, which is why that line's absence excludes nothing.
Only a removal that takes `.git` first lands on the signature.

**The last line names what was staged, not which command staged it.** An earlier reading of this section attributed `fatal: updating files failed` to `git commit -a` specifically, and narrowed the failing call to the suite's six `commit -qam` invocations.
That is wrong, and it wrongly excluded two more.

The discriminator can be measured without a race at all, which is what settles it.
`chmod 500` on a repository's `.git/objects` makes loose-object *writes* fail while leaving reads intact, so every candidate command reaches the same failure point deterministically.
The first line then reports `insufficient permission` (EACCES) rather than the sightings' ENOENT, but the last line and the exit status come from the same code path, and those are the parts under test.
Measured 2026-08-15 on git 2.55.0 (macOS arm64):

| Command | Staged content | Last line | rc |
|---|---|---|---|
| `git commit -qam` | modified tracked file | `fatal: updating files failed` | 128 |
| `git add -A` | modified tracked file | `fatal: updating files failed` | 128 |
| `git add <path>` | modified tracked file | `fatal: updating files failed` | 128 |
| `git add -A` | modified tracked + new untracked | `fatal: updating files failed` | 128 |
| `git add -A` | new untracked only | `fatal: adding files failed` | 128 |
| `git checkout -q -b` | nothing | (succeeds) | 0 |
| `git merge` | contested file | `Merge with strategy ort failed.` | 2 |

So `fatal: updating files failed` is emitted by any command that stages a **modified tracked** file, `git add` included.
`fatal: adding files failed` appears only when everything staged is untracked, which is why the earlier measurement of a "plain `git add -A`" read as a clean discriminator: it happened to stage new files.
When both kinds are staged, the modified tracked file is processed first and decides the message.

Two exclusions get stronger in exchange.
`git merge` produces a different signature entirely and exits 2, not 128, and `git checkout -q -b` writes no objects on this path at all, so neither can ever be the failing call.
Both previously rested on a zero out of 40 attempts, which the reproduction rate made weak evidence.

The corrected candidate set is **eight** calls, all of which stage a modified tracked `docs/plan/README.md`:

- the two `commit -qam` in `run_merge`
- the two `commit -qam` staging the rebase case
- the two `commit -qam` staging the subdirectory case
- **the two `git add -A` in the gate-agreement case**, which `file_plan` reaches after it rewrites the tracked `README.md`

The gate-agreement pair is the correction that matters, because the earlier reading named that case explicitly as one that "would have failed with a different last line".
It is the only candidate in a repository built from the real `docs/plan/` tree rather than from fixtures.
Its `add -A` still writes only the two objects `file_plan` dirties, though, so it carries no wider a race window than the six commits; the suite's one large write (156 files, at the `add -A` that first populates that repository) stages untracked content only and is excluded by the last line.

### But no candidate remover exists

This is where it stops.
The family is confirmed, and nothing in the suite or the fan-out can supply the concurrent `rm -rf`:

- The suite's merges are strictly sequential, so no git is still running when the next `plain_repo` call removes a tree.
- `plain_repo`'s `rm -rf "$repo"` only collides with an earlier repo when `$RANDOM` repeats (`merge_repo "resolved-$RANDOM"`), and even then the earlier repo's git has already exited.
- The suite's own `trap 'rm -rf "$WORKDIR"' EXIT` runs after the last command, and cannot fire early, per the subshell finding above.
- A `SIGTERM` from [`run-parallel.sh`](../../scripts/ci/run-parallel.sh)'s `cleanup` would fit the shape, since the EXIT trap would remove the tree while a git child kept running, but that path exits **143** and all three sightings report **128**.

## Reproducing locally

**Run `make check`, not `make scripts-test`.** Sighting 4 came from a single `make check` on an 18-core machine, where 14 consecutive `make scripts-test` runs on the same machine had not hit it.
`make check` runs the scripts fan-out alongside the Go phases rather than alone, so the contention is far higher than the suite's own fan-out produces, and the earlier reading that a local machine may not be contended enough was measuring the wrong command.
One run is not a rate, but it is the first local reproduction, and it makes this cheaper to chase locally than to wait on CI for.

**Do not edit tracked docs while the loop runs.** The suite's gate-agreement case copies the *live working tree* (`cp -R "$REPO_ROOT/docs/plan/." …` and `cp "$REPO_ROOT/docs/STATUS.md" …`), so a plan file added without its [`docs/plan/README.md`](README.md) row yet fails the case's own baseline with `FAIL gate agreement: base=1 …` and exit **1**.
That is a self-inflicted transient rather than this flake, and the exit status tells them apart: Q820 exits **128**.

## What the next sighting should capture

1. **The step-level conclusions**, per the `gh api` call above.
   The check name will again say `shellcheck`.
2. **Which of the eight staging calls failed.** The signature's last line narrows it to that set but not to one call, so the suite now answers it itself.
   An `ERR` trap prints `git-merge-plan-index-test.sh:<line>: FAILED (rc=128): <command>` just above the git errors, and `set -o errtrace` is what reaches the two commits inside `run_merge`; the rest are at top level, where the trap fires without it.
   Measured by injecting a failing `git rev-parse` into `run_merge`: the trap named the injected line and reproduced the rc=128 the sightings carry, and a clean run prints nothing.
3. **The `Q820:` tree reading**, which sighting 4 answered as `objects=yes` and which every later sighting should be compared against.
   A second `objects=yes` on a Linux runner would carry the finding across to the ENOENT variant, which is the gap sighting 4 leaves open; an `objects=no` would mean the two errnos are two different faults and the family is back for one of them.
4. **The errno and which object failed.** Sighting 4 is EINVAL on a commit object; sightings 1 to 3 are ENOENT on a blob.
   Whether that splits by platform (macOS against `ubuntu-latest`) or varies within one is the cheapest question outstanding, and it decides whether this is one bug or two.
5. **Whether a sibling suite failed in the same run.** [Q826](../queue/Q826.md) is a sibling flake in [`git-merge-gate-lists-test.sh`](../../scripts/ci/git-merge-gate-lists-test.sh) with a different signature, and [Q822](../queue/Q822.md) tracks unrelated suites failing under concurrent load.
   A shared window would reframe all three as one contention problem rather than three defects.
