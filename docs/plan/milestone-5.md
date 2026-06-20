# Milestone 5 Implementation Plan — Hardening & Load Testing

← [Milestone 4](milestone-4.md) | [Back to implementation phases](../design/06-implementation-phases.md)

---

## Overview

**Goal:** Make the system production-deployable. Two themes:

1. **Packaging and posture** — ship the operator and proxy via a
   reproducible install path (Helm chart, per §1.1) with
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

- A reproducible install artifact (Helm chart, per D-M5-1 §1.1) exists
  under `charts/actions-gateway/` and produces a working tenant from a
  single `helm install`.
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
| Production Helm chart (`charts/actions-gateway/`) | ⚠️ Partial | Chart exists ([q12-helm-chart.md](q12-helm-chart.md), Q12): GMC core (CRDs, RBAC, webhook, VAP, NetworkPolicies) — `helm lint` + `helm template` + `kubeconform` clean offline, both cert modes. Live `helm install`→working-tenant validation pending (track A, needs creds + kind); polaris posture scan landed (Q14 — §3.2), CI drift check folds into Q66. `cmd/*/config/` kustomize bases stay the dev source-of-truth |
| `cmd/agc/test/load/` load harness | ✅ Done | Q13 — in-process listener-core harness (§2); `make load-test-quick`/`load-test-full`, single-use re-registration modelled (Q114) |
| 1,000 concurrent sessions × 10 tenants — load test | ✅ Done (in-process) | Q13 — 1,000 sustained sessions across 10 tenants pinned in-process (§2.2); real-pod/cross-tenant-network scale stays the staging-cluster item (§2.6) |
| Proxy HPA verified under burst | ⚠️ Partial | Unit/integration coverage and e2e §7.3 spec for 50-job burst exist; 1,000-session scale not run |
| gVisor / Kata `RuntimeClass` opt-in | ⚠️ Documented | [Appendix B](../design/appendix-b-worker-isolation.md) documents the per-`RunnerGroup` opt-in pattern; not exercised on a real cluster |
| `kube-bench` or `polaris` scan with zero critical findings | ✅ Done | polaris audits the rendered chart in CI (Q14), gating on `danger` findings — score 100, zero danger; `make polaris-scan` mirrors it. kube-bench is a live-cluster CIS scan → documented as a pre-production runbook in [security-operations.md](../operations/security-operations.md#cis-benchmark-posture--kube-bench-manual-pre-production) |

### Critical path

The packaging artifact and the load harness are independent — both can
land in parallel. The posture scan depends on packaging existing
(something to install and scan). The 1,000-session run depends on the
harness. gVisor validation depends on a cluster with the runtime
installed (operator concern, not code).

---

## 1. Packaging (Helm chart)

### 1.1 Install vehicle — decided: Helm chart

**Decision (D-M5-1): ship a Helm chart** under
`charts/actions-gateway/` as the 1.0 install artifact. The existing
`cmd/gmc/config/` and `cmd/agc/config/` kustomize bases are **retained
as the dev/CI source of truth** (they back `make manifests` and the
envtest/e2e tiers) — they are *not* a second shipped distribution path.

**Why Helm over a Kustomize overlay.** The original plan defaulted to
Kustomize on author-effort grounds ("the bases already exist"). That is
the wrong axis for a *distribution* vehicle. GAG is a third-party-
installed platform operator — the same category as
actions-runner-controller, cert-manager, and prometheus-operator, all
of which ship Helm as their primary artifact. The reasons that matters
here:

- **Versioned, named releases.** "Install GAG 1.0" becomes one OCI
  chart ref, not "clone the repo and `apply` this overlay at this SHA."
- **Real day-2 lifecycle.** `helm upgrade` / `rollback --atomic` track
  what was installed. Kustomize has no installed-state notion; upgrades
  are `apply` + manual prune, and removed resources orphan silently.
- **A values UX for the axes operators actually tune** (`gmc.image`,
  `proxy.image`, `agc.image`, `leaderElection.enabled`,
  `metrics.enabled`, default `securityProfile`, `certManager.enabled`)
  instead of patch overlays each operator has to fork and re-base.

Kustomize's genuine edge — GitOps where you own the repo — is an
in-house pattern; it is the weaker fit for an artifact handed to other
orgs.

**Effort is smaller than it looks.** The repo is on kubebuilder 4.14,
whose `helm/v1-alpha` plugin scaffolds a chart from the existing
`config/` bases (`kubebuilder edit --plugins=helm/v1-alpha`) rather
than hand-authoring templates. Keep the kustomize bases authoritative
for dev; generate and then maintain the chart as the shipped artifact.

**Two gotchas this introduces (both must be handled in §1.2):**

1. **cert-manager dependency.** The validating webhook's serving cert
   comes from kubebuilder's `config/certmanager/` today (CA injected
   via cert-manager annotation), so cert-manager is already an install
   prerequisite under *either* vehicle. Helm handles it better: expose
   a `certManager.enabled` value and ship a self-signed-cert hook as
   the fallback so operators without cert-manager can still install.
