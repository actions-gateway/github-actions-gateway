# Q583 — An AGC restart replays the queue and provisions workers for jobs long gone

**Status:** done 2026-08-01.
Measured, then fixed: the replay is real, `DeleteMessage` works, deleting prunes the queue, and the listener now issues the delete half of its ack once every job in a message has concluded.
The rc.4 dogfood gate was never needed.
The residual — unpruned process-scoped guard sets — was filed as Q597 and closed 2026-08-03; see [What shipped](#what-shipped).

Q583 was filed with an instruction to measure at the rc.4 gate before building, because the row's mechanism was asserted rather than observed.
That instruction is discharged here: the measurement was already in the repo, twice, and the residual unknown is about the **fix**, not the defect.

## The defect

The scale-set listener acks by advancing a cursor and nothing else. `advanceCursor` ([listener.go](../../../cmd/agc/internal/scalesetlistener/listener.go)) moves an in-memory `lastMessageID`; no code path in `scalesetlistener` calls `Client.DeleteMessage`.
The queue log is scale-set-scoped, so it outlives both the session and the process, and a new session polls from cursor 0.

What makes the replay harmful rather than merely redundant is that the guards against it are process-scoped maps — `provisioned`, `completed`, and `abandoned` (the last one Q553's give-up guard).
A restarted AGC has all three empty, so a replayed `JobAssigned` for a job that concluded days ago passes every check and provisions a worker.

The listener's own comment states this consequence outright; what it does not say is that **nothing ever prunes the queue**.
The replay is therefore not bounded to a recent window — it is every message the scale set still retains.

## What was already measured

### The backend half — live on dogfood, 2026-07-05

[gke-dogfood-turnup-findings.md](gke-dogfood-turnup-findings.md): during the Q264 P4 clean-green re-run, reconnecting a rebuilt AGC to the first pass's scale set `gag-scaleset2` (scaleSetID 4) replayed that pass's `JobAssigned` messages, "which briefly provisioned 7 pre-Q269 workers".
The re-run moved to a fresh scale-set label — a new scale-set object, and so an empty queue — to avoid it.

Those seven jobs had been provisioned *and* cursor-acked by the previous process before it went away.
So the observation establishes the load-bearing fact: the cursor is session-scoped at the backend, the queue log is not, and a fresh session polling from 0 receives messages an earlier session already acked.

It was written up as an aside to a different result, which is why it never became a bug row at the time.

### Retention — 2026-07-29

[q468-jobcompleted-retention.md](q468-jobcompleted-retention.md) measured an unacknowledged message surviving a **13 h 3 m 40 s** gap with no session in existence, redelivered to a session under a different owner name on its first poll.
Read it as a lower bound only: `LOST` was never observed at any gap, so it says nothing about where retention ends.

### What neither one settles

Both were measured for other reasons, and each leaves a hole the other does not fill:

- Q468 acknowledges its `JobAssigned` **in full** — cursor *and* `DeleteMessage` — so its check cannot separate "gone because deleted" from "gone because cursor-acked".
  Its result is about an undeleted, un-cursor-acked message.
- The dogfood observation is the right shape but is a side-observation in a narrative, with no message ids and no controlled before/after.

Neither is a reason to doubt the defect.
Both are reasons the *fix* wants its own measurement before it is built.

## Investigation G — the measurement the fix needs

One probe run settles three things, in one process, with no multi-hour gap and no dogfood cluster.
Selector `PROBE_REPLAY_TEST=true`; fixture [`q583-replay-probe.yml`](../../../.github/workflows/q583-replay-probe.yml).

| # | Question | Verdicts |
|---|---|---|
| 1 | Does a cursor-acked but **undeleted** `JobAssigned` replay to a fresh session polling from cursor 0? | `REPLAYED` / `NOT-REPLAYED` |
| 2 | Does `DeleteMessage` work on the wire? | `DELETE-OK` / `DELETE-FAILED` |
| 3 | After deleting, does a third session polling from cursor 0 still see it? | `PRUNED` / `STILL-REPLAYS` |

Question 1 is Q583's premise under control.
Questions 2 and 3 are the P4-era `DeleteMessage` unknown that Q264 left open — and together they are the proof that the proposed fix actually fixes the defect, taken *before* the fix is written rather than after.

Phases, all in one run:

1. Ensure the durable scale set, open session **gen 1**, wait for a dispatched job, receive its `JobAssigned` (message *M*).
2. Advance the cursor past *M* without deleting it — exactly what the shipping listener does.
3. Cancel the run so the job goes terminal with no runner involved (the Q468 technique), then receive and cursor-ack its `JobCompleted` (message *C*), again without deleting.
   The queue is now in the state a live AGC leaves behind: every message acked by cursor, none deleted, the job provably over.
4. Delete session gen 1.
   Open session **gen 2** under a different owner name and poll from cursor 0 — **measurement 1**.
5. `DeleteMessage` on whatever came back — **measurement 2**.
6. Delete session gen 2.
   Open session **gen 3**, poll from cursor 0 — **measurement 3**.
7. Delete the session and the scale set, which takes the queue log with it.

A different owner name per generation is deliberate: it is what makes the backend see a new listener arriving rather than the same one resuming, which is what a restarted AGC is.

### A 404 reads as success, so the verdict comes off the wire

Building the probe surfaced something that would have invalidated its own headline result. `Client.DeleteMessage` reports a 404 or 410 as success — for a listener that is right, since a message already gone is a benign ack — but a backend that does not serve the endpoint **also** answers 404.
The typed return says "acked" for the case the fix depends on and for the case that kills it.

So measurement 2 is read from the response status via a `ResponseObserver`, not from the error.
This is the rule Investigation E already states for its own reporting: the finding is what the wire did, not what the client made of it.

It also constrains the fix.
A listener that delete-acks and trusts `DeleteMessage`'s error would believe it had pruned a queue it had not touched, turning Q583 from a loud defect into a silent one.
Whatever ships has to treat a 404 as an ack only where that is genuinely safe, or check the status itself.

### What would make a result invalid

- **A `NOT-REPLAYED` verdict at step 4 must not be read as "no defect".** It would instead mean the backend prunes on session delete, and would contradict the 2026-07-05 dogfood observation — in which case the contradiction is the finding, and the next question is what differs (scale set age, job count, backend change since July).
- **A `DELETE-FAILED` at step 5 says nothing about step 4's result.** The two are independent; record both.
- **Step 6 with no step 5** is meaningless.
  If the delete failed, a `PRUNED` at step 6 means something else pruned the queue, not that the delete worked.

## The fix, once Investigation G reports

Both candidate approaches were weighed against the code before the probe was written; the probe decides between them rather than confirming a choice already made.
It has since reported — see [Decision: delete-ack](#decision-delete-ack).

**Delete-ack** — issue `DeleteMessage` once every job in a message is concluded.
This is the root-cause fix: it prunes the queue, so a replay carries only genuinely unfinished work, and it bounds the process-scoped sets, which the listener's own comment names as the intended endgame.
It needs Investigation G's questions 2 and 3 to come back `DELETE-OK` / `PRUNED`.

Its cost is that replay-from-0 is currently load-bearing in two places, and both need explicit handling rather than inheriting a backstop:

- The Q373/Q575 Secret reclaim treats replay as a free retry for a completion handled between the queue write and the Secret delete.
- Q551's deferred jobs are cursor-acked past and held only in memory, so today a restart recovers them *because* the queue replays.
  Deleting a message whose job is deferred would strand the run at GitHub forever — the exact failure Q551 shipped to fix.
  So the delete condition is "all jobs concluded", not "message ackable".

**Persist the cursor** on `RunnerSet.status` and seed a fresh listener with it.
Smaller, and independent of the wire shape — but the cursor is advanced past deferred jobs, so persisting it alone strands them at restart in exactly the way above, with no queue replay left to recover them.
Making it safe means persisting the deferred set too, which is most of the delete-ack work relocated into the status subresource.
Held as the fallback for a `DELETE-FAILED` verdict.

## Findings

### The answer: REPLAYED, DELETE-OK, PRUNED (2026-08-01)

One run against `actions-gateway/github-actions-gateway`, scale set `gag-q583-replay` (id 18), job `ee751b22-…`, run `30721627864`.
No warnings, no errors, scale set torn down at the end.

| Step | Observed |
|---|---|
| Stage | `JobAssigned` = message `100000001`, delivered ~0.8 s after the gen-1 session opened. Cursor advanced past it, **not** deleted. Run cancelled; `JobCompleted` = `100000002` (`result: canceled`) arrived ~0.8 s later, cursor advanced past it, again not deleted. gen-1 session deleted. |
| Measurement 1 | The gen-2 session — a different owner name, polling from cursor 0 — received **both** `100000001` and `100000002`, carrying the same job id, run id, and `jobWorkflowRef` as staged. **REPLAYED.** |
| Measurement 2 | `DELETE https://broker.actions.githubusercontent.com/scalesets/message/{id}` → **204 No Content** for both messages, ~100 ms each. **DELETE-OK.** |
| Measurement 3 | The gen-3 session polled from cursor 0 for the full 45 s window and received **nothing at all**. **PRUNED.** |

**Q583 is real, and the mechanism is the one the row asserted.** A message a previous session acked by cursor is redelivered to a session that never saw it: the cursor is session-scoped, the queue log is not.
A restarted AGC — whose `provisioned`, `completed`, and `abandoned` guards are all empty — reads that `JobAssigned` for a job cancelled minutes earlier and has nothing left that would stop it provisioning a worker.

**The `DeleteMessage` wire shape is confirmed, closing Q264's P4 unknown.** The source-derived construction is right, and the observed path is worth writing down because it is not what the client's own doc comment implies: the queue URL's path is `/scalesets/message` on `broker.actions.githubusercontent.com`, so the delete target is `/scalesets/message/{messageId}` — not per-scale-set in its path.
The client already builds it correctly by appending to `MessageQueueURL`.

**Delete-acking is a fix, not just a theory.** Measurement 3 is the one that matters: the same poll that returned two messages 45 seconds earlier returned an empty log after the deletes.
The contrast is what carries it — gen 2 got its first message ~350 ms after opening, so gen 3's empty 45 s window is not a too-short budget.

What this does **not** establish, the same caveats Q468 records: one tenant, one region, one point in time, on backend behaviour GitHub does not document.
The claim is "observed, 2026-08-01", not "GitHub guarantees".
And nothing here bounds retention — Q468's ≥13 h remains the only number, still a lower bound.

### What shipped

Acking is now cursor advance **plus** a delete, and the delete waits.
A handled message is registered against the jobs it names that have not concluded; a job concludes by completing with its Secret reclaimed, or by being abandoned (Q553); and a message whose set empties is deleted on the next poll cycle.
A failed delete keeps its entry and is retried on the following cycle, because the cursor has already moved past the message and nothing else would bring it back.

The wait is the whole safety argument, and each of its three cases has a test that fails without it: a job provisioned but still running, a Q551 deferred job the previous process never provisioned, and a completion whose Secret reclaim failed all hold their message in the queue, so a restart still re-reads them.

One trap worth recording, because it cost a wrong-green test before it was caught. `Client.DeleteMessage` reports a 404 as success, so a stub answering 404 made the listener believe it had deleted a message it had not, and the retry test passed while testing nothing.
Treating a 404 as an ack is nonetheless right *here* — Investigation G proved the endpoint is served, so a 404 means the message is genuinely gone — but it means a failure that must be retried has to be a 5xx.
The stub's lever now takes the status rather than a bool, which forces each test to say which backend it is modelling.

Left open deliberately: `provisioned`, `completed`, and `abandoned` are still never pruned, so they grow with the jobs a listener handles over its lifetime.
Pruning them is now *possible* — an entry is dead once its message is deleted — but it is a separate change with its own way of going wrong (drop an entry too early and a replay double-provisions), so it was filed as Q597 rather than bundled here.

**Q597 closed 2026-08-03.** The bound is the delete, keyed on the jobs a message *assigned*: an entry is retired once every held message carrying a `JobAssigned` for its job has been deleted.
The safety argument and the two rules that were tried and rejected are recorded in [02-architecture.md](../../design/02-architecture.md) alongside this fix.
One of them was caught by a test already in the repo — retiring on every job a deleted message *names* drops the guard when a completion's message goes, which `TestListener_DoesNotProvisionAnAssignmentReplayedAfterItsCompletion` (Q575) turns red.

### Decision: delete-ack

`DELETE-OK` plus `PRUNED` selects delete-acking over persisting the cursor.
The fallback is not needed.
The two costs named above stand and are what the implementation has to handle: a message is deleted only once **every job in it is concluded**, so Q551's deferred jobs keep the replay that recovers them, and the Q373/Q575 Secret reclaim keeps its backstop for anything still in flight.
