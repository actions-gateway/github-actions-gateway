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

Last touched: 2026-06-08

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
| M5: Hardening | `milestone` `security` | ⚠️ | Security half + polaris posture scan done; packaging, load test harness open — [plan](plan/milestone-5.md) |
| Release 1.0 | `milestone` | ⚠️ | Bar = installable + multi-tenant + trustworthy; load test ([Q13](#Q13)) & gVisor ([Q15](#Q15)) deferred post-1.0. Gating: `1.0-gate` Queue rows + M4 live e2e + docs-honesty pass — [plan](plan/release-1.0.md) |
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
| <a id="Q71"></a>Q71 | Live-validate runner 2.334.0 registration (Tier C) | `tests` `1.0-gate` | 🔲 | S | Runner bumped 2.327.1→2.334.0 (#137); `runnerVersion` is a contract GitHub validates at session creation. Unit + Tier-A/B pass, Tier-C is CI-skipped. Run `E2E_GMC_TenantProvisioning` with real App creds to confirm GitHub accepts 2.334.0. |
| <a id="Q9"></a>Q9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining: **H2** (rerun-API 5xx contract), **H3** (decryption-failure fallback), **M3** (`activePodCount` Pending branch). Worth picking up after 5c–5g. |
| <a id="Q7b"></a>Q7b | [Worker egress runtime negatives on Calico/Cilium CNI](plan/worker-egress-proxy.md#known-limitation-runtime-negative-case-enforcement-under-kindnet) | `security` `infra` `tests` | 🔲 | M | Two CI iterations showed kindnet's `kube-network-policies` does not drop egress for the Q7 negative cases (external-IP + cross-namespace pod). Re-run `WorkloadEgressBlockedToNonProxyPod` + `WorkerCannotReachK8sAPI` on a kind cluster with Calico or Cilium installed. |
| <a id="Q83"></a>Q83 | Tier-A verify GMC manager NetworkPolicy enforcement | `security` `infra` `tests` | 🔲 | S | Q34/E5 enabled the manager NP by default (metrics :8443 limited to `metrics: enabled`; webhook 9443 open-source). Tier-A verify runtime: scrape denied unlabeled / allowed labeled, admission still works. Kindnet caveat — see [Q7b](#Q7b). |
| <a id="Q12"></a>Q12 | [M5 packaging — Helm chart](plan/milestone-5.md#11-install-vehicle--decided-helm-chart) | `milestone` `1.0-gate` | 🔲 | L | Chart landed (GMC core; `helm lint`/`template`/`kubeconform` clean offline). Remaining: live `helm install` working-tenant proof (track A, needs creds+kind). See [plan](plan/q12-helm-chart.md). |
| <a id="Q29"></a>Q29 | [API server audit policy sample](plan/security.md) | `security` `infra` | 🚫 | S | Blocked by [Q12](#Q12). Surfaces a compromised GMC's Secret `get` calls. |
| <a id="Q13"></a>Q13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | Blocked by [Q12](#Q12). **Highest "right thing" risk — project pitch is thousands of virtual sessions per AGC and nothing pins that claim.** Consider whether a minimal harness could run on the M3 Tier-C kind setup before [Q12](#Q12) lands. |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| <a id="Q59"></a>Q59 | [Pre-acquisition admission control (capacity-gated `acquirejob`)](plan/acquire-admission-control.md) | `infra` `speed` | 🔲 | L | AGC acquires jobs before checking pod capacity, so ceiling-held jobs are claimed-then-dropped under pressure. Add a capacity gate before `acquirejob` (not a durable queue — GitHub is the queue). |
| <a id="Q82"></a>Q82 | Per-cluster proxy HPA-max guard (admission webhook + quota) | `infra` `security` | 🔲 | L | From Q34/E8. Proxy HPA `maxReplicas` allows up to 100/tenant, no per-cluster guard. Add a validating webhook correlating it with the namespace ResourceQuota — chosen over a lower CRD max (would reject existing tenants on re-apply). |
| <a id="Q45"></a>Q45 | Compress Progress table — drop Notes column | `docs` | 🔲 | S | Most cells just say "see plan" or restate the plan doc; the plan link in the row's name already carries the detail. Reduces edit surface and width. |
| <a id="Q52"></a>Q52 | Markdown link + anchor check CI gate | `docs` `infra` `tests` | 🔲 | S | Add GitHub-slug-aware markdown link/anchor checker to `unit-test.yml`. The L2 validation script in [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) is a working reference. |
| <a id="Q68"></a>Q68 | Enforce single Go version across all workspace files | `infra` `tests` | 🔲 | S | CLAUDE.md's "all go modules use the same Go version" rule is unenforced; the 2 `go.work.gen` files drifted to 1.26/1.26.0, breaking `make manifests`. Add a CI check that the `go` directive matches across go.work, all go.mod, and go.work.gen. |
| <a id="Q94"></a>Q94 | Normalize go.sum tidiness + gate drift in CI | `infra` `tests` | 🔲 | S | Committed go.sum drifts from `scripts/go-work-tidy.sh` output: the documented tidy+vendor flow re-adds `/go.mod` hashes to broker/probe/proxy/githubapp, so contributors revert spurious diffs. Normalize once, gate drift in CI. Sibling of [Q68](#Q68). |
| <a id="Q73"></a>Q73 | Sync GMC's bundled RunnerGroup CRD with the AGC source | `infra` `bug` | 🔲 | S | GMC's bundled RunnerGroup CRD (`…runnergroups.yaml`) drifted from AGC source: missing fields + different PodTemplateSpec (k8s.io/api skew), risking silent field pruning on deploy. Add a sync target + drift CI check. Overlaps [Q68](#Q68). |
| <a id="Q75"></a>Q75 | Exercise GMC validating webhook in envtest (not just e2e) | `tests` `infra` | 🔲 | M | GMC webhook checks (gitHubAppRef, privileged-container, profile downgrade) are tested only via direct calls + e2e. Wire envtest `WebhookInstallOptions` to catch admission-through-apiserver at integration tier (mind `failurePolicy=fail` readiness). |
| <a id="Q51"></a>Q51 | Reconcile documented vs emitted Prometheus metrics | `infra` `docs` `bug` | 🔲 | M | 6 documented metrics never registered in code (headline `pod_creation_latency_seconds` + 5 others). Per-metric decision: implement, re-point, or mark `(planned)`. See [docs-six-layer-audit.md](plan/docs-six-layer-audit.md) Layer 3. |
| <a id="Q72"></a>Q72 | Per-tenant metrics scrape wiring (Services + ServiceMonitors) | `infra` `tests` | 🔲 | M | Q69 shipped mTLS `/metrics` on :8443 for proxy+AGC, but nothing scrapes it: the proxy Service exposes only :8080 and the AGC has no Service. Add metrics-port Services + ServiceMonitors presenting the metrics-client bundle. Overlaps [Q35](#Q35). |
| <a id="Q87"></a>Q87 | Hot-path INFO→DEBUG + log correlation fields (listener/multiplexer) | `infra` | 🔲 | M | Logging-audit Themes D+F: demote per-session/job INFO to DEBUG in listener/provisioner (dominates volume at scale); add namespace/group/sessionId/podName correlation to listener+multiplexer contexts. See [logging-audit](plan/logging-audit.md). |
| <a id="Q88"></a>Q88 | Debugging blind-spot logs (podwaiter, mux restart, GMC steps, webhook audit) | `infra` | 🔲 | S | Logging-audit Theme E: add debug logs where stuck paths are silent: podwaiter (silent; top stuck cause), multiplexer restart/backoff, GMC reconcileResources steps, webhook-rejection audit, cert renewal. See [logging-audit](plan/logging-audit.md). |
| <a id="Q89"></a>Q89 | Per-tenant `spec.logLevel` CRD knob | `infra` | 🔲 | M | Logging-audit Theme G (post-1.0, after F1): add `spec.logLevel` (info\|debug) to ActionsGateway, threaded to AGC+proxy like `securityProfile` (rolling restart). Needs CRD+operator docs. See [logging-audit](plan/logging-audit.md). |
| <a id="Q55"></a>Q55 | Verify provisioner-test goleak cascade fix held in CI | `tests` `bug` | 🔲 | S | Intermittent ~20-test goleak cascade in `internal/provisioner` fixed by `waitForPodCreated` helper in 59c0714; delete row once CI is clean. If flakes recur, migrate remaining ~18 Eventually-on-Pod sites to the helper. |
| <a id="Q60"></a>Q60 | [Competitive analysis — GAG vs ARC-adjacent runner/queue tooling](design/appendix-d-alternatives-considered.md) | `docs` | 🔲 | M | Competitive analysis vs ARC-adjacent tooling: Kueue, Exostellar (verify the Kueue-under-ARC GPU pattern), KEDA. Expands [appendix-d](design/appendix-d-alternatives-considered.md). Narrow Kueue-vs-admission angle is in [Q59](#Q59). |
| <a id="Q62"></a>Q62 | Short per-attempt timeout on IP-range `/meta` fetch | `infra` `speed` | 🔲 | S | GMC HTTP client's 60s timeout is shared; a stalled `/meta` fetch burns 60s before the Q61 backoff retries. Add a ~10s per-attempt `context.WithTimeout` in `HTTPGitHubIPRangeFetcher.FetchIPRanges`. Follow-on to Q61. |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🔲 | S | Verified 2026-06-01: not deletable. Operator-doc for the `--agent-key-type=ed25519` opt-in; RSA-3072 stays the default regardless. Needs probe flag extensions + manual run with real credentials. Low priority: not a 1.0-gate. |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q74"></a>Q74 | [CRD conversion-webhook scaffolding (audit D7)](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | S | Graduating the API from `v1alpha1` to `v1beta1` (need `Hub`/`Convertible` stubs). |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
