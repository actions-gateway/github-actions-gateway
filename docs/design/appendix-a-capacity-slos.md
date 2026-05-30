# Appendix A — Capacity Targets & SLOs

← [Glossary](08-glossary.md) | [Back to index](README.md) | Next: [Appendix B — Worker Isolation →](appendix-b-worker-isolation.md)

---

The following targets are conservative defaults derived from the architectural constraints in [§2](02-architecture.md) and [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget). They are intended as starting points to be refined against real production data; operators are expected to override them based on their cluster size, GitHub plan, and workload profile.

## Latency SLOs (per-job, per-tenant)

| Metric | Target | Source | Note |
| --- | --- | --- | --- |
| Pod-creation latency (p95) | ≤ 15s | `actions_gateway_pod_creation_latency_seconds` | From `acquirejob` success to pod `Scheduled` event. Dominated by image pull on cold nodes; sub-second on warm. |
| Pod-creation latency (p99) | ≤ 60s | `actions_gateway_pod_creation_latency_seconds` | Tolerates cold-start image pull. |
| Session reacquisition after Actions Gateway Controller (AGC) restart | ≤ 2 min | derived | Equal to GitHub's redelivery window; jobs redelivered within this window suffer no observable disruption. |
| Token refresh failure budget | < 1 / hour | `actions_gateway_token_refresh_errors_total` | Anything above this rate indicates either GitHub API instability or a credential problem. |

---

## Capacity Targets (per-AGC pod, single tenant)

| Resource | Target | Rationale |
| --- | --- | --- |
| Concurrent virtual sessions (peak burst) | ≤ 1,000 | Memory-bound burst ceiling: each goroutine stack + HTTP buffer + token-manager indirection averages ~60 KiB resident; 1,000 sessions ≈ 60 MiB at peak. Steady-state cost is 1 session per RunnerGroup (~60 KiB each), far below this ceiling for typical deployments. |
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
| CPU request / limit | 10m / 100m | Defaults per `ProxyConfig`. Adjust upward if HPA lag is observed under bursty load. |
| Memory request / limit | 32 MiB / 64 MiB | Stateless CONNECT proxies have a small footprint; these defaults survive 500 concurrent tunnels with headroom. |

---

## Tenant-Aggregate Capacity (single `ActionsGateway`)

| Resource | Target | Note |
| --- | --- | --- |
| Active jobs (worker pods) | ≤ 250 | Conservative default governed by `namespaceQuota`, `maxWorkers`, or the last `priorityTiers` threshold — whichever is most restrictive. Not rate-limit-bounded under the adaptive listener model; increase this ceiling by adjusting namespace ResourceQuota and per-`RunnerGroup` concurrency controls. |
| Aggregate NamespaceQuota | 20 CPU / 40Gi memory / 50 pods | Conservative starting allocation. Adjust against observed job CPU/memory profiles. |

---

These numbers must be re-derived once two consecutive weeks of production telemetry are available. Treat them as a load-test design input, not as a contract.

---

← [Glossary](08-glossary.md) | [Back to index](README.md) | Next: [Appendix B — Worker Isolation →](appendix-b-worker-isolation.md)
