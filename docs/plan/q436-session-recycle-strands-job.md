# Q436: a failed `DeleteSession` strands the job queued on a recycling session

**Observed:** [`e2e / e2e` run 30209990520](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30209990520)
(kindnet lane, 2026-07-26), spec `E2E_AGC_JobLifecycle / E2E_AGC_MultipleJobsQueued:
each queued job gets its own worker pod`. Failed with `expected >= 1 new worker
pods, have 0` after the spec's full 6-minute `Eventually` budget. 50 of 51 other
specs passed in the same run.

This is filed as a **bug with an e2e symptom**, not a bare flake: the timeline
below identifies a specific mechanism, and its production analogue is a job that
nobody picks up until the broker expires the session server-side.

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

The spec enqueues onto a session it has just observed as live, and the AGC
recycles that same session nine seconds later. That much is expected and the
spec is written to tolerate it — [`job_lifecycle_test.go`](../../cmd/gmc/test/e2e/job_lifecycle_test.go)
says so inline: *"a recycled single-use session redelivers pool-wide, so the
worker pod for this job can lag behind the enqueue by a full re-register +
acquire cycle"*.

**That tolerance depends on the recycle completing.** Redelivery is what happens
when a session is cleanly closed. Here `DeleteSession` timed out three times, so
the session leaked instead of closing: fakegithub still holds it, nothing is
polling it, and the job queued on it is never redelivered to the pool. The
6-minute budget cannot help — no amount of waiting redelivers a message on a
session that was never closed.

So the widened budget (4 min → 6 min, Q179) treats a *latency* problem, and this
is a *liveness* problem wearing the same symptom. That is why the spec has looked
flaky-but-slow and why widening the window did not retire it.

## Why this is not only a test concern

`DeleteSession` failing is a normal partial failure — a slow or unreachable
broker. The AGC already logs the consequence accurately (*"leaked until it
expires server-side"*), but the log frames it as a resource leak. The timeline
above shows the leak also **strands any message queued on that session**. In
production that is a job which no runner will ever acquire until server-side
expiry, with no condition, event, or metric announcing it.

## What to investigate

1. **Does the leak strand messages in the real broker too, or only in fakegithub?**
   The e2e harness is the only place this has been observed. Confirm against the
   real GitHub broker semantics before treating it as a product bug — fakegithub
   may simply not implement the redelivery-on-expiry that GitHub does.
2. **Should a failed `DeleteSession` escalate past a log line?** Options: retry
   on a longer backoff beyond the current 3 attempts, surface a condition on the
   RunnerGroup, or emit a metric so a leaked session is observable.
3. **Should the spec stop racing the recycle?** Independently of the product
   question, the spec picks a live session and enqueues without pinning it, so it
   is inherently racing the AGC's single-use recycle. Enqueuing onto a session
   the harness has reserved would remove the e2e symptom regardless of how (1)
   resolves — but should not be done *before* (1), or it hides the signal.

## Evidence

Attempt-1 logs for the run above (the default log view is overwritten once a
failed job is re-run; recover a prior attempt with
`gh api repos/<owner>/<repo>/actions/runs/<id>/attempts/1/logs`).
