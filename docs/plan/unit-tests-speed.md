# Unit Test Speed Improvements

This document analyses where time is spent in the unit test suite and describes four concrete improvements, in order of estimated impact. Each section covers motivation, implementation steps, files affected, and estimated savings.

---

## Background — where time goes today

A typical `go test ./...` run (excluding e2e and integration suites) breaks down roughly as follows:

| Package | Typical duration |
|---|---|
| `agc/internal/listener` | ~11.5s |
| `agc/internal/controller` | ~2.4s |
| `agc/internal/agentpool` | ~2.4s |
| `githubapp` | ~1.3s |
| `agc/internal/provisioner` | ~1.1s |
| `gmc/internal/controller` | ~0.9s |
| `gmc/internal/webhook/v1alpha1` | ~0.7s |
| `agc/internal/token` | ~0.7s |
| `broker` | ~0.6s |
| `proxy`, `worker` | ~0.5s each |
| **Total (sequential)** | **~22s** |

The listener package dominates. Within it, the slowest individual tests are:

| Test | Time | Root cause |
|---|---|---|
| `TestMultiplexer_NonRetriableNoRestart` | ~2.1s | `assert.Never` holds 2s to clear the 1s restart backoff |
| `TestRenewLoop_NonOKContinues` | ~1.9s | Fake clock advanced 1s per 10ms poll to reach 60s tick × 3 |
| `TestRenewLoop_TicksAt60s` | ~1.8s | Same as above |
| `TestMultiplexer_CeilingRespected` | ~0.9s | RSA key generation per test + ceiling check |

---

## 1. RenewLoop fake-clock advancement — ~3.4s saved

### Problem

`TestRenewLoop_TicksAt60s` and `TestRenewLoop_NonOKContinues` each take ~1.8s despite using a `fakeClock`. `StartRenewLoop` waits for `clk.After(60 * time.Second)`. The tests verify each of 3 RenewJob calls in a loop:

```go
for i := 0; i < 3; i++ {
    assert.Eventually(t, func() bool {
        clk.Advance(time.Second)        // advance 1s per 10ms poll
        return renewCalls.Load() >= int32(i+1)
    }, 5*time.Second, 10*time.Millisecond, ...)
}
```

To trigger a single 60s tick, the loop must advance 60 times × 10ms real time = 0.6s. Across 3 iterations that is ~1.8s of real wall time per test.

### Fix

Advance the clock by a large step (enough to pass the 60s threshold) before entering the `Eventually` poll. The poll then only needs one or two iterations to observe the call.

```go
for i := 0; i < 3; i++ {
    clk.Advance(65 * time.Second) // jump past the tick in one step
    assert.Eventually(t, func() bool {
        return renewCalls.Load() >= int32(i+1)
    }, 2*time.Second, time.Millisecond, ...)
}
```

### Files affected

- `cmd/agc/internal/listener/goroutine_test.go` — `TestRenewLoop_TicksAt60s` (line ~617) and `TestRenewLoop_NonOKContinues` (line ~672)

### Estimated saving

~1.7s × 2 tests = **~3.4s**

---

## 2. Multiplexer restart backoff — ~2s saved

### Problem

`multiplexer.go` uses a hardcoded 1s restart backoff for the permanent baseline goroutine:

```go
// multiplexer.go:145
case <-time.After(time.Second):
```

Two tests are directly slowed by this:

- `TestMultiplexer_NonRetriableNoRestart` calls `assert.Never(..., 2*time.Second, 50*time.Millisecond)` to confirm no restart occurs. The 2s window is necessary only to hold past the 1s backoff.
- `TestMultiplexer_RestartOnCrash` must wait for the goroutine to exit, sleep 1s, then restart and reach `ActiveCount == 1`.

### Fix

Add a `restartDelay time.Duration` field to `Multiplexer`. Production code defaults to `time.Second` when zero; tests set it to 0 (or 1ms) via a new `WithRestartDelay` option or an exported field.

```go
// multiplexer.go
type Multiplexer struct {
    // ...
    restartDelay time.Duration // 0 → defaults to time.Second
}

// in spawn():
delay := m.restartDelay
if delay == 0 {
    delay = time.Second
}
select {
case <-ctx.Done():
    return
case <-time.After(delay):
}
```

Tests that use `newMuxWithServers` can then pass `restartDelay: 0` to skip the backoff entirely, reducing `TestMultiplexer_NonRetriableNoRestart` from ~2.1s to ~50ms.

### Files affected

