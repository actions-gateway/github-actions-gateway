# Parallel-dispatch playbook

How to clear a batch of backlog items quickly by running several agent sessions
in parallel — one session and pull request (PR) per task — coordinated by a
single **dispatcher** session that merges each PR after its checks pass.

This playbook captures a process that worked end-to-end for a release milestone
(ten `1.0-gate` items merged in one sitting) so it can be repeated for later
milestones. It is deliberately opinionated: the defaults here are the ones that
removed the friction we hit the first time.

## When to use it

Reach for parallel dispatch when **all** of these hold:

- You have a batch of **independent, well-scoped** backlog items (roughly S–M
  size) that can each become one focused PR.
- The work is mostly **mechanical or well-understood** (lint gates, CI wiring,
  packaging, docs, contained fixes) rather than open-ended design.
- You want **throughput** and are willing to keep a dispatcher session attending
  to merges.

Do **not** use it for a single large feature, for exploratory design work, or
for tightly coupled changes that all touch the same core files — those serialize
anyway and the coordination overhead is not worth it.

## Roles

**Dispatcher** (one session — typically the one you are in):
- Selects the batch, decides priority and ordering, groups by file contention.
- Spawns one worker session per task.
- Watches each PR, verifies its **scope** (checks-green is necessary, not
  sufficient), merges it, and advances the next task.
