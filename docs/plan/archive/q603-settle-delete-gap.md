# Q603 — A stop between concluding a job and deleting its message replays the assignment

**Status:** done 2026-08-02.
The graceful-stop half is closed and regression-tested; the hard-kill residual was closed by Q606: the concluded-job guards are persisted to a per-RunnerSet ConfigMap ahead of every delete ([02-architecture.md](../../design/02-architecture.md)).
A third window this doc did not see, a conclusion the listener never read, was closed by Q689 on 2026-08-05; see [What this does not close](#what-this-does-not-close).

Q583 made acking two halves: advance the cursor, then delete the message once every job it names has concluded.
The two halves are not atomic, and nothing carries the second one across a process boundary.
Q603 is that seam.

## The defect

`settle` ([listener.go](../../../cmd/agc/internal/scalesetlistener/listener.go)) marks a job concluded **in memory** — it drops the job from every held message's waiting set.
The message is not deleted there.
`flushDeletes` issues the DELETE, and it is called from the poll loop, so the delete lands some time after the conclusion that authorised it.

Between those two moments the queue still holds a message whose only remaining guard is process-scoped state.
A process that stops in that window leaves the message in the queue; the next process polls from cursor 0, receives it with `provisioned`, `completed`, and `abandoned` all empty, and provisions a worker for a job that is over.
That is the Q583 defect returning through the fix's own seam — which is what the row means by "Q583 narrows it, not closes it".

The row was filed off Q602's flake, and the flake is the measurement: the Q583 regression test stopped its listener on the abandoned counter, which rises at `settle`, and the message was sometimes still undeleted when the process went away.
Q602 fixed the test by waiting for the delete.
Production has no such wait.

### Two windows, not one

**The graceful-stop window is unconditional.** `run` returns on context cancellation — from the top-of-loop check or from `handlePollError` — with `deleteSession` as its only deferred work.
It never flushes.
So whatever settled since the last `flushDeletes` is stranded on *every* rollout, drain, eviction, and scale-down that lands in the window, and nothing brings it back: the cursor has already moved past the message, so only a new session replaying from 0 will ever see it again — which is exactly the harm.

**The in-cycle window is the one the row names.** The loop runs

```
reconcileDeferred → retryDeferred → flushDeletes → GetMessage → handleMessage
```

`abandonDeferredBefore` settles inside `reconcileDeferred`, so its delete is separated from its conclusion by `retryDeferred` — which re-offers every remaining deferred job, walking the runner-name ladder against the network, and which the loop's own comment notes "can hold the loop for a few pollBackoffs".
The abandon path always has deferred jobs by construction, so this is the widest of the in-cycle gaps rather than an unlikely one.
`completeJob` settles inside `handleMessage`, at the end of an iteration, so its delete waits on the next iteration's `reconcileDeferred` (a `RefreshSession` round trip) and `retryDeferred` before it is issued.

## The fix

Both halves are ordering, and neither needs persistence or an unverified backend property.

1. **Flush on loop exit**, before the session is torn down, on a context detached from the cancelled one — rules 1 and 3 of [kubernetes-conventions.md § Graceful shutdown](../../development/kubernetes-conventions.md#graceful-shutdown-sigterm).
   An undeleted message for a concluded job is work the listener owns: GitHub is still holding it, and the next process will act on it.
   The session delete must follow the flush, not precede it — `DeleteMessage` is issued against the session, and the client reports the 404 a dead session would answer as a successful ack.

2. **Flush adjacent to each settle site** rather than once per cycle at a fixed point: after `reconcileDeferred` (where abandoning settles) and after `handleMessage` (where completing settles).
   No network work then separates a conclusion from its delete, and the per-cycle retry of a failed delete is preserved — it gets two attempts per cycle instead of one.

The AGC pod's grace period is 60s ([shared_agc_deployment.go](../../../cmd/gmc/internal/controller/shared_agc_deployment.go)), so a 10s flush budget mirroring `deleteSession`'s fits alongside it with room for the manager's own shutdown.

### What this does not close

**Update 2026-08-05 (Q689):** the two residuals below are both on the far side of a conclusion the listener has already read.
There is a third window before it: the job concludes at GitHub and the process stops before a poll delivers the `JobCompleted`, and it needs no hard kill, since a graceful stop lands in it.
Measured over the 60 stops recorded for Q685, all 4 taken before the completion was read replayed and re-provisioned; none of the 56 taken after it did.
Closed by draining the queue's outstanding conclusions on the way out, ahead of the exit flush ([02-architecture.md](../../design/02-architecture.md)).

A hard kill — SIGKILL at grace expiry, OOM, node loss — between `settle` and a successful DELETE still strands the message, and no ordering can prevent that: the conclusion lives in memory and the ack lives at GitHub, and the two cannot be made atomic from this side.
Closing it needs either persisted guard state or a way for the restarted listener to recognise a stale assignment at intake.
The second is the interesting one and it rests on a backend property nobody has measured — whether the statistics delivered with a replayed message report live assigned-job counts or the counts captured when the message was enqueued.
That is an Investigation-G-shaped live probe, not a code change, so it is filed separately rather than guessed at here.

## Verification

The graceful-stop window is deterministic and testable directly: with the queue's DELETE refused, a settled message is guaranteed to still be pending when the process stops, so the assertion is that stopping attempts the delete at all.
Pre-fix that attempt does not exist.

The end-to-end invariant — stop in the gap, restart, no worker — needs the running loop held out of its own flush while the test stops it.
Parking it in a long poll does that: the stub records a poll on arrival and then holds it, so a poll call appearing after the timeout is raised proves the loop is inside it and cannot flush.

Both are paired with the mechanism-deletion check the repo requires for a causation claim: with the exit flush removed the tests must go red.

## Findings

### The graceful-stop window was the bigger half, and it was unconditional

The row describes a race.
The exit path is not one: `run` had no flush at all, so *every* stop that landed after a conclusion and before the next `flushDeletes` stranded the message, on every rollout, drain, eviction, and scale-down.
That is what the first regression test pins, and it needs no timing at all to observe — with the queue's DELETE refused throughout, the message is certainly still pending when the process stops, so the assertion is simply that a delete is attempted on the way out.
Before the fix the count does not move.

### What shipped

- `flushDeletesOnExit` runs as `run` unwinds, on a detached context bounded by the new `teardownBudget`, ordered before `deleteSession` by defer's LIFO.
  `deleteSession`'s own 10s literal became that constant: they are the same class of call, both spending the same pod grace period, and naming the budget once says so.
- The per-cycle `flushDeletes` moved to sit directly after `reconcileDeferred`, and a second call now follows `handleMessage`.
  Those are the two places a job concludes, so a conclusion is no longer separated from its delete by `retryDeferred`'s re-offers or `reconcileDeferred`'s session refresh.

### Verification

`cmd/agc/internal/scalesetlistener/settlegap_q603_test.go`, three tests:

| Test | Asserts |
|---|---|
| `StopIssuesTheDeleteForAConcludedJob` | a stop attempts the outstanding delete at all |
| `StopBetweenAbandonAndDeleteDoesNotReprovision` | the row's scenario end to end — stop mid-abandon, restart, no worker |
| `StopWithTheDeleteRefusedStillReplays` | with the exit delete refused the restart re-provisions, so the test above is carried by the delete landing and not by something else |

Removing `defer l.flushDeletesOnExit(sess)` turns the first two red on their own assertions and leaves the third green, which is the shape it should have.
The package is green at `-count=6` on the restart and abandon tests together, so the new `parkInPoll` helper is not itself a flake source.

`parkInPoll` is the piece worth remembering.
Stopping a listener *inside* the gap needs the loop held out of its own flush, and the stub's long poll does it: a poll is recorded on arrival and only then held, so the poll count going quiet is the loop being parked rather than the loop being between two polls.
The first attempt waited for two fresh polls instead, which cannot happen — once a poll parks, there is no second one.

### The residual, stated precisely

A hard kill between `settle` and a successful `DELETE` still strands the message.
Two candidate closures, neither built here:

- **Persist the guard state.** Durable and unambiguous.
  Q597 (2026-08-03) removed the size objection — the guards are now retired with the message that assigned their job, so they are bounded by the queue rather than by the process's lifetime — but it sharpened the rest: what would have to be persisted is the guards *and* the pending- message bookkeeping that retires them, since a stored guard with no retirement rule is the unbounded set again, in etcd.
- **Recognise a stale assignment at intake.** A restarted listener could refuse to provision a replayed `JobAssigned` that arrives while the scale set reports no assigned jobs — Q553's rule applied before provisioning instead of only after a provision failure.
  It hangs entirely on whether the statistics delivered with a message report live counts or the counts captured when it was enqueued, which nobody has measured; Investigation G recorded no statistics.
  Guessing wrong here builds a check that silently never fires, or one that refuses real work.

Filed as Q606, closed by persisting the guards: the row's sharper candidate held up, with the write-ahead ordering and the drained-queue retirement rule recorded in [02-architecture.md](../../design/02-architecture.md).
The intake-recognition alternative (and its unmeasured statistics premise) stays unbuilt.
