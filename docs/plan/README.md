# Plans

Topic-organized index of plan files. For current status and priorities, see [docs/STATUS.md](../STATUS.md).

Each file is a self-contained plan with rationale, scope, and (where appropriate) a status table near the top. Authoritative state always lives in the individual file.

Legend: ✅ done, ⚠️ partial / mixed, ❌ open, ⓘ informational
(forward-looking spec or design rationale, no progress to track).

## Implementation roadmap

The five-milestone delivery from
[docs/design/06-implementation-phases.md](../design/06-implementation-phases.md).

| Plan | Scope | Status |
|---|---|---|
| [milestone-1.md](milestone-1.md) | Wire-protocol probe; broker + githubapp packages | ✅ Done |
| [milestone-2.md](milestone-2.md) | AGC controller, reconciler, agent pool, token manager | ⚠️ Mostly done — code shipped; goroutine-leak integration suite + live `kind` `activeSessions == 1` verification still open |
| [milestone-3.md](milestone-3.md) | Worker pod, Named Pipe handoff, pod provisioner, eviction retry | ⚠️ Code done — end-to-end green-checkmark gated on Investigation A (Named Pipe protocol) |
| [milestone-4.md](milestone-4.md) | GMC, ActionsGateway CRD, proxy binary, webhook, TLS pinning | ⚠️ Code done — live `kind` multi-tenant validation pending (blocked on M3) |
| [milestone-5.md](milestone-5.md) | Hardening + 1,000-session load testing + posture audit + packaging | ⚠️ Security-half done via security.md W2/W7/W8 + ResourceQuota; packaging, `test/load/` harness, `kube-bench`/`polaris` scan, gVisor validation still open |

## Security

| Plan | Scope | Status |
|---|---|---|
| [security.md](security.md) | OWASP-style code review with finding-level workstreams | ⚠️ Phase 1 + 2 + 3 backlog all done in code; **M-11b** (live Ed25519 GitHub probe) and Phase 1 live `kind` validation remain |
| [worker-egress-proxy.md](worker-egress-proxy.md) | Worker traffic must route through per-tenant proxy pool | ⚠️ Code done (NetworkPolicy split, commit `4932ce7`); live `curl` validation pending |

## Test plans

Per-milestone test gap plans. The durable design rationale for what the
unit/integration/e2e layers cover lives in
[`docs/design/07-test-plan.md`](../design/07-test-plan.md); developer
run commands live in
[`docs/development/testing.md`](../development/testing.md).

| Plan | Scope | Status |
|---|---|---|
| [milestone-1-tests.md](milestone-1-tests.md) | M1 unit-test coverage gaps | ✅ Done — all five gaps closed |
| [milestone-3-tests.md](milestone-3-tests.md) | M3 metric/decryption/eviction test gaps | ❌ Open — H/M/L items not yet implemented |

## Speed improvements

Performance plans for build and test pipelines. Each has inline ✓
markers per item.

| Plan | Scope | Status |
|---|---|---|
| [docker-image-speed.md](docker-image-speed.md) | Image build + load-into-kind time | ⚠️ Has own Status table — §1/2/4/5 done; §7/8/9/12 still TODO |
| [unit-tests-speed.md](unit-tests-speed.md) | Four targeted unit-test latency cuts (~6s total) | ❌ Open — no ✓ markers on any of the four items |
| [e2e-tests-speed.md](e2e-tests-speed.md) | Five e2e suite improvements | ⚠️ Mixed — §2, §3 marked ✓; §1, §4, §5 not |

## Cross-cutting