2. **Helm never upgrades CRDs.** Charts' `crds/` directory installs CRDs
   but skips them on `helm upgrade`. For an API still on `v1alpha1`
   with field changes ahead, that breaks day-2. Template the two CRDs
   into `templates/` with `helm.sh/resource-policy: keep` so they
   upgrade — accepting the trade-off that Helm no longer delete-protects
   them. Record this in [upgrade.md](../operations/upgrade.md).

### 1.2 Contents of the chart

The chart must produce:

- The two CRDs (`ActionsGateway`, `RunnerGroup`) — in `templates/` with
  `helm.sh/resource-policy: keep` (per §1.1 gotcha 2), not `crds/`, so
  they upgrade.
- The GMC Deployment + RBAC + webhook configuration in `gmc-system`.
- The webhook serving cert: cert-manager-issued when
  `certManager.enabled=true` (default), self-signed-cert hook otherwise
  (per §1.1 gotcha 1).
- The IP-range refresh schedule and proxy image references.
- An optional sample `ActionsGateway` CR with safe defaults
  (`values.yaml`-gated, off by default).
- Image references pinned by digest (per security plan image-digest
  pinning recommendation) — chart `appVersion` tracks the release tag,
  `values.yaml` carries the digests.

### 1.3 Operator-facing values

Enumerate every value an operator might tune in `values.yaml`:
`gmc.image`, `proxy.image`, `agc.image`, `leaderElection.enabled`,
`metrics.enabled`, `securityProfile` (default), `certManager.enabled`,
and `sampleGateway.create`. Each gets a documented default and a comment
in `values.yaml`; the README table mirrors them.

### 1.4 Files

```
charts/actions-gateway/
├── Chart.yaml                       # appVersion = release tag
├── values.yaml                      # every tunable from §1.3, documented
├── templates/
│   ├── crds/                        # CRDs as templates (resource-policy: keep)
│   ├── gmc-deployment.yaml
│   ├── rbac.yaml
│   ├── webhook.yaml
│   ├── certmanager.yaml             # gated on .Values.certManager.enabled
│   ├── selfsigned-cert-hook.yaml    # fallback when certManager disabled
│   ├── iprange-schedule.yaml
│   └── sample-gateway.yaml          # gated on .Values.sampleGateway.create
└── README.md                        # values reference + install/upgrade flow
```

The kustomize bases under `cmd/*/config/` stay in place as the dev/CI
source of truth and the scaffolding input for the chart (§1.1); they are
not published.

---

## 2. Load testing harness (`cmd/agc/test/load/`) — IMPLEMENTED (Q13)

### 2.0 What the headline claim actually is, and which tier pins it

The pitch is **thousands of virtual runner sessions per AGC**: the AGC
multiplexes each runner session as a goroutine that long-polls the
broker, and spawns an ephemeral worker pod *only* when a job is
acquired. The capacity risk in that claim is entirely inside one AGC
process — goroutine, memory, connection, and per-job re-registration
scaling — **not** in the cluster's ability to schedule pods (that is a
node-count question, separate from the AGC). So the tier that observes
the claim is an **in-process Go load test** that drives the AGC's real
listener-multiplexing core (`listener.Multiplexer` + `agentpool.Pool` +
a per-goroutine `broker.Client`) at scale against an in-process broker,
**not** a kind e2e standing up 1,000 real worker pods. A pod-per-session
e2e would mostly measure kind/kubelet, would not run on a dev box, and
would not isolate the AGC's own scaling — the property under test.

The harness therefore lives under `cmd/agc/test/load/` (it must, to
import the `agc/internal/...` listener and agentpool packages) behind a
`//go:build load` tag — the same tier-isolation pattern the envtest
suites use with `//go:build integration`. It needs **no cluster and no
real GitHub credentials**: agent Secrets go through a controller-runtime
fake client, registration through an in-memory registrar, and the broker
through an in-process stub.

### 2.1 Architecture

The harness reconstructs, per tenant, exactly what
`RunnerGroupReconciler.getOrCreateMultiplexer` wires in production —
same factory shape, same `ClaimAgent`/`ReleaseAgent`/`MarkConsumed`/
`Recycle` callbacks — then drives it under load. Three pieces:

- **Tenant generator** — builds N independent RunnerGroups (default 10),
  each with its own `agentpool.Pool` (fake-client Secrets, in-memory
  registrar) and `listener.Multiplexer` sized to M listeners.
