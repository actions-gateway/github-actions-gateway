# Release 2.1 Milestone Definition

> **Status: scoped 2026-11-09; the ledger below says what has landed.** The maintainer made flaky-job detection and automatic retry for more platform failures the `v2.1.0` headline on 2026-11-09.
> Q1236's and Q1317's dogfood readings and Q1318's API readings are taken (2026-11-22), and Q1382's dogfood reading (2026-11-24); the ledger's open rows still gate the tag and nothing rides yet.
> The readings' three refutations, Q1374 to Q1376, gated it from 2026-11-22 because each broke a premise the rest builds on: Q1376 the context Q1315 and Q1316 recognise their classes from, and Q1374 and Q1375 the `trigger="other"` rate this release measures success by, biased in opposite directions; all three are built.
> Q1381 gated it too, settled 2026-11-22, because its re-runs landed in merge-queue runs nothing reads and each spent a `maxEvictionRetries` slot, which is the opposite of what this release is for; it and Q1320 are built, and the case it leaves is Q1391.
> Q1389 gates it too: the out-of-memory class `v2.0.0` shipped can act on the wrong job where a worker dies before its pod names the job it ran, and Q1319 fixes only the re-run half of that, not the raised limit.
> The wrong-job half is read from source, so its first step is reproducing it.
> Q1395 gates it too, settled 2026-11-24: detection could name a platform fault that did not happen, in the release whose headline is telling platform failures from a job's own.
> Q1400 gates it too, settled 2026-11-24, for the same reason in the out-of-memory class `v2.0.0` shipped: it reads a runner killed for its node's memory as one killed at its own limit, so the Event and the raised limit name a cause it did not check.
> That is read from source, so its first step is reproducing it.
> Q1401 gates it too: job-granular re-run (Q1319) recovers less than `v2.0.0` on a fail-fast matrix behind an aggregate job, and this release adds recovery rather than taking any away.
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` ships the first half of automatic recovery: flaky-job detection (Q555), the futility checks on the disruption arms (Q1181), and the first retry class, an out-of-memory kill under right-sizing (Q1183).
Each of those landed ahead of the dogfood readings its design put first, and the classes after the first are designed but not built ([q555-automatic-recovery-design.md](q555-automatic-recovery-design.md)).
This release finishes it: it measures what the shipped half rests on, then adds the classes, so that fewer platform failures need someone to click re-run.

The order inside the release is the design's: the measurements first, because each class and the job-granular re-run are built on their answers.

## What this release must NOT do

**It must not re-run a genuine failure.** The boundary in [troubleshooting.md](../operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do) stands: every class retries only a cause GAG can observe on facts it owns (R9), arrives with a dogfood reproduction (R12), and is punted rather than shipped when no discriminator separates it from a tenant-caused shape.
One known limit stays from `v2.0.0`: beside a memory kill, a job past its `timeout-minutes` is re-run with it where no job ended `failure` on its own, because GitHub's job list spells it as it does a fail-fast sibling ([how](../design/04-operational-flows.md#the-re-run-takes-only-the-killed-jobs-q1319), Q1399).

**It must not ship a blanket opt-in retry.** `spec.flakyRetry` from [appendix G.17](../design/appendix-g-future-enhancements.md#g17-opt-in-auto-retry-for-flaky-jobs-beyond-disruptions) stays unscheduled; the direction is classified platform failures on the existing arms.

## Scope ledger

What the `v2.1.0` tag waits for, per the [scope-ledger convention](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1236 | Measure flaky-job detection on dogfood: result spelling, re-run job IDs (M1, M2, M4) | `2.1-gate` | ✅ measured 2026-11-22; three refuted assumptions filed as Q1374 to Q1376 |
| Q1376 | Record a failed job's context from its terminated pod, not the still-`Running` one (M4) | `2.1-gate` | ✅ landed: the record is amended when the worker finishes |
| Q1375 | Keep jobs that share one `name:` from pairing as a false flake (M2) | `2.1-gate` | ✅ landed: the run's job list tells a re-run from a job of the same name |
| Q1374 | Record a job killed by `timeout-minutes`, which `JobCompleted` reports `canceled` (M1) | `2.1-gate` | ✅ landed: counted once its re-run passes and GitHub's annotation says it exceeded its limit; tested against a fake only |
| Q1317 | Reproduce the out-of-memory retry class on dogfood (M3, R12) | `2.1-gate` | ✅ reproduced 2026-11-22 under `Throughput`: the kill takes the runner, and the class fires and re-runs as built; `Binpack` is Q1382's |
| Q1382 | Reproduce the out-of-memory retry class on a `Binpack` set (R12) | `2.1-gate` | ✅ reproduced 2026-11-24: the request rises with the limit and the re-run succeeds; a raise no node can hold is filed as Q1388 |
| [Q1389](../queue/Q1389.md) | Keep the out-of-memory class from acting before the pod names the job it ran | `2.1-gate` | 🔲 open: reproduce first, since the wrong-job half is read from source |
| Q1320 | Build the `already_rerun`, `run_succeeded` and `worker_unstartable` checks | `2.1-gate` | ✅ landed on the scale-set tier; `worker_unstartable` reads the startup verdict only, not placeability |
| Q1318 | Measure the `superseded` check on a fork PR whose branch is `main` (M5) | `2.1-gate` | ✅ measured 2026-11-22 from the API: the `event` filter separates a fork's `main` from upstream's; a displaced merge-group run is not cancelled, filed as Q1381 |
| Q1381 | Withhold the re-run of a merge-group run displaced from the queue, which GitHub does not cancel | `2.1-gate` | ✅ landed: withheld once the pull request has a newer group, which 5 of 106 measured had as they concluded (the failed ones among 373 runs that never merged); a removed one not queued again is [Q1391](../queue/Q1391.md), not gated |
| Q1319 | Re-run only the classified job, found by its runner's name | `2.1-gate` | ✅ landed: M6 measured 2026-11-24, and the out-of-memory retry re-runs the killed job alone |
| [Q1401](../queue/Q1401.md) | Recover a fail-fast matrix's siblings behind an aggregate job | `2.1-gate` | 🔲 open: Q1319 leaves them cancelled, where `v2.0.0` re-runs them |
| [Q1315](../queue/Q1315.md) | Retry a job failed while its colocated egress proxy pool was unavailable | `2.1-gate` | 🔲 open |
| [Q1316](../queue/Q1316.md) | Retry a job whose Docker-in-Docker sidecar crashed under it | `2.1-gate` | 🔲 open: readings taken 2026-11-24 on a local node, no discriminator found in what GAG reads; dogfood reproduction not run |
| Q1395 | Keep a nested container's out-of-memory kill from reading as a platform fault on the sidecar | `2.1-gate` | ✅ landed: a sidecar's `OOMKilled` is a fault only with exit code 137; not run on Kata |
| [Q1400](../queue/Q1400.md) | Keep a runner killed for its node's memory from being read as killed at its own limit | `2.1-gate` | 🔲 open: read from source; Q1316's dogfood reproduction would produce the kill |
| - | RC validated on dogfood | gates | 🔲 no candidate cut |

**Scoped 2026-11-09, and nothing rides yet.** `v2.1.0` follows `v2.0.0`, so nothing here waits on anything but its own row and the 2.0 tag.
The ledger takes a row for each item that merges onto the released surface before the tag.
