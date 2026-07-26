# Q435: does a restarted AGC reclaim workers orphaned while it was down?

**Status:** measured 2026-07-26. Answer: **partly, and conditionally.** Three of
the four orphan classes are reclaimed unconditionally. The fourth — a `Running`
worker whose job ended while the AGC was down — is reclaimed **if and only if**
GitHub still redelivers that job's terminal `JobCompleted` to the restarted
AGC's new session. Nothing in the AGC decides whether it does.

The durable-deadline fix for the residual is tracked as
[Q438](../STATUS.md#Q438).

## Why the question was open

The [architecture doc](../design/02-architecture.md) justifies putting the
worker-pod reaper in the reconciler rather than in the session goroutine on the
grounds that it makes cleanup restart-safe — it "also reaps pods orphaned by an
AGC crash." That claim had never been measured against the state a crashed AGC
actually leaves behind.

It became urgent with the Q434 dogfood incident: `stop.sh` scaled the system
pool to 0 with jobs in flight, the tenant AGC went `Pending` with nowhere to
reschedule, and five worker pods kept running on the `workers` pool for 16
hours, pinning their nodes (workers carry
`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` by design, so the
autoscaler would not reclaim the nodes either). 82 spot node-hours were
stranded. Q434 fixed the teardown script, but **the AGC never restarted during
that incident** — it stayed `Pending` throughout — so the incident proved
nothing about the recovery path. Hence: measure it.

Dogfood runs the v2 scale-set tier (`scripts/dogfood/setup.sh` applies
`actions-gateway.com/v2alpha1` `RunnerSet`s), so the incident's five pods were
scale-set workers.

## What the reaper can see

[`reapWorkerPodsByLabel`](../../cmd/agc/internal/controller/runner_shared.go)
gives a pod a reap deadline from two inputs, both read off the pod itself:

| Pod state | Deadline from | Restart-safe? |
|---|---|---|
| Terminal (`Succeeded`/`Failed`/`Unknown`) | `podTerminalTime` + `completedPodTTL` | yes — phase and `finishedAt` are apiserver state |
| `Pending` | `creationTimestamp` + `pendingPodDeadline` | yes — same |
| `Running`, stamped `actions-gateway.com/job-completed-at` | stamp + `completedJobRunningGrace` (5 min, a constant) | yes — the stamp lives on the pod |
| `Running`, unstamped | **none** | the gap |

The unstamped `Running` arm is deliberate: an unstamped pod is read as "its job
is still assigned", and reaping it would kill a live job. The stamp is the only
thing that distinguishes a busy worker from an abandoned one, and its **only**
writer is
[`markJobCompleted`](../../cmd/agc/internal/provisioner/provisioner.go), reached
from `CleanupScaleSetJob` when a live scale-set listener processes the job's
terminal `JobCompleted`. An AGC that was down when the job ended never wrote it.

## Measurement

Tier B (envtest, real apiserver, real Pod watch and `RequeueAfter` loop) in
[`cmd/agc/internal/controller/integration/v2_restart_reclaim_test.go`](../../cmd/agc/internal/controller/integration/v2_restart_reclaim_test.go).
A restart is modelled the only way that matters to the reaper: worker pods
already exist in the apiserver and the process has no in-memory state for any of
them.

### Experiment 1 — which orphan classes a fresh AGC reclaims

`TestV2_RunnerSet_RestartReclaimsOrphanedWorkers` stages all four classes
**before** starting the manager, then starts it.

| Orphan | Result | Reap reason |
|---|---|---|
| Terminal, finished 1 h ago | reclaimed | `completed_ttl` |
| `Pending` past the deadline | reclaimed | `pending_deadline` |
| `Running`, stamped 1 h ago | reclaimed | `orphaned_running` |
| `Running`, unstamped | **retained indefinitely** | — |

The fourth is asserted with `require.Never` while the reaper is demonstrably
live (it has just deleted the other three), and its annotations are re-read
afterwards to confirm no reconcile-path writer backfills the stamp from cluster
state alone. There is none: the reconciler cannot tell an abandoned worker from
a busy one without asking GitHub.

### Experiment 2 — the recovery path, across a real restart

`TestV2_RunnerSet_ScaleSet_RestartReclaimsWorkerOrphanedWhileDown` runs two
managers against one `scalesettest` fake:

1. Manager A registers the scale set, a job is enqueued, A provisions the
   worker, the test drives it to `Running`, and asserts it is unstamped.
2. A is shut down and drained — the AGC process going away mid-flight.
3. The job goes terminal at GitHub **with nothing listening**. This is the
   16-hour window.
4. Manager B starts: new process, new session, and a worker pod it never
   created.

B stamps the pod. The mechanism is that
[`completeJob`](../../cmd/agc/internal/scalesetlistener/listener.go) runs its
reclaim hook on *every* delivery rather than only on jobs the current process
acquired, and the scale set's queue log is not session-scoped — so a re-created
session replays the completion, `markJobCompleted` finds the pod by its
deterministic name, and the ordinary five-minute grace then applies
(`TestV2_RunnerSet_OrphanedRunningPodReaped` already covers the reap itself).

The pod is named by the real provisioner, not by the test, so the test cannot
drift from `scaleSetPodName`. A negative control — the same test with the
completion never delivered — was run and fails, so the pass is not vacuous.

## Findings

1. **The restart-safety claim holds for three of four classes.** Every orphan
   whose deadline is derivable from durable pod state is reclaimed by a fresh
   AGC with no operator action.
2. **The fourth class has exactly one recovery path, and it is external.** An
   unstamped `Running` worker is reclaimed only via a redelivered
   `JobCompleted`. That works — measured end to end — but whether the real
   GitHub queue still holds the completion after a multi-hour outage is
   **not** answerable in envtest.
3. **The dogfood incident is consistent with class 4 and explains itself
   without any bug in the reaper.** The AGC never restarted, so neither the
   reconcile path nor the replay path ever ran. Q434's drain fix removes the
   cause; it does not give the AGC a way to recover if it is down anyway.
4. **The classic tier never stamps at all**, so it has no replay path — but its
   worker runs its job and exits, which lands the pod in a terminal phase that
   `completedPodTTL` reclaims. A classic orphan only persists if its runner
   never terminates.

## Residual, and why it is not fixed here

The honest gap is a `Running` worker orphaned with no completion stamp whose
completion is *not* redelivered. It has no durable deadline of any kind, and no
amount of AGC-side reconciliation can distinguish it from a worker running a
long job — the two are identical in cluster state.

Closing it needs a deadline that does not depend on a live listener, e.g. a
maximum worker lifetime stamped at provision time. That is a real design
decision, not a mechanical fix: a default low enough to have bounded the 16-hour
incident is also low enough to kill a legitimate long job, so the safe default
and the useful one point in opposite directions. Tracked as
[Q438](../STATUS.md#Q438).

Two things an operator needs meanwhile are shipped with this measurement: the
[troubleshooting runbook](../operations/troubleshooting.md) now names the
hand-delete recovery for this shape, and the architecture doc's restart-safety
claim is qualified rather than absolute.

## Open, not measured here

- **Does GitHub redeliver a `JobCompleted` after a multi-hour gap with no
  session?** Tier A / live only. The fake's queue log is deliberately
  session-independent, which is the *permissive* assumption; real retention is
  unknown and could be shorter. If it turns out GitHub does not redeliver, the
  replay path stops being a recovery path at all and [Q438](../STATUS.md#Q438)
  becomes the only one.
