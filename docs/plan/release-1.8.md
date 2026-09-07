# Release 1.8 Milestone Definition

> **Status: scoped 2026-09-07; the gating row is closed, no candidate cut.** The one gating row, Q1029, the scale-set drain recovery that was lost when no reconcile started inside a terminating worker's window, closed the same day: recovery now runs off the worker-pod watch event ([below](#the-gating-row-q1029)).
> Three rows ride without gating: the two v2 GA soak readings, [Q1059](../queue/Q1059.md) and [Q1060](../queue/Q1060.md), and the Phase 2 alias decision, [Q452](../queue/Q452.md).
> The bump is measured rather than assumed: `semver-floor.sh v1.7.0` read 44 commits and **FLOOR: NONE** on 2026-09-07, with seven `feat`/`fix` subjects withheld because they ship in no image and no chart.
> Q1029's fix raises the floor to PATCH, and the release is a MINOR only if a shipped feature lands beside it, so the version this doc names is provisional until the floor says otherwise.

## Why this is a release rather than a row that lands whenever

The [release ladder](release-ladder.md) reads 1.7 → 2.0, and 2.0 is [parked on a soak](v2-ga.md#phase-1--the-soak-what-well-validated-means) whose evidence nobody has gathered.
Read on 2026-09-07: criterion 1 has elapsed on the record rather than by re-derivation here, since `v1.4.0` through `v1.7.0` all shipped on `v2beta1` and each release's pre-flight API review returned *ship as-is* over additive surface ([1.4](release-1.4.md#pre-flight-the-api-surface-this-tag-publishes), [1.5](release-1.5.md#pre-flight-the-api-surface-this-tag-publishes), [1.6](release-1.6.md), [1.7](release-1.7.md#pre-flight-verdicts)); criterion 2 is unmet on one kind, since the dogfood overlays and `scripts/dogfood/setup.sh` between them apply `ActionsGateway`, `RunnerSet`, `ClusterRunnerTemplate` and `RunnerTemplate` while `setup.sh` deliberately creates no `EgressProxy` on that cluster, and the e2e lane that does exercise one runs on a kind cluster; criterion 3 has no recorded round-trip across the served versions, the nearest reading being [Q415](archive/q415-migrate-dogfood-validation.md)'s live migration, which drove the webhook on one kind in one direction.
So the GA rung cannot be labelled without publishing a commitment on unmeasured criteria, and the roadmap's near-term section has been empty since 2026-08-29.

1.8 is the release that gathers the evidence, on the venue that produces it: a release candidate books the dogfood window criterion 2 needs, criterion 3 can be read on the same cluster at any time, and the same window is the one three rows have waited for since 1.7 ([Q1038](../queue/Q1038.md), [Q1048](../queue/Q1048.md), [Q1039](../queue/Q1039.md)).
The theme is v2 GA readiness; what makes it a release an operator upgrades for is the gating row.

## The gating row: Q1029

Q1029 was a measured product defect on the scale-set tier.
`RecoverEvictedScaleSetWorkers` lists from the informer cache at the top of a `RunnerSet` reconcile, so a gracefully deleted worker was judged only if a reconcile began in the seconds between the kubelet publishing the terminal phase and removing the object.
When none did, the job was silently never re-run.
The row carried three CI sightings with the AGC log each captured, one of them the control that recovered because its reconcile loop happened to turn over inside the window.

**Closed 2026-09-07.** The reconciler's worker-pod watch now hands each phase-changing or newly preempted scale-set worker straight to the same judge and the same optimistic-lock claim the scan uses, so the claim no longer waits on the reconcile queue; the scan stays for what no event reaches ([04-operational-flows.md](../design/04-operational-flows.md#detecting-a-disruption-is-not-the-same-as-claiming-it)).
The deletion-proof is at the envtest tier: `TestAGC_Drain_ScaleSetWorkerRecovers_WhileTheReconcileQueueIsHeld` parks the controller's only reconcile worker in a second set's listener bootstrap for the whole drain and asserts the claim lands while the reconcile count does not move, and it went red on a build where the watch path was inert.
The e2e drain spec is unchanged: it already re-stages rather than fails on a missed window ([q549-scaleset-rerun-flake.md](q549-scaleset-rerun-flake.md#mode-c-closed-2026-09-07-recovery-runs-off-the-worker-pod-watch)), so a stricter e2e assertion is a call this plan still owes an answer on.

It gates because the exposure is an operator's, not the e2e venue's: a node drain terminates many workers at once, so one missed window there is many lost jobs.
The design already records that this arm cannot be made restart-safe ([04-operational-flows.md](../design/04-operational-flows.md#detecting-a-disruption-is-not-the-same-as-claiming-it)), and the row argues that Q844's orphan recovery does not cover a worker the scan simply missed.

**Why the gap opened was unverified**, and the row said so: the reconciler runs at `MaxConcurrentReconciles: 1` and the listener bootstrap does DNS and an HTTP POST inside `Reconcile`, which was a hypothesis read off timestamps.
What the fix established is narrower than a duration: on the vendored controller-runtime the pod's own phase-change event moves a waiting key to ready at once, so the 8-second gap held a reconcile in flight rather than a backoff, and its duration is still not measured directly ([the reading](q549-scaleset-rerun-flake.md#mode-c-closed-2026-09-07-recovery-runs-off-the-worker-pod-watch)).
The fix does not depend on which step inside it was slow.

## What rides: the soak readings and the alias decision

| Row | What it delivers | Why it rides rather than gates |
|---|---|---|
| [Q1059](../queue/Q1059.md) | Every `v2beta1` kind applied and reconciled on the dogfood cluster, recorded against criterion 2 | A soak reading closes when the evidence exists; a tag cannot wait on a measurement that may come back negative |
| [Q1060](../queue/Q1060.md) | The conversion webhook round-tripped over real objects, including a `v1alpha1` object such as the gateway `deploy/dogfood-migrate` applies, recorded against criterion 3 | Same |
| [Q452](../queue/Q452.md) | Whether GA `v2` defines `CiliumFQDN`/`CalicoFQDN`, written into [v2-ga.md § Phase 2](v2-ga.md#phase-2--the-graduation-hop) | A design decision, not a shipped change; deciding it here lets the hop start without one pending |

A reading that comes back negative is the release working: it names the shape fix `v2beta1` still needs, which resets the soak clock and is exactly what GA is gated on finding first.

## Scope ledger

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1029 | Drain recovery lost when no reconcile starts inside the window | `1.8-gate` | ✅ closed 2026-09-07 |
| [Q1059](../queue/Q1059.md) | Every `v2beta1` kind on the dogfood cluster (soak criterion 2) | rides | 🔲 open |
| [Q1060](../queue/Q1060.md) | Conversion round-trips on real dogfood objects (soak criterion 3) | rides | 🔲 open |
| [Q452](../queue/Q452.md) | GA `v2` and the deprecated FQDN aliases | rides | 🔲 open |
| — | RC validated on dogfood | gates | 🔲 no candidate cut |

## Explicitly out of scope

- **[Q413](../queue/Q413.md) itself, and 2.0's coupled removals** ([Q273](../queue/Q273.md), [Q264](../queue/Q264.md)).
  Q413 stays parked until both soak readings exist and read clean; this release supplies the readings and does not pre-empt the verdict.
- **The `feature` rows in the ready queue.** Q988 (the registry read behind `RunnerVersionTooOld`; its row is closed, so it is no longer linked) landed after this was scoped and rides, and Q725 and [Q1011](../queue/Q1011.md) are unrelated to the theme.
  Any of them landing before the tag rides in the floor's reading of the window, with no label.
- **The proxy-hardening cluster** and everything else the ladder punts past 2.0, unchanged.

## Definition of done

1. ✅ **Q1029 closed** (2026-09-07), with the queue mechanism established before the fix, the reconcile's duration left unmeasured and said so, and an envtest assertion that fails when the watch-path recovery is inert; the e2e assertion the criterion asked for is still open, per [the gating row](#the-gating-row-q1029).
2. **Criterion 2 and criterion 3 have a recorded reading** in [v2-ga.md](v2-ga.md)'s Phase 1 table, positive or negative, each naming the candidate window it was taken in.
3. **Q452 decided** in the plan, with the losing option's cost recorded beside it.
4. **The three dogfood-window rows** get their window from the candidate: [Q1038](../queue/Q1038.md)'s `mirror-timing` probe, [Q1048](../queue/Q1048.md)'s mirror client census, and [Q1039](../queue/Q1039.md)'s shared-tenants topology.
5. **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.7.0..<rc commit>`, expecting no change: nothing in scope touches the CRDs, so anything it reports is a finding.
6. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Critical path

Q1029's measurement → its fix (done) → a candidate.
The soak readings and the alias decision run beside it and need the candidate's window, not each other.
The window is the schedule risk, as it was in 1.7: a reading whose venue is a booked cluster run cannot be compressed the way a code change can.

## Pre-flight verdicts

None yet.
Each verdict names the commit it is measured at ([release.md](../operations/release.md#1-pre-flight)).

## Candidate validation

No candidate cut.
