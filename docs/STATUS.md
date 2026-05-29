# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · ▶ started · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = needs a plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug`

**Maintaining this file:**
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** mark it **▶ Started** here; create or update a plan doc in `docs/plan/`; delete the row here when done.
- **New item identified:** insert it in the Queue at the right priority position.
- **⚠️ item fully done:** move it to the Progress table as ✅.

Last refreshed: 2026-05-29 (5j ✅ — intermittent e2e hang instrumented ahead of next occurrence. (1) `_GINKGO_RUN` in the root Makefile lowered `--poll-progress-after 60s` → `30s` so the ginkgo per-node goroutine dump fires inside the 45 min job timeout window with margin to spare on the next hang (the 2026-05-28 PR #68 hang [run 26614404029](https://github.com/actions-gateway/github-actions-gateway/actions/runs/26614404029) ran ~15 min before being cancelled, well past the previous 60s threshold but it produced no progress report visible in the surfaced logs). (2) `e2e-test.yml` "Collect diagnostic info" step gained a per-tenant-namespace `kubectl get networkpolicy -o yaml` dump so the next hang reveals whether `IPRangeReconciler` ever populated the `actions-gateway-proxy` NetworkPolicy's ipBlock peers — the leading hypothesis is that the spec stalls on a GitHub `api.github.com/meta` fetch or on the CONNECT to api.github.com, and an empty/missing ipBlock list points squarely at the IPRangeReconciler. The "consider verifying cancel actually runs the diagnostic step against a deliberately-hung spec" subtask was deferred — the next real hang will reveal it implicitly; a synthetic experiment isn't worth the runner-minutes until then. Earlier landmarks: Queue #8 M2 unit-test gaps (3–11) verified pre-shipped ✅, 5g wrapper `spawnclient,3,4` invocation regression test ✅, 5f AGC proxy CA TLS pool helper ✅, 5e IP fetcher merge regression test ✅, 5d ALPN HTTP/1.1-only test ✅, 5c Tier-A `ProxyConnectWorks` ✅, named-pipe ✅, GithubRegistrar ✅, eviction retry CRD fields ✅, M2 envtest goroutine-leak suite ✅, credential rotation ✅, M3 metric assertions ✅, M4 test gaps ✅, open docs items ✅, AGC rename ✅, go-workspace prefix-match ✅, Make UX Phase 2 ✅, e2e test speed ✅, envtest/unit test split ✅, M2 kind activeSessions check ✅, ARC alignment ✅, JIT config plumbing for worker ✅. PR #59 fixes shipped: workload NP `ipBlock` → `podSelector`, proxy HTTP/2 disable, IP-range fetcher `actions`→`api+actions+web`, AGC TLS pool replace → append, wrapper `--startuptype workerprocess` → `spawnclient`.

---

## Progress

Plan-level view. ✅ = all criteria met. ⚠️ = code shipped, specific pieces remain open in the Queue below.

| Item | Labels | Status | Notes |
|---|---|---|---|
| M1: Wire-protocol probe | `milestone` | ✅ | [plan](plan/milestone-1.md) |
| M1: Unit-test coverage | `milestone` `tests` | ✅ | All 5 gaps closed — [plan](plan/milestone-1-tests.md) |
| M2: AGC controller | `milestone` | ✅ | All criteria met including live kind check (`activeSessions==1`) — [plan](plan/milestone-2.md) |
| M3: Worker pod | `milestone` | ⚠️ | Code complete; end-to-end gated on Named Pipe investigation — [plan](plan/milestone-3.md) |
| M4: GMC + proxy | `milestone` | ⚠️ | Code + rename complete; multi-tenant kind validation blocked on M3 — [plan](plan/milestone-4.md) |
| M5: Hardening | `milestone` `security` | ⚠️ | Security half done; packaging, load test harness, posture scan open — [plan](plan/milestone-5.md) |
| Security hardening | `security` | ⚠️ | W2–W8/M-12/13/L-2/3/7 shipped; M-11b + live kind validation remain — [plan](plan/security.md) |
| Worker egress proxy | `security` `infra` | ⚠️ | NetworkPolicy split shipped; live `curl` validation blocked on M3/M4 — [plan](plan/worker-egress-proxy.md) |
| Docs | `docs` | ✅ | All Phase 1–3 items done; alerting.md deferred — [plan](plan/docs.md) |
| Make UX | `infra` | ✅ | Phase 1 + Phase 2 done — [plan](plan/make.md) |
| Docker image speed | `speed` | ✅ | All items done or explicitly closed — [plan](plan/docker-image-speed.md) |
| e2e test speed | `speed` `tests` | ✅ | All items done — [plan](plan/e2e-tests-speed.md) |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears.

| # | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| 6 | [M3/M4 kind end-to-end validation](plan/milestone-3.md) | `milestone` | 🔲 | M | **Unblocked.** With PR #59 fixes, JIT config plumbing (5a, 2026-05-27), and the AGC apiserver post-DNAT fix (5b) all shipped, the test reaches worker-pod creation, Runner.Worker parses the job, the runner config files are in place, and the AGC can reach the kube-apiserver under kindnet. Run the live kind dry-run end-to-end against real GitHub. |
| 7 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🚫 | S | → M3/M4 kind end-to-end |
| 9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining items for preventing churn: **H2** (rerun-API 5xx is non-fatal — no test pins that contract), **H3** (decryption-failure fallback path is untested — silent payload corruption could ship undetected), **M3** (`activePodCount` Pending-pod branch — ceiling enforcement edge case). Worth picking up after 5c–5g. |
| 11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🚫 | S | → M3/M4 kind end-to-end (needs live kind cluster) |
| 12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🚫 | L | → M3/M4 kind end-to-end |
| 13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | → M5 packaging |
| 14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | → M5 packaging |
| 15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| 17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| 18 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| 19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
