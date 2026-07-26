# Agent reference: Testing

## Running tests

The repo is a Go workspace (`go.work`), so `go test ./...` from the repo root does **not** work — run tests per module, or use explicit per-module patterns from the root (`go test ./broker/... ./cmd/agc/...`), which the workspace does resolve. See [go-workspaces.md](go-workspaces.md) for why. `make test` runs the whole workspace as **one** multi-module `go test` invocation (all `./<module>/...` patterns at once) so the modules compile and test as a single parallel build graph instead of serially (Q17).

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

### The inner loop: cheap checks while iterating, `make check` once pre-PR

Run the full gate **once, before opening the PR**. While iterating, use the two cheap checks that cover what you actually changed:

```bash
make lint
```

```bash
(cd cmd/agc && go test ./...)
```

`make lint` is change-scoped locally — gofmt over every module, `golangci-lint` only over the modules the branch can affect (see [Change-scoped lint on local runs](#change-scoped-lint-on-local-runs)), and it skips the heavy-build lock entirely when a change has no Go-lint effect. Pair it with the **module-scoped** unit tests for the module you touched; [Running tests](#running-tests) above lists the invocation for each one. This is a Go workspace, so `go test ./...` from the repo root does not work — the `(cd <module> && …)` form is the one that does. Narrow further while chasing a single failure:

```bash
(cd broker && go test -run TestSessionMux ./...)
```

Then, once, when the work is done and you are about to open the PR:

```bash
make check
```

**Why the split.** `make check`'s heavy phases take a [machine-wide heavy-build slot](#resource-auto-throttle-on-gui-dev-machines), of which there are only a couple, so repeat runs — yours plus every sibling worktree session's — queue behind each other once the slots are full. Over a 21-day sample of session transcripts, a session averaged 3–4 full runs at a median of 2 minutes but a **p90 of 9 minutes**, the tail being lock wait (Q375). Each extra run also mostly re-verifies code the branch never touched: the unit/coverage step ([`make cover-check`](#coverage-measurement-and-the-ratchet)) measures **every** workspace module regardless of change scope, so only the lint phase gets any benefit from scoping. Running it three times mid-iteration buys almost nothing the module-scoped loop above did not already tell you.

Add a heavier tier only when the change warrants it — `make test-race` for the concurrency core, `make test-integration` or the e2e targets for controller and cluster behaviour, `make vulncheck`/`make trivy-scan` for dependency and image changes — and give each one an [explicit timeout or a background run](#slow-tiers-need-an-explicit-timeout-or-a-background-run).

One caveat on the inner loop: the `make` targets throttle themselves, but a bare `go test` run directly does not. That is harmless for a plain module-scoped run and **not** harmless with `-race` — use `make test-race`, or prefix a manual run with `$(scripts/local-throttle.sh prefix)`. See [Resource auto-throttle on GUI dev machines](#resource-auto-throttle-on-gui-dev-machines).

### The `make check` pre-review gate

For the one-command gate before requesting review, run `make check` from the repo root. It runs gofmt, `golangci-lint`, the `docs/STATUS.md` format lint, the [roadmap/backlog coherence gate](#the-roadmap-coherence-gate) (`make roadmap-check`), the plan-index/no-plan-refs drift gates (`make plan-index-check no-plan-refs-check`, which assert active plans in `docs/plan/README.md` are still STATUS-referenced and that Go code cites no `docs/plan/` paths), the single-Go-version gate (`make go-version-check`, which asserts the `go` directive matches across `go.work`, every `go.mod`, and every `go.work.gen`), the [v2 API sync gate](#the-v2-api-sync-gate) (`make v2-api-sync-check`), the [build-tag gate](#the-build-tag-gate) (`make build-tags-check`, which compiles and vets the `integration`/`e2e`/`load`-tagged files no other fast gate builds), the [path-filter gate](#the-path-filter-gate) (`make path-filters-check`, which reconciles CI's `dorny/paths-filter` lists with `go.work`), `shellcheck` over the helper scripts (see [the shell-lint gate](#the-shellcheck-gate) below), the chart CRD/RBAC/webhook drift gates (`make chart-crds-check chart-rbac-check chart-webhook-check`, which fail if the Helm chart's CRD/RBAC/webhook templates drifted from their sources), the scripts behavioural tests (`make scripts-test`), the Markdown link/anchor check (see [the doc-link gate](#the-doc-link-gate) below), and the unit tests with the coverage ratchet ([`make cover-check`](#coverage-measurement-and-the-ratchet) below, which supersets `make test` — the same unit-test packages, run once per module with `-cover`, plus the per-module coverage floor). It covers the lint, unit-test *logic*, and coverage gates the `.github/workflows/unit-test.yml` + `coverage` CI jobs enforce — run it once when the work is done, not after every edit ([the inner loop](#the-inner-loop-cheap-checks-while-iterating-make-check-once-pre-pr) above is what you iterate with). The one CI step `make check` does **not** reproduce is the race detector: the CI `unit-test` job runs the same per-module unit tests under `-race` (see [the race gate](#the-race-detector-unit-gate) below), which roughly doubles their runtime. Reproduce that locally with `make test-race` — kept out of `make check` so the default dev gate doesn't become an unthrottled `-race` run. The slower security gates (`make vulncheck`, `make trivy-scan`, `make polaris-scan`), the [install-artifact validation](#install-artifact-validation) (`make manifest-validate`), and the integration/e2e tiers below stay separate too so this loop stays fast.

`make check` also **does not** run the three dependency-drift gates — `make vendor-check`, `make tidy-check`, and `license-notices` — because two of them re-fetch modules and can hit the network on a cold cache, which would tax every run to catch a class that lands on ~4% of commits. CI runs all three as their own jobs ([`unit-test.yml`](../../.github/workflows/unit-test.yml) `vendor-check`/`tidy-check`, [`license-notices.yml`](../../.github/workflows/license-notices.yml)), path-gated on the dependency files. The consequence to keep in mind: **a green `make check` does not imply a green `unit-test.yml` when a change touches `go.mod`/`go.sum`/`vendor/`/`go.work*`** — the drift gates can still fail on push. So after any dependency change, run `make vendor-sync` (the one-shot remedy) and commit the result before pushing; see [go-workspaces.md § Changing dependencies](go-workspaces.md#changing-dependencies). As a backstop, `make check` prints a one-line reminder (via `scripts/check-dep-advisory.sh`, its last step) whenever the change it sees touches a dependency file — advisory only, it never fails the gate.

**Run order.** The cheap gates (everything except `lint` and `cover-check`) take no heavy-build slot and are independent, so `make check` runs them concurrently through [`scripts/run-parallel.sh`](../../scripts/run-parallel.sh) and reports them first — a `docs/STATUS.md` format slip surfaces in seconds instead of waiting out the unit suite. Every line is prefixed with its gate's label, so a failure stays attributable. (`make -j` is not used: macOS ships GNU make 3.81, which has no `-O` output sync, so two failing gates would interleave unreadably.) The heavy phases then run in sequence, each taking a slot of its own.

Test output is non-verbose by default: `go test` prints one `ok <pkg>` line per passing package and the full output of any package that fails (compress success, expand failure). When debugging a **slow or hanging** test, add `V=1` (`make check V=1` or `make test V=1`) to stream output live — without `-v`, `go test` buffers each package's output until the package completes, so a hung test shows nothing (not even its `t.Log` lines) until it finishes or hits `-timeout`.

A sub-second subset (gofmt on staged Go files + the STATUS.md lint) also runs automatically at commit time via the tracked pre-commit hook in `.githooks/`. Install it once with `make hooks` (or `scripts/setup.sh`); bypass a single commit with `git commit --no-verify`.

#### Resource auto-throttle on GUI dev machines

`make lint`/`make test`/`make check` lint each module with `golangci-lint` (which fans out one worker per logical CPU and ignores `GOMAXPROCS`/`GOFLAGS`) and run `go test` across every module. On a small machine this can saturate every core and make the desktop unresponsive. On macOS it is worst: the WindowServer compositor misses its kernel watchdog and restarts — the whole GUI freezes (it shows up as `WindowServer … userspace_watchdog_timeout` in **Console ▸ Crash Reports**). On a Linux/WSL desktop you instead get input lag and compositor stutter while the build runs.

To prevent that, these phases auto-throttle on an **interactive, GUI-bearing dev shell**: the scripts behind the make targets (`scripts/go-test.sh`, `scripts/go-lint.sh`, `scripts/coverage.sh`) run them at a low-priority Quality of Service (QoS) tier that demotes both CPU **and** disk I/O below the desktop (macOS: `taskpolicy -c utility`; Linux/WSL: `nice -n 19`, plus `ionice -c 3` when available), and cap parallelism to physical-cores − 2 (`golangci-lint -j`, `go test -p`, `GOMAXPROCS`). Detection and sizing live in [`scripts/local-throttle.sh`](../../scripts/local-throttle.sh).

On macOS the I/O demotion matters as much as the CPU demotion: an unthrottled build already runs at a lower QoS than WindowServer yet still trips the watchdog, so the fix is throttling the build's I/O so the compositor's I/O isn't stuck behind it — and `taskpolicy` is the only macOS way to express that (there is no `ionice`). The gentler `utility` tier is used rather than the lowest `background`/`-b` band because it delivers the same protection while letting builds finish 2–4× faster.

The parallelism cap bounds **one** run's fan-out, but it is blind to siblings: several worktree/sessions each running `make check` (or `make lint`/`make test`) at once still collectively saturate a small core count, and then every phase stretches — most visibly `golangci-lint`, which counts the wait for its own parallel-runner lock against its deadline and starts reporting timeouts. So the heavy phases also take one of **N machine-wide advisory slots** (`serialize_heavy_build` in [`scripts/lib/common.sh`](../../scripts/lib/common.sh), paths from `scripts/local-throttle.sh lockfile [N]`, count from `scripts/local-throttle.sh slots`): N runs proceed, the rest queue, rather than every run trampling the others. The lock files live in the per-user cache dir (`~/Library/Caches/github-actions-gateway/` on macOS, `${XDG_CACHE_HOME:-~/.cache}/github-actions-gateway/` on Linux), **outside** any worktree, so the main checkout and every `.claude/worktrees/*` clone coordinate on the same files. They are implemented with `perl`'s `flock` — an advisory lock present on both macOS (which ships no `flock(1)`) and Linux, released automatically when the holder dies, so a Ctrl-C'd build never strands a stale lock. Like the throttle itself it activates only on a GUI dev shell; CI/headless/SSH report no lock file and run fully parallel and unserialized. (The `golangci-lint` `run.timeout` in `.golangci.yml` was also raised from 5m to 10m so a run that *does* queue behind a sibling has slack; CI is uncontended and never approaches it.)

**N defaults to 2** (1 on a machine with fewer than 4 physical cores); `GAG_HEAVY_BUILD_SLOTS` overrides it, and `GAG_HEAVY_BUILD_SLOTS=1` restores the original strict "one heavy run machine-wide" behaviour. Two holders at `jobs` each can oversubscribe the physical cores, and that is the intended trade: the desktop-safety property is the QoS demotion — CPU *and* I/O below the compositor — which every holder still carries, while the parallelism cap is a secondary bound. Strict serialization made the gate, not the work, set the pace on a machine running several sessions: one run used `jobs` threads while every sibling blocked for its whole duration (waits up to 5 h were observed — Q376). Slot 1 keeps the original single-lock filename so a worktree still on the pre-semaphore code contends with slot 1 rather than running unbounded alongside the new code. Rationale and measurements: [local-gate-throughput.md](../plan/local-gate-throughput.md).

Only the make targets (via their scripts) throttle themselves, so a bare `go build`/`go test` run directly (not via `make`) bypasses it — a heavy `-race` run that way once froze the macOS GUI. Two safety nets cover that gap, both reusing `scripts/local-throttle.sh` so they share the same activation rules and stay no-ops on CI/headless/SSH:
- **When you call `go` directly, prefix it** with `$(scripts/local-throttle.sh prefix)` (e.g. `$(scripts/local-throttle.sh prefix) go test -race ./...`), or just run it under `make` where a target exists.
- A Claude Code `PreToolUse` hook ([`scripts/claude-go-throttle-hook.sh`](../../scripts/claude-go-throttle-hook.sh), wired in `.claude/settings.json`) automates that prefix for agent-run commands: a bare `go build`/`go test` is rewritten transparently and auto-*allowed*. It auto-allows only that bare form — never a compound command (`cd … && go test …`) or one with a redirect — so its `allow` can't carry another segment or an outside-workspace redirect past the permission system or the branch-guard/workspace-guard hooks. But the throttle still has to reach the dangerous `-race` amplifier in those forms, so a compound/redirected command carrying `-race` is **rewritten to insert the prefix before its `go` invocation and returned as an `ask`** (not `allow`): the run is throttled while the confirmation prompt keeps the user and the guard hooks in the loop. Only when the hook can't pin down a single `go build`/`go test` token to prefix (more than one invocation, or a shape it can't parse) does it **`deny`** — with the specific reason — rather than throttle the wrong token and leave `-race` running unthrottled; there, add the prefix manually. The hook's behaviour is covered by [`scripts/claude-go-throttle-hook-test.sh`](../../scripts/claude-go-throttle-hook-test.sh) (run under `make scripts-test`).

It is a no-op everywhere the throttle would only slow things down for no benefit, so those runs go at full speed:
- **CI** — the `CI` environment variable is set (GitHub Actions et al.).
- **Headless / SSH Linux shells** — no graphical session (`DISPLAY`/`WAYLAND_DISPLAY` unset), so build servers and remote shells are unaffected.
- **Unsupported OSes** — native Windows (Git Bash/MSYS); use WSL2, which reports as Linux and follows the Linux rule.

To opt out locally (e.g. a machine with cores to spare), set `CI=1` for the run: `CI=1 make check`.

##### Not every WindowServer watchdog crash is a build

The throttle addresses one specific cause of `WindowServer … userspace_watchdog_timeout`: a build saturating CPU **and** disk I/O so the compositor's own work is stuck behind it. That is a *resource-starvation* stall — WindowServer's main thread is runnable but can't get serviced. There is a second, unrelated cause that the throttle does **not** fix, and the two look identical in **Console ▸ Crash Reports** (same `userspace_watchdog_timeout` suffix), so confirm which one you hit before assuming a build was at fault:

- **GPU/compositor stall (integrated-graphics contention).** On a Mac with integrated graphics (e.g. the `MacBookPro16,2` 13" with Intel Iris Plus, shared-memory VRAM), WindowServer's main thread can *block* waiting on the GPU/display pipeline to return a frame — not starve for CPU. The spin report's reason reads `Display … not ready: DisplayID: 0x…`, WindowServer's own CPU time in the window is tiny (well under 1 s), and the sampled kernel threads name the GPU stack (`AppleIntelICLGraphicsMTLDriver`, `AppleIntelFramebuffer`, `AppleGPUWrangler`, `IntelAccelerator`). The driver here is many simultaneous GPU clients on one weak iGPU: each Chromium/Electron app runs its own GPU process (`CrGpuMain`/`GpuWatchdog` — Claude desktop, Chrome, Slack, Discord, VS Code/GoLand), a Virtualization.framework VM adds a `virtio-gpu` client, and a Spotlight (`mds`) reindex piles on. No `go` process need be involved, and memory/swap can be near-idle. The throttle wrapper cannot help — it only demotes CPU/I/O, not GPU command-queue pressure.

  To tell them apart, read the spin file in `/Library/Logs/DiagnosticReports/WindowServer_*.spin`: a *build* stall shows WindowServer hot or its work blocked behind heavy I/O; a *GPU* stall shows the `Display … not ready` reason and the Intel/GPU driver threads above. Mitigate the GPU case by reducing concurrent GPU clients (close unused Electron apps, shut down the VM if headless, let Spotlight finish or exclude worktrees/module caches/Docker data from indexing); a reboot resets the accumulated `N induced crashes` counter.

#### Change-scoped lint on local runs

On a local run, `make lint` (and therefore the lint step of `make check`) scopes `golangci-lint` to the modules the change can actually affect: the modules owning files changed vs the `origin/main` merge-base — committed, uncommitted, and untracked — plus every workspace module that depends on one of them, transitively (the dependency edges come from the workspace-local `replace` directives in each `go.mod`; a dependency change can break a dependent's typecheck and therefore its lint). The gofmt check always covers every module, and a change that touches nothing with Go-lint effect (docs-only, charts-only) skips `golangci-lint` entirely — without queueing on the machine-wide heavy-build lock, which `scripts/go-lint.sh` now takes only when there is real lint work.

Why this is sound: an unscoped module lints byte-identically to the merge-base commit — same sources, same `.golangci.yml`, same `tools/`-pinned linter — and that commit is on `main`, where CI's full sweep is a required check. Anything that breaks that equivalence forces the full sweep instead: a change to `go.work`/`go.work.sum`, `.golangci.yml`, `vendor/`, `tools/`, or the scoping machinery itself (`scripts/go-lint.sh`, `scripts/lib/common.sh`, `scripts/local-throttle.sh`), and likewise whenever no `origin/main` merge-base resolves. CI always runs the full sweep (the `CI` env var forces it), so the PR gate is unaffected. Force it locally with `LINT_ALL=1 make lint`. The scoping decision itself is pure bash asserted by `scripts/go-lint-scope-test.sh` under `make scripts-test`.

The point is wall-clock under contention: most branches touch one or two modules, `make lint` is the target you re-run most in the [inner loop](#the-inner-loop-cheap-checks-while-iterating-make-check-once-pre-pr), and every full sweep holds the [machine-wide heavy-build lock](#resource-auto-throttle-on-gui-dev-machines) while sibling worktree sessions queue behind it. Scoping shrinks both the run and the lock hold.

#### Build and lint caches across worktrees

Parallel sessions each run `make check` from their own `.claude/worktrees/*` clone, which raised the question of whether every fresh worktree pays a cold cache (Q343). Measured (2026-07): **the two big caches are already machine-shared and path-independent at their defaults — do not repoint them.** Setting `GOCACHE`/`GOLANGCI_LINT_CACHE` to per-repo or per-worktree dirs was the proposed remedy and is a measured no-op (per-worktree dirs would actively *lose* sharing).

- **Go build cache** (`GOCACHE`, default `~/Library/Caches/go-build` on macOS, `~/.cache/go-build` on Linux). Compile artifacts are content-keyed and hit across worktree paths: compiling the `broker` module against an empty cache took ~12 s in one worktree and ~0.6 s in a second worktree sharing that same cache.
- **golangci-lint analysis cache** (`GOLANGCI_LINT_CACHE`, default `~/Library/Caches/golangci-lint` / `~/.cache/golangci-lint`). Also shared and path-independent for the expensive analysis: after a content change, linting `cmd/agc` costs ~2 min *once machine-wide*; the next worktree at the same content pays only ~9 s of per-path overhead (`go list`/export-data regeneration), and an in-place rerun ~2 s. Entries are even shared between `.build/golangci-lint` binaries built with different Go patch versions — there is no per-binary cache salt.

  **The sharing has one failure mode: a cached entry keeps the path of the worktree that produced it, and worktrees get deleted.** When that happens the post-processors cannot read the source they are reporting on, and the run emits `failed to parse file … no such file or directory` warnings followed by *contradictory* findings — observed 2026-07 as a simultaneous `G204: Subprocess launched with variable` **and** `directive //nolint:gosec … is unused for linter "gosec"` on the same two lines, in a file whose directive was present and correct. If a finding cites a path under a `.claude/worktrees/*` directory that no longer exists, do not chase the code: run `.build/golangci-lint cache clean` and re-lint.

The **test-result** cache is the third one, and it is path-keyed *at the default flags* — a result cached in one worktree does not hit from another even at an identical commit. Q343 concluded from that there was "no supported knob to share test results across paths"; there is one, and the unit tier now passes it (2026-07):

- **`-trimpath` makes the result cache path-independent.** The flag removes the absolute worktree path from the test binary, so the cache key depends on content alone. Measured on `cmd/agc` with `-coverprofile`: **226 s cold in one worktree, 5 s on the first-ever run in a second worktree** (12 packages cached), emitting a byte-identical coverage profile — so the ratchet reads the same number either way. `broker` reproduces it: without the flag the sibling worktree re-runs, with it the sibling prints `(cached)` immediately. [`scripts/go-test.sh`](../../scripts/go-test.sh) and [`scripts/coverage.sh`](../../scripts/coverage.sh) pass it, so a fresh worktree at an already-tested commit re-runs only the packages whose content actually changed.
- **Do not promote it to a global `GOFLAGS`.** [`cmd/gmc/test/e2e`](../../cmd/gmc/test/e2e/e2e_suite_test.go) resolves the v2 CRD chart directory from `runtime.Caller(0)`, which a trimmed path breaks. The unit tier does no such thing, and the release images already build with `-trimpath`.
- **Two cosmetic consequences.** Adopting the flag changes the build-cache keys, so the first unit run after it lands recompiles from scratch once, machine-wide. And a failing test's stack frames read as module-relative paths (`github.com/actions-gateway/…/listener.go:42`) rather than absolute ones, so they are no longer click-to-open in some terminals.

What a fresh worktree still pays, and why it stays:

- **Tool builds.** Each worktree builds its own `.build/golangci-lint` (~16 s with a warm build cache). Sharing tool binaries across worktrees would need version-keyed storage to avoid silently running a stale binary after a `tools/` dependency bump — complexity that isn't worth ~16 s per worktree.

### The race-detector unit gate

The CI `unit-test` job runs the workspace unit tests under Go's race detector (`go test -race`), not plain `go test`. The multiplexing core — agentpool, listener/mux, broker, token — is where data races hide, and plain `go test` never flags them; `-race` is pass/fail (a detected race fails the job). This is the only `unit-test.yml` step `make check` does not mirror, because `-race` instruments every memory access and roughly doubles unit runtime.

Reproduce the CI race gate locally with:

```bash
make test-race        # one multi-module `go test -race` across the whole workspace
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
| `cmd/probe` | 45.4% (compat suite) | `test/fakegithub` | n/a (helper-only module) |

Unlike `make test-race` and `make vulncheck`, `cover-check` **is** the unit-test step of `make check`: it supersets `make test` — the same unit-test packages (streamed as `ok <pkg>` lines, honouring `V=1`), just run per module with `-cover` and then gated against the floors — so the local gate runs the suite a single time, not twice, and never lets a coverage regression slip past a green `make check`. It carries the same [local throttle](#resource-auto-throttle-on-gui-dev-machines) and machine-wide serialize lock as `make test`, so a GUI run stays desktop-safe; on CI the prefix is a no-op. `make test` remains the no-coverage target for the fastest inner loop, and `make cover-check` runs standalone when you just want the ratchet.

### The shellcheck gate

`make shellcheck` runs `shellcheck` over every shell script present under `scripts/` and is wired into `make check`, so the local pre-review gate matches CI. The dedicated `shellcheck` job in `.github/workflows/unit-test.yml` runs the same `make shellcheck` target, gated on a `scripts` paths-filter (`scripts/**`, the `Makefile`, and the workflow itself) so a scripts-only change doesn't also trigger the full Go lint.

**The CI job pins shellcheck** (`SHELLCHECK_VERSION` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) is the source of truth) rather than using `ubuntu-latest`'s preinstalled copy — that version drifts with the runner image, and shellcheck's heuristics (e.g. when SC2015 fires on `A && B || true`) differ between releases, so an unpinned gate gives a different verdict locally vs. CI. Install the **same** version locally so `make shellcheck` matches the gate: see <https://github.com/koalaman/shellcheck#installing> (the target prints this hint if shellcheck is missing). The pin is bumped automatically by updatecli (see [dependency-updates.md](dependency-updates.md)); when its PR lands, install the new version locally to match.

**What is covered:** the git pathspec `scripts/*.sh` resolved through `git ls-files --cached --others --exclude-standard` — every `.sh` file present under `scripts/` that is either **tracked** or **untracked and not gitignored**, at any depth. The pathspec is recursive because git's default `*` spans `/`, so it already covers a future `scripts/<subdir>/*.sh` without re-touching the gate. Two paths are deliberately excluded: a **gitignored** one (that is how you opt a script out — see below), and a **deleted-but-tracked** one, which `--cached` still lists but shellcheck cannot read. This complements `actionlint`, which only lints the inline `run:` blocks in workflows; before this gate the standalone helper scripts (`setup.sh`, `kind-with-registry.sh`, …) shipped unlinted.

Including untracked files is load-bearing (Q432). The gate used to be `git ls-files` alone — tracked-only — so a **brand-new script was invisible to its own first `make check`**: the gate passed, then produced findings the moment the file was committed, after `make check` had already been reported green. Same false-green class as Q404 (the gate that compiled no build-tagged file), and it cost a real session on 2026-07-26. Consequence for how you keep a scratch script out of the gate: **gitignore it** rather than merely leaving it untracked — write it under the gitignored `tmp/` at the repo root, per the repo temp-file convention. The selection rules (existence filter, merge-stage de-dupe, gitignore honoured, recursion) are asserted by `scripts/shellcheck-scripts-test.sh` under `make scripts-test`.

Accepted findings carry a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (see the dynamic-name `read`/`export` in `scripts/probe-investigations-cd.sh`); everything else is fixed to match the repo bash conventions listed in [`scripts/README.md`](../../scripts/README.md).

### The doc-link gate

`make doc-links` runs `scripts/check-doc-links.sh` over every tracked, non-vendored Markdown file and is wired into `make check`, so the local pre-review gate matches CI. CI runs the same `make doc-links` target from its **own** workflow, [`.github/workflows/doc-links.yml`](../../.github/workflows/doc-links.yml), scoped (via `on.paths`) to `**.md`, the checker, and the workflow itself. It is deliberately separate from `unit-test.yml` — that workflow path-ignores docs, so a docs-only change triggers only this lightweight check and never the Go suite (mirroring how `e2e-test.yml` is its own workflow).

It fails on two classes of breakage: **dead relative file links** (a `[text](path)` whose resolved target is neither a tracked file nor directory — a trailing `:NN` line reference is tolerated and only the file part is resolved) and **dead anchors** (a `#fragment` that matches no heading slug or explicit `<a id>`/`<a name>` in the target Markdown file). Anchors are resolved with GitHub's heading-slug algorithm (strip inline markdown — respecting code spans — lowercase, drop everything outside `[a-z0-9 _-]`, spaces to hyphens, de-dupe repeats with `-1`/`-2`), so the verdict matches what GitHub renders. External URLs (http/https/mailto/tel), links inside fenced or inline code, and anchors into non-Markdown or vendored targets are out of scope.

### The roadmap coherence gate

`make roadmap-check` (`scripts/check-roadmap.sh`) fails when the public [roadmap](../roadmap.md) disagrees with the backlog in [`docs/STATUS.md`](../STATUS.md). It exists because the two drift silently and expensively: a 2026-07-25 audit found **six of seven** "In progress / near-term" roadmap items had already shipped, and the v2 API was still badged `alpha` more than two weeks after `v2beta1` graduated — released that way, because a stable tag deploys that tag's docs permanently. The `docs/development/doc-update-matrix.md` rule requiring the update already existed; a human-followed convention was not enough.

What makes it mechanical is a property of this repo's backlog: **done Queue rows are deleted** (git is the archive). So a roadmap bullet naming a Q-ID that `STATUS.md` no longer has is an exact, zero-false-negative signal that the work shipped. Each bullet under "In progress / near-term" and "Exploring / longer-term" therefore carries an invisible annotation naming its backing rows:

```markdown
- **Capacity-aware job intake.** <!-- q:Q405,Q406 --> Additional opt-in rungs on …
```

HTML comments render nowhere, on github.com or the MkDocs site. The gate fails on: a bullet with no annotation; an ID that resolves to no row (it shipped — move the bullet to "Available now", or drop just that ID when only part of a multi-item bullet shipped); a near-term bullet whose rows are all in **Deferred** (it was parked); and an exploring bullet whose rows are all in the **Queue** (it is active work). "Available now" is deliberately ungated — it describes shipped capability, with no row left to point at. A format change on either side exits 2 rather than passing silently. Rules are asserted by `scripts/check-roadmap-test.sh` under `make scripts-test`.

CI runs the same script from [`status-lint.yml`](../../.github/workflows/status-lint.yml) — alongside the `STATUS.md` format lint and under the same `status-lint-gate` required check — rather than from `unit-test.yml`, which path-ignores docs. That placement is load-bearing: the drift arrives on docs-only PRs, which never trigger the Go suite.

The one gap it cannot close: deleting the Queue row *and* the annotation together, without moving the bullet.

### The conflict-marker gate

`make conflict-markers-check` (`scripts/check-conflict-markers.sh`) fails when any tracked, non-vendored file contains a leftover merge-conflict marker line — the seven-character `<<<<<<<` / `=======` / `>>>>>>>` forms or diff3's `|||||||`. It exists because an edit-based conflict resolution can miss a marker sitting just outside the text it replaced, and format-aware linters skip lines they don't parse; exactly that combination let a stray marker merge to `main` via PR #724 (Q379; fixed same day in PR #730). Wired into `make check`; CI runs the same target from its own lightweight workflow, [`.github/workflows/conflict-markers.yml`](../../.github/workflows/conflict-markers.yml), on **every** PR with no path filter — a marker can be left in any file type. Only exact seven-character marker lines are flagged, so Markdown setext underlines of any other length stay legal, as do mid-line mentions like the backticked examples in this paragraph; the vendored trees are excluded. The pattern logic is asserted by `scripts/check-conflict-markers-test.sh` under `make scripts-test`. When resolving conflicts by hand, `git diff --check` gives the same signal per-file before you stage.

### The v2 API sync gate

`make v2-api-sync-check` ([`scripts/check-v2-api-sync.sh`](../../scripts/check-v2-api-sync.sh)) holds `api/v2alpha1` and `api/v2beta1` identical wherever they share a file. Two served versions of one API must duplicate their *versioned types* — Kubernetes requires it — but the shared spec fragments beside them are identical by contract, and a one-sided edit breaks the storage/hub conversion with nothing to catch it.

The gate's default is inverted from the Q345 original, which named two paths (`conditions.go`) and so covered 332 of ~2,550 identical lines. Now **every** `.go` file present in both packages must match, and a file added to both is covered the day it lands with no edit to the script. Two differences are normalised away before the diff — the `package` clause and a `+kubebuilder:storageversion` marker (only one version ever carries it). Files that genuinely differ per version are named in the script's `EXEMPT` list with the reason; a stale entry (naming a file no longer paired) fails, so the list cannot rot into a silent hole. A file present in one version only is reported but never fails — adding a test to one package is normal.

Wired into `make check` and CI's `lint` job. Its behaviour — including that a body divergence in a previously-unguarded file actually fails — is asserted by `scripts/check-v2-api-sync-test.sh` under `make scripts-test`.

### The build-tag gate

`make build-tags-check` ([`scripts/go-vet-tags.sh`](../../scripts/go-vet-tags.sh)) compiles and vets the Go files that no other fast gate builds: everything behind `//go:build integration`, `e2e`, or `load`. `make lint` and `make test` (and so `make check` and CI's `unit-test` job) build the workspace with the **default** tag set, which excludes those files entirely. So a refactor could leave an unused import or a stale call signature in an envtest suite, `make check` would pass green, and the break would surface only on CI's path-gated integration/e2e leg, which may not even run on the PR that introduced it (Q404).

It runs one workspace-wide `go vet -tags integration,e2e,load` over every `go.work` module. `go vet` typechecks what it analyses, so a compile break fails the gate, and it needs no envtest assets, no cluster, and runs no tests. Actually *running* the tagged suites remains the job of [`make test-integration`](#integration-tests), the [e2e tiers](#end-to-end-tests), and [`make load-test-quick`](#load-tests).

Enabling all three tags at once is sound because they select disjoint package trees rather than alternative implementations of one package: no first-party file is constrained on the negation of another's tag. If that ever changes, say a tag gaining a `!tag` counterpart file, the one-shot invocation stops working and the gate has to vet each tag separately.

**Adding a build tag means editing this gate.** `BUILD_TAGS` in the script is the list, and a coverage assertion keeps it honest: before vetting, the gate asserts (from `go list`'s `IgnoredGoFiles`) that the tag set leaves **no** first-party `.go` file uncompiled. Introduce a tag it does not list and the gate fails with instructions, rather than silently carving another hole in the same shape as Q404. Both properties, the coverage guard failing on an unlisted tag and a tagged-file break that an untagged vet reports clean, are asserted by `scripts/go-vet-tags-test.sh` under `make scripts-test`.

### The path-filter gate

`make path-filters-check` ([`scripts/check-path-filters.sh`](../../scripts/check-path-filters.sh)) reconciles the hand-maintained `dorny/paths-filter` lists in `.github/workflows/` with what the repo actually contains. It exists because a filter that omits a directory makes its gate report green **by skipping** — the worst kind of false negative, since nothing is red and `main` ends up green on evidence it never gathered. That is not hypothetical: `api/` and `scaleset/` were absent from the integration, e2e, and security filters, so changes confined to either module merged without ever meeting envtest, e2e, `govulncheck`, or trivy (Q400, fixed by hand; this gate is the recurrence guard — Q429).

Three assertions, cheapest first:

1. **Registry completeness.** Every filter in every `filters:` block is listed in the script as either `WORKSPACE_FILTERS` (must cover the whole workspace) or `NARROW_FILTERS` (scoped to one gate's inputs, with the reason inline). A new workflow, or a new filter in an existing one, fails until someone classifies it — so the hole cannot reopen in a new shape. A stale entry naming a filter that no longer exists fails too.
2. **Module coverage.** Every `WORKSPACE_FILTERS` entry matches every `go.work` module. Only a recursive glob rooted at the module or an ancestor counts: a bare `api` matches the literal path and nothing beneath it, and `api/config/**` leaves the rest of the module ungated. Failures name the module, the workflow, and the exact pattern to add, one per gap.
3. **Live paths.** Every pattern's literal prefix still exists on disk. A pattern left behind by a rename matches nothing, which narrows its gate as silently as a missing module does.

**So adding a workspace module now fails the gate instead of slipping through** — but the gate only knows about *whole-workspace* coverage. Judgement is still yours for the narrow filters: when you add a module, ask what each gate actually compiles, scans, or bakes, and remember the same applies to a gate that names files individually (`manifest-validate.sh`'s `standalone_manifests` — adding a path there means adding its directory to the filter). Wired into `make check` and CI's `path-filters` job in `unit-test.yml`, which is gated on the `workflows` filter — the one filter watching all of `.github/workflows/`, so editing any `filters:` block re-runs the gate that lints it. Behaviour, including that each assertion fails when it should, is asserted by `scripts/check-path-filters-test.sh` under `make scripts-test`.

Wired into `make check` and CI's `lint` job.

**What it deliberately does not cover: the rest of the linters.** `golangci-lint` still runs with the default tag set, so `gosec`, `errcheck`, `staticcheck`, `unused`, `dupl`, and `funlen` see none of the tagged packages. Closing that is a one-line `run.build-tags` addition to `.golangci.yml`, but it surfaces 21 pre-existing findings in the envtest/e2e/load trees (`gosec` `G204`/`G301` in the e2e `kubectl` harness, unchecked returns, a dot import, two dead helpers) that each need a fix or a justified inline accept. That triage is its own change, tracked as [Q430](../STATUS.md#Q430), and was kept out of Q404 so the compile gate could land without a lint sweep attached to it. `go vet` was the right first rung: a compile break is unambiguous and blocks a whole tier, where the rest is code quality in test scaffolding.

### Never foreground-poll CI, logs, or files

Do **not** run a blocking watch/tail on the main thread to wait for something to change — no `gh pr checks --watch`, no `gh run watch`, no `while … sleep` tail loops, no re-running `gh pr checks`/`gh run view`/`kubectl logs -f`/`tail -f` on a timer to see if a result has landed yet. A foreground poll pins the session doing nothing until it times out or is killed, and it competes with the background machinery that already tracks these signals. In a two-week sample this pattern alone produced ~130 blocked poll attempts.

Use the asynchronous mechanisms instead:

- **PRs and CI** — the Auto-fix/PR-monitor path watches CI and pushes fixes on its own (see [parallel-dispatch.md](parallel-dispatch.md) § self-healing); let it. If you need the current state right now, take **one** non-blocking snapshot (`gh pr checks <n>` without `--watch`, `gh run view <id>`) and move on — schedule a later re-check, don't spin.
- **Long-running local work** — launch it as a background task (a background Bash run, or a background agent) and let the completion notification wake you, rather than blocking the foreground on it.

The rule: a single point-in-time check is fine; a loop or a `--watch`/`-f` that blocks the main thread waiting for change is not.

### Slow tiers need an explicit timeout or a background run

The Bash tool's default timeout is short (two minutes) and it **kills** anything that overruns — in the same two-week sample, 36 slow runs were killed mid-flight this way, wasting the whole run. Any invocation that can exceed the default — the envtest integration suites, the kind e2e tiers, and `go test -race` / `make test-race` above all — must therefore either:

- carry an **explicit timeout** on the Bash call generous enough to cover the run (up to the 10-minute Bash ceiling; use `make … TIMEOUT=…`/`go test -timeout …` for the test-level deadline as well), **or**
- be launched as a **background task** so it runs to completion detached and notifies on exit.

Never fire one of these as a default-timeout foreground run and hope it finishes — it will be killed partway and you learn nothing. Pick the timeout from the tier's real cost (see [Cost & cadence](#cost--cadence-rough-ephemeral-ci-2026-ballparks) below); when in doubt, background it.

**Read a background run's output, not its reported exit status.** The status you get back is the *pipeline's* — so a run piped through `tail`, `head`, or `grep` reports the filter's exit code, not the test's. `go test … | tail -30` is reported as exit 0 even when the suite failed, which silently inverts the one signal the run existed to produce. Either drop the pipe (the output file is readable in full anyway) or add `set -o pipefail` so the pipeline carries the test's status through. Confirm a pass by reading the `ok` / `FAIL` line, never from the exit code alone.

Both rules in this section are enforced mechanically by the foreground-guard hook: it prompts on foreground watch/`sleep`-poll forms, and its slow-command registry in `.claude/foreground-guard.json` names the tiers above (`make test-race`, `make test-integration`, the `e2e` targets) with their minimum timeouts — keep that registry in sync when a tier's runtime or target name changes.

### Ad-hoc shell varies: don't rely on word-splitting

Committed scripts under `scripts/` are `#!/usr/bin/env bash` and follow [bash-style.md](bash-style.md), so their behaviour is pinned by the shebang. **Ad-hoc commands are not pinned** — they run in whatever login shell the contributor has: zsh on macOS (the default since Catalina), bash on most Linux distributions and CI images. Check yours with `echo $0` rather than assuming.

That matters because the shells disagree on **word-splitting of unquoted parameter expansions** — bash and `sh` split, zsh does not:

```sh
FLAGS='-run TestFoo -count 1'
go test $FLAGS ./...   # bash/sh: four arguments.  zsh: ONE argument, the whole string.
```

Under zsh that `go test` receives a single literal argument `-run TestFoo -count 1` and fails to parse it — a confusing "unknown flag" or "no such file" from a snippet a bash reader would call correct. Because the shell differs per contributor, an unquoted expansion is **not portable in either direction**: a recipe that works on a bash box breaks for the next person on macOS, and vice versa.

**The fix is almost always to drop the variable.** Ad-hoc commands are one-shots — write the arguments literally and there is nothing to split:

```sh
go test -run TestFoo -count 1 ./...
```

Reach for a variable only when something genuinely reuses the list — a loop, or a flag set applied to several commands in one session. Then pick by where it has to run:

| Context | Form | Portability |
|---|---|---|
| bash or zsh (any interactive shell you'll actually meet) | `flags=(-run TestFoo -count 1)` → `go test "${flags[@]}" ./...` | bash + zsh. **Not POSIX** — `dash` rejects the `(` outright, so don't carry it into an `sh` context |
| must also run under `sh` | `set -- -run TestFoo -count 1` → `go test "$@" ./...` | every shell, including `dash`; costs you the positional parameters |
| worth keeping at all | a script under `scripts/` with a bash shebang | pinned by the shebang, and [shellcheck](#the-shellcheck-gate)-gated |

Note that "write POSIX" is **not** a fix on its own: zsh's not-splitting *is* its deviation from POSIX, so POSIX-style `$FLAGS` still breaks there. Portability comes from the quoted form you choose, not from avoiding extensions.

Two things to avoid:

- **Don't "fix" a snippet by dropping quotes** and relying on splitting — that is the bash reading, and it silently does the wrong thing under zsh.
- **Don't reach for zsh's `${=VAR}`** (its explicit split-this operator) in anything shared. It is zsh-only: bash rejects it with `${=FLAGS}: bad substitution`, converting a portable command into one that fails for half the team.

## Picking the right test tier

Prefer the narrowest tier that can actually *observe* the bug class — but no narrower:

- **Unit (fake client)** — pure logic and field-level behavior. The fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`) reproduces none of the real-apiserver semantics below, so a fake-client test cannot prove claims that depend on them.
- **envtest (integration)** — any claim that depends on real-apiserver semantics: schema/admission defaulting, server-side no-op-write dedup (the apiserver skips the `resourceVersion` bump when a patch's defaulted result is unchanged), admission/validation webhooks and CEL, and `IsConflict` handling. Both `cmd/agc` and `cmd/gmc` already have envtest suites at `internal/controller/integration/` (build tag `integration`, see [Integration tests](#integration-tests)) — add to them rather than concluding none exists; confirm with a directory listing before deciding a tier is missing. Example: PR #143 (Q65) migrated the GMC `apply*` helpers to `CreateOrPatch`; a fake-client test could verify field-level behavior, but only `apply_nochurn_test.go` (envtest, asserting `resourceVersion` stability across periodic reconciles) could prove the whole-`Spec` helpers don't churn.
- **Tier-A kind e2e** — behaviors that emerge from real Container Network Interface (CNI), kube-proxy Destination NAT (DNAT), kubelet image-pull policy, or TLS-over-tunnel. When a feature crosses one of those boundaries, the Tier-A test (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests) and [End-to-end tests](#end-to-end-tests)) is the only thing that proves it works. Example: PR #59 fixed 5 bugs that all unit tests passed for — a single planned-but-unimplemented Tier-A test (`E2E_GMC_TenantProvisioning_ProxyConnectWorks`) would have caught 4 of them locally.
- **Load (in-process)** — scaling claims about the AGC's own goroutine/memory/throughput footprint, not a functional bug class. The load harness (build tag `load`, see [Load tests](#load-tests)) drives the real listener-multiplexing core at thousands of concurrent virtual sessions without a cluster. Use it to pin a capacity claim or guard against a concurrency-core regression (goroutine leak, sustained-session collapse); it cannot speak to anything downstream of the AGC process (real pods, apiserver/GitHub latency).

Before concluding a test failure is a code bug, check whether the problem is in the test expectations, the test setup, or the code itself — the intent of the test must match the implementation.

## Diagnosing failures: measure before asserting a root cause

A root-cause claim needs evidence measured from *this* failure, not a resemblance to a remembered one. Two shortcuts recur and both produce confident-but-wrong diagnoses:

- **Symptom-matching a prior issue.** When a failure looks like a known issue — a flake row on the [backlog](../STATUS.md), a previously fixed bug, a memory of "this is always X" — that match is a **hypothesis, not a diagnosis**. The same surface symptom (a scheduling timeout, an egress blip, a wedged run) can have a different cause each time. Before acting on the remembered cause — and above all before spending a billable re-run, a fix PR, or a state-changing command on it — take a direct measurement from the failing system: read the actual events, describe the actual pod, pull the actual log line. If the environment tears down evidence on failure, capturing diagnostics *before* teardown is part of the fix, not optional (filed from the v1.2.0 release retro, where gate failures had to be re-run just to observe them).
- **Trusting source inspection.** Reading code — or a plan doc's ✅ investigation findings, which usually derive from source-reading — tells you what *should* happen, not what does. Treat such findings as unverified until confirmed end-to-end: actually exec the thing. Source-reading alone has produced wrong conclusions before (PR #59).

### Proving a flake fix: invert it

Repeated passes do not validate a flake fix. A green `-count=20` is equally consistent with *"the race is closed"* and *"the race didn't fire this time"* — and on an unloaded dev machine the second is the more likely of the two, because the timing that produces the flake on a loaded CI runner often can't be reproduced locally at all. Passing-after is necessary evidence, not sufficient evidence.

Run the **negative control** before concluding: invert the fix — restore the old value, remove the pin, revert the ordering — and confirm the suite *fails*. A fix you cannot make fail on demand has not been shown to be load-bearing, and shipping it closes the backlog row while leaving the flake live. When the inverted form refuses to fail either, that is itself the finding: the diagnosis is wrong, or the mechanism isn't the one you think it is.

Q378 is the worked example. Pinning `BaselineRecheckInterval` in the reaper tests passed 10× under `-race`, which on its own proved nothing; setting the pin to 1s instead — and watching the suite fail — is what established that the pin was the thing closing the race.

The [structural-ceiling triage](technical-debt.md#distinguish-a-fixable-defect-from-an-external-structural-ceiling) is the same principle at a larger scale: when fixes stop converging, isolate and *measure* the external actor instead of asserting the next on-our-side cause.

## Where each tier can physically run (and what it costs)

The tier above says *what* observes a bug; this says *where that tier can run*. Most validation is local on a dev machine; a short list needs real GitHub, real cloud, or real scale. The **environment definitions** below are durable; the **Q-item mapping** is a snapshot of the [backlog](../STATUS.md) as of 2026-06 and may lag.

- **Local — `kind` (the default).** Unit, envtest, Tier-A/B e2e, and the load harness need only a Linux-kernel cluster plus a fake or in-cluster GitHub. This covers the large majority of work and runs on an Intel Mac under Docker Desktop.
- **Local — `minikube` + gVisor addon (the one thing kind can't do).** A `RuntimeClass=gvisor` node needs `runsc` on the node, which kind's container-nodes can't supply cleanly. minikube can: locally `minikube start --driver=qemu` (a Linux VM) then `minikube addons enable gvisor`; on a Linux CI runner `--driver=none` (or `docker`) + the same addon. gVisor's **systrap platform needs no nested virtualization**, so it works on a stock machine and a stock `ubuntu-latest` runner alike. Reach for minikube **only** for gVisor — kind stays the default everywhere else (lighter, already wired into the e2e workflows). Full local VMs (Lima/Colima/Multipass) host the same `runsc` setup but unlock nothing beyond gVisor.
- **Needs real GitHub.** Tier-C e2e and the live broker-compatibility probe (the credential-gated `cmd/probe` binary). Free (GitHub API within rate limits); needs a test App/org credential as a CI secret. Automatable per-PR or nightly. The credential-free counterpart — the `cmd/probe/compat` suite that asserts every documented broker contract against the in-process broker model — runs locally in `make check` with no secrets; its published result is [broker-compatibility.md](broker-compatibility.md).
- **Needs real cloud.** Cloud KMS signing, managed control-plane behavior (EKS/GKE/AKS), and cloud workload-identity binding (IRSA / GKE WI / Azure WI). Not reproducible in kind/minikube — needs the actual provider. Automatable as a scheduled job that provisions an **ephemeral** cluster (eksctl/Terraform), torn down after.
- **Needs real scale.** The 1,000-pod real-cluster capacity run. A 4-core Docker Desktop VM can't host it; needs a multi-node cluster. The in-process load harness already covers the AGC-only claim locally for free, so this is release-gated, not routine.

### Cost & cadence (rough, ephemeral CI, 2026 ballparks)

| Validation | Substrate | ~Cost | Cadence |
|---|---|---|---|
| Broker-compat, Tier-C (Q191; Q11†) | test GitHub App | $0 (free API) | per-PR / nightly |
| gVisor `RuntimeClass` (Q15) | minikube + gvisor addon, stock runner | $0 | per-PR / nightly |
| Cloud KMS + workload-identity legs (Q197 cloud) | KMS key + ephemeral EKS/GKE/AKS | KMS <$5/mo; ~$0.50–1 / run | nightly / weekly |
| Managed-cluster audit paths (Q182) | ephemeral EKS/GKE/AKS ×3 | ~$1–2 / full-matrix run | weekly / release |
| Per-cloud apiserver CIDR (Q183) | ephemeral cluster/cloud, or doc-only | ~$0.20–0.50 / cloud, or $0 | release / manual |
| 1,000-pod scale (Q181 real run; Q193 benchmark) | ~25–50-node ephemeral cluster | ~$10–30 / full run (~$3–8 at 250 pods) | occasional / release |

† Q11 is *also* blocked on a GitHub feature (X25519 ECDH) that does not yet exist — untestable at any cost until then.

*Ephemeral* = provision → test (~20–40 min) → tear down; cost is hourly proration of a small managed control plane (~$0.10/hr) plus a few small/spot nodes. A standing cluster costs more (~$50–100/mo) but CI doesn't need one. Hosted Linux runners also expose `/dev/kvm` if VM-level isolation (Kata, Firecracker) is ever needed, but gVisor does not require it.

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

### Test doubles must long-poll

Both GitHub doubles model the real backend's long poll: an empty poll is **held** until a message lands or a poll window elapses. A double that answers "nothing to deliver" instantly turns any polling client into a spin loop, which burns CI CPU on every unrelated test in the package and widens the timing windows other tests race against.

- [`scaleset/scalesettest`](../../scaleset/scalesettest/) — `DefaultPollTimeout` (1s) bounds the wait, and a parked poll wakes the instant the queue changes (a job enqueued, acquired, or completed; the session dropped), so delivery stays immediate and tests stay fast. `Server.SetPollTimeout(0)` restores the non-blocking behavior — use it *only* in a test that asserts the 202 itself, never in one that drives a polling client. Before this landed (Q287) an idle scale-set listener polled the stub ~5,000×/s; it is now ~1/s.
- [`test/fakegithub`](../../test/fakegithub/) — long-polls job delivery for the same reason (Q148, where an instantly-returning fake collapsed the listener pool: replacement listeners idle-exited in milliseconds).

The AGC's scale-set listener also enforces its own floor (`minPollInterval`) between two consecutive polls that deliver nothing, so a *real* backend that declines to hold the poll cannot spin it either. That is defense in depth, not a substitute — a double that does not long-poll still distorts every timing assertion around it.

### The broker doubles share one protocol core

Three broker doubles implement the GitHub Actions broker wire protocol: [`broker/brokertest`](../../broker/brokertest/) (the in-process integration stub), [`test/fakegithub`](../../test/fakegithub/) (the deployed Tier B e2e image), and the [load harness stub](../../cmd/agc/test/load/broker_stub.go). They diverge in what a job delivery and an AcquireJob *mean* — fan-out accounting (Q260), single-use JIT consumption (Q114), saturated auto-delivery — but the session and credential *mechanics* are identical: minting `session-<n>` IDs, resolving a DELETE by its `sessionId` query param or bearer token, owner-scoped session listing, and the connection-reuse-safe JSON framing. Those live once in [`broker/brokerstub`](../../broker/brokerstub/) (Q368); each double layers its own delivery/acquire policy on top. `broker/brokerstub` is deliberately **standard-library-only** so the fakegithub distroless image links no third-party code — do not import the `broker` client (or anything else) into it.

### The scale-set protocol has exactly one double

[`scaleset/scalesettest`](../../scaleset/scalesettest/) is the only stub for the runner-scale-set protocol, shared by the scale-set listener's unit tests, the `cmd/agc` v2 RunnerSet integration suite, and `cmd/probe`'s Investigation E scenario (Q389). Keep it that way: a second hand-rolled scale-set stub is a second dialect to keep in sync, and the probe exists to catch library-vs-wire divergence — which it cannot do against a stub that agrees with whatever the probe assumes.

It models the protocol *semantically* rather than replaying a scripted response list, so a test states the backend condition it wants and the stub derives the wire from it: `PrequeueJobs` queues jobs against the scale set's label before it registers (for a caller that creates its own scale set mid-run), `EnableGHESAcquireFlow` switches from auto-assign to the JobAvailable→acquire path, and the `Fail*` levers (`FailRunnerGroups`, `FailSessionCreate`, `FailSessionRefresh`, `FailStaticAcquireRoute`, `SetRateLimitPolls`) each model one observed backend failure. Assert on `Server.Calls()`, the ordered call log. Reach for `SeedMessage`/`SeedRawMessage` only for the shapes the model cannot reach on its own — a lifecycle message with no preceding assignment on that scale set, or a body no client can decode.

**No `package main` may reach `net/http/httptest`.** A production binary must never link a test server. `TestNoPackageMainReachesHTTPTest` (in `cmd/probe/compat`) enforces this: it walks every `package main` in the workspace and fails if any transitively imports `net/http/httptest` in its compiled build graph (`go list -deps`, so a `_test.go` file importing httptest — as fakegithub's own tests do — is correctly ignored). It runs in `make check`; a stray import of a broker double into a shipped binary fails the gate.

## Load tests

The load harness (Q13) pins the design's headline capacity claim — thousands of virtual runner sessions multiplexed per AGC, each costing one re-registration per job (the single-use JIT lifecycle, Q114). It is gated by the `//go:build load` tag and lives under [`cmd/agc/test/load/`](../../cmd/agc/test/load/); its [README](../../cmd/agc/test/load/README.md) documents every knob and how to read the output, and [milestone-5.md §2](../plan/milestone-5.md) the design rationale.

It needs **no cluster and no GitHub credentials**: it drives the real `listener.Multiplexer` + `agentpool.Pool` + per-goroutine `broker.Client` wiring against an in-process broker stub (single-use JIT + long-poll), a controller-runtime fake client for agent Secrets, and an in-memory registrar.

```bash
make load-test-quick   # 10 tenants × 100 listeners = 1,000 sessions, short window (~1 min)
make load-test-full    # same scale, realistic job hold; writes results/latest.md (~3-5 min)
```

Both run under the same desktop-safety throttle as the rest of the suite (a no-op on CI). The Service Level Objectives (SLOs) it asserts — sustained concurrent sessions, ≈1 re-registration per job, no goroutine leak — are the faithful results; absolute throughput and recycle latency are bounded by the in-process control-plane stand-ins and are reported for trend, not as production figures (see the README's fidelity boundaries). It is **not** wired into `make check` or per-PR CI — run it when changing the concurrency core (listener/multiplexer/agentpool) or validating a capacity claim.

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

**Curl test image.** The connectivity, isolation, and metrics specs run a `curlimages/curl` pod. It defaults to the upstream Docker Hub ref (`curlimages/curl:8.10.1`), which is fine locally. CI sets `E2E_CURL_IMAGE` to a local-registry mirror (`127.0.0.1:5000/curlimages/curl:8.10.1`, populated by the workflow's mirror step) so the kind nodes never pull from Docker Hub — anonymous Hub rate limits (HTTP 429) were starving these pods and flaking three specs.

**Test labels and the `multi-node` suite.** Three Ginkgo labels annotate the suite. CI runs the **full** suite — `make e2e` with no `SUITE`, so no `--label-filter` — on the default 2-worker cluster (`test/kind-config-2worker.yaml`), so every labelled spec runs in CI:

- `multi-node` — specs that need the 2-worker cluster shape to be meaningful: `E2E_GMC_ProxyPodScheduledOnWorker` (pod-to-worker placement), `E2E_GMC_PDBPreventsEvictionBelowMinAvailable` (PodDisruptionBudget (PDB) blocks eviction while a replica survives on another node), and `E2E_GMC_GMCRestartPreservesState`.
- `github-real` — the Tier C specs that dispatch against real GitHub (`E2E_GitHub_RealDispatch`); they self-skip when the `GITHUB_E2E_*` env vars are unset.
- `real-github-egress` — the specs whose traffic terminates at the live `api.github.com`: the v1/v2 `ProxyConnectWorks` CONNECT specs, the two `E2E_V2_DirectEgress` specs (their NP ipBlock-peer waits also depend on the GMC's live `/meta` fetch), and the Tier C container. Not a filter label: a suite-level `AfterEach` (`cmd/gmc/test/e2e/github_egress_preflight_test.go`) uses it for failure-time attribution — see [Runner→GitHub egress attribution](#runnergithub-egress-attribution-q352).

For a faster local inner loop on a 1-worker cluster, `make e2e SUITE=single-node` maps to `--label-filter '!multi-node'` and skips the multi-node specs; unset `SUITE` runs everything (matching CI). The HPA scale-up spec (`E2E_GMC_HPADrivesScaleUp`) is unlabelled and CI-safe: it patches `HPA.spec.minReplicas` to drive the HPA→Deployment control path deterministically rather than burning CPU to trigger autoscaling, so it runs everywhere.

**Waiting for the AGC, not just its Deployment.** A spec that waits for a broker session (or anything else that needs the AGC operational) must gate on `utils.WaitForRunnerGroupReconciled`, not only `utils.WaitForDeploymentReady`. Deployment readiness means only that the AGC's health server is up — it binds within seconds of pod start and is deliberately decoupled from the GitHub-App token fetch (`cmd/agc/main.go`), whose budget alone is up to ~2 minutes. `WaitForRunnerGroupReconciled` waits for `RunnerGroup.status.observedGeneration` to be set, which the AGC does only after token + agent registration + listener-multiplexer start all succeed. Gating on Deployment readiness alone folds the AGC's whole startup into the session wait's budget, which under parallel CI load (token/registration/session round-trips to the shared single-replica fakegithub) can exhaust it and surface as a misleading "no session registered" timeout (Q134).

**Tier C.** Set `GITHUB_E2E_APP_ID`, `GITHUB_E2E_INSTALLATION_ID`, `GITHUB_E2E_PRIVATE_KEY` (a PEM path or the PEM body), `GITHUB_E2E_ORG`, and `GITHUB_E2E_REPO` in the environment, then run `make e2e` (Tier C specs skip themselves at runtime when any variable is missing). The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

## CI workflows and scripts

CI must use the same commands as [Running tests](#running-tests) above — per-module invocations or the explicit multi-module patterns `scripts/go-test.sh` builds; never `go test ./...` from the repo root, which does not work with the Go workspace layout.

### Pinned tool installs: always via `download-verified.sh`

Every CI step that installs a pinned third-party binary — kind (`e2e-reusable.yml`), shellcheck (`unit-test.yml`), kubeconform (`manifest-validate.yml`), polaris (`security-scan.yml`) — and the local `$(COSIGN)` rule fetch it through [`scripts/download-verified.sh`](../../scripts/download-verified.sh) (`<url> <sha256> <output-path>`). Do not hand-roll `curl` + `sha256sum -c` in a new step; use the script, and keep the version and its digest pinned side by side in the workflow `env:` block.

The script exists because both halves of that fetch are easy to get subtly wrong:

- **Retry.** `curl --retry` covers 408/429/5xx and connection failures **only**, so a GitHub releases-CDN **403** — the denial the CDN actually serves under load — fails the download instantly, in well under a second, with `curl: (22)`. That reddened a whole PR run via `security-scan-gate` (Q433, PR #828). `--retry-all-errors` widens the retry to any error, including that 403; the script always passes it (`DOWNLOAD_RETRIES`/`DOWNLOAD_RETRY_DELAY`, default 5×2s).
- **Integrity.** GitHub release assets are mutable for an existing tag, so the bytes must be checked against a pinned digest (Q126/Q127). The digest is a required argument, the download lands in a temp file, and the output path is written only after the digest matches — there is no flag or environment variable that skips the check. [`scripts/download-verified-test.sh`](../../scripts/download-verified-test.sh) (under `make scripts-test`) asserts that: a mismatch fails and leaves nothing at the output path, a malformed digest is rejected outright, and the `curl` line still carries `--retry-all-errors`.

### Path-gated workflows: verify the heavy gates actually ran

Most code-exercising workflows keep unrelated PRs cheap by **skipping their expensive jobs internally** rather than skipping the whole workflow. The build/lint/test/security gates (`unit-test.yml`, `integration-test.yml`, `e2e-test.yml`, `e2e-calico.yml`, `security-scan.yml` — trivy + govulncheck, `manifest-validate.yml`, `license-notices.yml`, plus `status-lint.yml` and `plan-hygiene.yml`) trigger on **every** `pull_request` (no top-level path filter), then a `dorny/paths-filter` `changes` job classifies the diff and each real job's `if:` guard skips it when nothing it covers changed. Each workflow ends with a small **`<workflow>-gate`** job (`unit-test-gate`, `security-scan-gate`, …; `if: always()`, `needs:` every real job) that passes only when each concluded `success` or `skipped` — this is the job whose check context is (or is intended to be) the branch's **required status check**. The ids are unique per workflow on purpose: a normal job's check-run name **is its job id**, GitHub matches required checks by that name, so nine jobs all named `gate` would collapse to one indistinguishable entry in the ruleset UI. See [required-status-checks.md](../plan/archive/required-status-checks.md).

**Why not the simpler top-level `paths-ignore`:** a workflow skipped by a top-level path filter reports **no check at all**, which leaves a *required* check **Pending forever** and wedges the merge. Triggering on every PR and gating internally means the `gate` context always reports — green (all jobs skipped) on an unrelated PR, red when a real job fails — so it is safe to require.

**The historical gotcha this closes — a PR going green/`CLEAN` without ever testing its code.** Under the old top-level `paths-ignore`, a PR **opened while docs-only** with code **added in a later push** could leave the path-gated workflows **skipped** (the `synchronize` did not reliably re-trigger them; see [actions/runner#2324](https://github.com/actions/runner/issues/2324)), so the PR showed all-green with the code never built or tested. Because the workflows above now trigger on every PR and re-evaluate the diff via the `changes` job on each push, this specific skip-through no longer applies to them. The `gate` contexts are now marked required in the ruleset, so a red or Pending gate blocks the merge.

**The `changes` job fails open (Q363).** `dorny/paths-filter` resolves the changed-file list through a **single un-retried GitHub API call** — `@actions/github`'s Octokit carries no retry plugin — so one transient 5xx or reset used to fail the `changes` job and, through `needs:`, the whole gate. Worse, a JavaScript action reports failure *only* as an `::error::` annotation, and a rerun replaces the check run and destroys that annotation: the surviving log simply stopped after `Invoking listFiles` with no error recorded anywhere, which is why the original occurrence looked like a silent failure. Each `changes` job now carries `continue-on-error: true` on the paths-filter step and derives its outputs as `steps.filter.outcome == 'success' && steps.filter.outputs.<name> || 'true'`, so an unclassifiable diff **runs every gated job** instead of skipping it — fail-open on *detection*, still fail-closed on *validation*. A step that trips it also emits a `::warning::` so the degradation is visible in the run log rather than inferred. Note this is deliberately not a retry: re-running the classifier could still return a wrong answer, whereas running the gated jobs is always safe.

**Adding a Go module means editing every filter that covers it (Q400).** The filters are hand-maintained lists, and a module absent from a gate that does compile it makes that gate stay green by *skipping*, not by passing. Both modules added after the initial filters were written hit this: `api/` and `scaleset/` were in `unit-test.yml` but in none of `integration-test.yml`, `security-scan.yml`, or `e2e-test.yml`, so an `api`- or `scaleset`-only change skipped the envtest tier (whose AGC ScaleSet suite imports `scaleset/scalesettest` directly), govulncheck, trivy, and e2e. `manifest-validate.yml` had the same gap for the five v2 CRDs under `api/config/crd/`, which `scripts/manifest-validate.sh` validates by name.

The whole-workspace half of that is now mechanical: the [path-filter gate](#the-path-filter-gate) (`make path-filters-check`, in `make check` and CI) fails when a `go.work` module is missing from a filter whose jobs exercise the whole workspace, when a filter is not classified, or when a pattern points at a path that no longer exists. The judgement half is not automatable and remains yours: for the deliberately narrow filters, walk every `filters:` block in `.github/workflows/` and ask what that gate actually compiles, scans, or bakes — not what its filter happens to list today. The same applies in reverse to a gate that names files individually (`manifest-validate.sh`'s `standalone_manifests`): adding a path there means adding its directory to the filter.

**Verify before declaring a PR review-ready and before merging it:** confirm the gates that exercise the change actually executed **on the PR's head commit** — green is not enough if a gate was skipped, and *no red checks* is not the same as *the checks ran*. For any Go / CRD / chart change you should see runs for `build`, `lint`, `integration-test`, `e2e`, `security-scan` (trivy + govulncheck), and `manifest-validate`:

```bash
gh pr view <n> --json headRefOid --jq .headRefOid   # the SHA every check must be attached to
gh pr checks <n>                                    # are the expected gates PRESENT — not merely un-red?
gh run list --branch <branch> --limit 30            # cross-check which workflows actually ran, and on which SHA
```

Read the output as a **checklist against the expected set above**, not as a pass/fail summary. A PR with zero rows, or with only the lightweight docs workflows listed, has not been tested — it just has nothing to fail.

**Absence of checks is not green (Q383).** There are two distinct ways a PR ends up under-tested, and they need different fixes:

- **Gates skipped by the diff classifier** — the workflow ran, its `changes` job classified the diff as irrelevant, and the real jobs were skipped. `gh pr checks` shows the `gate` contexts as green. Confirm against the expected-gate list above; if a gate that should cover the change is missing, the classifier's path filters are wrong.
- **No workflow runs attached at all** — the push registered, but GitHub never dispatched any workflow for the head SHA. Observed on this repo for **~10 minutes** after a push: `gh run list --branch <branch>` returned nothing for that commit while the PR page showed no checks section whatsoever. Nothing is red, so the PR reads as clean at a glance; in fact nothing has run.

**Fix for both:** `gh pr close <n> && gh pr reopen <n>`. The `reopened` event re-dispatches the workflows and re-evaluates the path filters against the full PR diff. Then re-verify with the commands above (asynchronously — see [Never foreground-poll CI, logs, or files](#never-foreground-poll-ci-logs-or-files)) and confirm the expected gates are now present and concluded before treating the PR as tested.

### The e2e workflows: kindnet and Calico

The cluster/image/test plumbing for the e2e suite lives in one reusable workflow, [`.github/workflows/e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`workflow_call`, parameterized by a `kind_cni` input). Two callers drive it so a kind bump, image-tag change, or flake mitigation is made once and both lanes inherit it:

- **[`e2e-test.yml`](../../.github/workflows/e2e-test.yml)** — the per-PR / push-to-main leg, `kind_cni: kindnet`. Path-gated (skips PRs touching no e2e-relevant files) and `cancel-in-progress` on PRs. This is the merge gate.

**Infrastructure image caching.** External images that the kind *nodes* would otherwise pull on every run — a recurring flake source under registry rate limits and a latency cost on the critical path — are pre-pulled on the runner (cached via `actions/cache`, retried) and seeded onto the nodes so the in-cluster pull is a local hit. This covers, on **both** legs, the `curlimages/curl` test image (mirrored into the local registry), the **cert-manager** controller/webhook/cainjector images (pre-pulled + `kind load`ed before `make apply-cert-manager`, whose rollout is then waited on), and the **metrics-server** image (pre-pulled + `kind load`ed before the suite runs, since the suite applies the pinned `components.yaml` itself in `setupMetricsServer` — Q150); and on the **Calico** lane, the Calico CNI images (see below). The pinned versions live in the workflow env (`E2E_CURL_UPSTREAM`, `CERTMANAGER_VERSION`, `METRICSSERVER_VERSION`, `CALICO_VERSION`) and must be kept in sync with their source of truth (`CERTMANAGER_VERSION` in `cmd/gmc/Makefile`, `metricsServerVersion` in `cmd/gmc/test/e2e/e2e_suite_test.go`, `CALICO_VERSION` in the root `Makefile`) — bump together. The first-party images (`gmc`/`agc`/`proxy`/`fakegithub`/`worker`) are built and pushed to the local registry by the bake step, whose layers are cached via buildx `GHA_CACHE`.

#### Runner→GitHub egress attribution (Q352)

A handful of specs deliberately reach the **live** `api.github.com` (see the `real-github-egress` label above), so a transient outage of the CI runner's own GitHub egress kills them with signatures that look like product regressions — observed as a proxy CONNECT 502 (kindnet lane, 2026-07-14) and curl exit-28 timeouts including the proxy-less DirectEgress spec (Calico lane, 2026-07-19), both green on re-run. Two probes make such blips self-attribute instead of costing a triage:

- **In-suite, at failure time (the authoritative signal):** when a `real-github-egress`-labelled spec fails, a suite-level `AfterEach` immediately issues an HTTPS GET to `api.github.com/zen` from the test process — the runner host, the segment every in-cluster path NATs through — and stamps a `RUNNER-HOST GITHUB PREFLIGHT: FAILED/OK` banner into the spec's failure output. `FAILED` (a transport error; any HTTP status counts as reachable) means infra blip — re-run; `OK` means treat the failure as real. A non-fatal baseline probe also logs reachability at suite start; it deliberately does **not** fail fast, since a start-time blip may clear before those specs run and a fatal preflight would add a flake surface rather than remove one.
- **In the workflow's failure-diagnostic step:** `e2e-reusable.yml` curls `api.github.com/zen` from the runner alongside the cluster dumps, covering the case where the suite process itself died before the `AfterEach` could report.

#### The Calico e2e lane

- **[`e2e-calico.yml`](../../.github/workflows/e2e-calico.yml)** — `kind_cni: calico` (Q119). kindnet accepts `NetworkPolicy` but its bundled enforcer does not drop egress, so the NetworkPolicy-enforcement specs self-skip on the per-PR kindnet leg; on Calico they assert real packet drops. The full suite runs on both CNIs — these specs simply activate under Calico: the two `TenantProvisioning` egress negatives (`WorkloadEgressBlockedToNonProxyPod`, `WorkerCannotReachK8sAPI`), `ProxyConnectWorks` (which runs on both but is only truly enforced here), and the two `ManagerMetricsNP` specs (Q83). No per-lane spec selection is needed — the suite's runtime `egressEnforcingCNI()` self-skip does the routing.

  **When it runs:** **per-PR (and on push to main) only when the diff touches NetworkPolicy/proxy code** — the GMC (`cmd/gmc/**`, which generates the tenant + manager policies and the proxy), the egress proxy (`cmd/proxy/**`), the chart's policy templates (`charts/actions-gateway/**`), or the CNI/cluster plumbing (`scripts/kind-with-registry.sh`, `Makefile`, the two e2e workflows). PRs that cannot regress enforcement stay on the fast kindnet leg and pay no Calico cost. The path filter is the *sole* automatic gate (there is no nightly catch-all), so it deliberately errs toward the components that produce or police the enforced traffic. **Trigger it manually** any time from the Actions tab → *e2e (calico)* → *Run workflow* (`workflow_dispatch`). Because it triggers on the PR's own files, a change to the lane itself (or to NP/proxy code) is validated on that PR rather than only post-merge.

  **It gates merge via its `gate` job.** The workflow triggers on every PR (no top-level path filter) and skips the expensive Calico leg internally via the `changes` job, so a non-matching PR still reports a green `e2e-calico-gate` — the required-check-safe pattern (see [required-status-checks.md](../plan/archive/required-status-checks.md)).

  **Calico image caching.** The Calico manifest pulls `calico/node`, `calico/cni`, and `calico/kube-controllers` from quay.io/docker.io on every node during install — and those pulls happen *before* the local registry is wired into the nodes, so they cannot be mirrored the way the curl image is. Instead the lane pre-pulls the exact image refs the pinned manifest references into the runner's Docker daemon (cached via `actions/cache`, keyed on `CALICO_VERSION`, retried), and `scripts/kind-with-registry.sh` `kind load`s whatever is present onto the nodes so the rollout never touches quay.io. This keeps the per-PR Calico cost bounded and quay.io off the critical path. Calico still gets a 60-minute timeout vs. the kindnet leg's 45 for rollout headroom. `CALICO_VERSION` is pinned in both the root `Makefile` and the workflow env — bump them together.

### The Dockerfile-lint gate

[`.github/workflows/dockerfile-lint.yml`](../../.github/workflows/dockerfile-lint.yml) runs `hadolint` over all six Dockerfiles (a matrix leg each: `gmc`, `agc`, `proxy`, `worker`, `fakegithub`, and `scripts/dogfood/runner/Dockerfile` — the last a dev/reference image, not a shipped one), path-gated on `**/Dockerfile`. The failure threshold is `style` — the strictest level, which all six currently pass clean — so a regression such as an unpinned base tag, a dropped digest pin, or a relaxed non-root `USER` fails at PR time. It is its own lightweight workflow (like `doc-links.yml` and `status-lint.yml`), so a Dockerfile-only change does not trigger the Go suite. There is no local `make` target; reproduce a run with `docker run --rm -i hadolint/hadolint hadolint --failure-threshold style - < cmd/gmc/Dockerfile`.

## Security scanning

The `security-scan.yml` workflow runs three gates on every PR (and on push to `main`), independent of the unit/integration/e2e suites — two supply-chain scans plus a Kubernetes posture scan. All three have local equivalents so you can reproduce a CI verdict before pushing.

**govulncheck** — scans each workspace module for vulnerabilities reachable from our code (Go stdlib + dependency CVEs). It is symbol-precise: a CVE in a dependency only fails the gate if our code actually calls the affected path. Run it locally with:

```
make vulncheck
```

A finding usually means bumping the Go toolchain (`go` directive in `go.work` + every `go.mod`, kept in lockstep) for a stdlib CVE, or `go get`-ing the fixed dependency version for a module CVE.

**trivy** — builds each of the six images and scans it for fixable HIGH/CRITICAL CVEs in OS packages and bundled libraries. Run it locally (requires `trivy` and `docker` on `PATH`) with:

```
make trivy-scan
```

The five images we build from a minimal/distroless or scratch base (`gmc`, `agc`, `proxy`, `fakegithub`, and the `FROM scratch` `wrapper`) **block** the gate — every package in them is one we chose, so a finding is actionable by bumping a dependency or the base digest. The `worker` image is built `FROM` the upstream `ghcr.io/actions/actions-runner` and inherits CVEs in the bundled node20 runtime and the runner's own Go binaries that we cannot fix without forking the runner; its leg is the sole **report-only** one (findings printed, never blocks). Runner-base CVEs are reduced by bumping the pinned tag — automated via the `docker` ecosystem in `dependabot.yml` and tracked in [`STATUS.md`](../STATUS.md) Q70.

The same `trivy` job also generates an **SBOM** (Software Bill of Materials, SPDX-JSON, via [`syft`](https://github.com/anchore/syft)) for each image it builds and uploads it as a `sbom-<image>.spdx.json` build artifact. This runs on every code PR purely so the SBOM-generation path can't silently break before a release — it does **not** sign or publish anything. On a `v*` release tag, the separate [`publish.yml`](../../.github/workflows/publish.yml) workflow pushes the five first-party images (`gmc`, `agc`, `proxy`, `worker`, `wrapper`) to GHCR, regenerates each SBOM for the pushed image, signs every image **keyless** with [`cosign`](https://docs.sigstore.dev/) (sigstore/Fulcio via GitHub Actions OIDC — no signing key or stored secret), and attaches the SBOM as a keyless cosign attestation. Operator-facing verification (`cosign verify`, SBOM retrieval) is documented in [security-operations.md § Image provenance](../operations/security-operations.md#image-provenance-signature--sbom-verification). The signing/attestation steps run only on publish, so PR CI does not exercise them.

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
- **kubeconform** schema-validates against the cluster API at the chart's `kubeVersion` floor (1.30.0 — validating the oldest supported version catches a field that does not exist there): the controller-gen manifests + the two ValidatingAdmissionPolicies under `cmd/*/config/` (the codegen + envtest substrate; there is no longer a kustomize overlay to render), and `helm template` output in digest-pinned, dev/test opt-out (`allowFloatingImageTags=true`), and all-optional-features form, plus `helm lint` on the chart and a fail-closed check that a render with any of the four image digests (`gmc`/`agc`/`proxy`/`wrapper`) empty is **rejected** — each image is tested independently with the other three pinned (all four required — Q96/Q307 secure-by-default; the check fails if any rejection ever stops happening). `-ignore-missing-schemas` skips only third-party/custom kinds whose schema is not in the upstream Kubernetes set (cert-manager `Certificate`/`Issuer`, the Prometheus Operator `ServiceMonitor`, and our own `ActionsGateway`/`RunnerGroup` CRs); the `CustomResourceDefinition`s that define them **are** validated, since that is a native `apiextensions` kind.

The tool versions are pinned in the workflow (`KUBECONFORM_VERSION`, `YAMLLINT_VERSION`); bump them deliberately, since a new kubeconform can change validation behaviour. CI persists kubeconform's downloaded JSON schemas in an `actions/cache` keyed on the validated Kubernetes version so runs do not re-fetch the schema set from GitHub.
