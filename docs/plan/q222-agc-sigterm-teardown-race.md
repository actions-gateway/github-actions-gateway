# Q222 — AGC SIGTERM teardown race (`TestAGC_SIGTERM_DeletesAllSessions`)

**Status:** open, escalated. Two timeout-ceiling bumps have already failed to hold. Per the standing note in [sigterm_test.go](../../cmd/agc/internal/controller/integration/sigterm_test.go), the next step is a real teardown-race investigation — **not** a third bump.

## Symptom

`TestAGC_SIGTERM_DeletesAllSessions` fails at the `WaitForSessionDelete`
assertion: after the manager context is cancelled and `mgr.Start` has returned,
one or more registered sessions never produce a `DELETE /session` to the broker
stub within the ceiling.

## Mitigation history

| Item | Change | Held? |
|---|---|---|
| Q120 | DELETE ceiling 10s → 30s | No — recurred |
| Q222 / PR #415, #416 | ceiling 30s → 60s, plus a failure dump (still-active sessions, global active count, registered list) | No — recurred, below |

The ceiling is a safety net, not the expected latency: `WaitForSessionDelete` is
channel-based and returns the instant the broker processes the DELETE.

## Recurrence — 2026-07-20

Observed on [run 29760328242](https://github.com/actions-gateway/github-actions-gateway/actions/runs/29760328242)
(PR #723). The PR's diff deletes a stale per-module golangci-lint config and
reflows a comment — **no runtime code**, so the branch is behaviourally
identical to `main` at `72fc519`. Treat this as a `main` recurrence for the
purposes of the [Flake watch](../development/maintaining-backlog.md#flake-fixes-go-first)
revive trigger.

The failure dump (both assertions, ~60s apart):

```
sigterm_test.go:132: SIGTERM teardown timeout: session "session-58" not deleted after 60s;
  still-active for owner "sigterm-rg"=[session-58 session-60];
  global ActiveSessionCount=11; all registered=[session-58 session-60 session-14 ...]
sigterm_test.go:132: SIGTERM teardown timeout: session "session-60" not deleted after 60s;
  still-active for owner "sigterm-rg"=[session-58 session-60]; global ActiveSessionCount=11
```

Total test wall time 120.29s — i.e. **both** per-session waits burned their full
60s ceiling.

## What the dump narrows down

Recorded observations only; none of these is yet a confirmed root cause.

- **Neither** of the owner's two sessions was deleted. This is a total teardown
  failure for `sigterm-rg`, not a partial or slow one. A CPU-starvation story
  predicts *late* DELETEs and a spread of outcomes; two clean 60s timeouts fit
  "the exit-defer never ran" better than "the runner was slow".
- Because both waits ran to the ceiling serially, a third bump buys nothing: the
  DELETEs were still absent at t+120s.
- Preceding the failure, the log shows the expected cancellation noise —
  `EnsureAgents failed ... context canceled` and `DeleteSession failed on
  goroutine exit ... context canceled` for *other* namespaces. Whether an
  equivalent line exists for `session-58`/`session-60` is **not** established
  from the captured excerpt and is the first thing to check.

## Investigation plan

1. **Confirm whether the exit-defer runs at all.** The defer builds a fresh
   `context.Background()` with a 10s timeout, so a cancelled reconcile context
   cannot strand it — but that only holds if the defer is reached. Add a log
   line at defer entry (not just on DELETE failure) so absence is
   distinguishable from failure.
2. **Check the registration/teardown window.** `WaitForFirstPoll` is what
   guarantees the goroutine passed `createSession` and registered its defer. Verify
   it cannot return true before the defer is actually in place.
3. **Look for a lost wakeup in the poll-loop exit path** — a goroutine parked in
   a way that `cancelMgr()` plus `<-mgrDone` does not unblock, which would
   explain a permanent (not slow) missing DELETE.
4. Only after 1–3: decide whether the product has a real shutdown-ordering bug
   (sessions leaking on SIGTERM in production) or the test's synchronisation is
   under-specified.

The production stake is the point of the spec: if listener goroutines can skip
their DELETE on shutdown, real deployments leak GitHub-side sessions on every
rollout.
