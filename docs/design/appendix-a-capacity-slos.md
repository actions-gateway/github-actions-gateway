# Appendix A — Capacity Targets & SLOs

← [Glossary](08-glossary.md) | [Back to index](README.md) | Next: [Appendix B — Worker Isolation →](appendix-b-worker-isolation.md)

---

The following targets are conservative defaults derived from the architectural constraints in [§2](02-architecture.md) and [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget). They are intended as starting points to be refined against real production data; operators are expected to override them based on their cluster size, GitHub plan, and workload profile.

## Latency SLOs (per-job, per-tenant)

| Metric | Target | Source | Note |
| --- | --- | --- | --- |
| Pod-creation latency (p95) | ≤ 15s | `actions_gateway_pod_creation_latency_seconds` | From worker pod creation to runner container start (scheduling + image pull). Dominated by image pull on cold nodes; sub-second on warm. |
| Pod-creation latency (p99) | ≤ 60s | `actions_gateway_pod_creation_latency_seconds` | Tolerates cold-start image pull. |
| Session reacquisition after Actions Gateway Controller (AGC) restart | ≤ 2 min | derived | Equal to GitHub's redelivery window; jobs redelivered within this window suffer no observable disruption. |
| Token refresh failure budget | < 1 / hour | `actions_gateway_token_refresh_errors_total` | Anything above this rate indicates either GitHub API instability or a credential problem. |

---

## Capacity Targets (per-AGC pod, single tenant)

| Resource | Target | Rationale |
| --- | --- | --- |
| Concurrent virtual sessions (peak burst) | ≤ 1,000 | Memory-bound burst ceiling: each goroutine stack + HTTP buffer + token-manager indirection averages ~60 KiB resident (a deliberately conservative sizing figure — the AGC's own per-session structures measure **~12 KiB**, see [Per-session memory & density](#per-session-memory--density-measured)); 1,000 sessions ≈ 60 MiB at peak. Steady-state cost is 1 session per RunnerGroup, far below this ceiling for typical deployments. |
| Memory request | 2 GiB | Sized for the peak burst ceiling of 1,000 concurrent goroutines (~60 MiB) with 4× safety margin for Go runtime overhead, heap churn, and reconcile storms. Actual steady-state resident size will be much smaller. |
| Memory limit | 4 GiB | Allows transient bursts during reconcile storms without triggering OOM. |
| CPU request | 500m | Predominantly I/O-bound; request reflects baseline scheduling weight rather than steady CPU draw. |
| CPU limit | 2 (cores) | Permits short bursts during reconcile churn or token refresh contention without throttling. |

---

## Capacity Targets (per GitHub App installation)

| Resource | Target | Source |
| --- | --- | --- |
| Concurrent sessions per installation | ≤ 250 | Bounded by [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget) rate-limit math: ~72 message polls/hr/session against the 15,000/hr installation budget. |
| Sustained `RateLimited` condition | < 1 min | Anything longer indicates the operator is over budget and should shard across installations. |

---

## Capacity Targets (per proxy pod)

| Resource | Target | Note |
| --- | --- | --- |
| Concurrent CONNECT tunnels | ≤ 500 | File-descriptor-bound; tune the proxy pod `ulimit nofile` if increasing. |
| CPU request / limit | 10m / 500m | Defaults per `ProxyConfig`. The 500m limit (not 100m) keeps the pod from throttling before the HPA's 60%-utilization signal trips under CONNECT load. Adjust upward if HPA lag is observed under bursty load. |
| Memory request / limit | 32 MiB / 64 MiB | Stateless CONNECT proxies have a small footprint; these defaults survive 500 concurrent tunnels with headroom. |

---

## Tenant-Aggregate Capacity (single `ActionsGateway`)

| Resource | Target | Note |
| --- | --- | --- |
| Active jobs (worker pods) | ≤ 250 | Conservative default governed by the platform-owned namespace `ResourceQuota`, `maxWorkers`, or the last `priorityTiers` threshold — whichever is most restrictive. Not rate-limit-bounded under the adaptive listener model; increase this ceiling by adjusting the namespace ResourceQuota and per-`RunnerGroup` concurrency controls. |
| Aggregate namespace ResourceQuota | 20 CPU / 40Gi memory / 50 pods | Conservative starting allocation. Platform-owned (set on the namespace, not the CR). Adjust against observed job CPU/memory profiles. |

---

## Per-session memory & density (measured)

The peak-burst sizing above uses a deliberately conservative ~60 KiB/session. To
pin the AGC's *actual* per-session overhead — the figure behind the
density-versus-pod-per-runner claim — `TestAGCPerSessionMemory`
(`cmd/agc/test/load/mem_test.go`, `make mem-profile`) isolates it locally, with
no cluster, no real broker, and crucially **no in-process broker stub** (the stub
inflated the earlier ~127 KiB figure because its server side runs in the same
process).

**Methodology.** The probe drives the real multiplexing core
(`listener.Multiplexer` + `agentpool.Pool` + per-goroutine `broker.Client`) but
replaces the broker stub with `memTransport`, an in-process `http.RoundTripper`
that answers the OAuth/CreateSession/GetMessage calls with canned responses and
**no server, socket, or per-session server-side state**. `GET …/message` parks
the caller on its request context, so each of the 1,000 started listeners rests
in exactly one goroutine blocked in its long-poll — the steady idle-session
state. It then takes a three-point heap+stack differential: shared infra only →
plus N pooled agents and empty multiplexers → plus all N goroutines parked. The
last delta (`mFull − mAgents`) is the marginal cost of one more concurrent
session and excludes both the agent pool and the fake k8s client's retained
Secrets (an apiserver-side cost in production, not AGC memory).

**Result (1,000 sessions, Go on `darwin/arm64`):**

| Component | Per session |
|---|---|
| listener goroutine stack | ~8.1 KiB |
| heap (`broker.Client` + live session state: sessionID, AES key, scoped logger) | ~4.1 KiB |
| **AGC-only total (measured)** | **~12.2 KiB** |

The pre-registered agent struct (Ed25519 key + credentials, no JIT blob in this
path) adds sub-KiB on top; the agent *Secret* itself is apiserver-resident in
production. The measured ~12.2 KiB is **~5× below** the ~60 KiB design estimate —
the gap is the per-connection HTTP transport buffers that an active long-poll
holds in production, which the in-process transport omits. The design estimate is
therefore confirmed as a conservative upper bound.

**Density versus pod-per-runner.** Against ARC's `Runner.Listener` (~256 MiB
resident for the full .NET runtime):

