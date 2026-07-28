# Q468: how long does GitHub hold a `JobCompleted` when no session exists?

**Status:** harness built, not yet run live. `cmd/probe`'s Investigation F
(`PROBE_RETENTION_TEST`) arms and checks the experiment; no live run has been
performed, so **the retention question remains unanswered**. The findings
section below is empty on purpose — it gets written from a run's output, not
from expectation.

## Why the question is open

[Q435](archive/q435-restart-orphan-reclaim.md) measured which worker-pod orphan classes
a restarted AGC reclaims. Three of the four are reclaimed unconditionally. The
fourth — a `Running` worker whose job ended *while the AGC was down* — has no
durable deadline of its own, and is reclaimed only if GitHub redelivers that
job's terminal `JobCompleted` to the restarted AGC's **new** session, which
stamps the pod and lets the reaper act.

Whether GitHub does that is not something the AGC decides, and
[Q438](archive/q438-worker-lifetime-deadline.md) §3 established that it is not
something reading settles either. The published contract covers only the
*within-session* acknowledgement loop: a client acknowledges with
`DeleteMessage`, and messages it never acknowledged are redelivered on its next
poll. That describes a client crashing mid-batch with its session still alive.
It says nothing about how long the backend holds an unacknowledged message when
**no session exists at all** for hours, which is exactly the Q435 replay path.

Two things follow from that, and they are why this needs a live measurement
rather than more reading:

- **The fake cannot answer it, by construction.** `scaleset/scalesettest`'s
  queue log is scale-set-scoped rather than session-scoped and never expires, so
  a message survives any gap. That is the deliberately *permissive* assumption —
  it lets the integration tier exercise the replay path at all — but it means
  every green test on that path is conditional on a real retention window we
  have never observed.
- **The answer changes confidence, not design.** Q438's `maxWorkerLifetime` cap
  is on by default either way. What the answer changes is whether replay is a
  *recovery path* that usually works or a coincidence that occasionally does —
  and therefore whether [the architecture doc](../design/02-architecture.md) and
  the [troubleshooting runbook](../operations/troubleshooting.md) may keep
  presenting it as recovery.

## What is actually being measured

> After a gap of *G* during which the scale set had **no session**, does a newly
> created session receive an unacknowledged `JobCompleted` that provably existed
> and was provably unacknowledged when the gap began?

Four constraints fall out of that sentence, and between them they fix the shape
of the harness:

1. **No session may exist during the gap.** So the arming run must delete its
   session and exit. A process that stays up holding a session measures
   something else entirely.
2. **The gap is hours.** So it outlives any single process, and the experiment's
   state has to be on disk rather than in memory.
3. **The message must provably have been there, unacknowledged, at *T*₀.** A
   negative result is only interesting if the message existed to be lost. So the
   arming run polls until it *sees* the `JobCompleted`, and then deliberately
   does not acknowledge it — it neither advances the cursor past it nor issues
   the `DeleteMessage` half of the ack. The recorded cursor is the id of the
   message *before* it.
4. **The scale set must survive the gap.** So Investigation F's scale set is
   durable and named, unlike Investigation E's throwaway, and is torn down by an
   explicit third phase.

### Producing a `JobCompleted` without running a job

The arming run needs a job to reach a terminal state at GitHub. Running one for
real would need a live runner registered against the probe's scale set, which
drags in the JIT-config and container machinery Investigation E uses for its
hold mode.

