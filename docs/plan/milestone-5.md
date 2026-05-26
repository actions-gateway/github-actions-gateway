# Milestone 5 Implementation Plan — Hardening & Load Testing

← [Milestone 4](milestone-4.md) | [Back to implementation phases](../design/06-implementation-phases.md)

---

## Overview

**Goal:** Make the system production-deployable. Two themes:

1. **Packaging and posture** — ship the operator and proxy via a
   reproducible install path (Helm chart or Kustomize overlay) with
   hardened defaults that pass an automated posture audit.
2. **Load validation** — prove the design's headline capacity claim
   (1,000 concurrent virtual runner sessions across 10 tenants) on a
   staging cluster, with no dropped jobs, no cross-tenant leakage, and
   correct HPA behavior under burst.

**Duration:** Days 23–26 (per design)

**Foundation:** Everything in Milestones 1–4. Several of the M5
"hardening" sub-items overlap with workstreams in
[security.md](security.md) and have already landed there; this plan
inherits those and concentrates on what remains.

**Definition of Done:**

- A reproducible install artifact (Helm chart or Kustomize root) exists
  under `deploy/` (or `charts/`) and produces a working tenant from a
  single `helm install` / `kubectl apply -k`.
- `kube-bench` or `polaris` scan against the installed stack returns
  zero critical findings.
- `test/load/` harness simulates 1,000 concurrent sessions across 10
  tenants; load report committed with results.
- Proxy HPA verified at scale: scales up under burst, returns to
  `minReplicas` within 5 minutes of load subsiding, zero dropped jobs.
- gVisor `RuntimeClass` documented as a supported opt-in, with at
  least one staging tenant configured under it.

---

## Status at a glance

Last refreshed 2026-05-25. The "security" half of M5 landed inside
[security.md](security.md) workstreams W2/W7/W8 — the GMC already
stamps PSA labels, provisions per-tenant ResourceQuotas, and ships
hardened pod specs for AGC + proxy. What remains is packaging,
load-testing, and the posture audit.

| Sub-item | Status | Notes |
|---|---|---|
| Locked-down Pod Security Standards | ✅ Done | Security W2 — `applyNamespacePSA` stamps labels per `securityProfile`; CEL forbids `privileged: true` |
| Per-tenant `ResourceQuota` | ✅ Done | `buildResourceQuota` in [cmd/gmc/internal/controller/builder.go:225](../../cmd/gmc/internal/controller/builder.go); driven by `spec.namespaceQuota` |
| Hardened proxy pod spec (read-only root, no caps, seccomp) | ✅ Done | Security W8 — [builder.go:323-327](../../cmd/gmc/internal/controller/builder.go); full `Capabilities.Drop: ALL` + `SeccompRuntimeDefault` |
| Hardened AGC pod spec | ✅ Done | Security W8 — [builder.go:492-497](../../cmd/gmc/internal/controller/builder.go) |
| TLS hardening (AGC↔proxy) | ✅ Done | Security W7 — GMC self-signed cert + AGC pinning |
| Production Helm chart or Kustomize overlays | ❌ Open | No `charts/` or `deploy/` directory; only per-binary `cmd/*/config/` kustomize bases exist |
| `test/load/` multi-tenant load harness | ❌ Open | Directory does not exist |
| 1,000 concurrent sessions × 10 tenants — load test | ❌ Open | Blocked on harness |
| Proxy HPA verified under burst | ⚠️ Partial | Unit/integration coverage and e2e §7.3 spec for 50-job burst exist; 1,000-session scale not run |
| gVisor / Kata `RuntimeClass` opt-in | ⚠️ Documented | [Appendix B](../design/appendix-b-worker-isolation.md) documents the per-`RunnerGroup` opt-in pattern; not exercised on a real cluster |
| `kube-bench` or `polaris` scan with zero critical findings | ❌ Open | No scan run yet |

### Critical path

The packaging artifact and the load harness are independent — both can
land in parallel. The posture scan depends on packaging existing
(something to install and scan). The 1,000-session run depends on the
harness. gVisor validation depends on a cluster with the runtime
installed (operator concern, not code).

---

## 1. Packaging (Helm chart or Kustomize overlay)

### 1.1 Choose the install vehicle

Two viable shapes:

- **Kustomize overlay** under `deploy/`, composed from the existing
  `cmd/gmc/config/` and `cmd/agc/config/` kustomize bases. Lowest
  effort; reuses the kubebuilder-generated layout. Operators apply
  `kubectl apply -k deploy/`.
- **Helm chart** under `charts/actions-gateway/`. Higher effort; gives
  operators templated values, `helm upgrade --atomic` rollback, and a
  single artifact to publish to an OCI registry.