- **In-process broker stub** (`broker_stub.go`) — implements the broker
  v2 wire protocol (`/token`, `/session`, `/message`, `/acquirejob`,
  `/renewjob`) with two production-faithful behaviours the load model
  depends on: a **long-poll hold** on `GET /message` (so idle sessions
  don't busy-spin to idle-shutdown, mirroring the real ~50s broker hold
  — Q148) and the **single-use JIT lifecycle** (Q114): acquiring a job
  consumes the delivering session's agent, so the goroutine must
  re-register before it can poll again.
- **Job driver** — keeps every session saturated so the pool ramps to M
  listeners per tenant and stays there; each delivered job costs one
  `AcquireJob` + one agent **re-registration** (the Q114 per-job cost
  this harness exists to measure — it never assumes a long-lived
  runner). `LOAD_JOB_DURATION` sets how long a session "holds" a job
  (the simulated worker-pod runtime) before recycling, which tunes the
  blend between concurrency-holding and re-registration churn.

A virtual runner session **is** a listener goroutine; sustained
concurrent sessions = the sum of `Multiplexer.ActiveCount()` across
tenants (cross-checked against `actions_gateway_active_sessions` and
`runtime.NumGoroutine`).

### 2.2 What it measures

| Metric | How |
|---|---|
| Concurrent virtual sessions sustained | min/avg/max of Σ `Multiplexer.ActiveCount()` sampled across the steady-state window; assert avg ≥ target (default 1,000) |
| Throughput | `AcquireJob` count over the steady window → jobs/sec |
| Per-job re-registration cost (Q114) | recycles/job (asserted ≈ 1.0) and recycle-latency p50/p95/p99, timed around the `RecycleAgent` callback (deregister + register + Secret write + token + CreateSession) |
| Session-establishment latency | p50/p95/p99 of `CreateSession` round-trip under load (Q134 territory) |
| Memory per session | `runtime.MemStats` HeapInuse/Sys at peak ÷ concurrent sessions |
| Goroutine cost & leak check | peak `runtime.NumGoroutine`; after teardown, assert it returns to within slack of the pre-test baseline (catches Risk-1 leaks) |
| Unexpected poll errors | Σ `actions_gateway_message_poll_errors_total` for non-rate-limit reasons; assert 0 |

### 2.3 Run modes

- `make load-test-quick` — 10 tenants × 100 listeners = 1,000 concurrent
  sessions, short steady window (smoke; ~1 min). Also the compile/lint
  smoke for the `load`-tagged code.
- `make load-test-full` — 10 tenants × 100 listeners = 1,000 concurrent,
  longer window with a realistic job hold (acceptance; ~3–5 min).

Every knob is an env var (`LOAD_TENANTS`, `LOAD_LISTENERS_PER_TENANT`,
`LOAD_DURATION`, `LOAD_JOB_DURATION`, `LOAD_REPORT`, …) so the same
target scales up on a bigger box without code edits.

### 2.4 Output

- Key metrics printed to the test log and asserted as pass/fail SLOs.
- A Markdown report (path from `LOAD_REPORT`) capturing host shape,
  knobs, observed metrics, and SLO comparisons; a sample committed under
  `cmd/agc/test/load/results/`.
- `go test -json | go-junit-report` yields JUnit XML for CI (documented
  in the README rather than hand-rolled).

### 2.5 Files

```
cmd/agc/test/load/
├── README.md            # how to run, what it measures, how to read results
├── doc.go               # package doc (untagged stub so the dir always has a home)
├── broker_stub.go       # in-process broker v2 stub + in-memory registrar (//go:build load)
├── harness.go           # per-tenant wiring, job driver, metric sampling (//go:build load)
├── report.go            # SLO eval + Markdown/log report (//go:build load)
├── load_test.go         # TestAGCLoad entrypoint, reads env knobs (//go:build load)
└── results/
    └── <date>.md        # committed sample run
```

### 2.6 Fidelity boundaries (what this tier does *not* measure)

Documented in the README so results are not over-read:

- **Apiserver Secret-write cost.** Recycle writes an agent Secret; the
  fake client makes that near-instant, so the harness counts
  re-registrations and times the in-process recycle but **understates**
  the real per-job apiserver write load. The recycle *rate* it reports
  is the figure to carry into apiserver capacity planning.
- **Real worker pods, CNI, image pulls, cross-tenant network isolation.**
  Out of scope here; those belong to the Tier-A kind e2e (§5.1) and the
  staging run below.
- **Proxy HPA under burst** (§2.2 rows in the original plan) — an
  HPA/Deployment-status behaviour that needs a real cluster; tracked
  with the staging run, not this harness.

