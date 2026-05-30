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

Last touched: 2026-05-30

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
| Make UX | `infra` | ✅ | Phase 1 + Phase 2 done — [plan](plan/make.md) |
| Docker image speed | `speed` | ✅ | All items done or explicitly closed — [plan](plan/docker-image-speed.md) |
| e2e test speed | `speed` `tests` | ✅ | All items done — [plan](plan/e2e-tests-speed.md) |

---

## Queue

Specific actionable items in priority order. Pick from the top; skip 🚫 items until their blocker clears.

| # | Item | Labels | St | Sz | Notes |
|---|---|---|---|---|---|
| 7 | [Egress proxy live curl validation](plan/worker-egress-proxy.md) | `security` `infra` | 🔲 | S | **Unblocked by item 6 on 2026-05-30.** Same kind cluster + real GitHub App available; need to assert workload→proxy CONNECT + DNAT + IP-range egress with `curl` from a workload-labeled debug pod. |
| 20 | [Proxy server + relay timeouts (M-17/M-18)](plan/security.md) | `security` `bug` | 🔲 | S | High + Medium DoS. Add `ReadHeaderTimeout`/`IdleTimeout` to proxy + health servers; per-conn idle + tunnel-lifetime deadline in `handleConnect`. Independent, ~30 LoC + tests. |
| 21 | [Pin worker Dockerfile base image digest (M-19)](plan/security.md) | `security` `infra` | 🔲 | S | Resolve `ghcr.io/actions/actions-runner:2.327.1` to `@sha256:…`; tie the digest update to the runner-version bump procedure. |
| 9 | [M3-tests remaining items (H2/M/L)](plan/milestone-3-tests.md) | `milestone` `tests` | 🔲 | M | **Unblocked** — M3 metric assertions (H1) landed. Highest-leverage remaining items for preventing churn: **H2** (rerun-API 5xx is non-fatal — no test pins that contract), **H3** (decryption-failure fallback path is untested — silent payload corruption could ship undetected), **M3** (`activePodCount` Pending-pod branch — ceiling enforcement edge case). Worth picking up after 5c–5g. |
| 22 | [Repo hygiene: SECURITY.md + dependabot config](plan/security.md) | `security` `docs` | 🔲 | S | Disclosure policy + automated dep updates across 7 go.mod files. |
| 23 | [CI security scanning (govulncheck + trivy)](plan/security.md) | `security` `infra` | 🔲 | M | Per-module workspace-aware `govulncheck`; `trivy image` against each built Dockerfile in PR CI. |
| 24 | [Enforce `@sha256:` syntax on AGC_IMAGE/PROXY_IMAGE at GMC startup](plan/security.md) | `security` | 🔲 | S | Reject non-digest references; promoted from the security plan's "out of scope but worth noting" note. |
| 25 | [Restrict `:8081` health/metrics ingress (L-8)](plan/security.md) | `security` | 🔲 | S | Explicit NP ingress rule on proxy + AGC permitting only kubelet probe + Prometheus scrape selector. |
| 26 | [Remove over-declared `watch` verb on AGC Role](plan/security.md) | `security` | 🔲 | S | One-line cleanup; no Secret informer is registered. H-2 residual notes it. Overlaps partially with k8s-audit §B B4 (#30). |
| 27 | [Security operations runbook](plan/security.md) | `security` `docs` | 🔲 | S | Convert abuse heuristics from `05-security.md` into operator alerts (Secret list rate, eviction retries exhausted, etc). |
| 30 | [K8s audit — §B RBAC & cluster-wide privilege](plan/k8s-best-practices.md#b-rbac--cluster-wide-privilege-) | `security` | 🔲 | S | 🔴 GMC ClusterRole grants `rbac:escalate` (privilege-escalation primitive); `namespaces:patch/update` cluster-wide (could relabel `kube-system` PSA); `applyNamespacePSA` silently overwrites admin edits; `secrets:list` returns full GitHub-App credential bodies. B4 (`secrets:list`/`watch`) overlaps with #26. Independent of M3/M4 e2e. |
| 31 | [K8s audit — §C Worker/proxy pod security defaults](plan/k8s-best-practices.md#c-worker--proxy-pod-security-defaults-) | `security` | 🔲 | S | 🔴 Worker pods get no default `SecurityContext` (`runAsNonRoot`, RO rootfs, cap drop, seccomp) — PSA `baseline` does not enforce; no default resource requests/limits → Best-Effort pods burn eviction-retry budget; no `RuntimeDefault` seccomp blocks PSA `restricted` upgrade. |
| 32 | [K8s audit — §A Controller correctness](plan/k8s-best-practices.md#a-controller-correctness-) | `bug` `infra` | 🔲 | M | 🔴 No `EventRecorder` anywhere (`kubectl describe ag/rg` is silent on credential/quota/eviction failures); RunnerGroup controller has no `Owns()`/watch on worker Pods → `ActiveSessions` stale on eviction; provisioner polls 5 s `r.Get` instead of watching (~200 gets/s at 1k sessions); finalizer race leaks provisioner pool maps. |
| 33 | [K8s audit — §D CRD design polish](plan/k8s-best-practices.md#d-crd-design-polish-) | `infra` | 🔲 | S | 🟡 Missing `+listType=map` on RunnerGroup conditions (SSA churn); no `omitempty` on numeric status; no `categories=actions-gateway`; no CEL immutability on `gitHubAppRef.name` / `securityProfile` (silent security downgrades); no `MinItems=1` on `RunnerLabels`. |
| 34 | [K8s audit — §E Manifest defaults & HA](plan/k8s-best-practices.md#e-manifest-defaults--ha-) | `infra` | 🔲 | M | 🟡 GMC `replicas: 1` + no PDB + no PriorityClass; no `startupProbe`; cert-manager metrics certs, ServiceMonitor, and NetworkPolicy all commented out in default kustomization (violates secure-by-default); proxy anti-affinity `Preferred` defeats PDB; missing `terminationGracePeriodSeconds` truncates CONNECT tunnels on rollout. |
| 35 | [K8s audit — §F Observability & operational](plan/k8s-best-practices.md#f-observability--operational-) | `infra` | 🔲 | M | 🟡 Two logger libs (`slog`+`zap`) emit incompatible JSON; no OpenTelemetry tracing; AGC has no `HealthProbeBindAddress` wired and the AGC Deployment carries no liveness/readiness probes — wedged AGC is invisible; AGC hard-codes `zap.UseDevMode(true)` in production. |
| 36 | [K8s audit — §G Supply chain (labels + build flags)](plan/k8s-best-practices.md#g-supply-chain-) | `security` `infra` | 🔲 | S | 🟡 G2 missing `org.opencontainers.image.*` labels on any Dockerfile (SBOM scanners miss provenance); G3 `go build` missing `-trimpath -ldflags=-buildid=` for SLSA-L3 reproducibility. G1 (worker image digest pin) is tracked by #21. |
| 42 | Proxy `/readyz` must gate on CONNECT listener (analogue of GMC §11.D fix) | `security` `infra` `bug` | 🔲 | S | Surfaced by item 6 re-run on 2026-05-30. `cmd/proxy/proxy.go` serves `/healthz` (returns 200 as soon as health server binds) but the CONNECT server on port 8080 is in a separate goroutine — kubelet can mark the pod Ready and add it to the Service endpoints before CONNECT is listening, causing transient `connection refused` for worker HTTPS_PROXY traffic on proxy rollouts/HPA scale-up. Add `/readyz` that returns OK only after the CONNECT listener has bound; switch readiness probe in `cmd/gmc/internal/controller/builder.go` from `/healthz` to `/readyz`. Diagnosis in `docs/plan/milestone-3.md` §11.D follow-up. Overlaps with #34 (manifest defaults) but is its own diagnosis and patch. |
| 11 | [Ed25519 live probe — M-11b](plan/security.md) | `security` `tests` | 🔲 | S | **Unblocked by item 6 on 2026-05-30** (live kind cluster + real GitHub App available). |
| 12 | [M5 packaging — Kustomize overlay](plan/milestone-5.md) | `milestone` | 🔲 | L | **Unblocked by item 6 on 2026-05-30.** |
| 28 | [SBOM + cosign signing of built images](plan/security.md) | `security` `infra` | 🚫 | M | → M5 packaging. Distroless + digest pinning are the foundation. |
| 29 | [API server audit policy sample](plan/security.md) | `security` `infra` | 🚫 | S | → M5 packaging. Surfaces a compromised GMC's Secret `get` calls. |
| 13 | [M5 load test harness](plan/milestone-5.md) | `milestone` `tests` | 🚫 | L | → M5 packaging |
| 14 | [M5 polaris/kube-bench posture scan](plan/milestone-5.md) | `milestone` `security` | 🚫 | S | → M5 packaging |
| 15 | [M5 gVisor RuntimeClass validation](plan/milestone-5.md) | `milestone` | 🚫 | S | needs a cluster with gVisor installed |
| 37 | Go best-practices: CI lint gate | `infra` `tests` | 🔲 | S | Add `gofmt -l`, `go vet`, and `golangci-lint` to `.github/workflows/unit-test.yml`. 20 files currently fail `gofmt -l` on `main`; no Go linter runs in CI today. A repo-root `.golangci.yml` is also missing (only `cmd/gmc/.golangci.yml` exists, Kubebuilder default, never invoked). Would prevent regressions of items 38–41. |
| 38 | Go best-practices: unify Go versions across modules | `infra` | 🔲 | S | Three distinct `go` directives across 9 `go.mod` files: `1.26` (broker, githubapp, cmd/proxy, cmd/probe, test/fakegithub), `1.26.0` (cmd/agc, cmd/gmc, tools), `1.26.3` (cmd/worker, matches `go.work`). Pin all to `1.26.3`. CLAUDE.md hard rule: "All go modules in the repo must use the same Go version." Also drop the now-redundant `replace` directives covered by `go.work` in broker/agc/gmc/probe. |
| 39 | Go best-practices: fix CLAUDE.md async-channel violation | `bug` | 🔲 | S | `cmd/agc/internal/listener/goroutine.go:121` `StartRenewLoop` returns `stop func()` and hides the done channel inside the returned closure — violates the CLAUDE.md "return `<-chan struct{}` done channel; do not hide it inside a closure or call site" rule. Change signature to `(stop func(), done <-chan struct{})` and update callers. |
| 40 | Go best-practices: misc idiom cleanup | `bug` | 🔲 | S | (a) `cmd/agc/internal/provisioner/provisioner.go:210` does `_ = json.Unmarshal(payload, &ap)` then uses the parsed struct — silent payload corruption risk. (b) `cmd/agc/internal/listener/multiplexer.go:66` `SetMaxListeners(max int32)` shadows the Go 1.21+ builtin `max`. (c) Rename `broker.BrokerClient` → `broker.Client` (package-name stutter). (d) Replace the 8 remaining `interface{}` occurrences with `any` in non-test code (e.g. `test/fakegithub/main.go:67`, `broker/brokertest/server.go:31,169`). (e) Remove dead `_ = name // used for label selector above` at `cmd/agc/internal/controller/actionsgateway_controller.go:246`. |
| 41 | Go best-practices: extend goleak coverage | `tests` | 🔲 | S | `broker/` and `cmd/worker/` spawn goroutines but their test suites don't apply `goleak.VerifyNone` in `TestMain`. Pattern already established in `cmd/proxy/proxy_test.go` and `cmd/agc/internal/{listener,token}/*_test.go`. `goleak` is already a `broker/` dependency — just unused. Closes a quiet correctness gap. |
| 43 | Structured `Blocked by:` + queue-unblock helper | `infra` `docs` | 🔲 | S | Replace free-text "→ X" blocker notes with `Blocked by #N`; add `make queue-unblock ID=N` to enumerate dependents for one-commit unblock sweeps. Fixes the stale-blocker class CLAUDE.md already warns about. See [maintaining-backlog](development/maintaining-backlog.md). |
| 44 | `make lint-status` for STATUS.md format rules | `infra` `tests` | 🔲 | S | ~30 LoC shell enforcing the rules in [maintaining-backlog](development/maintaining-backlog.md): single-line `Last touched:`, no duplicate Queue IDs, Notes ≤250 chars. Wire to `unit-test.yml` + pre-commit. |
| 45 | Compress Progress table — drop Notes column | `docs` | 🔲 | S | Most cells just say "see plan" or restate the plan doc; the plan link in the row's name already carries the detail. Reduces edit surface and width. |
| 46 | Archive completed plan docs under `docs/plan/archive/` | `docs` | 🔲 | S | When STATUS.md no longer references a plan doc, move it to the archive subdir so `ls docs/plan/` shows active work only. Document the move-on-close step in [maintaining-backlog](development/maintaining-backlog.md). |
| 47 | Append-by-default for new low-priority Queue rows | `docs` | 🔲 | S | Loosen "insert at right priority position" to "append unless re-prioritizing" so row order stays stable in diffs across parallel sessions. Re-prioritization becomes a deliberate separate commit. |
| 17 | [Unit/integration test speed improvements](plan/unit-tests-speed.md) | `speed` `tests` | 💤 | M | low priority; pick up when CI latency is the bottleneck |
| 18 | [alerting.md](plan/docs.md) | `docs` | 💤 | M | deferred until a real Prometheus/Alertmanager setup exists |
| 19 | [Proxy features: allowlist, rate-limit, audit log, TLS, per-RG pool, X25519](design/appendix-g-future-enhancements.md) | `security` | 💤 | L | explicit non-commitments; build only when a named trigger fires |
