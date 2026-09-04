# Q645 — What the run service does with an `abandoned` completion

**Status:** answered 2026-08-04 — conclude, not re-dispatch, and the conclusion is **success**.
See [Findings](#findings).
The remedy (Q676) is measured and decided the same day: the listener reports **nothing** for its own unrun delivery, per [the remedy measurements](#q676--the-remedy-measurements-2026-08-04).
The fast ending (Q683) is measured and shipped 2026-08-05: a standalone REST force-cancel concludes run and job `cancelled` in ~1 s, per [the fast-ending measurement](#q683--the-fast-ending-measurement-2026-08-05).
The recovery arm that ending arms (Q691) shipped 2026-08-08: the cancelled run is re-run automatically once capacity returns, bounded by the shared per-run retry budget, per [the recovery re-run arm](#q691--the-recovery-re-run-arm-2026-08-08).
Remaining follow-up: [Q682](../queue/Q682.md) (sibling `skipped` arm).

Queue item: Q645 (completed; done rows are deleted).
Origin: [release-1.3.md § The rc.5 re-run](release-1.3.md#the-rc5-re-run-2026-08-02).

## The question

The Q628 fix releases an acquired-but-never-run job assignment (a worker pod reaped while `Pending`, so no runner binary ever registered) by calling `POST {run_service_url}/completejob` with `result=abandoned` (`cmd/agc/internal/listener/job.go`, behind `AGC_FANOUT_COMPLETION`).
That stops the assignment dangling until GitHub's ~15-minute unstarted-job timeout cancels the whole run.
What it does **not** establish is what GitHub does next:

1. **Re-dispatch.** The job returns to the queue and is offered to the next available runner.
   The AGC needs no further mechanism: when capacity returns, a new worker picks the job up.
2. **Conclude.** The job (and with it the run) is driven terminal.
   Then Q628 trades a silent 16-minute hang for a fast, visible failure.
   Better, but a re-run arm (the `rerun-failed-jobs` mechanism the disruption-recovery arms already have) would be needed for the job to ever execute.

Which one happens decides whether a re-run arm is also needed, which is the decision the Q645 row exists to inform.
A secondary unknown rides along for free: `broker/types.go` flags the completejob **wire format** (`result` as a lowercase-camelCase string) as never live-confirmed, the reason `AGC_FANOUT_COMPLETION` defaults off.
The same live call that answers the primary question confirms or refutes the serialization.

## Why a probe, not a dogfood turn-up

The Q645 row proposes a turn-up: reap a Pending worker on GKE, watch the run.
That measures the same wire exchange, but through five extra moving layers (cluster, autoscaler, AGC deploy, worker scheduling, reap timing), each of which has independently eaten a validation attempt in the 1.3 ledger.
The question is about **GitHub's behaviour, not ours**, which is exactly the class [testing.md § The credential-gated probe scenarios](../development/testing.md#the-credential-gated-probe-scenarios) routes to `cmd/probe`: no double can answer it, and no cluster is needed to ask it.
The probe drives the same shipping `broker.Client` calls the listener makes, so the exchange under measurement is byte-identical to the AGC's.

## Investigation H — the measurement

`PROBE_ABANDONED_TEST=true` selects it.
One run, two JIT runners, both registered repo-level against this repo (the org `Default` group refuses public repos, same as Investigations E/F/G):

1. Register JIT runners **A** and **B** with the probe label (`gag-q645-abandoned`) via `generate-jitconfig`, and open a broker v2 session for each.
   This is the registration, OAuth-exchange, and session flow the AGC's agent pool and listener run (`agentpool.GithubRegistrar` / `listener.createSession`), reproduced in the probe over the shared `githubapp` and `broker` packages.
2. Dispatch the fixture workflow ([`q645-abandoned-probe.yml`](../../.github/workflows/q645-abandoned-probe.yml)): one job, `runs-on: gag-q645-abandoned`, nothing ever runs it.
3. Both sessions receive the fan-out `RunnerJobRequest`.
   **A acquires** its delivery (`acquirejob` on the delivered `run_service_url`); B only polls and logs.
   The probe resolves the run REST-side (newest queued run of the fixture workflow) before acquiring, so the REST watch has a subject.
4. **A completes its assignment with `result=abandoned`**: the Q628 call, nothing else in between.
   No renew loop is needed, since the initial lock outlasts the seconds between acquire and complete.
   T0 is this call's response.
   A's session is then deleted and its consumed runner record deregistered, mirroring the listener's post-job recycle, so B is the only listener during the window.
5. Observe for a bounded window (default 20 min, spanning the ~15-minute unstarted-job horizon), on two channels at once:
   - **B's session.** A `RunnerJobRequest` arriving after T0 is a re-dispatch, observed on the same channel a real AGC listener would see it on.
     Its `runner_request_id` (same as A's, or fresh) is recorded: that is the fan-out identity a recovering AGC would have to dedup against.
   - **REST.** The fixture run's and job's `status`/`conclusion` polled every 15 s; every transition logged with its timestamp.
     Both levels, because the live run split them (see Findings): the run concluded while the job record never did.

Verdicts, each logged with the evidence behind it:

| Verdict | Meaning |
|---|---|
| `WIRE-ACCEPTED` / `WIRE-REJECTED` | completejob's response status: the serialization confirmation `broker/types.go` is waiting on. Rejected ends the run, since the primary question cannot be asked with a call the service refused. |
| `REDISPATCHED` | B received a job delivery after T0. The job survives an abandoned completion; no re-run arm is needed. |
| `CONCLUDED-run-<c>` / `CONCLUDED-job-<c>` | The REST run (or job) went terminal without redelivery. An abandoned completion kills the job; a re-run arm is needed for it to ever execute. |
| `NO-SIGNAL` | Neither within the window. The job is dangling exactly as if nothing had been reported, itself a finding: completejob(abandoned) released nothing. |

`REDISPATCHED` and `CONCLUDED-*` are not mutually exclusive in principle (GitHub could re-queue, fail to place, then conclude at the unstarted-job timeout); the log carries both channels' full timelines so a compound outcome reads as what it is rather than being flattened into one word.

### What would make a result invalid

- **B's session missing or late.** B must be polling before T0, or a prompt re-dispatch lands on no listener and `NO-SIGNAL` lies.
  The probe opens both sessions before the acquire and fails the run if either drops.
- **Another consumer on the label.** A leftover runner from an earlier run acquiring the redelivery would turn `REDISPATCHED` into `NO-SIGNAL` or `CONCLUDED`.
  The label is probe-specific and both runner records are deregistered on exit; the probe logs every runner on the label at startup.
- **The wrong run watched.** REST discovery takes the newest queued run of the fixture workflow.
  A stale queued run from an aborted earlier probe would be older, but the probe logs the run id and `created_at` it locked onto so a misattribution is visible in the record.
- **A cancel racing the window.** Cleanup cancels the fixture run only after the window closes (409-tolerant, per Investigation F's helper); a cancel before T0+window would manufacture `CONCLUDED-cancelled`.

## What the answer feeds

- **`REDISPATCHED`** → Q628 is complete as shipped; the Q645 row is deleted and the `release-1.3.md` "unmeasured" sentence gets the answer.
  The `AGC_FANOUT_COMPLETION` default-off caveat in `broker/types.go` can cite a live confirmation instead of a gap (flipping the default is its own decision, filed separately if taken).
- **`CONCLUDED-*`** → a new Queue row for the re-run arm: on an abandoned completion the listener must also call `rerun-failed-jobs` (or the run-level re-run), the mechanism `externallyDeletedBeforeTerminal` deliberately does not arm today because "a job that never ran has no failed job to re-run", which this verdict would refute at the run level.
- **`WIRE-REJECTED`** → the `TaskResult` serialization is wrong; fixing it precedes everything above, and the gated-off default was the right call.

## Findings

### The answer: WIRE-ACCEPTED, then CONCLUDED-run-success (2026-08-04)

One live run, first attempt, fixture run [30886332454](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30886332454), full log in the session record.
The timeline (all times UTC):

| t | Event |
|---|---|
| 07:04:10 | Fixture dispatched by a `[q645-probe]` branch push; run and job `queued` |
| 07:04:27 | Runners A (id 28620) and B (28621) registered, both sessions polling; the `RunnerJobRequest` reaches **A only** in under a second |
| 07:04:29.7 | A acquires (plan `808639ca`, job token present); the REST **job** flips to `in_progress`, `runner_name` stamped, `started_at` set |
| 07:04:29.9 | A sends `completejob result=abandoned` → **204 No Content** |
| 07:04:30 | The REST **run** is `completed` / **`success`** (`updated_at` pins it to this second) |
| 07:04:30 → 07:24:32 | 20-minute window: B receives nothing; the REST **job** stays `in_progress`, `conclusion: null`, still true 25 minutes later, after the run was long green |

**The wire format is confirmed.** The run service accepted the lowercase camelCase `result` serialization with a 204.
The `WIRE FORMAT NOT LIVE-CONFIRMED` caveat in `broker/types.go` is resolved; keeping `AGC_FANOUT_COMPLETION` off is no longer justified by serialization risk; it is now justified by the semantics below.

**The answer to Q645 is: conclude, not re-dispatch — and the conclusion is `success`.** GitHub ended the run one second after the abandoned completion.
No re-queue, no redelivery to a live listener, no unstarted-job timeout.
A job that never executed a single step reports green: the false-green outcome, worse for a CI consumer than either the silent 16-minute hang Q628 replaced or an honest failure.
The remedy decision was filed as Q676 and measured the same day, in [the remedy measurements](#q676--the-remedy-measurements-2026-08-04); candidate arms included reporting `failed` instead (concludes red and gives `rerun-failed-jobs` a target; result-value semantics unmeasured beyond `abandoned`), cancelling the run via REST before or instead of completing, or not completing at all (the told-nothing path measured 2026-08-02 left the job `queued` for 16+ minutes, which at least stays visibly unfinished).

**The run and job records disagree.** The run concluded while its only job never did: `in_progress`, `conclusion: null`, `completed_at: null`, indefinitely.
The probe's own window verdict printed `NO-SIGNAL` because its watch covered the job endpoint only; the run endpoint carried the signal.
The instrument now watches both levels (verdicts `CONCLUDED-run-<c>` / `CONCLUDED-job-<c>`), so a re-run reports this outcome directly instead of leaving it to a post-hoc REST read.

Secondary observations, recorded because a stub would have answered otherwise:

- **No fan-out to B.** Both sessions were polling before dispatch; the delivery reached A alone.
  The Q260 multi-delivery fan-out did not occur for a single queued job with two idle same-label sessions on the classic v2 flow.
- **Acquire is what "starts" the job REST-side.** `started_at` and `runner_name` were stamped at `acquirejob`, before any runner binary existed.
- **Both broker sessions came back with no encryption key** — message bodies arrived as plaintext JSON and parsed directly (`hasAESKey=false` in the log).
- **Cancel of an already-completed run returned 2xx**, not the 409 the cleanup helper treats as already-terminal.

### What this feeds

- Q676: the Q628 release path must not ship `result=abandoned` for the winner's own delivery as-is; pick and measure a remedy.
  Done: see [the remedy measurements](#q676--the-remedy-measurements-2026-08-04).
  The Q260 sibling case (`result=skipped` on deduped deliveries while the winner still runs) is **not** covered by this measurement.
  Whether a sibling completion also concludes the whole run is exactly the semantics brokertest's fan-out accounting assumes it does not, and needs its own probe arm ([Q682](../queue/Q682.md)).
- `broker/types.go` and `listener/job.go` comments updated to cite this measurement instead of the unmeasured ~15-minute-timeout rationale.

## Q676 — the remedy measurements (2026-08-04)

The same instrument re-run with `PROBE_ABANDONED_RESULT` selecting a candidate remedy value, plus `PROBE_ABANDONED_RERUN_CHECK=true` for the half a red conclusion alone cannot prove (does `rerun-failed-jobs` get a target?).
Full logs in the session record; fixture runs named per row.

| Run | Result sent | Wire answer | Outcome |
|---|---|---|---|
| [30912707732](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30912707732) | `failed` | **401** `Not authorized for this job` | The value never reached semantics: refused outright on the same call shape (job token, post-acquire) that `abandoned` got a 204 on. |
| [30913319212](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30913319212) | `failed` | **401**, identical body | Reproduced: two independent runs, two plans. (Both `failed` runs had a fan-out sibling on B; the `canceled` run did not. A residual confound, noted rather than chased, since two 401s disqualify the value either way.) |
| [30913691716](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30913691716) | `canceled` | **2xx accepted** | Run concluded **`success`** 1 s later, the same false green as `abandoned`; job record again orphaned `in_progress`. `rerun-failed-jobs` on the concluded run: **403** `This workflow run cannot be retried`. |
| [30914399921](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30914399921) | *(none: acquire, then silence)* | n/a | No redelivery at the ~10-min acquire-lock lapse (the Q247 recycle applies to started jobs, not never-started ones). At **T0+15m14s** the run *and* the job both concluded **`cancelled`**: honest, visible, and no orphaned `in_progress` record, the only arm that leaves none. |

Every accepted completejob value drives the run to `success`, `failed` is refused, and the green conclusion arms no re-run.
**The completejob family cannot produce an honest outcome for an acquired-but-never-run job; saying nothing produces the only honest one**: `cancelled` at GitHub's ~15-minute unstarted-job horizon.

### The remedy

The listener **reports nothing** for its own unrun delivery: the Q628 `completejob(abandoned)` release is removed (it was live **by default**, since `AGC_FANOUT_COMPLETION` defaults on, contrary to what the pre-remedy comments here and in `broker/types.go` claimed), and a reaped-Pending worker's job now ends in the measured told-nothing cancel.
The Q260 sibling fan-out completion is untouched: with the winner's own delivery still open, sibling completions conclude nothing (the 2026-07-04 dogfood re-route #5 measured per-delivery scoping), though the sibling `skipped` value has no live probe measurement.
That arm is Q682.
Making the released job's ending faster than 15 minutes was Q683, measured and shipped 2026-08-05: [the fast-ending measurement](#q683--the-fast-ending-measurement-2026-08-05).

Secondary observations, each load-bearing for the remedy choice:

- **Fan-out is real on this flow.** Both runs delivered a sibling `RunnerJobRequest` (distinct `runner_request_id`) to observer B immediately, then a second delivery to A ~60 s later; A acquired its own.
  The Q645 run's "no fan-out to B" was one draw, not a rule.
  An **unacked** sibling redelivers ~1/s with a fresh broker `MessageID` for as long as it stays unresolved; the probe now filters pre-T0 request ids out of the `REDISPATCHED` verdict, and the Q260 dedup accounting gets live confirmation that siblings are per-delivery assignments.
- **A plain REST cancel does not promptly conclude a run whose only job is an acquired-but-ownerless `in_progress` record.** Run 30912707732: cancel 202-accepted, still `in_progress` ~2.5 min later; `force-cancel` concluded it within ~1 min.
  Run 30913319212: cancel 202, then force-cancel, and the run still took ~3 more minutes to conclude `cancelled`.
  Any REST-cancel remedy inherits this latency, needs `force-cancel` as the effective call, and needs the run id, which the broker delivery does not carry (it is in the worker's job payload, not `RunnerJobRequestBody`).
- **The acquire's `in_progress` job pins the runner record.** Deleting the acquiring runner answers 422 `currently running a job` until the run concludes, so the release path's recycle cannot remove the runner while the orphan lives; measured twice via the probe's own cleanup.

## Q683 — the fast-ending measurement (2026-08-05)

The instrument gained a `PROBE_ABANDONED_FORCECANCEL=true` arm: after the told-nothing walk-away (`result=none`), a **standalone** REST `POST …/actions/runs/{id}/force-cancel` — no prior plain cancel, the call shape a listener remedy would use (the 2026-08-04 force-cancels all followed a 202-accepted plain cancel, and plain cancel alone was measured sluggish against an orphaned acquire).
One run, [31022925992](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31022925992), first attempt (T0 = the acquire + walk-away, 15:59:39 UTC):

| t | Event |
|---|---|
| T0+0.5 s | `force-cancel` → **202 Accepted**, standalone |
| T0+~1 s | The REST **job** is `completed`/`cancelled` (`completed_at` pins it to T0's second) and the **run** is `completed`/`cancelled` (`updated_at` T0+1 s) — before the probe's cleanup plain-cancel at T0+2.4 s, so the conclusion is the force-cancel's own |
| T0+1.1 s | `DELETE` runner A → **204**: the cancelled conclusion unpins the record the orphaned acquire held (no 422) |
| post-run | `rerun-failed-jobs` on the cancelled run → **2xx accepted**, job re-queued — where the false-green `success` conclusion refused it with 403 |

Every property the remedy needs, measured on one run: honest (`cancelled`), ~1 s instead of 15 m 14 s, no orphaned `in_progress` job record, the runner record immediately deletable, and the ending is recoverable via re-run.
The deregistration candidate (Q418's mechanism) is moot for this scenario: the record is deletable only *after* a conclusion, so it cannot cause one.

Shipped the same day: the classic-tier provisioner force-cancels the run (identity from the acquire payload's `github` context, the same `repoInfo()` read the eviction re-run uses) before reporting `abandoned`, counted by `actions_gateway_abandoned_run_force_cancels_total`; the unstarted-job timeout stays the backstop for `identity_unknown`/`error` outcomes.
The recovery re-run arm this measurement arms is Q691, below.

## Q691 — the recovery re-run arm (2026-08-08)

The Q683 ending is honest and recoverable, but recovery was still a human opening the run and clicking re-run.
This arm automates it, and the whole design question is *when*, not *whether*: the run was abandoned because a worker pod sat Pending past `pendingPodDeadline` and the reaper deleted it, so an immediate re-run re-queues the job into the pool that was starved in the first place.
Fire it unconditionally and a capacity shortage becomes a re-run storm that deepens the shortage.

### What "capacity returned" means

The codebase already answers this, for the Q512 capacity-gate latch: a worker pod that **bound to a node** (`PodScheduled=True`) after the decline is the evidence capacity came back, and the binding rather than the phase is the signal, because a bound pod still pulling images proves as much as a running one.
Q691 reuses that definition verbatim, with the abandonment time as the "after".
`podScheduledAt` moves from the controller package to the provisioner as `PodScheduledAt`, so both readers share one answer instead of two.

An entry is registered only on the `cancelled` force-cancel outcome.
That is the state the 2026-08-05 measurement showed accepts `rerun-failed-jobs`; `identity_unknown` has no endpoint to address, and after an `error` the run has not been concluded by us at all.

### The loop budget

The re-run fires through the existing `handleEviction`, with a new `recoveryCauseAbandoned` cause.
That is the load-bearing choice: the budget is `reserveEvictionRetry`, keyed by `run_id` alone and shared with the eviction, preemption, and deletion arms, so a run that is abandoned, re-run, and abandoned again spends one slot per re-run and stops at `spec.maxEvictionRetries`.
A re-run loop is therefore bounded by the same Q106 hard cap that already bounds every other recovery, and a run cannot spend two budgets by being disrupted two ways.

Exhaustion is not silent: it emits `actions_gateway_eviction_retries_exhausted_total{cause="abandoned"}` and the `EvictionRetriesExhausted` warning Event on the owner, the surfaces an operator already alerts on.

The second bound is time.
Capacity may never return (an idle group, or a scheduling constraint no amount of waiting fixes, since `pending_deadline` also covers an unpullable image), so an entry that waits longer than `defaultAbandonedRerunWaitWindow` (30 minutes) is dropped.
Both endings are counted by a new `actions_gateway_abandoned_run_rerun_waits_total{outcome}`: `capacity_returned` when a worker bound and the re-run was handed to the budget, `expired` when it never did.

### Shape

An `AbandonedRerunSweeper` (`manager.Runnable`, per replica, like `EvictionSweeper`) polls the pending set every 30 s.
The registry is in-memory and keyed by owner plus `run_id`, so two abandoned jobs of one run cost one re-run rather than two budget slots, and it is bounded by the 30-minute expiry rather than by a cap.

Classic tier only, matching Q683: the scale-set tier does not force-cancel, so it has no cancelled conclusion to re-run.

### What is tested, and how the bound is proven

`abandoned_rerun_internal_test.go`, unit tier: the sweeper's pass is driven directly against a fake pod client and a counting `rerun-failed-jobs` stub.

The bound is the part worth being careful about.
An aggregate call count cannot show a per-run budget binding, because one run looping can produce every call on its own, so the loop test runs **two distinct runs** through repeated abandon-and-recover cycles and asserts the re-run count **per run id**, taken from the stub's own per-path tally.
Run A exhausting its budget must leave run B's intact.
Deleting the `reserveEvictionRetry` call turns the per-run counts from `maxRetries` into the full cycle count, which is the red the test exists to produce.
