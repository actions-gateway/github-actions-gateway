# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · ▶ started · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = needs a plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug`

**Maintaining this file:** see [`docs/development/maintaining-backlog.md`](development/maintaining-backlog.md) for the full rules (churn reduction, format conventions, anti-patterns). Short version:
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** create or update a plan doc in `docs/plan/`; delete the row here when done. (Skip the `▶ Started` marker unless you have a specific reason — the open PR is the in-flight signal.)
- **New item identified:** insert it in the Queue at the right priority position with the next unused ID. Batch audit-discovery items in one commit.
- **⚠️ item fully done:** move it to the Progress table as ✅.
- **`Last touched:` is one line, date only.** Do not append session narrative.

Last touched: 2026-05-31

---

## Progress

Plan-level view. ✅ = all criteria met. ⚠️ = code shipped, specific pieces remain open in the Queue below.

| Item | Labels | Status | Notes |
|---|---|---|---|
| M1: Wire-protocol probe | `milestone` | ✅ | [plan](plan/milestone-1.md) |
| M1: Unit-test coverage | `milestone` `tests` | ✅ | All 5 gaps closed — [plan](plan/milestone-1-tests.md) |
| M2: AGC controller | `milestone` | ✅ | All criteria met including live kind check (`activeSessions==1`) — [plan](plan/milestone-2.md) |
| M3: Worker pod | `milestone` | ✅ | All success criteria met; Tier-C live test green on 2026-05-30 — [plan](plan/milestone-3.md) |
| M4: GMC + proxy | `milestone` | ⚠️ | Single-tenant validated by M3 Tier-C run on 2026-05-30; multi-tenant scenario still unverified — [plan](plan/milestone-4.md) |
| M5: Hardening | `milestone` `security` | ⚠️ | Security half done; packaging, load test harness, posture scan open — [plan](plan/milestone-5.md) |
| Security hardening | `security` | ⚠️ | W2–W8/M-12/13/L-2/3/7 shipped; M-11b + live kind validation remain — [plan](plan/security.md) |
| Worker egress proxy | `security` `infra` | ⚠️ | NetworkPolicy split shipped; live `curl` validation blocked on M3/M4 — [plan](plan/worker-egress-proxy.md) |
| Docs | `docs` | ✅ | All Phase 1–3 items done; alerting.md deferred — [plan](plan/docs.md) |
| Six-layer docs audit | `docs` | ✅ | All six layers audited and fixed (0 broken links/anchors); follow-ons tracked as [Q51](#Q51) + [Q52](#Q52) — [plan](plan/docs-six-layer-audit.md) |
| Make UX | `infra` | ✅ | Phase 1 + Phase 2 done — [plan](plan/make.md) |
| Docker image speed | `speed` | ✅ | All items done or explicitly closed — [plan](plan/docker-image-speed.md) |
| e2e test speed | `speed` `tests` | ✅ | All items done — [plan](plan/e2e-tests-speed.md) |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q7"></a>Q7 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🔲 | S | **Unblocked by Q6 on 2026-05-30.** Same kind cluster + real GitHub App available; need to assert workload→proxy CONNECT + DNAT + IP-range egress with `curl` from a workload-labeled debug pod. |
| <a id="Q42"></a>Q42 | Proxy `/readyz` must gate on CONNECT listener (analogue of GMC §11.D fix) | `security` `infra` `bug` | 🔲 | S | Proxy `/healthz` returns OK before CONNECT listener binds → workers hit `connection refused` on rollouts. Same bug class as Q6; see [§11.D follow-up](plan/milestone-3.md#11d--gmc-readiness-probe-did-not-gate-on-webhook-server-start). |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🔲 | S | **Verify first** whether existing Tier-A suite already covers this — if so, delete this row. Otherwise: live probe against kind cluster + real GitHub App (both available since Q6). |
| <a id="Q21"></a>Q21 | [Pin worker Dockerfile base image digest (M-19)](plan/security.md) | `security` `infra` | 🔲 | S | Resolve `ghcr.io/actions/actions-runner:2.327.1` to `@sha256:…`; tie the digest update to the runner-version bump procedure. |
| <a id="Q9"></a>Q9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining: **H2** (rerun-API 5xx contract), **H3** (decryption-failure fallback), **M3** (`activePodCount` Pending branch). Worth picking up after 5c–5g. |
| <a id="Q22"></a>Q22 | [Repo hygiene: SECURITY.md + dependabot config](plan/security.md) | `security` `docs` | 🔲 | S | Disclosure policy + automated dep updates across 7 go.mod files. |
| <a id="Q23"></a>Q23 | [CI security scanning (govulncheck + trivy)](plan/security.md) | `security` `infra` | 🔲 | M | Per-module workspace-aware `govulncheck`; `trivy image` against each built Dockerfile in PR CI. |
| <a id="Q24"></a>Q24 | [Enforce `@sha256:` syntax on AGC_IMAGE/PROXY_IMAGE at GMC startup](plan/security.md) | `security` | 🔲 | S | Reject non-digest references; promoted from the security plan's "out of scope but worth noting" note. |
| <a id="Q25"></a>Q25 | [Restrict `:8081` health/metrics ingress (L-8)](plan/security.md) | `security` | 🔲 | S | Explicit NP ingress rule on proxy + AGC permitting only kubelet probe + Prometheus scrape selector. |
| <a id="Q26"></a>Q26 | [Remove over-declared `watch` verb on AGC Role](plan/security.md) | `security` | 🔲 | S | One-line cleanup; no Secret informer is registered. H-2 residual notes it. Overlaps partially with k8s-audit §B B4 ([Q30](#Q30)). |
| <a id="Q27"></a>Q27 | [Security operations runbook](plan/security.md) | `security` `docs` | 🔲 | S | Convert abuse heuristics from `05-security.md` into operator alerts (Secret list rate, eviction retries exhausted, etc). |
| <a id="Q30"></a>Q30 | [K8s audit — §B RBAC & cluster-wide privilege](plan/k8s-best-practices.md#b-rbac--cluster-wide-privilege-) | `security` | 🔲 | S | 🔴 GMC ClusterRole grants `rbac:escalate`, `namespaces:patch`, `secrets:list`; `applyNamespacePSA` overwrites admin edits. Overlaps [Q26](#Q26). See [k8s-best-practices.md §B](plan/k8s-best-practices.md#b-rbac--cluster-wide-privilege-). |
| <a id="Q31"></a>Q31 | [K8s audit — §C Worker/proxy pod security defaults](plan/k8s-best-practices.md#c-worker--proxy-pod-security-defaults-) | `security` | 🔲 | S | 🔴 Worker/proxy pods get no default `SecurityContext` (`runAsNonRoot`/RO-rootfs/cap-drop/seccomp) or resource limits; blocks PSA `restricted`. See [k8s-best-practices.md §C](plan/k8s-best-practices.md#c-worker--proxy-pod-security-defaults-). |
| <a id="Q32"></a>Q32 | [K8s audit — §A Controller correctness](plan/k8s-best-practices.md#a-controller-correctness-) | `bug` `infra` | 🔲 | M | 🔴 No `EventRecorder` anywhere; RunnerGroup has no Pod `Owns()` (stale `ActiveSessions`); provisioner polls instead of watching; finalizer race leaks pool maps. See [k8s-best-practices.md §A](plan/k8s-best-practices.md#a-controller-correctness-). |
| <a id="Q33"></a>Q33 | [K8s audit — §D CRD design polish](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | 🟡 Missing `+listType=map` on conditions, CEL immutability on `gitHubAppRef.name`/`securityProfile` (silent security downgrades), `MinItems`/`omitempty`/`categories`. See [k8s-best-practices.md §D](plan/k8s-best-practices.md#d-crd-design-polish-). |
| <a id="Q34"></a>Q34 | [K8s audit — §E Manifest defaults & HA](plan/k8s-best-practices.md#e-manifest-defaults--ha-) | `infra` | 🔲 | M | 🟡 GMC `replicas: 1`, no PDB/PriorityClass/`startupProbe`, ServiceMonitor/NP commented out (secure-by-default regression), no `terminationGracePeriodSeconds`. See [k8s-best-practices.md §E](plan/k8s-best-practices.md#e-manifest-defaults--ha-). |
| <a id="Q35"></a>Q35 | [K8s audit — §F Observability & operational](plan/k8s-best-practices.md#f-observability--operational-) | `infra` | 🔲 | M | 🟡 Two logger libs (`slog`+`zap`) emit incompatible JSON; no tracing; AGC missing health probes; AGC hard-codes `zap.UseDevMode(true)` in production. See [k8s-best-practices.md §F](plan/k8s-best-practices.md#f-observability--operational-). |
| <a id="Q36"></a>Q36 | [K8s audit — §G Supply chain (labels + build flags)](plan/k8s-best-practices.md#g-supply-chain-) | `security` `infra` | 🔲 | S | 🟡 G2 missing `org.opencontainers.image.*` labels on any Dockerfile (SBOM scanners miss provenance); G3 `go build` missing `-trimpath -ldflags=-buildid=` for SLSA-L3 reproducibility. G1 (worker image digest pin) is tracked by [Q21](#Q21). |
| <a id="Q38"></a>Q38 | Go best-practices: unify Go versions across modules | `infra` | 🔲 | S | Three `go` directives across 9 `go.mod` files (`1.26`/`1.26.0`/`1.26.3`); CLAUDE.md requires one. Pin all to `1.26.3`. See [go-best-practices.md §1](plan/go-best-practices.md#1-unify-go-versions). |
| <a id="Q39"></a>Q39 | Go best-practices: fix CLAUDE.md async-channel violation | `bug` | 🔲 | S | `cmd/agc/internal/listener/goroutine.go:121` `StartRenewLoop` hides done channel inside `stop` closure — CLAUDE.md async-channel rule violation. See [go-best-practices.md §2](plan/go-best-practices.md#2-async-channel-violation-startrenewloop). |
| <a id="Q40"></a>Q40 | Go best-practices: misc idiom cleanup | `bug` | 🔲 | S | Silent unmarshal swallow, `max` builtin shadow, `broker.BrokerClient` stutter, residual `interface{}` in non-test code, dead `_ = name` comment. See [go-best-practices.md §4](plan/go-best-practices.md#4-misc-idiom-cleanup). |
| <a id="Q41"></a>Q41 | Go best-practices: extend goleak coverage | `tests` | 🔲 | S | `broker/` and `cmd/worker/` spawn goroutines but no `goleak.VerifyNone` in `TestMain`. `goleak` is already a `broker/` dep. See [go-best-practices.md §3](plan/go-best-practices.md#3-extend-goleak-coverage). |
| <a id="Q49"></a>Q49 | [Per-key merge for `proxy.resources` (gaps.md fix #2)](plan/gaps.md) | `bug` `infra` | 🔲 | S | Setting `proxy.resources.requests.cpu` silently drops default memory/limits, breaks HPA math (`builder.go:248-250`). Fix in [gaps.md §2](plan/gaps.md#2-fix-proxy-resource-override-dropping-cpu-request-hpa-silent-failure). |
| <a id="Q12"></a>Q12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🔲 | L | **Unblocked by Q6 on 2026-05-30.** |
| <a id="Q28"></a>Q28 | [SBOM + cosign signing of built images](plan/security.md) | `security` `infra` | 🚫 | M | Blocked by [Q12](#Q12). Distroless + digest pinning are the foundation. |
| <a id="Q29"></a>Q29 | [API server audit policy sample](plan/security.md) | `security` `infra` | 🚫 | S | Blocked by [Q12](#Q12). Surfaces a compromised GMC's Secret `get` calls. |
| <a id="Q13"></a>Q13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | Blocked by [Q12](#Q12). **Highest "right thing" risk — project pitch is thousands of virtual sessions per AGC and nothing pins that claim.** Consider whether a minimal harness could run on the M3 Tier-C kind setup before [Q12](#Q12) lands. |
| <a id="Q14"></a>Q14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | Blocked by [Q12](#Q12). |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| <a id="Q45"></a>Q45 | Compress Progress table — drop Notes column | `docs` | 🔲 | S | Most cells just say "see plan" or restate the plan doc; the plan link in the row's name already carries the detail. Reduces edit surface and width. |
| <a id="Q47"></a>Q47 | Append-by-default for new low-priority Queue rows | `docs` | 🔲 | S | Loosen "insert at right priority position" to "append unless re-prioritizing" so row order stays stable in diffs across parallel sessions. Re-prioritization becomes a deliberate separate commit. |
| <a id="Q52"></a>Q52 | Markdown link + anchor check CI gate | `docs` `infra` `tests` | 🔲 | S | Add GitHub-slug-aware markdown link/anchor checker to `unit-test.yml`. The L2 validation script in [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) is a working reference. |
| <a id="Q51"></a>Q51 | Reconcile documented vs emitted Prometheus metrics | `infra` `docs` `bug` | 🔲 | M | 6 documented metrics never registered in code (headline `pod_creation_latency_seconds` + 5 others). Per-metric decision: implement, re-point, or mark `(planned)`. See [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) Layer 3. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