```
256 MiB ÷ 60 KiB (conservative design figure) ≈ 4,400×   ← the published "4,000×"
256 MiB ÷ 12.2 KiB (measured AGC structures)  ≈ 21,000×   ← floor excludes HTTP-conn buffers
```

The measurement **confirms the published ~4,000× claim and shows it is
conservative**: even adding a generous per-connection HTTP-buffer allowance to the
measured ~12 KiB keeps the per-session cost well under the 60 KiB the 4,000×
figure assumes. We retain ~4,000× as the headline because it is the defensible,
conservative number; the higher ratios above follow directly from the math but
lean on assumptions the local probe cannot fully exercise.

---

These numbers should still be re-derived once two consecutive weeks of production telemetry are available. Treat the locally-measured figures as validated lower bounds on efficiency, not as a production-scale contract.

> **Validation status.** The session-multiplexing core **has been load-tested**, and its **per-session memory is now pinned**. The in-process harness (`cmd/agc/test/load/`, Q13; `make load-test-quick`) holds **~1,000 concurrent virtual sessions in a single AGC** — a representative run sustained avg 998/1,000 with **zero goroutine leak** and 1.0 re-registrations per job (the single-use model under load). The faithful results from that tier are the **sustained-session count, the no-leak guarantee, and the re-registration rate**; it deliberately stubs the apiserver, registrar, and broker, so it does not speak to real apiserver/GitHub latency or worker-pod scheduling. The earlier ~127 KiB/session figure was an **upper bound inflated by the in-process broker stub**; the stub-free probe above (Q181) isolates the AGC's own structures at **~12.2 KiB/session**, confirming the ~4,000× density claim is conservative. One caveat remains: the **real-cluster, real-GitHub scale run** — worker-pod scheduling and cross-tenant network at full concurrency — is still deferred. Operators should size against their own observed telemetry rather than treat these ceilings as proven.

---

← [Glossary](08-glossary.md) | [Back to index](README.md) | Next: [Appendix B — Worker Isolation →](appendix-b-worker-isolation.md)
