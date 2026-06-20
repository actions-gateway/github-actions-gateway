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

## How to start a run

Kick off a run with **`/goal`** — the goal's Stop hook is what keeps the
dispatcher attending to merges until the batch is done. Point the condition at
this playbook (so the dispatcher follows it without restating it) and fill in the
run-specific knobs. A ready-to-paste template:

> **`/goal`** Act as the **dispatcher** for a parallel-dispatch run, following
> `docs/development/parallel-dispatch.md`. Clear **[BATCH — e.g. "the remaining
> `1.0-gate` Queue items in `docs/STATUS.md`"]**: one worker session (task chip)
> and one PR per task, **max [3] concurrent**. Each worker must be a **full,
> independent Claude Code session (a task chip), never a sub-agent**. Give every
> worker the self-healing contract from task one (enable Auto-fix via `/autofix-pr`
> for CI + review comments, run a **background** conflict-watcher that
> `git merge origin/main`, keep the main thread free, never self-merge, escalate
> after 5 tries). **You own assignment,
> merge ordering, and scope** — hand each worker exactly one item so none collide;
> each worker removes its own `docs/STATUS.md` Queue row in its PR (isolated
> commit). Stream tasks by shared files and land foundational changes first. Verify each PR's
> **scope**, then merge after CI is green. **No secret may be read, printed,
> logged, or passed to a model** — exclude any task needing real credentials and
> tell me. Minimize asks (only genuine decisions, e.g. a license choice).
> Document decisions in `tmp/`. I can stop or amend the rules anytime.

The knobs to set each run (everything else comes from this playbook):

- **Batch / scope** — which items (a label filter, a Queue range, an explicit
  list).
- **Concurrency cap** — 3 is a good default.
- **Exclusions** — anything needing real secrets or a live cluster; state it up
  front rather than making the dispatcher discover it mid-run.
- **Merge gating** — default is dispatcher-gated after a scope review; say so if
  you want risk-tiered auto-merge (and note that needs branch protection +
  required checks first, per [the merge model](#the-merge-model)).
- **Model per task** — match the model to the work, not the batch (see
  [Model selection](#model-selection)). The dispatcher sets each worker's model
  in its spawn prompt; an autonomous worker cannot run `model-advisor`
  interactively.

Two practical notes:

- You will **click each task chip** to start its session — that is the intended,
  secure mechanism. Do not ask for headless auto-start; the safety classifier
  blocks it.
- The condition above *references* this playbook, so the dispatcher must be able
  to read it from its checkout. If you are running a dispatch before this file
  has landed on the branch the dispatcher reads, paste the full rule set inline
  instead of referencing it.

## Roles

**Dispatcher** (one session — typically the one you are in):
- Selects the batch, decides priority and ordering, groups by file contention.
- Spawns one worker session per task.
- Watches each PR, verifies its **scope** (checks-green is necessary, not
  sufficient), merges it, and advances the next task.
- Owns **assignment**, merge ordering, and scope review (see
  [the dispatcher owns assignment](#the-dispatcher-owns-assignment-not-coordination-files)).
- Is the single place to **stop or amend** the run.

**Worker** (one session per task, each in its own worktree):
- Implements exactly one task, runs the local gate, opens a PR.
- **Self-heals** until its PR is green and mergeable (see
  [the worker contract](#the-worker-contract-self-healing)).
- Never merges its own PR; never touches another session's branch or files
  outside its worktree.

## Spawn mechanism: task chips

Spawn each worker as a **task chip** (a real, separate Claude Code session that
starts in its own fresh worktree on a `claude/*` branch when started). This is the
mechanism to use.

**A worker must be a full, independent Claude Code session — not a sub-agent of
the dispatcher.** Do **not** spawn workers with the Agent/Task tool or any other
in-process sub-agent: a sub-agent shares the dispatcher's session and context, has
no worktree or branch of its own, cannot open and self-heal its own PR, and dies
when the dispatcher's turn ends. A task chip is a *peer* session — its own
worktree, branch, context, permission gates, and entry in the session list — and
that independence is what the whole model (one session + one PR per task,
background self-healing, the dispatcher merging across sessions) depends on. If
you find yourself reaching for the Agent tool to "parallelize" the work, that is
the wrong mechanism here — use chips.

Reasons chips are the right call:

- They run under the normal permission gates — no blanket permission bypass.
- Each session is isolated (own worktree, own context, visible in the session
  list).

Do **not** try to auto-start headless worker sessions with a "skip all
permissions" flag. The safety classifier blocks it, and it is the less-secure
path regardless. The small cost of chips — one click to start each — is the
correct trade.

> One decision to settle **before** spawning: do not design the run around
> `send_message`-ing a worker mid-run to nudge it (see
> [Coordination channels](#coordination-channels)). Treat cross-session messaging
> as best-effort, not a control channel — a worker running unattended may not act
> on a message until re-engaged. Design for the worker to finish the job itself
> (next section) rather than relying on re-engagement.

## The worker contract (self-healing)

Bake this into **every** worker prompt from the first task. It is the single
biggest reducer of dispatcher toil — added late the first time, it should be the
default.

After opening its PR, a worker does **not** stop, and it does **not** sit in an
active CI-polling loop. It offloads both kinds of post-PR work — CI/review fixes
and conflict resolution — to background mechanisms, so its **main thread stays
free** (you can ask it follow-up questions or iterate on the change while its PR
churns toward green in the background). The background loop continues until the PR
is green **and** mergeable:

1. **Enable Auto-fix for CI and review comments.** Run `/autofix-pr` on the PR's
   branch (Claude Code detects the open PR and spawns a cloud session with
   auto-fix enabled). Auto-fix subscribes to the PR's GitHub webhooks and, on a
   **failed check** or a **review comment**, investigates and pushes the **real**
   fix — never disabling a gate to go green — with no `gh pr checks --watch`
   polling. Because it runs in a separate cloud session, the worker's main thread
   is not tied up.
   - *Requirement / fallback:* Auto-fix needs the Claude GitHub App installed on
     the repo and runs in Claude Code on the web. If it is unavailable, fall back
     to the active `gh pr checks <pr> --watch` loop (read the failing log, fix the
     real cause, commit, push, re-watch).
2. **Run a background conflict-watcher for merges.** Auto-fix **cannot** react to
   merge conflicts — GitHub emits no webhook when the base branch advances — so a
   sibling merging will silently leave the PR conflicting. Cover that gap with a
   **background agent** that periodically `git fetch origin main` then
   `git merge origin/main` (**merge, not rebase**, so the push stays fast-forward
   and never needs `--force`), resolves, re-runs the local gate, and pushes. Run
   it in the background so it does not block the main thread either.
3. When green and mergeable: **stop** the background work (do not self-merge — the
   dispatcher merges).
4. Safety valve: if Auto-fix or the conflict-watcher cannot get the PR green after
   ~5 attempts, post a PR comment summarizing the blocker and stop, so the
   dispatcher can intervene.

Self-healing also makes the contention problem mostly disappear: when one PR
merges, every other open PR's conflict-watcher notices it is now conflicting and
merges `main` back in.

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
- **Model:** the model the worker should run on, chosen by the dispatcher per
  [Model selection](#model-selection) — a fresh worker session cannot stop to
  run `model-advisor` interactively, so the choice is made for it at spawn.
- **Do not merge** — the dispatcher merges.
- **The self-healing loop** above.

## Model selection

The dispatcher picks each worker's model and bakes it into the spawn prompt. A
worker is a fresh, unattended session: it cannot pause to run the `model-advisor`
skill (which prompts the user interactively), so the per-task choice is the
dispatcher's to make up front.

Match the model to the *task*, not the batch — a dispatch run usually mixes
sizes:

- **Mechanical / well-understood work** (lint gates, CI wiring, packaging,
  docs-only edits, contained fixes) runs fine on a faster, cheaper model. Most
  parallel-dispatch batches are dominated by these.
- **Tasks with real judgment** (a fix touching the concurrency core, an
  admission/security change, anything where scope is easy to get wrong) warrant
  the strongest model — the dispatcher's scope review is the only gate, so the
  worker should not be under-powered.

When unsure, size up: a worker that picks the wrong approach costs more
dispatcher toil than the model delta. Record the per-task model choice in the
`tmp/` tracker alongside task → chip → PR → state so the run stays auditable.

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

- **Auto-*fix* is delegated to each session.** Pushing CI/review fixes (via the
  Auto-fix feature) and conflict merges (via the background conflict-watcher) is
  scoped to one PR and reversible, so it is safe to hand to the worker. (This is
  the self-healing loop.)
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

### The dispatcher owns assignment, not coordination files

Keep two things separate. The real need is preventing two workers from
implementing the **same** Queue item — an *assignment* problem — not keeping
`docs/STATUS.md` out of worker hands.

- **The dispatcher owns assignment.** It hands each worker exactly one Queue
  item, so no two pick the same one; the spawn decision *is* the claim (no lock
  mechanism needed in this assigns model).
- **Each worker owns its own Queue-row removal.** A worker removes its completed
  item from `docs/STATUS.md` in its own PR, in an **isolated commit** (per the
  repo rule that STATUS.md changes get their own commit). PRs stay self-contained
  and the Queue stays current as they merge.
- **Self-healing absorbs the churn.** When a sibling merges, the trivial
  STATUS.md conflict is resolved by the worker's `git merge origin/main` step.

The earlier rule was "the dispatcher owns the coordination files." That was a
workaround from before self-healing was robust — every PR editing STATUS.md made
each merge invalidate every sibling. With self-healing plus the isolated-commit
rule, workers owning their own row is cheaper and keeps PRs whole. The dispatcher
still owns **merge ordering** and **scope review**.

## Coordination channels

One principle holds throughout: sessions coordinate by exchanging **deliberately
published** state, never by reading one another's transcripts. A session's log is
private working memory.

In practice the coordination is carried by built-in mechanisms — no shared
mailbox, database, or comms daemon (see [What we deliberately don't
build](#what-we-deliberately-dont-build-and-why)):

- **Spawn prompt = dispatcher → worker handoff.** The task, scope, boundaries,
  and self-healing contract all go in the chip's prompt at spawn. A worker
  normally needs no further instruction.
- **`list_sessions` = worker-state visibility.** The dispatcher polls it for
  running/stalled status, PR state, and last-activity to decide what to merge and
  what to spawn next. Read-only and not permission-gated.
- **PR + PR comments = worker → dispatcher results and escalation.** A
  green+mergeable PR is the "done" signal; the safety-valve PR comment is the
  "stuck" signal.
- **Self-healing is the spine.** Workers enable Auto-fix (CI + review comments)
  and run a background conflict-watcher (`git merge origin/main`), so the
  dispatcher rarely needs to touch a running worker.
- **`send_message` = rare, reactive nudge only.** Best-effort and
  unattended-gated, so never the control path. In practice it has been used only
  to relay a specific CI-failure fix to a worker that failed to self-heal. The
  autonomous loop must not depend on it (see [the worker
  contract](#the-worker-contract-self-healing)).

### What we deliberately don't build (and why)

This was investigated end to end; recording the conclusions so they are not
relitigated:

- **No file-based mailbox.** A shared maildir adds worktree + workspace-guard
  friction (out-of-worktree writes prompt unless allowlisted) and duplicates what
  `list_sessions` + `send_message` already do.
- **No SQLite claim table.** Atomic claim only matters if workers *pull* tasks. In
  the dispatcher-assigns model the spawn decision is the claim, so none is needed.
- **No comms daemon (e.g. Agent Mail).** Evaluated; it adds a durable inbox, file
  reservations, and a TUI, but the coordination pattern that actually occurs
  (state polling + spawn + self-healing + rare nudge) is already covered by
  built-ins, and a daemon does not address the one real gap below.
- **The residual gap, accepted as rare:** knowing *why* a worker is slow or stuck
  needs reading its private output, which is gated/awkward (`list_sessions` shows
  *that* it stalled, not *why*; `search_session_transcripts` requires approval).
  This is infrequent enough to handle with a manual look when it happens rather
  than standing up infrastructure for it.

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
- [ ] Model chosen per task (mechanical → faster/cheaper; judgment-heavy →
      strongest), set in each spawn prompt.
- [ ] Workers spawned as full Claude Code sessions (task chips), **never**
      sub-agents of the dispatcher.
- [ ] Worker prompt template ready (rules + boundaries + self-healing loop:
      Auto-fix for CI/review + background conflict-watcher).
- [ ] Claude GitHub App installed so Auto-fix can receive PR webhooks; active
      `gh pr checks --watch` fallback noted for repos where it is unavailable.
- [ ] Dispatcher owns assignment + merge ordering + scope; each worker removes
      its own Queue row in an isolated commit (not the dispatcher).
- [ ] Coordination via built-ins (spawn prompt, `list_sessions`, PR/comments,
      self-healing); `send_message` only as a rare reactive nudge — no mailbox or
      comms daemon.
- [ ] Merge model decided (gated vs. risk-tiered; branch protection if using
      native auto-merge).
- [ ] PR-watcher gates on checks **and** mergeability and handles zero-check PRs.
- [ ] No-secrets boundary set; credential-dependent items excluded up front.
- [ ] Cleanup plan for leftover worktrees/branches at the end.

## Anti-patterns (lessons paid for)

- **Adding self-healing late.** The first several PRs were hand-rebased and
  needed dedicated fix/resolve chips. Make self-healing the default from task #1.
- **Bundling the STATUS.md Queue-row edit into a code commit.** Mixed into a code
  commit it makes every sibling merge conflict painfully; keep it an isolated
  commit so self-healing absorbs the trivial conflict. (Workers owning their own
  row is fine — the old "dispatcher owns the file" rule was a pre-self-healing
  workaround.)
- **Running same-file tasks in parallel without sequencing.** Causes avoidable
  rebase churn; stream them instead.
- **A watcher that trusts CI buckets alone.** It reported "mergeable" for
  conflicting PRs and went silent after flip-flops. Gate on mergeability and
  re-emit on transitions.
- **Chasing the headless-CLI auto-start path.** It is blocked by the safety
  classifier and is the less-secure option. Use chips.
- **Spawning workers as sub-agents of the dispatcher.** An Agent/Task sub-agent
  has no worktree or branch, cannot self-heal its own PR, and dies with the
  dispatcher's turn. Workers must be full, independent Claude Code sessions
  (chips).
- **Burning a session in an active `gh pr checks --watch` loop.** It pins the
  main thread so you cannot iterate while CI runs. Enable Auto-fix for CI/review
  and put conflict handling in a background agent instead; reserve active polling
  for the fallback case where Auto-fix is unavailable.
- **Conflating auto-fix with auto-merge.** Delegate fixes; gate merges.
