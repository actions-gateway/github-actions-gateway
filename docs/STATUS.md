# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview. Pick the next task from the top of the [Queue](#queue).

## Conventions

**Status:** 🔲 ready · 🚫 blocked  
**Size:** S = one session · M = 2–3 sessions · L = multi-session, needs a phased plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug` `flake` `retro` `1.0-gate` (blocks the [Release 1.0](plan/release-1.0.md) tag)  
**Next ID:** Q384

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
| [Worker right-sizing profiles](plan/runner-sizing-profiles.md) | `infra` | ⚠️ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q383"></a>Q383 | [Verify CI checks exist, not just that none failed](development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran) | `docs` `retro` | 🔲 | S | A push produced no workflow runs for ~10 min (fixed by close/reopen). Absence of checks reads like nothing-failing — verify check-runs exist on the head SHA. Also ad-hoc shell is zsh: no word-splitting. |
| <a id="Q380"></a>Q380 | [Recreate dogfood from zero — `setup.sh` unproven](plan/gke-dogfood.md#recreate-is-not-yet-proven-end-to-end-q380) | `infra` | 🔲 | S | Cluster deleted 07-20 (`delete.sh` validated live), so the next dogfood session must bootstrap from nothing. The from-zero `setup.sh` path has never run — incl. the new `workers-od` pool. Validate before you need it; gaps in the plan. |
| <a id="Q359"></a>Q359 | [Worker right-sizing profiles (recommendations first)](plan/runner-sizing-profiles.md) | `infra` | 🔲 | L | Killer-feature gap vs ARC: measure per-RunnerSet worker usage, surface recommended requests/limits in status, later opt-in auto-apply profiles. Phase 1 is usage metrics + an operator recipe; see the plan. |
| <a id="Q173"></a>Q173 | [v2 bring-your-own proxy autoscaler (managedAutoscaling opt-out)](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` | 🔲 | M | Add managedAutoscaling (default true) on EgressProxy: false ⇒ GMC creates only the proxy Deployment; the operator attaches KEDA / VPA / the OSS MPA / a custom HPA. Additive; distinct from connection-metric scaling ([Q19](#Q19)). |
| <a id="Q360"></a>Q360 | [Managed VPA opt-in for the control planes](design/appendix-e-capacity-planning.md) | `infra` | 🔲 | M | Chart `vpa.enabled` emits a VPA for the GMC; a per-gateway opt-in has the GMC stamp one next to each AGC Deployment (appendix-e documents AGC restart safety). Needs the VPA CRDs present (degrade gracefully) + precedence over `agcResources` settled. |
| <a id="Q364"></a>Q364 | [Egress-proxy NetworkPolicy open-codes the shared CIDR rule](plan/structural-debt-audit-2026-07.md) | `security` `infra` | 🔲 | S | `githubCIDREgressRule` (builder.go:404) is called from 3 sites but not `egressproxy_builder.go`, which open-codes the peer loop twice (:470, :487). Three spellings of one 443 allowlist. F4. |
| <a id="Q374"></a>Q374 | [v2 conditions sync gate covers 13% of the identical API lines](plan/structural-debt-audit-2026-07.md) | `infra` `tests` | 🔲 | M | 2,550 lines are byte-identical across v2alpha1/v2beta1; `check-conditions-sync.sh` hardcodes 2 paths and guards 332. Rest drifts silently. `conditions.go`/`sidecar.go` hold no API structs — sharable, not just gatable. F3. |
| <a id="Q362"></a>Q362 | [Probe reimplements the scaleset package it validates](plan/structural-debt-audit-2026-07.md) | `tests` `infra` | 🔲 | M | `cmd/probe` never imports `scaleset`; it shadows 5 exported types and hand-builds requests. A library-vs-wire divergence is invisible exactly where the probe should catch it. Deletes most of 1,048 lines. F2. |
| <a id="Q366"></a>Q366 | [Collapse the 29 GMC CreateOrPatch wrappers](plan/structural-debt-audit-2026-07.md) | `infra` | 🔲 | M | 33 calls / 29 `apply*` funcs differing only by type. Collapsing surfaces a latent issue: only 4 of 11 v1 helpers set an ownerRef, policy undocumented — a force-removed finalizer leaks SAs/RoleBindings/NPs/HPAs. F7. |
| <a id="Q367"></a>Q367 | [Split the two god wiring functions](plan/structural-debt-audit-2026-07.md) | `infra` `tests` | 🔲 | M | `gmc/cmd/main.go` main() is 669 lines under `nolint:gocyclo` (~12 concerns, none test-reachable); `agc/main.go` run() is 431 with 23 scattered `os.Getenv`. Both already have extracted helpers proving the pattern. F8. |
| <a id="Q371"></a>Q371 | [Add nolintlint + a ratcheted funlen gate](plan/structural-debt-audit-2026-07.md) | `infra` | 🔲 | S | Config-only. `nolintlint` (allow-unused:false) flags the inert `nolint:gocyclo` on gmc `main()` — `gocyclo` isn't even enabled. Start `funlen` above the worst survivor, ratchet down. Land after [Q367](#Q367). §Prevention. |
| <a id="Q368"></a>Q368 | [Consolidate the broker-protocol test doubles](plan/structural-debt-audit-2026-07.md) | `tests` | 🔲 | M | 3 stub servers reimplement one wire protocol (~1,200 of 2,240 lines recoverable). `fakegithub` is a published, Trivy-scanned image. Also gate that no `package main` reaches `httptest` — `compat.go` asserts this by convention only. F5. |
| <a id="Q369"></a>Q369 | [Unify the broker/scaleset error taxonomy](plan/structural-debt-audit-2026-07.md) | `infra` | 🔲 | S | `RateLimitError` and `UnauthorizedError` are declared in both packages, and `parseRateLimitError` twice with identical bodies. Callers spanning both protocols type-switch on two same-named types. `githubapp/httpx` is the home. F9. |
| <a id="Q370"></a>Q370 | [Reduce script-layer sprawl](plan/structural-debt-audit-2026-07.md) | `infra` | 🔲 | M | `dogfood/setup.sh` is 688 lines / 15 concerns behind one main(); `lib/common.sh` is sourced by 26 of 69 scripts while the biggest non-adopters re-roll its helpers; the 3 `sync-chart-*.sh` triplicate one skeleton. F10. |
| <a id="Q365"></a>Q365 | [Move shared foundation out of v1-named files](plan/structural-debt-audit-2026-07.md) | `infra` | 🔲 | M | Most cross-version GMC foundation sits in `builder.go`/`actionsgateway_controller.go`; AGC's `listener` pkg likewise owns `Metrics`/`AdmitFunc` used by both tiers. Do it now, while both live: de-risks [Q273](#Q273) from refactor to `git rm`. F6. |
| <a id="Q375"></a>Q375 | [Iteration guidance: one full `make check`, cheap loops between](development/testing.md) | `docs` `speed` | 🔲 | S | Sessions average 3–4 full `make check` runs (21-day transcripts; median 2 min, p90 9 min queued on the heavy-build lock). Guide: iterate with `make lint` + module-scoped tests; full gate once, pre-PR. |
| <a id="Q376"></a>Q376 | [Stagger dispatch-batch gate runs](development/parallel-dispatch.md) | `docs` `speed` `infra` | 🔲 | S | Batch workers all run `make check` at once and queue on the heavy-build lock (observed waits up to 5 h). Stagger the gate across workers, or run it as a background task while docs/PR prep continue. |
| <a id="Q377"></a>Q377 | [Change-scope `make test`/coverage to affected modules](../scripts/go-test.sh) | `speed` `tests` | 🔲 | M | Tests dominate `make check` runtime; reuse go-lint.sh's affected-module scoping. Needs care: cover-check floors are per-module, so a scoped run gates only the modules it ran. |
| <a id="Q381"></a>Q381 | [Document the STATUS-only-conflict rebase fast path](development/maintaining-backlog.md) | `docs` `retro` | 🔲 | S | PR #724 ate 4 rebase cycles; each ~6 min full-gate re-run widened the race window that caused the next conflict. Codify: a STATUS-only conflict re-verifies with lint-backlog + doc-links + conflict-markers-check, then pushes immediately. |
| <a id="Q382"></a>Q382 | [Q-ID allocation under parallel sessions](development/maintaining-backlog.md) | `infra` `retro` | 🔲 | M | 5 ID-allocating merges on 07-20 forced 3 renumberings across one PR's rebases — the global `Next ID` counter is the contention point. Explore provisional session IDs normalized at merge, or reserved blocks. Distinct from [Q376](#Q376). |
| <a id="Q273"></a>Q273 | [Complete v1 removal (full v2-only)](plan/q273-v2-front-door.md) | `docs` `infra` | 🚫 | M | v1-sunset milestone. Front door, deprecate-v1 banners, and `gag-migrate` are done; the residual v1 removal is blocked on the Classic/v1alpha1 deprecation window (from v1.1.0, §6.2) elapsing. Completing it unblocks [Q264](#Q264). |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. When an item's trigger fires, move its row back into the Queue at the position it then deserves.

Each trigger is tagged by source: **Demand:** an outside operator/user ask · **Event:** an observable outside-our-control condition · **Decision:** our own call (we're the blocker; grep `**Decision:**` for what we could move on unilaterally).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q372"></a>Q372 | [Re-run the structural debt audit](plan/structural-debt-audit-2026-07.md) | `infra` | M | **Event:** the next minor release is cut, OR non-test Go LOC grows ≥20% over the 41,011-line baseline (2026-07-20) — whichever first. Growth, not calendar time, tracks drift; linters catch ~2 of 10 findings, so the sweep stays necessary. |
| <a id="Q354"></a>Q354 | [Raise e2e/tested K8s floor 1.35 → 1.36](../.github/workflows/e2e-reusable.yml) | `infra` `tests` | S | **Event:** GKE regular channel reaches 1.36, OR we want a 1.36-only feature (explicit floor-raise), OR kind stops shipping 1.35 node images. Then bump KIND_NODE_IMAGE digest + refresh the "verified on k8s 1.35" operations-doc claims. |
| <a id="Q298"></a>Q298 | [Infra PriorityClass allowlist ConfigMap watch (Q188 parity)](operations/security-operations.md#infra-pods-the-separate-allowed-infra-priority-classes-allowlist) | `infra` `security` | S | **Demand:** an operator wants to grow `--allowed-infra-priority-classes` without a GMC restart. Q284 shipped it flag-only; add the same additive, fail-safe watched-ConfigMap augmentation the worker allowlist has (Q188). |
| <a id="Q238"></a>Q238 | [Versioned docs tree (per-release docs)](plan/docs-six-layer-audit.md) | `docs` | M | **Event:** a single `main` page can't be correct for all supported users at once — a release's install/config steps would break a prior, still-supported release. NOT a new *API* version. Then adopt a versioned docs tree (mike/Docusaurus). |
| <a id="Q166"></a>Q166 | [v2 API M4: cross-namespace EgressProxy sharing](plan/v2-api.md) | `infra` `security` | M | **Demand:** a concrete operator ask for cross-namespace proxy sharing (same-namespace already works). Adds allowedNamespaces consent, CA distribution, dual-side NetworkPolicy, managed-IP refresh relocation. Additive on M3a. |
| <a id="Q174"></a>Q174 | [v2 bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path) | `infra` `security` | M | **Demand:** an operator with managed PKI/Vault wants to supply the proxy cert instead of GMC's self-signed default. Add certificateSecretRef on EgressProxy. Invariant: same-namespace TLS Secret, no cross-tenant reuse. Additive; design goal 6. |
| <a id="Q169"></a>Q169 | [AGC horizontal scaling / multi-replica HA](design/appendix-e-capacity-planning.md) | `infra` | L | **Event:** a single per-tenant AGC becomes a measured bottleneck or a SPOF concern (near the ~1000-session ceiling). Single-replica with an in-memory session registry by design; real HA needs distributed session state. |
| <a id="Q15"></a>Q15 | [gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` `security` | S | **Demand:** an operator wants lightweight (non-VM) syscall-filtering for compute-only, non-DinD CI jobs — likeliest on GKE, where gVisor is first-party. Kata (Q224) already covers the DinD case, the primary sandboxing motivation here. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | S | **Decision:** the broker swaps RSA-OAEP session-key delivery for X25519 ECDH ([Q351](#Q351)), making Ed25519 the *secure* default. Until then it's a less-secure opt-in (loses the AES session-key layer); RSA-3072 stays the default. |
| <a id="Q361"></a>Q361 | [CI latency round 2: lint + coverage module loops](plan/archive/unit-tests-speed.md#2026-07-20-re-baseline-q17-revival) | `speed` `infra` | M | **Decision:** CI latency is again the bottleneck (self-set threshold). Critical path is lint (~230 s per-module golangci-lint loop); coverage ~140 s. Levers in the linked re-baseline. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | **Decision:** A real Prometheus/Alertmanager setup exists to document against (infra we'd stand up). |
| <a id="Q19"></a>Q19 | [Proxy features: rate-limit, audit log, AGC↔proxy TLS, per-RG pool](design/appendix-g-future-enhancements.md) | `security` | L | **Decision:** A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). Allowlist (G.1) shipped as Q242; X25519 (G.6) split to [Q351](#Q351). |
| <a id="Q351"></a>Q351 | [X25519 ECDH session-key exchange](design/appendix-g-future-enhancements.md#g6-x25519-ecdh-session-key-exchange) | `security` | M | **Decision:** we swap RSA-OAEP session-key delivery for X25519 ECDH (Appendix G.6) — makes Ed25519 the *secure* default and unblocks [Q11](#Q11). Until then RSA-3072/RSA-OAEP stays the secure default. |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | **Event:** upstream `actions-runner` base scans clean. The worker leg of [security-scan.yml](../.github/workflows/security-scan.yml) is report-only (~36 upstream HIGH/CRITICAL CVEs); when a bump clears them, set its `exit-code` to `1`. |
| <a id="Q198"></a>Q198 | [Quantified benchmark / case study](index.md) | `docs` | M | **Decision:** a paid scale run is funded (or Q181 real-cluster data exists) so real-GitHub-at-scale numbers can back a published case study. Needs ~$10–30 ephemeral cluster + real GitHub. Split from Q193 (free demo stays active). |
| <a id="Q203"></a>Q203 | [Enable Plausible analytics on the docs site](development/website.md) | `docs` | S | **Decision:** a maintainer decides to collect site traffic and provisions a Plausible site. Client wiring shipped (Q195) — set `extra.analytics.plausible_domain` in `mkdocs.yml` and redeploy; analytics sends nothing until then. |
| <a id="Q214"></a>Q214 | [SPIFFE/SPIRE workload-identity signer](plan/v2beta1.md#workload-identity-a-different-config-vault-first) | `security` `infra` | M | **Demand:** An operator wants keyless / SPIRE-based App-JWT signing. Slots behind the existing Q197 `githubapp.Signer` interface as another `signer.provider`, exactly like the deferred cloud KMS providers — additive, post-beta. |
| <a id="Q215"></a>Q215 | [Worker cache backend (actions/cache + Docker layer cache)](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane) | `infra` | L | **Demand:** a concrete ARC-parity ask for build/dependency caching. Workers are storage-less today. Add an optional PVC/object-store cache for `actions/cache` + Docker layers. Needs a plan doc + security review of cross-job cache isolation. |
| <a id="Q216"></a>Q216 | [First-class GPU runner support (GPU Operator/NFD)](design/appendix-e-capacity-planning.md) | `infra` | M | **Demand:** a concrete GPU runner workload/ask. priorityTiers already nominally carry GPU labels; first-class support adds nodeSelector/tolerations/RuntimeClass conventions + GPU Operator / NFD awareness (and Volcano gang-scheduling for multi-GPU). |
| <a id="Q217"></a>Q217 | [OLM / OperatorHub bundle](operations/install.md) | `infra` `docs` | M | **Demand:** OpenShift/OperatorHub adoption demand. Helm-only is the deliberate install stance; an OLM bundle/catalog entry waits for a concrete OperatorHub ask. Additive packaging, no core code change. |
| <a id="Q268"></a>Q268 | [Warm worker pool (`minIdleWorkers`)](design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers) | `infra` | M | **Demand:** a CPU-CI team hits pod-schedule latency after pre-pull (Q211) + cache volumes (Q215) are exhausted. Opt-in per-RG idle-pod pool. Does NOT address Q224 starvation — see the [lever spike](plan/q224-fanout-dispatch-lever-spike.md). |
| <a id="Q272"></a>Q272 | [Scale-set upstream maturity watch](plan/v1-classic-sunset-review.md) | `infra` | S | **Event:** `actions/scaleset` reaches GA/v1.0 or the auto-assign contract (`actions/scaleset#107`) is documented. Not a graduation blocker (sunset review §6.1); it lifts the Public-Preview caveat and triggers the U6 vendor-vs-own client revisit. |
| <a id="Q264"></a>Q264 | [Remove deprecated classic acquisition machinery](plan/q264-scale-set-protocol.md) | `infra` | L | **Event:** v1.2.0 shipped 2026-07-20, so the v1.1.0 deprecation window has elapsed — now waiting only on [Q273](#Q273) (v1alpha1 migrated). Then one isolated PR removes the classic machinery + transitional `acquisitionProtocol`/`maxListeners`. |
| <a id="Q275"></a>Q275 | [Reconcile AGC capacity/density docs with the ScaleSet default](design/appendix-a-capacity-slos.md) | `docs` | S | **Decision:** classic removal proceeds on the deprecation-window schedule ([Q264](#Q264)) — reconcile alongside it. appendix-a's ≤1,000-session ceiling and README Tier 2's "thousands per AGC" are classic framing; keep the density evidence. |
| <a id="Q274"></a>Q274 | [Live-GitHub e2e: rerun-failed-jobs on eviction](plan/archive/milestone-3-tests.md) | `tests` | S | **Event:** a live-GitHub Tier-C e2e lane/credentials exist. The eviction→rerun-failed-jobs retry logic is already envtest-covered (`failure_recovery_test.go`); this adds the live happy-path companion. |
| <a id="Q310"></a>Q310 | Operator diagnostic aggregator (`gag status` / kubectl plugin) | `infra` | L | **Demand:** operators ask for gateway diagnostics beyond raw kubectl + the runbook. Add a `gag status <gateway>` / kubectl plugin aggregating session, pool, and runner state per gateway. |
| <a id="Q319"></a>Q319 | Export v2 RunnerSet worker-capacity conditions as gauges | `infra` | S | **Demand:** an operator wants to Prometheus-alert on v2 RunnerSet capacity. The Q303 conditions exist but no gauges do — only v1 RunnerGroup exports them. Add per-`RunnerSet` gauge collectors. |
| <a id="Q344"></a>Q344 | First-class `scaleset` list/prune for orphan scale sets | `infra` | S | **Event:** orphan scale sets recur or an operator wants a prune path. Q334 fixed runner-*record* orphans, not stale *scale sets*; a throwaway deleted `gag-scaleset3`. Add a `scaleset` prune command. |

### Flake watch

Flakes whose mitigation has shipped and that have **not recurred since**, plus rare first sightings not yet worth fixing. The trigger to revive is the flake recurring on `main` after its fix. On recurrence, [flakes-first](development/maintaining-backlog.md#flake-fixes-go-first) pulls the row back to the **top of the Queue** — now escalated, since the first mitigation didn't hold. Kept here (not closed) so a second occurrence is recognised as a recurrence rather than a fresh find.

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q378"></a>Q378 | [TestReconcile_ReaperDefaults baseline-recheck race](../cmd/agc/internal/controller/runnergroup_reaper_test.go) | `tests` `flake` | S | **Event:** recurs on `main` after PR #738 (reaper tests pin `BaselineRecheckInterval` to 1h; brokerless test listeners let `ActiveCount()` fall below `MaxListeners`, swapping the asserted requeue for the 15s baseline). → top of Queue, escalated. |
| <a id="Q350"></a>Q350 | [scalesetlistener name-conflict test setup race](../cmd/agc/internal/scalesetlistener/listener_test.go) | `tests` `flake` | S | **Event:** recurs on `main` after PR #700 (staging conflicts after `EnqueueJob` raced the poll loop; tests now hold capacity 0 until the conflict is staged). → top of Queue, escalated. |
| <a id="Q300"></a>Q300 | [Systemic kindnet e2e leg flakiness (cross-spec)](plan/q300-gmc-kindnet-e2e-flake.md) | `tests` `flake` `infra` | M | **Event:** kindnet `e2e / e2e` red on `main` again. 07-18 escalation was misattributed (Q349, pre-fix). Dump now captures nfqueue/memory counters + full-window kindnet logs — attribute per the plan's triage table before any code change. |
| <a id="Q299"></a>Q299 | [manager-metrics curl pod flake (kindnet)](../cmd/gmc/test/e2e/e2e_test.go) | `tests` `flake` | S | **Event:** recurs on `main` after PR #608 (bound curl connect-timeout + gate on metrics endpoints; unbounded curls hung ~133s, so the retry loop got ~2 tries before the 5min budget). → top of Queue, escalated. |
| <a id="Q291"></a>Q291 | [e2e-calico egress-to-GitHub reachability flake](plan/q291-e2e-calico-egress-github-flake.md) | `tests` `flake` `infra` | S | **Event:** recurs on `main` after PR #593 (egress retry budget widened to 150s/4m; Felix ipBlock-programming window outlasted the curl budget under CI load). Recurred 07-04 + 07-11 pre-fix. → top of Queue, escalated. |
| <a id="Q292"></a>Q292 | [e2e hosted-runner disk exhaustion during bring-up](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | **Event:** recurs after PR #597 (drop ~15–20 GB unused toolchains up front; a main calico run hit ENOSPC mid kind-load, 59 MB free — distinct from Q291). → top of Queue, escalated. |
| <a id="Q256"></a>Q256 | [e2e-calico infra bring-up (registry + Calico node)](../.github/workflows/e2e-reusable.yml) | `tests` `flake` `infra` | S | **Event:** recurs on `main` after PR #590 (bake `IP_AUTODETECTION_METHOD` into the manifest so calico-node starts once; racy `first-found` autodetection broke CNI bring-up cluster-wide). Clean on 3/3 post-fix runs. → top of Queue, escalated. |
| <a id="Q285"></a>Q285 | [TestListener_AssignedCountReconciliation](../cmd/agc/internal/scalesetlistener/listener_test.go) | `tests` `flake` | M | **Event:** recurs after the PR #580 fix (complete only provisioner-recorded jobs; `Status.AssignedJobs` leads provisioning). No product bug; poll rate ruled out (Q287). → top of Queue, escalated. |
| <a id="Q221"></a>Q221 | [metrics-NP AllowsLabeledNamespace (calico)](../cmd/gmc/test/e2e/manager_np_test.go) | `tests` `flake` | S | **Event:** recurs after the PR #412 mitigation (positive control folded into the Q159 retry-gate pod; the second probe re-racing NP programming is dropped). → top of Queue, escalated. |
| <a id="Q179"></a>Q179 | [two kindnet v1 e2e timing races](../cmd/gmc/test/e2e/isolation_test.go) | `tests` `flake` | S | **Event:** recurs after the PR #370 mitigation (isolation probe budget 60→150 iters, waits widened to 6m). → top of Queue, escalated. |
