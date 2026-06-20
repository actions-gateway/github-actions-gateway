# Quota-pressure conditions & metrics (Q82)

Surface namespace-`ResourceQuota` pressure on the proxy pool and on worker pods
as standard Kubernetes status conditions **and** controller-exported Prometheus
metrics, so operators can both see the problem in `kubectl describe` and alert on
it without depending on kube-state-metrics scraping CRD conditions.

Resolves E8 (k8s-best-practices audit). Replaces the original Q82 admission-webhook
sketch — see [05-security.md](../design/05-security.md) and the rationale below.

## Why not a webhook / runtime gate

- **Apply-time webhook** rejecting `proxy.maxReplicas > quota`: rejects existing
  tenants on re-apply, couples provisioning to a webhook failure mode, and can't
  see live quota *usage* at admission. The quota is the real enforcement point.
- **Runtime gate (à la Q59)**: Q59 gates *job acquisition* on the configured
  worker ceiling to avoid claimed-then-dropped jobs. The proxy claims no external
  work — over-configured `maxReplicas` just runs fewer replicas (degraded, not
  dropping). No equivalent harm, so no gate is warranted.

Conditions + metrics are the chosen low-coupling surface.

## Relationship to Q59

Q59 (`jobs_admission_rejected_total`, in-memory reservation gate) binds on the
**configured** `maxWorkers`/`priorityTiers` ceiling — normal backpressure. These
conditions bind on the **namespace ResourceQuota** — the silent failure mode Q59
does not speak to (quota tighter than `maxWorkers` ⇒ Q59 gate never fires but pod
creates are quota-rejected). Orthogonal; the worker conditions deliberately do
**not** re-expose configured-ceiling backpressure.

## Convention (the reusable pattern)

Two-tier ladder per scarce resource, abnormal-is-`True` polarity, mutually
exclusive (the error forces the warning `False`):

| Tier | Condition suffix | Meaning | Alert |
|---|---|---|---|
| Warning | `*QuotaPressure` | predictive: configured max can't fit current quota headroom (`hard − used`) | warn, no page; use `for:` (flaps with load) |
| Error | `*QuotaExceeded` | observed: creates are being rejected by quota right now | page; use `for:` to debounce |

Every alertable condition is mirrored by a controller-exported gauge (value
`1`/`0`), labelled by namespace + object name, so alerting needs no
kube-state-metrics. The gauge series is deleted when the owning object is torn
down (no stale series).

## Scope

### Proxy (ActionsGateway / GMC) — replaces the static `ProxyQuotaPressure`
- `ProxyQuotaPressure` (warn): `(maxReplicas − currentReplicas) × per-replica`
  demand exceeds `hard − used` on any namespace quota.
- `ProxyQuotaExceeded` (error): the proxy Deployment reports `ReplicaFailure=True`
  with an "exceeded quota" message (creates being rejected now).
- Metrics: `actions_gateway_proxy_quota_pressure`,
  `actions_gateway_proxy_quota_exceeded` (gauge, `{namespace,name}`), via a
  scrape-time collector reading ActionsGateway `.status.conditions`.

### Worker (RunnerGroup / AGC)
- `WorkerQuotaPressure` (warn): `(ceiling − currentActiveWorkers) × per-worker`
  demand exceeds `hard − used`, where ceiling = `maxWorkers` / max priorityTier
  threshold (`provisioner.WorkerCeiling`, the same value Q59's gate enforces).
- `WorkerQuotaExceeded` (error): the quota can't admit even one more worker pod
  (`hard − used` < a single worker's footprint) — derived from live quota state,
  no provisioner tracker. The observed-rejection complement is the existing
  `actions_gateway_quota_retries_exhausted_total` counter.
- Metrics: `actions_gateway_worker_quota_pressure`,
  `actions_gateway_worker_quota_exceeded` (gauge, `{namespace,runner_group}`),
  via a scrape-time collector reading RunnerGroup `.status.conditions`.
- New AGC read-only `resourcequotas` RBAC (agc-tenant-role fragment + marker).

## Docs to update

- Design: `03-api-contracts.md` (condition lists), `05-security.md` (threat model
  rows + RBAC posture).
- Operator: `troubleshooting.md`, `tenant-onboarding.md`, `observability.md`
  (alert examples on the new metrics), `runbook.md`.
- Convention: `docs/development/kubernetes-conventions.md`.

## Status

▶ Started 2026-06-20.
