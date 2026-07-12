# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · ▶ started · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = multi-session, needs a phased plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug` `1.0-gate` (blocks the [Release 1.0](plan/release-1.0.md) tag)

**Maintaining this file:** see [`docs/development/maintaining-backlog.md`](development/maintaining-backlog.md) for the full rules (churn reduction, format conventions, anti-patterns). Short version:
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** create or update a plan doc in `docs/plan/`; delete the row here when done. (Skip the `▶ Started` marker unless you have a specific reason — the open PR is the in-flight signal.)
- **New item identified:** decide its priority *first*, then insert it at that position (not the bottom by default) with the next unused ID. See [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry). Batch audit-discovery items in one commit.
- **Parked item (explicit trigger, no near-term intent):** put it in [Deferred](#deferred), not the Queue; move it back into the Queue at the right priority when its trigger fires. See [deferred items live below the Queue](development/maintaining-backlog.md#deferred-items-live-below-the-queue-not-in-it).
- **⚠️ item fully done:** move it to the Progress table as ✅.
- **`Last touched:` is one line, date only.** Do not append session narrative.
- **Queue `Notes` are present tense.** No merged-PR lists, no dated "ACHIEVED"/"SHIPPED" narration — that history belongs in `git log` and the plan doc. Answer only: *what is this item*, and *what's the next concrete step*. See [Notes carry no status history](development/maintaining-backlog.md#notes-carry-no-status-history).
- **Queue `Notes` ≤ 250 characters** (hard, lint-enforced), and **> 200 characters must link a doc** from the Item or Notes cell (also lint-enforced). A markdown link counts its full `[text](url)` source length — count before committing rather than waiting for the hook.
- **Context that won't fit a row → write the doc and link it, whatever the item's `Sz`.** Size estimates effort, not context: a one-session `S` item can rest on a decision that took an afternoon. Durable rationale (decisions, security governance) → `docs/design/`; in-flight findings → `docs/plan/`. Never compress a row until a decision or a finding is gone. See [when the context doesn't fit](development/maintaining-backlog.md#when-the-context-doesnt-fit-write-the-doc--whatever-the-items-size).

Last touched: 2026-07-11
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
| [GKE dogfood](plan/gke-dogfood.md) | `infra` `docs` | ✅ |
| <a id="Q248"></a>[Dogfood runner right-sizing](plan/dogfood-runner-rightsizing.md) | `infra` | ✅ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q291"></a>Q291 | [e2e-calico egress-to-GitHub reachability flake](plan/q291-e2e-calico-egress-github-flake.md) | `tests` `flake` `infra` | 🔲 | S | Three real-GitHub egress specs red the e2e-calico leg together; recurred 07-04 + 07-11. Felix ipBlock-programming window outlasts the curl retry budget under CI load. Budget widened (150s/4m); keep open until a clean soak. |
| <a id="Q264"></a>Q264 | [Migrate AGC acquisition to the runner-scale-set protocol](plan/q264-scale-set-protocol.md) | `infra` | ▶ | L | ScaleSet is the default acquisition protocol; Classic is deprecated. Remaining: serve the one-minor deprecation window, then remove the classic machinery and v1alpha1. |
| <a id="Q242"></a>Q242 | [Implement G.1 proxy destination allowlist](plan/q242-g1-proxy-destination-allowlist.md) | `security` `infra` | ▶ | L | Admin-set destination allowlist on the per-tenant egress proxy. Remaining: per-tenant egress IP ([Q243](#Q243)). |
| <a id="Q243"></a>Q243 | [Per-tenant egress-IP reference architecture (cloud)](plan/q243-egress-ip-reference-arch.md) | `security` `infra` `docs` | 🔲 | L | Reference arch and mechanism are live-validated: per-range Cloud NAT gives two tenants distinct, stable IPs. The scheduling blocker is cleared (Q282). Remaining: live-validate proxy-pool pinning end-to-end. |
| <a id="Q284"></a>Q284 | [Expand PodScheduling to the full desirable scheduling surface](plan/q284-podscheduling-surface.md) | `infra` `security` | 🔲 | M | Add `topologySpreadConstraints` + `priorityClassName` (an evicted proxy takes a tenant's egress down). Needs a *separate* infra allowlist and a v2 `ActionsGateway` webhook, which doesn't exist yet — see the plan. |
| <a id="Q286"></a>Q286 | [Move GAG e2e CI onto the Kata runner (unprivileged kind)](plan/kata-on-gke.md) | `security` `infra` | 🔲 | M | Reference arch is delivered (unprivileged kind-in-Kata; Workload Identity required). Remaining: a GAG e2e runner image (dockerd+kind+toolchain), a permanent nested-virt pool, then move e2e onto it. |
| <a id="Q273"></a>Q273 | [Make v2 the front door + exemplary v1→v2 migration](plan/q273-v2-front-door.md) | `docs` `infra` | ▶ | M | Front door, deprecate-v1 banners, and the `gag-migrate` slice are in place. Remaining: full v2-only (v1 removal), gated on the Classic deprecation window (§6.2). |
| <a id="Q290"></a>Q290 | `make plan-index-check` misses plan docs absent from the index | `docs` `infra` | 🔲 | S | It only checks README→STATUS. A plan doc on disk but missing from [plan/README.md](plan/README.md) is invisible — `q264-scale-set-protocol.md` was, for its whole life. Add the disk→README direction. |
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
| <a id="Q278"></a>Q278 | [Validate signed release artifacts post-publish (v2 CRD asset)](operations/release.md) | `security` `infra` `tests` | S | **Decision:** A `v*` release is planned/cut. Build a post-publish smoke test that applies the signed `actions-gateway-crds-v2.yaml` (`kubectl apply --server-side`), runs `cosign verify-blob` against the Sigstore bundle, and asserts the five v2 CRDs register — so the **first** release ships validated, not retroactively. Trigger on release *intent* so it enters the Queue **high** (release-gate) before the asset ships. Distinct from dogfood, which uses the from-source render path (Q277) and never exercises the signed-asset operator flow. Surfaced by Q276 (#559). |
| <a id="Q263"></a>Q263 | [v1 AGC: default resource requests + tunable agcResources (v2 parity)](operations/tenant-onboarding.md#copy-pasteable-template) | `infra` | S | **Demand:** The v1 AGC pod stamps no resource requests, so a namespace `requests.*` `ResourceQuota` needs a `LimitRange` to admit it — v2 already defaults (`defaultAGCResources()`) and exposes `spec.agcResources`. Backport both to the v1 `ActionsGateway`. Caveat: defaulting requests changes quota accounting, so a tight existing quota could fail to schedule the AGC on upgrade — gate/document it. Surfaced by Q262. |
| <a id="Q198"></a>Q198 | [Quantified benchmark / case study](index.md) | `docs` | M | **Decision:** A paid scale run is funded (or Q181 real-cluster data exists) so real-GitHub-at-scale numbers can back a published case study. Can't be validated for free — needs ~$10–30 ephemeral cluster + real GitHub. Split from Q193 (free demo stays active). |
| <a id="Q203"></a>Q203 | [Enable Plausible analytics on the docs site](development/website.md) | `docs` | S | **Decision:** A maintainer decides to collect site traffic and provisions a Plausible site (hosted or self-hosted). Client wiring already shipped (Q195) — set `extra.analytics.plausible_domain` (+ `plausible_src` if self-hosted) in `mkdocs.yml` and redeploy; analytics is off and sends nothing until then. |
| <a id="Q214"></a>Q214 | [SPIFFE/SPIRE workload-identity signer](plan/v2beta1.md#workload-identity-a-different-config-vault-first) | `security` `infra` | M | **Demand:** An operator wants keyless / SPIRE-based App-JWT signing. Slots behind the existing Q197 `githubapp.Signer` interface as another `signer.provider`, exactly like the deferred cloud KMS providers — additive, post-beta. |
| <a id="Q215"></a>Q215 | [Worker cache backend (actions/cache + Docker layer cache)](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `infra` | L | **Demand:** A concrete ARC-parity ask for build/dependency caching. Workers are storage-less today (no PVC/CSI). Add an optional PVC/object-store (S3/MinIO) cache for `actions/cache` + Docker layer cache. Needs a plan doc + security review of cross-job cache isolation. |
| <a id="Q216"></a>Q216 | [First-class GPU runner support (GPU Operator/NFD)](design/appendix-e-capacity-planning.md) | `infra` | M | **Demand:** A concrete GPU runner workload/ask. priorityTiers already nominally carry GPU labels; first-class support adds nodeSelector/tolerations/RuntimeClass conventions + NVIDIA GPU Operator / Node Feature Discovery awareness (and Volcano gang-scheduling for multi-GPU jobs). |
| <a id="Q217"></a>Q217 | [OLM / OperatorHub bundle](operations/install.md) | `infra` `docs` | M | **Demand:** OpenShift/OperatorHub adoption demand. Helm-only is the deliberate install stance; an OLM bundle/catalog entry waits for a concrete OperatorHub ask. Additive packaging, no core code change. |
| <a id="Q268"></a>Q268 | [Warm worker pool (`minIdleWorkers`)](design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers) | `infra` | M | **Demand:** A self-hosted CPU-CI team hits pod-schedule latency after exhausting P2P/pre-pull (Q211) + cache volumes (Q215). Opt-in/default-off per-RG pool of idle pods, JIT-injected on acquisition. GPU excluded (idle-accelerator cost). **NB (2026-07-05, [Q224 lever spike](plan/q224-fanout-dispatch-lever-spike.md)):** this warm *worker-pod* pool does **NOT** address the Q224 fan-out distinct-delivery starvation — a warm pod is not a long-poll session and presents no extra idle runner to GitHub. The dispatch lever would be a *distinct*, unbuilt warm idle-**listener** baseline (`minIdleListeners`), and even that is only a probabilistic stopgap, not a fix ([Q264](#Q264) is the structural fix). |
| <a id="Q272"></a>Q272 | [Scale-set upstream maturity watch](plan/v1-classic-sunset-review.md) | `infra` | S | **Event:** `actions/scaleset` reaches GA/v1.0 **or** the auto-assign contract (`actions/scaleset#107`) gets documented/answered. Per the [sunset review](plan/v1-classic-sunset-review.md) §6.1 this is **not** a graduation/removal blocker — v2beta1 gates on GAG's own clean-green + stability soak, not GitHub's GA timeline. It lifts the Public-Preview caveat and triggers the U6 vendor-vs-own client revisit. |
| <a id="Q275"></a>Q275 | [Reconcile AGC capacity/density docs with the ScaleSet default](design/appendix-a-capacity-slos.md) | `docs` | S | **Decision:** The v2beta1 graduation (Q74) shipped and the classic-removal proceeds on the deprecation-window schedule — reconcile alongside it. appendix-a's "≤1,000 concurrent virtual sessions (peak burst)" ceiling and README Tier 2's "multiplexes virtual runner sessions … thousands per AGC pod" are classic many-acquirers framing; under the ScaleSet default ([Q264](#Q264) P5, #553) steady state is 1 listener session/set and concurrency is worker pods (capacity-gated). Reframe both; keep the load-tested ~4,000× goroutine-vs-.NET density evidence (protocol-independent). Nothing wrong today — classic is still served. Surfaced by Q273 (#552). |
| <a id="Q274"></a>Q274 | [Live-GitHub e2e: rerun-failed-jobs on eviction](plan/archive/milestone-3-tests.md) | `tests` | S | **Event:** A live-GitHub Tier-C e2e lane/credentials exist. The eviction→rerun-failed-jobs retry logic is already envtest-covered (`failure_recovery_test.go`); this adds the live happy-path companion. Was finding H2 in the (now-archived) k8s-best-practices audit. |

### Flake watch

Flakes whose mitigation has shipped and that have **not recurred since**, plus rare first sightings not yet worth fixing. They carry no priority position; the trigger to revive is the flake recurring on `main` after its fix. On recurrence, [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first) pulls the row back to the **top of the Queue** — now escalated, since the first mitigation didn't hold. Kept here (not closed) so a second occurrence is recognised as a recurrence rather than a fresh find.

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q256"></a>Q256 | [e2e-calico infra bring-up (registry + Calico node)](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | Recurs on `main` after PR #590: calico-node started twice (manifest apply, then `set env` for `IP_AUTODETECTION_METHOD`) — the first pod came up with racy `first-found` autodetection, BIRD never wrote `/var/lib/calico/nodename`, so CNI sandbox setup failed cluster-wide (`FailedCreatePodSandBox`). #590 bakes `IP_AUTODETECTION_METHOD=kubernetes-internal-ip` into the manifest before apply so calico-node starts once; clean bring-up on 3/3 post-fix runs (the one red run failed only on unrelated egress-to-GitHub specs). → top of Queue, escalate. |
| <a id="Q285"></a>Q285 | [TestListener_AssignedCountReconciliation](../cmd/agc/internal/scalesetlistener/listener_test.go) | `tests` `flake` | M | Recurs after the #580 fix (test completed only the jobs the provisioner had recorded; `Status.AssignedJobs` leads provisioning). No product bug. Poll rate ruled out (Q287): old shape flaked ~1-1.5% at both 5,000 and 1 req/s. → top of Queue, escalate. |
| <a id="Q222"></a>Q222 | [AGC SIGTERM_DeletesAllSessions](../cmd/agc/internal/controller/integration/sigterm_test.go) | `tests` `flake` | S | Recurs after PR #415 mitigation (DELETE-on-SIGTERM ceiling 30→60s + failure dump). DELETE path itself robust. → top of Queue, escalate. |
| <a id="Q221"></a>Q221 | [metrics-NP AllowsLabeledNamespace (calico)](../cmd/gmc/test/e2e/manager_np_test.go) | `tests` `flake` | S | Recurs after PR #411 mitigation (fold positive control into Q159 retry-gate pod, drop 2nd probe re-racing per-pod NP programming). → top of Queue, escalate. |
| <a id="Q179"></a>Q179 | [two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | S | Recurs after PR #369 mitigation (isolation probe budget 60→150 iters + wait 5m→6m; job_lifecycle worker-pod wait 4m→6m). → top of Queue, escalate. |
