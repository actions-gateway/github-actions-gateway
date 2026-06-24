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

Last touched: 2026-06-23

---

## Progress

Plan-level view. ✅ = all criteria met. ⚠️ = code shipped, specific pieces remain open in the Queue below.

| Item | Labels | Status |
|---|---|---|
| [M1: Wire-protocol probe](plan/milestone-1.md) | `milestone` | ✅ |
| [M1: Unit-test coverage](plan/milestone-1-tests.md) | `milestone` `tests` | ✅ |
| [M2: AGC controller](plan/milestone-2.md) | `milestone` | ✅ |
| [M3: Worker pod](plan/milestone-3.md) | `milestone` | ✅ |
| [M4: GMC + proxy](plan/milestone-4.md) | `milestone` | ✅ |
| [M5: Hardening](plan/milestone-5.md) | `milestone` `security` | ⚠️ |
| [Release 1.0](plan/release-1.0.md) | `milestone` | ✅ |
| [Security hardening](plan/security.md) | `security` | ⚠️ |
| [Security audit 2 (2026-06)](plan/security-audit-2026-06.md) | `security` | ⚠️ |
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
| <a id="Q179"></a>Q179 | [Deflake two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | ▶ | S | Top per [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first). PR #369 kindnet flake (calico passed): isolation probe budget 60→150 iters + wait 5m→6m; job_lifecycle worker-pod wait 4m→6m. Escalate if recurs. |
| <a id="Q176"></a>Q176 | [Deflake E2E_GMC_HPADrivesScaleUp (calico)](../cmd/gmc/test/e2e/hpa_pdb_test.go) | `tests` `flake` | ▶ | S | Top of queue per [flakes-first rule](development/maintaining-backlog.md#flake-fixes-go-first). Timed out at 120s on calico, passed on rerun. Mitigated: minReplicas-floor wait 2m->5m + failure dump. Escalate if recurs. |
| <a id="Q191"></a>Q191 | [Broker-compatibility probe suite + report](design/03-api-contracts.md) | `tests` | 🔲 | M | v2beta1 blocker (do first): confirm full broker compatibility before freezing the beta shape — a gap could force an API change. Expand cmd/probe into a compat suite + publish a report; turns silent-break risk into a visible asset. |
| <a id="Q196"></a>Q196 | [Credentials discriminated-union shape (v2beta1 blocker)](design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching) | `infra` `security` | 🔲 | S | v2beta1 blocker: alpha→beta is the last free breaking change, so the credential shape must be right before the cut. Nest githubAppRef under an explicit discriminated `spec.credentials` parent. With Q197, precedes the Q74 cut. See §H.15. |
| <a id="Q197"></a>Q197 | [Workload-identity credentials (external signer)](design/05-security.md) | `security` `infra` | 🔲 | L | v2beta1 blocker: 2nd union member, built before the cut so both auth methods ship in the first beta shape. Sign the App JWT via an external signer (no in-cluster PEM); MVP = Vault transit + k8s auth (kind-validatable), cloud KMS follows. |
| <a id="Q74"></a>Q74 | [v2alpha1→v2beta1 graduation: conversion webhook](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | The beta cut, after Q191/Q196/Q197: `Hub`/`Convertible` stubs + add v2beta1 served/storage version + storage migration. Distinct from the M5 fan-out tool. See [graduation](plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2). |
| <a id="Q181"></a>Q181 | [Pin AGC-only per-session memory + publish capacity numbers](design/appendix-a-capacity-slos.md) | `tests` `docs` | 🔲 | M | Multiplexing at 1000 sessions IS validated (Q13: avg 998 sustained, 0 leak, ~127 KiB/session incl. stub). Left: isolate AGC-only per-session mem to confirm the 4000× claim, publish it, fix appendix-a's stale "no load test" blurb. |
| <a id="Q182"></a>Q182 | [Auto-install / document the API-server audit policy](operations/security-operations.md) | `security` `docs` | 🔲 | M | Compromised-Secret and PSA-escalation signals are invisible without the audit policy, today a sample operators must hand-install into kube-apiserver. Auto-install where possible + document EKS/GKE/AKS managed-cluster paths. |
| <a id="Q183"></a>Q183 | [Per-cloud apiserver-CIDR egress tightening](design/05-security.md) | `security` `docs` | 🔲 | S | AGC NetworkPolicy allows 443/6443 to any dest by default (security residual). Document how to find the stable apiserver CIDR per cloud (EKS/GKE/AKS/kubeadm/kind) and review whether a tighter default is feasible. |
| <a id="Q184"></a>Q184 | [`make validate-cluster` pre-flight check](operations/install.md) | `infra` | 🔲 | M | No pre-flight check: deploying onto kindnet silently voids tenant isolation (NetworkPolicy inert). Add a make/helm preflight validating CNI enforcement, K8s>=1.30, cert-manager, metrics-server before install. |
| <a id="Q185"></a>Q185 | [Backup/restore + DR runbook](operations/runbook.md) | `docs` | 🔲 | S | ActionsGateway CR is source of truth; its deletion cascades to Secrets/Deployments. No backup/restore guidance today. Document GitOps + etcd/CR backup and a recovery runbook. |
| <a id="Q186"></a>Q186 | [Ship Grafana dashboard JSON + alerts-as-code](operations/observability.md) | `docs` `infra` | 🔲 | S | observability.md describes dashboard panels and alert rules as prose only. Ship a reference Grafana dashboard JSON + PrometheusRule resources so operators don't rebuild from scratch. |
| <a id="Q187"></a>Q187 | [Air-gapped / private-registry install](operations/install.md) | `infra` `docs` | 🔲 | L | No image bundle or mirror instructions; all three images must come from GHCR. Blocks egress-restricted enterprises. Add image-pull-secret support to the chart + an air-gapped install doc. |
| <a id="Q188"></a>Q188 | [Dynamic PriorityClass allowlist via ConfigMap](operations/security-operations.md) | `infra` | 🔲 | M | PriorityClass allowlist is a static GMC flag — any new class needs a flag edit + GMC rollout, no self-service. Allow the allowlist to be sourced from a watched ConfigMap. Additive. |
| <a id="Q189"></a>Q189 | [ResourceQuota sizing helper / calculator](operations/tenant-onboarding.md) | `docs` | 🔲 | S | Quota sizing is manual: proxy.maxReplicas×res + Σ(maxWorkers×res), no formula. Mistakes cause mid-flight quota-pressure. Add a worked calculator/template to onboarding. |
| <a id="Q190"></a>Q190 | [GitHub App setup walkthrough](operations/tenant-onboarding.md) | `docs` | 🔲 | S | Onboarding omits GitHub App creation (appId/installationId/PEM); strict PEM format is a common first-day failure. Add a gh-CLI/screenshot walkthrough for first-time app + Secret creation. |
| <a id="Q192"></a>Q192 | [Quantified cost story / savings calculator](design/appendix-f-cost-model.md) | `docs` | 🔲 | M | Cost model (appendix-f) uses placeholder rates; homepage "lower cost" is unquantified. Replace with real per-job / $ figures and an interactive savings calculator vs ARC. |
| <a id="Q193"></a>Q193 | [End-to-end demo + case study/benchmark](index.md) | `docs` | 🔲 | M | No demo, screencast, or production evidence — biggest top-of-funnel friction. Record an end-to-end deploy and publish a quantified case study/benchmark (depends on Q181 load-test data). |
| <a id="Q194"></a>Q194 | [Homepage segment clarity + roadmap + Discussions](index.md) | `docs` | 🔲 | S | Landing page doesn't say who it's for or show a roadmap, and there's no community channel. Add segment/persona clarity, a public roadmap page, and enable GitHub Discussions linked from README. |
| <a id="Q195"></a>Q195 | [SEO bundle: structured data, robots.txt, analytics](development/website.md) | `docs` | 🔲 | S | Site has no JSON-LD structured data, no robots.txt, and no analytics. Add SoftwareSourceCode/Organization schema, robots.txt with sitemap, and privacy-respecting analytics (e.g. Plausible). |
---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | A concrete operator ask for cross-namespace proxy sharing (same-namespace sharing already works without it). Adds inline allowedNamespaces consent, ConfigMap CA distribution to granted namespaces, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | An operator wants KEDA / VPA / a custom HPA for the proxy pool instead of GMC's managed CPU HPA. Add managedAutoscaling (default true, mirrors managedNetworkPolicy): false ⇒ GMC creates only the Deployment (stable name + scale subresource), operator targets it. Additive. Distinct from the connection-metric work (Q19). |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | An operator with managed PKI/Vault wants to supply the proxy cert (different algorithm/lifetime/HSM) instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy: set ⇒ use that Secret. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | A single per-tenant AGC becomes a measured bottleneck or a SPOF concern beyond GitHub's job-level redelivery (near the ~1000-session ceiling). The AGC is single-replica with an in-memory session registry by design; real HA needs distributed session state. v2 multi-gateway eases sharding but not in-process HA. |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | S | Wanted, but parked until after the v2 API ([plan](plan/v2-api.md)) ships. gVisor cluster is now locally/CI-testable via minikube + gvisor addon (systrap, no nested virt) — see [testing.md](development/testing.md). |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | Broker swaps RSA-OAEP session-key delivery for X25519 ECDH (Appendix G §G.6 / Q19), making Ed25519 the *secure* default. Until then Ed25519 is a less-secure performance opt-in (loses the AES session-key encryption layer); RSA-3072 stays the default and the probe gates docs nobody should reach for. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
