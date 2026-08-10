# Q468: how long does GitHub hold a `JobCompleted` when no session exists?

**Status:** answered 2026-07-29 and concluded.
GitHub redelivered the unacknowledged `JobCompleted` to a session created **13 h 3 m 40 s** after the arming session was deleted, with no session in existence for the whole gap.
That is past Q438's 12 h default `maxWorkerLifetime`, which is the comparison that decides the question: beyond that point the kubelet has already killed the worker, so retention outlasts the entire window in which the replay path could still have something to reclaim.
The Q435 replay path is a **recovery path**, and the architecture and runbook may say so.
Scale set torn down; experiment closed.

## Why the question was open

[Q435](q435-restart-orphan-reclaim.md) measured which worker-pod orphan classes a restarted AGC reclaims.
Three of the four are reclaimed unconditionally.
The fourth — a `Running` worker whose job ended *while the AGC was down* — has no durable deadline of its own, and is reclaimed only if GitHub redelivers that job's terminal `JobCompleted` to the restarted AGC's **new** session, which stamps the pod and lets the reaper act.

Whether GitHub does that is not something the AGC decides, and [Q438](q438-worker-lifetime-deadline.md) §3 established that it is not something reading settles either.
The published contract covers only the *within-session* acknowledgement loop: a client acknowledges with `DeleteMessage`, and messages it never acknowledged are redelivered on its next poll.
That describes a client crashing mid-batch with its session still alive.
It says nothing about how long the backend holds an unacknowledged message when **no session exists at all** for hours, which is exactly the Q435 replay path.

Two things follow from that, and they are why this needs a live measurement rather than more reading:

- **The fake cannot answer it, by construction.** `scaleset/scalesettest`'s queue log is scale-set-scoped rather than session-scoped and never expires, so a message survives any gap.
  That is the deliberately *permissive* assumption — it lets the integration tier exercise the replay path at all — but it means every green test on that path is conditional on a real retention window we have never observed.
- **The answer changes confidence, not design.** Q438's `maxWorkerLifetime` cap is on by default either way.
  What the answer changes is whether replay is a *recovery path* that usually works or a coincidence that occasionally does — and therefore whether [the architecture doc](../../design/02-architecture.md) and the [troubleshooting runbook](../../operations/troubleshooting.md) may keep presenting it as recovery.

## What is actually being measured

> After a gap of *G* during which the scale set had **no session**, does a newly created session receive an unacknowledged `JobCompleted` that provably existed and was provably unacknowledged when the gap began?

Four constraints fall out of that sentence, and between them they fix the shape of the harness:

1. **No session may exist during the gap.** So the arming run must delete its session and exit.
   A process that stays up holding a session measures something else entirely.
2. **The gap is hours.** So it outlives any single process, and the experiment's state has to be on disk rather than in memory.
3. **The message must provably have been there, unacknowledged, at *T*₀.** A negative result is only interesting if the message existed to be lost.
   So the arming run polls until it *sees* the `JobCompleted`, and then deliberately does not acknowledge it — it neither advances the cursor past it nor issues the `DeleteMessage` half of the ack.
   The recorded cursor is the id of the message *before* it.
4. **The scale set must survive the gap.** So Investigation F's scale set is durable and named, unlike Investigation E's throwaway, and is torn down by an explicit third phase.

### Producing a `JobCompleted` without running a job

The arming run needs a job to reach a terminal state at GitHub.
Running one for real would need a live runner registered against the probe's scale set, which drags in the JIT-config and container machinery Investigation E uses for its hold mode.

