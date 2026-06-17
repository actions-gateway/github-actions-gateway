# Q51 — Reconcile documented vs emitted Prometheus metrics

Bring the operator-facing metrics docs into line with the metrics the code actually
registers. The six-layer docs audit (Layer 3) found six documented metric names that
the docs reference but the code does not register, so operators are pointed at
telemetry they cannot scrape. This task makes a per-metric decision — implement,
re-point, or mark `(planned)` — and applies it in code and docs.

## Per-metric decisions

| Documented metric | Decision | Rationale |
|---|---|---|
| `actions_gateway_pod_creation_latency_seconds` | **Implement** (AGC) | Headline pod-startup SLO (Appendix A). Clear, low-risk instrumentation point: the `InformerPodWaiter` already observes every worker-pod event, so the histogram is observed from the pod's own timestamps (container start − pod creation) when the pod resolves — no hot-path change, clock-clean (kubelet/apiserver clocks), one observation per pod. |
| `actions_gateway_managed_gateways` | **Implement** (GMC) | Total `ActionsGateway` CRs managed. The 24h IP-range refresh loop is far too stale for a gauge, and a per-reconcile List adds hot-path load; the clean low-risk implementation is a custom `prometheus.Collector` that lists the CRs from the cached reader at scrape time (no staleness, no hot-path cost). Backs the security-operations runaway-CR alert. |
| `actions_gateway_reconcile_errors_total` | **Re-point** (docs only) | controller-runtime already emits `controller_runtime_reconcile_errors_total{controller="…"}` for every manager-driven controller (both GMC and AGC). The documented `actions_gateway_`-prefixed name is a naming error pointing operators at a metric that does not exist. Fix the docs to reference the built-in; no code needed. |
| `actions_gateway_ip_range_updates_total` | **Implement** (GMC) | NetworkPolicy egress-rule refreshes from the GitHub meta API. Clear, low-risk point already called out in milestone-4: increment a counter (label `namespace`) on each successful NetworkPolicy patch in `IPRangeReconciler.patchNetworkPolicy`. |
| `actions_gateway_proxy_replicas` | **Mark (planned)** | Only referenced in `milestone-5.md` (proxy HPA autoscaling, not yet built) — not in any operator-facing doc, so no operator is currently misled. Annotate `(planned — Milestone 5)`. |
| `actions_gateway_proxy_tunnel_duration_seconds` | **Already implemented** | Registered in `cmd/proxy/proxy.go` (M-17/M-18). No action; docs already accurate. (Listed in the audit table as ✅; included here for completeness.) |

## Implementation notes

- **pod_creation_latency_seconds** measures pod creation → the runner container
  starting (the earliest non-zero container `StartedAt`), which includes scheduling
  and image pull — the dominant, SLO-relevant cost. Observed inside the
  `InformerPodWaiter` at terminal resolution, deduped via the existing
  resolve-once-per-pod path. Buckets span 0.5s–300s to bracket the p95 ≤ 15s /
  p99 ≤ 60s SLO.
- **GMC metrics** live in a new `cmd/gmc/internal/controller/metrics.go`, registered
  with the controller-runtime `metrics.Registry` so they appear on the same `/metrics`
  endpoint as the built-in controller-runtime metrics.

## Docs touched

`docs/operations/observability.md`, `runbook.md`, `troubleshooting.md`, `upgrade.md`,
`docs/design/02-architecture.md`, `appendix-a-capacity-slos.md`, `plan/milestone-5.md`,
and `plan/docs-six-layer-audit.md` (mark the Layer 3 finding resolved).