- Owns the high-contention coordination files (see
  [Dispatcher owns coordination files](#dispatcher-owns-coordination-files)).
- Is the single place to **stop or amend** the run.

**Worker** (one session per task, each in its own worktree):
- Implements exactly one task, runs the local gate, opens a PR.
- **Self-heals** until its PR is green and mergeable (see
  [the worker contract](#the-worker-contract-self-healing)).
- Never merges its own PR; never touches another session's branch or files
  outside its worktree.

## Spawn mechanism: task chips

Spawn each worker as a **task chip** (a real, separate session that starts in its
own fresh worktree on a `claude/*` branch when started). This is the mechanism to
use. Reasons:

- It runs under the normal permission gates — no blanket permission bypass.
- Each session is isolated (own worktree, own context, visible in the session
  list).

Do **not** try to auto-start headless worker sessions with a "skip all
permissions" flag. The safety classifier blocks it, and it is the less-secure
path regardless. The small cost of chips — one click to start each — is the
correct trade.

> One decision to settle **before** spawning: the dispatcher cannot send messages
> to a worker session in unsupervised mode, so it cannot nudge a stopped worker
> to fix its own PR. Design for the worker to finish the job itself (next
> section) rather than relying on re-engagement.

## The worker contract (self-healing)

Bake this into **every** worker prompt from the first task. It is the single
biggest reducer of dispatcher toil — added late the first time, it should be the
default.

After opening its PR, a worker does **not** stop. It loops until the PR is green
**and** mergeable:

1. Block on its own CI (`gh pr checks <pr> --watch`).
2. On a **failed** check: read the failing log, fix the **real** cause (never
   disable a gate to go green), commit, push, re-watch.
3. On a **conflict** with `main` (a sibling merged): `git fetch origin main` then
   `git merge origin/main` — **merge, not rebase**, so the push stays
   fast-forward and never needs `--force`. Resolve, re-run the local gate, push,
   re-watch.
4. When green and mergeable: **stop** (do not self-merge — the dispatcher
   merges).
5. Safety valve: after ~5 unsuccessful attempts, post a PR comment summarizing
   the blocker and stop, so the dispatcher can intervene.

Self-healing also makes the contention problem mostly disappear: when one PR
merges, every other open PR notices it is now conflicting and rebases itself.

### Standard worker prompt skeleton

Every worker prompt should be self-contained (a fresh session has no memory of
the dispatcher conversation) and include:

- **Rules:** follow the repo's contributor instructions; Conventional Commits
  with no AI attribution; the project's doc-update expectations; test via the
  repo's `make` gate, not bare tooling.
- **Boundaries:** work only inside this worktree; never touch another branch or
  PR; never read, print, log, or pass any secret/credential anywhere.
- **The task:** what to change, which files, the acceptance check, and the bare
  backlog ID to put in the PR title and body.
- **Do not merge** — the dispatcher merges.
- **The self-healing loop** above.

## The dispatcher loop

For each task, in priority order and respecting the concurrency cap:

1. Spawn the worker chip with a self-contained prompt.
2. When its PR reaches **green + mergeable**, **review the diff for scope** — is
   it doing exactly the task, with no stray changes, no weakened gate, no
   security default regressed? Green CI does not prove this.
3. Merge (`gh pr merge <n> --squash --delete-branch`).
4. Advance: spawn the next task in that stream.

Keep a small written tracker (a scratch file in the gitignored `tmp/`) of
task → chip → PR → state, plus the decisions made. It is cheap and makes the run
auditable and resumable.

## The merge model

This is the key design decision; get it right up front.

- **Auto-*fix* is delegated to each session.** Pushing fixes/rebases is scoped to
  one PR and reversible, so it is safe to hand to the worker. (This is the
  self-healing loop.)
- **Auto-*merge* stays dispatcher-gated.** Merge is a global, irreversible write
  to `main`. Keep it behind one gate because the dispatcher's merge step is where
  **scope review** and **merge ordering** happen, and it is the single
  **stop/amend** control point. CI-green is not the same as correct-and-in-scope.
- **GitHub-native auto-merge needs branch protection + required status checks.**
  Without them, `gh pr merge --auto` has no required-checks gate and can merge on
  mergeability alone — including before CI attaches. So "let each session
  auto-merge" is not a free switch; it would first require configuring branch
  protection.
- **If you want to cut dispatcher polling**, the right move is: enable branch
  protection with required checks, then have the **dispatcher** run
  `gh pr merge <n> --auto` *after its scope check* — GitHub merges on green while
  the gate stays. Optionally **tier by risk**: let purely mechanical PRs (lint
  gates, docs) auto-merge on green, keep feature/large/security PRs
  dispatcher-gated.

## Concurrency and contention

- **Cap concurrency** (3 is a sane default) so a small machine and the reviewer
  are not overwhelmed.
- **Group tasks by the files they touch into "streams" and sequence within a
  stream.** Two PRs editing the same CI workflow or `Makefile` will conflict; run
  them one after another, and run *different* streams in parallel. Self-healing
  covers accidental overlaps, but sequencing avoids needless rebase churn.
- **Land foundational/shared-file changes first, then fan out dependents.** If
  one task introduces a fix that others will inherit (e.g. a shared `Makefile`
  setting), merge it before the dependents run so they do not rediscover the same
  problem in parallel. Warn workers about known shared-file pitfalls in their
  prompts.

### Dispatcher owns coordination files

The biggest source of conflicts the first time was every PR editing the same two
**coordination files** — the backlog status file (each removing its row) and the
release checklist (each ticking its box). Every merge then invalidated every
other open PR.

Fix: **the dispatcher owns those files.** Workers do not edit the backlog status
file or the release checklist. The dispatcher removes completed rows and ticks
boxes itself, in its own isolated commits (or a single follow-up PR). Workers
still update *task-specific* docs (the design/operations pages their change
affects) — just not the shared coordination files. This removes the dominant
conflict class outright.

## Conflict policy

- **Doc-only / trivial conflicts** the dispatcher can resolve directly (a small
  helper that merges `origin/main` into the PR branch in a throwaway worktree and
  fast-forward-pushes works well).
- **Semantic / code conflicts** go back to a worker: spawn a small resolve chip
  that takes over the PR branch, merges `main`, resolves with full judgment,
  re-runs the gate, and pushes. The dispatcher does not hand-edit code conflicts
  on another session's branch.

## PR-watcher requirements

If you automate "tell me when a PR is ready," the watcher must:

- Gate "mergeable" on **both** all-checks-green **and** the `mergeable` field —
  not checks alone (a green PR can still be conflicting).
- Re-emit on **state transitions** (failed → green, conflicting → mergeable), not
  once-and-forever, because PRs flip-flop as siblings merge.
- Handle **docs-only PRs that trigger zero CI checks** — treat
  zero-checks + mergeable as ready (and keep a periodic backstop poll, since an
  event-only watcher can miss them).

## The no-secrets rule

Workers must never read, print, log, or pass any secret to a model, and the
campaign must not introduce stored credentials. Where a task seems to need a
secret (e.g. image signing), prefer the keyless/OIDC path; if that is genuinely
infeasible, the worker **flags it** rather than introducing a key. Some tasks
simply cannot run autonomously under this rule (e.g. anything needing real
production credentials) — exclude them explicitly and hand them to a human.

## Pre-flight checklist

- [ ] Batch chosen; each item is independent and one-PR-sized.
- [ ] Tasks grouped into streams by shared files; foundational items ordered
      first.
- [ ] Concurrency cap chosen.
- [ ] Worker prompt template ready (rules + boundaries + self-healing loop).
- [ ] Dispatcher owns the coordination files; workers told not to touch them.
- [ ] Merge model decided (gated vs. risk-tiered; branch protection if using
      native auto-merge).
- [ ] PR-watcher gates on checks **and** mergeability and handles zero-check PRs.
- [ ] No-secrets boundary set; credential-dependent items excluded up front.
- [ ] Cleanup plan for leftover worktrees/branches at the end.

## Anti-patterns (lessons paid for)

- **Adding self-healing late.** The first several PRs were hand-rebased and
  needed dedicated fix/resolve chips. Make self-healing the default from task #1.
- **Letting every PR edit the shared coordination files.** This made every merge
  invalidate every sibling. Give those files to the dispatcher.
- **Running same-file tasks in parallel without sequencing.** Causes avoidable
  rebase churn; stream them instead.
- **A watcher that trusts CI buckets alone.** It reported "mergeable" for
  conflicting PRs and went silent after flip-flops. Gate on mergeability and
  re-emit on transitions.
- **Chasing the headless-CLI auto-start path.** It is blocked by the safety
  classifier and is the less-secure option. Use chips.
- **Conflating auto-fix with auto-merge.** Delegate fixes; gate merges.
