# Release 2.1 Milestone Definition

> **Status: scoped 2026-11-09; the [scope ledger](#scope-ledger) says what has landed and what still gates the tag.** The maintainer made flaky-job detection and automatic retry for more platform failures the `v2.1.0` headline on 2026-11-09.
> A row added since then gates for the reason [under its ID](#why-the-rows-added-since-gate).
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` ships the first half of automatic recovery: flaky-job detection (Q555), the futility checks on the disruption arms (Q1181), and the first retry class, an out-of-memory kill under right-sizing (Q1183).
Each of those landed ahead of the dogfood readings its design put first, and the classes after the first are designed but not built ([q555-automatic-recovery-design.md](q555-automatic-recovery-design.md)).
This release finishes it: it measures what the shipped half rests on, and was to add a class where something GAG owns separates it from a tenant's own failure, so that fewer platform failures need someone to click re-run.
Neither of the two classes it named is buildable on what has been read, each for the reason [below](#explicitly-out-of-scope), so both are punted and it ships no new retry class.
What it ships is [in the ledger](#scope-ledger), among it job-granular re-run (Q1319), the fix that keeps the out-of-memory class off a job whose pod has not named it (Q1389), the detection readings and their corrections, and the reporter that says which memory limit ran out (Q1403).

The order inside the release is the design's: the measurements first, because each class and the job-granular re-run are built on their answers.

## What this release must NOT do

**It must not re-run a genuine failure.** The boundary in [troubleshooting.md](../operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do) stands: every class retries only a cause GAG can observe on facts it owns (R9), arrives with a dogfood reproduction (R12), and is punted rather than shipped when no discriminator separates it from a tenant-caused shape.
Two known limits stay from `v2.0.0`.
Beside a memory kill, a job past its `timeout-minutes` is re-run with it where no job ended `failure` on its own, because GitHub's job list spells it as it does a fail-fast sibling ([how](../design/04-operational-flows.md#the-re-run-takes-only-the-killed-jobs-q1319), Q1399).
Beside any other disruption, a job that failed on its own is re-run with the disrupted one, because those arms call `rerun-failed-jobs` and only the out-of-memory class re-runs the killed job alone; the boundary says so since Q1434, and the fix is [Q1429](../queue/Q1429.md).

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
| Q1389 | Keep the out-of-memory class from acting before the pod names the job it ran | `2.1-gate` | ✅ landed: the class waits up to 15 minutes for the job's start, and gives up with no raise and no re-run |
| Q1320 | Build the `already_rerun`, `run_succeeded` and `worker_unstartable` checks | `2.1-gate` | ✅ landed on the scale-set tier; `worker_unstartable` reads the startup verdict only, not placeability |
| Q1318 | Measure the `superseded` check on a fork PR whose branch is `main` (M5) | `2.1-gate` | ✅ measured 2026-11-22 from the API: the `event` filter separates a fork's `main` from upstream's; a displaced merge-group run is not cancelled, filed as Q1381 |
| Q1381 | Withhold the re-run of a merge-group run displaced from the queue, which GitHub does not cancel | `2.1-gate` | ✅ landed: withheld once the pull request has a newer group, which 5 of 106 measured had as they concluded (the failed ones among 373 runs that never merged); a removed one not queued again is [Q1391](../queue/Q1391.md), not gated |
| Q1319 | Re-run only the classified job, found by its runner's name | `2.1-gate` | ✅ landed: M6 measured 2026-11-24, and the out-of-memory retry re-runs the killed job alone |
| Q1401 | Recover a fail-fast matrix's siblings behind an aggregate job | `2.1-gate` | ✅ landed for one matrix and one aggregate, on a second attempt and a second slot of `maxEvictionRetries`; what is left is in the upgrade notes |
| [Q1315](../queue/Q1315.md) | Retry a job failed while its colocated egress proxy pool was unavailable | rides | ⤴ punted: [explicitly out of scope](#explicitly-out-of-scope) |
| [Q1316](../queue/Q1316.md) | Retry a job whose Docker-in-Docker sidecar crashed under it | rides | ⤴ punted: [explicitly out of scope](#explicitly-out-of-scope) |
| Q1395 | Keep a nested container's out-of-memory kill from reading as a platform fault on the sidecar | `2.1-gate` | ✅ landed: a sidecar's `OOMKilled` is a fault only with exit code 137; the status it is keyed on was read under Kata on 2026-11-25, `OOMKilled`, 143, after each of two nested kills, and the rule is unit-tested and not run on a cluster |
| Q1400 | Keep a runner killed for its node's memory from being read as killed at its own limit | `2.1-gate` | ✅ landed as text: the Events name the worker, its node, its memory request and its limit, and no cause; nothing the AGC reads separates the two ([what each fact says](../design/04-operational-flows.md#out-of-memory-under-right-sizing-the-first-retry-class-scale-set-q1183)) |
| Q1403 | Say which memory limit killed a container, with a node reporter | `2.1-gate` | ✅ landed: [a node reporter](q1403-oom-kill-reporter.md) writes the limit as a condition and an Event, and the failed job's record names it; CI's kind lane holds one kill at each of four limits to both, and the operator's page states what GKE has not shown; not whose memory it was |
| [Q1406](../queue/Q1406.md) | Act on the limit a killed runner's condition names | rides | 🔲 open |
| [Q1407](../queue/Q1407.md) | Read the kernel log beside the counters, as a reporter option | rides | 🔲 open |
| Q1434 | Say that a disruption re-run takes the run's own failures with it | `2.1-gate` | ✅ landed: the runbook's boundary, the README and the comparison page name the limit; re-running the disrupted job alone is [Q1429](../queue/Q1429.md), not gated |
| - | RC validated on dogfood | gates | 🔲 no candidate cut |

**Scoped 2026-11-09.
Q1406 and Q1407 ride; the `rides` cells on Q1315 and Q1316 mark punted rows.** `v2.1.0` follows `v2.0.0`, so nothing here waits on anything but its own row and the 2.0 tag.
The ledger takes a row for each item that merges onto the released surface before the tag.

## Why the rows added since gate

One heading per row or set, in ID order.

### Q1374 to Q1376

Gated from 2026-11-22.
They are the readings' three refutations, and each broke a premise the rest builds on: Q1376 the context Q1315 and Q1316 recognise their classes from, and Q1374 and Q1375 the `trigger="other"` rate this release measures success by, biased in opposite directions.

### Q1381

Settled 2026-11-22.
Its re-runs landed in merge-queue runs nothing reads, and each spent a `maxEvictionRetries` slot, which is the opposite of what this release is for.
The case it leaves is [Q1391](../queue/Q1391.md).

### Q1389

The out-of-memory class `v2.0.0` shipped could act on the wrong job where a worker died before its pod named the job it ran, reproduced in a unit-test probe and fixed on 2026-11-24.

### Q1395

Settled 2026-11-24.
Detection could name a platform fault that did not happen, in the release whose headline is telling platform failures from a job's own.

### Q1400

Settled 2026-11-24.
The out-of-memory class `v2.0.0` shipped could name a cause it did not check, in the release whose headline is telling platform failures from a job's own: its Events named the runner's own limit for a kill the node could have made.
They name no cause now, and what the class re-runs, raises and withholds is unchanged; a kill at `kubepods`' limit leaves the same status, reproduced on GKE on 2026-11-25, and a node-wide kill has not been reproduced.

### Q1401

Job-granular re-run (Q1319) recovered less than `v2.0.0` on a fail-fast matrix behind an aggregate job, and recovering less is the opposite of what this release is for.
What it still recovers less of is [in the upgrade notes](../operations/upgrade.md#non-breaking-an-out-of-memory-retry-re-runs-the-killed-job-not-every-failed-job-of-its-run).

### Q1403

Naming no cause leaves the operator without one, and [its design](q1403-oom-kill-reporter.md) reads which limit ran out on the node.

### Q1434

Gated 2026-11-27.
The boundary this release must not cross was stated as holding with one limit, and an audit read a second from source: every disruption arm but the out-of-memory class re-runs each failed job of the run, a job's own failure among them.
The behaviour is older than `v2.0.0` and this release does not change it; what gated is the statement, in the release whose headline is telling platform failures from a job's own.

## Explicitly out of scope

| Q-ID | Item | Why it is out |
|---|---|---|
| [Q1315](../queue/Q1315.md) | Retry a job failed while its colocated egress proxy pool was unavailable | Nothing read so far tells a job the outage failed from one that failed for its own reason during it (reasoned), so the class could re-run genuine failures, which this release must not do. The designed reading also misses an outage the worker outlives and that is over when the completion arrives ([the readings](q555-automatic-recovery-design.md#readings-for-the-egress-proxy-class-q1315), 2026-11-24, kind), and how often a pool loses every replica is unmeasured. The row keeps its place in the queue. |
| [Q1316](../queue/Q1316.md) | Retry a job whose Docker-in-Docker sidecar crashed under it | A class needs a platform cause that kills the sidecar and leaves the runner alive to fail the job, and none was found: [the second of two dogfood runs](q1316-dogfood-reproduction.md#what-the-second-run-settles), on 2026-11-25, filled a node to `kubepods`' limit and killed the runner, in four kills of four, and a class without it would retry jobs that failed for their own reason, which this release must not do. A node-wide kill's order is computed and not read, and how often a sidecar crashes first is unmeasured. The row is deferred, with the trigger that would revive it. |