| Plan | Scope | Status |
|---|---|---|
| [gaps.md](gaps.md) | Three code-level fixes surfaced by doc audit (CRD eviction fields, proxy resource merge, credential rotation observability) | ⚠️ Fixes #1 and #3 done; fix #2 (per-key `proxy.resources` merge — HPA silent failure) still open |
| [docs.md](docs.md) | Documentation roadmap across phases | ⚠️ Phase 1 fully done; 4 items open in Phase 2/3 |
| [docs-six-layer-audit.md](docs-six-layer-audit.md) | Six-layer consistency audit of `docs/` (terminology, cross-refs, nav, reuse) | ⚠️ Layer 2 healthy; Layers 1/4/5/6 have audit + small fixes open |
| [make.md](make.md) | Makefile UX (help target, e2e workflow, image var consistency) | ⚠️ Phase 1 done; Phase 2 has open drift items (image vars, envtest, `all` semantics) |
| [k8s-best-practices.md](k8s-best-practices.md) | Project-wide Kubernetes best-practices audit (RBAC, pod security, controller correctness, CRD polish, manifests, observability, supply chain) | ⚠️ Findings logged; fixes open as STATUS Queue Q30–Q36 |
| [go-best-practices.md](go-best-practices.md) | Small Go-idiom cleanups: unify module versions, fix async-channel violation, extend goleak coverage, misc | ⚠️ Findings logged; fixes open as STATUS Queue Q38–Q41 |
| [logging-audit.md](logging-audit.md) | Cross-module log-call-site audit: format fragmentation (slog/zap), credential-leak surface, hot-path spam, correlation, per-tenant log level | ⚠️ Theme A (F1, JSON unify) + Theme B (body redaction) ✅ done; Themes D–G open as STATUS Queue Q87–Q89 |
| [acquire-admission-control.md](acquire-admission-control.md) | Gate worker-pod capacity *before* `acquirejob` so jobs aren't claimed-then-dropped under pressure; durable internal queue considered and rejected | ⓘ Design sketch — open as STATUS Queue Q59 |
| [competitive-analysis.md](competitive-analysis.md) | Unverified working notes on GAG vs ARC per-benefit advantages + open questions to verify; feeds the comparison content | ⓘ Notes for STATUS Queue Q60 — verify and fold into [appendix-d](../design/appendix-d-alternatives-considered.md) |
| [platform-owned-quota.md](platform-owned-quota.md) | Remove tenant-authored `spec.namespaceQuota`; platform owns Namespace + `ResourceQuota` + `LimitRange`; GMC drops quota write RBAC | ❌ Open — design captured, breaking CRD change pre-1.0; STATUS Queue Q130 |

## Archive

Plans whose work has fully landed and which `docs/STATUS.md` no longer references. Moved here so `ls docs/plan/` shows active work only. The doc remains available — the rationale is often more valuable than the diff.

| Plan | Scope | Closed |
|---|---|---|
| [archive/milestone-2-tests.md](archive/milestone-2-tests.md) | M2 unit + envtest gaps (11 items) | 2026-05-29 — banner: "All 9 gaps shipped" |
| [archive/milestone-4-tests.md](archive/milestone-4-tests.md) | M4 builder + IPRange + webhook test gaps (8 items) | 2026-05-30 — `TestBuildNoProxy`, `TestBuildNetworkPolicy`, `TestHTTPFetcher*`, `TestBuildProxyServiceAddr`, `TestServer_ListenAndServe`, `TestIPRangeReconciler_Start` all present; `ValidateDelete` covered inline in webhook test |
| [archive/integration-tests-speed.md](archive/integration-tests-speed.md) | Five integration polling/sleep cuts | 2026-05-30 — superseded; GMC integration tests now use Gomega defaults (~10ms polling), faster than the 25ms target |
| [archive/rename-agc-to-controller.md](archive/rename-agc-to-controller.md) | Rename on-cluster `actions-gateway-agc` → `actions-gateway-controller` to match docs | 2026-05-30 — zero `"actions-gateway-agc"` literals remain in `cmd/`; M3 Tier-C kind run validated the rename live |

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
  section. Update any other in-repo links to the new path. The doc
  stays available; the working directory just gets less noisy. See the
  full protocol in [`docs/development/maintaining-backlog.md`](../development/maintaining-backlog.md#archiving-completed-plan-docs).

Add a row to this README when creating, completing, or archiving a plan.