**Recommended starting point:** Kustomize overlay. The kubebuilder
bases already exist; an overlay is straightforward to assemble and
test. A Helm chart can wrap the same manifests later if operator
demand justifies the maintenance cost.

**Decision required (D-M5-1):** Helm or Kustomize for v1. Default if
undecided: Kustomize.

### 1.2 Contents of the install root

Regardless of vehicle, the artifact must produce:

- The two CRDs (`ActionsGateway`, `RunnerGroup`) installed cluster-wide
- The GMC Deployment + RBAC + webhook configuration in `gmc-system`
- The IP-range refresh schedule and proxy image references
- An optional sample `ActionsGateway` CR with safe defaults
- Image references pinned by digest (per security plan "out of scope
  but flagged" — image-digest pinning recommendation)

### 1.3 Operator-facing values

If Helm: enumerate every value an operator might tune
(`gmc.image`, `proxy.image`, `agc.image`, `leaderElection.enabled`,
`metrics.enabled`, `securityProfile` default, `kindOfRegistry`). If
Kustomize: provide overlay variants (`overlays/staging/`,
`overlays/production/`) that demonstrate the same axes via patches.

### 1.4 Files

```
deploy/                              # if Kustomize
├── kustomization.yaml
├── overlays/
│   ├── staging/
│   └── production/
└── samples/
    └── actionsgateway-sample.yaml
```

or

```
charts/actions-gateway/              # if Helm
├── Chart.yaml
├── values.yaml
├── templates/
└── README.md
```

---

## 2. Load testing harness (`test/load/`)

### 2.1 Architecture

The harness needs to drive load *into* the system, not just measure
it. Two layers:

- **Tenant generator** — applies N `ActionsGateway` CRs (default 10)
  with distinct namespaces and RunnerGroups, waits for them to reach
  `Ready`.
- **Job generator** — for each tenant, drives M concurrent virtual
  jobs (default 100, total 1,000) through the fakegithub broker used
  by existing e2e tests. Each "job" is a session that the AGC
  acquires, holds for a configurable duration, and releases.

Reusing `fakegithub` (per [e2e_suite_test.go](../../cmd/gmc/test/e2e/e2e_suite_test.go))
avoids real GitHub API quota and keeps the test deterministic.

### 2.2 What it measures

| Metric | How to assert |
|---|---|
| Sessions concurrently held | Sum `actions_gateway_active_sessions` across all RunnerGroups; assert ≥ 1,000 sustained ≥ 60s |
| Dropped messages | Count `actions_gateway_message_poll_errors_total` minus expected (rate-limit) errors; assert 0 unexpected |
| Cross-tenant resource visibility | After load, walk each tenant namespace and assert no pods/secrets carry labels from another tenant |
| Goroutine deadlocks | `pprof` goroutine dump at peak; assert no goroutine has been blocked > 5 min on a channel |
| Proxy HPA scale-up | `actions_gateway_proxy_replicas` (or HPA status) ≥ `minReplicas + 1` during burst |
| Proxy HPA scale-down | Within 5 min of load drop, replicas return to `minReplicas` |
| Job acquisition latency p95 | Histogram percentile from `actions_gateway_pod_creation_latency_seconds`; compare against Appendix A SLO |

### 2.3 Run modes

- `make load-test-quick` — 10 tenants × 10 jobs = 100 concurrent
  (smoke; ~2 min)
- `make load-test-full` — 10 tenants × 100 jobs = 1,000 concurrent
  (acceptance; ~10 min on a 3-node kind cluster, longer on staging)

### 2.4 Output

- JUnit XML for CI integration
- Markdown report committed to `test/load/results/<date>.md` capturing
  cluster size, observed metrics, SLO comparisons
- Optional flame graph from `pprof` for any hot spots

### 2.5 Files

```
test/load/
├── README.md                  # how to run, what it measures, expected results
├── harness/
│   ├── tenants.go             # spawns ActionsGateway CRs
│   ├── jobs.go                # drives fakegithub job dispatch
│   └── assertions.go          # SLO + leak checks
├── cmd/
│   └── load-driver/main.go    # CLI entry point
└── results/
    └── .gitkeep
```

---

## 3. Posture audit (`kube-bench` / `polaris`)

### 3.1 Tool choice

- **kube-bench** — CIS benchmark; cluster-level (kubelet, control
  plane). Useful but not the most relevant signal for *this* operator —
  most CIS findings apply to cluster config, not workload manifests.
- **polaris** — workload-level (pod specs, NetworkPolicy, RBAC).
  Higher signal for finding regressions in our generated manifests.
- **kubescape** — combines both; CI-friendly. Worth evaluating.

**Recommended:** polaris in CI for workload posture; kube-bench as a
one-shot manual run against the staging cluster.

### 3.2 Integration

- Add a CI job that runs `polaris audit --format=score --resource <our manifests>`
  against the rendered output of the install artifact (§1) and fails on
  any "danger" finding.
- Document the one-shot `kube-bench` procedure in
  [docs/operations/runbook.md](../operations/runbook.md) as a
  pre-production checklist item.

### 3.3 Expected findings to address

Based on the current pod specs, polaris is likely to flag:

- Missing `livenessProbe` / `readinessProbe` on worker pods (by
  design — workers are short-lived; suppress)
- `runAsNonRoot` is already set on AGC and proxy; workers depend on
  tenant `PodTemplate` (suppress per-namespace per `securityProfile`)
- Image tags rather than digests (already in the security "out of
  scope" list; address as part of §1.2)

---

## 4. Sandbox runtime (gVisor / Kata) validation

[Appendix B](../design/appendix-b-worker-isolation.md) documents the
per-`RunnerGroup` opt-in. M5 adds: validate the path works on a real
cluster with at least one of the two runtimes installed.

### 4.1 What to test

- Install gVisor on a staging cluster (operator concern, but pin the
  exact version in `docs/operations/runbook.md`).
- Configure a `RunnerGroup` with `podTemplate.spec.runtimeClassName: gvisor`.
- Dispatch a job through the load harness and confirm the worker pod
  runs to completion under the sandbox.
- Measure overhead (job duration delta vs. `runc`); document.

### 4.2 Out of scope

- Kata Containers validation (parallel work; document as supported,
  validate when there's demand)
- GMC-side `RuntimeClass` installation (cluster-admin concern)

---

## 5. Test plan

### 5.1 Existing coverage to inherit

The following e2e §7.3 specs already exist; M5 reuses them at higher
scale rather than reimplementing:

- "Resource cleanup under load" — 50 sequential jobs across 5
  tenants. M5 scales to 1,000 / 10.
- "Proxy HPA scaling" — 50 concurrent. M5 scales to 100+ per tenant.

### 5.2 New tests

| Scenario | Pass criterion |
|---|---|
| Tenant generator | 10 `ActionsGateway` CRs applied; all reach `Ready` within 90s |
| Burst load — 1,000 concurrent | Sustained for 60s; no dropped messages; no goroutine leaks |
| Cross-tenant isolation under load | After burst, each tenant's namespace contains only its own resources |
| HPA scale-up under burst | Proxy replicas exceed `minReplicas` during peak |
| HPA scale-down post-burst | Returns to `minReplicas` within 5 min |
| Posture audit | polaris score ≥ 90; no danger findings |
| gVisor smoke | Single job runs to completion under `runtimeClassName: gvisor` |

---

## 6. Open decisions

| ID | Question | Affects | Default if undecided |
|---|---|---|---|
| D-M5-1 | Helm chart vs Kustomize overlay for v1 install artifact | §1 | Kustomize overlay |
| D-M5-2 | polaris vs kubescape for CI posture audit | §3 | polaris (narrower scope, easier to gate CI on) |
| D-M5-3 | Load harness language — Go (consistent with rest of repo) vs k6/Locust (richer reporting) | §2 | Go; reuse existing fakegithub + controller-runtime client |
| D-M5-4 | Sandbox runtime to validate — gVisor, Kata, or both | §4 | gVisor (lower install cost on most cloud K8s) |

---

## 7. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| 1,000-session run exposes goroutine leak undetected by current goleak tests | Medium | High | Run harness with `-race` and periodic `pprof` snapshots; capture full goroutine dump on assertion failure |
| polaris flags rules we've explicitly chosen against the design (e.g. no liveness probe on workers) | High | Low | Suppression list lives in `polaris.yaml` with a comment per suppression citing the design doc |
| Staging cluster doesn't support gVisor (no nested-virt, restrictive cloud) | Medium | Low | Mark gVisor validation as opt-in; document the runtime requirement in the runbook |
| Helm chart maintenance overhead if chosen and demand never materializes | Medium | Medium | Default to Kustomize per D-M5-1; revisit once a real operator asks for Helm |
| Load harness becomes flaky in CI (timing-sensitive HPA assertions) | Medium | Medium | Run full load test nightly, not per-PR; per-PR runs `load-test-quick` (100 concurrent) |

---

## 8. Deferred / out of scope

- **Cost benchmarking** under realistic load — covered by
  [docs/plan/docs.md §3.4](docs.md) and
  [docs/design/appendix-f-cost-model.md](../design/appendix-f-cost-model.md).
  Use the M5 harness as data source once it lands.
- **Long-running soak test** (24h+) — useful but separate; not in the
  4-day design budget.
- **Chaos testing** (pod kill, network partition) — defer to a future
  hardening pass.
- **Cluster autoscaler interactions** — operator concern; document
  recommended cluster-autoscaler settings in the runbook, do not test
  in M5.
