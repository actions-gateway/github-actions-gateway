# Q844: an owed-rerun tombstone that survives an AGC restart

**Status:** done 2026-08-13.
Queue item: Q844.
Origin: [04-operational-flows.md § Why preemption *deletes* rather than *evicts*](../../design/04-operational-flows.md#why-preemption-deletes-rather-than-evicts-and-what-that-costs-us).

## The gap

On the scale-set tier a disrupted worker's recovery evidence **is** the pod, and two of the four disruption causes delete it.

| Cause | What the pod does | Restart-safe |
|---|---|---|
| Kubelet node-pressure eviction | Sits in `PodFailed` until the reaper takes it | Yes; a late scan still finds it |
| kube-scheduler preemption | Condition stamped, then deleted; readable for the termination grace period | **No** |
| External graceful deletion (drain, `kubectl delete pod`) | Terminal phase publishes as the container exits, shortly before the object goes away | **No** |
| Deleted before any container ran | Transient `Failed`-with-mark, then gone | **No** |

`RecoverEvictedScaleSetWorkers` lists worker pods and reads the discriminator off each one, so an AGC that is down for the deletion window sees nothing at all and issues no `rerun-failed-jobs`.
The classic tier has no such window: its provisioning goroutine is already watching the pod and reads the markers off the resolving event, including the informer's delete event.

**Measured against the code, 2026-08-13**, confirming the row's asserted mechanism rather than assuming it:

* `cmd/agc/internal/provisioner/eviction_scaleset.go:87`: the scan's only input is a `List` of pods that still exist.
* `cmd/agc/internal/provisioner/eviction_scaleset.go:241`: `disruptionAwaitingRecovery` reads `pod.Status` and `pod.Annotations`; there is no other source.
* `cmd/agc/internal/controller/runnerset_controller.go:431`: the scan is driven from the reconcile, so it runs only while the AGC process is up.

## Why replay does not cover it, and why the pod cannot be the record

The listener's assignment replay recovers a job **still assigned** at GitHub: its `JobAssigned` is held in the queue and a re-created session re-reads it.
A preempted worker is not that case.
The Q385 SIGTERM relay gets the runner its grace period, the runner reports its own outcome, and the job concludes `failure` at GitHub in a measured 15–26 s, so the assignment is gone and a `JobCompleted` is what replays instead.

What is lost is therefore the **discriminator**, not the run.
GitHub still holds the run and still accepts `rerun-failed-jobs`; nothing that reads the queue afterwards can tell a preemption-induced `failure` from a job that failed on its own.

A finalizer on the worker pod would make the evidence durable, since the object would persist in `Terminating` until the AGC cleared it.
It is deliberately **not** the mechanism.
A finalizer on a worker pod is exactly what makes `kubectl drain` hang, and drain is one of the causes this recovers; the repo's own posture on finalizers over objects other controllers delete is in [appendix-h § H.8](../../design/appendix-h-v2-api-decomposition.md#h8-ownership-gc-and-deletion), which cites stuck-`Terminating` as the recurring failure.
An AGC that stays down would leave one un-collectable worker pod per interrupted job.

## The design

A record of the runs the gateway currently has workers for, persisted where per-job state already crosses a process boundary: the per-`RunnerSet` guard ConfigMap (Q606).

### What is stored

`GuardState` gains an `inFlight` set beside `completed` and `abandoned`: one entry per job this listener provisioned a worker for and has not seen conclude, carrying the run identity `rerun-failed-jobs` addresses a run by:

```
{jobID, owner, repository, runID, provisionedAt}
```

The worker pod's name is deliberately **not** stored: it is derived from `(runnerSetName, jobID)` at a single site (`scaleSetPodName`), so re-deriving it cannot drift from what provisioning created, while a stored copy could.

### Who writes it

The listener's poll goroutine, and only it.
The single-writer invariant the Q606 store rests on is unchanged:

* **Added** in `provisionAssigned`, next to `l.provisioned[jobID] = true`, once `Provision` has returned success.
  An assignment carrying no complete run identity records nothing: there would be no run to re-run.
* **Removed** in `completeJob` (terminal `JobCompleted`) and when a job is concluded gone at GitHub (`abandoned`, Q553).
* **Persisted** through the existing `guardsDirty`/`saveGuards` path, which is write-ahead of every message delete.
  So a removal is durable before the message authorising it is deleted, and an *add* that is lost to a crash degrades to today's behaviour rather than to a wrong one.

### Who reads it, and when

The reconciler, read-only, so no second writer touches the ConfigMap.
The verdict is taken **once per AGC process per `RunnerSet`**, in the existing recovery pass, which already runs *before* the reaper for exactly this reason.

For each stored entry, the worker pod's derived name is looked for in the same `List` the disruption scan already makes:

* **Pod present.** The live path owns it, whatever state it is in.
  No action.
* **Pod absent.** The worker went away while no AGC was watching, and the job never concluded in this gateway's view.
  Hand it to the same `handleEviction` every other cause uses, under a new `cause="vanished"`.

Three properties make once-per-process the right bound, rather than every reconcile:

1. The failure being closed is exclusively "the AGC was absent".
   An entry this process created is covered by the live pod-watch recovery, so re-examining it can only produce false positives.
2. It runs before the listener starts, so the entry is still in the ConfigMap; a `JobCompleted` replayed seconds later would otherwise clear it first, which is precisely what happens on the preemption path.
3. It runs before the reaper, so a terminal pod the reaper is about to collect is still there to vote "not a disruption".

The claim is in-memory and keyed by `RunnerSet`, bounded by the number of sets rather than by uptime.
A scan whose pod `List` failed releases its claim, so an unreadable cluster costs a retry rather than the process's one chance.

The `List` goes through the **uncached** reader, unlike the disruption scan's.
That scan acts on pods it finds, where a cold informer cache costs a deferred action; this one acts on a pod it does *not* find, where a cold cache would read as every worker in the set having been disrupted at once.

### Why a restarted listener does not adopt what it loads

`loadGuards` seeds `completed` and `abandoned` from the store and deliberately ignores `inFlight`.
The reconciler has already adjudicated those entries, ahead of the listener starting, and nothing in the listener can retire one, so carrying them forward would let a *second* restart re-run a run the first one already recovered.

What rebuilds the set instead is the assignment replay Q583 rests on: a job still running holds its `JobAssigned` in the queue, so the new session re-reads it, re-provisions idempotently, and records it afresh.
A job that is over replays its `JobCompleted` rather than its assignment, and is owed nothing.

### What it does not close

**A previous process that reaped a terminal pod and then died before reading that job's completion re-runs a genuinely failed job, once.** Reaching it needs the completion to have gone unread for at least `completedPodTTL` while the listener was polling, and then a kill inside that state.
The re-run draws on the shared per-run `maxEvictionRetries` budget like every other cause, so it cannot loop.
This is the same trade already accepted for an operator's bare `kubectl delete pod` of a running worker, which re-runs the job it interrupted by design.

**A worker deleted before any container ran is re-run rather than force-cancelled first.** The `vanished` path cannot see whether the container started, because that is a fact about the pod, and the pod is gone. `rerun-failed-jobs` against a run with no failed job is refused by GitHub, which the existing error path logs and drops, so the outcome is the pre-Q844 one (a manual re-run) rather than a wrong one.

## Scope

| Area | Change |
|---|---|
| `scalesetlistener` | `GuardState.InFlight` + `InFlightJob`, the add/remove sites, the 24-hour age sweep |
| `provisioner` | `RecoverOrphanedScaleSetWorkers`, `recoveryCauseVanished`, the once-per-process claim, `APIReader` |
| `controller` | Read the stored set in the recovery pass, ahead of the reaper |
| Tests | Listener unit (persist/retire cycle), provisioner unit (present vs absent pod, once-per-process, unreadable cluster), controller unit (the real ConfigMap round-trip), envtest (two manager generations across a real restart) |
| Docs | design/02 (guard ConfigMap contents, Events table), design/04 (the residual paragraphs), observability-metrics, observability-dashboards, upgrade, troubleshooting |

## Status

* 2026-08-13: mechanism measured against the code; design settled; implemented.
  Both halves of every behavioural claim are pinned, and three were checked by deleting the mechanism and requiring red: the pod-presence guard (provisioner unit), the retirement in `completeJob` (listener unit), and the reconciler wiring (envtest).
  The envtest models the restart literally: one manager generation writes the record, it is stopped, the worker is destroyed and its job concluded `failed` at GitHub as the Q385 relay would, and a second generation has only the ConfigMap to go on.
