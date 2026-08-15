# Q766: abandoned-run recovery on the ScaleSet tier

Port the Q683 force-cancel and the Q691 capacity-gated automatic re-run from the classic acquisition tier to the ScaleSet tier, so a worker removed before its container ever ran gets the same one-second `cancelled` ending and the same automatic re-run on both tiers.

Design context: [04-operational-flows.md § Stuck-Pending Worker Pod](../../design/04-operational-flows.md#stuck-pending-worker-pod) and [§ Which disruptions are recovered](../../design/04-operational-flows.md#which-disruptions-are-recovered-and-which-are-not).
The two prior ports of the same shape are Q417 (eviction recovery) and Q443 (the capacity ladder).

## Why it matters now

`v2beta1` is ScaleSet-only and ScaleSet is the default `acquisitionProtocol`, so no v2 tenant has either recovery today.
[Q264](../../STATUS.md#Q264) deletes the classic machinery at `v2.0.0`, which would turn the gap into the silent capability deletion [04-operational-flows.md](../../design/04-operational-flows.md) names, which is the outcome Q417 and Q443 were ported to avoid.

## What the classic tier actually does

Measured from source 2026-08-09.

`provision()` holds a goroutine for the life of the job.
`InformerPodWaiter` resolves that wait from the pod's **informer delete event**, whose payload is the last-known pod object, and stamps `PodOutcome.DeletedBeforeStart` from `!podEverStarted(pod)` ([podwaiter.go:342](../../../cmd/agc/internal/provisioner/podwaiter.go)).
`provision()` then force-cancels the run and registers it for a capacity-gated re-run, using the `owner/repo/run_id` it parsed out of the AcquireJob payload it still holds ([provisioner.go:661](../../../cmd/agc/internal/provisioner/provisioner.go)).

Two properties of that trigger matter for the port:

- **It fires on the AGC's own deletes too.** `DeletedBeforeStart` carries no `AnnotationDeletionReason` exclusion, and the dominant cause is precisely an AGC delete: the reaper's `pending_deadline` reap of a worker that never left `Pending`.
  That is the flow the design doc's Stuck-Pending sequence draws.
- **It is not the `rerun-failed-jobs` path.** A never-run job has no failed job to re-run, which is why the run is force-cancelled *first*; the cancelled conclusion is the state `rerun-failed-jobs` was measured to accept (Q683).

## Why a fourth arm on `disruptionAwaitingRecovery` is the wrong shape

`RecoverEvictedScaleSetWorkers` scans **surviving** pods ([eviction_scaleset.go:75](../../../cmd/agc/internal/provisioner/eviction_scaleset.go)), and each arm of `disruptionAwaitingRecovery` returns a `cause` that feeds `handleEviction`, i.e. `rerun-failed-jobs` directly.
Both facts rule the fourth arm out:

- A pod reaped while `Pending` is **gone** by the time any later scan runs, so a List-based predicate cannot see the common case at all.
- Routing a never-started pod into `handleEviction` is exactly the defect the predicate's own doc comment excludes: it would re-run a job that never ran, without the force-cancel that makes the re-run legal.

The exclusion therefore stays.
What is re-opened is narrower: the never-started shape gets its **own** detection and its **own** action (force-cancel, then register), and the two never overlap.

## The two seams

| Cause on the classic tier | ScaleSet equivalent | Where |
|---|---|---|
| Reaper deletes a stuck-`Pending` worker (`pending_deadline`) | The reaper itself, which holds the pod and stamps its own delete | `reapWorkerPodsByLabel` hook → `RecoverAbandonedScaleSetWorker` |
| External delete of a never-started worker (a drain catching a `Pending` pod) | The transient `Failed` + deletion mark + **no** container exit record the kubelet publishes | `RecoverEvictedScaleSetWorkers` scan, new `abandonedAwaitingRecovery` arm |

The two are disjoint by construction: the reaper stamps `AnnotationDeletionReason` before it deletes, and the scan arm requires `!deletedByAGC(pod)`.
The scan runs before the reaper in the same reconcile, and a still-`Pending` pod matches no arm of either predicate, so nothing double-fires.

Run identity comes from the pod's `actions-gateway.com/run-id` and `actions-gateway.com/repository` annotations, the same `runIdentityFromPod` read the Q417 eviction port already relies on.

### What is deliberately out

- **`completed_pending`** (Q575): the job is already terminal at GitHub, so there is nothing to cancel and a re-run would re-run finished work.
  The reaper's own branch already separates it from `pending_deadline` by the `job-completed-at` stamp, so the exclusion costs nothing.
- **Gateway teardown** (`reapAllWorkerPodsByLabel`): the owning `ActionsGateway` is being deleted, so `Target.Resolve` is about to fail and no later worker pod will ever bind to satisfy the capacity wait.
- **A never-started worker that simply vanishes** with no transient `Failed` publish.
  That is the same inherent residual the doc already records for preemption and drain recovery on this tier: the evidence is the pod, and the delete removes it.

## Scope

1. `pendingAbandonedRerun` carries its tier; `sweepAbandonedReruns` stops hardcoding `evictionTierClassic`.
2. `forceCancelAbandonedRun` takes a tier and labels its metric with it.
3. `RecoverAbandonedScaleSetWorker` — the shared action, gated on the pod carrying `LabelAcquisitionProtocol: ScaleSet`.
4. `reapHooks.recoverAbandoned`, fired after a successful `pending_deadline` delete, wired on the `RunnerSet` reaper only.
5. `abandonedAwaitingRecovery` arm in `RecoverEvictedScaleSetWorkers`.
6. `tier` label on `abandoned_run_force_cancels_total` and `abandoned_run_rerun_waits_total`, matching the eviction metrics Q417 gave the same treatment.
7. Docs: the design boundary table and both prose blocks, the two observability pages, `troubleshooting.md`'s WorkerPodStuckPending runbook and auto-re-run matrix, and an `upgrade.md` migration note (which is what puts the change in the curated release notes, since `operator-caveats-since.sh` reads the `docs/operations/` diff).

## Status

- 2026-08-09: mechanism confirmed from source; plan written; shipped.

### Why this does not touch `features.md`, `release-1.4.md`, or the v2 gap docs

The tier scope those four surfaces state was added by PR #1365, which is still open.
On `main` they make no tier claim at all, so nothing there is falsified by this change and there is nothing to rewrite. #1365 rebases on top of this work and reconciles them in one place, rather than the two branches editing the same prose from opposite directions.
