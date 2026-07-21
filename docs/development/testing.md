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

### The `make check` pre-review gate

For the one-command gate before requesting review, run `make check` from the repo root. It runs gofmt, `golangci-lint`, the `docs/STATUS.md` format lint, the plan-index/no-plan-refs drift gates (`make plan-index-check no-plan-refs-check`, which assert active plans in `docs/plan/README.md` are still STATUS-referenced and that Go code cites no `docs/plan/` paths), the single-Go-version gate (`make go-version-check`, which asserts the `go` directive matches across `go.work`, every `go.mod`, and every `go.work.gen`), `shellcheck` over the helper scripts (see [the shell-lint gate](#the-shellcheck-gate) below), the chart CRD/RBAC/webhook drift gates (`make chart-crds-check chart-rbac-check chart-webhook-check`, which fail if the Helm chart's CRD/RBAC/webhook templates drifted from their sources), the scripts behavioural tests (`make scripts-test`), the Markdown link/anchor check (see [the doc-link gate](#the-doc-link-gate) below), and the unit tests with the coverage ratchet ([`make cover-check`](#coverage-measurement-and-the-ratchet) below, which supersets `make test` — the same unit-test packages, run once per module with `-cover`, plus the per-module coverage floor). This is the fast local loop and covers the lint, unit-test *logic*, and coverage gates the `.github/workflows/unit-test.yml` + `coverage` CI jobs enforce. The one CI step `make check` does **not** reproduce is the race detector: the CI `unit-test` job runs the same per-module unit tests under `-race` (see [the race gate](#the-race-detector-unit-gate) below), which roughly doubles their runtime. Reproduce that locally with `make test-race` — kept out of `make check` so the default dev gate doesn't become an unthrottled `-race` run. The slower security gates (`make vulncheck`, `make trivy-scan`, `make polaris-scan`), the [install-artifact validation](#install-artifact-validation) (`make manifest-validate`), and the integration/e2e tiers below stay separate too so this loop stays fast.

`make check` also **does not** run the three dependency-drift gates — `make vendor-check`, `make tidy-check`, and `license-notices` — because two of them re-fetch modules and can hit the network on a cold cache, which would tax every run to catch a class that lands on ~4% of commits. CI runs all three as their own jobs ([`unit-test.yml`](../../.github/workflows/unit-test.yml) `vendor-check`/`tidy-check`, [`license-notices.yml`](../../.github/workflows/license-notices.yml)), path-gated on the dependency files. The consequence to keep in mind: **a green `make check` does not imply a green `unit-test.yml` when a change touches `go.mod`/`go.sum`/`vendor/`/`go.work*`** — the drift gates can still fail on push. So after any dependency change, run `make vendor-sync` (the one-shot remedy) and commit the result before pushing; see [go-workspaces.md § Changing dependencies](go-workspaces.md#changing-dependencies). As a backstop, `make check` prints a one-line reminder (via `scripts/check-dep-advisory.sh`, its last step) whenever the change it sees touches a dependency file — advisory only, it never fails the gate.

Test output is non-verbose by default: `go test` prints one `ok <pkg>` line per passing package and the full output of any package that fails (compress success, expand failure). When debugging a **slow or hanging** test, add `V=1` (`make check V=1` or `make test V=1`) to stream output live — without `-v`, `go test` buffers each package's output until the package completes, so a hung test shows nothing (not even its `t.Log` lines) until it finishes or hits `-timeout`.

A sub-second subset (gofmt on staged Go files + the STATUS.md lint) also runs automatically at commit time via the tracked pre-commit hook in `.githooks/`. Install it once with `make hooks` (or `scripts/setup.sh`); bypass a single commit with `git commit --no-verify`.

#### Resource auto-throttle on GUI dev machines

`make lint`/`make test`/`make check` lint each module with `golangci-lint` (which fans out one worker per logical CPU and ignores `GOMAXPROCS`/`GOFLAGS`) and run `go test` across every module. On a small machine this can saturate every core and make the desktop unresponsive. On macOS it is worst: the WindowServer compositor misses its kernel watchdog and restarts — the whole GUI freezes (it shows up as `WindowServer … userspace_watchdog_timeout` in **Console ▸ Crash Reports**). On a Linux/WSL desktop you instead get input lag and compositor stutter while the build runs.

To prevent that, these phases auto-throttle on an **interactive, GUI-bearing dev shell**: the scripts behind the make targets (`scripts/go-test.sh`, `scripts/go-lint.sh`, `scripts/coverage.sh`) run them at a low-priority Quality of Service (QoS) tier that demotes both CPU **and** disk I/O below the desktop (macOS: `taskpolicy -c utility`; Linux/WSL: `nice -n 19`, plus `ionice -c 3` when available), and cap parallelism to physical-cores − 2 (`golangci-lint -j`, `go test -p`, `GOMAXPROCS`). Detection and sizing live in [`scripts/local-throttle.sh`](../../scripts/local-throttle.sh).

On macOS the I/O demotion matters as much as the CPU demotion: an unthrottled build already runs at a lower QoS than WindowServer yet still trips the watchdog, so the fix is throttling the build's I/O so the compositor's I/O isn't stuck behind it — and `taskpolicy` is the only macOS way to express that (there is no `ionice`). The gentler `utility` tier is used rather than the lowest `background`/`-b` band because it delivers the same protection while letting builds finish 2–4× faster.

The parallelism cap bounds **one** run's fan-out, but it is blind to siblings: several worktree/sessions each running `make check` (or `make lint`/`make test`) at once still collectively saturate a small core count, and then every phase stretches — most visibly `golangci-lint`, which counts the wait for its own parallel-runner lock against its deadline and starts reporting timeouts. So the heavy phases also hold a **machine-wide advisory lock** (`serialize_heavy_build` in [`scripts/lib/common.sh`](../../scripts/lib/common.sh), path from `scripts/local-throttle.sh lockfile`): concurrent runs queue and each runs at full throttle in turn rather than trampling each other. The lock file lives in the per-user cache dir (`~/Library/Caches/github-actions-gateway/` on macOS, `${XDG_CACHE_HOME:-~/.cache}/github-actions-gateway/` on Linux), **outside** any worktree, so the main checkout and every `.claude/worktrees/*` clone coordinate on the same file. It is implemented with `perl`'s `flock` — a blocking advisory lock present on both macOS (which ships no `flock(1)`) and Linux, released automatically when the holder dies, so a Ctrl-C'd build never strands a stale lock. Like the throttle itself it activates only on a GUI dev shell; CI/headless/SSH report no lock file and run fully parallel and unserialized. (The `golangci-lint` `run.timeout` in `.golangci.yml` was also raised from 5m to 10m so a run that *does* queue behind a sibling has slack; CI is uncontended and never approaches it.)

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

The point is wall-clock under contention: most branches touch one or two modules, sessions run `make check` several times, and every full sweep holds the [machine-wide heavy-build lock](#resource-auto-throttle-on-gui-dev-machines) while sibling worktree sessions queue behind it. Scoping shrinks both the run and the lock hold.

#### Build and lint caches across worktrees

Parallel sessions each run `make check` from their own `.claude/worktrees/*` clone, which raised the question of whether every fresh worktree pays a cold cache (Q343). Measured (2026-07): **the two big caches are already machine-shared and path-independent at their defaults — do not repoint them.** Setting `GOCACHE`/`GOLANGCI_LINT_CACHE` to per-repo or per-worktree dirs was the proposed remedy and is a measured no-op (per-worktree dirs would actively *lose* sharing).

- **Go build cache** (`GOCACHE`, default `~/Library/Caches/go-build` on macOS, `~/.cache/go-build` on Linux). Compile artifacts are content-keyed and hit across worktree paths: compiling the `broker` module against an empty cache took ~12 s in one worktree and ~0.6 s in a second worktree sharing that same cache.
- **golangci-lint analysis cache** (`GOLANGCI_LINT_CACHE`, default `~/Library/Caches/golangci-lint` / `~/.cache/golangci-lint`). Also shared and path-independent for the expensive analysis: after a content change, linting `cmd/agc` costs ~2 min *once machine-wide*; the next worktree at the same content pays only ~9 s of per-path overhead (`go list`/export-data regeneration), and an in-place rerun ~2 s. Entries are even shared between `.build/golangci-lint` binaries built with different Go patch versions — there is no per-binary cache salt.

What a fresh worktree still pays, and why it stays:

- **One unit-test re-run.** `go test`'s *result* cache is path-keyed: a result cached in one worktree does not hit from another worktree even at an identical commit (verified — same package shows `(cached)` in place but re-runs in a sibling). Each new worktree therefore executes the unit suite once; compilation still hits the shared build cache, and there is no supported knob to share test *results* across paths.
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

`make shellcheck` runs `shellcheck` over every tracked shell script under `scripts/` and is wired into `make check`, so the local pre-review gate matches CI. The dedicated `shellcheck` job in `.github/workflows/unit-test.yml` runs the same `make shellcheck` target, gated on a `scripts` paths-filter (`scripts/**`, the `Makefile`, and the workflow itself) so a scripts-only change doesn't also trigger the full Go lint.

**The CI job pins shellcheck** (`SHELLCHECK_VERSION` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) is the source of truth) rather than using `ubuntu-latest`'s preinstalled copy — that version drifts with the runner image, and shellcheck's heuristics (e.g. when SC2015 fires on `A && B || true`) differ between releases, so an unpinned gate gives a different verdict locally vs. CI. Install the **same** version locally so `make shellcheck` matches the gate: see <https://github.com/koalaman/shellcheck#installing> (the target prints this hint if shellcheck is missing). The pin is bumped automatically by updatecli (see [dependency-updates.md](dependency-updates.md)); when its PR lands, install the new version locally to match.

The file set is the git pathspec `scripts/*.sh` resolved through `git ls-files` — **tracked-only and recursive**: git's default `*` spans `/`, so the one pathspec already covers a future `scripts/<subdir>/*.sh` without re-touching the gate, while untracked scratch scripts are skipped. This complements `actionlint`, which only lints the inline `run:` blocks in workflows; before this gate the standalone helper scripts (`setup.sh`, `kind-with-registry.sh`, …) shipped unlinted.

Accepted findings carry a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (see the dynamic-name `read`/`export` in `scripts/probe-investigations-cd.sh`); everything else is fixed to match the repo bash conventions listed in [`scripts/README.md`](../../scripts/README.md).

### The doc-link gate

`make doc-links` runs `scripts/check-doc-links.sh` over every tracked, non-vendored Markdown file and is wired into `make check`, so the local pre-review gate matches CI. CI runs the same `make doc-links` target from its **own** workflow, [`.github/workflows/doc-links.yml`](../../.github/workflows/doc-links.yml), scoped (via `on.paths`) to `**.md`, the checker, and the workflow itself. It is deliberately separate from `unit-test.yml` — that workflow path-ignores docs, so a docs-only change triggers only this lightweight check and never the Go suite (mirroring how `e2e-test.yml` is its own workflow).

It fails on two classes of breakage: **dead relative file links** (a `[text](path)` whose resolved target is neither a tracked file nor directory — a trailing `:NN` line reference is tolerated and only the file part is resolved) and **dead anchors** (a `#fragment` that matches no heading slug or explicit `<a id>`/`<a name>` in the target Markdown file). Anchors are resolved with GitHub's heading-slug algorithm (strip inline markdown — respecting code spans — lowercase, drop everything outside `[a-z0-9 _-]`, spaces to hyphens, de-dupe repeats with `-1`/`-2`), so the verdict matches what GitHub renders. External URLs (http/https/mailto/tel), links inside fenced or inline code, and anchors into non-Markdown or vendored targets are out of scope.

### The conflict-marker gate

`make conflict-markers-check` (`scripts/check-conflict-markers.sh`) fails when any tracked, non-vendored file contains a leftover merge-conflict marker line — the seven-character `<<<<<<<` / `=======` / `>>>>>>>` forms or diff3's `|||||||`. It exists because an edit-based conflict resolution can miss a marker sitting just outside the text it replaced, and format-aware linters skip lines they don't parse; exactly that combination let a stray marker merge to `main` via PR #724 (Q379; fixed same day in PR #730). Wired into `make check`; CI runs the same target from its own lightweight workflow, [`.github/workflows/conflict-markers.yml`](../../.github/workflows/conflict-markers.yml), on **every** PR with no path filter — a marker can be left in any file type. Only exact seven-character marker lines are flagged, so Markdown setext underlines of any other length stay legal, as do mid-line mentions like the backticked examples in this paragraph; the vendored trees are excluded. The pattern logic is asserted by `scripts/check-conflict-markers-test.sh` under `make scripts-test`. When resolving conflicts by hand, `git diff --check` gives the same signal per-file before you stage.

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
- `github-real` — the Tier C specs that dispatch against real GitHub (`E2E_GitHub_RealDispatch`); they self-skip when the `E2E_GITHUB_*` env vars are unset.
- `real-github-egress` — the specs whose traffic terminates at the live `api.github.com`: the v1/v2 `ProxyConnectWorks` CONNECT specs, the two `E2E_V2_DirectEgress` specs (their NP ipBlock-peer waits also depend on the GMC's live `/meta` fetch), and the Tier C container. Not a filter label: a suite-level `AfterEach` (`cmd/gmc/test/e2e/github_egress_preflight_test.go`) uses it for failure-time attribution — see [Runner→GitHub egress attribution](#runnergithub-egress-attribution-q352).

For a faster local inner loop on a 1-worker cluster, `make e2e SUITE=single-node` maps to `--label-filter '!multi-node'` and skips the multi-node specs; unset `SUITE` runs everything (matching CI). The HPA scale-up spec (`E2E_GMC_HPADrivesScaleUp`) is unlabelled and CI-safe: it patches `HPA.spec.minReplicas` to drive the HPA→Deployment control path deterministically rather than burning CPU to trigger autoscaling, so it runs everywhere.

**Waiting for the AGC, not just its Deployment.** A spec that waits for a broker session (or anything else that needs the AGC operational) must gate on `utils.WaitForRunnerGroupReconciled`, not only `utils.WaitForDeploymentReady`. Deployment readiness means only that the AGC's health server is up — it binds within seconds of pod start and is deliberately decoupled from the GitHub-App token fetch (`cmd/agc/main.go`), whose budget alone is up to ~2 minutes. `WaitForRunnerGroupReconciled` waits for `RunnerGroup.status.observedGeneration` to be set, which the AGC does only after token + agent registration + listener-multiplexer start all succeed. Gating on Deployment readiness alone folds the AGC's whole startup into the session wait's budget, which under parallel CI load (token/registration/session round-trips to the shared single-replica fakegithub) can exhaust it and surface as a misleading "no session registered" timeout (Q134).

**Tier C.** Set `E2E_GITHUB_APP_ID`, `E2E_GITHUB_APP_INSTALLATION_ID`, `E2E_GITHUB_APP_PRIVATE_KEY`, `E2E_GITHUB_ORG`, and `E2E_GITHUB_REPO` in the environment, then run `make e2e` (Tier C specs skip themselves at runtime when any variable is missing). The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

## CI workflows and scripts

CI must use the same commands as [Running tests](#running-tests) above — per-module invocations or the explicit multi-module patterns `scripts/go-test.sh` builds; never `go test ./...` from the repo root, which does not work with the Go workspace layout.

### Path-gated workflows: verify the heavy gates actually ran

Most code-exercising workflows keep unrelated PRs cheap by **skipping their expensive jobs internally** rather than skipping the whole workflow. The build/lint/test/security gates (`unit-test.yml`, `integration-test.yml`, `e2e-test.yml`, `e2e-calico.yml`, `security-scan.yml` — trivy + govulncheck, `manifest-validate.yml`, `license-notices.yml`, plus `status-lint.yml` and `plan-hygiene.yml`) trigger on **every** `pull_request` (no top-level path filter), then a `dorny/paths-filter` `changes` job classifies the diff and each real job's `if:` guard skips it when nothing it covers changed. Each workflow ends with a small **`<workflow>-gate`** job (`unit-test-gate`, `security-scan-gate`, …; `if: always()`, `needs:` every real job) that passes only when each concluded `success` or `skipped` — this is the job whose check context is (or is intended to be) the branch's **required status check**. The ids are unique per workflow on purpose: a normal job's check-run name **is its job id**, GitHub matches required checks by that name, so nine jobs all named `gate` would collapse to one indistinguishable entry in the ruleset UI. See [required-status-checks.md](../plan/archive/required-status-checks.md).

**Why not the simpler top-level `paths-ignore`:** a workflow skipped by a top-level path filter reports **no check at all**, which leaves a *required* check **Pending forever** and wedges the merge. Triggering on every PR and gating internally means the `gate` context always reports — green (all jobs skipped) on an unrelated PR, red when a real job fails — so it is safe to require.

**The historical gotcha this closes — a PR going green/`CLEAN` without ever testing its code.** Under the old top-level `paths-ignore`, a PR **opened while docs-only** with code **added in a later push** could leave the path-gated workflows **skipped** (the `synchronize` did not reliably re-trigger them; see [actions/runner#2324](https://github.com/actions/runner/issues/2324)), so the PR showed all-green with the code never built or tested. Because the workflows above now trigger on every PR and re-evaluate the diff via the `changes` job on each push, this specific skip-through no longer applies to them. The `gate` contexts are now marked required in the ruleset, so a red or Pending gate blocks the merge.

**The `changes` job fails open (Q363).** `dorny/paths-filter` resolves the changed-file list through a **single un-retried GitHub API call** — `@actions/github`'s Octokit carries no retry plugin — so one transient 5xx or reset used to fail the `changes` job and, through `needs:`, the whole gate. Worse, a JavaScript action reports failure *only* as an `::error::` annotation, and a rerun replaces the check run and destroys that annotation: the surviving log simply stopped after `Invoking listFiles` with no error recorded anywhere, which is why the original occurrence looked like a silent failure. Each `changes` job now carries `continue-on-error: true` on the paths-filter step and derives its outputs as `steps.filter.outcome == 'success' && steps.filter.outputs.<name> || 'true'`, so an unclassifiable diff **runs every gated job** instead of skipping it — fail-open on *detection*, still fail-closed on *validation*. A step that trips it also emits a `::warning::` so the degradation is visible in the run log rather than inferred. Note this is deliberately not a retry: re-running the classifier could still return a wrong answer, whereas running the gated jobs is always safe.

**Verify before declaring a PR review-ready and before merging it:** confirm the gates that exercise the change actually executed on the PR head — green is not enough if a gate was skipped. For any Go / CRD / chart change you should see runs for `build`, `lint`, `integration-test`, `e2e`, `security-scan` (trivy + govulncheck), and `manifest-validate`:

```bash
gh pr checks <n>                         # are the heavy gates present and passing — or absent?
gh run list --branch <branch> --limit 30 # cross-check which workflows actually ran on the head commit
```

**Fix it if they were skipped:** `gh pr close <n> && gh pr reopen <n>`. The `reopened` event re-evaluates the path filters against the full PR diff and triggers the skipped workflows. Re-watch them to completion before treating the PR as tested.

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
