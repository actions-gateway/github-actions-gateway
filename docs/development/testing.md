# Agent reference: Testing

## Running tests

The repo is a Go workspace (`go.work`), so `go test ./...` from the repo root does **not** work — run tests per module. See [go-workspaces.md](go-workspaces.md) for why.

```bash
(cd broker     && go test ./...)    # broker module
(cd githubapp  && go test ./...)    # githubapp module
(cd cmd/agc    && go test ./...)    # AGC module
(cd cmd/gmc    && go test ./...)    # GMC module
(cd cmd/probe  && go test ./...)    # probe module
(cd cmd/proxy  && go test ./...)    # proxy module
(cd cmd/worker && go test ./...)    # worker module
```

Run tests locally before pushing to a PR to avoid burning CI. Prefer the narrowest scope that covers the change: a single module's unit tests, `-run` to target a specific test, integration tests for controller changes, or `--focus` for a targeted e2e spec. Run the full e2e suite only when the change is broad enough to warrant it.

### The `make check` pre-review gate

For the one-command gate before requesting review, run `make check` from the repo root. It runs gofmt, `golangci-lint`, the `docs/STATUS.md` format lint, the single-Go-version gate (`make go-version-check`, which asserts the `go` directive matches across `go.work`, every `go.mod`, and every `go.work.gen`), `shellcheck` over the helper scripts (see [the shell-lint gate](#the-shellcheck-gate) below), the Markdown link/anchor check (see [the doc-link gate](#the-doc-link-gate) below), and the (plain) unit tests across every module. This is the fast local loop and covers the lint and unit-test *logic* the `.github/workflows/unit-test.yml` workflow enforces. The one CI step `make check` does **not** reproduce is the race detector: the CI `unit-test` job runs the same per-module unit tests under `-race` (see [the race gate](#the-race-detector-unit-gate) below), which roughly doubles their runtime. Reproduce that locally with `make test-race` — kept out of `make check` so the default dev gate doesn't become an unthrottled `-race` run. The slower security gates (`make vulncheck`, `make trivy-scan`, `make polaris-scan`), the [install-artifact validation](#install-artifact-validation) (`make manifest-validate`), and the integration/e2e tiers below stay separate too so this loop stays fast.

Test output is non-verbose by default: `go test` prints one `ok <pkg>` line per passing package and the full output of any package that fails (compress success, expand failure). When debugging a **slow or hanging** test, add `V=1` (`make check V=1` or `make test V=1`) to stream output live — without `-v`, `go test` buffers each package's output until the package completes, so a hung test shows nothing (not even its `t.Log` lines) until it finishes or hits `-timeout`.

A sub-second subset (gofmt on staged Go files + the STATUS.md lint) also runs automatically at commit time via the tracked pre-commit hook in `.githooks/`. Install it once with `make hooks` (or `scripts/setup.sh`); bypass a single commit with `git commit --no-verify`.

#### Resource auto-throttle on GUI dev machines

`make lint`/`make test`/`make check` lint each module with `golangci-lint` (which fans out one worker per logical CPU and ignores `GOMAXPROCS`/`GOFLAGS`) and run `go test` across every module. On a small machine this can saturate every core and make the desktop unresponsive. On macOS it is worst: the WindowServer compositor misses its kernel watchdog and restarts — the whole GUI freezes (it shows up as `WindowServer … userspace_watchdog_timeout` in **Console ▸ Crash Reports**). On a Linux/WSL desktop you instead get input lag and compositor stutter while the build runs.

To prevent that, these phases auto-throttle on an **interactive, GUI-bearing dev shell**: the scripts behind the make targets (`scripts/go-test.sh`, `scripts/go-lint.sh`, `scripts/coverage.sh`) run them at a low-priority QoS tier that demotes both CPU **and** disk I/O below the desktop (macOS: `taskpolicy -c utility`; Linux/WSL: `nice -n 19`, plus `ionice -c 3` when available), and cap parallelism to physical-cores − 2 (`golangci-lint -j`, `go test -p`, `GOMAXPROCS`). Detection and sizing live in [`scripts/local-throttle.sh`](../../scripts/local-throttle.sh).

On macOS the I/O demotion matters as much as the CPU demotion: an unthrottled build already runs at a lower QoS than WindowServer yet still trips the watchdog, so the fix is throttling the build's I/O so the compositor's I/O isn't stuck behind it — and `taskpolicy` is the only macOS way to express that (there is no `ionice`). The gentler `utility` tier is used rather than the lowest `background`/`-b` band because it delivers the same protection while letting builds finish 2–4× faster.

Only the make targets (via their scripts) throttle themselves, so a bare `go build`/`go test` run directly (not via `make`) bypasses it — a heavy `-race` run that way once froze the macOS GUI. Two safety nets cover that gap, both reusing `scripts/local-throttle.sh` so they share the same activation rules and stay no-ops on CI/headless/SSH:
- **When you call `go` directly, prefix it** with `$(scripts/local-throttle.sh prefix)` (e.g. `$(scripts/local-throttle.sh prefix) go test -race ./...`), or just run it under `make` where a target exists.
- A Claude Code `PreToolUse` hook ([`scripts/claude-go-throttle-hook.sh`](../../scripts/claude-go-throttle-hook.sh), wired in `.claude/settings.json`) automates that prefix for agent-run commands: a bare `go build`/`go test` is rewritten transparently to carry it. It deliberately auto-allows only that bare form — never a compound command (`cd … && go test …`) or one with a redirect — so its `allow` can't carry another segment or an outside-workspace redirect past the permission system or the branch-guard/workspace-guard hooks; such a command carrying `-race` is blocked instead, with a reminder to add the prefix manually.

It is a no-op everywhere the throttle would only slow things down for no benefit, so those runs go at full speed:
- **CI** — the `CI` environment variable is set (GitHub Actions et al.).
- **Headless / SSH Linux shells** — no graphical session (`DISPLAY`/`WAYLAND_DISPLAY` unset), so build servers and remote shells are unaffected.
- **Unsupported OSes** — native Windows (Git Bash/MSYS); use WSL2, which reports as Linux and follows the Linux rule.

To opt out locally (e.g. a machine with cores to spare), set `CI=1` for the run: `CI=1 make check`.

##### Not every WindowServer watchdog crash is a build

The throttle addresses one specific cause of `WindowServer … userspace_watchdog_timeout`: a build saturating CPU **and** disk I/O so the compositor's own work is stuck behind it. That is a *resource-starvation* stall — WindowServer's main thread is runnable but can't get serviced. There is a second, unrelated cause that the throttle does **not** fix, and the two look identical in **Console ▸ Crash Reports** (same `userspace_watchdog_timeout` suffix), so confirm which one you hit before assuming a build was at fault:

- **GPU/compositor stall (integrated-graphics contention).** On a Mac with integrated graphics (e.g. the `MacBookPro16,2` 13" with Intel Iris Plus, shared-memory VRAM), WindowServer's main thread can *block* waiting on the GPU/display pipeline to return a frame — not starve for CPU. The spin report's reason reads `Display … not ready: DisplayID: 0x…`, WindowServer's own CPU time in the window is tiny (well under 1 s), and the sampled kernel threads name the GPU stack (`AppleIntelICLGraphicsMTLDriver`, `AppleIntelFramebuffer`, `AppleGPUWrangler`, `IntelAccelerator`). The driver here is many simultaneous GPU clients on one weak iGPU: each Chromium/Electron app runs its own GPU process (`CrGpuMain`/`GpuWatchdog` — Claude desktop, Chrome, Slack, Discord, VS Code/GoLand), a Virtualization.framework VM adds a `virtio-gpu` client, and a Spotlight (`mds`) reindex piles on. No `go` process need be involved, and memory/swap can be near-idle. The throttle wrapper cannot help — it only demotes CPU/I/O, not GPU command-queue pressure.

  To tell them apart, read the spin file in `/Library/Logs/DiagnosticReports/WindowServer_*.spin`: a *build* stall shows WindowServer hot or its work blocked behind heavy I/O; a *GPU* stall shows the `Display … not ready` reason and the Intel/GPU driver threads above. Mitigate the GPU case by reducing concurrent GPU clients (close unused Electron apps, shut down the VM if headless, let Spotlight finish or exclude worktrees/module caches/Docker data from indexing); a reboot resets the accumulated `N induced crashes` counter.

### The race-detector unit gate

The CI `unit-test` job runs the per-module unit tests under Go's race detector (`go test -race`), not plain `go test`. The multiplexing core — agentpool, listener/mux, broker, token — is where data races hide, and plain `go test` never flags them; `-race` is pass/fail (a detected race fails the job). This is the only `unit-test.yml` step `make check` does not mirror, because `-race` instruments every memory access and roughly doubles unit runtime.

Reproduce the CI race gate locally with:

```bash
make test-race        # per-module `go test -race` across the whole workspace
```

`make test-race` is the single source of truth for the race flags and timeout the CI job uses, and it carries the **same** throttle prefix and parallelism cap as `make test` (see [the auto-throttle above](#resource-auto-throttle-on-gui-dev-machines)). That matters here more than anywhere: a `-race` build is a ~2–10× CPU/memory/I/O amplifier, so an *unthrottled* one on a GUI dev machine is the most likely single command to trip the macOS WindowServer watchdog. Run it through `make test-race` (throttled) rather than a bare `go test -race`, or prefix a manual run with `$(scripts/local-throttle.sh prefix)`. On CI the throttle is a no-op, so the job runs at full speed. The detector needs cgo, which is available on both the ubuntu CI image and a macOS dev box by default.

It is deliberately a separate target from `make test`/`make check` so the fast local loop stays fast and never silently becomes a `-race` run; treat it like `make vulncheck` — a heavier gate you run when a change warrants it (anything touching the concurrency core) or before a final pre-PR pass.

### Coverage measurement and the ratchet

The CI `unit-test.yml` workflow has a `coverage` job that measures per-module unit-test coverage and gates it with a **no-regression ratchet**, not an absolute percentage target. [`scripts/coverage.sh`](../../scripts/coverage.sh) is the single source of truth; the Makefile exposes three targets, all of which measure coverage the same per-module way the workspace requires (a repo-root `go test ./...` does not work — see [go-workspaces.md](go-workspaces.md)):

```bash
make cover         # print the per-module coverage table (writes nothing)
make cover-check   # the CI gate: fail if a module dropped below its floor
make cover-update  # re-record the baseline floor in coverage-baseline.txt
```

**What is measured.** For each module the script runs `go test -coverprofile`, then computes the module's aggregate statement coverage with `go tool cover -func` over a profile from which two kinds of non-production code are filtered out. First, **mechanically-generated code** — `zz_generated*.go` (controller-gen DeepCopy) and `groupversion_info.go` (scheme boilerplate); filtering these keeps the floor reflecting hand-written logic, so adding a CRD field (which grows `zz_generated`) can't trip the gate without a real test change. Second, **test-helper packages** — the `<pkg>test` external-helper convention (`broker/brokertest`) and anything under a `test/` helper tree (`gmc/test/utils`, the `test/fakegithub` module); these exist only to support other packages' tests, never ship in a binary, and folding their partial self-coverage into a module's floor made the ratchet track helper code (broker measured ~48% blended while its production package was ~80% — Q110). We deliberately **do not** exclude `main.go`: in this repo several binaries (`cmd/worker`, `cmd/proxy`) keep real, unit-tested logic in their `package main`, so a blanket entrypoint exclusion would hide tested logic and leave those modules ungated. The genuinely-thin entrypoints (`cmd/agc`, `cmd/gmc`) instead contribute a lower but still-defended floor — which costs the ratchet nothing, since a lower floor never causes a false failure.

**How it gates.** [`coverage-baseline.txt`](../../coverage-baseline.txt) records each module's floor. `make cover-check` fails only if a module drops **more than 0.5 percentage points** below its floor. Coverage is deterministic (the gate runs without `-race`), so this small tolerance is not for flake — it absorbs benign denominator drift (adding a couple of uncovered boilerplate lines marginally dilutes the ratio) while still catching a real regression (deleting a tested function, gutting a test) on any module of meaningful size. When coverage rises well above a floor, the gate prints a note suggesting `make cover-update`.

**Updating the floor.** When you intentionally add tests and coverage goes up, run `make cover-update` and commit the new `coverage-baseline.txt` — the ratchet then defends the higher number. Lowering a floor is allowed but lands as an explicit, reviewable diff in that file rather than silently. The current baseline (helper-package exclusion added in Q110):

| Module | Floor | Module | Floor |
|---|---|---|---|
| `broker` | 81.3% | `cmd/proxy` | 72.8% |
| `cmd/agc` | 78.1% | `cmd/worker` | 72.0% |
| `cmd/gmc` | 57.1% | `githubapp` | 82.6% |
| `cmd/probe` | 0.0% (no tests yet) | `test/fakegithub` | n/a (helper-only module) |

Like `make test-race` and `make vulncheck`, `cover-check` is **not** part of `make check`: it re-runs the unit tests a second time (with `-cover` instead of `-race`), so folding it into the fast local loop would double its test time. Run it when a change adds or removes tests, or before a final pre-PR pass. Like the other heavy targets it applies the [local throttle](#resource-auto-throttle-on-gui-dev-machines), so a run on a GUI dev machine stays desktop-safe; on CI the prefix is a no-op.

### The shellcheck gate

`make shellcheck` runs `shellcheck` over every tracked shell script under `scripts/` and is wired into `make check`, so the local pre-review gate matches CI. The dedicated `shellcheck` job in `.github/workflows/unit-test.yml` runs the same `make shellcheck` target, gated on a `scripts` paths-filter (`scripts/**`, the `Makefile`, and the workflow itself) so a scripts-only change doesn't also trigger the full Go lint.

**The CI job pins shellcheck (`v0.11.0`)** rather than using `ubuntu-latest`'s preinstalled copy — that version drifts with the runner image, and shellcheck's heuristics (e.g. when SC2015 fires on `A && B || true`) differ between releases, so an unpinned gate gives a different verdict locally vs. CI. Install the **same** version locally so `make shellcheck` matches the gate: see <https://github.com/koalaman/shellcheck#installing> (the target prints this hint if shellcheck is missing). When bumping the pin, update both the `SHELLCHECK_VERSION` env in the workflow and this paragraph.

The file set is the git pathspec `scripts/*.sh` resolved through `git ls-files` — **tracked-only and recursive**: git's default `*` spans `/`, so the one pathspec already covers a future `scripts/<subdir>/*.sh` without re-touching the gate, while untracked scratch scripts are skipped. This complements `actionlint`, which only lints the inline `run:` blocks in workflows; before this gate the standalone helper scripts (`setup.sh`, `kind-with-registry.sh`, …) shipped unlinted.

Accepted findings carry a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (see the dynamic-name `read`/`export` in `scripts/probe-investigations-cd.sh`); everything else is fixed to match the repo bash conventions listed in [`scripts/README.md`](../../scripts/README.md).

### The doc-link gate

`make doc-links` runs `scripts/check-doc-links.sh` over every tracked, non-vendored Markdown file and is wired into `make check`, so the local pre-review gate matches CI. CI runs the same `make doc-links` target from its **own** workflow, [`.github/workflows/doc-links.yml`](../../.github/workflows/doc-links.yml), scoped (via `on.paths`) to `**.md`, the checker, and the workflow itself. It is deliberately separate from `unit-test.yml` — that workflow path-ignores docs, so a docs-only change triggers only this lightweight check and never the Go suite (mirroring how `e2e-test.yml` is its own workflow).

It fails on two classes of breakage: **dead relative file links** (a `[text](path)` whose resolved target is neither a tracked file nor directory — a trailing `:NN` line reference is tolerated and only the file part is resolved) and **dead anchors** (a `#fragment` that matches no heading slug or explicit `<a id>`/`<a name>` in the target Markdown file). Anchors are resolved with GitHub's heading-slug algorithm (strip inline markdown — respecting code spans — lowercase, drop everything outside `[a-z0-9 _-]`, spaces to hyphens, de-dupe repeats with `-1`/`-2`), so the verdict matches what GitHub renders. External URLs (http/https/mailto/tel), links inside fenced or inline code, and anchors into non-Markdown or vendored targets are out of scope.

## Picking the right test tier

Prefer the narrowest tier that can actually *observe* the bug class — but no narrower:

- **Unit (fake client)** — pure logic and field-level behavior. The fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`) reproduces none of the real-apiserver semantics below, so a fake-client test cannot prove claims that depend on them.
- **envtest (integration)** — any claim that depends on real-apiserver semantics: schema/admission defaulting, server-side no-op-write dedup (the apiserver skips the `resourceVersion` bump when a patch's defaulted result is unchanged), admission/validation webhooks and CEL, and `IsConflict` handling. Both `cmd/agc` and `cmd/gmc` already have envtest suites at `internal/controller/integration/` (build tag `integration`, see [Integration tests](#integration-tests)) — add to them rather than concluding none exists; confirm with a directory listing before deciding a tier is missing. Example: PR #143 (Q65) migrated the GMC `apply*` helpers to `CreateOrPatch`; a fake-client test could verify field-level behavior, but only `apply_nochurn_test.go` (envtest, asserting `resourceVersion` stability across periodic reconciles) could prove the whole-`Spec` helpers don't churn.
- **Tier-A kind e2e** — behaviors that emerge from real CNI, kube-proxy DNAT, kubelet image-pull policy, or TLS-over-tunnel. When a feature crosses one of those boundaries, the Tier-A test (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests) and [End-to-end tests](#end-to-end-tests)) is the only thing that proves it works. Example: PR #59 fixed 5 bugs that all unit tests passed for — a single planned-but-unimplemented Tier-A test (`E2E_GMC_TenantProvisioning_ProxyConnectWorks`) would have caught 4 of them locally.

Before concluding a test failure is a code bug, check whether the problem is in the test expectations, the test setup, or the code itself — the intent of the test must match the implementation.

## Integration tests

Integration tests use envtest and are gated by the `integration` build tag. They live under `internal/controller/integration/` in both `cmd/agc` and `cmd/gmc`. Use the dedicated Makefile targets — they set `KUBEBUILDER_ASSETS` automatically:

```bash
make test-integration              # runs both cmd/agc and cmd/gmc integration tests
make -C cmd/agc test-integration   # AGC only
make -C cmd/gmc test-integration   # GMC only
```

Or manually, after building setup-envtest:

```bash
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.35 --bin-dir .build -p path)
(cd cmd/agc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
(cd cmd/gmc && go test -v -tags integration -timeout 5m -count=1 ./internal/controller/integration/...)
```

Unit tests (`make test` / `go test ./...`) do **not** require envtest — the integration packages are excluded by their `//go:build integration` tag.

### Avoiding shared-stub flakes in the AGC suite

The `cmd/agc` integration suite shares one broker stub (`brokertest.Server`, created once in `TestMain`) across every test in the package. Sessions other tests register stay in the stub's global maps, so the global accessors (`RegisteredSessions()`, `ActiveSessionCount()`) accumulate across the whole package. Picking a session from that global list — e.g. `RegisteredSessions()[len-1]` — can land a job on a session another test left active, which never spawns a worker pod in your namespace, so the test times out intermittently on a loaded CI runner (this flake class was Q91, Q113, Q120).

Two rules keep a new test deterministic:

- **Scope every session assertion and enqueue to your RunnerGroup's owner.** Use `ActiveSessionsForOwner("<rg-name>")` and `enqueueJobOnOwnerSession(...)` instead of the global accessors. A RunnerGroup name is unique to one test, so owner-scoping returns exactly the sessions you created — never a sibling's. `enqueueJobOnOwnerSession` also retries until an owner session is present, so it is immune to the picked session having just idle-shut.
- **Wait on the condition, not the clock.** Prefer the stub's channel-based waiters (`WaitForFirstPoll`, `WaitForSessionDelete`) over wall-clock sleeps; they return the instant the event happens. The timeout you pass is only a safety ceiling, not the expected latency — size it generously for a CPU-starved 2-vCPU CI runner (seconds of headroom, well inside the package's 5m test timeout), since raising a too-tight ceiling alone just moves a flake rather than fixing it.

## End-to-end tests

E2E tests run on a local `kind` cluster, are gated by the `//go:build e2e` tag, and live under `cmd/gmc/test/e2e/`. They split into three tiers (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests)):

- **Tier A** — GMC infrastructure (no GitHub required).
- **Tier B** — AGC lifecycle against the in-cluster `test/fakegithub/` server.
- **Tier C** — real GitHub workflow dispatch (requires App credentials).

Typical local run:

```bash
make e2e-cluster        # one-time: create the kind cluster
make e2e-images         # builds gmc/agc/proxy/worker/fakegithub, loads into kind
make e2e                # runs Tier A + B
make e2e-clean          # tear down when done
```

For iterating against a single spec without re-creating the cluster, see [kind-iteration.md](kind-iteration.md). It also covers pointing AGC at fakegithub vs. real GitHub via the `AGC_EXTRA_*` env vars and using `E2E_SKIP_TEARDOWN=true` to keep state between runs.

**Egress-enforcing CNI profile.** `make e2e-cluster KIND_CNI=calico` builds the cluster with Calico instead of kindnet (see [kind-iteration.md § CNI selection](kind-iteration.md#cni-selection-kindnet-default-vs-calico)). The two runtime egress-negative specs (`E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod`, `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI`) and the two manager metrics-NP specs (`E2E_GMC_ManagerMetricsNP_DeniesUnlabeledNamespace`, `E2E_GMC_ManagerMetricsNP_AllowsLabeledNamespace`) skip themselves on kindnet — whose enforcer does not drop egress — and only assert real packet drops on a Calico/Cilium cluster. Run them with the Calico profile when validating NetworkPolicy enforcement changes (Q7b/Q83). CI runs this profile per-PR whenever a change touches NetworkPolicy/proxy code — see [the Calico e2e lane](#the-calico-e2e-lane) below.

**Curl test image.** The connectivity, isolation, and metrics specs run a `curlimages/curl` pod. It defaults to the upstream Docker Hub ref (`curlimages/curl:8.10.1`), which is fine locally. CI sets `E2E_CURL_IMAGE` to a local-registry mirror (`localhost:5000/curlimages/curl:8.10.1`, populated by the workflow's mirror step) so the kind nodes never pull from Docker Hub — anonymous Hub rate limits (HTTP 429) were starving these pods and flaking three specs.

**Test labels and the `multi-node` suite.** Two Ginkgo labels partition the suite. CI runs the **full** suite — `make e2e` with no `SUITE`, so no `--label-filter` — on the default 2-worker cluster (`test/kind-config-2worker.yaml`), so every labelled spec runs in CI:

- `multi-node` — specs that need the 2-worker cluster shape to be meaningful: `E2E_GMC_ProxyPodScheduledOnWorker` (pod-to-worker placement), `E2E_GMC_PDBPreventsEvictionBelowMinAvailable` (PDB blocks eviction while a replica survives on another node), and `E2E_GMC_GMCRestartPreservesState`.
- `github-real` — the Tier C specs that dispatch against real GitHub (`E2E_GitHub_RealDispatch`); they self-skip when the `E2E_GITHUB_*` env vars are unset.

For a faster local inner loop on a 1-worker cluster, `make e2e SUITE=single-node` maps to `--label-filter '!multi-node'` and skips the multi-node specs; unset `SUITE` runs everything (matching CI). The HPA scale-up spec (`E2E_GMC_HPADrivesScaleUp`) is unlabelled and CI-safe: it patches `HPA.spec.minReplicas` to drive the HPA→Deployment control path deterministically rather than burning CPU to trigger autoscaling, so it runs everywhere.

**Waiting for the AGC, not just its Deployment.** A spec that waits for a broker session (or anything else that needs the AGC operational) must gate on `utils.WaitForRunnerGroupReconciled`, not only `utils.WaitForDeploymentReady`. Deployment readiness means only that the AGC's health server is up — it binds within seconds of pod start and is deliberately decoupled from the GitHub-App token fetch (`cmd/agc/main.go`), whose budget alone is up to ~2 minutes. `WaitForRunnerGroupReconciled` waits for `RunnerGroup.status.observedGeneration` to be set, which the AGC does only after token + agent registration + listener-multiplexer start all succeed. Gating on Deployment readiness alone folds the AGC's whole startup into the session wait's budget, which under parallel CI load (token/registration/session round-trips to the shared single-replica fakegithub) can exhaust it and surface as a misleading "no session registered" timeout (Q134).

**Tier C.** Set `E2E_GITHUB_APP_ID`, `E2E_GITHUB_APP_INSTALLATION_ID`, `E2E_GITHUB_APP_PRIVATE_KEY`, `E2E_GITHUB_ORG`, and `E2E_GITHUB_REPO` in the environment, then run `make e2e` (Tier C specs skip themselves at runtime when any variable is missing). The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

## CI workflows and scripts

CI must use the same per-module commands as [Running tests](#running-tests) above — never `go test ./...` from the repo root, which does not work with the Go workspace layout.

### The e2e workflows: kindnet and Calico

The cluster/image/test plumbing for the e2e suite lives in one reusable workflow, [`.github/workflows/e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`workflow_call`, parameterized by a `kind_cni` input). Two callers drive it so a kind bump, image-tag change, or flake mitigation is made once and both lanes inherit it:

- **[`e2e-test.yml`](../../.github/workflows/e2e-test.yml)** — the per-PR / push-to-main leg, `kind_cni: kindnet`. Path-gated (skips PRs touching no e2e-relevant files) and `cancel-in-progress` on PRs. This is the merge gate.

#### The Calico e2e lane

- **[`e2e-calico.yml`](../../.github/workflows/e2e-calico.yml)** — `kind_cni: calico` (Q119). kindnet accepts `NetworkPolicy` but its bundled enforcer does not drop egress, so the NetworkPolicy-enforcement specs self-skip on the per-PR kindnet leg; on Calico they assert real packet drops. The full suite runs on both CNIs — these specs simply activate under Calico: the two `TenantProvisioning` egress negatives (`WorkloadEgressBlockedToNonProxyPod`, `WorkerCannotReachK8sAPI`), `ProxyConnectWorks` (which runs on both but is only truly enforced here), and the two `ManagerMetricsNP` specs (Q83). No per-lane spec selection is needed — the suite's runtime `egressEnforcingCNI()` self-skip does the routing.

  **When it runs:** **per-PR (and on push to main) only when the diff touches NetworkPolicy/proxy code** — the GMC (`cmd/gmc/**`, which generates the tenant + manager policies and the proxy), the egress proxy (`cmd/proxy/**`), the chart's policy templates (`charts/actions-gateway/**`), or the CNI/cluster plumbing (`scripts/kind-with-registry.sh`, `Makefile`, the two e2e workflows). PRs that cannot regress enforcement stay on the fast kindnet leg and pay no Calico cost. The path filter is the *sole* automatic gate (there is no nightly catch-all), so it deliberately errs toward the components that produce or police the enforced traffic. **Trigger it manually** any time from the Actions tab → *e2e (calico)* → *Run workflow* (`workflow_dispatch`). Because it triggers on the PR's own files, a change to the lane itself (or to NP/proxy code) is validated on that PR rather than only post-merge.

  **It is not a required check.** With `on.<event>.paths`, the workflow simply does not trigger on unrelated PRs (no skipped check is reported), which is correct only because it does not gate merge. If it is ever made a required status, switch to the always-runs-then-skips `dorny/paths-filter` pattern `e2e-test.yml` uses so a non-matching PR still reports a green check.

  **Calico image caching.** The Calico manifest pulls `calico/node`, `calico/cni`, and `calico/kube-controllers` from quay.io/docker.io on every node during install — and those pulls happen *before* the local registry is wired into the nodes, so they cannot be mirrored the way the curl image is. Instead the lane pre-pulls the exact image refs the pinned manifest references into the runner's Docker daemon (cached via `actions/cache`, keyed on `CALICO_VERSION`, retried), and `scripts/kind-with-registry.sh` `kind load`s whatever is present onto the nodes so the rollout never touches quay.io. This keeps the per-PR Calico cost bounded and quay.io off the critical path. Calico still gets a 60-minute timeout vs. the kindnet leg's 45 for rollout headroom. `CALICO_VERSION` is pinned in both the root `Makefile` and the workflow env — bump them together.

### The Dockerfile-lint gate

[`.github/workflows/dockerfile-lint.yml`](../../.github/workflows/dockerfile-lint.yml) runs `hadolint` over all five Dockerfiles (a matrix leg each), path-gated on `**/Dockerfile`. The failure threshold is `style` — the strictest level, which all five currently pass clean — so a regression such as an unpinned base tag, a dropped digest pin, or a relaxed non-root `USER` fails at PR time. It is its own lightweight workflow (like `doc-links.yml` and `status-lint.yml`), so a Dockerfile-only change does not trigger the Go suite. There is no local `make` target; reproduce a run with `docker run --rm -i hadolint/hadolint hadolint --failure-threshold style - < cmd/gmc/Dockerfile`.

## Security scanning

The `security-scan.yml` workflow runs three gates on every PR (and on push to `main`), independent of the unit/integration/e2e suites — two supply-chain scans plus a Kubernetes posture scan. All three have local equivalents so you can reproduce a CI verdict before pushing.

**govulncheck** — scans each workspace module for vulnerabilities reachable from our code (Go stdlib + dependency CVEs). It is symbol-precise: a CVE in a dependency only fails the gate if our code actually calls the affected path. Run it locally with:

```
make vulncheck
```

A finding usually means bumping the Go toolchain (`go` directive in `go.work` + every `go.mod`, kept in lockstep) for a stdlib CVE, or `go get`-ing the fixed dependency version for a module CVE.

**trivy** — builds each of the five images and scans it for fixable HIGH/CRITICAL CVEs in OS packages and bundled libraries. Run it locally (requires `trivy` and `docker` on `PATH`) with:

```
make trivy-scan
```

The four images we build from a minimal/distroless base (`gmc`, `agc`, `proxy`, `fakegithub`) **block** the gate — every package in them is one we chose, so a finding is actionable by bumping a dependency or the base digest. The `worker` image is built `FROM` the upstream `ghcr.io/actions/actions-runner` and inherits CVEs in the bundled node20 runtime and the runner's own Go binaries that we cannot fix without forking the runner; its leg is **report-only** (findings printed, never blocks). Runner-base CVEs are reduced by bumping the pinned tag — automated via the `docker` ecosystem in `dependabot.yml` and tracked in [`STATUS.md`](../STATUS.md) Q70.

The same `trivy` job also generates an **SBOM** (Software Bill of Materials, SPDX-JSON, via [`syft`](https://github.com/anchore/syft)) for each image it builds and uploads it as a `sbom-<image>.spdx.json` build artifact. This runs on every code PR purely so the SBOM-generation path can't silently break before a release — it does **not** sign or publish anything. On a `v*` release tag, the separate [`publish.yml`](../../.github/workflows/publish.yml) workflow pushes the four first-party images to GHCR, regenerates each SBOM for the pushed image, signs every image **keyless** with [`cosign`](https://docs.sigstore.dev/) (sigstore/Fulcio via GitHub Actions OIDC — no signing key or stored secret), and attaches the SBOM as a keyless cosign attestation. Operator-facing verification (`cosign verify`, SBOM retrieval) is documented in [security-operations.md § Image provenance](../operations/security-operations.md#image-provenance-signature--sbom-verification). The signing/attestation steps run only on publish, so PR CI does not exercise them.

**polaris** — audits the Kubernetes security/best-practice posture of the **shipped install artifact**: it renders the [Helm chart](../../charts/actions-gateway) (digest-pinned, matching the production posture) and checks the rendered manifests. The gate **fails on `danger` findings only** (privileged container, host namespace, dangerous capabilities, missing `securityContext`, a floating `:latest` image tag) — a real posture regression in the chart cannot merge — while `warning`s are reported for visibility. False-positive warnings against a Helm-packaged operator chart are tuned to `ignore` in [`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml) (via `--merge-config`, so every default `danger` check stays active), each with a justifying comment. Run it locally (requires `helm` and `polaris` on `PATH`) with:

```
make polaris-scan
```

This `polaris` job is path-gated on the chart (and `Makefile`). The operator-facing writeup — including the manual `kube-bench` CIS scan that complements polaris at the live-cluster layer — is in [security-operations.md](../operations/security-operations.md#posture-scanning-preventive).

The three gates are path-gated (they skip when a PR touches only unrelated files); the two Go scans use `go-version-file: go.work`, so the toolchain version flows automatically.

## Install-artifact validation

The `manifest-validate.yml` workflow checks that the **shipped install artifact** — the [`actions-gateway` Helm chart](../../charts/actions-gateway), the sole install path (Q142) — is well-formed and schema-valid, so a malformed RBAC/CRD/policy file cannot merge silently. It is independent of the security gates above (validity, not posture) and path-gated on the manifests, the chart, and the `Makefile`. Run the exact gate locally (requires `yamllint`, `kubeconform`, and `helm` on `PATH`) with:

```
make manifest-validate
```

It first runs the chart CRD/RBAC drift gates (`make chart-crds-check` + `make chart-rbac-check`: the chart's CRD templates and `manager-role` rules are generated from the controller-gen sources under `cmd/*/config/`, so a marker change that isn't propagated fails here), then runs two layers over `cmd/*/config/**` and [`charts/actions-gateway`](../../charts/actions-gateway):

- **yamllint** lints the `controller-gen` YAML and the chart metadata against [`.yamllint.yaml`](../../.yamllint.yaml). The config targets real defects (tabs, trailing whitespace, duplicate keys, a missing final newline, truthy typos) and relaxes the purely cosmetic rules that would only ever fire on machine-generated style — `line-length` (CRD `description` lines are verbatim Go doc comments well over 200 chars) and `indentation` (the generated YAML mixes block-sequence indent styles). Helm templates are excluded — they embed `{{ ... }}` and are not parseable YAML; their rendered output is validated below instead.
- **kubeconform** schema-validates against the cluster API at the chart's `kubeVersion` floor (1.30.0 — validating the oldest supported version catches a field that does not exist there): the controller-gen manifests + the two ValidatingAdmissionPolicies under `cmd/*/config/` (the codegen + envtest substrate; there is no longer a kustomize overlay to render), and `helm template` output in digest-pinned, dev/test opt-out (`allowFloatingImageTags=true`), and all-optional-features form, plus `helm lint` on the chart and a fail-closed check that rendering with pure default values is **rejected** (`gmc.image.digest` is required — Q96 secure-by-default; the check fails if that rejection ever stops happening). `-ignore-missing-schemas` skips only third-party/custom kinds whose schema is not in the upstream Kubernetes set (cert-manager `Certificate`/`Issuer`, the Prometheus Operator `ServiceMonitor`, and our own `ActionsGateway`/`RunnerGroup` CRs); the `CustomResourceDefinition`s that define them **are** validated, since that is a native `apiextensions` kind.

The tool versions are pinned in the workflow (`KUBECONFORM_VERSION`, `YAMLLINT_VERSION`); bump them deliberately, since a new kubeconform can change validation behaviour. CI persists kubeconform's downloaded JSON schemas in an `actions/cache` keyed on the validated Kubernetes version so runs do not re-fetch the schema set from GitHub.
