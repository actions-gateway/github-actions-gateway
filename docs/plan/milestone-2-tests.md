# Milestone 2 — Remaining Test Gaps

← [Milestone 2](milestone-2.md)

---

## Overview

**Goal:** Fill the moderate-severity test gaps identified in the post-PR coverage review of the Milestone 2 implementation. These were deferred from the main M2 PR to keep the initial PR focused.

All gaps below are unit tests unless noted. They run without network access; existing fake-clock, httptest-stub, and fake-k8s-client infrastructure from the M2 test suite is reused throughout.

Separate from this plan, the envtest integration test suite (§7.2 of the Milestone 2 plan) is also deferred and tracked via the open checklist item in `milestone-2.md`.

---

## Gaps

### Gap 3 — `Token()` cancelled before first fetch

**Package:** `cmd/agc/internal/token`  
**Code path:** `Token()` → `case <-ctx.Done(): return "", ctx.Err()`

The `Token()` function blocks on a `ready` channel until the first successful fetch. If the context is cancelled before that fetch completes, it should return `ctx.Err()`. No existing test exercises this path — `TestManager_NoLeakOnCancel` only cancels *after* a successful fetch.

**Test to add in `manager_test.go`:**

| Test | What it verifies |
|---|---|
| `TestManager_TokenCancelledBeforeReady` | Provider blocks until context cancelled; `Token()` returns an error wrapping context cancellation, not an empty string. |

**Implementation notes:**
- Use a blocking provider (channel-based) that doesn't return until a release signal is sent.
- Cancel the context before releasing the provider.
- Assert `Token()` returns a non-nil error; assert no goroutine leak (goleak).

---

### Gap 4 — `EnsureAgents` deregister error swallowed

**Package:** `cmd/agc/internal/agentpool`  
**Code path:** scale-down loop in `EnsureAgents` → `Deregister` returns error → `slog.Warn` + `continue`

The "best-effort deregister" behaviour is correct but unasserted. A future refactor could accidentally change the `continue` to a `return err`, breaking idempotency silently.

**Test to add in `pool_test.go`:**

| Test | What it verifies |
|---|---|
| `TestPool_EnsureAgents_DeregisterErrorContinues` | Use a `Registrar` stub that returns an error from `Deregister`. Scale down from 3 → 1. Assert `EnsureAgents` returns `nil` and the excess Secrets are still deleted. |

**Implementation notes:**
- Extend `StubRegistrar` or add a new `errorRegistrar` test helper that accepts a per-call error map.
- Use the fake k8s client to count remaining Secrets.

---

### Gap 5 — `pool.reload()` silently skips unparseable Secrets

**Package:** `cmd/agc/internal/agentpool`  
**Code path:** `reload` → `secretToAgent` returns error → `continue` (Secret skipped)

A single corrupted Secret silently shrinks the pool at runtime with no error returned to the caller.

**Test to add in `pool_test.go`:**

| Test | What it verifies |
|---|---|
| `TestPool_LoadAgents_SkipsCorruptSecret` | Manually create 3 Secrets (2 valid, 1 with a missing/malformed `privateKeyPEM`). Call `LoadAgents`. Assert 2 valid agents are returned and no error; log output optionally asserted. |

**Implementation notes:**
- Create Secrets directly on the fake client with `client.Create` rather than going through `EnsureAgents`.
- The corrupt Secret needs valid labels (so `listSecrets` finds it) but an invalid PEM value.

---

### Gap 6 — `refreshBrokerToken` failure early-exits `Run`

**Package:** `cmd/agc/internal/listener`  
**Code path:** `Run` → `refreshBrokerToken` → OAuth token fetch fails → `return err`

`refreshBrokerToken` is the first thing `Run` does. A failing OAuth server causes an immediate return, but this path is invisible to the existing test suite (every test provides a working OAuth stub).

**Test to add in `goroutine_test.go`:**

| Test | What it verifies |
|---|---|
| `TestListener_OAuthTokenFetchError` | OAuth stub returns 500. Assert `Run` returns a non-nil error without reaching `CreateSession` (verify via `brokerMux` spy that CreateSession was never called). |

---

### Gap 7 — `AcquireJob` failure increments metrics counter

**Package:** `cmd/agc/internal/listener`  
**Code path:** `handleJob` → `AcquireJob` returns error → `Metrics.JobAcquisitionErrors.Inc()` → `return err`

Neither the error return nor the metrics increment is verified by any test.

**Test to add in `goroutine_test.go`:**

| Test | What it verifies |
|---|---|
| `TestListener_AcquireJobError` | `brokerMux` delivers one job; `/acquirejob` returns 500. Assert `handleJob` returns an error; assert `JobAcquisitionErrorsTotal` counter incremented once with the correct label. |

**Implementation notes:**
- Use `prometheus/testutil` or a custom `Metrics` struct with exported counters to inspect counter values.
- The goroutine should continue polling after the failed acquisition (session is not closed).

---

### Gap 8 — Generic poll-error backoff path

**Package:** `cmd/agc/internal/listener`  
**Code path:** `GetMessage` returns a non-rate-limit, non-session-expired error → `pollErrors++` → `backoffDelay(consecutiveErrors > 5, clock)` returns 30–60 s range

Neither the `pollErrors` accumulation path nor the `>5` tier of `backoffDelay` is exercised.

**Tests to add in `goroutine_test.go`:**

