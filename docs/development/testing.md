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

For the one-command gate before requesting review, run `make check` from the repo root. It runs gofmt, `golangci-lint`, the `docs/STATUS.md` format lint, and the (plain) unit tests across every module. This is the fast local loop and covers the lint and unit-test *logic* the `.github/workflows/unit-test.yml` workflow enforces. The one CI step `make check` does **not** reproduce is the race detector: the CI `unit-test` job runs the same per-module unit tests under `-race` (see [the race gate](#the-race-detector-unit-gate) below), which roughly doubles their runtime. Reproduce that locally with `make test-race` — kept out of `make check` so the default dev gate doesn't become an unthrottled `-race` run. The slower security gates (`make vulncheck`, `make trivy-scan`) and the integration/e2e tiers below stay separate too so this loop stays fast.

Test output is non-verbose by default: `go test` prints one `ok <pkg>` line per passing package and the full output of any package that fails (compress success, expand failure). When debugging a **slow or hanging** test, add `V=1` (`make check V=1` or `make test V=1`) to stream output live — without `-v`, `go test` buffers each package's output until the package completes, so a hung test shows nothing (not even its `t.Log` lines) until it finishes or hits `-timeout`.

A sub-second subset (gofmt on staged Go files + the STATUS.md lint) also runs automatically at commit time via the tracked pre-commit hook in `.githooks/`. Install it once with `make hooks` (or `scripts/setup.sh`); bypass a single commit with `git commit --no-verify`.

#### Resource auto-throttle on GUI dev machines

`make lint`/`make test`/`make check` lint each module with `golangci-lint` (which fans out one worker per logical CPU and ignores `GOMAXPROCS`/`GOFLAGS`) and run `go test` across every module. On a small machine this can saturate every core and make the desktop unresponsive. On macOS it is worst: the WindowServer compositor misses its kernel watchdog and restarts — the whole GUI freezes (it shows up as `WindowServer … userspace_watchdog_timeout` in **Console ▸ Crash Reports**). On a Linux/WSL desktop you instead get input lag and compositor stutter while the build runs.

To prevent that, the Makefile auto-throttles these phases on an **interactive, GUI-bearing dev shell**: it runs them at a low-priority QoS tier that demotes both CPU **and** disk I/O below the desktop (macOS: `taskpolicy -c utility`; Linux/WSL: `nice -n 19`, plus `ionice -c 3` when available), and caps parallelism to physical-cores − 2 (`golangci-lint -j`, `go test -p`, `GOMAXPROCS`). Detection and sizing live in [`scripts/local-throttle.sh`](../../scripts/local-throttle.sh).

On macOS the I/O demotion matters as much as the CPU demotion: an unthrottled build already runs at a lower QoS than WindowServer yet still trips the watchdog, so the fix is throttling the build's I/O so the compositor's I/O isn't stuck behind it — and `taskpolicy` is the only macOS way to express that (there is no `ionice`). The gentler `utility` tier is used rather than the lowest `background`/`-b` band because it delivers the same protection while letting builds finish 2–4× faster.

The Makefile only throttles its own recipes, so a bare `go build`/`go test` run directly (not via `make`) bypasses it — a heavy `-race` run that way once froze the macOS GUI. Two safety nets cover that gap, both reusing `scripts/local-throttle.sh` so they share the same activation rules and stay no-ops on CI/headless/SSH:
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

**Curl test image.** The connectivity, isolation, and metrics specs run a `curlimages/curl` pod. It defaults to the upstream Docker Hub ref (`curlimages/curl:8.10.1`), which is fine locally. CI sets `E2E_CURL_IMAGE` to a local-registry mirror (`localhost:5000/curlimages/curl:8.10.1`, populated by the workflow's mirror step) so the kind nodes never pull from Docker Hub — anonymous Hub rate limits (HTTP 429) were starving these pods and flaking three specs.

**Local-only tests.** Two Tier-A tests are excluded from CI by the `--label-filter '!local-only'` flag:

- `E2E_GMC_HPA_ScalesUpUnderLoad` — needs sustained CPU load to trigger autoscaling; flaky on 2-vCPU runners where the load generator and proxy pods compete for the same cores.
- `E2E_GMC_PDB_PreventsEvictionBelowMinAvailable` — uses `kubectl drain`, whose eviction timing becomes flaky under CPU contention.

Both pass reliably on a local machine with more cores. To run them locally, drop the label filter from the `make e2e` invocation or invoke `ginkgo` directly.

**Tier C.** Set `E2E_GITHUB_APP_ID`, `E2E_GITHUB_APP_INSTALLATION_ID`, `E2E_GITHUB_APP_PRIVATE_KEY`, `E2E_GITHUB_ORG`, and `E2E_GITHUB_REPO` in the environment, then run `make e2e` (Tier C specs skip themselves at runtime when any variable is missing). The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

## CI workflows and scripts

CI must use the same per-module commands as [Running tests](#running-tests) above — never `go test ./...` from the repo root, which does not work with the Go workspace layout.

## Security scanning

The `security-scan.yml` workflow runs two supply-chain gates on every PR (and on push to `main`), independent of the unit/integration/e2e suites. Both have local equivalents so you can reproduce a CI verdict before pushing.

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

Both gates are path-gated (they skip when a PR touches only docs/non-code files) and use `go-version-file: go.work`, so the toolchain version flows automatically.
