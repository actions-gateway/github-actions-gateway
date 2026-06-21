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

Last touched: 2026-06-20

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

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q149"></a>Q149 | [v2 API M1: `v2alpha1` types + codegen](plan/v2-api.md) | `infra` `security` | 🔲 | M | Foundation: `v2alpha1` group `actions-gateway.com`, 5 kinds + deepcopy/CRDs/RBAC, CEL (immutability, maxLength 52, drop SecretReference.namespace, maxListeners=10, printer cols). No controllers; served beside v1alpha1. |
| <a id="Q175"></a>Q175 | [v2: multi-gateway `securityProfile` composition rule](design/appendix-h-v2-api-decomposition.md#h16-open-questions--sign-off-needed) | `infra` `security` | 🔲 | S | `securityProfile` PSA-labels the namespace; under multi-gateway two gateways in one ns can contend for that single label. Decide the rule (most-restrictive-wins / reject-on-conflict / off-label) before M3b (Q167); may touch M1 CEL. |
| <a id="Q163"></a>Q163 | [v2 API M2: EgressProxy + RunnerTemplate reconcilers](plan/v2-api.md) | `infra` | 🚫 | M | Blocked on M1 (Q149). Data/noun kinds: EgressProxy reconciler (owns Deploy/Svc/HPA/PDB), RunnerTemplate/ClusterRunnerTemplate + reserved-field webhook. Same-namespace only. |
| <a id="Q164"></a>Q164 | [v2 API M3a: ActionsGateway + RunnerSet, single-gateway parity](plan/v2-api.md) | `infra` `security` | 🚫 | L | Blocked on M2 (Q163). Verb kinds, one gateway/ns (v1 parity): templateRef/proxyRef runtime resolution + conditions, proxy required. Parity checklist gates exit. Core build. |
| <a id="Q167"></a>Q167 | [v2 API M3b: multi-gateway per namespace](plan/v2-api.md) | `infra` `security` | 🚫 | M | Blocked on M3a (Q164). Per-gateway naming (52-char), AGC scoping via gatewayRef field selector, per-gateway ownership/GC. VAP unchanged (keys on namespace marker). |
| <a id="Q165"></a>Q165 | [v2 API M5: migration tool + v1/v2 cutover](plan/v2-api.md) | `infra` | 🚫 | M | Blocked on M3b (Q167). Fan-out migration tool (v1→N v2 objects) + tests, dual-read window (group domain + Q147 values + finalizers), deprecation + operator migration guide. |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q74"></a>Q74 | [CRD conversion-webhook scaffolding (audit D7)](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | S | Graduating `v2alpha1` → `v2beta1` (first in-place graduation): needs `Hub`/`Convertible` conversion-webhook stubs. Distinct from the M5 fan-out migration tool, which a conversion webhook cannot replace. See [v2-api graduation](plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2). |
| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | A concrete operator ask for cross-namespace proxy sharing (same-namespace sharing already works without it). Adds inline allowedNamespaces consent, ConfigMap CA distribution to granted namespaces, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q168"></a>Q168 | [v2 direct egress (optional-proxy behavior)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | A proxy-less deployment is actually wanted (proxy-required is v1 parity, so not a GA gate). Adds proxyMode: Direct, default-deny egress NetworkPolicy without a proxy, managed-IP refresh relocation, EgressUnattributed condition. Schema lands in M1; additive on M3a. |
| <a id="Q172"></a>Q172 | [v2 optional templateRef (default ClusterRunnerTemplate)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | Onboarding friction — tenants want to omit templateRef and get a platform default. Relax required→optional (non-breaking): resolve unset via ActionsGateway.defaultTemplateRef → a default-marked ClusterRunnerTemplate (StorageClass pattern; ≤1 default; fail-closed TemplateNotFound if none). Parallels Q168. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | An operator wants KEDA / VPA / a custom HPA for the proxy pool instead of GMC's managed CPU HPA. Add managedAutoscaling (default true, mirrors managedNetworkPolicy): false ⇒ GMC creates only the Deployment (stable name + scale subresource), operator targets it. Additive. Distinct from the connection-metric work (Q19). |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | An operator with managed PKI/Vault wants to supply the proxy cert (different algorithm/lifetime/HSM) instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy: set ⇒ use that Secret. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | A single per-tenant AGC becomes a measured bottleneck or a SPOF concern beyond GitHub's job-level redelivery (near the ~1000-session ceiling). The AGC is single-replica with an in-memory session registry by design; real HA needs distributed session state. v2 multi-gateway eases sharding but not in-process HA. |
| <a id="Q170"></a>Q170 | [Kubernetes Events for job lifecycle](design/02-architecture.md) | `infra` | S | Operator wants event-based incident visibility (`kubectl describe` / event watchers). Job-lifecycle transitions (acquisition failure, eviction-retry exhaustion, quota rejection, session failure) surface via metrics/conditions only today; the reaper already emits pod events. Broaden the AGC EventRecorder. |
| <a id="Q171"></a>Q171 | [Tenant-tunable AGC resources (agcResources)](design/appendix-e-capacity-planning.md) | `infra` | S | Tenants consistently need per-gateway AGC CPU/memory different from the platform default. Additive optional field on the v2 ActionsGateway spec (appendix-e flags it as a future consideration); ship behind a sensible default. |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | S | Wanted, but parked until after the v2 API ([plan](plan/v2-api.md)) ships. Also needs a cluster with gVisor installed. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | Broker swaps RSA-OAEP session-key delivery for X25519 ECDH (Appendix G §G.6 / Q19), making Ed25519 the *secure* default. Until then Ed25519 is a less-secure performance opt-in (loses the AES session-key encryption layer); RSA-3072 stays the default and the probe gates docs nobody should reach for. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
