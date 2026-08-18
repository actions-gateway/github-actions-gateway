# Parallel-dispatch playbook

How to clear a batch of backlog items by running several agent sessions in parallel: one session and pull request (PR) per task, coordinated by a single **dispatcher** session that carries each PR to verified-and-ready and hands it to the maintainer.
No agent merges ([the merge model](#the-merge-model)).

**The process itself is two globally-installed skills** — [`session-orchestrator`](skills.md#session-orchestrator) for the dispatcher and [`session-worker`](skills.md#session-worker) for each worker — and this page does not restate them.
They own selection, spawning, the worker contract, the watcher loop, assignment and spacing, coordination, conflict principles, and the never-merge rule.
This page owns what is true of *this* repo: the gate, the throttle, the merge queue, the tooling under `scripts/agent/`, the hooks, and every measurement taken here.
Where the two disagree, this page wins, and both skills say so.
The role is called the **dispatcher** here; the skill calls it the orchestrator, and they are the same session.

The skills are private, so a contributor reading this page alone gets the deltas rather than the process, and what holds the rules for them is the tooling rather than the prose ([skills.md § What a contributor without the skills actually loses](skills.md#what-a-contributor-without-the-skills-actually-loses)).

Dispatch here is for roughly S–M Queue rows.
Larger or coupled work is not dispatched at all.

## How to start a run

Kick off a run with **`/goal`** — the goal's Stop hook is what keeps the dispatcher attending until the batch is done.
Point the condition at this playbook and fill in the run-specific knobs.
A ready-to-paste template:

> **`/goal`** Act as the **dispatcher** for a parallel-dispatch run, following `docs/development/parallel-dispatch.md`.
> Clear **[BATCH — e.g. "the remaining `1.0-gate` items in `docs/queue/`"]**: one worker session (task chip) and one PR per task, **max [N] concurrent** (from `scripts/agent/local-throttle.sh workers`).
> Spawn each worker by invoking `/session-worker` with only the delta; the skill carries the contract.
> **You own assignment, merge ordering, and scope.** Verify each PR's scope and that its heavy gates ran, then **report it ready.
> I merge; you never do.** **No secret may be read, printed, logged, or passed to a model** — exclude any task needing real credentials and tell me.
> Minimize asks (only genuine decisions, e.g. a license choice).
> Document decisions in `tmp/`.
> I can stop or amend the rules anytime.

The knobs to set each run:

- **Batch / scope** — which items (a label filter, a Queue range, an explicit list).
  The release theme is no longer a knob to pre-state: the dispatcher asks for it before selecting anything, and "none" is a real answer ([what each half means here](#what-the-release-theme-means-here)).
- **Concurrency cap** — `scripts/agent/local-throttle.sh workers` sizes it for the machine you are on ([Concurrency and contention](#concurrency-and-contention)).
- **Exclusions** — anything needing real secrets or a live cluster; state it up front rather than making the dispatcher discover it mid-run.
- **Model per task** — the dispatcher sets each worker's model in its spawn prompt, and records the choice in the `tmp/` tracker alongside task → chip → PR → state.

Two practical notes:

- You will **click each task chip** to start its session — that is the intended, secure mechanism.
  Do not ask for headless auto-start; the safety classifier blocks it.
- The condition above *references* this playbook, so the dispatcher must be able to read it from its checkout.
  If you are running a dispatch before this file has landed on the branch the dispatcher reads, paste the rules inline instead.

## The sub-agent rule is enforced here

Workers are task chips, never `Agent`/`Task` sub-agents.
`session-orchestrator` §1 owns the rule and the reasoning; what is local is that a `PreToolUse` hook (`scripts/agent/claude-no-subagent-workers-hook.sh`, wired in `.claude/settings.json`) fires when a spawn looks like a worker — it requests its own worktree, or its prompt carries PR-producing verbs (`gh pr create`, `git push`/`commit`, "open a PR", self-heal, `implement Q<NN>`) — and asks for confirmation, pointing back here.
It is a soft nudge (`ask`, not a block) tuned for low false positives: read-only agent types (`Explore`, `Plan`) pass untouched, so legitimate research and build agents are unaffected.

## The worker contract (self-healing)

**The contract is the [`session-worker`](skills.md#session-worker) skill**, which every worker follows by invocation rather than by being told.
Do not restate any of it here or in a prompt.
What follows is only what that skill cannot know.

### This repo's deltas

- **The gate is `make check`**, and the fast prose gate is `make docs-gates`.
  Run `docs-gates` the moment prose is written rather than waiting on the ten-minute gate; `em-dash-check` and `md-reflow-check` are the two it catches that nothing else does until then.
  The sub-gates worth reading by name in `make check`'s output are `doc-links`, `lint-backlog`, `plan-index-check`, `no-plan-refs-check` and `em-dash-check`.
- **Once the final gate is running, a `Bash` call is an edit.** Any Bash call runs the piped-gate hook, which rebuilds the shared `.build/pipedgate` binary that `claude-piped-gate-hook-test` deletes mid-run, so the suite reads a real deny payload and fails (Q825).
  Wait for the task notification rather than polling.
- **Allocate every new Queue ID with `make queue-id TITLE="…"`**, never by hand — it searches for near-duplicates before it claims, and concurrent workers otherwise pick the same number.
- **A `flake` row is not deleted when it is fixed.** It moves to Deferred § Flake watch with a revive trigger, per `lint-backlog.sh` rule 8.
  Match the rows already there.
- **The e2e lanes are exempt from "prove the gate ran".** Both are merge-group-only (Q675), so they never run on a PR and their absence there is expected rather than a skipped gate.
  Everything else follows [testing.md § Path-gated workflows](testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran).
- **No live cluster unless the prompt scopes the worker to one.** The GKE dogfood cluster is classified production and prod-guard denies unpinned mutating commands, so pin the target on every one.

### Primary: the pr-sentinel background watcher

[pr-sentinel](https://github.com/karlkfi/claude-pr-sentinel) is the watcher this repo uses, installed as a Claude Code plugin per that repo's README.
After a `gh pr create` or a PR-branch `git push`, its `PostToolUse` hook nudges the session to launch a `bash` watcher as a background task, and the nudge prints the real path every time it fires, so there is nothing to copy from this page.

The skill states the launch shape.
Two things about it are measured here:

- **`CLAUDE_PLUGIN_ROOT` is not set in the shell the Bash tool runs** (measured 2026-08-01; the other `CLAUDE_*` variables are present).
  So substituting it expands to `/scripts/pr-sentinel-watch.sh` and exits 127, which is loud and recoverable — while *setting* it somewhere would trade the loud 127 for a silent stall, because pr-sentinel's guard tokenizes with `shlex.split` and expands nothing, so the launch would still fall through to a prompt.
  An unattended worker waits there and its PR spends the rest of its life unwatched.
- **The knobs go in `.claude/settings.json`'s `env` block**, not in an inline `VAR=…` prefix, which costs the launch its three-token auto-allow.
  That is where this repo sets `PR_SENTINEL_TIMEOUT` to 6 h: the stock 1 h budget expires under a batch, where the heavy gates queue behind each other, and each expiry costs a `timeout` wake plus a relaunch the worker has to still be alive to perform.
  `PR_SENTINEL_WATCH_UNTIL` stays at the plugin default, `ready`, so a worker goes idle at green and the green-to-merged window falls to the dispatcher ([the post-`ready` gap](#the-post-ready-gap)).

Because the watcher wakes on **mergeable state** as well as checks, a sibling PR merging — which silently leaves this PR conflicting with no CI signal — is just another wake rather than a separate mechanism to run.
On a `conflict` wake this repo takes pr-sentinel's default `rebase` heal: `git rebase origin/main`, resolve, re-run `make check`, `git push --force-with-lease`.
Branches here are single-owner, so a rebase keeps history linear and costs nothing a squash-merge would not discard anyway, and branch-guard's default `strict` push policy auto-approves a force-push of the worktree's own branch, so this needs no prompt.

pr-sentinel also **denies** foreground CI polling — `gh pr checks --watch`, `gh run watch`, and hand-rolled `while/until … sleep` loops — via a `PreToolUse` hook, so a worker cannot accidentally pin its main thread.
Override for one legitimate poll with `PR_SENTINEL_OVERRIDE=<reason>`.

### Fallbacks

- **A self-managed background watcher**, the shape `session-worker` specifies.
  Here that means `gh pr checks <n>` and `gh pr view <n> --json mergeable`, healing with `git rebase origin/main`, `make check`, `git push --force-with-lease`, relaunch.
- **`/autofix-pr` cloud session, last resort.** It has been unreliable, it **cannot** react to merge conflicts (GitHub emits no webhook when the base branch advances), and it wakes on the PR **comment stream** — an indirect prompt-injection channel on a public repo, since anyone who can comment can plant text the agent then treats as instructions.
  Avoid it here; reserve it for cases where neither the plugin nor a background task is workable.

### The safety valve reaches the dispatcher, not the maintainer

If a PR cannot be made green after ~5 attempts, post a PR comment summarizing the blocker and stop.
A PR comment reaches the dispatcher, which reads the PR; it does not reach the maintainer, who reviews the body and the diff.

A check that fails on `main` too is not this PR's to carry, and a dispatch run is where that matters most: every worker is blocked at once, so the fix gets a PR of its own, [searched for before it is written](../../CONTRIBUTING.md#when-main-is-broken).

### The post-`ready` gap

`session-orchestrator` §7 assigns this window to the dispatcher and says why relaunching the worker's own watcher does not close it.
What is local is the watch itself:

```bash
scripts/agent/pr-mergeability-watch.sh <pr>
```

[`pr-mergeability-watch.sh`](../../scripts/agent/pr-mergeability-watch.sh) narrows the self-managed fallback to three fields, `state`, `mergeStateStatus` and `baseRefName`.
It never reads the PR body or any comment stream, so nothing a third party can write reaches the session that acts on its exit; `baseRefName` is a branch in this repository rather than authored text, and the watch refuses one that is not a plain refname instead of quoting it into the wake.
The base is read because the wake has to name a branch, and a stacked PR told to rebase onto `main` absorbs its own base into its diff (Q839).
The wake names the base and never a `git rebase --onto` line: that needs the old base head, which `merge-base` cannot recover once the base has been force-pushed.
It carries no CI output, so a batch of them does not fill the dispatcher's context with logs for failures the owning worker is fixing.

On `conflict` the dispatcher wakes the owning worker; the worker rebases onto the branch the wake names, re-runs the gate, pushes, and relaunches its own pr-sentinel watcher.

> **With no dispatcher, the window belongs to the worker.** A session started straight from a Q-ID has nobody to hand it to, and the assignment above silently addresses no one.
> Measured 2026-08-14: three PRs from one dispatcher-less session went `DIRTY` five times between `ready` and merge, `docs/STATUS.md` being the file every one of them touched.

### What the worker prompt adds here

`session-orchestrator` §2 lists what a spawn prompt carries and why restating anything else makes the chip list unreviewable.
Three of those land differently:

- **The model** is picked up front because a fresh worker has nobody to ask.
  Mechanical work runs fine on a faster model; anything touching the concurrency core, admission, or a security default warrants the strongest one, because the dispatcher's scope review is the only gate before a human sees it.
- **An unmeasured claim in a prompt is the expensive kind.** Q805's chip forwarded the row's "all three fail closed" as fact and drew an instruction from it, not to change what the checker decides; the worker's first measurement found one probe failing *open*, on the call immediately before `gh pr merge --squash`.
  Mark an unverified claim as unverified, or leave it out.
- **Contention has a guard attached.** When several workers will touch one large doc, say so **and** say what the resulting `gh pr create` denial means: *read the other PR and override with the reading*, never *re-scope this change*.
  A worker who shrinks a good change to get past the guard reports that as scoping rather than friction, so it never reaches a friction report and cannot be found retrospectively.
  Measured across one batch: five overrides, each recorded with a reading, one of which caught a cross-PR contradiction nothing else would have.

**Keep the slash invocation**, and read the `<command-name>` marker it records as a set — `{"/dispatch-worker", "/session-worker"}` — because sessions dispatched before the skill was renamed carry the old spelling and stay valid.

> ```
> /session-worker Q664 — Opus 5. Verified 2026-08-04: the reap wait at
> worker_lifecycle_test.go:187 times out with two pods still listed. Do NOT
> raise the timeout; decide test-bug vs reaper-bug on evidence. Q666 is in
> flight adding the failure dumps this needs.
> ```

## What selection costs, measured here

`session-orchestrator` §5 states the general shape: parallelizability, spacing and size all bias a batch away from the top of the Queue, so a cleared batch is evidence about what parallelizes rather than about what mattered most.

The size bias is the largest and the only one measurable from the transcripts.
Rows worked by dispatched sessions were 79% `S` and 20% `M`; rows worked by hand were 57% `S` and 41% `M`.
That single bias is enough to explain why a dispatched session costs about 0.61× a manual one: it runs 0.77× the turns at 0.79× the tokens per turn, and a smaller task produces both, since fewer files to touch means fewer turns and less new context cached per turn.
Measured from the session transcripts over 2026-07-26 to 08-16.

Two caveats on those figures: a brief can name more than one Q-ID, and a manual session only enters the comparison when its opening names a row, which excludes exploratory work that never had one.
Both push the same way, so the gap is a floor rather than a point estimate.

### What the release theme means here

`session-orchestrator` § What is this batch for? asks for the theme before selecting anything, and splits it into a *ceiling* that excludes and an *emphasis* that sorts.
Both halves resolve to something this repo has already written down, so neither is invented per run.

**The ceiling is post-1.0 SemVer, and [release.md § Patch releases and backports](../operations/release.md#patch-releases-and-backports) is this project's statement of it.** The skill defers the question to the versioning scheme rather than hardcoding one; here a patch is bugfix-only, so a `feature` row is off-ceiling for a patch and a breaking change is off-ceiling for a minor.
That page also carries the exception the skill says selection must never volunteer: the fix goes to a `release-X.Y` branch cut from the tag, which costs a branch decision and a second release.

**A scoped release states its own ceiling durably, so read it rather than asking again.** The skill holds the theme for the run and writes it nowhere, which is right for an emphasis and does not describe what happens here once a release is scoped: the ceiling is already recorded as a plan doc plus the `X.Y-gate` labels naming what the tag waits for ([the scope ledger](maintaining-backlog.md#cutting-a-release-the-scope-ledger)).
A `-gate` row that the stated theme would exclude is the disagreement the skill says to report once and proceed through, not something to resolve silently.

**The emphases that recur here**, which are the candidates worth offering with their counts:

- **Parity work**: v1/v2, classic/scale-set, ARC parity for 1.6.
  These cut across versions, so the emphasis is the only thing that names them.
- **The cleanup tail**: `debt` and `retro` rows, when the aim is work that does not affect users.
- **Release harness**: the CI, test, docs and dogfood work that blocks a tag without being what anyone upgrades for.
  A version number cannot express this one, which is why [a gate label answers two questions](maintaining-backlog.md#a-gate-label-and-its-roadmap-bullet-are-two-commits-and-the-first-one-is-red) and only its `feature`/`security` half owes a roadmap bullet.

## The merge model

Both skills state it: auto-*fix* is delegated to each session, auto-*merge* is the maintainer's and no agent takes it, and a request to go faster is a request to shorten everything **before** that step.
The mechanics here:

- **The merge queue is the mechanical half, not a delegation of the gate** (active on `main` since 2026-08-03; see [merge-queue.md](../plan/merge-queue.md)).
  The queue is entered from the PR's web UI, then validates the candidate merge result, including the union with whatever is ahead of it, and kicks a failing entry back to its PR, which pr-sentinel surfaces to the owning session.
  It arbitrates green-ness, freshness, and the jointly-red case.
  It does **not** decide whether a change should land, so enqueueing is the maintainer's action, taken after their review.
- **One carve-out: restoring an enqueue the maintainer already made** (Q692).
  A worker may rebase and re-enqueue **only** when [`scripts/agent/pr-requeue-eligible.sh`](../../scripts/agent/pr-requeue-eligible.sh) says so, which requires a prior human enqueue, an open non-draft PR, no current queue entry, and a rebase whose conflicts fall solely in the merge-driver-owned files.
  A conflict anywhere else changes what was reviewed, so it wakes the maintainer instead.
  A read the checker could not take is a third answer, not a refusal: it exits 2 naming what it could not measure, because a `gh` failure otherwise reads as a measured "not OPEN", "not queued", or "nobody enqueued it", and that reason is what a later reader has instead of the eviction.
- **`ELIGIBLE` does not make the re-enqueue runnable.** `gh pr merge` routes it through `enablePullRequestAutoMerge`, which this repository forbids (`allow_auto_merge: false`), so every form of it fails `Auto merge is not allowed for this repository` (measured 2026-08-14 on #1525, gh 2.96.0).
  Report the verdict and its `measured:` line, and hand the re-enqueue to the maintainer.

## Concurrency and contention

**Take the concurrency cap from the machine**, not from a constant:

```bash
scripts/agent/local-throttle.sh workers
```

It returns the smaller of what RAM and physical cores allow, under a ceiling of 12.
A worker costs one Claude Code session (measured 0.43 GB mean / 0.60 GB peak resident) plus a share of the gate — and the gate share is already bounded by the [2-slot semaphore](testing.md#resource-auto-throttle-on-gui-dev-machines) however many workers exist, so each extra worker costs the session alone.
On a 128 GB machine that leaves room for ~138 sessions, which is why the ceiling, not the hardware, is what answers there.

**Above the ceiling the constraints are not local** — dispatcher review throughput and GitHub Actions concurrency — so going higher is a judgement call you make and set with `GAG_DISPATCH_WORKERS`, not something the machine can measure for you.
The hardware terms still bind downward: a 16 GB laptop gets 1.

**A stream is defined by the *docs* a change touches too, not just its code.** The skill states the rule; the instance worth carrying is that every agent-hook change edits the same six-bullet hook list in `CLAUDE.md`, so two hook tasks on different hook files still collide there by construction.
Q624 and Q665/Q668 ran as separate streams on that reasoning and conflicted twice, once on `CLAUDE.md` content.

### Run the local gate in the background, not on the critical path

`make check` is the biggest single block of a worker's wall clock, and under a batch most of it is *waiting*.
Its three heavy phases (`build-tags-check`, `lint`, `cover-check`) each take one of **2** machine-wide slots ([resource auto-throttle](testing.md#resource-auto-throttle-on-gui-dev-machines)), so whenever a 6-worker batch is in those phases, four of the six are queued.

**How much that costs is a property of the machine, and the two we have measured differ by an order of magnitude.** A cold gate is dominated by `cover-check`: ~19 of ~21 min on the small dev machine the original baseline was taken on, but **102 s end-to-end** on the M5 Max replacement (18 physical cores, 128 GB) with a fully cold build cache and no slot contention ([measurements](../plan/archive/local-gate-throughput.md)).
Take your own number before reasoning about the wall clock; the rule is right either way, but on a wide machine its payoff is seconds rather than the better part of an hour.

The confirming run `session-worker` §3 requires is cheap here for a specific reason: the gates that validate the doc and backlog work done while the first run was going — `lint-backlog`, `doc-links`, `plan-index-check`, `no-plan-refs-check`, `shellcheck` — are the *fast* gates, which take **no** heavy-build slot and run concurrently.
Only the three heavy phases re-queue, and they are cache-warm.
A *code* change during that window is what voids the head start, and here that means anything the gate compiles or lints: a `scripts/*.sh` or `Makefile` edit counts, not only Go.

A queued run reports itself now, so a background gate's log distinguishes "queued" from "hung": a heartbeat every 30 s (`==> waiting for a heavy-build slot (2 in use, queued 90s)...`) and a `==> heavy-build slot acquired after Ns queued` line on admission.

**Why not stagger the workers' gate starts instead?** Staggering re-orders arrivals at a fixed-throughput server; it adds no service capacity, so the batch's aggregate queue time is unchanged, and holding a worker back while a slot sits idle is strictly worse than letting the semaphore admit it.
The one mechanism a stagger could offer — let one worker warm `GOCACHE` before the rest start — is already delivered by content-keyed caching (`-trimpath` made even the test-result cache path-independent), which does not care about arrival spacing.
And `make check` takes a slot three separate times, releasing between phases, so whatever spacing you set at *t=0* is gone by the second acquisition.
Backgrounding does not reduce contention either — it stops the queue time from being *dead* time, which is the part that was actually costing the batch (Q376).

### The dispatcher owns assignment, not coordination files

The real need is preventing two workers from implementing the **same** backlog item — an *assignment* problem — not keeping `docs/STATUS.md` out of worker hands.
Each worker removes its own completed row in its own isolated commit, so PRs stay self-contained and the Queue stays current as they merge.

**Self-healing absorbs the resolution, not the cycle it costs.** The [merge driver](maintaining-backlog.md#the-merge-drivers-resolve-registry-rows-by-key-not-by-line-position) decides the Queue table by row ID, so a sibling's row deletion resolves silently inside the worker's `git rebase origin/main`.
It is per-clone `git config`, so it never runs on GitHub: the mergeability read behind `mergeStateStatus` and the merge queue's candidate build both take the plain three-way merge, where two adjacent row deletions conflict.
That server-side half is the expensive one.
A PR that is merely *behind* costs nothing, while a `DIRTY` one must rebase and force-push, and the force-push restarts the whole CI cycle whatever the driver did locally.
The same `make merge-driver` installs the [plan-index driver](maintaining-backlog.md#the-same-treatment-for-docsplanreadmemd), which does the same for `docs/plan/README.md`, and the [roadmap driver](maintaining-backlog.md#and-for-docsroadmapmd), for a batch whose items each delete their own roadmap bullet.

**So space the assignment: never hand one batch two adjacent Queue rows.** Measured 2026-08-14 against the live 97-row Queue, merged driverless in a fresh repo: adjacent deletions conflict, the same two deletions one row apart merge clean, and a new row inserted directly above a deleted one conflicts too.
The control arm is the pair re-run with the backlog driver configured, which resolves, so the probe is measuring the missing driver rather than the file.
Serializing the batch reaches the same place by idling every worker but one, so spacing is strictly cheaper (Q807).

## Coordination channels

Sessions coordinate by exchanging deliberately published state, never by reading one another's transcripts.
`session-orchestrator` §8 lists the channels.
The measurements taken here are about what a message may carry:

- **A message does reach an idle worker and drive a turn**: measured 2026-08-11, two trials, 16 s and about 20 s from send to read, the second after 25 to 35 minutes of idle with no other event in the turn.
  Its **timing** is what is not guaranteed, so it carries wakes and nudges, never a deadline.
- **A message describing repo state carries its own expiry, or it arrives wrong.** Measured 2026-08-09: a message asked a session to rebase onto an open PR's branch, that PR merged before the session acted, and the instruction had to be chased with a correction.
  "Rebase onto X, or onto `main` if X has already merged when you read this" costs one clause and needs no chasing.
- **A message asserting a mechanism carries its measurement, or says it has none.** Q805 produced it in both directions within an hour: a chip forwarded the row's fail-closed claim as verified, and the worker, having spent the session correcting exactly that, then sent the dispatcher an unmeasured mechanism of its own that a third session refuted.
  The asymmetry is the tell — a claim is easy to hold to the standard in the file you are editing and easy to drop in the message you are sending, though only the message gets acted on with no diff to review.
- **A maintainer decision relayed through a session is not something the receiving session can act on.** The two rules above are about claims that can be re-derived: state goes stale, a mechanism can be measured again.
  A decision cannot.
  It has no measurement behind it by construction, so the receiving session has no way to confirm it and no account to give if it turns out to have been garbled, superseded, or never said.
  Measured 2026-08-16: a session was told "the table layout is dead, it is going to the per-item store" as grounds for changing what `maintaining-backlog.md` documents.
  It was a real decision, accurately relayed, and still the wrong input — the repo showed a draft, dirty PR whose own Queue row was not on `main`, which is what the docs may describe.
  So a session may forward what it measured, and may say a decision was taken, but a change to repo docs, `CLAUDE.md`, or config waits for the maintainer to say it directly.
  Measurements travel between sessions; decisions do not.

### What we deliberately don't build (and why)

This was investigated end to end; recording the conclusions so they are not relitigated:

- **No file-based mailbox.** A shared maildir adds worktree + workspace-guard friction (out-of-worktree writes prompt unless allowlisted) and duplicates what `list_sessions` + `send_message` already do.
- **No SQLite claim table.** Atomic claim only matters if workers *pull* tasks.
  In the dispatcher-assigns model the spawn decision is the claim, so none is needed.
- **No comms daemon (e.g.
  Agent Mail).** Evaluated; it adds a durable inbox, file reservations, and a TUI, but the coordination pattern that actually occurs (state polling + spawn + self-healing + rare nudge) is already covered by built-ins, and a daemon does not address the one real gap below.
- **The residual gap, accepted as rare:** knowing *why* a worker is slow or stuck needs reading its private output, which is gated/awkward (`list_sessions` shows *that* it stalled, not *why*; `search_session_transcripts` requires approval).
  This is infrequent enough to handle with a manual look when it happens rather than standing up infrastructure for it.

## Conflict policy

Healing is the worker's job — the dispatcher only steps in when a worker is gone or stuck, resolving doc-only conflicts directly and sending semantic ones back to a resolve chip.
`session-orchestrator` §9 states the principles.
Everything below is what measuring them here produced.

### The heal destroys the evidence, so the wake records it

A rebase is the fix and the erasure at once.
Afterwards the branch merges clean, so `git merge-tree origin/main HEAD` reports no conflict whatever the eviction was about.
Ask later why the PR was evicted, or whether the worker and the dispatcher saw the same thing, and there is no answer left to read.
That is not hypothetical: a dispatcher's post-hoc read once contradicted a worker's contemporaneous `--assess`, and by then neither could be confirmed (Q810).

[`pr-requeue-eligible.sh --assess`](../../scripts/agent/pr-requeue-eligible.sh) runs before the rebase and appends what it measured to `tmp/requeue/<pr>.verdict`: the two commit OIDs it merged, and the paths that conflicted.

**It captures on an ordinary `DIRTY` wake too, which it did not always do.** The eligibility checks used to run ahead of the probe, and the first of them is whether a human has ever enqueued the PR.
A worker healing its own not-yet-enqueued PR is the common case and fails that check, so `--assess` refused and recorded a verdict carrying no OIDs and no conflict set: the measurement the capture exists to preserve, lost on exactly the wake that prompted it (Q814, hit independently by two workers on 2026-08-12).
The local probe now runs first and the paginated timeline read stays behind the checks, so the cheapest-first intent survives and the record carries the OIDs either way.

### Probing a merge without the driver

- **`-c merge.<name>.driver=false` does not emulate a driverless merge, it fabricates conflicts.** The value is a *command line*, so `false` runs `/usr/bin/false`, which exits non-zero, and git records a conflict for every driver-owned path both sides touched without ever attempting the built-in three-way merge.
  `true` and other no-ops fail the opposite way: they exit 0 having written nothing, so the merge silently keeps *ours* and every probe reads clean.
  Measured 2026-08-12 against GitHub as the control, on two PRs it reported `CLEAN`: `driver=false` gave `rc=1`, `driver=true` gave `rc=0`, and a bare clone with no driver configured gave `rc=0`, matching GitHub.
  There is no `-c` form that unsets a driver, so a clone that has run `make merge-driver` cannot answer this question at all.

- **Ask GitHub whether a branch is dirty, and use a driverless clone only for the conflict set.** `gh pr view <n> --json mergeStateStatus` is the server's own verdict rather than a model of it, and it costs one call.
  When you need the *paths* rather than the verdict, take them where no driver is configured:

  ```bash
  git clone --quiet --bare . /tmp/nodrv        # or any clone that never ran make merge-driver
  git --git-dir=/tmp/nodrv fetch --quiet "$(pwd)" <base_oid> <head_oid>
  git --git-dir=/tmp/nodrv merge-tree --write-tree <base_oid> <head_oid>
  ```

  [`pr-requeue-eligible`](../../scripts/agent/pr-requeue-eligible.py) used the `driver=false` form under a comment claiming to measure "what the merge queue will see".
  It did not; it measured which driver-owned files both sides touched, a superset, which is conservative for its own `ELIGIBLE` rule but a false positive every time a branch and `main` both touch `docs/STATUS.md` if read as "does this branch need a heal".
  It now derives the owned paths and the driver names from `.gitattributes` instead of the two hand-kept arrays it carried, so a stale list can no longer under-report, which is the direction that would let an unattended enqueue through.

  Two further traps made the `-c` form look like it was working, both measured 2026-08-12 and both silent.
  **A misspelled driver name fails open**: git ignores the unknown config key, runs the real driver anyway, and exits 0, so a probe written against a guessed name (`merge.status` for `docs/STATUS.md`, whose driver is `backlog`) produces byte-identical output to no probe at all.
  Together with the fabricated conflicts above, that is a probe whose two spellings are wrong in opposite directions and neither of which is ever an error.
  **The driver writes to stderr**, and its advisory line names the file it resolved, so a reader scanning the combined output for a path finds one and reports a conflict list assembled from chatter that says the opposite.
  Read the stage lines only (`^[0-9]{6} <sha> [123]`) whichever way you probe; a run with no conflict prints the merged tree OID and nothing else.

### Reading a verdict, and what it is a verdict about

- **`mergeStateStatus` lags a force-push, so pair it with the head OID.** The branch ref updates immediately and the PR object does not: measured 2026-08-12, `git ls-remote` showed the new head while `gh pr view` still returned the *superseded* OID with `DIRTY` for minutes, and pr-sentinel read it in that window and fired `conflict` on a branch that was already healed.
  Ask for `headRefOid` alongside the status and compare it to `git ls-remote origin refs/heads/<branch>`; differing OIDs mean the verdict is about something you no longer have, so re-read rather than act.
  **`UNKNOWN` with *matching* OIDs is a third case**: the read is fresh and GitHub is still computing.
  Rebase anyway rather than waiting, because a no-op rebase costs nothing and acting on an uncomputed field costs a cycle — the asymmetry decides it, not the field.

- **`HEAD` is not the PR's head, and a probe that reads it answers about the checkout instead.** Take the head from `gh pr view --json headRefOid`, and fetch `refs/pull/<n>/head` when the clone does not hold that commit: a dispatcher assessing a worker's PR never has the branch, and a worker's own local commits run ahead of what it pushed.
  Measured 2026-08-12 on the shipped checker: `--assess 1438` then `--assess 1447` from one worktree returned byte-identical output at exit 0 `ELIGIBLE`, because neither run looked at either PR.
  The failure is silent in the direction that matters, since a checkout that merges clean then reports `ELIGIBLE` for a PR whose own head conflicts in code, which is the one case the whole policy exists to hand back (Q834).

- **A ref pair is not a measurement**, because both refs move.
  The OIDs make the probe re-runnable: `git merge-tree --write-tree <base_oid> <head_oid>` re-derives the same conflict set from the objects at any later time, so a disagreement is settled by re-running it rather than argued from memory.
  That command is printed as well as recorded, which is what puts it in the session transcript the worker reports from.

- **A `WAKE` refusal is recorded, a probe that could not run is not.** So an absent record means either that the assessment never ran or that it ran and could not measure, and the file cannot tell you which.
  `--confirm` fails closed either way, since no record is not `ELIGIBLE`; what is lost is diagnostic, on exactly the after-the-fact question the record exists for.
  The shell predecessor wrote a third verdict, `UNMEASURABLE`, for this; the Python treats a probe that could not run as never a verdict, which is defensible and is filed upstream as [claude-skills#129](https://github.com/karlkfi/claude-skills/issues/129) rather than patched here, because an unmodified vendor is what keeps the next fix a clean overwrite.
  Records accumulate and the last one governs, which keeps a refusal that short-circuited before probing from erasing the measurement an earlier one took.

- **It is not a registry.** `tmp/` is gitignored and session-local; nothing reconciles it and no gate reads it.
  It is evidence for whoever asks, and it expires with the worktree.

### Driver-owned does not mean auto-resolving

**It means resolved *by key*.** A keyed merge still conflicts when both sides change the same key, and [`merge-keyed-records.awk`](../../scripts/lib/merge-keyed-records.awk) refuses rather than guessing in three enumerated cases: changed differently on both sides, deleted on one side and changed on the other, and the same new ID added on both sides with different text.
It leaves ordinary conflict markers by design, because a wrongly resolved row loses backlog state while a marker costs a minute.
Measured 2026-08-12: two PRs edited the same two rows of the `scripts/README.md` registry, and the script-index driver refused the file while the backlog driver resolved `docs/STATUS.md` alongside it, in the same rebase.
So "the conflict is confined to the driver-owned files" answers who owns the resolution, not whether one is needed, and a plan resting on the rebase coming out clean has to survive the case where it does not.
Resolve a keyed conflict by keeping **both** sides and verifying each survives, not by picking one; two sides changing one row usually means two facts about it, not a disagreement.

### The two overlap measurements, and what each settles

- **"Same file" and "different sections" both predict badly; diff context is what decides.** Measured 2026-08-12 on one file in one batch: two edits to adjacent rows of a Markdown table conflicted, while two edits to sections 180 lines apart rebased as a pure line offset.
  A dispatcher reasoning from "they touch different cells" got the first one wrong and would have got the second one right by luck.
  Measure the pair rather than predicting it, and say which you did when you tell a worker.

- **A clean merge proves the absence of a textual conflict and nothing else.** Measured 2026-08-12 across one batch: three open PRs asserted about a thing called "quota", carrying two referents between them, a Kubernetes `ResourceQuota` tenant cap in two of them and a global, project-wide GCE CPU limit in the third.
  A keyword scan reads a single collision across the three where there are really two referents.
  A topic-level glance happens to group them correctly, because the two sharing a referent are also the two competitor-analysis PRs, and that is the more dangerous result: the grouping is a coincidence of this batch, and a method that succeeds by coincidence gets trusted on the next one.
  When overriding piped-gate's overlap denial, say what the other PR claims, not only where it sits.

## PR-watcher requirements

`session-worker` states the requirement — a process that polls check state and mergeability, sleeps between polls, wakes on a transition, and reads only provider-controlled state.
Two additions come from what broke here:

- **Handle docs-only PRs that trigger zero CI checks.** Treat zero-checks plus mergeable as ready, and keep a periodic backstop poll, since an event-only watcher misses them entirely.
- **Re-emit on transitions**, not once-and-forever: PRs flip-flop as siblings merge.

**Do not arm a watch during a known-dirty window.** [`pr-mergeability-watch.sh`](../../scripts/agent/pr-mergeability-watch.sh) polls before its first sleep, so a watch armed against a PR that is already `DIRTY` fires `conflict` and exits within one `gh` call, spending the watch on a state you already knew.
Arm it after the heal reports `CLEAN`.
The inverse is not a symptom: an instant fire *after* a clean read means the PR went dirty in between, which is the watch working.

## The no-secrets rule

Workers must never read, print, log, or pass any secret to a model, and the campaign must not introduce stored credentials.
Where a task seems to need a secret (e.g. image signing), prefer the keyless/OIDC path; if that is genuinely infeasible, the worker **flags it** rather than introducing a key.
Some tasks simply cannot run autonomously under this rule (e.g. anything needing real production credentials) — exclude them explicitly and hand them to a human.

## Pre-flight checklist

Only the items this repo adds; the skills' own checks are theirs.

- [ ] Batch chosen, each item independent and one-PR-sized, with the theme taken from the dispatcher's opening question and checked against the release plan doc where one exists ([what each half means here](#what-the-release-theme-means-here)).
- [ ] No two assigned Queue rows adjacent ([why](#the-dispatcher-owns-assignment-not-coordination-files)); streams grouped by shared **docs** as well as code.
- [ ] Concurrency cap taken from `scripts/agent/local-throttle.sh workers`, or a deliberate `GAG_DISPATCH_WORKERS` override.
- [ ] pr-sentinel installed in each worker session, launched bare, with `PR_SENTINEL_TIMEOUT` and `PR_SENTINEL_WATCH_UNTIL` left to `.claude/settings.json`.
- [ ] Post-`ready` window covered by `scripts/agent/pr-mergeability-watch.sh` per handed-off PR, armed after a clean read.
- [ ] Credential-gated and live-cluster items excluded up front.
- [ ] Cleanup plan for leftover worktrees/branches at the end.

## Anti-patterns paid for here

`session-orchestrator`'s own list carries the portable ones.
These three were paid for in this repo:

- **Relaunching pr-sentinel on `ready`.** It re-reports `ready` at once and the relaunch loop spins without sleeping.
  Measured, not assumed: the first draft of this doc shipped that rule and PR #892 span on it immediately.
- **Decorating the watcher launch with an env prefix.** It costs the launch its auto-allow, so every relaunch prompts and an unattended worker stops relaunching.
  This is what made a stale "branch-guard blocks force-push" note expensive: it forced a `PR_SENTINEL_HEAL=merge` prefix onto every launch, for a restriction branch-guard's default `strict` policy does not actually impose.
- **Burning a session in an active `gh pr checks --watch` loop.** It pins the main thread so you cannot iterate while CI runs, and pr-sentinel's `PreToolUse` hook denies it outright.