These are the multi-tenant-on-a-real-cluster items the original plan
sketched; they remain the **staging-cluster** half of M5 (DoD bullets 3
and 4) and are not regressed by landing the in-process harness — which
pins the part of the headline claim that lives inside the AGC.

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

### 3.2 Integration — IMPLEMENTED (Q14)

Both halves landed:

- **polaris (automated, gating).** A `polaris` job in
  [`.github/workflows/security-scan.yml`](../../.github/workflows/security-scan.yml)
  renders the Helm chart (`helm template`, digest-pinned to reflect the
  production posture) and runs
  `polaris audit --merge-config --config charts/actions-gateway/polaris.yaml --set-exit-code-on-danger`.
  It **fails the PR on any `danger` finding** (decision below) and runs on every
  PR touching the chart or `Makefile`, plus every push to `main`.
  [`make polaris-scan`](../../Makefile) runs the identical gate locally.
- **kube-bench (manual, documented).** kube-bench is a CIS scan against a
  **live node**, so it cannot run in our manifest-only CI. The expected
  pre-production procedure (run the upstream Job, triage `[FAIL]`s, which are
  cluster-admin vs. chart concerns) is documented as an operator runbook in
  [security-operations.md](../operations/security-operations.md#cis-benchmark-posture--kube-bench-manual-pre-production).

**Decision — gate on `danger`, report `warning` (block vs. report-only).**
`danger` findings are real security regressions (privileged container, host
namespace, dangerous capabilities, missing `securityContext`, a floating
`:latest` image tag) — a chart change that introduces one must not merge, so the
gate blocks. `warning` findings are heuristic and frequently false-positive
against a Helm-packaged operator chart, so blocking on them would red-gate
unrelated work; they are printed for visibility instead. The false-positive
warnings are tuned to `ignore` in
[`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml)
(via `--merge-config`, so every default `danger` check stays active), each with
a justifying comment. The digest-pinned default install scores **100** with zero
danger findings.

### 3.3 Findings triaged (Q14)

The audit surfaced one `danger` and five `warning`s. Triage (fix what the chart
should fix; record justified exceptions — never regress a secure default):

| Finding | Severity | Disposition |
|---|---|---|
| `tagNotSpecified` (`:latest`) | danger | **Kept gating.** Production installs pin `gmc.image.digest`; the CI scan renders with a digest so this passes, and an un-pinned `:latest` install still fails the gate. |
| `topologySpreadConstraint` | warning | **Fixed in chart.** Added a soft (`ScheduleAnyway`) hostname spread to the GMC Deployment — `replicaCount: 2` + the PDB only deliver HA if the two pods aren't co-located. Configurable via `topologySpreadConstraints` in `values.yaml`. |
| `automountServiceAccountToken` | warning | **Exception.** The controller-manager requires its SA token to reach the apiserver (controller-runtime). |
| `missingNetworkPolicy` | warning | **Exception.** The chart ships GMC NetworkPolicies; polaris can't match them across documents in a static render. |
| `pullPolicyNotAlways` | warning | **Exception.** Images are digest-pinned (immutable); `IfNotPresent` is correct and avoids a redundant pull. |
| `metadataAndInstanceMismatched` | warning | **Exception.** Helm sets `app.kubernetes.io/instance` to the release name by convention. |

No secure default was relaxed to silence a finding.

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
| D-M5-1 | Helm chart vs Kustomize overlay for v1 install artifact | §1 | **Decided: Helm chart** (§1.1) — kustomize bases retained as dev source-of-truth |
| D-M5-2 | polaris vs kubescape for CI posture audit | §3 | polaris (narrower scope, easier to gate CI on) |
| D-M5-3 | Load harness language — Go (consistent with rest of repo) vs k6/Locust (richer reporting) | §2 | Go; reuse existing fakegithub + controller-runtime client |
| D-M5-4 | Sandbox runtime to validate — gVisor, Kata, or both | §4 | gVisor (lower install cost on most cloud K8s) |

---

## 7. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| 1,000-session run exposes goroutine leak undetected by current goleak tests | Medium | High | Run harness with `-race` and periodic `pprof` snapshots; capture full goroutine dump on assertion failure |
| polaris flags rules we've explicitly chosen against the design (e.g. no liveness probe on workers) | High | Low | **Done (Q14).** Per-check exceptions live in [`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml), each with a justifying comment; `--merge-config` keeps every default danger check active |
| Staging cluster doesn't support gVisor (no nested-virt, restrictive cloud) | Medium | Low | Mark gVisor validation as opt-in; document the runtime requirement in the runbook |
| Helm chart maintenance overhead (templates drift from the kustomize bases) | Medium | Medium | Bases stay the source of truth (§1.1); scaffold the chart from them via the kubebuilder `helm/v1-alpha` plugin and add a CI drift check that re-renders and diffs |
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
