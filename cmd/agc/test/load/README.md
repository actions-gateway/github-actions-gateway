# AGC load-test harness (Q13)

An in-process load test that pins the design's headline capacity claim:
**thousands of virtual runner sessions multiplexed as goroutines inside one
AGC**, each costing **one runner re-registration per job** (the single-use JIT
lifecycle, [Q114](../../../../docs/STATUS.md)).

It drives the AGC's real listener-multiplexing core — the same
`listener.Multiplexer` + `agentpool.Pool` + per-goroutine `broker.Client`
wiring that `RunnerGroupReconciler` builds in production — against an in-process
broker stub, a controller-runtime fake client for agent Secrets, and an
in-memory registrar. **No cluster and no GitHub credentials are required.**

## Run it

```bash
make load-test-quick   # 10 tenants × 100 listeners = 1,000 sessions, ~1 min
make load-test-full    # same scale, realistic job hold, writes results/latest.md
```

Both wrap a single `go test -tags load -run TestAGCLoad ./test/load/...` in
`cmd/agc`, under the desktop-safety throttle prefix (a no-op on CI). A run holds
~1,000 goroutines; the throttle keeps a GUI dev machine responsive
([Q92](../../../../docs/STATUS.md)).

### Tuning

Every knob is an environment variable, so the same target scales up on a bigger
host without code edits:

| Env var | Default | Meaning |
|---|---|---|
| `LOAD_TENANTS` | 10 | independent RunnerGroups |
| `LOAD_LISTENERS_PER_TENANT` | 100 | listeners (= sustained sessions) per tenant |
| `LOAD_WARMUP` | 5s | ramp before steady-state sampling begins |
| `LOAD_DURATION` | 20s | steady-state measurement window |
| `LOAD_JOB_DURATION` | 100ms | simulated worker-pod runtime (how long a session holds a job) |
| `LOAD_THINK_TIME` | 0 | gap between jobs per session (0 = saturated) |
| `LOAD_LONGPOLL_HOLD` | 2s | broker idle-poll hold |
| `LOAD_SAMPLE_INTERVAL` | 250ms | sampling cadence |
| `LOAD_RENEW_INTERVAL` | 30s | per-job RenewJob cadence |
| `LOAD_REPORT` | _(none)_ | path (relative to this package dir) to write the Markdown report |

Example — push one AGC to 5,000 sessions for two minutes:

```bash
cd cmd/agc
LOAD_TENANTS=10 LOAD_LISTENERS_PER_TENANT=500 LOAD_DURATION=120s \
  go test -tags load -timeout 15m -run TestAGCLoad -v ./test/load/...
```

### JUnit XML for CI

```bash
cd cmd/agc
go test -tags load -run TestAGCLoad -json ./test/load/... | go-junit-report > load-junit.xml
```

## What it measures, and how to read it

A virtual runner session **is** a listener goroutine. Each goroutine long-polls
the broker; on a job it acquires, holds it for `LOAD_JOB_DURATION`, then — because
single-use JIT runners are spent on acquisition — re-registers its agent and
opens a fresh session before polling again. The driver keeps every session
saturated so the pool ramps to `LISTENERS_PER_TENANT` and holds there.

| Metric | Reading |
|---|---|
| **sustained concurrent sessions** | Σ `Multiplexer.ActiveCount()` sampled over the steady window. The headline number: how many virtual sessions one AGC holds at once. SLO: avg ≥ 95% of target. |
| **throughput** | jobs acquired per second over the steady window. |
| **re-registration / job** | recycles ÷ jobs. SLO: ≈ 1.0 — confirms the Q114 model (one re-registration per job, not a long-lived runner). |
| **re-registration latency** | p50/p95/p99 of the recycle callback. **Informational only** (see caveats) — not an SLO. |
| **peak goroutines / memory** | `runtime.NumGoroutine` and `MemStats` at peak; memory/session = peak heap-inuse ÷ avg sessions. |
| **leaked goroutines** | live goroutines minus baseline after teardown. SLO: ≤ 16 — catches a multiplexer/listener goroutine leak. |

The sample under [`results/sample-run.md`](results/sample-run.md) is a real run:
1,000 sessions held on an 8-core laptop at ~115 KiB heap/session, ~490 jobs/s,
1.0 re-registration/job, zero leak.

## What it does **not** measure (fidelity boundaries)

This tier isolates the AGC's *own* scaling. It deliberately does not stand up a
cluster, so it cannot speak to anything downstream of the AGC process. Read
absolute throughput and latency with these caveats; the **sustained-sessions**,
**re-registration-per-job**, and **no-leak** results are the faithful ones.

- **No real apiserver / GitHub round-trips.** Agent Secrets go through a fake
  client and registration through an in-memory registrar, so the *latency* of a
  recycle is bounded by the fake client's in-process lock, not by real apiserver
  or GitHub-API round-trips. The faithful figure to carry forward is the
  re-registration **rate** (≈ throughput, since it is 1:1), for apiserver and
  GitHub-API capacity planning — not the recycle latency.
- **Peak goroutines/memory include the in-process broker stub.** Both the broker
  client *and* server run in this one process, so the absolute goroutine count is
  roughly double a real AGC (which talks to a remote broker). The
  per-session **trend** is what matters, not the absolute peak. The
  goroutine-leak check closes the stub first, so it measures only AGC goroutines.
- **No worker pods, CNI, image pulls, or cross-tenant network isolation.** Those
  need the Tier-A kind e2e and the M5 staging run — see
  [docs/plan/milestone-5.md §2.6](../../../../docs/plan/milestone-5.md).
- **Proxy HPA under burst** is a real-cluster behaviour, out of scope here.

## Design

See [docs/plan/milestone-5.md §2](../../../../docs/plan/milestone-5.md) for the
rationale (why an in-process Go load test is the tier that observes the claim,
and why the harness lives here rather than in a kind e2e).

## Files

| File | Role |
|---|---|
| `doc.go` | package godoc (untagged) |
| `broker_stub.go` | in-process broker v2 stub (long-poll + single-use JIT) + in-memory registrar |
| `harness.go` | per-tenant wiring, job driver, metric sampling |
| `report.go` | SLO evaluation + Markdown/log report |
| `load_test.go` | `TestAGCLoad` entrypoint (reads `LOAD_*` env knobs) |
| `results/` | committed sample run; `make load-test-full` writes `latest.md` here (gitignored) |
