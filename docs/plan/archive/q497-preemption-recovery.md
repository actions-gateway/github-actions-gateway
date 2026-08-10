# Q497 — recover preempted workers off `PreemptionByScheduler`

Close the preemption slice of the graceful-removal recovery gap: when kube-scheduler preempts a worker pod to make room for a higher-priority tier, re-run the displaced workflow run automatically, on both acquisition tiers.

## Why this is closable now, and why only this slice

[Q423 measured](../eviction-oversubscription-validation.md#result-measured-2026-07-29-preemption-is-not-eviction) that a `priorityTiers` preemption reaches no recovery at all.
The scheduler removes its victim by **deleting** it, so the pod never carries the `PodFailed`/`Evicted` shape both tiers key on, and the displaced job is left needing a manual re-run.

Two findings from that experiment decide the design here:

1. **The terminal phase cannot discriminate.** A preempted worker lands in `Pending`, `Succeeded` or `Failed` depending on what its container was doing and what it exited with.
   No phase/reason pair separates a disruption from an ordinary outcome.
2. **The scheduler leaves an unambiguous marker.** The victim carries a `DisruptionTarget` condition with reason `PreemptionByScheduler`.
   Only kube-scheduler writes it — not a human `kubectl delete pod`, not a drain, not a job failing on its own.

That second point is what let this slice close first.
The *drain / plain delete* slice must key on `deletionTimestamp`, which a human cancel might also set, so it could not be switched on without measuring whether a cancel is distinguishable — the risk being a re-run of a run an operator deliberately stopped. `PreemptionByScheduler` carries no such ambiguity and needed no such measurement, so this plan deliberately covers **only** the preemption cause and leaves the rest of the graceful-removal path exactly as it is.

(That measurement landed the same day, while this work was in flight: a cancelled run's worker publishes no deletion mark, so the drain slice is decided as well; Q502 carried its implementation, since shipped ([q459-drained-worker-recovery.md](q459-drained-worker-recovery.md)).
It did not change anything here — the two slices key on different signals and ship independently — but it does mean the "stays open" framing this plan was written against is now only true of the code, not of the decision.)

## Is a re-run the right answer here?

The existing exclusion comment on `evictedAwaitingRecovery` argues that graceful deletion must not be recovered, because the Q385 SIGTERM relay lets the runner report its own outcome and a re-run would double-report.
That argument does not carry to preemption:

- The relay makes the job **conclude** at GitHub; it does not make the job **succeed**.
  Q459 measured the conclusion at live-GitHub — `failure` in 15–26s, and `rerun-failed-jobs` accepted with `201`.
  So the run really is left failed, and re-running it is the intended repair rather than a duplicate report.
- `rerun-failed-jobs` re-runs only the *failed* jobs of a run.
  A run whose jobs all finished before the preemption landed has nothing to re-run and GitHub rejects the call, which the existing error path already logs and drops.

The retry budget is unchanged and shared: `maxEvictionRetries` remains a hard lifetime cap per `run_id` across both tiers **and** both causes together, so a run that is alternately evicted and preempted cannot spend two budgets.

## Design

One new discriminator, wired into the two detection mechanisms that already exist — mirroring exactly how eviction is split, for the same reason the split exists (the classic goroutine holds the payload; the scale-set tier has neither goroutine nor payload, so it recovers from the owning reconciler off pod annotations).

### Shared

- `preemption.go`: `preemptedByScheduler(pod)` — true when the pod carries `DisruptionTarget=True` with reason `corev1.PodReasonPreemptionByScheduler`.
- `handleEviction` gains a `cause` argument (`eviction` | `preemption`) that flows into the log line, the Kubernetes Event, and a new `cause` label on the eviction counters.
  The budget itself is untouched — it is keyed by `run_id` alone.

### Classic tier

`provision()` already holds a goroutine on the worker pod through `waitForCompletion`.
The condition is set by the scheduler *before* the delete, so the pod object that resolves the wait — whether via a terminal-phase event or via the informer's delete event, which carries the last-known pod — still carries it.

`PodWaiter.WaitForCompletion` therefore returns a `PodOutcome` struct rather than a `(phase, reason)` pair, so the preemption marker travels with the phase it was observed alongside.
Step 7 of `provision()` branches on eviction first, then preemption.

### Scale-set tier

`RecoverEvictedScaleSetWorkers` already scans this owner's worker pods every reconcile and claims each recoverable one under an optimistic lock.
The scan's filter widens from "`PodFailed`/`Evicted` and unhandled" to "unhandled **and** (evicted **or** preempted)".
Claim, identity lookup, at-most-once semantics and the reaper ordering are all reused unchanged.

**One new requirement: the reconcile must happen while the victim still exists.** Unlike an evicted pod — which sits in `PodFailed` until the reaper takes it — a preempted pod is being deleted, and is readable only for its termination grace period (30s by default).
The worker-pod watch predicate currently admits an update **only on a phase change**, and a preemption of a `Pending` victim changes no phase, so today's predicate would enqueue nothing until the delete event, by which point the pod is gone from the cache.

The predicate therefore also admits an update where the pod **newly becomes** a preemption victim.
That is a strictly additive edge — it fires at most once per pod, only for pods carrying the worker label.

### The residual, stated plainly

Scale-set recovery of a preemption is **not** restart-safe, and cannot be made so: the evidence is the pod, and the scheduler deletes it.
An AGC that is down for the whole grace period loses the marker and the displaced run needs a manual re-run.
Eviction recovery keeps its restart-safety (an `Evicted` pod persists until reaped).
This is a property of the signal, not of the implementation, and is documented rather than worked around.

## Scope

| # | Change | Files |
|---|---|---|
| 1 | `preemptedByScheduler` detector + `cause` vocabulary | `cmd/agc/internal/provisioner/preemption.go` |
| 2 | `PodOutcome` on the waiter; carry the marker through both resolve paths and the poll fallback | `podwaiter.go`, `completion.go` |
| 3 | Classic step 7 branches on preemption | `provisioner.go` |
| 4 | Scale-set scan admits preempted pods | `eviction_scaleset.go` |
| 5 | `cause` label on the eviction counters | `runnercore/metrics.go`, `eviction.go` |
| 6 | Worker-pod predicate admits the preemption edge | `controller/runner_shared.go` |
| 7 | Tests: unit (detector, waiter, both tiers), envtest (scale-set scan + predicate) | `*_internal_test.go`, `controller/integration/` |
| 8 | Flip the fake-GitHub canary: it asserts **0** reruns today, by design | `cmd/gmc/test/e2e/worker_preemption_test.go` |
| 9 | Docs | see below |

Out of scope, deliberately: the drain / plain-delete slice (Q459), and re-measuring the wrapper's SIGTERM relay against real GitHub (inherited from Q459's live-GitHub result, as the experiment's "what is not measured here" already records).

## Docs to update

- `docs/design/04-operational-flows.md` §"Worker Pod Eviction and Auto-Retry" — the deletion-is-excluded paragraph now has a carve-out; the Q423 paragraph's verdict moves from "no re-run fires" to "recovered as of Q497".
- `docs/design/01-executive-summary.md` — the oversubscription claim's *safety* half is restored for preemption (three bullets plus the two "what the displaced job costs" paragraphs).
- `README.md` — the `priorityTiers` and eviction-retry bullets.
- `docs/operations/troubleshooting.md` — the drain/preempt section splits: preemption recovers, drain still does not.
- `docs/operations/observability-metrics.md`, `-dashboards.md` — the `cause` label.
- `docs/plan/eviction-oversubscription-validation.md` — finding 2 gets its outcome.

## Status

**Done 2026-07-29.** Every scope row shipped.
Verified:

| Tier | What it pins | Result |
|---|---|---|
| Unit — `preemption_internal_test.go` | The discriminator's full triple, the scan filter's positive *and* negative rows, scale-set recovery, at-most-once | pass |
| Unit — `provisioner_test.go` | Classic recovery on a victim that exits `Succeeded` (so the phase provably is not what drives it); an ordinary `PodFailed` is not re-run | pass |
| Unit — `podwaiter_internal_test.go` | The marker survives both resolve paths, including the informer delete event and its tombstone | pass |
| Unit — `runnergroup_podwatch_internal_test.go` | The predicate admits the preemption edge exactly once, and still gates on the worker label | pass |
| envtest — `TestAGC_Preemption_ScaleSetWorker_IsRecovered` | Real apiserver: watch → scan → optimistic-lock claim → rerun, at most once. Its twin `TestAGC_Drain_ScaleSetWorkerEviction_DoesNotRecover` differs only in the condition's `reason` and must still not recover | pass (`make -C cmd/agc test-integration`, and re-run individually with `-v` to confirm it executed rather than skipped) |
| fake-GitHub — `E2E_AGC_PreemptedWorkerIsRecovered` | A real kube-scheduler preemption of a real gateway worker, on a real cluster | **pass** — CI run 30473065608, both CNI legs. The spec was flipped from "no rerun" to "exactly one rerun"; its never-`Evicted` and marker-published assertions still hold, so recovery is reached by the scheduler's condition and not by the pod taking the kubelet shape |

The e2e is what closes the argument.
Every other tier stamps the `DisruptionTarget` condition by hand, so only this one converts "the AGC recovers a pod carrying this condition" into "the AGC recovers a real preemption" — it is the sole venue with a real kube-scheduler.
It also found [Q504](../../STATUS.md) on its first run: the rerun call ignored `GITHUB_API_BASE_URL` and was refused a `401` by `api.github.com`, so recovery had never been able to work on GHES either.
That is the value of asserting a *successful* re-run rather than an absence — every prior spec on this path asserted absence, which passes whether the call goes nowhere or to the wrong host.

Metric labels changed (`cause` on three counters); the migration note is in [observability-metrics.md](../../operations/observability-metrics.md#breaking-observability-changes-q417).
