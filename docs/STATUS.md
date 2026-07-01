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

Last touched: 2026-06-29
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
| [M5: Hardening](plan/milestone-5.md) | `milestone` `security` | ✅ |
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
| [Per-module coverage ≥75%](plan/coverage-to-75-per-module.md) | `tests` | ✅ |
| [GKE dogfood](plan/gke-dogfood.md) | `infra` `docs` | ⚠️ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q224"></a>Q224 | [GKE dogfood: route production CI (green CI blocked)](plan/gke-dogfood.md) | `milestone` `infra` | 🔲 | M | rc.4 turn-on validated; toolchain unblocked (Q239). Egress blocker for vendor-check/tidy-check resolved: Athens in-cluster cache deployed (Q244). Run `scripts/dogfood/start.sh` + validate green CI; blockers [Q246](#Q246)/[Q247](#Q247). On-demand. |
| <a id="Q246"></a>Q246 | [Re-diagnose dogfood release-asset download timeout (egress already allows it)](plan/gke-dogfood.md) | `infra` | 🔲 | S | Release assets redirect to `objects.githubusercontent.com` (185.199.108.0/22), already allowed by the worker egress NP via /meta `web`. "Add to allowlist" premise wrong; live-verify cold-cache (Q61) or [Q247](#Q247) CPU starve. Blocks [Q224](#Q224). |
| <a id="Q247"></a>Q247 | [AGC runner session recovery after job recycling](plan/gke-dogfood.md) | `infra` | ▶ | M | 3 facets, all fixed + live-validated: RenewJob wrong ID (#481), unbounded renewal wedge (#485), broker-token-not-job-scoped 401 (#486). Full DinD e2e green on GAG (both lanes, run 28496664762; no 10-min orphan). Follow-up: [Q254](#Q254). |
| <a id="Q248"></a>Q248 | [Right-size GAG dogfood CI runner pods + node pool](plan/dogfood-runner-rightsizing.md) | `infra` | 🔲 | M | Worker pod requests/limits are an unmeasured guess. Measure peak CPU/mem per CI job class on GAG; right-size pods + node pool; decide pod tiers (general+e2e). For dogfooding cost/correctness, not speed (no queue; e2e long pole). |
| <a id="Q242"></a>Q242 | [Implement G.1 proxy destination allowlist](plan/q242-g1-proxy-destination-allowlist.md) | `security` `infra` | ▶ | L | Impl merged #460–#464. Dogfood vendor-check/tidy-check unblocked via Athens (Q244). Remaining: flip `GAG_RUNNER`, validate green CI ([Q224](#Q224)); FQDN intent/backend split ([Q245](#Q245)). v2beta1 blocker. |
| <a id="Q243"></a>Q243 | [Per-tenant egress-IP reference architecture (cloud)](plan/gke-dogfood.md) | `security` `infra` `docs` | 🔲 | L | Substantiate the per-tenant egress-IP isolation claim: spike + validate Cilium Egress Gateway vs per-tenant NAT on a cloud; doc single-tenant-direct vs production topology + cost. Dogfood stays direct (single-tenant). v2beta1 blocker. |
| <a id="Q245"></a>Q245 | [FQDN egress: split intent from CNI backend + GKE backend](plan/q242-g1-proxy-destination-allowlist.md#provider-fqdn-egress-fragmentation-post-implementation-finding) | `security` `infra` | 🔲 | L | egressPolicyMode FQDN variants encode a per-CNI kind, fragmented across GKE/AKS/EKS/OVN. Decouple tenant intent (CIDR\|FQDN) from a platform --fqdn-policy-backend; add gke backend (networking.gke.io FQDNNetworkPolicy). Fold into v2beta1 (Q74). |
| <a id="Q226"></a>Q226 | [Kata Containers on GKE — secure CI reference architecture](plan/kata-on-gke.md) | `security` `infra` | 🔲 | M | OSS untrusted-PR threat + GAG dogfood requirement rule out privileged DinD. Spike: GKE nested-virt node pool + Kata RuntimeClass: kind in micro-VM, no privileged pod. Reference arch. [plan](plan/kata-on-gke.md) |
| <a id="Q231"></a>Q231 | [Dogfood GAG e2e on the GKE cluster](plan/gke-dogfood.md) | `infra` `docs` | 🔲 | M | Dogfood validated end-to-end. Bring Part F / dogfood/e2e-setup.sh to v2 (still v1); land F2 (GAG_E2E_RUNNER in e2e-reusable.yml, default ubuntu-latest); decide on-demand vs always-on; re-run + route an e2e job Kata→kind→GitHub. |
| <a id="Q249"></a>Q249 | [Warn on reap-blocking worker sidecars](plan/worker-sidecar-reap-warning.md) | `infra` | 🔲 | M | DinD-e2e finding: a regular (non-native) sidecar keeps the worker pod alive after the runner exits, stranding maxWorkers ([Q247](#Q247)). Warn (non-blocking admission) + RunnerSet condition + metric; name-list opt-out; nudge to native sidecars. |
| <a id="Q254"></a>Q254 | [Tear down the worker when a job lock is definitively lost](operations/troubleshooting.md#renewjob-failures-rising) | `infra` | 🔲 | S | Post-Q247, StartRenewLoop logs a sustained RenewJob failure (network/lost lock) as non-fatal and never cancels the worker — orphan pod + sibling dup-acquire still possible. Cancel the job ctx after N failures or a definitive job-not-found. |
| <a id="Q253"></a>Q253 | [appendix-e capacity-planning: resolve v1/v2 API-version straddle](design/appendix-e-capacity-planning.md) | `docs` `bug` | 🔲 | M | Split from Q250 (high goal-1): E.9 frames AGC sizing via v2 `spec.agcResources` but worked-example YAMLs use v1alpha1 `spec.runnerGroups[].podTemplate` + bare `apiVersion: v1`. No API version accepts them; pick one, rewrite consistently. |
| <a id="Q251"></a>Q251 | [Fix stale defaults & counts docs drift (docs audit Batch B)](plan/q237-docs-quality-audit.md#remediation) | `docs` | 🔲 | M | 16 medium goal-1 findings from the Q237 audit: stale make-check/image/test-gate counts, non-existent kubebuilder default markers, categorical egress claim, terminationGracePeriod default, observability :8081→:8443. Re-confirm each vs code. |
| <a id="Q252"></a>Q252 | [Docs usability & tone polish (docs audit Batch C)](plan/q237-docs-quality-audit.md#remediation) | `docs` | 🔲 | S | goal-5 (4) + goal-6 (10) findings from the Q237 audit: acronym-on-first-use, copy-paste hazards, minor promotional tone. Low reader impact; sweep in one pass. |
| <a id="Q74"></a>Q74 | [v2alpha1→v2beta1 graduation: conversion webhook](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | Beta cut, after Q191/Q196/Q197/Q224/Q242/Q243: `Hub`/`Convertible` stubs + v2beta1 served/storage version + storage migration. Distinct from the M5 fan-out tool. See [graduation](plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2). |
| <a id="Q220"></a>Q220 | [Validate service-mesh coexistence guidance on a live cluster](operations/service-mesh-coexistence.md) | `tests` `docs` | 🔲 | M | Q206 guide's in-mesh recipes (native sidecars, egress exclusions) reasoned from code+docs, untested. Stand up Istio (sidecar/native/ambient)+Linkerd on kind; run a job through a meshed GAG ns; confirm pods terminate + egress IP preserved. |
| <a id="Q193"></a>Q193 | [End-to-end demo / screencast](index.md) | `docs` | 🔲 | S | No demo or screencast — biggest top-of-funnel friction. Record a free end-to-end kind deploy showing job→pod→GitHub. The quantified benchmark/case-study split to Q198 (it needs a paid scale run). |
| <a id="Q223"></a>Q223 | [Worker scale-up rate limit (anti-stampede)](design/appendix-g-future-enhancements.md#g11-worker-scale-up-rate-limiting-anti-stampede) | `infra` | 🔲 | M | Opt-in/default-off per-RunnerGroup ramp on worker-pod creation rate; complements the quota ceiling. For onset stampedes on shared egress (NAT/firewall/VPN, multi-site) — not image pulls (P2P/Q211). Distinct from proxy rate-limit (G.2/Q19). |
---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

Each trigger is tagged by source: **Demand:** an outside operator/user ask · **Event:** an observable outside-our-control condition · **Decision:** our own call (we're the blocker; grep `**Decision:**` for what we could move on unilaterally).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q238"></a>Q238 | [Versioned docs tree (per-release docs)](plan/docs-six-layer-audit.md) | `docs` | M | **Event:** a single `main` page can't be correct for all supported users at once — e.g. a release's install/config steps would break the prior, still-supported release (removed field, flipped default). NOT a new *API* version (one GAG serves both; migration guide covers it). Then adopt a versioned docs tree (mike/Docusaurus). Rationale: six-layer audit. |
| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | **Demand:** A concrete operator ask for cross-namespace proxy sharing (same-namespace sharing already works without it). Adds inline allowedNamespaces consent, ConfigMap CA distribution to granted namespaces, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | **Demand:** An operator wants KEDA / VPA / a custom HPA for the proxy pool instead of GMC's managed CPU HPA. Add managedAutoscaling (default true, mirrors managedNetworkPolicy): false ⇒ GMC creates only the Deployment (stable name + scale subresource), operator targets it. Additive. Distinct from the connection-metric work (Q19). |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | **Demand:** An operator with managed PKI/Vault wants to supply the proxy cert (different algorithm/lifetime/HSM) instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy: set ⇒ use that Secret. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | **Event:** A single per-tenant AGC becomes a measured bottleneck or a SPOF concern beyond GitHub's job-level redelivery (near the ~1000-session ceiling). The AGC is single-replica with an in-memory session registry by design; real HA needs distributed session state. v2 multi-gateway eases sharding but not in-process HA. |
| <a id="Q15"></a>Q15 | [gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` `security` | S | **Demand:** Operator demand for lightweight (non-VM) syscall-filtering isolation on compute-only CI jobs that don't need DinD. Kata Containers (Q224) covers DinD use cases, which are the primary motivation for runtime sandboxing on GAG. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | **Decision:** Broker swaps RSA-OAEP session-key delivery for X25519 ECDH (Appendix G §G.6 / Q19), making Ed25519 the *secure* default. Until then Ed25519 is a less-secure performance opt-in (loses the AES session-key encryption layer); RSA-3072 stays the default and the probe gates docs nobody should reach for. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | **Decision:** CI latency becomes the bottleneck (our self-set threshold). |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | **Decision:** A real Prometheus/Alertmanager setup exists to document against (infra we'd stand up). |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | **Decision:** A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | **Event:** Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
| <a id="Q198"></a>Q198 | [Quantified benchmark / case study](index.md) | `docs` | M | **Decision:** A paid scale run is funded (or Q181 real-cluster data exists) so real-GitHub-at-scale numbers can back a published case study. Can't be validated for free — needs ~$10–30 ephemeral cluster + real GitHub. Split from Q193 (free demo stays active). |
| <a id="Q203"></a>Q203 | [Enable Plausible analytics on the docs site](development/website.md) | `docs` | S | **Decision:** A maintainer decides to collect site traffic and provisions a Plausible site (hosted or self-hosted). Client wiring already shipped (Q195) — set `extra.analytics.plausible_domain` (+ `plausible_src` if self-hosted) in `mkdocs.yml` and redeploy; analytics is off and sends nothing until then. |
| <a id="Q214"></a>Q214 | [SPIFFE/SPIRE workload-identity signer](plan/v2beta1.md#workload-identity-a-different-config-vault-first) | `security` `infra` | M | **Demand:** An operator wants keyless / SPIRE-based App-JWT signing. Slots behind the existing Q197 `githubapp.Signer` interface as another `signer.provider`, exactly like the deferred cloud KMS providers — additive, post-beta. |
| <a id="Q215"></a>Q215 | [Worker cache backend (actions/cache + Docker layer cache)](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `infra` | L | **Demand:** A concrete ARC-parity ask for build/dependency caching. Workers are storage-less today (no PVC/CSI). Add an optional PVC/object-store (S3/MinIO) cache for `actions/cache` + Docker layer cache. Needs a plan doc + security review of cross-job cache isolation. |
| <a id="Q216"></a>Q216 | [First-class GPU runner support (GPU Operator/NFD)](design/appendix-e-capacity-planning.md) | `infra` | M | **Demand:** A concrete GPU runner workload/ask. priorityTiers already nominally carry GPU labels; first-class support adds nodeSelector/tolerations/RuntimeClass conventions + NVIDIA GPU Operator / Node Feature Discovery awareness (and Volcano gang-scheduling for multi-GPU jobs). |
| <a id="Q217"></a>Q217 | [OLM / OperatorHub bundle](operations/install.md) | `infra` `docs` | M | **Demand:** OpenShift/OperatorHub adoption demand. Helm-only is the deliberate install stance; an OLM bundle/catalog entry waits for a concrete OperatorHub ask. Additive packaging, no core code change. |
| <a id="Q230"></a>Q230 | [Automated GKE Dataplane V2 DNS-under-egress-NP regression lane](operations/troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache) | `tests` `infra` | M | **Event:** A GKE Dataplane V2 CI lane exists (after Q224 CI migration). Q229's DNS-on-DPv2 fix was verified manually on a live cluster; e2e runs only kindnet/Calico (no node-local-dns). Add an automated DPv2 DNS-resolves-under-egress-NP assertion. |

### Flake watch

Flakes whose mitigation has shipped and that have **not recurred since**, plus rare first sightings not yet worth fixing. They carry no priority position; the trigger to revive is the flake recurring on `main` after its fix. On recurrence, [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first) pulls the row back to the **top of the Queue** — now escalated, since the first mitigation didn't hold. Kept here (not closed) so a second occurrence is recognised as a recurrence rather than a fresh find.

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q222"></a>Q222 | [AGC SIGTERM_DeletesAllSessions](../cmd/agc/internal/controller/integration/sigterm_test.go) | `tests` `flake` | S | Recurs after PR #415 mitigation (DELETE-on-SIGTERM ceiling 30→60s + failure dump). DELETE path itself robust. → top of Queue, escalate. |
| <a id="Q221"></a>Q221 | [metrics-NP AllowsLabeledNamespace (calico)](../cmd/gmc/test/e2e/manager_np_test.go) | `tests` `flake` | S | Recurs after PR #411 mitigation (fold positive control into Q159 retry-gate pod, drop 2nd probe re-racing per-pod NP programming). → top of Queue, escalate. |
| <a id="Q179"></a>Q179 | [two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | S | Recurs after PR #369 mitigation (isolation probe budget 60→150 iters + wait 5m→6m; job_lifecycle worker-pod wait 4m→6m). → top of Queue, escalate. |
| <a id="Q176"></a>Q176 | [E2E_GMC_HPADrivesScaleUp (calico)](../cmd/gmc/test/e2e/hpa_pdb_test.go) | `tests` `flake` | S | Recurs after mitigation (minReplicas-floor wait 2m→5m + failure dump). Timed out at 120s on calico, passed on rerun. → top of Queue, escalate. |
| <a id="Q256"></a>Q256 | [e2e bake: local registry drops connection mid-push](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | First seen 2026-07-01 (#487 e2e-calico): kind local registry (127.0.0.1:5000) reset/refused mid image-push in bake; kindnet passed same commit; green on rerun. No fix yet. If it recurs: bake-push retry or registry restart-policy — not HA. |
