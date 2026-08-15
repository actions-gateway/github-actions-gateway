# Q436: a failed `DeleteSession` strands the job queued on a recycling session

**Observed:** [`e2e / e2e` run 30209990520](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30209990520) (kindnet lane, 2026-07-26), spec `E2E_AGC_JobLifecycle / E2E_AGC_MultipleJobsQueued: each queued job gets its own worker pod`.
Failed with `expected >= 1 new worker pods, have 0` after the spec's full 6-minute `Eventually` budget.
50 of 51 other specs passed in the same run.

This is filed as a **bug with an e2e symptom**, not a bare flake: the timeline below identifies a specific mechanism, and its production analogue is a job that nobody picks up until the broker expires the session server-side.

## Timeline

Every line is the AGC's own log for `session-14`, from the failing run:

| Time | Event |
|---|---|
| 16:24:26 | `listener goroutine started` — agentIndex 0, session-14 |
| 16:24:33 | `job message received` — messageId 6, the *previous* spec's job |
| **16:24:36.954** | **the spec picks session-14 as "a live session" and enqueues job 1 onto it** |
| 16:24:44 | previous job's worker pod completes (`phase: Failed`, 11.1 s) |
| 16:24:45 | `job finished; recycling single-use JIT agent` — session-14's listener tears down |
| 16:24:55 | `DeleteSession failed; the broker session is leaked until it expires server-side` — 3 attempts, `context deadline exceeded` |
| 16:30:36 | spec's `Eventually` expires: no worker pod ever appeared for job 1 |

## Mechanism

The spec enqueues onto a session it has just observed as live, and the AGC recycles that same session nine seconds later.
That much is expected and the spec is written to tolerate it — [`job_lifecycle_test.go`](../../../cmd/gmc/test/e2e/job_lifecycle_test.go) says so inline: *"a recycled single-use session redelivers pool-wide, so the worker pod for this job can lag behind the enqueue by a full re-register + acquire cycle"*.

**That tolerance depends on the recycle completing.** Redelivery is what happens when a session is cleanly closed.
Here `DeleteSession` timed out three times, so the session leaked instead of closing: fakegithub still holds it, nothing is polling it, and the job queued on it is never redelivered to the pool.
The 6-minute budget cannot help — no amount of waiting redelivers a message on a session that was never closed.

So the widened budget (4 min → 6 min, Q179) treats a *latency* problem, and this is a *liveness* problem wearing the same symptom.
That is why the spec has looked flaky-but-slow and why widening the window did not retire it.

## Why this is not only a test concern

`DeleteSession` failing is a normal partial failure — a slow or unreachable broker.
The AGC already logs the consequence accurately (*"leaked until it expires server-side"*), but the log frames it as a resource leak.
The timeline above shows the leak also **strands any message queued on that session**.
In production that is a job which no runner will ever acquire until server-side expiry, with no condition, event, or metric announcing it.

## Findings

Investigated 2026-07-26 against the attempt-1 logs and the two implementations.

### 1. The strand is fakegithub-only — the product analogue does not exist ✅

The timeline is confirmed verbatim in the attempt-1 log, including the AGC's silence afterwards: no further listener activity for `agentIndex 0` until the suite tore down.
Two listeners were up (`session-14` at agentIndex 0, `session-17` at agentIndex 1), and `session-17` polled throughout — it simply had no way to see a job sitting on `session-14`'s queue.

That per-session queue is the artifact. fakegithub's `/control/enqueue?sessionId=` addresses a job to **one** session (`jobQueues[sessionID]`), and only two paths move it back to the deliverable pool: `DELETE /session` and the single-use consumption hook, both of which call `requeueLocked`.
The real broker has no such state — a job stays in the pool until some session polls for it, and a delivery that is not acquired within ~2 min is redelivered pool-wide ([02-architecture §2.2](../../design/02-architecture.md), live-confirmed in the Q260 dogfood, where GitHub redelivered one job repeatedly over ~12 min).
`DeleteSession` *accelerates* that redelivery; it is not what makes the work reachable.
So in production a failed `DeleteSession` costs a session record and a delayed redelivery, not a stranded job.

Corollary worth keeping: `/control/sessions` reports **registered**, not **polling**.
`session-14` was already mid-job when the spec picked it at 16:24:36 — nine seconds before the recycle — so the enqueue was betting on the DELETE from the start.
All seven `fakegithubEnqueueJob` call sites take that bet.

### 2. The leak deserved a metric, not more retries ✅

The AGC's behaviour is already right: `deleteSessionDetached` retries three times on a detached context (Q222) and the listener recycles into a fresh session either way.
What was missing is that the leak is *silent* — no condition, no event, and the listener recovers as if nothing happened, so a broker slow enough to leak a session on every recycle looks like a healthy gateway.
A counter (`actions_gateway_broker_session_leaks_total`) closes that gap; a longer retry budget would only hold a shutdown open longer, and a RunnerGroup condition would alarm on something the gateway self-heals.

### 3. The spec did not need to change ✅

Fixing the harness invariant covers the race for every spec rather than pinning one.
A job addressed to a session now ages into the owner pool after 30 s undelivered, so reachability no longer depends on a DELETE landing or on the target session ever polling again.
A session that *is* polling drains its queue within one `longPollTick`, so targeted delivery — which the single-use specs rely on — is unaffected.

## What shipped

| Change | Where |
|---|---|
| Undelivered per-session jobs age into the owner pool after `defaultSessionQueueGrace` (30 s) | [`test/fakegithub/main.go`](../../../test/fakegithub/main.go) (`sweepStaleQueuesLocked`) |
| Regression test: stranded job reaches a sibling session; a polling session is not diverted; the sweep is owner-scoped | [`test/fakegithub/main_test.go`](../../../test/fakegithub/main_test.go) |
| `actions_gateway_broker_session_leaks_total{namespace, runner_group}` | [`cmd/agc/internal/runnercore/metrics.go`](../../../cmd/agc/internal/runnercore/metrics.go), incremented in `deleteSessionDetached` |
| Docs: metric in the design + operator metric references; fakegithub fidelity note | `docs/design/02-architecture.md`, `docs/design/07-test-plan.md`, `docs/operations/observability-metrics.md` |

Not addressed: **why** three 3 s DELETE attempts all timed out.
The AGC's broker client clones `http.DefaultTransport` (no `MaxConnsPerHost` cap), so connection starvation behind the long-polls is ruled out; fakegithub logs no requests, so there is no server-side record of whether the DELETE arrived.
A loaded kindnet node stalling a 3 s control-plane call is the remaining hypothesis and is not worth chasing — the harness no longer depends on the answer, and the metric now makes the same event visible in production if it recurs.

## Evidence

Attempt-1 logs for the run above (the default log view is overwritten once a failed job is re-run; recover a prior attempt with `gh api repos/<owner>/<repo>/actions/runs/<id>/attempts/1/logs`).
