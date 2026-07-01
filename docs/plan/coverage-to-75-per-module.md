# Per-module unit-test coverage → ≥75%

Tracked as [Q255](../STATUS.md#Q255).

---

## Overview

**Goal:** bring every Go module's hand-written unit-test coverage to **≥75%**, as measured by [`scripts/coverage.sh`](../../scripts/coverage.sh) (generated DeepCopy/scheme boilerplate and test-helper packages already excluded).

**Why now:** the coverage gate is a [no-regression ratchet, not an absolute target](../development/testing.md#coverage-measurement-and-the-ratchet) — deliberately, to avoid manufacturing low-value tests. So this plan is *not* about chasing a number for its own sake. It is about closing the specific gaps where the untested code carries real production risk, and doing so lands every module at ≥75% as a side effect. Where a gap is a genuinely-thin entrypoint whose logic is already factored into tested helpers, we leave it uncovered by design and say so.

**Baseline (2026-06-30), from `make cover`:**

| Module | Current | Gap to 75% | Nature |
|---|---|---|---|
| `./githubapp` | 84.2% | — | above bar |
| `./broker` | 82.1% | — | above bar |
| `./cmd/proxy` | 76.7% | — | above bar |
| `./cmd/agc` | 75.1% | — | above bar (on the 0.5pp tolerance edge) |
| `./cmd/worker` | 73.7% | +1.3 | two error branches away |
| `./cmd/gmc` | 66.3% | +8.7 | spread across controller + v2 webhooks |
| `./api` | 47.6% | +27.4 | trivial — 2 real funcs + 4 `init()` untested |
| `./cmd/probe` | 46.6% | +28.4 | concentrated — `run()` is a 320-line entrypoint at 0% |

`./test/fakegithub` is a test-helper module (`n/a`, excluded from the ratchet) — out of scope.

---

## Entrypoint thinness audit

Skipping an entrypoint is only acceptable when it is *thin* — its non-trivial logic already lives in separately-tested helpers, so there is little hidden-bug surface to miss. Audit result:

| Entrypoint | Size / cover | Verdict |
|---|---|---|
| `worker.main()` | ~24 lines | **Thin ✓** — dispatch to `installSelf`/`run` only |
| `worker.run()` | 138 lines @ 65% | **Skippable ✓** — OS plumbing (pipes/exec/subprocess), linear with syscall-error returns; the real logic (`translateWorkerExitCode`, `materializeJITConfig`, `readPayload`, …) is extracted and tested |
| `gmc.main()` | ~575 lines @ 0% | **Skippable ✓** — long, but every non-trivial decision is factored into helpers (`validateLeaderElectionTimings`, `validateImageDigest`, `parseAPIServerCIDRs` — already tested); body is flag registration + manager wiring, exercised by envtest/e2e |
| `probe.main()` | ~7 lines | **Thin ✓** |
| `probe.run()` | **320 lines @ 0%** | **NOT thin ✗** — embeds ~60 lines of env/config parsing with a dozen error branches, plus token-mint + broker-session orchestration, all untested. Real hidden-bug surface. **Needs refactor** (see Workstream E) |

Exactly one entrypoint fails the test: `probe.run()`. Everywhere else the risky logic is already extracted, so the remaining work is test-only.

---

## Sequencing rule

Per session guidance: **do not change production code and tests in the same commit** where avoidable. The only production-code change in this plan is the `probe.run()` refactor (Workstream E), which is split into two PRs — a behavior-preserving refactor whose *existing tests stay green and unchanged* (proof the refactor is safe), then a test-only follow-up. Every other workstream is test-only.

After each workstream lands, run `make cover-update` and commit the raised `coverage-baseline.txt` floor in its own isolated commit so the ratchet defends the gain.

---

## Workstreams

Ordered by ROI (cheapest, highest-certainty first).

### A — `api` → ~100% (test-only, one PR)

The whole `api/v2alpha1/` package has no test file; only `apilabels` is covered. After excluding generated code, the only uncovered hand-written funcs are:

- `(*ActionsGatewaySpec).GitHubAppSecretName()` — nil-`GitHubApp` vs set branch
- `EffectiveSecurityProfile(profile)` — empty→`SecurityProfileBaseline` vs passthrough
- 4× `init()` (SchemeBuilder registration) — run on package load, so merely *having* a test in the package counts them covered

**Add `api/v2alpha1/types_test.go`:** table tests for both helpers + a scheme-registration smoke test (`AddToScheme` into a fresh `runtime.Scheme`, assert no error). Clears the module on its own.

Optionally fold a one-case buffer test into whichever module sits on the tolerance edge (`cmd/agc`) here or in Workstream B.

### B — `cmd/worker` → ~77% (test-only, one PR)

Extend `cmd/worker/worker_test.go` with cases for the uncovered branches of `run()` (65%) and `installSelf()` (71.4%) — the extracted, testable paths (bad dir, missing binary, env fallbacks). `main()` stays 0% by design (thin dispatch).

### C — `cmd/gmc` pure helpers (test-only, part of the gmc PR)

Three already-extracted parsers in `cmd/gmc/cmd/main.go` are untested at 0%:

- `parseAllowedPriorityClasses(raw)`
- `parseAllowedEgressCIDRs(raw)` — including the malformed-CIDR error branch
- `mustEnv(name)` — set vs unset

Add `cmd/gmc/cmd/main_test.go` (or extend the existing test) with table tests. No code change.

### D — `cmd/gmc` controller/webhook/metrics (test-only, part of the gmc PR)

The bulk of the +8.7pp. All pure logic, unit-testable without a cluster:

- **v2 webhook validators** (`internal/webhook/v2alpha1`, package at 26.8%): `egressproxy_webhook` `validateEgressDestinations` + `ValidateCreate/Update/Delete`; `runnertemplate_webhook` `ValidateCreate/Update/Delete` (both types) + `logRejection`. All 0%.
- **Builder helpers**: `builder.go` `workerLabels`, `buildWorkerServiceAccount`, `securityProfileOrDefault`; `egressproxy_builder.go` `proxyAllowlistEnv`, `proxyHostSuffix`; `actionsgateway_v2_builder.go` `agcResources`.
- **Metrics collectors** (`metrics.go`, all 0%): `NewMetrics` + each collector's `Describe`/`Collect`, driven by `prometheus/testutil`.
- **Migrate**: `internal/migrate/render.go` `renderWarningsComment`; `migrate.go` `runnerGroupName`/`labelSafe` partials.

`Reconcile`/`SetupWithManager` (0%) stay for the existing `internal/controller/integration/` envtest suite — add a couple of reconcile cases there only if C+D fall short of +8.7pp.

### E — `cmd/probe` → ≥75% (the one refactor, **two PRs**)

**PR E1 — refactor only, behavior-preserving.** Extract from `run()`:
- `parseProbeConfig(getenv func(string) string) (probeConfig, error)` — the ~60-line env/config block (app ID/installation ID parsing, PEM load, broker URL/v2 selection, pool ID) with all its error branches.
- keep the `investigate*`/`probeAcknowledge` broker calls at package scope (they already are).

`run()` shrinks to thin orchestration (mint token → open session → dispatch investigations). **Existing tests stay green and unchanged** — that is the safety proof. No new tests in this PR.

**PR E2 — tests only.** Unit-test:
- `parseProbeConfig` branches (missing var, unparseable int, bad PEM, v2-URL override, pool-ID default/override).
- pure helpers `backoffDelay`, `jitter` (bounds), `mustEnv`, `loadPEM` (inline PEM / `@file` / error).
- `investigateSessionReuse`, `investigateJobDelivery`, `probeAcknowledge` against the `broker/brokertest` stub.

Lifts `probe` past 75%; `main()`/the residual `run()` orchestration stay uncovered by design.

---

## PR sequence

1. **A** — `api` tests → `cover-update`
2. **B** — `cmd/worker` tests (+ `cmd/agc` buffer) → `cover-update`
3. **C+D** — `cmd/gmc` tests → `cover-update`
4. **E1** — `cmd/probe` refactor (tests unchanged)
5. **E2** — `cmd/probe` tests → `cover-update`

Each is scoped to one module/concern. A/B/C+D/E2 are pure test additions; E1 is a pure refactor. No PR mixes production-code and test changes.

---

## Done when

- `make cover` shows every module (except the `n/a` helper) at ≥75%.
- `coverage-baseline.txt` floors raised to match; `make cover-check` green.
- The `probe.run()` config-parsing logic is under unit test.
