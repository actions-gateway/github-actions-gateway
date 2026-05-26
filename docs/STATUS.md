# Project Status

Single source of truth for progress and priorities across the full project. `docs/plan/` holds the implementation detail; this file holds the ordering and the overview.

## Conventions

**Status:** ✅ done · ⚠️ partial (code shipped, pieces remain) · 🔲 ready · 🚫 blocked · 💤 deferred  
**Size:** S = one session · M = 2–3 sessions · L = needs a plan doc in `docs/plan/`  
**Labels:** `milestone` `security` `tests` `speed` `docs` `infra` `bug`

**Maintaining this file:**
- **Starting an S item:** complete it, delete the row.
- **Starting an M/L item:** mark it **▶ Started** here; create or update a plan doc in `docs/plan/`; delete the row here when done.
- **New item identified:** insert it in the Queue at the right priority position.
- **⚠️ item fully done:** move it to the Progress table as ✅.

Last refreshed: 2026-05-25.

---

## Progress

Plan-level view. ✅ = all criteria met. ⚠️ = code shipped, specific pieces remain open in the Queue below.

| Item | Labels | Status | Notes |
|---|---|---|---|
| M1: Wire-protocol probe | `milestone` | ✅ | [plan](plan/milestone-1.md) |
| M1: Unit-test coverage | `milestone` `tests` | ✅ | All 5 gaps closed — [plan](plan/milestone-1-tests.md) |
| M2: AGC controller | `milestone` | ⚠️ | Code shipped; envtest suite → Q#6, kind check → Q#13 — [plan](plan/milestone-2.md) |
| M3: Worker pod | `milestone` | ⚠️ | Code complete; end-to-end gated on Named Pipe investigation → Q#3 — [plan](plan/milestone-3.md) |
| M4: GMC + proxy | `milestone` | ⚠️ | Code complete; multi-tenant kind validation blocked on M3 → Q#14 — [plan](plan/milestone-4.md) |
| M5: Hardening | `milestone` `security` | ⚠️ | Security half done; packaging → Q#19, load test → Q#20, posture scan → Q#21 — [plan](plan/milestone-5.md) |
| Security hardening | `security` | ⚠️ | W2–W8/M-12/13/L-2/3/7 shipped; M-11b + live kind validation remain — [plan](plan/security.md) |
| Worker egress proxy | `security` `infra` | ⚠️ | NetworkPolicy split shipped; live `curl` validation blocked on M3/M4 → Q#15 — [plan](plan/worker-egress-proxy.md) |
| Docs | `docs` | ⚠️ | Phase 1 done; 4 items open → Q#11 — [plan](plan/docs.md) |
| Make UX | `infra` | ⚠️ | Phase 1 done; Phase 2 drift items open — [plan](plan/make.md) |
| Docker image speed | `speed` | ⚠️ | §1/2/4/5 done; §7/8/9/12 open — [plan](plan/docker-image-speed.md) |
| e2e test speed | `speed` `tests` | ⚠️ | §2/3 done; §1/4/5 open — [plan](plan/e2e-tests-speed.md) |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears.

| # | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| 1 | [buildNoProxy merge bug](plan/milestone-4-tests.md) | `bug` `milestone` `tests` | 🔲 | S | Custom `noProxyCIDRs` silently drops cluster-internal exclusions — AGC malfunctions |
| 2 | [proxy.resources per-key merge](plan/gaps.md) | `bug` `milestone` | 🔲 | S | Partial override silently drops `requests.cpu`; HPA reports `<unknown>` |
| 3 | [Named Pipe investigation (M3 §5.A)](plan/milestone-3.md) | `milestone` | 🔲 | M | Critical path — unblocks M3/M4 end-to-end (#14) and everything downstream |
| 4 | [Wire live GithubRegistrar in main.go](plan/milestone-2.md) | `milestone` | 🔲 | S | StubRegistrar still wired in production binary |
| 5 | [Expose maxEvictionRetries / evictionRetryDelay on CRD](plan/gaps.md) | `milestone` | 🔲 | S | Fields hardcoded; GPU operators can't disable auto-retry |
| 6 | [M2 envtest goroutine-leak integration suite](plan/milestone-2.md) | `milestone` `tests` | 🔲 | M | Last two unchecked M2 success criteria; 7 scenarios from §7.2 |
| 7 | [Credential rotation: Secret watch + CredentialUnavailable](plan/gaps.md) | `milestone` `security` | 🔲 | M | Silent failure when referenced Secret is deleted mid-operation |
| 8 | [M3 metric assertions + dead PodCreationLatency](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | S | Metrics untested; `PodCreationLatency` declared but never emitted |
| 9 | [M4 remaining test gaps](plan/milestone-4-tests.md) | `milestone` `tests` | 🔲 | S | IPRange edge cases, webhook IP-range test, HPA/PDB coverage |
| 10 | [Open docs items: HPA callout, DefaultWorkerImage, capacity examples](plan/docs.md) | `docs` | 🔲 | S | Items 2.7, 2.6, 2.3 from docs plan |
| 11 | [Rename actions-gateway-agc → actions-gateway-controller](plan/rename-agc-to-controller.md) | `infra` `milestone` | 🔲 | M | Code/docs mismatch since M4; 5 constants, all tests, ops docs |
| 12 | [Go workspace prefix-match bug investigation](development/go-workspaces.md) | `infra` | 🔲 | S | Check if Go 1.22–1.24 fixed it; drop `replace` workaround if so |
| 13 | [M2 kind: live activeSessions==1 check](plan/milestone-2.md) | `milestone` `tests` | 🚫 | S | → #6 |
| 14 | [M3/M4 kind end-to-end validation](plan/milestone-3.md) | `milestone` | 🚫 | M | → #3 |
| 15 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🚫 | S | → #14 |
| 16 | [M2-tests remaining unit gaps (3–11)](plan/milestone-2-tests.md) | `milestone` `tests` | 🚫 | M | → #6 |
| 17 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🚫 | M | → #8 |
| 18 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🚫 | S | → #14 (needs live kind cluster) |
| 19 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🚫 | L | → #14 |
| 20 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | → #19 |
| 21 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | → #19 |
| 22 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| 23 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| 24 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| 25 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
