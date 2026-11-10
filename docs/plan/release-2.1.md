# Release 2.1 Milestone Definition

> **Status: scoped 2026-11-09, nothing landed.** The maintainer made flaky-job detection and automatic retry for more platform failures the `v2.1.0` headline on 2026-11-09.
> Seven rows gate the tag and nothing rides yet.
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` ships the first half of automatic recovery: flaky-job detection (Q555), the futility checks on the disruption arms (Q1181), and the first retry class, an out-of-memory kill under right-sizing (Q1183).
Each of those landed ahead of the dogfood readings its design put first, and the classes after the first are designed but not built ([q555-automatic-recovery-design.md](q555-automatic-recovery-design.md)).
This release finishes it: it measures what the shipped half rests on, then adds the classes, so that fewer platform failures need someone to click re-run.

The order inside the release is the design's: the measurements first, because each class and the job-granular re-run are built on their answers.

## What this release must NOT do

**It must not re-run a genuine failure.** The boundary in [troubleshooting.md](../operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do) stands: every class retries only a cause GAG can observe on facts it owns (R9), arrives with a dogfood reproduction (R12), and is punted rather than shipped when no discriminator separates it from a tenant-caused shape.

**It must not ship a blanket opt-in retry.** `spec.flakyRetry` from [appendix G.17](../design/appendix-g-future-enhancements.md#g17-opt-in-auto-retry-for-flaky-jobs-beyond-disruptions) stays unscheduled; the direction is classified platform failures on the existing arms.

## Scope ledger

What the `v2.1.0` tag waits for, per the [scope-ledger convention](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| [Q1236](../queue/Q1236.md) | Measure flaky-job detection on dogfood: result spelling, re-run job IDs (M1, M2, M4) | `2.1-gate` | 🔲 open |
| [Q1317](../queue/Q1317.md) | Reproduce the out-of-memory retry class on dogfood (M3, R12) | `2.1-gate` | 🔲 open |
| [Q1320](../queue/Q1320.md) | Build the `already_rerun`, `run_succeeded` and `worker_unstartable` checks | `2.1-gate` | 🔲 open |
| [Q1318](../queue/Q1318.md) | Measure the `superseded` check on a fork PR whose branch is `main` (M5) | `2.1-gate` | 🔲 open |
| [Q1319](../queue/Q1319.md) | Re-run only the classified job once job names are shown to match (M6) | `2.1-gate` | 🔲 open |
| [Q1315](../queue/Q1315.md) | Retry a job failed while its colocated egress proxy pool was unavailable | `2.1-gate` | 🔲 open |
| [Q1316](../queue/Q1316.md) | Retry a job whose Docker-in-Docker sidecar crashed under it | `2.1-gate` | 🔲 open: the likeliest punt, since its discriminator may not exist |
| - | RC validated on dogfood | gates | 🔲 no candidate cut |

**Scoped 2026-11-09, and nothing rides yet.** `v2.1.0` follows `v2.0.0`, so nothing here waits on anything but its own row and the 2.0 tag.
The ledger takes a row for each item that merges onto the released surface before the tag.
