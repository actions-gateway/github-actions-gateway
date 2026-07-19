# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview. Pick the next task from the top of the [Queue](#queue).

## Conventions

**Status:** 🔲 ready · 🚫 blocked  
**Size:** S = one session · M = 2–3 sessions · L = multi-session, needs a phased plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug` `flake` `1.0-gate` (blocks the [Release 1.0](plan/release-1.0.md) tag)  
**Next ID:** Q350

Maintained per [`docs/development/maintaining-backlog.md`](development/maintaining-backlog.md): done rows are deleted (git is the archive), the open PR is the in-flight signal, new items enter at the priority they deserve, parked items live in [Deferred](#deferred), and every edit is an isolated `docs(status):` commit gated by `scripts/lint-backlog.sh`.

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
| [v1 sunset → v2-only](plan/v1-classic-sunset-review.md) | `infra` | ⚠️ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q335"></a>Q335 | [On-demand e2e AGC no longer fits the 1-node system pool](plan/archive/kata-on-gke.md#what-the-live-session-found-2026-07-16) | `infra` | 🔲 | S | DaemonSet/kube-dns growth leaves <500m free on the single e2-standard-2, so `e2e-start.sh` times out Pending. Scale the system pool for the e2e window (start/stop scripts) or document 2 nodes in the runbook. |
| <a id="Q324"></a>Q324 | v2 proxy metrics scrape stack (metrics-mTLS + ServiceMonitors) | `infra` | 🔲 | M | The M2→M3a metrics deferral never landed: v2 EgressProxy has no metrics port/TLS/scrape rule and the v2 gateway provisions no ServiceMonitors, so v2 proxy metrics are unscrapable. [Analysis](plan/v2-api-gap-analysis.md#observability). |
| <a id="Q345"></a>Q345 | [Enforce v2alpha1/v2beta1 conditions.go sync](development/code-generation.md#crd-marker-and-api-file-gotchas) | `tests` `infra` | 🔲 | S | The two conditions.go files must stay byte-identical except the package line; today the sync is manual discipline. Add a make-check lint (or unit test) diffing them so a one-sided condition/reason addition fails fast. |
| <a id="Q342"></a>Q342 | Script recurring dogfood e2e ops under scripts/dogfood/ | `infra` | 🔲 | S | Pool config, kata-deploy install, AGC bounce, and debug-pod loops get re-typed ad hoc with per-command PROD_GUARD_OVERRIDEs. Fold them into the lifecycle scripts (hook-exempt, reviewed once). |
| <a id="Q273"></a>Q273 | [Complete v1 removal (full v2-only)](plan/q273-v2-front-door.md) | `docs` `infra` | 🚫 | M | v1-sunset milestone. Front door, deprecate-v1 banners, and `gag-migrate` are done; the residual v1 removal is blocked on the Classic/v1alpha1 deprecation window (from v1.1.0, §6.2) elapsing. Completing it unblocks [Q264](#Q264). |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. When an item's trigger fires, move its row back into the Queue at the position it then deserves.

Each trigger is tagged by source: **Demand:** an outside operator/user ask · **Event:** an observable outside-our-control condition · **Decision:** our own call (we're the blocker; grep `**Decision:**` for what we could move on unilaterally).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q298"></a>Q298 | [Infra PriorityClass allowlist ConfigMap watch (Q188 parity)](operations/security-operations.md#infra-pods-the-separate-allowed-infra-priority-classes-allowlist) | `infra` `security` | S | **Demand:** an operator wants to grow `--allowed-infra-priority-classes` without a GMC restart. Q284 shipped it flag-only; add the same additive, fail-safe watched-ConfigMap augmentation the worker allowlist has (Q188). |
| <a id="Q238"></a>Q238 | [Versioned docs tree (per-release docs)](plan/docs-six-layer-audit.md) | `docs` | M | **Event:** a single `main` page can't be correct for all supported users at once — a release's install/config steps would break a prior, still-supported release. NOT a new *API* version. Then adopt a versioned docs tree (mike/Docusaurus). |
| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | **Demand:** a concrete operator ask for cross-namespace proxy sharing (same-namespace already works). Adds allowedNamespaces consent, CA distribution, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | M | **Demand:** an operator wants KEDA / VPA / a custom HPA for the proxy pool. Add managedAutoscaling (default true): false ⇒ GMC creates only the Deployment; the operator targets it. Additive. Distinct from the connection-metric work (Q19). |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | **Demand:** an operator with managed PKI/Vault wants to supply the proxy cert instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | **Event:** a single per-tenant AGC becomes a measured bottleneck or a SPOF concern (near the ~1000-session ceiling). Single-replica with an in-memory session registry by design; real HA needs distributed session state. |
| <a id="Q15"></a>Q15 | [gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` `security` | S | **Demand:** Operator demand for lightweight (non-VM) syscall-filtering isolation on compute-only CI jobs that don't need DinD. Kata Containers (Q224) covers DinD use cases, which are the primary motivation for runtime sandboxing on GAG. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | **Decision:** the broker swaps RSA-OAEP session-key delivery for X25519 ECDH (Appendix G §G.6 / Q19), making Ed25519 the *secure* default. Until then it's a less-secure opt-in (loses the AES session-key layer); RSA-3072 stays the default. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | **Decision:** CI latency becomes the bottleneck (our self-set threshold). |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | **Decision:** A real Prometheus/Alertmanager setup exists to document against (infra we'd stand up). |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | **Decision:** A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | **Event:** upstream `actions-runner` base scans clean. The worker leg of [security-scan.yml](../.github/workflows/security-scan.yml) is report-only (~36 upstream HIGH/CRITICAL CVEs); when a bump clears them, set its `exit-code` to `1`. |
| <a id="Q263"></a>Q263 | [v1 AGC: default resource requests + tunable agcResources (v2 parity)](operations/tenant-onboarding.md#copy-pasteable-template) | `infra` | S | **Demand:** the v1 AGC pod stamps no resource requests, so a `requests.*` ResourceQuota needs a LimitRange to admit it (v2 already defaults + exposes `spec.agcResources`). Backport both to v1. Caveat: defaulting changes quota accounting on upgrade. |
| <a id="Q198"></a>Q198 | [Quantified benchmark / case study](index.md) | `docs` | M | **Decision:** a paid scale run is funded (or Q181 real-cluster data exists) so real-GitHub-at-scale numbers can back a published case study. Needs ~$10–30 ephemeral cluster + real GitHub. Split from Q193 (free demo stays active). |
| <a id="Q203"></a>Q203 | [Enable Plausible analytics on the docs site](development/website.md) | `docs` | S | **Decision:** a maintainer decides to collect site traffic and provisions a Plausible site. Client wiring shipped (Q195) — set `extra.analytics.plausible_domain` in `mkdocs.yml` and redeploy; analytics sends nothing until then. |
| <a id="Q214"></a>Q214 | [SPIFFE/SPIRE workload-identity signer](plan/v2beta1.md#workload-identity-a-different-config-vault-first) | `security` `infra` | M | **Demand:** An operator wants keyless / SPIRE-based App-JWT signing. Slots behind the existing Q197 `githubapp.Signer` interface as another `signer.provider`, exactly like the deferred cloud KMS providers — additive, post-beta. |
| <a id="Q215"></a>Q215 | [Worker cache backend (actions/cache + Docker layer cache)](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `infra` | L | **Demand:** a concrete ARC-parity ask for build/dependency caching. Workers are storage-less today. Add an optional PVC/object-store cache for `actions/cache` + Docker layers. Needs a plan doc + security review of cross-job cache isolation. |
| <a id="Q216"></a>Q216 | [First-class GPU runner support (GPU Operator/NFD)](design/appendix-e-capacity-planning.md) | `infra` | M | **Demand:** a concrete GPU runner workload/ask. priorityTiers already nominally carry GPU labels; first-class support adds nodeSelector/tolerations/RuntimeClass conventions + GPU Operator / NFD awareness (and Volcano gang-scheduling for multi-GPU). |
| <a id="Q217"></a>Q217 | [OLM / OperatorHub bundle](operations/install.md) | `infra` `docs` | M | **Demand:** OpenShift/OperatorHub adoption demand. Helm-only is the deliberate install stance; an OLM bundle/catalog entry waits for a concrete OperatorHub ask. Additive packaging, no core code change. |
| <a id="Q268"></a>Q268 | [Warm worker pool (`minIdleWorkers`)](design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers) | `infra` | M | **Demand:** a CPU-CI team hits pod-schedule latency after pre-pull (Q211) + cache volumes (Q215) are exhausted. Opt-in per-RG idle-pod pool. Does NOT address Q224 starvation — see the [lever spike](plan/q224-fanout-dispatch-lever-spike.md). |
| <a id="Q272"></a>Q272 | [Scale-set upstream maturity watch](plan/v1-classic-sunset-review.md) | `infra` | S | **Event:** `actions/scaleset` reaches GA/v1.0 or the auto-assign contract (`actions/scaleset#107`) is documented. Not a graduation blocker (sunset review §6.1); it lifts the Public-Preview caveat and triggers the U6 vendor-vs-own client revisit. |
| <a id="Q264"></a>Q264 | [Remove deprecated classic acquisition machinery](plan/q264-scale-set-protocol.md) | `infra` | L | **Event:** v1.2.0 ships (deprecation window from v1.1.0 elapses) and [Q273](#Q273) completes (v1alpha1 migrated). Then one isolated PR removes the classic machinery + transitional `acquisitionProtocol`/`maxListeners`. |
| <a id="Q275"></a>Q275 | [Reconcile AGC capacity/density docs with the ScaleSet default](design/appendix-a-capacity-slos.md) | `docs` | S | **Decision:** classic removal proceeds on the deprecation-window schedule ([Q264](#Q264)) — reconcile alongside it. appendix-a's ≤1,000-session ceiling and README Tier 2's "thousands per AGC" are classic framing; keep the density evidence. |
| <a id="Q274"></a>Q274 | [Live-GitHub e2e: rerun-failed-jobs on eviction](plan/archive/milestone-3-tests.md) | `tests` | S | **Event:** a live-GitHub Tier-C e2e lane/credentials exist. The eviction→rerun-failed-jobs retry logic is already envtest-covered (`failure_recovery_test.go`); this adds the live happy-path companion. |
| <a id="Q310"></a>Q310 | Operator diagnostic aggregator (`gag status` / kubectl plugin) | `infra` | L | **Demand:** operators ask for gateway diagnostics beyond raw kubectl + the runbook. Add a `gag status <gateway>` / kubectl plugin aggregating session, pool, and runner state per gateway. |
| <a id="Q319"></a>Q319 | Export v2 RunnerSet worker-capacity conditions as gauges | `infra` | S | **Demand:** an operator wants to Prometheus-alert on v2 RunnerSet capacity. The Q303 conditions exist but no gauges do — only v1 RunnerGroup exports them. Add per-`RunnerSet` gauge collectors. |
| <a id="Q344"></a>Q344 | First-class `scaleset` list/prune for orphan scale sets | `infra` | S | **Event:** orphan scale sets recur or an operator wants a prune path. Q334 fixed runner-*record* orphans, not stale *scale sets*; a throwaway deleted `gag-scaleset3`. Add a `scaleset` prune command. |
| <a id="Q348"></a>Q348 | [Re-trim CLAUDE.md as guard-plugin fixes land](plan/claude-md-hook-rule-retirement.md) | `docs` | S | **Event:** any upstream fix in the retirement map ships in an installed plugin release, or Q347 lands. Retire the mapped rule(s) piecemeal; verify the previously-prompting command no longer prompts before deleting. |

### Flake watch

Flakes whose mitigation has shipped and that have **not recurred since**, plus rare first sightings not yet worth fixing. The trigger to revive is the flake recurring on `main` after its fix. On recurrence, [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first) pulls the row back to the **top of the Queue** — now escalated, since the first mitigation didn't hold. Kept here (not closed) so a second occurrence is recognised as a recurrence rather than a fresh find.

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q300"></a>Q300 | [Systemic kindnet e2e leg flakiness (cross-spec)](plan/q300-gmc-kindnet-e2e-flake.md) | `tests` `flake` `infra` | M | **Event:** recurs on `main` after PR #612 (removed kind's 100m kindnetd CPU limit that starved the in-band kube-network-policies enforcer; 15%-throttled→0 on the fix run). → top of Queue, escalated. |
| <a id="Q299"></a>Q299 | [manager-metrics curl pod flake (kindnet)](../cmd/gmc/test/e2e/e2e_test.go) | `tests` `flake` | S | **Event:** recurs on `main` after PR #608 (bound curl connect-timeout + gate on metrics endpoints; unbounded curls hung ~133s, so the retry loop got ~2 tries before the 5min budget). → top of Queue, escalated. |
| <a id="Q291"></a>Q291 | [e2e-calico egress-to-GitHub reachability flake](plan/q291-e2e-calico-egress-github-flake.md) | `tests` `flake` `infra` | S | **Event:** recurs on `main` after PR #593 (egress retry budget widened to 150s/4m; Felix ipBlock-programming window outlasted the curl budget under CI load). Recurred 07-04 + 07-11 pre-fix. → top of Queue, escalated. |
| <a id="Q292"></a>Q292 | [e2e hosted-runner disk exhaustion during bring-up](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | **Event:** recurs after PR #597 (drop ~15–20 GB unused toolchains up front; a main calico run hit ENOSPC mid kind-load, 59 MB free — distinct from Q291). → top of Queue, escalated. |
| <a id="Q256"></a>Q256 | [e2e-calico infra bring-up (registry + Calico node)](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | **Event:** recurs on `main` after PR #590 (bake `IP_AUTODETECTION_METHOD` into the manifest so calico-node starts once; racy `first-found` autodetection broke CNI bring-up cluster-wide). Clean on 3/3 post-fix runs. → top of Queue, escalated. |
| <a id="Q285"></a>Q285 | [TestListener_AssignedCountReconciliation](../cmd/agc/internal/scalesetlistener/listener_test.go) | `tests` `flake` | M | **Event:** recurs after the PR #580 fix (complete only provisioner-recorded jobs; `Status.AssignedJobs` leads provisioning). No product bug; poll rate ruled out (Q287). → top of Queue, escalated. |
| <a id="Q222"></a>Q222 | [AGC SIGTERM_DeletesAllSessions](../cmd/agc/internal/controller/integration/sigterm_test.go) | `tests` `flake` | S | **Event:** recurs after the PR #415 mitigation (DELETE-on-SIGTERM ceiling 30→60s + failure dump). The DELETE path itself is robust. → top of Queue, escalated. |
| <a id="Q221"></a>Q221 | [metrics-NP AllowsLabeledNamespace (calico)](../cmd/gmc/test/e2e/manager_np_test.go) | `tests` `flake` | S | **Event:** recurs after the PR #411 mitigation (positive control folded into the Q159 retry-gate pod; the second probe re-racing NP programming is dropped). → top of Queue, escalated. |
| <a id="Q179"></a>Q179 | [two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | S | **Event:** recurs after the PR #369 mitigation (isolation probe budget 60→150 iters, waits widened to 6m). → top of Queue, escalated. |
