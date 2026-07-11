# Plans

Topic-organized index of plan files. For current status and priorities, see [docs/STATUS.md](../STATUS.md).

Each file is a self-contained plan with rationale, scope, and (where appropriate) a status table near the top. Authoritative state always lives in the individual file.

Legend: ✅ done, ⚠️ partial / mixed (open **Queue** item remains), 💤 deferred
(parked with a trigger, tracked in [STATUS.md Deferred](../STATUS.md#deferred)),
❌ open, ⓘ informational (forward-looking spec or design rationale, no progress
to track). A plan with only deferred residuals is ✅, not ⚠️ — see
[maintaining-backlog.md](../development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count).

## Implementation roadmap

The five-milestone delivery from
[docs/design/06-implementation-phases.md](../design/06-implementation-phases.md).

| Plan | Scope | Status |
|---|---|---|
| [milestone-1.md](milestone-1.md) | Wire-protocol probe; broker + githubapp packages | ✅ Done |
| [milestone-2.md](milestone-2.md) | AGC controller, reconciler, agent pool, token manager | ✅ Done — full session lifecycle exercised end-to-end by M3's real-GitHub dispatch e2e; goleak coverage landed |
| [milestone-3.md](milestone-3.md) | Worker pod, Named Pipe handoff, pod provisioner, eviction retry | ✅ Done — Investigation A (Named Pipe) complete; Q6 Tier-C real-GitHub dispatch validated 2026-05-30 |
| [milestone-4.md](milestone-4.md) | GMC, ActionsGateway CRD, proxy binary, webhook, TLS pinning | ✅ Done — all success criteria live-validated on a real `kind` cluster 2026-06-11/12 (§12) |
| [milestone-5.md](milestone-5.md) | Hardening + 1,000-session load testing + posture audit + packaging | ⚠️ Packaging (Q12) now live-validated end-to-end (Q219, §1.5 — found+fixed an egress-proxy registration bug); load harness (Q13), polaris + kube-bench (Q14) shipped. Only staging-cluster residuals remain: 1,000-session proxy-HPA-under-burst + gVisor isolation ([Q15](../STATUS.md#Q15), deferred) |

## Security

| Plan | Scope | Status |
|---|---|---|
| [security.md](security.md) | OWASP-style code review with finding-level workstreams | ✅ Done — every workstream shipped; sole residual is the deferred live Ed25519 probe (M-11b, [Q11](../STATUS.md#Q11)). Phase 1 live `kind` validation covered by the M3/M4 live runs |
| [worker-egress-proxy.md](worker-egress-proxy.md) | Worker traffic must route through per-tenant proxy pool | ✅ Done — NetworkPolicy split shipped (commit `4932ce7`); proxied worker→GitHub egress live-validated via M4 §12 |
| [kata-on-gke.md](kata-on-gke.md) | Kata Containers on GKE: spike + reference architecture for unprivileged kind-in-runner CI (Q226 spike ✅ GO, live-validated) | ❌ Open — reference arch delivered; CI rollout tracked as [Q286](../STATUS.md#Q286) |
| [q242-g1-proxy-destination-allowlist.md](q242-g1-proxy-destination-allowlist.md) | G.1: admin-set destination allowlist (FQDN host suffixes + CIDRs) on the per-tenant egress proxy so CI jobs reach build dependencies (e.g. `proxy.golang.org`, internal/cloud-private IP ranges) without forfeiting per-tenant egress attribution | ❌ Open — approved; v2beta1 blocker ([Q242](../STATUS.md#Q242)), promoted from Appendix G.1 / the Q19 bundle |
| [q243-egress-ip-reference-arch.md](q243-egress-ip-reference-arch.md) | Per-tenant egress-IP reference architecture: how the proxy pool's per-tenant choke point is bound to a distinct, stable source IP at GitHub — Cilium Egress Gateway vs per-tenant cloud NAT — plus single-tenant-direct (dogfood) vs production multi-tenant topology, cost model, and a deferred live-validation plan | ❌ Open — design + live validation done (per-range Cloud NAT gives distinct+stable per-tenant IPs, [campaign](q243-q245-q230-live-validation-campaign.md)); OPEN residual = EgressProxy API can't bind a tenant's proxy to one egress IP. v2beta1 blocker ([Q243](../STATUS.md#Q243)) |
| [q245-fqdn-intent-backend-split.md](q245-fqdn-intent-backend-split.md) | Decouple tenant egress intent (`CIDR`\|`FQDN`) from the operator-chosen FQDN backend (`--fqdn-policy-backend=none\|cilium\|calico\|gke`), killing the per-CNI enum fragmentation; add a `gke` backend emitting `networking.gke.io FQDNNetworkPolicy`, with the union-vs-default-deny composition invariant | ⓘ Design reference — split + `gke` backend shipped (#576) and **live-GKE validated** (enforcement + fail-closed, [campaign](q243-q245-q230-live-validation-campaign.md)); Phase-3 `aks`/`eks` cluster-scoped backends remain a separate optional follow-up |
| [q243-q245-q230-live-validation-campaign.md](q243-q245-q230-live-validation-campaign.md) | One coordinated live-GKE DPv2 spike batching three deferred egress residuals: Q245 FQDNNetworkPolicy enforcement, Q230 DNS-under-egress-NP, Q243 per-tenant egress IP (cloud NAT) — on one throwaway cluster, torn down same-session | ⓘ Evidence record — Q245 PASS, Q230 PASS, Q243 mechanism PASS + API-binding gap ([Q243](../STATUS.md#Q243)); cluster torn down 2026-07-07 |
| [q284-podscheduling-surface.md](q284-podscheduling-surface.md) | Add `topologySpreadConstraints` + `priorityClassName` to the `PodScheduling` block on `EgressProxy`/`ActionsGateway`; records the governance decision that `priorityClassName` needs a **separate infra allowlist** (reusing the worker one lets a tenant lift its workers to infra priority and preempt other tenants' proxies), that `topologySpreadConstraints` composes with rather than replaces the built-in anti-affinity, and that v2 `ActionsGateway` has no validating webhook yet | ❌ Open — design agreed, unimplemented ([Q284](../STATUS.md#Q284)); §2 promotes into `docs/design/05-security.md` on close |

## Test plans

Per-milestone test gap plans. The durable design rationale for what the
unit/integration/e2e layers cover lives in
[`docs/design/07-test-plan.md`](../design/07-test-plan.md); developer
run commands live in
[`docs/development/testing.md`](../development/testing.md).

| Plan | Scope | Status |
|---|---|---|
| [milestone-1-tests.md](milestone-1-tests.md) | M1 unit-test coverage gaps | ✅ Done — all five gaps closed |
| [coverage-to-75-per-module.md](coverage-to-75-per-module.md) | Every Go module's hand-written unit-test coverage to ≥75% (Q255) | ✅ Done — all 8 code modules ≥75% (probe/gmc reached via a `runProbe`/fake-client refactor + tests) |
| [q291-e2e-calico-egress-github-flake.md](q291-e2e-calico-egress-github-flake.md) | Three real-GitHub egress specs (2 proxy-connect + direct-egress) red the `e2e-calico` leg together when the Felix ipBlock-programming / GitHub-dial window outlasts the curl retry budget under CI load; widen the bounded retry budget without weakening assertions | ❌ Open — mitigation shipped (retry budget 60/90s→150s, ceiling→4m); keep open until `e2e-calico` soaks clean on `main` ([Q291](../STATUS.md#Q291)) |

## Speed improvements

Performance plans for build and test pipelines. Each has inline ✓
markers per item.

| Plan | Scope | Status |
|---|---|---|
| [docker-image-speed.md](docker-image-speed.md) | Image build + load-into-kind time | ✅ Done — every item shipped (§1/2/4/5/8/9/13) or explicitly 🚫 not pursued (§7/12); §3/6/10/11 obsoleted by vendoring + in-cluster registry |
| [unit-tests-speed.md](unit-tests-speed.md) | Four targeted unit-test latency cuts (~6s total) | 💤 Deferred — parked as [Q17](../STATUS.md#Q17), revive when CI latency becomes the bottleneck |
| [e2e-tests-speed.md](e2e-tests-speed.md) | E2E suite + CI-pipeline speed improvements | ✅ Done — Round 1 (§1–§14) and Round 2 (§15–§18) all shipped (the top-of-file TOC ✓ markers lag the authoritative status tables) |

## Deployment

| Plan | Scope | Status |
|---|---|---|
| [gke-dogfood.md](gke-dogfood.md) | On-demand GKE cluster for dogfooding GAG's own CI — GCP setup, GAG install, workflow variable toggle, start/stop/teardown runbook | ✅ Complete (2026-07-07) — turn-up + per-job-green + concurrent-matrix-green on the ScaleSet default (Q224 closed via Q264 P4, #545); v2beta1 dogfood path live (Q231). Turn-up findings Q246/Q247/Q254/Q259/Q260 all resolved. Stays as the living operational runbook |
| [dogfood-runner-rightsizing.md](dogfood-runner-rightsizing.md) | Measure peak CPU/mem per CI job class on GAG and right-size worker pod requests/limits + node pool; decide pod tiers (general + e2e) | ✅ Complete (2026-07-07) — disk-class ceiling resolved; general-worker (2Gi/3Gi) + e2e-worker (runner req 3vCPU/1Gi/3Gi, dind 3Gi/4Gi) pods right-sized from measured peak; "small" tier measured and declined. Kata end-state deferred to [Q286](../STATUS.md#Q286) |

## Cross-cutting

| Plan | Scope | Status |
|---|---|---|
| [docs.md](docs.md) | Documentation roadmap across phases | ✅ Done — all Phase 1/2/3 items shipped except alerting.md, deferred as [Q18](../STATUS.md#Q18) |
| [docs-six-layer-audit.md](docs-six-layer-audit.md) | Six-layer consistency audit of `docs/` (terminology, cross-refs, nav, reuse) | ✅ Done — all six layers resolved; Layer 3 metrics gap closed by Q51; the optional link-check CI gate is a separate non-blocking decision |
| [make.md](make.md) | Makefile UX (help target, e2e workflow, image var consistency) | ✅ Done — Phase 1 + Phase 2 complete; items 2.5/2.7b are cosmetic defers only |
| [go-to-market.md](go-to-market.md) | Adoption plan (OSS, non-commercial): ICP, demand evidence vs ARC, messaging priority, channels, AI discoverability, donation posture | ⓘ Strategy — follow-ups (ARC→GAG migration guide, README problem-first) on the STATUS Queue |
| [ecosystem-integration-landscape.md](ecosystem-integration-landscape.md) | ~100 Kubernetes ecosystem integrations cataloged + mapped to GAG (conflict / integrate / interact); basis for ecosystem enhancements and "feels-native" conventions | ⓘ Research — items filed on the STATUS Queue/Deferred as Q205–Q218; Q218 (worker disruption-safety) is a v2beta1 gate |
| [v1-classic-sunset-review.md](v1-classic-sunset-review.md) | Two-axis (v1alpha1 API vs classic protocol) strategic review of the "sunset v1 faster" hypothesis: per-workload viability, scaling/perf, structural-ceiling verdict, and a sequenced sunset-timeline recommendation | ⓘ Review — endorses the Q264/Q74 plan; recommends accelerate positioning, hold both removals; proposed follow-ups for the maintainer to file |
| [q264-scale-set-protocol.md](q264-scale-set-protocol.md) | Migrate AGC job acquisition from the classic many-acquirers protocol to the runner-scale-set protocol; phased P1–P5 with the default flip, and the structural fix for the fan-out distinct-delivery starvation classic cannot solve | ▶ In progress — P1–P5 shipped (ScaleSet is the default); residual is the one-minor Classic deprecation window, then classic/v1alpha1 removal ([Q264](../STATUS.md#Q264)) |
| [q273-v2-front-door.md](q273-v2-front-door.md) | Route the front door (README, onboarding, positioning) to the v2 API, add deprecate-v1 banners, and make the `gag-migrate` v1→v2 story exemplary — the do-now slice of the sunset review §6.2 ([Q273](../STATUS.md#Q273)) | ▶ In progress — do-now front-door/banners/migration slice landing; full v2-only (v1 removal) gated on v2beta1 (Q74) |
| [website.md](website.md) | Public GitHub Pages site: MkDocs Material rendering of `docs/` + a custom landing page and "vs ARC" comparison; domain decision folded in (org move) | ✅ Done — scaffold, landing, comparison, and public launch shipped (was Q52/Q99/Q129, all completed) |

## Archive

Plans whose work has fully landed and which `docs/STATUS.md` no longer references. Moved here so `ls docs/plan/` shows active work only. The doc remains available — the rationale is often more valuable than the diff.

| Plan | Scope | Closed |
|---|---|---|
| [archive/q223-worker-scaleup-rate-limit.md](archive/q223-worker-scaleup-rate-limit.md) | Opt-in, default-off per-RunnerGroup token bucket (`spec.scaleUp.maxPerSecond`/`burst`) capping worker-pod *creation rate* (not count) to smooth cold-start stampedes on shared egress (NAT/firewall/VPN); gates pod creation in the provisioner, composes with the quota-retry wait; `actions_gateway_worker_scaleup_throttled_total` metric | 2026-07-06 — Q223 (G.11); v1 RunnerGroup + v2 RunnerSet field, `golang.org/x/time/rate` limiter, unit + provisioner-behaviour + envtest coverage, operator docs |
| [archive/worker-sidecar-reap-warning.md](archive/worker-sidecar-reap-warning.md) | Non-blocking warning + `PossibleReapBlockingSidecar` condition + `actions_gateway_reap_blocking_sidecar_templates` metric when a worker template has a regular (non-native) sidecar that can block pod reaping; name-list opt-out; steer to native sidecars (no reaper) | 2026-07-03 — Q249; detection helper in `api/v2alpha1`, GMC admission warning, AGC RunnerSet condition+gauge; unit + webhook + envtest coverage; operator docs |
| [archive/q237-docs-quality-audit.md](archive/q237-docs-quality-audit.md) | Six-goal quality audit of the published docset: 57 ranked findings (36 goal-1 docs-vs-code drift, 17 high) with remediation batches | 2026-07-01 — Q237 audit; remediation batches Q250 (A, goal-1 high), Q251 (B, goal-1 medium), Q252 (C, goal-5/6 usability & tone) all shipped; appendix-e v1/v2 straddle split to Q253 (resolved) |
| [archive/q246-release-asset-timeout-live-diagnosis.md](archive/q246-release-asset-timeout-live-diagnosis.md) | Live cold-run diagnosis of the dogfood release-asset download timeout: (a) Q61 cache race vs (b) Q247 CPU starve | 2026-07-01 — Q246: confirmed (a) the Q61 cold-start cache race (per-CR reconcile blanks the direct-egress allowlist from an empty cache; live-measured ~25s window on `gag-dogfood`). Fix: preserve an existing NP's egress while the cache warms. (b) CPU is only an amplifier. Findings folded into [gke-dogfood.md](gke-dogfood.md) |
| [archive/q235-worker-wrapper-injection.md](archive/q235-worker-wrapper-injection.md) | Inject the `cmd/worker` wrapper into worker pods at runtime so the default install and any `actions/runner`-derived (ARC) image run jobs without a baked-in wrapper image | 2026-06-28 — Q235: OCI image volume (K8s ≥1.33) / initContainer fallback, GMC forwards `WRAPPER_IMAGE`; default-on; e2e-validated on kindnet + Calico (#437). Live GKE re-validate folds into Q224 |
| [archive/q187-air-gapped-install.md](archive/q187-air-gapped-install.md) | Air-gapped / private-registry install: chart image-pull-secret support + per-image registry overrides (digests preserved) + air-gapped install guide | 2026-06-26 — Q187: `imagePullSecrets` on the GMC pod; runtime AGC/proxy/worker covered by the SA-attach pattern; `docs/operations/air-gapped-install.md` |
| [archive/q205-label-metric-naming-audit.md](archive/q205-label-metric-naming-audit.md) | `app.kubernetes.io/*` recommended labels on all created objects + metric/span semconv alignment before the v2beta1 freeze | 2026-06-26 — Q205: shared `api/apilabels` helper, `renewjob_errors_total`→`renew_job_errors_total`, span attrs → `k8s.*`/`gateway.*`; envtest-asserted |
| [archive/milestone-2-tests.md](archive/milestone-2-tests.md) | M2 unit + envtest gaps (11 items) | 2026-05-29 — banner: "All 9 gaps shipped" |
| [archive/milestone-4-tests.md](archive/milestone-4-tests.md) | M4 builder + IPRange + webhook test gaps (8 items) | 2026-05-30 — `TestBuildNoProxy`, `TestBuildNetworkPolicy`, `TestHTTPFetcher*`, `TestBuildProxyServiceAddr`, `TestServer_ListenAndServe`, `TestIPRangeReconciler_Start` all present; `ValidateDelete` covered inline in webhook test |
| [archive/integration-tests-speed.md](archive/integration-tests-speed.md) | Five integration polling/sleep cuts | 2026-05-30 — superseded; GMC integration tests now use Gomega defaults (~10ms polling), faster than the 25ms target |
| [archive/rename-agc-to-controller.md](archive/rename-agc-to-controller.md) | Rename on-cluster `actions-gateway-agc` → `actions-gateway-controller` to match docs | 2026-05-30 — zero `"actions-gateway-agc"` literals remain in `cmd/`; M3 Tier-C kind run validated the rename live |
| [archive/gaps.md](archive/gaps.md) | Three code-level fixes from the doc audit (CRD eviction fields, per-key `proxy.resources` merge, credential-rotation observability) | 2026-06-01 — all three fixes shipped |
| [archive/go-best-practices.md](archive/go-best-practices.md) | Go-idiom cleanups: module-version unification, async-channel fix, goleak coverage | Q38–Q41 all shipped |
| [archive/milestone-3-tests.md](archive/milestone-3-tests.md) | M3 metric/decryption/eviction test gaps | 2026-05-30 — H1–H5 + M1–M4 merged (`17a7f5c`); L items done/obsolete (Q9) |
| [archive/acquire-admission-control.md](archive/acquire-admission-control.md) | Gate worker-pod capacity before `acquirejob`; in-cluster queue rejected | Q59 — implemented |
| [archive/competitive-analysis.md](archive/competitive-analysis.md) | GAG vs ARC per-benefit working notes; fed the comparison content | Q60 — verified + folded into [appendix-d](../design/appendix-d-alternatives-considered.md) |
| [archive/platform-owned-quota.md](archive/platform-owned-quota.md) | Remove tenant `spec.namespaceQuota`; platform owns Namespace + `ResourceQuota` + `LimitRange` | 2026-06-14 — Q130, breaking CRD change pre-1.0 |
| [archive/logging-audit.md](archive/logging-audit.md) | Cross-module log-call-site audit: format fragmentation, credential-leak surface, hot-path spam, correlation, per-tenant log level | Q86–Q89 — all themes shipped (Theme A was the 1.0-gating JSON unification) |
| [archive/k8s-best-practices.md](archive/k8s-best-practices.md) | Project-wide Kubernetes best-practices audit (RBAC, pod security, controller correctness, CRD polish, manifests, observability, supply chain) | 2026-07-06 — Q30–Q36 all shipped; D7 conversion-webhook scaffolding deferred to v2beta1 graduation (Q74), H2 live-e2e companion deferred ([Q274](../STATUS.md#Q274)). Durable A6 (`CreateOrPatch` vs SSA) + HTTP/2 Rapid-Reset facts promoted to [02-architecture.md](../design/02-architecture.md) + [05-security.md](../design/05-security.md) |

## Conventions

When adding a new plan:

- Put it at the top of the file: a one-paragraph "what and why," then a
  **Status at a glance** table if there are 3+ discrete work items with
  mixed state. The table is the index a returning reader scans first.
- Cite code with file:line links. They go stale, but stale links are
  easier to fix than missing ones.
- Mark deferred or accepted items explicitly (⚠️ Partial — *what was
  accepted and why*). Silent omissions become land mines.
- Once everything in a plan ships, leave the plan in place with the
  status table updated to ✅ Done. Don't delete it — the rationale
  is more valuable than the diff.

When a plan fully closes:

- If `docs/STATUS.md` still references it (Progress table or any Queue
  row), leave it under `docs/plan/`.
- Once STATUS.md no longer references it, `git mv` it to
  `docs/plan/archive/` and move its row in this README to the Archive
  section. Update any other in-repo links to the new path **and the moved
  doc's own relative links** (dropping into `archive/` adds one `../` level).
  The doc stays available; the working directory just gets less noisy. See the
  full protocol in [`docs/development/maintaining-backlog.md`](../development/maintaining-backlog.md#archiving-completed-plan-docs).
- **Do this on close, not in a later audit** — in the same change that drops the
  plan's last STATUS reference. `make plan-index-check` (part of `make check`)
  fails when an active, non-`ⓘ` plan here is no longer referenced by STATUS.md,
  so a forgotten archival can't ship silently.

Add a row to this README when creating, completing, or archiving a plan.
