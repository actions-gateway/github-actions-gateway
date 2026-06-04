# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · ▶ started · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = needs a plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug`

**Maintaining this file:** see [`docs/development/maintaining-backlog.md`](development/maintaining-backlog.md) for the full rules (churn reduction, format conventions, anti-patterns). Short version:
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** create or update a plan doc in `docs/plan/`; delete the row here when done. (Skip the `▶ Started` marker unless you have a specific reason — the open PR is the in-flight signal.)
- **New item identified:** decide its priority *first*, then insert it at that position (not the bottom by default) with the next unused ID. See [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry). Batch audit-discovery items in one commit.
- **Parked item (explicit trigger, no near-term intent):** put it in [Deferred](#deferred), not the Queue; move it back into the Queue at the right priority when its trigger fires. See [deferred items live below the Queue](development/maintaining-backlog.md#deferred-items-live-below-the-queue-not-in-it).
- **⚠️ item fully done:** move it to the Progress table as ✅.
- **`Last touched:` is one line, date only.** Do not append session narrative.
- **Queue `Notes` ≤ 250 characters** (hard, lint-enforced). A markdown link counts its full `[text](url)` source length — count before committing rather than waiting for the hook. Overflow → move detail to the linked plan doc.

Last touched: 2026-06-03

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
| Worker egress proxy | `security` `infra` | ⚠️ | NetworkPolicy split + Tier-A positive curl + authoring-guard NP-spec shipped; runtime negatives deferred to [Q7b](#Q7b) (kindnet NP-enforcement gap) — [plan](plan/worker-egress-proxy.md) |
| Docs | `docs` | ✅ | All Phase 1–3 items done; alerting.md deferred — [plan](plan/docs.md) |
| Six-layer docs audit | `docs` | ✅ | All six layers audited and fixed (0 broken links/anchors); follow-ons tracked as [Q51](#Q51) + [Q52](#Q52) — [plan](plan/docs-six-layer-audit.md) |
| Make UX | `infra` | ✅ | Phase 1 + Phase 2 done — [plan](plan/make.md) |
| Docker image speed | `speed` | ✅ | All items done or explicitly closed — [plan](plan/docker-image-speed.md) |
| e2e test speed | `speed` `tests` | ✅ | All items done — [plan](plan/e2e-tests-speed.md) |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q25"></a>Q25 | [Restrict `:8081` health/metrics ingress (L-8)](plan/security.md) | `security` | 🔲 | S | Explicit NP ingress rule on proxy + AGC permitting only kubelet probe + Prometheus scrape selector. |
| <a id="Q24"></a>Q24 | [Enforce `@sha256:` syntax on AGC_IMAGE/PROXY_IMAGE at GMC startup](plan/security.md) | `security` | 🔲 | S | Reject non-digest references; promoted from the security plan's "out of scope but worth noting" note. |
| <a id="Q22"></a>Q22 | [Repo hygiene: SECURITY.md + dependabot config](plan/security.md) | `security` `docs` | 🔲 | S | Disclosure policy + automated dep updates across 7 go.mod files. |
| <a id="Q23"></a>Q23 | [CI security scanning (govulncheck + trivy)](plan/security.md) | `security` `infra` | 🔲 | M | Per-module workspace-aware `govulncheck`; `trivy image` against each built Dockerfile in PR CI. |
| <a id="Q27"></a>Q27 | [Security operations runbook](plan/security.md) | `security` `docs` | 🔲 | S | Convert abuse heuristics from `05-security.md` into operator alerts (Secret list rate, eviction retries exhausted, etc). |
| <a id="Q33"></a>Q33 | [K8s audit — §D CRD design polish](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | 🟡 Missing `+listType=map` on conditions, CEL immutability on `gitHubAppRef.name`/`securityProfile` (silent security downgrades), `MinItems`/`omitempty`/`categories`. See [k8s-best-practices.md §D](plan/k8s-best-practices.md#d-crd-design-polish-). |
| <a id="Q65"></a>Q65 | [K8s audit §A6 — migrate GMC `apply*` helpers to CreateOrPatch](plan/k8s-best-practices.md#a-controller-correctness-) | `infra` | 🔲 | M | 🟡 Eleven `apply*` helpers do read-modify-write without `IsConflict` handling; migrate to `controllerutil.CreateOrPatch`. Split from Q32 §A. |
| <a id="Q34"></a>Q34 | [K8s audit — §E Manifest defaults & HA](plan/k8s-best-practices.md#e-manifest-defaults--ha-) | `infra` | 🔲 | M | 🟡 GMC `replicas: 1`, no PDB/PriorityClass/`startupProbe`, ServiceMonitor/NP commented out (secure-by-default regression), no `terminationGracePeriodSeconds`. See [k8s-best-practices.md §E](plan/k8s-best-practices.md#e-manifest-defaults--ha-). |
| <a id="Q35"></a>Q35 | [K8s audit — §F Observability & operational](plan/k8s-best-practices.md#f-observability--operational-) | `infra` | 🔲 | M | 🟡 Two logger libs (`slog`+`zap`) emit incompatible JSON; no tracing; AGC missing health probes; AGC hard-codes `zap.UseDevMode(true)` in production. See [k8s-best-practices.md §F](plan/k8s-best-practices.md#f-observability--operational-). |
| <a id="Q36"></a>Q36 | [K8s audit — §G Supply chain (labels + build flags)](plan/k8s-best-practices.md#g-supply-chain-) | `security` `infra` | 🔲 | S | 🟡 G2 missing `org.opencontainers.image.*` labels on any Dockerfile (SBOM scanners miss provenance); G3 `go build` missing `-trimpath -ldflags=-buildid=` for SLSA-L3 reproducibility. G1 (worker image digest pin) closed 2026-06-01. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🔲 | S | Verified 2026-06-01: not deletable. Operator-doc for the `--agent-key-type=ed25519` opt-in; RSA-3072 stays the default regardless. Needs probe flag extensions + manual run with real credentials. |
| <a id="Q9"></a>Q9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining: **H2** (rerun-API 5xx contract), **H3** (decryption-failure fallback), **M3** (`activePodCount` Pending branch). Worth picking up after 5c–5g. |
| <a id="Q7b"></a>Q7b | [Worker egress runtime negatives on Calico/Cilium CNI](plan/worker-egress-proxy.md#known-limitation-runtime-negative-case-enforcement-under-kindnet) | `security` `infra` `tests` | 🔲 | M | Two CI iterations showed kindnet's `kube-network-policies` does not drop egress for the Q7 negative cases (external-IP + cross-namespace pod). Re-run `WorkloadEgressBlockedToNonProxyPod` + `WorkerCannotReachK8sAPI` on a kind cluster with Calico or Cilium installed. |
| <a id="Q40"></a>Q40 | Go best-practices: misc idiom cleanup | `bug` | 🔲 | S | Silent unmarshal swallow, `max` builtin shadow, `broker.BrokerClient` stutter, residual `interface{}` in non-test code, dead `_ = name` comment. See [go-best-practices.md §4](plan/go-best-practices.md#4-misc-idiom-cleanup). |
| <a id="Q41"></a>Q41 | Go best-practices: extend goleak coverage | `tests` | 🔲 | S | `broker/` and `cmd/worker/` spawn goroutines but no `goleak.VerifyNone` in `TestMain`. `goleak` is already a `broker/` dep. See [go-best-practices.md §3](plan/go-best-practices.md#3-extend-goleak-coverage). |
| <a id="Q12"></a>Q12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🔲 | L | **Unblocked by Q6 on 2026-05-30.** |
| <a id="Q28"></a>Q28 | [SBOM + cosign signing of built images](plan/security.md) | `security` `infra` | 🚫 | M | Blocked by [Q12](#Q12). Distroless + digest pinning are the foundation. |
| <a id="Q29"></a>Q29 | [API server audit policy sample](plan/security.md) | `security` `infra` | 🚫 | S | Blocked by [Q12](#Q12). Surfaces a compromised GMC's Secret `get` calls. |
| <a id="Q13"></a>Q13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | Blocked by [Q12](#Q12). **Highest "right thing" risk — project pitch is thousands of virtual sessions per AGC and nothing pins that claim.** Consider whether a minimal harness could run on the M3 Tier-C kind setup before [Q12](#Q12) lands. |
| <a id="Q14"></a>Q14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | Blocked by [Q12](#Q12). |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| <a id="Q59"></a>Q59 | [Pre-acquisition admission control (capacity-gated `acquirejob`)](plan/acquire-admission-control.md) | `infra` `speed` | 🔲 | L | AGC acquires jobs before checking pod capacity, so ceiling-held jobs are claimed-then-dropped under pressure. Add a capacity gate before `acquirejob` (not a durable queue — GitHub is the queue). |
| <a id="Q45"></a>Q45 | Compress Progress table — drop Notes column | `docs` | 🔲 | S | Most cells just say "see plan" or restate the plan doc; the plan link in the row's name already carries the detail. Reduces edit surface and width. |
| <a id="Q52"></a>Q52 | Markdown link + anchor check CI gate | `docs` `infra` `tests` | 🔲 | S | Add GitHub-slug-aware markdown link/anchor checker to `unit-test.yml`. The L2 validation script in [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) is a working reference. |
| <a id="Q66"></a>Q66 | YAML lint + manifest schema-validate CI gate | `infra` `tests` | 🔲 | S | No CI validation of hand-maintained k8s manifests (`config/**/*.yaml`); malformed RBAC/CRD/policy files ship silently (the Q32 §A tenant-role edits were hand-validated only). Add `yamllint` + `kubeconform` to `unit-test.yml`. |
| <a id="Q68"></a>Q68 | Enforce single Go version across all workspace files | `infra` `tests` | 🔲 | S | CLAUDE.md's "all go modules use the same Go version" rule is unenforced; the 2 `go.work.gen` files drifted to 1.26/1.26.0, breaking `make manifests`. Add a CI check that the `go` directive matches across go.work, all go.mod, and go.work.gen. |
| <a id="Q51"></a>Q51 | Reconcile documented vs emitted Prometheus metrics | `infra` `docs` `bug` | 🔲 | M | 6 documented metrics never registered in code (headline `pod_creation_latency_seconds` + 5 others). Per-metric decision: implement, re-point, or mark `(planned)`. See [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) Layer 3. |
| <a id="Q55"></a>Q55 | Verify provisioner-test goleak cascade fix held in CI | `tests` `bug` | 🔲 | S | Intermittent ~20-test goleak cascade in `internal/provisioner` fixed by `waitForPodCreated` helper in 59c0714; delete row once CI is clean. If flakes recur, migrate remaining ~18 Eventually-on-Pod sites to the helper. |
| <a id="Q60"></a>Q60 | [Competitive analysis — GAG vs ARC-adjacent runner/queue tooling](design/appendix-d-alternatives-considered.md) | `docs` | 🔲 | M | Competitive analysis vs ARC-adjacent tooling: Kueue, Exostellar (verify the Kueue-under-ARC GPU pattern), KEDA. Expands [appendix-d](design/appendix-d-alternatives-considered.md). Narrow Kueue-vs-admission angle is in [Q59](#Q59). |
| <a id="Q62"></a>Q62 | Short per-attempt timeout on IP-range `/meta` fetch | `infra` `speed` | 🔲 | S | GMC HTTP client's 60s timeout is shared; a stalled `/meta` fetch burns 60s before the Q61 backoff retries. Add a ~10s per-attempt `context.WithTimeout` in `HTTPGitHubIPRangeFetcher.FetchIPRanges`. Follow-on to Q61. |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
