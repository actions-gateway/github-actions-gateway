# Q222 — AGC SIGTERM teardown race (`TestAGC_SIGTERM_DeletesAllSessions`)

**Status:** fixed.
Two defects found in the shutdown path, one reproduced at the unit tier and one structural; both fixed, and the test's timeout ceiling is no longer load-bearing.
See [Findings](#findings) for what was measured and [What was not established](#what-was-not-established) for the limits of the evidence.

## Symptom

`TestAGC_SIGTERM_DeletesAllSessions` fails at the `WaitForSessionDelete` assertion: after the manager context is cancelled and `mgr.Start` has returned, one or more registered sessions never produce a `DELETE /session` to the broker stub within the ceiling.

## Mitigation history

| Item | Change | Held? |
|---|---|---|
| Q120 | DELETE ceiling 10s → 30s | No — recurred |
| Q222 / PR #415, #416 | ceiling 30s → 60s, plus a failure dump (still-active sessions, global active count, registered list) | No — recurred |
| Q222 (this change) | Fix the two teardown defects below; ceiling back to 10s | — |

## Recurrence — 2026-07-20

Observed on [run 29760328242](https://github.com/actions-gateway/github-actions-gateway/actions/runs/29760328242) (PR #723).
The PR's diff deletes a stale per-module golangci-lint config and reflows a comment — **no runtime code**, so the branch is behaviourally identical to `main` at `72fc519`.
Treat this as a `main` recurrence for the purposes of the [Flake watch](../../development/maintaining-backlog.md#flake-fixes-go-first) revive trigger.

The failure dump (both assertions, ~60s apart):

```
sigterm_test.go:132: SIGTERM teardown timeout: session "session-58" not deleted after 60s;
  still-active for owner "sigterm-rg"=[session-58 session-60];
  global ActiveSessionCount=11; all registered=[session-58 session-60 session-14 ...]
sigterm_test.go:132: SIGTERM teardown timeout: session "session-60" not deleted after 60s;
  still-active for owner "sigterm-rg"=[session-58 session-60]; global ActiveSessionCount=11
```

Total test wall time 120.29s — i.e. **both** per-session waits burned their full 60s ceiling.

## Findings

### 1. Nothing waited for listener goroutines at shutdown (structural)

`cmd/agc/main.go` ends in `return mgr.Start(ctx)`.
Listener goroutines are spawned from `Reconcile` onto the manager's context, so SIGTERM cancels them — but the manager does not track them.
`Multiplexer.Stop`, which cancels *and* waits on each goroutine's `done` channel, was called only from `reconcileDelete`/`cleanupLocalState` (RunnerGroup deletion), never on manager shutdown.

So `mgr.Start` returned as soon as the controllers stopped, `run()` returned, and the process exited out from under every goroutine still unwinding its poll loop and running its exit-defer `DELETE`.
**In production this leaks a GitHub-side session per in-flight listener on every rollout.** In the integration test the process does not exit, which is why the test only ever saw it as a race against a timeout: `<-mgrDone` carried no information about the goroutines at all, so the assertion was polling a 60s ceiling with nothing guaranteeing the work had even started.

**Fix:** a `listenerShutdown` manager `Runnable` ([listener_shutdown.go](../../../cmd/agc/internal/controller/listener_shutdown.go)) registered by both reconcilers.
It blocks on the manager context and then drains the reconciler's multiplexers, so `mgr.Start` returns only after every listener goroutine has exited.
Multiplexers are drained concurrently (done channel per the repo's async convention) so shutdown costs the slowest one, not their sum.

### 2. The recycle/heal handoff dropped the DELETE outright on cancellation

Reproduced at the unit tier, red → green: `TestListener_SIGTERMDuringPostJobRecycleDeletesSession` in [goroutine_q222_test.go](../../../cmd/agc/internal/listener/goroutine_q222_test.go).
Against the unfixed code it recorded **zero** DELETEs.

The sequence, in `Run`'s post-job recycle (and identically in the poll-loop heal):

1. `oldSession := sessionID; sessionID = ""` — ownership is handed to the recycle so the exit defer will not double-DELETE.
   (In the v2 flow DELETE is keyed by bearer token, so a re-delete could tear down the session the heal just created — the handoff is correct in itself.)
2. `recycleAndRestart` opens with `cfg.Broker.DeleteSession(ctx, oldSessionID)` on the **caller's** context.
   Under SIGTERM that context is already cancelled, so the call fails instantly and its error is discarded as best-effort.
3. `cfg.RecycleAgent(ctx)` then fails for the same reason; `Run` takes `if ctx.Err() != nil { return nil }`.
4. The exit defer sees `sessionID == ""` and skips the DELETE.

Nothing downstream ever deletes that session.
It is a permanent leak, not a slow one — the shape the plan predicted for a total (not partial) teardown failure.

**Fix:** `deleteSessionDetached` in [session.go](../../../cmd/agc/internal/listener/session.go) — one helper now used by the heal, the recycle, and the exit defer.
It runs on a context detached from the caller's, bounded by a 10s total budget.

### 3. A single transient DELETE failure was also a permanent leak

Found while fixing 2.
The exit defer's DELETE was the only one a session would ever get; any error was logged at Warn and dropped.
A connection reset as the fleet tears down, or a connection pool exhausted by sibling long-polls unwinding at once, leaked the session exactly as permanently as no attempt at all.

**Fix:** `deleteSessionDetached` retries within its budget (3 attempts, 3s per round trip, 250ms backoff) and logs a Warn naming the leak only after giving up.
Covered by `TestListener_ExitDeleteRetriesTransientFailure`.

## What was not established

The unit reproduction of defect 2 does **not** explain the CI failure above.
The SIGTERM integration test starts the reconciler with no provisioner (`provisionerOptions{}` → nil `JobHandler`), so its job-bearing goroutines run their post-job recycle to completion seconds *before* SIGTERM, and none is in the recycle window when the context is cancelled.
Reverting only the defect-2 fix and re-running the integration test locally still passes.

Defect 1 is the one that governs the CI symptom: pre-fix, `<-mgrDone` guaranteed nothing, so the assertion was a 60s race against goroutine scheduling on a 2-vCPU runner.
Post-fix it is settled before the assertion is reached.
The original CI failure was **not** reproduced locally, so the causal claim for that specific run rests on the structural argument, not on a local repro.

Defect 2 stands on its own regardless: it is a real production session leak on every rollout that catches a listener in its post-job recycle, and it is proven by a test that fails without the fix.

## Test-side consequence

With the drain in place, `mgr.Start` returning means every session DELETE has already been issued and answered.
The ceiling in [sigterm_test.go](../../../cmd/agc/internal/controller/integration/sigterm_test.go) drops from 60s to 10s and no longer scales with runner load — it now only absorbs the broker stub's own signal-delivery hop.
A recurrence there is a real teardown regression and must be investigated, not bumped.