- `cmd/agc/internal/listener/multiplexer.go` — add `restartDelay` field, use it in `spawn()`
- `cmd/agc/internal/listener/multiplexer_test.go` — set delay to 0 in test helpers

### Estimated saving

~1.5s for `TestMultiplexer_NonRetriableNoRestart` + ~0.5s for `TestMultiplexer_RestartOnCrash` = **~2s**

---

## 3. RSA 2048-bit key generation — ~1s saved

### Problem

Every call to `makeAgent()` in `goroutine_test.go` generates a fresh 2048-bit RSA key (~100ms each). `makeAgent` is called once per test that uses `makeCfg` or `newMuxWithServers`, plus multiple direct calls in `broker/crypto_test.go` and `githubapp/auth_test.go` and `githubapp/runner_auth_test.go`.

Approximate count:
- `goroutine_test.go` — `makeAgent` called in ~9 tests
- `multiplexer_test.go` — `makeAgent` called in ~2 helpers
- `broker/crypto_test.go` — 2 direct `rsa.GenerateKey` calls
- `githubapp/auth_test.go` — 2+ direct calls
- `githubapp/runner_auth_test.go` — 5 direct calls

Total: ~20 key generations × ~50–100ms each = ~1–2s spread across packages.

### Fix

Declare a package-level test key generated once. Tests that only need a valid key (i.e., not testing key parsing) share it.

```go
// goroutine_test.go (package-level)
var testRSAKey = func() *rsa.PrivateKey {
    k, err := rsa.GenerateKey(rand.Reader, 2048)
    if err != nil {
        panic(err)
    }
    return k
}()

func makeAgent(t *testing.T, oauthSrvURL string) *agentpool.Agent {
    t.Helper()
    return &agentpool.Agent{
        // ...
        PrivateKey: testRSAKey,
    }
}
```

For tests that explicitly test key generation or parsing (e.g., `TestParseRunnerRSAKey_RoundTrip`), keep per-test generation where correctness requires it. Alternatively, use 1024-bit keys in tests where key strength is irrelevant — they generate ~4× faster.

### Files affected

- `cmd/agc/internal/listener/goroutine_test.go`
- `cmd/agc/internal/agentpool/github_registrar_test.go`
- `broker/crypto_test.go`
- `githubapp/auth_test.go`
- `githubapp/runner_auth_test.go`

### Estimated saving

~10–15 fewer key generations × ~70ms each = **~0.7–1s** across packages

---

## 4. Add `t.Parallel()` to independent tests — variable savings

### Problem

No test in the entire codebase calls `t.Parallel()`. Within a package, Go runs tests sequentially by default. Several packages contain tests that are fully independent (separate HTTP servers, no shared global state) and would parallelize cleanly.

### Fix

Add `t.Parallel()` at the top of tests in packages where tests do not share mutable globals. Safe candidates:

- `broker/` — all tests use local `httptest.Server` instances
- `githubapp/` — same pattern
- `cmd/proxy/`, `cmd/worker/` — same pattern
- `cmd/agc/internal/listener/` — safe for most tests; the `goleak.VerifyNone` tests need care since goroutine leak detection is global, but can be made parallel if each test uses `goleak.VerifyNone(t)` (per-test scope rather than package-level)

Packages with `envtest` or shared Kubernetes state (`gmc/internal/controller`, `agc/internal/controller`, integration suites) should not be parallelized without more analysis.

### Files affected

- `broker/client_test.go`, `broker/crypto_test.go`, `broker/egress_ip_test.go`
- `githubapp/auth_test.go`, `githubapp/runner_auth_test.go`
- `cmd/proxy/proxy_test.go`, `cmd/worker/worker_test.go`
- `cmd/agc/internal/listener/goroutine_test.go`, `multiplexer_test.go`

### Estimated saving

Depends on CPU count. On a 4-core machine the listener package alone (currently ~11.5s sequential) could drop to ~4–5s if the top independent tests run in parallel. Total across all packages: **~5–8s** on typical CI hardware.

---

## Summary

| # | Change | Effort | Saving |
|---|---|---|---|
| 1 | Advance fake clock in large steps in RenewLoop tests | Low — test-only | ~3.4s |
| 2 | Injectable restart delay on Multiplexer | Medium — small prod change + tests | ~2s |
| 3 | Share package-level RSA test key | Low — test-only | ~1s |
| 4 | Add `t.Parallel()` to independent tests | Medium — needs per-package audit | ~5–8s on CI |

Implementing 1–3 alone saves ~6–7s (reducing total suite time from ~22s to ~15s) with minimal risk. Adding parallelism (4) could cut the total further by half on multi-core CI runners.