It cancels the workflow run instead: acquire the job, read the run identity off
the `JobAssigned` (`ownerName`/`repositoryName`/`workflowRunId` — confirmed
present on the live wire by Q417's verification run), then
`POST /repos/{owner}/{repo}/actions/runs/{run_id}/cancel`. The job goes terminal
and the backend queues its `JobCompleted` with nothing listening. This needs the
App to hold `actions: write`, which the Investigation C/D driver already
requires for `workflow_dispatch`.

A cancelled job is the right shape for this question, not a compromise: the
Q434 incident's jobs ended without their AGC observing it, and *how* a job
became terminal is not something the message queue's retention can depend on.

## Phases

| Phase | `PROBE_RETENTION_TEST` | What it does |
|---|---|---|
| Arm | `arm` | Create/reuse the named scale set, open a session, wait for a dispatched job, acquire it, acknowledge everything up to and including the `JobAssigned`, cancel the run, poll until the `JobCompleted` appears, **leave it unacknowledged**, delete the session, write the state file. |
| Check | `check` | Read the state file, create a **fresh** session under a different owner name, poll from the recorded cursor for a bounded window, and report `RETAINED` or `LOST` against the elapsed gap. |
| Cleanup | `cleanup` | Delete any live session and the scale set. |

The state file (`PROBE_RETENTION_STATE`, default `tmp/q468-retention-state.json`)
carries the scale set id, the job and run identity, the cursor, the observed
`JobCompleted` message id, and `armedAt` — the instant the session was deleted,
which is when the gap starts.

### Operating recipe

Two things carried over from the Q264/Q417 live runs
([scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#reproducing-the-run)) and
they are not optional here either:

- **Register repo-scoped, not org-scoped.** This repo is public and the org's
  `Default` runner group sets `allows_public_repositories: false`, so an
  org-scoped scale set never receives the job. A repo config URL bypasses runner
  groups entirely. Do not "fix" that by enabling public repositories on the
  runner group — it exposes self-hosted runners to fork pull requests.
- **Dispatch first, then arm.** A job queued against a not-yet-registered
  scale-set label waits server-side and is assigned the moment the scale set
  appears, which removes the race the probe's own `dispatch … NOW` prompt
  implies.

The fixture is [`q468-retention-probe.yml`](../../.github/workflows/q468-retention-probe.yml)
(`runs-on: gag-q468-retention`, one job, dispatch-only):

```bash
gh workflow run q468-retention-probe.yml --repo actions-gateway/github-actions-gateway
```

Then arm. The App key comes from the macOS keychain, written to a temp file
rather than an env var:

```bash
KEY_FILE=$(mktemp -t gag-probe-key.XXXXXX) && trap 'rm -f "$KEY_FILE"' EXIT INT TERM && security find-generic-password -a actions-gateway-test -s github-app-private-key -w | xxd -r -p > "$KEY_FILE" && GITHUB_APP_ID=3752347 GITHUB_APP_INSTALLATION_ID=135739122 GITHUB_ORG_URL=https://github.com/actions-gateway/github-actions-gateway GITHUB_APP_PRIVATE_KEY="$KEY_FILE" PROBE_RETENTION_TEST=arm go run -C cmd/probe .
```

Wait the gap, then re-run the same command with `PROBE_RETENTION_TEST=check`,
and when the experiment is over, once more with `PROBE_RETENTION_TEST=cleanup`.
The state file (`tmp/q468-retention-state.json` by default) carries the
experiment between them.

`PROBE_RETENTION_KEEP_ARMED=true` on a `RETAINED` check leaves the message
unacknowledged and appends the result to the state file's check history, so one
armed job can be probed at 1 h, 4 h, 12 h. Read that ladder with the caveat
below.

## What would make a result invalid

- **A check may itself reset retention.** Creating a session is an event the
  backend sees; if retention is measured from the last session rather than from
  the message's arrival, every rung of a ladder after the first is measuring a
  shorter gap than it claims. The headline number should come from a single
  check on a freshly armed experiment. The ladder is exploratory only.
- **A negative result is bounded by the check window.** `LOST` means "not
  redelivered within the check's poll window", not "provably deleted". The
  window is a few poll cycles, which is what a restarting AGC gets in practice,
  so that bound is the operationally meaningful one — but it is a bound.
- **One tenant, one region, one point in time.** Retention is backend policy and
  is not published; a measurement constrains what we may claim, it does not
  establish a contract. Anything written from a run says "observed", with the
  date.
- **An unacknowledged `JobAssigned` would confound it.** The arming run
  acknowledges the assignment fully (cursor *and* `DeleteMessage`) before
  cancelling, so the only unacknowledged message in the log at *T*₀ is the
  `JobCompleted` under test.

## Tests

Investigation F is exercised against the `scalesettest` stub in
[`cmd/probe/retention_test.go`](../../cmd/probe/retention_test.go): config
parsing and phase selection, the arm→check state round-trip, the `RETAINED`
verdict, the `LOST` verdict, the check history under
`PROBE_RETENTION_KEEP_ARMED`, and cleanup deleting the scale set.

The stub grew one route for this — `POST /repos/{owner}/{repo}/actions/runs/{id}/cancel`,
which completes the run's assigned jobs and queues their `JobCompleted`. That
models the causal chain the arming phase depends on, so the test covers the
probe's cancel wiring rather than stubbing around it.

What these tests cannot cover is the measurement itself: the stub's queue never
expires, so `RETAINED` is the only outcome it can produce from a real arm. The
`LOST` case is driven from a state file pointing at an empty queue. **A green
suite here means the harness works, not that the replay path does.**

## Findings

*(empty — no live run has been performed)*

## Open

- The live run itself. It needs Tier C-grade App credentials (`actions: write`
  on a repo whose workflow can target the probe's scale set) and a multi-hour
  wall clock, so it is a scheduled operator action rather than something CI can
  hold open.
- If the result is `LOST` at a short gap, the replay path stops being a recovery
  path: [architecture §Worker-Pod Reaper](../design/02-architecture.md) and the
  [troubleshooting runbook](../operations/troubleshooting.md) both need their
  redelivery sentences demoted, and Q438's cap becomes the sole recovery
  mechanism rather than a backstop.
