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

Last touched: 2026-06-18

---

## Progress

Plan-level view. ✅ = all criteria met. ⚠️ = code shipped, specific pieces remain open in the Queue below.

| Item | Labels | Status |
|---|---|---|
| [M1: Wire-protocol probe](plan/milestone-1.md) | `milestone` | ✅ |
| [M1: Unit-test coverage](plan/milestone-1-tests.md) | `milestone` `tests` | ✅ |
| [M2: AGC controller](plan/milestone-2.md) | `milestone` | ✅ |
| [M3: Worker pod](plan/milestone-3.md) | `milestone` | ✅ |
| [M4: GMC + proxy](plan/milestone-4.md) | `milestone` | ✅ |
| [M5: Hardening](plan/milestone-5.md) | `milestone` `security` | ⚠️ |
| [Release 1.0](plan/release-1.0.md) | `milestone` | ✅ |
| [Security hardening](plan/security.md) | `security` | ⚠️ |
| [Security audit 2 (2026-06)](plan/security-audit-2026-06.md) | `security` | ⚠️ |
| [Worker egress proxy](plan/worker-egress-proxy.md) | `security` `infra` | ✅ |
| [Docs](plan/docs.md) | `docs` | ✅ |
| [Six-layer docs audit](plan/docs-six-layer-audit.md) | `docs` | ✅ |
| [Make UX](plan/make.md) | `infra` | ✅ |
| [Docker image speed](plan/docker-image-speed.md) | `speed` | ✅ |
| [e2e test speed](plan/e2e-tests-speed.md) | `speed` `tests` | ✅ |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears. Intentionally parked items live in [Deferred](#deferred) below, out of the priority ordering.

| ID | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| <a id="Q135"></a>Q135 | Flake: e2e E2E_AGC_MultipleJobsQueued — 4min timeout waiting for 2nd worker pod | `tests` `bug` | 🔲 | S | Root cause fixed: recycle-401 was a fakegithub gap — single-use 401 not owner-scoped, so a non-single-use tenant's recycled agentID collided with an in-scope consumed one. Owner-scoped in PR (+Q137 revival). Open until e2e confirms green. |
| <a id="Q139"></a>Q139 | Flake: e2e E2E_GMC_TenantProvisioning_ProxyConnectWorks — curl through proxy gets HTTP 504 | `tests` `bug` `infra` | 🔲 | M | provisioning_test.go:282: curl CONNECT through the per-tenant proxy returned 504 on PR 231 e2e; not recurred since (watch-only). Suite still multi-flaky — Q134/Q135 both recurred on main 2026-06-15. Proxy tunnel timeout under CI load. |
| <a id="Q83"></a>Q83 | Tier-A verify GMC manager NetworkPolicy enforcement | `security` `infra` `tests` | 🔲 | S | Q34/E5 enabled the manager NP by default (metrics :8443 limited to `metrics: enabled`; webhook 9443 open-source). Tier-A verify runtime: scrape denied unlabeled / allowed labeled, admission still works. Kindnet caveat: use `KIND_CNI=calico`. |
| <a id="Q119"></a>Q119 | CI leg for the Calico egress-negative e2e profile | `tests` `infra` | 🔲 | S | Q7b ran locally on `make e2e-cluster KIND_CNI=calico` (Calico v3.31.5): both runtime egress negatives observed enforcing. Add a CI job (or scheduled lane) on that profile running the negatives + ProxyConnectWorks. |
| <a id="Q13"></a>Q13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🔲 | L | Unblocked 2026-06-12 (Q12 done). **Highest "right thing" risk — pitch is thousands of virtual sessions per AGC and nothing pins that claim.** Single-use JIT agents (Q114, fixed) cost one re-registration per job; the harness must model it. |
| <a id="Q15"></a>Q15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| <a id="Q59"></a>Q59 | [Pre-acquisition admission control (capacity-gated `acquirejob`)](plan/acquire-admission-control.md) | `infra` `speed` | 🔲 | L | AGC acquires jobs before checking pod capacity, so ceiling-held jobs are claimed-then-dropped under pressure. Add a capacity gate before `acquirejob` (not a durable queue — GitHub is the queue). |
| <a id="Q82"></a>Q82 | Per-cluster proxy HPA-max guard (admission webhook + quota) | `infra` `security` | 🔲 | L | From Q34/E8. Proxy HPA `maxReplicas` allows up to 100/tenant, no per-cluster guard. Add a validating webhook correlating it with the namespace ResourceQuota — chosen over a lower CRD max (would reject existing tenants on re-apply). |
| <a id="Q146"></a>Q146 | Refuse non-HTTPS GITHUB_API_BASE_URL outside dev mode | `security` | 🔲 | S | Q127 item 8 carve-out: reject non-HTTPS `GITHUB_API_BASE_URL` (`githubapp/auth.go`). Low value — prod blocks the env via `--allow-agc-extra-env` (default-off); a clean fix needs a dev escape plumbed through the e2e fakegithub `http` svc-DNS URL. |
| <a id="Q55"></a>Q55 | Verify provisioner-test goleak cascade fix held in CI | `tests` `bug` | 🔲 | S | Intermittent ~20-test goleak cascade in `internal/provisioner` fixed by `waitForPodCreated` helper in 59c0714; delete row once CI is clean. If flakes recur, migrate remaining ~18 Eventually-on-Pod sites to the helper. |
| <a id="Q60"></a>Q60 | [Competitive analysis — GAG vs ARC-adjacent runner/queue tooling](design/appendix-d-alternatives-considered.md) | `docs` | 🔲 | M | vs ARC-adjacent tooling (Kueue, Exostellar, KEDA); expands [appendix-d](design/appendix-d-alternatives-considered.md). Per-benefit notes + verify-list in [competitive-analysis](plan/competitive-analysis.md). Kueue-vs-admission angle in [Q59](#Q59). |
| <a id="Q11"></a>Q11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🔲 | S | Verified 2026-06-01: not deletable. Operator-doc for the `--agent-key-type=ed25519` opt-in; RSA-3072 stays the default regardless. Needs probe flag extensions + manual run with real credentials. Low priority: not a 1.0-gate. |
| <a id="Q147"></a>Q147 | Align grandfathered label/annotation values to no-boolean convention | `infra` `docs` | 🔲 | M | Align grandfathered `tenant`/`allow-profile-downgrade` `"true"` values to the no-boolean [convention](development/kubernetes-conventions.md); breaking (VAPs, onboarding, live namespaces) — needs a dual-read migration. Low priority. |

---

## Deferred

Intentionally parked items. These carry **no priority position** and are **not** picked from the top of the Queue — each waits on an explicit trigger before it returns to active work. Keeping them out of the Queue stops them from diluting the priority ordering. When an item's trigger fires, move its row back into the Queue at the position it then deserves (see [prioritize new items on entry](development/maintaining-backlog.md#prioritize-new-items-on-entry)).

| ID | Item | Labels | Sz | Trigger to revive |
|---|---|---|---|---|
| <a id="Q74"></a>Q74 | [CRD conversion-webhook scaffolding (audit D7)](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | S | Graduating the API from `v1alpha1` to `v1beta1` (need `Hub`/`Convertible` stubs). |
| <a id="Q147"></a>Q147 | [v2 API decomposition: split into ActionsGateway + RunnerSet + RunnerTemplate + EgressProxy](design/appendix-h-v2-api-decomposition.md) | `infra` `security` | L | Pod templates (DinD/sysbox) approach the etcd object-size limit, or tenants ask for multiple gateways per namespace / shared egress proxies. Absorbs Q144 (proxy optionality) and Q74 (conversion → tool-assisted migration). Sign-offs in §H.13. |
| <a id="Q17"></a>Q17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | M | CI latency becomes the bottleneck. |
| <a id="Q18"></a>Q18 | [alerting.md](plan/docs.md) | `docs` | M | A real Prometheus/Alertmanager setup exists to document against. |
| <a id="Q19"></a>Q19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | L | A named trigger fires — these are explicit non-commitments (see [Appendix G](design/appendix-g-future-enhancements.md)). |
| <a id="Q144"></a>Q144 | [Optional (disable-able) egress proxy](design/appendix-g-future-enhancements.md) | `infra` `security` | M | Operator ask for a single-tenant/dev/cost-sensitive deployment, or one already attributing egress per-tenant at the node/cloud layer. Opt-out only — default stays Required; forfeits per-tenant egress-IP attribution + containment (G.8). |
| <a id="Q70"></a>Q70 | Flip worker-image trivy leg to blocking | `security` `infra` | S | Upstream `actions-runner` base scans clean (or near-clean). Worker leg is report-only in `security-scan.yml` because the base carries ~36 upstream HIGH/CRITICAL CVEs; the dependabot `docker` ecosystem auto-bumps it. When a bump clears them, set the worker leg's `exit-code` to `1`. |
