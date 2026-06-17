# Milestone 3 Test Improvement Plan

← [Milestone 3](milestone-3.md)

---

## Overview

This document catalogues test quality gaps identified by reviewing the Milestone 3 implementation across three packages:

- `cmd/agc/internal/provisioner/` — worker pod provisioner
- `cmd/agc/internal/listener/` — AES-256-CBC message decryption and session key renewal
- `cmd/worker/` — entrypoint wrapper (FIFO protocol)

Items are ranked by impact on bug detection.

## Status

**All High- and Medium-priority items are implemented and merged** — H1 landed first,
and H2–H5 plus M1–M4 landed together in commit `17a7f5c`
("test: implement Milestone 3 test improvements from plan"). The Low-priority items
are resolved or obsolete (see L1/L2 below). Q9 is therefore complete.

| Item | State | Where |
|---|---|---|
| H1 | ✅ Done | `JobDuration`/`EvictionRetries`/`EvictionRetriesExhausted` asserted in `provisioner_test.go`; dead `PodCreationLatency` field removed from `metrics.go`. |
| H2 | ✅ Done | `TestProvisioner_EvictionRerunAPI5xx` (provisioner_test.go). |
| H3 | ✅ Done | `TestListener_DecryptFailureFallsBackToPlaintext` (goroutine_test.go). |
| H4 | ✅ Done | `TestProvisioner_PriorityTiersSecondTier` + `TestProvisioner_PriorityTiersBoundary`. |
| H5 | ✅ Done | `TestProvisioner_EvictionRetryBudgetExhausted` — metric-counter assertion, no wall-clock sleep. |
| M1 | ✅ Done | `TestListener_PlaintextSessionKey` + `TestListener_NoSessionKey`. |
| M2 | ✅ Done | `TestWrapper_WriteJobMessage_Empty` + `TestWrapper_WriteJobMessage_Large` (worker_test.go). |
| M3 | ✅ Done | `TestProvisioner_PendingPodsCountTowardCeiling`. |
| M4 | ✅ Done | `TestProvisioner_PodDeletedExternallySucceeds`. |
| L1 | ❎ Obsolete | The named-pipe FIFO wrapper was removed; the wrapper now writes via `WriteJobMessage` to a generic `io.Writer`, already covered by the `TestWrapper_WriteJobMessage_*` tests. No FIFO open to time out. |
| L2 | ✅ Done | `closeHTTP` now calls `srv.CloseClientConnections()` before `srv.Close()`. The few remaining `time.Sleep` calls in individual tests guard fake-clock goroutine drain, a different race than the HTTP-transport one. |

Per-item detail follows below for historical context.

---

## High Priority

### H1 — No metric assertions for any M3 Prometheus metrics ✅ Done

**What's missing:** The provisioner tests pass `nil` for `*listener.Metrics`, so `JobDuration`, `EvictionRetries`, and `EvictionRetriesExhausted` are never asserted. `PodCreationLatency` is declared in `Metrics` but never emitted anywhere in `provisioner.go` — dead code that a metric assertion would immediately surface.

**Proposed fix:**

- Add a `newTestMetrics()` helper in `provisioner_test.go` (analogous to the one in `goroutine_test.go`).
- In `TestProvisioner_CreatesPodAndSecret`, assert `testutil.ToFloat64(m.JobDuration.WithLabelValues(...)) > 0` after pod completion.
- In `TestProvisioner_EvictionAutoRetry`, assert `EvictionRetries` counter equals 1.
- In `TestProvisioner_EvictionRetryBudgetExhausted`, assert `EvictionRetriesExhausted` counter equals 1 on the second eviction.
- Add a dedicated test that either emits `PodCreationLatency` in the provisioner code and asserts it, or removes the dead field.

---

### H2 — Rerun API 5xx is non-fatal but no test verifies it ✅ Done

**What's missing:** `handleEviction` logs the error and continues when the GitHub rerun API returns a non-2xx status. No test verifies that: (a) `provision` still returns `nil` (non-fatal), (b) the retry budget counter is still incremented. A regression making the error fatal would go undetected.

**Proposed fix:**

Add `TestProvisioner_EvictionRerunAPI5xx`:
- Stub HTTP server returns `StatusInternalServerError`.
- Run one provision cycle that ends in pod eviction.
- Assert `provision` returns `nil`.
- Assert `EvictionRetries` counter incremented once.
- Assert rerun was attempted (verify the request path was received).

---

### H3 — `handleJob` decryption-failure fallback path is untested ✅ Done

**What's missing:** `goroutine.go` falls back to the raw `msg.Body` when `DecryptMessageBody` fails (wrong key produces bad PKCS#7 padding). No test exercises this branch. The contract — silent fallback vs. metric/log — is unverified, and a wrong-key scenario producing garbage is invisible to the test suite.

**Proposed fix:**

Add `TestListener_DecryptFailureFallsBackToPlaintext`:
- Return a valid RSA-encrypted session key K from `CreateSession`.
- Deliver a `GetMessage` body that is valid base64 but encrypted with a *different* key (correct structure, wrong padding when decrypted with K).
- Assert the desired contract: either `AcquireJob` is called via the fallback plaintext path, or it is not called and an appropriate warning metric fires.

---

### H4 — Second priority tier never assigned; tier boundary off-by-one untested ✅ Done

