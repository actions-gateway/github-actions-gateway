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
| [kata-on-gke.md](kata-on-gke.md) | Kata Containers on GKE: spike + reference architecture for unprivileged kind-in-runner CI ([Q226](../STATUS.md#Q226)) | ❌ Open |
| [q242-g1-proxy-destination-allowlist.md](q242-g1-proxy-destination-allowlist.md) | G.1: admin-set destination allowlist (FQDN host suffixes + CIDRs) on the per-tenant egress proxy so CI jobs reach build dependencies (e.g. `proxy.golang.org`, internal/cloud-private IP ranges) without forfeiting per-tenant egress attribution | ❌ Open — approved; v2beta1 blocker ([Q242](../STATUS.md#Q242)), promoted from Appendix G.1 / the Q19 bundle |

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
| [gke-dogfood.md](gke-dogfood.md) | On-demand GKE cluster for dogfooding GAG's own CI — GCP setup, GAG install, workflow variable toggle, start/stop/teardown runbook | ❌ Open — turn-up done 2026-07-01 (every CI job green per-job on `gag-ci`, Q246/Q247 hold); concurrent CI matrix blocked on [Q259](../STATUS.md#Q259) |
| [dogfood-runner-rightsizing.md](dogfood-runner-rightsizing.md) | Measure peak CPU/mem per CI job class on GAG and right-size worker pod requests/limits + node pool; decide pod tiers (general + e2e) | ❌ Open — measurement pending |

## Cross-cutting

| Plan | Scope | Status |
|---|---|---|
| [docs.md](docs.md) | Documentation roadmap across phases | ✅ Done — all Phase 1/2/3 items shipped except alerting.md, deferred as [Q18](../STATUS.md#Q18) |
| [docs-six-layer-audit.md](docs-six-layer-audit.md) | Six-layer consistency audit of `docs/` (terminology, cross-refs, nav, reuse) | ✅ Done — all six layers resolved; Layer 3 metrics gap closed by Q51; the optional link-check CI gate is a separate non-blocking decision |
| [make.md](make.md) | Makefile UX (help target, e2e workflow, image var consistency) | ✅ Done — Phase 1 + Phase 2 complete; items 2.5/2.7b are cosmetic defers only |
| [k8s-best-practices.md](k8s-best-practices.md) | Project-wide Kubernetes best-practices audit (RBAC, pod security, controller correctness, CRD polish, manifests, observability, supply chain) | ✅ Done — fixes shipped (was STATUS Queue Q30–Q36, all completed); kept active (still referenced by Q74's graduation work) |
| [worker-sidecar-reap-warning.md](worker-sidecar-reap-warning.md) | Non-blocking warning + status condition + metric when a worker template has a regular (non-native) sidecar that can block pod reaping; name-list opt-out; steer to native sidecars (no reaper) | ❌ Open — [Q249](../STATUS.md#Q249) |
| [go-to-market.md](go-to-market.md) | Adoption plan (OSS, non-commercial): ICP, demand evidence vs ARC, messaging priority, channels, AI discoverability, donation posture | ⓘ Strategy — follow-ups (ARC→GAG migration guide, README problem-first) on the STATUS Queue |
| [ecosystem-integration-landscape.md](ecosystem-integration-landscape.md) | ~100 Kubernetes ecosystem integrations cataloged + mapped to GAG (conflict / integrate / interact); basis for ecosystem enhancements and "feels-native" conventions | ⓘ Research — items filed on the STATUS Queue/Deferred as Q205–Q218; Q218 (worker disruption-safety) is a v2beta1 gate |
| [website.md](website.md) | Public GitHub Pages site: MkDocs Material rendering of `docs/` + a custom landing page and "vs ARC" comparison; domain decision folded in (org move) | ✅ Done — scaffold, landing, comparison, and public launch shipped (was Q52/Q99/Q129, all completed) |

## Archive

Plans whose work has fully landed and which `docs/STATUS.md` no longer references. Moved here so `ls docs/plan/` shows active work only. The doc remains available — the rationale is often more valuable than the diff.

| Plan | Scope | Closed |
|---|---|---|
| [archive/q237-docs-quality-audit.md](archive/q237-docs-quality-audit.md) | Six-goal quality audit of the published docset: 57 ranked findings (36 goal-1 docs-vs-code drift, 17 high) with remediation batches | 2026-07-01 — Q237 audit; remediation batches Q250 (A, goal-1 high), Q251 (B, goal-1 medium), Q252 (C, goal-5/6 usability & tone) all shipped; appendix-e v1/v2 straddle split to [Q253](../STATUS.md#Q253) |
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
