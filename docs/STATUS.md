# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · ▶ started · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = needs a plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug` `1.0-gate` (blocks the [Release 1.0](plan/release-1.0.md) tag)

**Maintaining this file:** see [`docs/development/maintaining-backlog.md`](development/maintaining-backlog.md) for the full rules (churn reduction, format conventions, anti-patterns). Short version:
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** create or update a plan doc in `docs/plan/`; delete the row here when done. (Skip the `▶ Started` marker unless you have a specific reason — the open PR is the in-flight signal.)
- **New item identified:** decide its priority *first*, then insert it at that position (not the bottom by default) with the next unused ID. See [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry). Batch audit-discovery items in one commit.
- **Parked item (explicit trigger, no near-term intent):** put it in [Deferred](#deferred), not the Queue; move it back into the Queue at the right priority when its trigger fires. See [deferred items live below the Queue](development/maintaining-backlog.md#deferred-items-live-below-the-queue-not-in-it).
- **⚠️ item fully done:** move it to the Progress table as ✅.
- **`Last touched:` is one line, date only.** Do not append session narrative.
- **Queue `Notes` ≤ 250 characters** (hard, lint-enforced). A markdown link counts its full `[text](url)` source length — count before committing rather than waiting for the hook. Overflow → move detail to the linked plan doc.

Last touched: 2026-06-25

---

## Progress

Plan-level view. ✅ = no open Queue row remains (intentionally-deferred residuals live in [Deferred](#deferred) and don't count against completion). ⚠️ = ≥1 open Queue row remains. See [maintaining-backlog.md](development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count).

| Item | Labels | Status |
|---|---|---|
| [M1: Wire-protocol probe](plan/milestone-1.md) | `milestone` | ✅ |
| [M1: Unit-test coverage](plan/milestone-1-tests.md) | `milestone` `tests` | ✅ |
| [M2: AGC controller](plan/milestone-2.md) | `milestone` | ✅ |
| [M3: Worker pod](plan/milestone-3.md) | `milestone` | ✅ |
| [M4: GMC + proxy](plan/milestone-4.md) | `milestone` | ✅ |
| [M5: Hardening](plan/milestone-5.md) | `milestone` `security` | ⚠️ |
| [Release 1.0](plan/release-1.0.md) | `milestone` | ✅ |
| [Security hardening](plan/security.md) | `security` | ✅ |
| [Security audit 2 (2026-06)](plan/security-audit-2026-06.md) | `security` | ✅ |
| [Worker egress proxy](plan/worker-egress-proxy.md) | `security` `infra` | ✅ |
| [Docs](plan/docs.md) | `docs` | ✅ |
| [Six-layer docs audit](plan/docs-six-layer-audit.md) | `docs` | ✅ |
| [Make UX](plan/make.md) | `infra` | ✅ |
| [Docker image speed](plan/docker-image-speed.md) | `speed` | ✅ |
| [e2e test speed](plan/e2e-tests-speed.md) | `speed` `tests` | ✅ |
| [v2 API decomposition](plan/v2-api.md) | `infra` | ✅ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q222"></a>Q222 | [Deflake AGC SIGTERM_DeletesAllSessions](../cmd/agc/internal/controller/integration/sigterm_test.go) | `tests` `flake` | ▶ | S | Top per [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first). PR #415 gate: DELETE-on-SIGTERM lagged past the 30s ceiling on a starved runner. Fix: ceiling 30→60s + failure dump. DELETE path robust. Escalate if recurs. |
| <a id="Q221"></a>Q221 | [Deflake metrics-NP AllowsLabeledNamespace (calico)](../cmd/gmc/test/e2e/manager_np_test.go) | `tests` `flake` | ▶ | S | Top per [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first). PR #411: HTTP_CODE=000 on post-gate scrape. Fix: fold positive control into Q159 retry-gate pod, drop 2nd probe re-racing per-pod NP programming. Escalate if recurs. |
| <a id="Q179"></a>Q179 | [Deflake two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | ▶ | S | Top per [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first). PR #369 kindnet flake (calico passed): isolation probe budget 60→150 iters + wait 5m→6m; job_lifecycle worker-pod wait 4m→6m. Escalate if recurs. |
| <a id="Q176"></a>Q176 | [Deflake E2E_GMC_HPADrivesScaleUp (calico)](../cmd/gmc/test/e2e/hpa_pdb_test.go) | `tests` `flake` | ▶ | S | Top of queue per [flakes-first rule](development/maintaining-backlog.md#flake-fixes-go-first). Timed out at 120s on calico, passed on rerun. Mitigated: minReplicas-floor wait 2m->5m + failure dump. Escalate if recurs. |
| <a id="Q15"></a>Q15 | [gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` `security` | 🔲 | S | v2beta1 blocker: validate worker `RuntimeClass=gvisor` isolation before signing the production-relyable contract. Now free — minikube + gvisor addon (systrap, no nested virt), local + CI. Parallel with Q191/Q196. |
| <a id="Q74"></a>Q74 | [v2alpha1→v2beta1 graduation: conversion webhook](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | Beta cut, after Q191/Q196/Q197/Q15: `Hub`/`Convertible` stubs + v2beta1 served/storage version + storage migration. Distinct from the M5 fan-out tool. See [graduation](plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2). |
| <a id="Q219"></a>Q219 | [M5 live `helm install` → working-tenant validation](plan/milestone-5.md) | `milestone` `infra` `tests` | 🔲 | M | M5 track A: chart is verified offline only (helm template/kubeconform/polaris). Run a live `helm install` on kind with real App creds → working tenant (job→pod→GitHub), the last M5 verification gap. Pairs with Q15 (same cluster run). |
| <a id="Q220"></a>Q220 | [Validate service-mesh coexistence guidance on a live cluster](operations/service-mesh-coexistence.md) | `tests` `docs` | 🔲 | M | Q206 guide's in-mesh recipes (native sidecars, egress exclusions) reasoned from code+docs, untested. Stand up Istio (sidecar/native/ambient)+Linkerd on kind; run a job through a meshed GAG ns; confirm pods terminate + egress IP preserved. |
| <a id="Q193"></a>Q193 | [End-to-end demo / screencast](index.md) | `docs` | 🔲 | S | No demo or screencast — biggest top-of-funnel friction. Record a free end-to-end kind deploy showing job→pod→GitHub. The quantified benchmark/case-study split to Q198 (it needs a paid scale run). |
| <a id="Q211"></a>Q211 | [P2P image distribution (Spegel/Dragonfly) for pull storms](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `docs` `infra` | 🔲 | S | Ephemeral per-job worker pods cause image-pull storms at scale. Document Spegel/Dragonfly P2P registry mirror as a recommended companion; note `imagePullPolicy`/digest-pin interplay. Scale-readiness. |
| <a id="Q212"></a>Q212 | [Velero backup/restore guidance](operations/install.md) | `docs` | 🔲 | S | Document what's safe to back up/restore for GAG CRs + tenant namespaces, with CA/Secret-rotation caveats (restoring a stale proxy/metrics CA Secret breaks TLS). DR story for operators. |
| <a id="Q213"></a>Q213 | [OpenCost/Kubecost per-tenant cost attribution](design/appendix-f-cost-model.md) | `docs` `infra` | 🔲 | S | Tenant=namespace fits per-tenant cost attribution natively. Document the label conventions OpenCost/Kubecost need to split cost per tenant. Pairs with the Q192 cost story and the Q205 label audit. |
---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | A concrete operator ask for cross-namespace proxy sharing (same-namespace sharing already works without it). Adds inline allowedNamespaces consent, ConfigMap CA distribution to granted namespaces, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | An operator wants KEDA / VPA / a custom HPA for the proxy pool instead of GMC's managed CPU HPA. Add managedAutoscaling (default true, mirrors managedNetworkPolicy): false ⇒ GMC creates only the Deployment (stable name + scale subresource), operator targets it. Additive. Distinct from the connection-metric work (Q19). |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | An operator with managed PKI/Vault wants to supply the proxy cert (different algorithm/lifetime/HSM) instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy: set ⇒ use that Secret. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | A single per-tenant AGC becomes a measured bottleneck or a SPOF concern beyond GitHub's job-level redelivery (near the ~1000-session ceiling). The AGC is single-replica with an in-memory session registry by design; real HA needs distributed session state. v2 multi-gateway eases sharding but not in-process HA. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | Broker swaps RSA-OAEP session-key delivery for X25519 ECDH (Appendix G §G.6 / Q19), making Ed25519 the *secure* default. Until then Ed25519 is a less-secure performance opt-in (loses the AES session-key encryption layer); RSA-3072 stays the default and the probe gates docs nobody should reach for. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
| <a id="Q198"></a>Q198 | [Quantified benchmark / case study](index.md) | `docs` | M | A paid scale run is funded (or Q181 real-cluster data exists) so real-GitHub-at-scale numbers can back a published case study. Can't be validated for free — needs ~$10–30 ephemeral cluster + real GitHub. Split from Q193 (free demo stays active). |
| <a id="Q203"></a>Q203 | [Enable Plausible analytics on the docs site](development/website.md) | `docs` | S | A maintainer decides to collect site traffic and provisions a Plausible site (hosted or self-hosted). Client wiring already shipped (Q195) — set `extra.analytics.plausible_domain` (+ `plausible_src` if self-hosted) in `mkdocs.yml` and redeploy; analytics is off and sends nothing until then. |
| <a id="Q214"></a>Q214 | [SPIFFE/SPIRE workload-identity signer](plan/v2beta1.md#workload-identity-a-different-config-vault-first) | `security` `infra` | M | An operator wants keyless / SPIRE-based App-JWT signing. Slots behind the existing Q197 `githubapp.Signer` interface as another `signer.provider`, exactly like the deferred cloud KMS providers — additive, post-beta. |
| <a id="Q215"></a>Q215 | [Worker cache backend (actions/cache + Docker layer cache)](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `infra` | L | A concrete ARC-parity ask for build/dependency caching. Workers are storage-less today (no PVC/CSI). Add an optional PVC/object-store (S3/MinIO) cache for `actions/cache` + Docker layer cache. Needs a plan doc + security review of cross-job cache isolation. |
| <a id="Q216"></a>Q216 | [First-class GPU runner support (GPU Operator/NFD)](design/appendix-e-capacity-planning.md) | `infra` | M | A concrete GPU runner workload/ask. priorityTiers already nominally carry GPU labels; first-class support adds nodeSelector/tolerations/RuntimeClass conventions + NVIDIA GPU Operator / Node Feature Discovery awareness (and Volcano gang-scheduling for multi-GPU jobs). |
| <a id="Q217"></a>Q217 | [OLM / OperatorHub bundle](operations/install.md) | `infra` `docs` | M | OpenShift/OperatorHub adoption demand. Helm-only is the deliberate install stance; an OLM bundle/catalog entry waits for a concrete OperatorHub ask. Additive packaging, no core code change. |