**What's missing:** `TestProvisioner_PriorityTiersAssignment` tests only 3 active pods against thresholds of 5 and 10, so only tier 1 (`runner-critical`) is ever assigned. Flipping `<` to `<=` in `ceilingCheck` would silently mis-assign pods at the boundary without any test failing.

**Proposed fix:**

- Add `TestProvisioner_PriorityTiersSecondTier`: pre-populate 6 active pods (above threshold 5, below 10); assert the new pod receives `PriorityClassName == "runner-standard"`.
- Add a boundary sub-case at exactly `activePods == threshold` (5 pods) to pin the comparison semantics — `activePods == threshold` should fall through to the next tier.

---

### H5 — Budget-exhaustion negative assertion uses a 100 ms wall-clock sleep ✅ Done

**What's missing:** `TestProvisioner_EvictionRetryBudgetExhausted` uses `time.After(100ms)` to assert "no API call made." This is racy on loaded CI machines (the handler may not have returned yet) and would silently pass if a bug introduced a >100 ms delay before calling the API.

**Proposed fix:**

Replace the channel-based negative assertion with a synchronous metric counter check. Since `provision` returns only after `handleEviction` finishes, asserting `testutil.ToFloat64(m.EvictionRetriesExhausted.WithLabelValues(...)) == 1` and `rerunCount == 1` directly after `runCycle` returns is race-free with no sleep required.

---

## Medium Priority

### M1 — `createSession` raw-key and no-key branches untested ✅ Done

**What's missing:** `createSession` has three encryption branches: (a) `encrypted == true` — RSA decrypt (tested); (b) `encrypted == false` — use raw key bytes; (c) no `encryptionKey` field — `aesKey` stays `nil`, messages parsed as plaintext. Branches (b) and (c) are dead to the test suite. A bug that sets `aesKey` to an empty slice instead of `nil` in branch (b) would cause every subsequent `DecryptMessageBody` call to fail with a cipher error.

**Proposed fix:**

- Add `TestListener_PlaintextSessionKey`: `CreateSession` returns `encryptionKey.encrypted == false` with the raw AES key; deliver an AES-CBC encrypted body; assert `AcquireJob` receives the correct `jobMessageId`.
- Add `TestListener_NoSessionKey`: `CreateSession` returns no `encryptionKey` field; deliver a plaintext JSON body directly as `msg.Body`; assert `AcquireJob` is called correctly.

---

### M2 — `writePayloadToPipe` with an empty payload is untested ✅ Done

**What's missing:** A misconfigured empty Kubernetes Secret produces a `[0,0,0,0]` wire message with no body bytes. `Runner.Worker` would then read zero bytes after the prefix, potentially hanging or erroring. No test covers this case or verifies the length prefix round-trips correctly for large payloads.

**Proposed fix:**

- Add `TestWrapper_EmptyPayload`: call `writePayloadToPipe` with `payload = []byte{}`; assert the reader receives exactly 4 bytes encoding value `0`.
- Add `TestWrapper_LargePayload`: use a 65 536-byte payload; assert the length prefix round-trips correctly via `binary.BigEndian`.

---

### M3 — `activePodCount` Pending-pod branch is untested ✅ Done

**What's missing:** `activePodCount` counts both `PodRunning` and `PodPending` pods, but every ceiling test seeds only `PodRunning` pods. Removing the `PodPending` branch from the count would silently allow over-provisioning past the ceiling.

**Proposed fix:**

Add `TestProvisioner_PendingPodsCountTowardCeiling`: pre-populate 2 Running + 1 Pending pod at `MaxWorkers: 3`; assert the new pod is held (ceiling enforced).

---

### M4 — Externally deleted pod path untested ✅ Done

**What's missing:** `waitForPodCompletion` returns `(PodSucceeded, "", nil)` when a pod disappears (not-found), representing an operator manually deleting the pod mid-run. No test verifies this does not trigger eviction-retry handling. Inverting the not-found logic to return `PodFailed` would spuriously fire the rerun API.

**Proposed fix:**

Add `TestProvisioner_PodDeletedExternallySucceeds`:
- After the pod appears, call `fc.Delete(ctx, pod)` instead of using `completePod`.
- Assert `provision` returns `nil`.
- Assert the rerun API is not called (wire up a stub server and verify its channel stays empty).
- Assert the job Secret is cleaned up.

---

## Low Priority

### L1 — `TestWrapper_WritesToNamedPipes` has no timeout ❎ Obsolete

**What's missing:** The reader goroutine has no deadline. A blocked FIFO open (e.g., `Mkfifo` fails silently and `writePayloadToPipe` never opens the write end) would hang the test indefinitely in CI rather than report a clear failure.

**Proposed fix:**

Add a `context.WithTimeout(t.Context(), 5*time.Second)` and select on `ctx.Done()` in the reader goroutine, failing the test with a descriptive message on deadline exceeded.

---

### L2 — `closeHTTP` uses an unconditional 50 ms sleep for goroutine drain ✅ Done

**What's missing:** `closeHTTP` in `goroutine_test.go` calls `time.Sleep(50ms)` before returning, and this pattern appears ~13 times in the file, adding ~650 ms of synthetic wait per test run. The sleep is undocumented and may be insufficient on slow hardware.

**Proposed fix:**

Call `srv.CloseClientConnections()` before `srv.Close()` to drain in-flight connections without a fixed wait. If a sleep is still required for a specific goroutine, document the race it guards against with a comment.
