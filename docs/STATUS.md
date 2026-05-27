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

Last refreshed: 2026-05-27 (named-pipe ✅, GithubRegistrar ✅, eviction retry CRD fields ✅, M2 envtest goroutine-leak suite ✅, credential rotation ✅, M3 metric assertions ✅, M4 test gaps ✅, open docs items ✅, AGC rename ✅, go-workspace prefix-match ✅ — workaround already removed in 6c23b0d, Make UX Phase 2 ✅, e2e test speed ✅, envtest/unit test split ✅, M2 kind activeSessions check ✅ — runner registration via generate-jitconfig working end-to-end, ARC alignment ✅; M3/M4 live-cluster dry-run via PR #59 surfaced 5 real bugs since fixed — workload NP `ipBlock` → `podSelector`, proxy HTTP/2 disable, IP-range fetcher `actions`→`api+actions+web`, AGC TLS pool replace → append, wrapper `--startuptype workerprocess` → `spawnclient` — and 7 new Queue items 5a–5g covering the two outstanding gaps and five regression-test additions that would have caught the bugs locally).

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
| 5a | Wrapper materializes JIT config files | `milestone` `bug` | 🔲 | M | AGC stores `encoded_jit_config` in agent Secret → provisioner copies into worker Secret → wrapper decodes base64 JSON map and writes `.runner`/`.credentials`/`.credentials_rsaparams` to `/home/runner/` before exec. Without these, `Runner.Worker` crashes with `ArgumentNullException: configuredSettings`. Surfaced during M3/M4 kind validation. |
| 5b | NetworkPolicy additivity broken under kindnet | `bug` `infra` | 🔲 | S | When the AGC pod is selected by both `actions-gateway-controller` (allows 443) and `actions-gateway-workload` (allows 53 + 8080-to-proxy) NPs, kindnet drops the 443 egress that the controller NP alone would allow. Reproduces by simply having both NPs apply; removing the workload NP restores k8s API access from the AGC. Likely fix: stop putting the workload label on the AGC pod and add an AGC-specific peer to the proxy ingress NP, or drop the workload NP for AGC entirely. |
| 5c | Implement `E2E_GMC_TenantProvisioning_ProxyConnectWorks` | `tests` `infra` | 🔲 | M | The single highest-leverage missing test. Already designed in [plan/e2e-tests.md §5.2](plan/e2e-tests.md) (Tier A) but never implemented. Deploy a workload-labeled `curl` pod, set `HTTPS_PROXY=https://actions-gateway-proxy.<ns>:8080`, target a public HTTPS URL (e.g. `https://api.github.com/zen`), and assert 200. This exercises the real kindnet NetworkPolicy enforcement, the proxy's TLS+CONNECT path, the IP-range allowlist on the proxy egress NP, and the AGC↔proxy CA chain in one pass. Would have caught 4 of the 5 bugs found in PR #59 (workload NP `ipBlock`, proxy HTTP/2, IP fetcher missing `api`, AGC TLS pool replace). |
| 5d | Unit test: proxy serves HTTP/1.1-only on TLS listener | `tests` | 🔲 | S | Add `TestProxy_TLS_RejectsHTTP2_ALPN` to `cmd/proxy/proxy_test.go`. Drive an `httptest`-style TLS listener through the production `ListenAndServe`, then connect with a `tls.Dial` configured with `NextProtos: ["h2", "http/1.1"]` and assert the server selects `http/1.1`. Also issue a `CONNECT` over that handshake and assert the response is `HTTP/1.1 200 Connection established`. Guards against the regression fixed in PR #59 (`fix(proxy): disable HTTP/2 on the TLS CONNECT listener`). |
| 5e | Unit test: IP fetcher merges `api`+`actions`+`web` from real `/meta` shape | `tests` | 🔲 | S | Existing `TestHTTPFetcher_*` fixtures feed only `{"actions": [...]}`. Capture a real `https://api.github.com/meta` response (or hand-build one with all three fields populated by distinct CIDRs) and add `TestHTTPFetcher_MergesAllRanges` that asserts the returned slice contains a CIDR from each of `api`, `actions`, `web`. Guards against the regression fixed in PR #59 (`fix(gmc): expand proxy egress to api + actions + web GitHub ranges`). |
| 5f | Refactor + unit test: AGC proxy CA TLS pool composition | `tests` `security` | 🔲 | S | Extract the TLS-pool construction from `cmd/agc/main.go` into an exported helper (e.g. `internal/transport.BuildProxyTrustPool(certPEM []byte) (*x509.CertPool, error)`). Add unit tests: (a) pool validates a leaf signed by a system root, (b) pool validates a leaf signed by the supplied proxy CA, (c) pool rejects a leaf signed by an unrelated CA, (d) missing PEM returns nil pool without error. Guards against the regression fixed in PR #59 (`fix(agc): append proxy CA to system pool instead of replacing it`). |
| 5g | Unit test: wrapper invokes Runner.Worker with `[spawnclient, 3, 4]` | `tests` | 🔲 | S | The existing `cmd/worker/worker_test.go` covers payload reading and the message-framing helpers in isolation but never exercises the subprocess invocation. Add `TestWrapper_InvokesRunnerWorker_WithSpawnclientArgs`: build a tiny stub Runner.Worker binary in a `t.TempDir()` (Go test helper or shell script) that records `os.Args` to a file, prepend the dir to `PATH`, run the wrapper, then assert the recorded args are exactly `["spawnclient", "3", "4"]`. Guards against the regression fixed in PR #59 (`fix(worker): invoke Runner.Worker with spawnclient, not --startuptype`) and is also the natural place to extend coverage once 5a lands (assert `.runner`/`.credentials`/`.credentials_rsaparams` exist before exec). |
| 6 | [M3/M4 kind end-to-end validation](plan/milestone-3.md) | `milestone` | 🚫 | M | → 5a + 5b. With #59 fixes shipped, the test reaches worker-pod creation and Runner.Worker parses the job; remaining blockers are the JIT config gap (5a) and NP additivity (5b). |
| 7 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🚫 | S | → M3/M4 kind end-to-end |
| 8 | [M2-tests remaining unit gaps (3–11)](plan/milestone-2-tests.md) | `milestone` `tests` | 🚫 | M | → M2 envtest suite |
| 9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🚫 | M | → M3 metric assertions |
| 11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🚫 | S | → M3/M4 kind end-to-end (needs live kind cluster) |
| 12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🚫 | L | → M3/M4 kind end-to-end |
| 13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | → M5 packaging |
| 14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | → M5 packaging |
| 15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| 17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| 18 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| 19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
