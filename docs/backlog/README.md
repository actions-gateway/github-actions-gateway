# Backlog

Prioritized near-term work — things not yet started but worth picking up soon.

## Conventions

**Sizes:** S = fits in one session | M = 2–3 sessions | L = needs a standalone plan doc in `docs/plan/`

- **S items:** delete from the backlog and commit directly. No plan doc needed.
- **M/L items:** keep the backlog entry marked **▶ Started** until the plan doc is committed to `docs/plan/`, then remove the backlog entry in the same commit.

**Lifecycle:**
- Items graduate to `docs/plan/` when actively started (M/L) or are removed on completion (S).
- Items that are genuinely deprioritized with a known reason go into **Parked** below.
- Long-horizon "someday" features live in `docs/design/appendix-g-future-enhancements.md`.

Last refreshed: 2026-05-25.

---

## Ready

Ordered by priority. Start from the top.

| # | Item | Size | Blocker |
|---|---|---|---|
| 1 | [`buildNoProxy` merge bug](#1-buildnoproxy-merge-bug) | S | — |
| 2 | [`proxy.resources` per-key merge](#2-proxyresources-per-key-merge) | S | — |
| 3 | [Named Pipe investigation (M3 Investigation A)](#3-named-pipe-investigation-m3-investigation-a) | M | — |
| 4 | [Wire live `GithubRegistrar` in `main.go`](#4-wire-live-githubregistrar-in-maingo) | S | Live GitHub creds |
| 5 | [Expose `maxEvictionRetries` / `evictionRetryDelay` on RunnerGroup CRD](#5-expose-maxevictionretries--evictionretrydelay-on-runnergroup-crd) | S | — |
| 6 | [M2 envtest goroutine-leak integration suite](#6-m2-envtest-goroutine-leak-integration-suite) | M | — |
| 7 | [Credential rotation: Secret watch + `CredentialUnavailable` condition](#7-credential-rotation-secret-watch--credentialunavailable-condition) | M | — |
| 8 | [M3 metric assertions + dead `PodCreationLatency` field](#8-m3-metric-assertions--dead-podcreationlatency-field) | S | — |
| 9 | [M4 remaining test gaps](#9-m4-remaining-test-gaps) | S | — |
| 10 | [Open docs items](#10-open-docs-items) | S | — |
| 11 | [Rename `actions-gateway-agc` → `actions-gateway-controller`](#11-rename-actions-gateway-agc--actions-gateway-controller) | M | — |
| 12 | [Go workspace prefix-match bug investigation](#12-go-workspace-prefix-match-bug-investigation) | S | — |

---

### 1. `buildNoProxy` merge bug

**What:** Fix `buildNoProxy` in `cmd/gmc/internal/controller/builder.go` to always append mandatory cluster-internal exclusions (`kubernetes.default.svc.cluster.local`, `localhost`, etc.) rather than replacing them when the operator supplies `spec.proxy.noProxyCIDRs`.

**Why now:** Active bug — if any operator sets `spec.proxy.noProxyCIDRs`, the AGC's Kubernetes API calls are routed through the egress proxy instead of directly to the API server, causing the AGC to malfunction silently at runtime.

**Done when:**
- `buildNoProxy` prepends user CIDRs and always appends `defaultNoProxy`.
- `builder_test.go` covers: user sets custom CIDRs (assert defaults preserved), user sets nothing (assert full defaults), user sets CIDRs that overlap with defaults (assert no duplicates acceptable).
- Full detail in [docs/plan/milestone-4-tests.md §1](../plan/milestone-4-tests.md).

**Size:** S (3-line fix + tests)

---

### 2. `proxy.resources` per-key merge

**What:** Change the `resources` override logic in `builder.go` from full-replace to per-key merge so an operator who sets only `limits.memory` still gets the default `requests.cpu`.

**Why now:** Silent HPA failure — a partial override drops `requests.cpu`, causing HPA to report `<unknown>` CPU utilization and autoscaling to stop with no error or event.

**Done when:**
- `buildProxyDeployment` merges `Requests` and `Limits` key-by-key over defaults.
- Webhook emits a `Warning` (not rejection) when `proxy.resources.requests` is set without a `cpu` key.
- Tests cover: limits-only override preserves default CPU request; explicit full override wins; no-override preserves all defaults.
- Full detail in [docs/plan/gaps.md §2](../plan/gaps.md).

**Size:** S (15-line fix + webhook change + tests)

---

### 3. Named Pipe investigation (M3 Investigation A)

**What:** Determine the exact Named Pipe protocol that `Runner.Worker` expects — pipe names, direction, payload format — then implement and validate `writeToNamedPipes` in `cmd/worker/main.go`.

**Why now:** Critical-path blocker. The M3 green-checkmark criterion (real job completes in GitHub Actions UI) and all M4 end-to-end validation are unreachable until the pipe handoff is confirmed. Everything else in M3/M4 is code-complete.

**Done when:**
- §11.A in `docs/plan/milestone-3.md` is filled with: pipe names, write direction, payload format, source citations.
- `writeToNamedPipes` in `cmd/worker/main.go` is implemented based on confirmed findings.
- Implementation validated against `testdata/job_payload.json` using a stub `Runner.Worker` script.
- Full investigation procedure in [docs/plan/milestone-3.md §5.A](../plan/milestone-3.md).

**Size:** M (investigation + implementation + local validation)

---

### 4. Wire live `GithubRegistrar` in `main.go`

**What:** Replace `StubRegistrar` with `GithubRegistrar` as the default in `cmd/agc/main.go` after confirming the registration schema against a live GitHub App and filling in §11.A in `docs/plan/milestone-2.md`.

**Why now:** `StubRegistrar` is currently wired in the production binary — the AGC cannot register real runner agents on any live cluster until this switches.

**Done when:**
- Live `config.sh --debug` capture or direct API test confirms `GithubRegistrar` request/response schema matches real GitHub.
- `StubRegistrar` removed from the default code path (still available for testing).
- `milestone-2.md §11.A` updated with live-confirmation note.
- Full context in [docs/plan/milestone-2.md §11.A](../plan/milestone-2.md).

**Size:** S (investigation + 1-line change in `main.go`)  
**Blocker:** Requires live GitHub App credentials.

---

### 5. Expose `maxEvictionRetries` / `evictionRetryDelay` on RunnerGroup CRD

**What:** Add two optional fields to `RunnerGroupSpec` and thread them into the provisioner via `HandlerFor` so operators can tune eviction-retry behavior per RunnerGroup instead of sharing hardcoded defaults.

**Why now:** The design doc already specifies these fields; they're hardcoded in `NewProvisioner`. Operators with GPU workloads (where any eviction warrants manual inspection) cannot disable auto-retry.

**Done when:**
- `MaxEvictionRetries` and `EvictionRetryDelay` added to `RunnerGroupSpec` with CEL validation.
- `provisioner.HandlerFor` reads them and passes per-call values (not stored on `p` to avoid data races).
- CRD YAML regenerated and committed.
- Tests: `maxEvictionRetries: 0` produces no rerun call; `evictionRetryDelay: "50ms"` delay is respected.
- Full detail in [docs/plan/gaps.md §1](../plan/gaps.md).

**Size:** S (type change + provisioner wiring + CRD regen + tests)

---

### 6. M2 envtest goroutine-leak integration suite

**What:** Implement the envtest integration scenarios from `milestone-2.md §7.2`: RunnerGroup create/scale/delete lifecycle, job acquisition cycle, SIGTERM graceful shutdown, and agent Secret persistence across AGC restart.

**Why now:** Two M2 success-criteria checklist items remain unchecked; the goroutine-leak behavior of the full reconciler under real Kubernetes CRUD is untested at the integration level.

**Done when:**
- All 7 scenarios from §7.2 pass under `envtest`.
- `goleak.VerifyNone` passes after each scenario.
- `milestone-2.md` success-criteria checkboxes updated.
- Full spec in [docs/plan/milestone-2.md §7.2](../plan/milestone-2.md).

**Size:** M (new test file, no production code changes)

---

### 7. Credential rotation: Secret watch + `CredentialUnavailable` condition

**What:** Three targeted changes to the GMC: (A) add a pod-template annotation recording the referenced Secret name so rotations appear in rollout history; (B) add a Secret watch that triggers reconcile and sets `CredentialUnavailable` when the Secret is gone; (C) document the rotation procedure in `docs/getting-started.md`.

**Why now:** Silent failure — if the referenced Secret is deleted, the AGC pod keeps running but any restart will permanently break it, with no condition or event to warn the operator.

**Done when:**
- Pod-template annotation `actions-gateway/github-app-secret` present in AGC Deployment.
- GMC reconciler sets `CredentialUnavailable` condition within one reconcile cycle of Secret deletion.
- Integration tests: rotation produces new ReplicaSet; Secret deletion produces condition; in-place Secret update does not produce spurious rollout.
- `docs/getting-started.md` has a "Rotating GitHub App credentials" section.
- Full detail in [docs/plan/gaps.md §3](../plan/gaps.md).

**Size:** M (controller change + watch registration + integration tests + docs)

---

### 8. M3 metric assertions + dead `PodCreationLatency` field

**What:** Add a `newTestMetrics()` helper to `provisioner_test.go`, assert `JobDuration`, `EvictionRetries`, and `EvictionRetriesExhausted` in the relevant tests, and either emit `PodCreationLatency` in the provisioner or remove the unused field.

**Why now:** Prometheus metrics are declared but never asserted in tests; `PodCreationLatency` is dead observability — declared and never recorded, so it silently reports nothing under any real workload.

**Done when:**
- `newTestMetrics()` helper in `provisioner_test.go` (mirrors pattern in `goroutine_test.go`).
- `TestProvisioner_CreatesPodAndSecret` asserts `JobDuration > 0`.
- `TestProvisioner_EvictionAutoRetry` asserts `EvictionRetries == 1`.
- `PodCreationLatency` is either emitted in `Provision` or removed from `Metrics`.
- Full detail in [docs/plan/milestone-3-tests.md §H1](../plan/milestone-3-tests.md).

**Size:** S (test-only changes except for the `PodCreationLatency` decision)

---

### 9. M4 remaining test gaps

**What:** Fill the remaining unit-test gaps from `docs/plan/milestone-4-tests.md`: `buildIPRangesCIDR` edge cases, `buildNoProxy` table-driven tests (after the bug fix in #1), webhook IP-range validation, and the `buildHPA` / `buildPDB` coverage items.

**Why now:** These are low-friction unit tests against already-shipped code; they guard against regressions during the M3 pipe-handoff and M5 packaging work ahead.

**Done when:**
- All open items in `docs/plan/milestone-4-tests.md` are resolved (checkboxes updated).

**Size:** S (test-only; no production code changes expected)

---

### 10. Open docs items

**What:** Four small documentation items from `docs/plan/docs.md`: (2.7) HPA silent-failure callout on `ProxyConfig` in the API contracts doc; (2.6) `DefaultWorkerImage` note in `03-api-contracts.md`; (2.3) worked capacity-planning examples in `appendix-e`; (3.2) alerting/dashboards doc under `docs/operations/`.

**Why now:** Items 2.7 and 2.6 are one-paragraph changes with high operator-safety value. Item 3.2 should wait until a real Prometheus setup exists.

**Done when:**
- `03-api-contracts.md` has the HPA `requests.cpu` requirement callout under `ProxyConfig` (2.7) and a `DefaultWorkerImage` note (2.6).
- `appendix-e-capacity-planning.md` has at least two worked sizing examples (2.3).
- `docs/plan/docs.md` status table updated.

**Size:** S (docs only)

---

### 11. Rename `actions-gateway-agc` → `actions-gateway-controller`

**What:** Standardize the on-cluster name of the AGC Deployment (and related constants across GMC and AGC) to `actions-gateway-controller`, matching the name used throughout docs and the design.

**Why now:** Code/docs mismatch since Milestone 4 — the implementation diverged silently from the planned name. Operators reading the docs see `actions-gateway-controller`; `kubectl get deploy` shows `actions-gateway-agc`.

**Done when:**
- One exported `ControllerName` constant in a shared package; both binaries import it.
- All five Go-side literals replaced; all tests updated (no literal-string greps).
- Operations docs flipped back to `actions-gateway-controller`.
- Release notes mention on-cluster rename, manual cleanup command, and Prometheus label change.
- Full scope and migration plan in [docs/plan/rename-agc-to-controller.md](../plan/rename-agc-to-controller.md).

**Size:** M (touches two binaries, test suite, and ops docs)

---

### 12. Go workspace prefix-match bug investigation

**What:** Check the Go issue tracker for the workspace module-path prefix-matching bug that forced the `replace` workaround in `go.work`, and remove the workaround if the bug is fixed.

**Why now:** Low urgency but worth checking periodically — if Go 1.22–1.24 fixed it, the `replace` directive in `go.work` can be dropped, simplifying the workspace layout.

**Done when:**
- Go issue tracker searched; release notes for Go 1.22–1.24 checked.
- If fixed: `replace` directive removed from `go.work`, workspace tested.
- If unfixed: upstream issue filed (or confirmed existing); `docs/development/go-workspaces.md` updated with issue link.
- Background in `docs/development/go-workspaces.md`.

**Size:** S (investigation; code change only if bug is fixed upstream)

---

## Parked

Explicitly deprioritized. Pick up when the blocker clears or priority shifts.

| Item | Why parked | Since |
|---|---|---|
| M2 `kind` live `activeSessions == 1` check | Deferred until envtest suite (#6) passes; the kind run is redundant until integration tests are green | 2026-05-25 |
| M3/M4 kind end-to-end validation | Blocked on Named Pipe investigation (#3); will become the next ready item once #3 is done | 2026-05-25 |
| Egress proxy live `curl` validation | Blocked on M3/M4 kind end-to-end working; validates NetworkPolicy split already in code | 2026-05-25 |
| M2-tests remaining unit gaps (Gaps 3/4/5) | Medium priority; interleave with #6 once started | 2026-05-25 |
| M3-tests H2/M1/M2/L items | Medium priority; add incrementally after #8 lands | 2026-05-25 |
| M5 Kustomize/Helm packaging (`deploy/`) | Blocked on end-to-end green checkmark in kind | 2026-05-25 |
| M5 load test harness (`test/load/`) | Blocked on packaging | 2026-05-25 |
| M5 `polaris`/`kube-bench` scan | Blocked on packaging | 2026-05-25 |
| Speed improvements (unit/integration/e2e/docker) | Low priority; pick up when CI is the bottleneck | 2026-05-25 |
| `docs/operations/alerting.md` (docs item 3.2) | Deferred until a real Prometheus/Alertmanager setup exists to source from | 2026-05-25 |
| gVisor `RuntimeClass` validation | Needs a cluster with gVisor installed; operator concern | 2026-05-25 |
