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

Last refreshed: 2026-05-30 (5h ✅ — worker proxy-CA trust install shipped. AGC `Provisioner.ProxyTLSSecretName` projects `actions-gateway-proxy-tls` (cert only, via `Items:[tls.crt]`) into each worker pod at `/etc/actions-gateway/proxy-ca/tls.crt`; GMC plumbs `PROXY_TLS_SECRET_NAME` on the AGC Deployment so each tenant's AGC finds the right Secret automatically; worker entrypoint wrapper concatenates the system bundle with the proxy CA and exports `SSL_CERT_FILE` on the child Runner.Worker env before exec. Five new unit tests + an end-to-end stub-Runner.Worker test cover the helper paths and assert `SSL_CERT_FILE` reaches the child. Docs updated across `02-architecture.md`, `05-security.md`, `troubleshooting.md`, `plan/milestone-3.md` §11.C. Queue item 6 (M3/M4 kind e2e re-run) is now unblocked and is the next critical-path action. Earlier 2026-05-29: item 6 live dry-run surfaced 5h as the blocker — runner exited 1 with `UntrustedRoot` on every outbound HTTPS through the egress proxy. Earlier 5j ✅ — intermittent e2e hang instrumented ahead of next occurrence. `_GINKGO_RUN` in the root Makefile lowered `--poll-progress-after 60s` → `30s` so the ginkgo per-node goroutine dump fires inside the 45 min job timeout window; `e2e-test.yml` "Collect diagnostic info" step gained a per-tenant-namespace `kubectl get networkpolicy -o yaml` dump so the next hang reveals whether `IPRangeReconciler` ever populated the `actions-gateway-proxy` NetworkPolicy's ipBlock peers. Earlier Queue #8 ✅ — verified all 9 M2 unit-test gaps (3–11) from `docs/plan/milestone-2-tests.md` were already shipped across prior sessions; tests pass per-name across `cmd/agc/...`. Plan doc updated with a per-gap landing-point table. No code change needed — this is a Queue cleanup, the kind of silent-completion the CLAUDE.md "verify blockers are real" note warns about. Earlier 5g ✅ — `TestWrapper_InvokesRunnerWorker_WithSpawnclientArgs` added to `cmd/worker/worker_test.go`. Spins up a stub `Runner.Worker` shell script in a fresh tempdir, prepends it to `PATH`, calls `run()`, and asserts the recorded `argc + argv` is exactly `[3, spawnclient, 3, 4]`. Verified to fail with `actual: [4, --startuptype, workerprocess, 3, 4]` when the wrapper is reverted to the buggy `--startuptype workerprocess` invocation — i.e. catches the exact PR #59 regression. Earlier landmarks: 5f AGC proxy CA TLS pool helper + tests ✅, 5e IP fetcher merge regression test ✅, 5d TLS ALPN HTTP/1.1-only ✅, 5c Tier-A `ProxyConnectWorks` ✅, named-pipe ✅, GithubRegistrar ✅, eviction retry CRD fields ✅, M2 envtest goroutine-leak suite ✅, credential rotation ✅, M3 metric assertions ✅, M4 test gaps ✅, open docs items ✅, AGC rename ✅, go-workspace prefix-match ✅, Make UX Phase 2 ✅, e2e test speed ✅, envtest/unit test split ✅, M2 kind activeSessions check ✅, ARC alignment ✅, JIT config plumbing for worker ✅. PR #59 fixes shipped: workload NP `ipBlock` → `podSelector`, proxy HTTP/2 disable, IP-range fetcher `actions`→`api+actions+web`, AGC TLS pool replace → append, wrapper `--startuptype workerprocess` → `spawnclient`.

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
| 6 | [M3/M4 kind end-to-end validation](plan/milestone-3.md) | `milestone` | 🔲 | M | **Unblocked by 5h on 2026-05-30** (worker proxy-CA trust shipped — provisioner mount + wrapper SSL_CERT_FILE install). 2026-05-29 dry-run via `E2E_GitHub_RealDispatch` (Tier C `Label("github-real")`) on fresh kind cluster + real GitHub App `actions-gateway-test`, target repo `actions-gateway/gateway-test` workflow `test-job.yml`: payload + JIT config delivered correctly, Runner.Worker received the job message and parsed it (`Message received` / `Job message: …`), then all outbound HTTPS calls failed with `UntrustedRoot` because the worker pod had `HTTPS_PROXY=https://actions-gateway-proxy:8080` but no proxy-CA mount — runner exited 1, workflow concluded `cancelled`. Re-run on the same kind cluster + repo; expectation is the runner can now post step logs + completion and the workflow goes green. |
| 7 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🚫 | S | → M3/M4 kind end-to-end |
| 20 | [Proxy server + relay timeouts (M-17/M-18)](plan/security.md) | `security` `bug` | 🔲 | S | High + Medium DoS. Add `ReadHeaderTimeout`/`IdleTimeout` to proxy + health servers; per-conn idle + tunnel-lifetime deadline in `handleConnect`. Independent, ~30 LoC + tests. |
| 21 | [Pin worker Dockerfile base image digest (M-19)](plan/security.md) | `security` `infra` | 🔲 | S | Resolve `ghcr.io/actions/actions-runner:2.327.1` to `@sha256:…`; tie the digest update to the runner-version bump procedure. |
| 9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining items for preventing churn: **H2** (rerun-API 5xx is non-fatal — no test pins that contract), **H3** (decryption-failure fallback path is untested — silent payload corruption could ship undetected), **M3** (`activePodCount` Pending-pod branch — ceiling enforcement edge case). Worth picking up after 5c–5g. |
| 22 | [Repo hygiene: SECURITY.md + dependabot config](plan/security.md) | `security` `docs` | 🔲 | S | Disclosure policy + automated dep updates across 7 go.mod files. |
| 23 | [CI security scanning (govulncheck + trivy)](plan/security.md) | `security` `infra` | 🔲 | M | Per-module workspace-aware `govulncheck`; `trivy image` against each built Dockerfile in PR CI. |
| 24 | [Enforce `@sha256:` syntax on AGC_IMAGE/PROXY_IMAGE at GMC startup](plan/security.md) | `security` | 🔲 | S | Reject non-digest references; promoted from the security plan's "out of scope but worth noting" note. |
| 25 | [Restrict `:8081` health/metrics ingress (L-8)](plan/security.md) | `security` | 🔲 | S | Explicit NP ingress rule on proxy + AGC permitting only kubelet probe + Prometheus scrape selector. |
| 26 | [Remove over-declared `watch` verb on AGC Role](plan/security.md) | `security` | 🔲 | S | One-line cleanup; no Secret informer is registered. H-2 residual notes it. |
| 27 | [Security operations runbook](plan/security.md) | `security` `docs` | 🔲 | S | Convert abuse heuristics from `05-security.md` into operator alerts (Secret list rate, eviction retries exhausted, etc). |
| 11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🚫 | S | → M3/M4 kind end-to-end (needs live kind cluster) |
| 12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🚫 | L | → M3/M4 kind end-to-end |
| 28 | [SBOM + cosign signing of built images](plan/security.md) | `security` `infra` | 🚫 | M | → M5 packaging. Distroless + digest pinning are the foundation. |
| 29 | [API server audit policy sample](plan/security.md) | `security` `infra` | 🚫 | S | → M5 packaging. Surfaces a compromised GMC's Secret `get` calls. |
| 13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | → M5 packaging |
| 14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | → M5 packaging |
| 15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| 17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| 18 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| 19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