It cancels the workflow run instead: acquire the job, read the run identity off the `JobAssigned` (`ownerName`/`repositoryName`/`workflowRunId` — confirmed present on the live wire by Q417's verification run), then `POST /repos/{owner}/{repo}/actions/runs/{run_id}/cancel`.
The job goes terminal and the backend queues its `JobCompleted` with nothing listening.
This needs the App to hold `actions: write`, which the Investigation C/D driver already requires for `workflow_dispatch`.

A cancelled job is the right shape for this question, not a compromise: the Q434 incident's jobs ended without their AGC observing it, and *how* a job became terminal is not something the message queue's retention can depend on.

## Phases

| Phase | `PROBE_RETENTION_TEST` | What it does |
|---|---|---|
| Arm | `arm` | Create/reuse the named scale set, open a session, wait for a dispatched job, acquire it, acknowledge everything up to and including the `JobAssigned`, cancel the run, poll until the `JobCompleted` appears, **leave it unacknowledged**, delete the session, write the state file. |
| Check | `check` | Read the state file, create a **fresh** session under a different owner name, poll from the recorded cursor for a bounded window, and report `RETAINED` or `LOST` against the elapsed gap. |
| Cleanup | `cleanup` | Delete any live session and the scale set. |

The state file (`PROBE_RETENTION_STATE`, default `tmp/q468-retention-state.json`) carries the scale set id, the job and run identity, the cursor, the observed `JobCompleted` message id, and `armedAt` — the instant the session was deleted, which is when the gap starts.

### Operating recipe

Two things carried over from the Q264/Q417 live runs ([scaleset-eviction-recovery.md](../scaleset-eviction-recovery.md#reproducing-the-run)) and they are not optional here either:

- **Register repo-scoped, not org-scoped.** This repo is public and the org's `Default` runner group sets `allows_public_repositories: false`, so an org-scoped scale set never receives the job.
  A repo config URL bypasses runner groups entirely.
  Do not "fix" that by enabling public repositories on the runner group — it exposes self-hosted runners to fork pull requests.
- **Dispatch first, then arm.** A job queued against a not-yet-registered scale-set label waits server-side and is assigned the moment the scale set appears, which removes the race the probe's own `dispatch … NOW` prompt implies.

The fixture is [`q468-retention-probe.yml`](../../../.github/workflows/q468-retention-probe.yml) (`runs-on: gag-q468-retention`, one job, dispatch-only):

```bash
gh workflow run q468-retention-probe.yml --repo actions-gateway/github-actions-gateway
```

Then arm.
The App key comes from the macOS keychain and reaches the probe as a file path, never as an env-var value:

```bash
KEY_FILE=./tmp/gag-probe-key.pem; trap 'rm -f ./tmp/gag-probe-key.pem' EXIT INT TERM; install -m 600 /dev/null "$KEY_FILE" && security find-generic-password -a actions-gateway-test -s github-app-private-key -w | xxd -r -p > "$KEY_FILE" && GITHUB_APP_ID=3752347 GITHUB_APP_INSTALLATION_ID=135739122 GITHUB_ORG_URL=https://github.com/actions-gateway/github-actions-gateway GITHUB_APP_PRIVATE_KEY="$PWD/tmp/gag-probe-key.pem" PROBE_RETENTION_TEST=arm PROBE_RETENTION_STATE="$PWD/tmp/q468-retention-state.json" go run -C cmd/probe .
```

Wait the gap, then re-run the same command with `PROBE_RETENTION_TEST=check`, and when the experiment is over, once more with `PROBE_RETENTION_TEST=cleanup`.
The state file carries the experiment between them.

Two details that cost a run each if missed:

- **The key file goes in the repo-local `./tmp/`, not `mktemp -t`.** The host-wide temp directory is shared across worktrees and outside the project root; `workspace-guard` blocks writes there, and the older Investigation E recipe predates that.
- **Pass `PROBE_RETENTION_STATE` as an absolute path.** `go run -C cmd/probe .` changes the process's working directory, so the default relative `tmp/q468-retention-state.json` would land under `cmd/probe/` and the next phase would not find it.

`PROBE_RETENTION_KEEP_ARMED=true` on a `RETAINED` check leaves the message unacknowledged and appends the result to the state file's check history, so one armed job can be probed at 1 h, 4 h, 12 h.
Read that ladder with the caveat below.

## What would make a result invalid

- **A check may itself reset retention.** Creating a session is an event the backend sees; if retention is measured from the last session rather than from the message's arrival, every rung of a ladder after the first is measuring a shorter gap than it claims.
  The headline number should come from a single check on a freshly armed experiment.
  The ladder is exploratory only.
- **A negative result is bounded by the check window.** `LOST` means "not redelivered within the check's poll window", not "provably deleted".
  The window is a few poll cycles, which is what a restarting AGC gets in practice, so that bound is the operationally meaningful one — but it is a bound.
- **One tenant, one region, one point in time.** Retention is backend policy and is not published; a measurement constrains what we may claim, it does not establish a contract.
  Anything written from a run says "observed", with the date.
- **An unacknowledged `JobAssigned` would confound it.** The arming run acknowledges the assignment fully (cursor *and* `DeleteMessage`) before cancelling, so the only unacknowledged message in the log at *T*₀ is the `JobCompleted` under test.

## Tests

Investigation F is exercised against the `scalesettest` stub in [`cmd/probe/retention_test.go`](../../../cmd/probe/retention_test.go): config parsing and phase selection, the arm→check state round-trip, the `RETAINED` verdict, the `LOST` verdict, the check history under `PROBE_RETENTION_KEEP_ARMED`, and cleanup deleting the scale set.

The stub grew one route for this — `POST /repos/{owner}/{repo}/actions/runs/{id}/cancel`, which completes the run's assigned jobs and queues their `JobCompleted`.
That models the causal chain the arming phase depends on, so the test covers the probe's cancel wiring rather than stubbing around it.

What these tests cannot cover is the measurement itself: the stub's queue never expires, so `RETAINED` is the only outcome it can produce from a real arm.
The `LOST` case is driven from a state file pointing at an empty queue. **A green suite here means the harness works, not that the replay path does.**

## Findings

### The first rung: RETAINED at 29 s (2026-07-28)

Armed against `actions-gateway/github-actions-gateway`, scale set `gag-q468-retention` (id 11), job `2446648c-…`, run `30409332447`.

| Step | Observed |
|---|---|
| Assignment | `JobAssigned` delivered ~0.6 s after the session opened (the fixture job was already queued against the label). Carried a complete run identity — `ownerName`, `repositoryName`, `workflowRunId` — corroborating [Q417](../scaleset-eviction-recovery.md#the-rerun-target-was-unidentified-on-scale-set--resolved-2026-07-26). |
| Cancel | `POST …/runs/30409332447/cancel` accepted; the job went terminal with **no runner ever involved**, confirming the arming phase does not need one. |
| Completion | `JobCompleted` (message `100000002`) appeared ~0.2 s later, `result: canceled`. Left unacknowledged; cursor recorded at `100000001`. |
| Gap start | Arming session deleted at `23:52:38Z`. |
| Check at +29 s | Same message id `100000002` redelivered to a **new** session under a different owner name. **RETAINED.** |

What that does and does not establish:

- **Does:** the replay path is real on the live wire, not just in the fake.
  A session that never saw the job receives its terminal message, which is exactly the mechanism Q435's fourth orphan class depends on.
  And the harness itself is validated end to end against GitHub rather than only against `scalesettest`.
- **Does not:** answer the question.
  29 s is not a multi-hour outage.
  The Q434 incident's window was 16 hours.
  A backend that holds a message for a minute and drops it at an hour would produce exactly this result.

**One divergence the live run caught.** GitHub spells the cancelled result `canceled`, one L. `scalesettest` had been written with `cancelled`, so the fake and the wire disagreed on a field a client can branch on.
Corrected in the same change — which is the whole argument for driving these probes against the shipping client rather than a bespoke one.

### The answer: RETAINED at 13 h 3 m 40 s (2026-07-29)

The same armed experiment, checked once more at 05:56 PDT / 12:56 UTC — a gap of **13 h 3 m 40 s** during which the scale set had no session at all.

| Step | Observed |
|---|---|
| Scale set | `gag-q468-retention` (id 11) still registered, its queue log intact across the gap. |
| Check session | Created under `gag-q468-check`, a different owner name than the arming session — what the backend sees is a new listener arriving after the gap, which is what a restarted AGC is. |
| Redelivery | Message `100000002` — byte-for-byte the armed one, `result: canceled`, `finishTime` still `2026-07-28T23:52:38Z` — arrived on the **first** poll, 351 ms after the session opened. **RETAINED.** |
| Teardown | Session deleted, then the scale set, taking its queue log with it. |

**The intervening-session caveat does not bite here, and the arithmetic is why.** The worry was that a check is itself a session, so if the backend measures retention from the last session rather than from the message's arrival, every later rung is measuring a shorter gap than it claims.
The only intervening session was the 29 s check, which ended ~31 s after arming.
So the pessimistic model gives 13 h 0 m 50 s and the optimistic one 13 h 3 m 40 s — a 0.2 % difference, both on the same side of every threshold that matters.
The ladder is exploratory *in general*; this particular ladder has its second rung more than three orders of magnitude past its first, which collapses the two models onto the same answer.
No freshly armed confirmation is needed to write the number down.

**Why 13 h settles a question the 16 h incident might look like it reopens.** The Q434 outage ran 16 hours, and this measurement does not reach that far.
It does not need to: `maxWorkerLifetime` defaults to 12 h and is enforced by the kubelet with no live AGC, so at the 12 h mark the stranded worker is already `Failed`/ `DeadlineExceeded` and is reclaimed by the ordinary terminal-orphan path that Q435 proved unconditional.
Retention only has to outlast the window in which a `Running` worker can still exist for redelivery to be worth anything, and 13 h outlasts 12 h.
The two mechanisms overlap rather than leaving a gap between them.

What this does **not** establish:

- **A retention window — only a lower bound.** `LOST` was never observed at any gap, so the measurement says retention ≥ 13 h and says nothing about where it ends.
  Anyone tempted to derive a timeout from this number should not.
- **A contract.** One tenant, one region, one point in time, on an undocumented backend policy.
  The claim written into the docs is "observed, 2026-07-29", not "GitHub guarantees".
- **That redelivery is sufficient on its own.** It is a recovery path, not the recovery mechanism.
  Q438's cap remains the unconditional backstop precisely because it depends on nothing GitHub does.

## Outcome

The redelivery sentences in [architecture §Worker-Pod Reaper](../../design/02-architecture.md) and the [troubleshooting runbook](../../operations/troubleshooting.md) stood as written and were upgraded from hedged to measured; the `LOST` contingency that would have demoted them did not occur.
The harness stays in `cmd/probe` — the experiment is re-armable against a future GitHub, which is the point of building it as three phases around a state file rather than as a one-off script.