| Test | What it verifies |
|---|---|
| `TestListener_PollErrorBackoff` | Stub returns generic 503 errors. Fake clock. Assert backoff called with correct durations; goroutine does not exit. |
| `TestBackoffDelay_HighErrorCount` | Call `backoffDelay(6, fakeClock)` directly (exported or tested via `Run` with 6 consecutive errors); assert returned duration is in the 30–60 s range. |

**Implementation notes:**
- The `backoffDelay` function is unexported; test via `goroutine_test.go` in the internal package, or export it as `BackoffDelay` for testability. Alternatively, test the goroutine behaviour with a fake clock.

---

### Gap 9 — `TokenManager.Token()` failure during reconcile and delete

**Package:** `cmd/agc/internal/controller`  
**Code path (reconcile):** `r.TokenManager.Token()` returns error → reconciler returns error  
**Code path (delete):** `r.TokenManager.Token()` returns error → `instToken = ""` + warning + continue (graceful degradation)

Neither path is tested. The delete path is especially important: a broken token manager during RunnerGroup deletion would otherwise orphan GitHub-registered agents.

**Tests to add in `runnergroup_controller_test.go`:**

| Test | What it verifies |
|---|---|
| `TestReconcile_TokenManagerError` | Use a token manager stub that always returns an error. Call reconcile. Assert reconciler returns error; no Secrets created. |
| `TestReconcile_DeleteWithBrokenTokenManager` | Provision a RunnerGroup (2 agents), then replace the token manager with one that always errors. Trigger deletion. Assert finalizer is removed and agent Secrets are deleted even though deregistration could not be attempted. |

**Implementation notes:**
- Add a `token.Manager` constructor that accepts a static-error provider for testing.
- The delete path uses `instToken = ""` → `pool.DeleteAll(ctx, "")` → `StubRegistrar.Deregister` with an empty token (should succeed in the stub).

---

### Gap 10 — `drainConditions` isolation across multiple RunnerGroups

**Package:** `cmd/agc/internal/controller`  
**Code path:** `drainConditions` → skips updates for other RunnerGroups → re-enqueues them

The re-enqueue logic is present but no test verifies it: a condition sent for RunnerGroup B is not applied to RunnerGroup A, and is later applied when RunnerGroup B is reconciled.

**Test to add in `runnergroup_controller_test.go`:**

| Test | What it verifies |
|---|---|
| `TestReconcile_DrainConditionsIsolation` | Create two RunnerGroups (A and B). Enqueue a condition for B using `SetConditionForTest`. Reconcile A. Assert the condition does NOT appear on A's status. Then reconcile B. Assert the condition appears on B's status. |

---

### Gap 11 — Pool-exhausted path in `getOrCreateMultiplexer`

**Package:** `cmd/agc/internal/controller` / `cmd/agc/internal/listener`  
**Code path:** `pool.ClaimAgent()` returns nil → `listener.Config{Agent: nil}` → `Run` returns `"listener: no agent available"` → `NonRetriableError`? (currently just a plain error, which would cause an infinite restart loop)

There are two issues here:
1. The pool-exhausted error from `Run` is not wrapped in `NonRetriableError`, so the permanent goroutine *would* restart in a tight loop.
2. No test exercises this path.

**Fix required before writing the test:**
In `goroutine.go`, the "no agent available" early return should be wrapped:
```go
if cfg.Agent == nil {
    return &NonRetriableError{Cause: fmt.Errorf("pool exhausted: no agent available")}
}
```

**Test to add in `runnergroup_controller_test.go`:**

| Test | What it verifies |
|---|---|
| `TestReconcile_PoolExhausted` | Create a RunnerGroup with `maxListeners=0` (or mock `ClaimAgent` to always return nil). Reconcile. Assert no infinite goroutine spawn; goroutine count stays at 0 after the non-retriable error. |

---

## Priority order

| Priority | Gap | Reason |
|---|---|---|
| 1 | Gap 11 (fix + test) | Actual bug: tight restart loop on pool exhaustion |
| 2 | Gap 10 (`drainConditions` isolation) | Could cause condition cross-contamination across RunnerGroups |
| 3 | Gap 9 (token failure during delete) | Silent data loss: orphaned GitHub agents |
| 4 | Gap 6 (`refreshBrokerToken`) | Invisible early-exit path |
| 5 | Gap 7 (AcquireJob failure + metrics) | Metrics correctness |
| 6 | Gap 8 (generic poll backoff) | Algorithm correctness |
| 7 | Gap 3 (Token() cancel before ready) | Edge case in startup |
| 8 | Gap 4 (deregister error swallowed) | Documents intentional best-effort |
| 9 | Gap 5 (reload skips corrupt Secrets) | Documents intentional skip |

---

## Envtest integration tests (from Milestone 2 checklist)

The unchecked item in `milestone-2.md` ("RunnerGroup create/scale/delete lifecycle produces no goroutine leaks in integration tests") calls for an `envtest` suite in `cmd/agc/internal/controller/`. This is a separate track from the unit-test gaps above and can be done in the same PR or a subsequent one.

Minimum scenarios (see `milestone-2.md §7.2` for the full list):
1. RunnerGroup create → agent Secrets exist, `status.activeSessions ≥ 1`, `Ready` condition true
2. Scale up → new Secrets appear
3. Scale down → excess Secrets deleted, no goroutine leak
4. Delete → Multiplexer stopped, all Secrets deleted, finalizer removed

**Setup:** `envtest` binaries via `setup-envtest`. Add a `Makefile` target and skip the suite when `KUBEBUILDER_ASSETS` is not set.
