# Agent reference: Testing

## Running tests

The repo is a Go workspace (`go.work`), so `go test ./...` from the repo root does **not** work; run tests per module, or use explicit per-module patterns from the root (`go test ./broker/... ./cmd/agc/...`), which the workspace does resolve.
See [go-workspaces.md](go-workspaces.md) for why. `make test` runs the whole workspace as **one** multi-module `go test` invocation (all `./<module>/...` patterns at once) so the modules compile and test as a single parallel build graph instead of serially (Q17).

```bash
(cd broker     && go test ./...)    # broker module
(cd githubapp  && go test ./...)    # githubapp module
(cd cmd/agc    && go test ./...)    # AGC module
(cd cmd/gmc    && go test ./...)    # GMC module
(cd cmd/probe  && go test ./...)    # probe module
(cd cmd/proxy  && go test ./...)    # proxy module
(cd cmd/worker && go test ./...)    # worker module
```

Run tests locally before pushing to a PR to avoid burning CI.
Prefer the narrowest scope that covers the change: a single module's unit tests, `-run` to target a specific test, integration tests for controller changes, or `--focus` for a targeted e2e spec.
Run the full e2e suite only when the change is broad enough to warrant it.

### The inner loop: cheap checks while iterating, `make check` once pre-PR

Run the full gate **once, before opening the PR**.
While iterating, use the two cheap checks that cover what you actually changed:

```bash
make lint
```

```bash
(cd cmd/agc && go test ./...)
```

`make lint` is change-scoped locally — gofmt over every module, `golangci-lint` only over the modules the branch can affect (see [Change-scoped lint on local runs](#change-scoped-lint-on-local-runs)), and it skips the heavy-build lock entirely when a change has no Go-lint effect.
Pair it with the **module-scoped** unit tests for the module you touched; [Running tests](#running-tests) above lists the invocation for each one.
This is a Go workspace, so `go test ./...` from the repo root does not work — the `(cd <module> && …)` form is the one that does.
Narrow further while chasing a single failure:

```bash
(cd broker && go test -run TestSessionMux ./...)
```

Then, once, when the work is done and you are about to open the PR:

```bash
make check
```

**Why the split.** `make check`'s heavy phases take a [machine-wide heavy-build slot](#resource-auto-throttle-on-gui-dev-machines), of which there are only a couple, so repeat runs — yours plus every sibling worktree session's — queue behind each other once the slots are full.
Over a 21-day sample of session transcripts, a session averaged 3–4 full runs at a median of 2 minutes but a **p90 of 9 minutes**, the tail being lock wait (Q375).
Each extra run also mostly re-verifies code the branch never touched: the unit/coverage step ([`make cover-check`](#coverage-measurement-and-the-ratchet)) measures **every** workspace module regardless of change scope, so only the lint phase gets any benefit from scoping.
Running it three times mid-iteration buys almost nothing the module-scoped loop above did not already tell you.

Add a heavier tier only when the change warrants it — `make test-race` for the concurrency core, `make test-integration` or the e2e targets for controller and cluster behaviour, `make vulncheck`/`make trivy-scan` for dependency and image changes — and give each one an [explicit timeout or a background run](#slow-tiers-need-an-explicit-timeout-or-a-background-run).

One caveat on the inner loop: the `make` targets throttle themselves, but a bare `go test` run directly does not.
That is harmless for a plain module-scoped run and **not** harmless with `-race` — use `make test-race`, or prefix a manual run with `$(scripts/agent/local-throttle.sh prefix)`.
See [Resource auto-throttle on GUI dev machines](#resource-auto-throttle-on-gui-dev-machines).

### Narrowing a run with `RUN=`

Every test target takes `RUN=` to narrow the run to matching tests.
One spelling across the tiers, but two filters underneath:

| Target | Reaches | Matches against | Zero matches |
|---|---|---|---|
| `make test`, `make test-race` | `go test -run` | the Go test-function name | **fails** |
| `make e2e` | `ginkgo --focus` | the spec's full text | **fails** |
| `make test-integration`, `make -C cmd/agc\|cmd/gmc test-integration` | `go test -run` | the Go test-function name | passes (Q736) |

Both filters are regexes, so `RUN='TestMerge_'` selects a family and `RUN='provisions a worker pod'` selects one spec by its description.
On `make e2e`, `RUN` composes with [`SUITE`](#end-to-end-tests): `SUITE` picks a labelled subset, `RUN` picks specs inside it.

```bash
make test RUN=TestSessionMux
make e2e SUITE=single-node RUN='E2E_GMC_ProxyServiceCreated'
```

**A filter that matches nothing is a failure, not a pass.** Left alone, both tools report success on a miss: `go test -run` prints `[no tests to run]` and exits 0, and ginkgo exits 0 having run 0 specs.
A mistyped name then reads exactly like the test passing, which is the knob's whole purpose inverted (Q680). `make test` therefore fails when no test ran in **any** module, and `make e2e` passes `--fail-on-empty`, which also catches a `SUITE` that selects nothing:

```
==> RUN='TestNoSuchThingAnywhere' matched no tests in any module — nothing ran
FAIL! - Detected no specs ran and --fail-on-empty is set
```

`RUN=` also forces `-v -count=1` on the Go tiers: a targeted run wants the test's own output, and a cached `PASS` prints none.

### The `make check` pre-review gate

For the one-command gate before requesting review, run `make check` from the repo root.
To see exactly what it runs, run `make list-gates`: it prints every gate in execution order with what each one covers, rendered from the `CHECK_FAST_GATES` and `CHECK_HEAVY_GATES` variables the `check` target itself expands.
That is the list — this page names the target rather than transcribing it, because the transcription drifted: it went the whole life of `license-header-check` and `conflict-markers-check` without mentioning either, while `make check` ran both (Q649). `make list-script-tests` does the same for the `scripts/` suites the `scripts-test` gate fans out over, for the same reason: that list was transcribed into a 1,399-character help line naming 50 of its 55 suites (Q671).

The shape is two phases.
First a concurrent fan-out of the cheap gates — the `docs/STATUS.md` and roadmap/plan-index coherence lints, the single-Go-version and license-header and conflict-marker checks, the v2 API and CI path-filter reconciliations, `shellcheck` and `actionlint`, the chart and codegen and API-reference drift gates, the Markdown link check, and the `scripts-test` and `claude-usage-test` suites — none of which takes a [heavy-build slot](#resource-auto-throttle-on-gui-dev-machines).
Then three sequential heavy phases: [`build-tags-check`](#the-build-tag-gate), `lint` (gofmt across all modules plus `golangci-lint`), and [`cover-check`](#coverage-measurement-and-the-ratchet), which supersets `make test` — the same unit-test packages, run once per module with `-cover`, plus the per-module coverage floor.
Each of the three takes a machine-wide slot, so they cannot usefully overlap.
The gates with behaviour worth knowing have their own `### The … gate` section below.

It covers the lint, unit-test *logic*, and coverage gates the `.github/workflows/unit-test.yml` + `coverage` CI jobs enforce — run it once when the work is done, not after every edit ([the inner loop](#the-inner-loop-cheap-checks-while-iterating-make-check-once-pre-pr) above is what you iterate with).
The one CI step `make check` does **not** reproduce is the race detector: the CI `unit-test` job runs the same per-module unit tests under `-race` (see [the race gate](#the-race-detector-unit-gate) below), which roughly doubles their runtime.
Reproduce that locally with `make test-race` — kept out of `make check` so the default dev gate doesn't become an unthrottled `-race` run.
The slower security gates (`make vulncheck`, `make trivy-scan`, `make polaris-scan`), the [install-artifact validation](#install-artifact-validation) (`make manifest-validate`), and the integration/e2e tiers below stay separate too so this loop stays fast.

`make check` also **does not** run the three dependency-drift gates — `make vendor-check`, `make tidy-check`, and `license-notices` — because two of them re-fetch modules and can hit the network on a cold cache, which would tax every run to catch a class that lands on ~4% of commits.
CI runs all three as their own jobs ([`unit-test.yml`](../../.github/workflows/unit-test.yml) `vendor-check`/`tidy-check`, [`license-notices.yml`](../../.github/workflows/license-notices.yml)), path-gated on the dependency files — plus, for `tidy-check`, on `**/*.go`, because an import edit alone can untidy a `go.mod` (Q545; see [go-workspaces.md § Changing dependencies](go-workspaces.md#changing-dependencies)).
The consequence to keep in mind: **a green `make check` does not imply a green `unit-test.yml` when a change touches `go.mod`/`go.sum`/`vendor/`/`go.work*` — or adds and drops imports** — the drift gates can still fail on push.
So after any dependency change, run `make vendor-sync` (the one-shot remedy) and commit the result before pushing; see [go-workspaces.md § Changing dependencies](go-workspaces.md#changing-dependencies).
As a backstop, `make check` prints a one-line reminder (via `scripts/ci/check-dep-advisory.sh`, its last step) whenever the change it sees touches a dependency file — advisory only, it never fails the gate, and it does not fire on an import-only change.

**A background run's verdict covers only the tree it saw.** `make check` is routinely launched as a *background* task so the doc updates, the `docs/STATUS.md` row, and the PR body can be written while it runs — that is the recommended shape ([parallel-dispatch.md § Run the local gate in the background](parallel-dispatch.md#run-the-local-gate-in-the-background-not-on-the-critical-path)), but it means a green report can describe a tree that no longer exists.
Every edit made while the gate is running is unverified, and that includes the parallel work itself: `docs/STATUS.md`, `docs/**`, and the plan docs are gated by `lint-backlog`, `doc-links`, `roadmap-check`, and `plan-index-check`. **Re-run `make check` over the final tree before concluding.** The confirming run is cheap — the gates covering that work are the fast ones, which take no heavy-build slot, and the heavy phases are cache-warm.
A **code** edit voids the verdict outright rather than merely narrowing it, and "code" means anything the gate compiles or lints: `scripts/*.sh` and the `Makefile` count, not only Go.

**The exit code you read has to belong to the gate.** A verdict is only as good as the command that reported it, and the usual way that breaks is wrapping the gate in something that has an exit status of its own.
Three shapes, all seen in real sessions:

- `make test-race > run.log 2>&1; grep -c "DATA RACE" run.log` — `grep` exits **1 when it matches nothing**, which here is the *passing* outcome.
  The chain reports failure precisely when the gate was clean.
  Keep the gate's own status (`make …; echo "EXIT=$?"`) and make the log search a separate statement, or invert to `grep -q … && echo FOUND || echo none`.
- `git add a.go b.go c.md 2>/dev/null` — `git add` fails **atomically** if any pathspec matches nothing (a file since renamed or `git mv`d), so one stale path stages *nothing*, and `2>/dev/null` throws away the one message that said so.
  The commit then goes out missing everything you thought you staged.
  Never redirect stderr away from a state-changing command, and read `git status` right before committing — that is the backstop that catches it.

- `make check > tmp/check.log 2>&1; echo "EXIT=$?" >> tmp/check.log`, **run as a background task** — the log records the gate's status correctly, and that is why the `echo` is there; but the *compound's* own status is the `echo`'s, which always succeeds.
  So the task-completion notification reports **success for a red gate**.
  Observed 2026-08-03: a notification read `exit code 0` while the run had failed `doc-links`.
  The fix is not to drop the `echo` — the log line is what you read — it is to never take the notification as the verdict.
  Reconcile it against the `EXIT=` line *and* a failure search of the log (`grep -nE 'FAILED|Error [0-9]|^make:'`) before reporting green.

The general form: when a command's output is filtered, tested, or counted, the *filter's* status replaces the command's — and when it is merely *followed* by another statement, the last statement's status replaces it.
Read both, or read neither and check the artifact.

**Run order.** The cheap gates (everything except `lint` and `cover-check`) take no heavy-build slot and are independent, so `make check` runs them concurrently through [`scripts/ci/run-parallel.sh`](../../scripts/ci/run-parallel.sh) and reports them first — a `docs/STATUS.md` format slip surfaces in seconds instead of waiting out the unit suite.
Every line is prefixed with its gate's label, so a failure stays attributable.
(`make -j` is not used: macOS ships GNU make 3.81, which has no `-O` output sync, so two failing gates would interleave unreadably.)
The heavy phases then run in sequence, each taking a slot of its own.

Test output is non-verbose by default: `go test` prints one `ok <pkg>` line per passing package and the full output of any package that fails (compress success, expand failure).
When debugging a **slow or hanging** test, add `V=1` (`make check V=1` or `make test V=1`) to stream output live — without `-v`, `go test` buffers each package's output until the package completes, so a hung test shows nothing (not even its `t.Log` lines) until it finishes or hits `-timeout`.

A sub-second subset also runs automatically at commit time via the tracked pre-commit hook in `.githooks/`: gofmt on staged Go files, the STATUS.md lint, and [`em-dash-check`](documentation-standards.md#enforcing-the-em-dash-rule) when any Markdown is staged.
Each part is skipped unless its file type is staged, so most commits pay nothing.
Install it once with `make hooks` (or `scripts/dev/setup.sh`); bypass a single commit with `git commit --no-verify`.

The em-dash part is there for *when* it fires rather than for coverage, since `make check` already runs it.
It is the gate a docs change most often trips at the very end of a full run, after the heavy phases are paid for, and commit time is the first moment the answer is both cheap (410 ms warm) and unavoidable.
It holds one branch to its own ceiling; two branches each sitting *at* a ceiling and merging over it is a property of the merge result, not of either commit (Q742, Q743).

#### Measuring the local gate: start from what is already recorded

Before benchmarking any change to the throttle, the parallelism cap, or the slot count, read the phase costs already measured in [local-gate-throughput.md](../plan/archive/local-gate-throughput.md) and pick a workload from them.
Two facts there decide the experiment, and re-deriving them costs a session:

- **`golangci-lint` is what saturates.** It fans out one worker per logical CPU and ignores `GOMAXPROCS`, so it — with coverage — is where `make check` spends its time. **`-race` is not part of `make check` at all**, and the full workspace race tier measures ~12–14 % mean CPU on an 18-core machine: far too little to tell two configurations apart.
- **Only the cold-cache case is CPU-heavy — and this applies to every tier, not just lint.** Since `-trimpath` made the test-result cache shared across worktrees, a warm gate runs at ~12–14 % CPU and no throttle setting is load-bearing. **`GOCACHE` is the cache that has to be cold**, not `GOLANGCI_LINT_CACHE`: bust only the latter and a module lints in ~1 s off the warm Go build cache.
  The same trap catches the unit tier — `go test -count=1` with a warm build cache defeats only the *result* cache, leaving a few suites waiting on timers and envtest, which measured 18–36 % CPU and an identical 40 s at every `jobs` from 4 to 24 (`INVALID`).
  Warm, every tier here is execution-bound and cannot discriminate between settings; bust `GOCACHE` or expect a null.

**Confirm the workload actually loaded the machine before believing any comparison.** Sample CPU busy% for the duration of each run and discard a trial where no configuration reaches ~50 %: an undersized workload returns identical timings for every configuration and reads as a clean result rather than an absent one.
[`scripts/agent/validate-throttle.sh`](../../scripts/agent/validate-throttle.sh) enforces this and marks such a trial `INVALID`; [`scripts/agent/qos-cluster-probe.sh`](../../scripts/agent/qos-cluster-probe.sh) measures the compute ceiling a candidate prefix can reach.
Calibrate any new instrument against a known-saturating load first — a null result is only evidence if the instrument can see a positive.

**The instruments' parsers are tested; their measurement paths are not.** Both scripts run a macOS-only tool (`iostat`, `vm_stat`, `powermetrics` — the last needs root) and then turn its text into the number the decision rests on.
That second half is plain text-to-number and runs anywhere, so it is asserted on every platform by [`scripts/agent/validate-throttle-test.sh`](../../scripts/agent/validate-throttle-test.sh) and [`scripts/agent/qos-cluster-probe-test.sh`](../../scripts/agent/qos-cluster-probe-test.sh) under `make scripts-test`.
The `iostat`/`vm_stat`/`uijitter` fixtures are output recorded from the real tools; the `powermetrics` ones cannot be (it needs root), so they are reproduced from that binary's own `printf` format strings — `strings -a /usr/bin/powermetrics | grep -E 'Cluster|CPU %u'` — which is what pins line shapes like `E-Cluster HW active residency:  50.00%` rather than a guess at them.
Both scripts guard `main` with `if [[ "${BASH_SOURCE[0]}" == "${0}" ]]`, so the tests source them for their parsers without triggering a measurement — **keep that guard, and keep new parsing in a named function rather than inline in a measurement path**, or it becomes untestable off macOS.
The cases that matter are the ones where a malformed capture yields a plausible number instead of an error: `iostat`'s first data row is a since-boot average that must be dropped (counting it turned a 77 % trial into 66 % — a false `INVALID`), it reprints its header every 20 rows, and a blank line in the capture used to be **fatal**, since awk resolves `$(NF-3)` on a short line to a negative field index and aborts the harness mid-trial.

#### Resource auto-throttle on GUI dev machines

`make lint`/`make test`/`make check` lint each module with `golangci-lint` (which fans out one worker per logical CPU and ignores `GOMAXPROCS`/`GOFLAGS`) and run `go test` across every module.
On a small machine this can saturate every core and make the desktop unresponsive.
On macOS it is worst: the WindowServer compositor misses its kernel watchdog and restarts — the whole GUI freezes (it shows up as `WindowServer … userspace_watchdog_timeout` in **Console ▸ Crash Reports**).
On a Linux/WSL desktop you instead get input lag and compositor stutter while the build runs.

To prevent that, these phases auto-throttle on an **interactive, GUI-bearing dev shell**: the scripts behind the make targets (`scripts/go/go-test.sh`, `scripts/go/go-lint.sh`, `scripts/go/coverage.sh`) run them with both CPU priority **and** disk I/O demoted below the desktop (macOS: `nice -n 10 taskpolicy -d throttle`; Linux/WSL: `nice -n 19`, plus `ionice -c 3` when available), and cap parallelism to physical-cores − 2 (`golangci-lint -j`, `go test -p`, `GOMAXPROCS`).
Detection and sizing live in [`scripts/agent/local-throttle.sh`](../../scripts/agent/local-throttle.sh).

On macOS the I/O demotion matters as much as the CPU demotion: an unthrottled build already runs at a lower QoS than WindowServer yet still trips the watchdog, so the fix is throttling the build's I/O so the compositor's I/O isn't stuck behind it — and `taskpolicy` is the only macOS way to express that (there is no `ionice`).

**Demote the two separately; don't clamp QoS.** The macOS prefix was `taskpolicy -c utility` until Q441.
A `-c` clamp does more than deprioritize — it confines the entire build to one CPU cluster at a pinned frequency (21 % of an M5 Max on synthetic load, 37–39 % mean CPU on a real cold-cache lint), and because `-c` only ratchets QoS *down* there is no higher tier to select. `-d throttle` sets the disk I/O policy on its own and `nice -n 10` supplies the CPU demotion, which returns the idle clusters: **3.6× faster on the cold-cache lint that dominates `make check`, for +1.4 ms of p99 desktop scheduling latency**, with no stutter past 50 ms, no swapins and no WindowServer reports across nine runs.
Keeping the `nice` was free — that variant beat bare `-d throttle` on *both* wall time and p99 — so a demotion of each kind is retained.
Measurements: [local-gate-throughput.md](../plan/archive/local-gate-throughput.md).

The parallelism cap bounds **one** run's fan-out, but it is blind to siblings: several worktree/sessions each running `make check` (or `make lint`/`make test`) at once still collectively saturate a small core count, and then every phase stretches — most visibly `golangci-lint`, which counts the wait for its own parallel-runner lock against its deadline and starts reporting timeouts.
So the heavy phases also take one of **N machine-wide advisory slots** (`serialize_heavy_build` in [`scripts/lib/common.sh`](../../scripts/lib/common.sh), paths from `scripts/agent/local-throttle.sh lockfile [N]`, count from `scripts/agent/local-throttle.sh slots`): N runs proceed, the rest queue, rather than every run trampling the others.
The lock files live in the per-user cache dir (`~/Library/Caches/github-actions-gateway/` on macOS, `${XDG_CACHE_HOME:-~/.cache}/github-actions-gateway/` on Linux), **outside** any worktree, so the main checkout and every `.claude/worktrees/*` clone coordinate on the same files.
They are implemented with `perl`'s `flock` — an advisory lock present on both macOS (which ships no `flock(1)`) and Linux, released automatically when the holder dies, so a Ctrl-C'd build never strands a stale lock.
Like the throttle itself it activates only on a GUI dev shell; CI/headless/SSH report no lock file and run fully parallel and unserialized.
(The `golangci-lint` `run.timeout` in `.golangci.yml` was also raised from 5m to 10m so a run that *does* queue behind a sibling has slack; CI is uncontended and never approaches it.)

**N defaults to 2** (1 on a machine with fewer than 4 physical cores); `GAG_HEAVY_BUILD_SLOTS` overrides it, and `GAG_HEAVY_BUILD_SLOTS=1` restores the original strict "one heavy run machine-wide" behaviour.
Two holders at `jobs` each can oversubscribe the physical cores, and that is the intended trade: the desktop-safety property is the priority demotion — CPU *and* I/O below the compositor — which every holder still carries, while the parallelism cap is a secondary bound.
Strict serialization made the gate, not the work, set the pace on a machine running several sessions: one run used `jobs` threads while every sibling blocked for its whole duration (waits up to 5 h were observed — Q376).
Slot 1 keeps the original single-lock filename so a worktree still on the pre-semaphore code contends with slot 1 rather than running unbounded alongside the new code.

**2 is where the knee is, measured.** Aggregate throughput of M concurrent cold-cache lints, relative to one holder: 2 → 1.25×, 3 → 1.30×, 4 → 1.31×.
Only the second slot pays for itself; beyond it the desktop tail keeps growing for nothing (worst single wake 16.8 ms at 4 holders, past a 60 Hz frame).
A single run already reaches 81–85 % CPU on an 18-core machine, so there is little left for a second to claim — and slot 2 is worth having as much for latency as throughput, since two holders finish in 36.7 s where strict serialization needs 46 s. **Don't raise it without re-measuring**; these numbers are one machine and one repo, and the sub-4-core floor covers a case none of them touch.
Method and full tables: [local-gate-throughput.md](../plan/archive/local-gate-throughput.md).

**A queued run reports itself.** While waiting for a slot it prints `==> waiting for a heavy-build slot (2 in use, queued 90s)...` on entry and every 30 s after, then `==> heavy-build slot acquired after Ns queued` once admitted (elided under 5 s).
This matters most for a gate run in the background, where the log is the only signal there is — a single line followed by open-ended silence is indistinguishable from a hang, which is why the queue depth the semaphore exists to bound was for a long time only anecdotal. **Under a batch of parallel sessions, run the gate in the background and do your docs/PR work while it queues**, then re-run it over the final tree; the mechanics and the reason a start-time stagger does *not* help are in [parallel-dispatch.md § Run the local gate in the background](parallel-dispatch.md#run-the-local-gate-in-the-background-not-on-the-critical-path).

Only the make targets (via their scripts) throttle themselves, so a bare `go build`/`go test` run directly (not via `make`) bypasses it — a heavy `-race` run that way once froze the macOS GUI.
Two safety nets cover that gap, both reusing `scripts/agent/local-throttle.sh` so they share the same activation rules and stay no-ops on CI/headless/SSH:
- **When you call `go` directly, prefix it** with `$(scripts/agent/local-throttle.sh prefix)` (e.g. `$(scripts/agent/local-throttle.sh prefix) go test -race ./...`), or just run it under `make` where a target exists.
- A Claude Code `PreToolUse` hook ([`scripts/agent/claude-go-throttle-hook.sh`](../../scripts/agent/claude-go-throttle-hook.sh), wired in `.claude/settings.json`) automates that prefix for agent-run commands: a bare `go build`/`go test` is rewritten transparently and auto-*allowed*.
  It auto-allows only that bare form — never a compound command (`cd … && go test …`), one with a redirect, or one behind a wrapper — so its `allow` can't carry another segment or an outside-workspace redirect past the permission system or the branch-guard/workspace-guard hooks.
  But the throttle still has to reach the dangerous `-race` amplifier in those forms, so such a command carrying `-race` is **rewritten to insert the prefix before its `go` invocation and returned as an `ask`** (not `allow`): the run is throttled while the confirmation prompt keeps the user and the guard hooks in the loop.
  Only when the throttle cannot be applied to a `-race` at all does it **`deny`**, with the specific reason, rather than let the run through unthrottled: the command holds more than one `go build`/`go test` invocation to throttle (one prefix, two places to put it), or the probe that resolves the prefix could not run.
  For the first, add the prefix manually or split the invocations onto separate commands; for the second, use the `make` target or retry, since a probe that failed under load usually answers on the next call.

The decision is [`devtools/agent/gothrottle`](../../devtools/agent/gothrottle), a Go program over a real shell parser (`mvdan.cc/sh`); the shell file is the entry point that resolves and execs it, and its failure paths (no Go toolchain, a build error, an unparseable command) are silent, so the hook is never the reason a Bash call fails; the one exception is a `-race` whose prefix could not be resolved, below.
It became Go for the same reason its sibling `pipedgate` did (Q708): 178 of the shell version's 423 lines hand-rolled a shell-grammar scanner, which is the parsing-density criterion in [technical-debt.md](technical-debt.md#a-shell-gate-becomes-a-go-devtool-on-parsing-density-not-length).

**Set `GOTHROTTLE_DEBUG` to find out why the hook said nothing.** Silence is the failure contract, and it is also why a failure here is unattributable: every silent path across the entry point and the binary ends in exit 0 with empty stdout, so the suite could only ever report `got decision= reason=` (Q703).
With the variable set, each path names itself on stderr, and the probe that resolves the throttle prefix reports whether it *failed* or reported throttling *off*, which are otherwise the same empty string.
Stdout is untouched either way, so the decision contract is unchanged; the suite sets it on every invocation and prints the trace with any failure.

**A probe that could not run is not a verdict of "throttling is off" (Q785).** `local-throttle.sh` exits 0 with empty stdout to report off, and non-zero only when it broke, so its exit status tells the two apart.
But the empty prefix reaches the decision looking identical, and reading the failure as the verdict silenced the whole decision including the `deny`.
A `-race` under a failed probe is now denied, carrying the probe's error as its reason, since that error is the only account of the failure outside `GOTHROTTLE_DEBUG`.
Everything else still passes through: a plain `go build`/`go test` is not the run that freezes a GUI, and denying it on a fork that failed under load would make the hook the reason an ordinary Bash call fails.

**Expect that deny on a saturated machine**, since the probe's 5 s timeout is what a contended one runs out of: spawning the probe costs ~120–200 ms idle, but measured 5 s+ under a 60-way parallel `make check` (load average 27).
That is the right answer there rather than a nuisance, since the moment the probe cannot answer is the moment an unthrottled `-race` is most expensive, and the reason says to retry or use the `make` target.

What counts as an invocation is a `go` token in **command position** — including behind an allowlisted wrapper (`timeout 900 go test -race …`, Q696), which the scanner missed entirely because `go` is an argument there.
The allowlist (`wrappers` in the Go package) names the wrappers whose own options and operands can be skipped safely; a name it does not list, or an option its spec does not describe, stops the peel and the command passes through unthrottled rather than have the prefix guessed into the wrong place.
Quoted strings and heredoc bodies parse as words rather than commands, so a `git commit -F -` message that quotes `go test -race` is not an invocation (Q624; before that it was, and a message naming it twice was denied outright).
One consequence worth knowing: a `go` inside a heredoc fed to a shell is still not seen.
The behaviour is covered both directions by the package's table test and, end to end, by [`scripts/agent/claude-go-throttle-hook-test.sh`](../../scripts/agent/claude-go-throttle-hook-test.sh) (run under `make scripts-test`).

It is a no-op everywhere the throttle would only slow things down for no benefit, so those runs go at full speed:
- **CI** — the `CI` environment variable is set (GitHub Actions et al.).
- **Headless / SSH Linux shells** — no graphical session (`DISPLAY`/`WAYLAND_DISPLAY` unset), so build servers and remote shells are unaffected.
- **Unsupported OSes** — native Windows (Git Bash/MSYS); use WSL2, which reports as Linux and follows the Linux rule.

To opt out locally (e.g. a machine with cores to spare), set `CI=1` for the run: `CI=1 make check`.

##### Not every WindowServer watchdog crash is a build

The throttle addresses one specific cause of `WindowServer … userspace_watchdog_timeout`: a build saturating CPU **and** disk I/O so the compositor's own work is stuck behind it.
That is a *resource-starvation* stall — WindowServer's main thread is runnable but can't get serviced.
There is a second, unrelated cause that the throttle does **not** fix, and the two look identical in **Console ▸ Crash Reports** (same `userspace_watchdog_timeout` suffix), so confirm which one you hit before assuming a build was at fault:

- **GPU/compositor stall (integrated-graphics contention).** On a Mac with integrated graphics (e.g. the `MacBookPro16,2` 13" with Intel Iris Plus, shared-memory VRAM), WindowServer's main thread can *block* waiting on the GPU/display pipeline to return a frame — not starve for CPU.
  The spin report's reason reads `Display … not ready: DisplayID: 0x…`, WindowServer's own CPU time in the window is tiny (well under 1 s), and the sampled kernel threads name the GPU stack (`AppleIntelICLGraphicsMTLDriver`, `AppleIntelFramebuffer`, `AppleGPUWrangler`, `IntelAccelerator`).
  The driver here is many simultaneous GPU clients on one weak iGPU: each Chromium/Electron app runs its own GPU process (`CrGpuMain`/`GpuWatchdog` — Claude desktop, Chrome, Slack, Discord, VS Code/GoLand), a Virtualization.framework VM adds a `virtio-gpu` client, and a Spotlight (`mds`) reindex piles on.
  No `go` process need be involved, and memory/swap can be near-idle.
  The throttle wrapper cannot help — it only demotes CPU/I/O, not GPU command-queue pressure.

  To tell them apart, read the spin file in `/Library/Logs/DiagnosticReports/WindowServer_*.spin`: a *build* stall shows WindowServer hot or its work blocked behind heavy I/O; a *GPU* stall shows the `Display … not ready` reason and the Intel/GPU driver threads above.
  Mitigate the GPU case by reducing concurrent GPU clients (close unused Electron apps, shut down the VM if headless, let Spotlight finish or exclude worktrees/module caches/Docker data from indexing); a reboot resets the accumulated `N induced crashes` counter.

#### Change-scoped lint on local runs

On a local run, `make lint` (and therefore the lint step of `make check`) scopes `golangci-lint` to the modules the change can actually affect: the modules owning files changed vs the `origin/main` merge-base — committed, uncommitted, and untracked — plus every module that depends on one of them, transitively (the dependency edges come from the workspace-local `replace` directives in each `go.mod`; a dependency change can break a dependent's typecheck and therefore its lint).
The gofmt check always covers every module, and a change that touches nothing with Go-lint effect (docs-only, charts-only) skips `golangci-lint` entirely — without queueing on the machine-wide heavy-build lock, which `scripts/go/go-lint.sh` now takes only when there is real lint work.

**Scoping covers every first-party module, workspace or not.** `scoped_module_dirs` unions the `go.work` members with `firstparty_nonworkspace_modules` (`devtools/`), and a scoped non-workspace module lints with `GOWORK=off` against its own `vendor/` tree.
Deriving the set from `go.work` alone is what disarmed the gate in Q670: a branch adding six files under `devtools/` owned no module the scoper could name, so it reported `no module changes`, skipped `golangci-lint` and exited 0 — a green that had linted nothing, while CI's full sweep found twelve issues.
Both halves of the union are discovered rather than enumerated, so a module added later is scoped without editing the gate.

Why this is sound: an unscoped module lints byte-identically to the merge-base commit — same sources, same `.golangci.yml`, same `tools/`-pinned linter — and that commit is on `main`, where CI's full sweep is a required check.
Anything that breaks that equivalence forces the full sweep instead: a change to `go.work`/`go.work.sum`, `.golangci.yml`, `vendor/`, `tools/`, or the scoping machinery itself (`scripts/go/go-lint.sh`, `scripts/lib/common.sh`, `scripts/agent/local-throttle.sh`), and likewise whenever no `origin/main` merge-base resolves.
CI always runs the full sweep (the `CI` env var forces it), so the PR gate is unaffected.
Force it locally with `LINT_ALL=1 make lint`.
The scoping decision itself is pure bash asserted by `scripts/go/go-lint-scope-test.sh` under `make scripts-test`, which asserts the real module graph alongside its fixtures — a scoper fed the wrong module set looks correct to every fixture.

The point is wall-clock under contention: most branches touch one or two modules, `make lint` is the target you re-run most in the [inner loop](#the-inner-loop-cheap-checks-while-iterating-make-check-once-pre-pr), and every full sweep holds the [machine-wide heavy-build lock](#resource-auto-throttle-on-gui-dev-machines) while sibling worktree sessions queue behind it.
Scoping shrinks both the run and the lock hold.

#### Build and lint caches across worktrees

Parallel sessions each run `make check` from their own `.claude/worktrees/*` clone, which raised the question of whether every fresh worktree pays a cold cache (Q343).
Measured (2026-07): **`GOCACHE` is machine-shared and path-independent at its default — do not repoint it.** The golangci-lint analysis cache was equally shared until its one failure mode bit (Q516, below); local runs now scope it per worktree, which the `GOCACHE` sharing makes nearly free.

- **Go build cache** (`GOCACHE`, default `~/Library/Caches/go-build` on macOS, `~/.cache/go-build` on Linux).
  Compile artifacts are content-keyed and hit across worktree paths: compiling the `broker` module against an empty cache took ~12 s in one worktree and ~0.6 s in a second worktree sharing that same cache.
- **golangci-lint analysis cache** (`GOLANGCI_LINT_CACHE`). **Per worktree on local runs** — [`scripts/go/go-lint.sh`](../../scripts/go/go-lint.sh) points it at the worktree's gitignored `tmp/golangci-lint` (an explicit `GOLANGCI_LINT_CACHE` still wins).
  The user-level default (`~/Library/Caches/golangci-lint` / `~/.cache/golangci-lint`) is shared and path-independent for the expensive analysis, but a cached entry keeps the absolute path of the worktree that produced it, and worktrees get deleted: the post-processors then cannot read the source they are reporting on and emit `failed to parse file … no such file or directory` warnings followed by *contradictory* findings — observed 2026-07 (Q516) as a simultaneous `G204: Subprocess launched with variable` **and** `directive //nolint:gosec … is unused for linter "gosec"` on the same two lines, in a file whose directive was present and correct.
  Scoping trades away little: `GOCACHE` (still shared) holds the load-bearing artifacts, and off a warm build cache the analysis re-runs in ~1 s per module (see the [throttle measurements](#resource-auto-throttle-on-gui-dev-machines)) — the shared-cache hit saved ~7 s per worktree per content change, not the ~2 min cold cost.
  CI keeps the default path: runners are fresh, so the failure mode cannot occur, and `unit-test.yml`'s `actions/cache` of `~/.cache/golangci-lint` stays keyed to it.
  A *direct* linter invocation outside `go-lint.sh` still uses the shared default — if it reports findings citing a `.claude/worktrees/*` path that no longer exists, do not chase the code: run `.build/golangci-lint cache clean` and re-lint.

The **test-result** cache is the third one, and it is path-keyed *at the default flags* — a result cached in one worktree does not hit from another even at an identical commit.
Q343 concluded from that there was "no supported knob to share test results across paths"; there is one, and the unit tier now passes it (2026-07):

- **`-trimpath` makes the result cache path-independent.** The flag removes the absolute worktree path from the test binary, so the cache key depends on content alone.
  Measured on `cmd/agc` with `-coverprofile`: **226 s cold in one worktree, 5 s on the first-ever run in a second worktree** (12 packages cached), emitting a byte-identical coverage profile — so the ratchet reads the same number either way. `broker` reproduces it: without the flag the sibling worktree re-runs, with it the sibling prints `(cached)` immediately.
  [`scripts/go/go-test.sh`](../../scripts/go/go-test.sh) and [`scripts/go/coverage.sh`](../../scripts/go/coverage.sh) pass it, so a fresh worktree at an already-tested commit re-runs only the packages whose content actually changed.
- **Do not promote it to a global `GOFLAGS`.** [`cmd/gmc/test/e2e`](../../cmd/gmc/test/e2e/e2e_suite_test.go) resolves the v2 CRD chart directory from `runtime.Caller(0)`, which a trimmed path breaks.
  The unit tier does no such thing, and the release images already build with `-trimpath`.
- **Two cosmetic consequences.** Adopting the flag changes the build-cache keys, so the first unit run after it lands recompiles from scratch once, machine-wide.
- **A test that reads a repo file outside its own module keeps its cached result when that file changes**, so the package prints `(cached)` and green while the thing it asserts on is broken.
  Measured 2026-08-11 on `devtools/agent/pipedgate`, whose `TestShippedRegistryCarriesRepoStateSettings` reconciles `.claude/piped-gate-guard.json` against the repo-root `.gitattributes`: appending a line to `.gitattributes` and re-running left it `(cached)`.
  Q799 shipped a `.gitattributes` entry without its registry half that way: `make check` green locally, `coverage` red in CI on a cold cache.
  This bites the `devtools/` gates hardest, because they are exactly the ones that read repo-root config as data.
  Run the package with `-count=1` (or `go clean -testcache` in that module) whenever the change under test is a file the test *reads* rather than one it compiles.
  And a failing test's stack frames read as module-relative paths (`github.com/actions-gateway/…/listener.go:42`) rather than absolute ones, so they are no longer click-to-open in some terminals.

What a fresh worktree still pays, and why it stays:

- **Tool builds.** Each worktree builds its own `.build/golangci-lint` (~16 s with a warm build cache).
  Sharing tool binaries across worktrees would need version-keyed storage to avoid silently running a stale binary after a `tools/` dependency bump — complexity that isn't worth ~16 s per worktree.

### The race-detector unit gate

The CI `unit-test` job runs the workspace unit tests under Go's race detector (`go test -race`), not plain `go test`.
The multiplexing core — agentpool, listener/mux, broker, token — is where data races hide, and plain `go test` never flags them; `-race` is pass/fail (a detected race fails the job).
This is the only `unit-test.yml` step `make check` does not mirror, because `-race` instruments every memory access and roughly doubles unit runtime.

Reproduce the CI race gate locally with:

```bash
make test-race        # one multi-module `go test -race` across the whole workspace
```

`make test-race` is the single source of truth for the race flags and timeout the CI job uses, and it carries the **same** throttle prefix and parallelism cap as `make test` (see [the auto-throttle above](#resource-auto-throttle-on-gui-dev-machines)).
That matters here more than anywhere: a `-race` build is a ~2–10× CPU/memory/I/O amplifier, so an *unthrottled* one on a GUI dev machine is the most likely single command to trip the macOS WindowServer watchdog.
Run it through `make test-race` (throttled) rather than a bare `go test -race`, or prefix a manual run with `$(scripts/agent/local-throttle.sh prefix)`.
On CI the throttle is a no-op, so the job runs at full speed.
The detector needs cgo, which is available on both the ubuntu CI image and a macOS dev box by default.

It is deliberately a separate target from `make test`/`make check` so the fast local loop stays fast and never silently becomes a `-race` run; treat it like `make vulncheck` — a heavier gate you run when a change warrants it (anything touching the concurrency core) or before a final pre-PR pass.

### Watching a unit run in progress

Measured on the CI `-race` gate: 200 s of wall clock at 8 % output density, 58 s between any two lines — and a *deadlocked* test prints nothing whatsoever until `-timeout` fires five minutes later, leaving you to infer which test hung from a goroutine dump. `make test` and `make test-race` therefore stream through [`devtools/gotest/progress`](../../devtools/gotest/progress/main.go), which prints one line per 30 s:

```
[unit t+2:14] 37/48 pkgs | 1204 ok, 0 failed, 3 skipped | running: broker.TestLeaseExpiry (58s), cmd/agc/controller.TestReconcile/case-3 (12s)
```

Knobs: `TEST_PROGRESS_INTERVAL` (seconds between lines; `0` runs plain `go test` with no renderer at all) and `V=1`, which streams `go test -v` live and bypasses the renderer — the two want opposite things.

**No test changed**, because `go test -json` already carries a run/pass/fail event per test.
That is the opposite of the e2e suite, whose 73 Ginkgo specs are one Go test and so needed [bespoke reporter instrumentation](#watching-an-e2e-run-in-progress).
Three measured properties of the `-json` stream are what the renderer stands on:

- **It streams; plain `go test` cannot.** Plain output buffers each package until that package ends *and* releases packages in command-line order, so one slow package holds back everything behind it.
  Under `-json` a second package's events arrive while the first is still running.
- **A build diagnostic carries an `ImportPath` (`pkg [pkg.test]`), not a `Package`**, and it never equals the package the failure is later reported against.
  Those lines print as they arrive; buffering them against a package would silently swallow compile errors.
- **A hung test never reaches a terminal event**, which is what keeps its output — including the eventual timeout goroutine dump — when its package is finally flushed.

Two deliberate differences from plain `go test` output:

- **Packages appear in completion order.** That is the same property that makes progress live.
- **A failing test keeps its `=== RUN` header, with its log lines in emission order**, where plain output re-groups them under `--- FAIL`.
  Output from every test that passed or skipped is dropped, so a green package is still one `ok` line.

**There is no test-level denominator**, and adding one is not worth its price. `go test -json` has no total, and recovering it needs a `go test -list '.*'` pre-pass — measured at 22 s wall / 131 s CPU with a warm build cache, because `-list` compiles every test binary *before* any test runs, which also serializes the compile that [`go-test.sh`](../../scripts/go/go-test.sh)'s single workspace-wide invocation exists to overlap with test execution.
The package denominator comes from `go list` instead: ~0.3 s, no compilation.
The test tally counts top-level tests only — subtests are created at run time, so counting them would make the number depend on which table cases happened to execute.

### Coverage measurement and the ratchet

The CI `unit-test.yml` workflow has a `coverage` job that measures per-module unit-test coverage and gates it with a **no-regression ratchet**, not an absolute percentage target.
[`scripts/go/coverage.sh`](../../scripts/go/coverage.sh) is the single source of truth; the Makefile exposes three targets, all of which report a floor per go.work module (a repo-root `go test ./...` does not work — see [go-workspaces.md](go-workspaces.md)):

```bash
make cover         # print the per-module coverage table (writes nothing)
make cover-check   # the CI gate: fail if a module dropped below its floor
make cover-update  # re-record the baseline floor in coverage-baseline.txt
```

**How it runs.** One workspace-wide `go test -coverprofile` over an explicit `./<module>/...` pattern per go.work module — the same shape [`scripts/go/go-test.sh`](../../scripts/go/go-test.sh) uses for `make test` — and the merged profile is then split back per module by import path.
The script used to loop the modules, one `go test` each; a single invocation lets Go schedule the whole workspace as one build graph, so the small modules overlap with the big `cmd/agc`/`cmd/gmc` dependency compiles instead of queueing behind them.
Measured on an 18-core M5 Max, two interleaved trials per regime: cold `GOCACHE` **92.5 s → 55.7 s (1.66×)**, warm build cache with `-count=1` **74.0 s → 42.5 s (1.74×)**.
Mean CPU rose 274 % → 439 % cold and 84 % → 129 % warm, so the serial loop's idle time was inter-module barriers, not throttle headroom.
Every module reported the same percentage as the per-module loop it replaced.
Method, full tables, and the handful of race-dependent blocks whose coverage tracks machine load rather than the shape: [local-gate-throughput.md](../plan/archive/local-gate-throughput.md).

**What is measured.** Per module, the aggregate statement coverage `go tool cover -func` reports over that module's slice of the profile, from which two kinds of non-production code are filtered out.
First, **mechanically-generated code** — `zz_generated*.go` (controller-gen DeepCopy) and `groupversion_info.go` (scheme boilerplate); filtering these keeps the floor reflecting hand-written logic, so adding a CRD field (which grows `zz_generated`) can't trip the gate without a real test change.
Second, **test-helper packages** — the `<pkg>test` external-helper convention (`broker/brokertest`), the `<pkg>stub` protocol-model convention those doubles are built on (`broker/brokerstub`, `scaleset/scalesetstub`), and anything under a `test/` helper tree (`gmc/test/utils`, the `test/fakegithub` module); these exist only to support other packages' tests, never ship in a production binary, and folding their partial self-coverage into a module's floor made the ratchet track helper code (broker measured ~48% blended while its production package was ~80% — Q110).
The `<pkg>stub` half was added with Q528: moving the scale-set protocol model into its own package changed nothing about how well it is tested — the AGC listener suite, `cmd/probe`, and the fakegithub tests all drive it — but those live in other modules, and per-package coverage credits none of them, so counting it sank the `scaleset` module from 84.6% to 59.4% on a refactor that *added* tests.
Excluding `brokerstub` in the same pass moved the `broker` floor 85.3 → 84.0: the helper had been scoring above its module's production average and inflating it.
We deliberately **do not** exclude `main.go`: in this repo several binaries (`cmd/worker`, `cmd/proxy`) keep real, unit-tested logic in their `package main`, so a blanket entrypoint exclusion would hide tested logic and leave those modules ungated.
The genuinely-thin entrypoints (`cmd/agc`, `cmd/gmc`) instead contribute a lower but still-defended floor — which costs the ratchet nothing, since a lower floor never causes a false failure.

**How it gates.** [`coverage-baseline.txt`](../../coverage-baseline.txt) records each module's floor. `make cover-check` fails only if a module drops **more than 0.5 percentage points** below its floor.
Coverage is deterministic (the gate runs without `-race`), so this small tolerance is not for flake — it absorbs benign denominator drift (adding a couple of uncovered boilerplate lines marginally dilutes the ratio) while still catching a real regression (deleting a tested function, gutting a test) on any module of meaningful size.
When coverage rises well above a floor, the gate prints a note suggesting `make cover-update`.

**Updating the floor.** When you intentionally add tests and coverage goes up, run `make cover-update` and commit the new `coverage-baseline.txt` — the ratchet then defends the higher number.
Lowering a floor is allowed but lands as an explicit, reviewable diff in that file rather than silently.
The current baseline (helper-package exclusion added in Q110):

| Module | Floor | Module | Floor |
|---|---|---|---|
| `broker` | 81.3% | `cmd/proxy` | 72.8% |
| `cmd/agc` | 78.1% | `cmd/worker` | 72.0% |
| `cmd/gmc` | 57.1% | `githubapp` | 82.6% |
| `cmd/probe` | 45.4% (compat suite) | `test/fakegithub` | n/a (helper-only module) |

Unlike `make test-race` and `make vulncheck`, `cover-check` **is** the unit-test step of `make check`: it supersets `make test` — the same unit-test packages, the same single workspace-wide invocation, streamed as the same `ok <pkg>` lines (honouring `V=1`), just run with `-cover` and then gated against the floors — so the local gate runs the suite a single time, not twice, and never lets a coverage regression slip past a green `make check`.
It carries the same [local throttle](#resource-auto-throttle-on-gui-dev-machines) and machine-wide serialize lock as `make test`, so a GUI run stays desktop-safe; on CI the prefix is a no-op. `make test` remains the no-coverage target for the fastest inner loop, and `make cover-check` runs standalone when you just want the ratchet.

### Gates scan present files, not just tracked ones

Six gates walk the tree ([shellcheck](#the-shellcheck-gate), [doc-links](#the-doc-link-gate), [conflict-markers](#the-conflict-marker-gate), `no-plan-refs-check`, `em-dash-check`, and `script-docs-check`), and all six draw their file list from one helper pair in [`scripts/lib/common.sh`](../../scripts/lib/common.sh):

- `git_candidates PATHSPEC…` — `git ls-files --cached --others --exclude-standard`, i.e. **tracked** files plus **untracked ones that are not gitignored**.
- `select_present_files` — drops what no reader can open: a **deleted-but-tracked** path (`--cached` still lists it) and merge-stage **duplicates** (`--cached` lists an unmerged path once per stage, which would multiply its findings).

**Listing `--cached` alone is a false green**, and it is the reason this is one shared helper rather than four pathspecs.
A brand-new file is untracked, so a tracked-only gate cannot see it: the gate passes, and the finding appears on the *next* run — after `make check` has already been reported green and the work called done.
Q432 hit this in the shellcheck gate (a new script, unlinted, cost a session on 2026-07-26); Q619 found the other three still selecting `--cached` only, so a brand-new doc's own links, a new file's conflict markers, and a new script's plan-doc citation all read green locally.

**Consequence for scratch files: gitignore them.** Leaving a file untracked no longer opts it out of anything — write it under the gitignored `tmp/` at the repo root, per the repo temp-file convention.
That is the documented opt-out and every one of them honours it.

**A gate that calls `git_candidates` twice has to filter both calls.** The pair is one selection in two steps, and the second step is the one that reads the disk, so an unfiltered second consumer silently answers "present" for a deleted-but-tracked path even when the gate's own scan list already dropped it.
The doc-link gate had exactly that shape: its scan list was filtered, its existence oracle was not, and a doc deleted from the worktree kept resolving every link pointing at it.
Q663 measured that red on `bash-style.md`, which six live links target.
Route every consumer through `select_present_files`, not just the one that opens the files.

The selection rules are asserted by `scripts/ci/shellcheck-scripts-test.sh` (the helpers directly, plus end-to-end against a throwaway repo covering every state: tracked, untracked, gitignored, deleted-but-tracked), and the untracked/gitignored behaviour is planted and measured per-gate in `scripts/ci/check-conflict-markers-test.sh`, `scripts/docs/check-no-plan-refs-in-code-test.sh`, `scripts/docs/check-em-dash-test.sh`, and `scripts/docs/check-script-docs-test.sh`.
All run under `make scripts-test`.

### The shellcheck gate

`make shellcheck` runs `shellcheck` over every shell script present under `scripts/` and is wired into `make check`, so the local pre-review gate matches CI.
The dedicated `shellcheck` job in `.github/workflows/unit-test.yml` runs the same `make shellcheck` target, gated on a `scripts` paths-filter (`scripts/**`, the `Makefile`, and the workflow itself) so a scripts-only change doesn't also trigger the full Go lint.

**The CI job pins shellcheck** (`SHELLCHECK_VERSION` in [`unit-test.yml`](../../.github/workflows/unit-test.yml) is the source of truth) rather than using `ubuntu-latest`'s preinstalled copy — that version drifts with the runner image, and shellcheck's heuristics (e.g. when SC2015 fires on `A && B || true`) differ between releases, so an unpinned gate gives a different verdict locally vs. CI.
Install the **same** version locally so `make shellcheck` matches the gate: see <https://github.com/koalaman/shellcheck#installing> (the target prints this hint if shellcheck is missing).
The pin is bumped automatically by updatecli (see [dependency-updates.md](dependency-updates.md)); when its PR lands, install the new version locally to match.

**What is covered:** the git pathspec `scripts/*.sh` resolved through [the shared present-file selection](#gates-scan-present-files-not-just-tracked-ones) — every `.sh` file present under `scripts/` that is either **tracked** or **untracked and not gitignored**, at any depth.
The pathspec is recursive because git's default `*` spans `/`, so every `scripts/<group>/*.sh` is covered without re-touching the gate.
Two paths are deliberately excluded: a **gitignored** one (that is how you opt a script out — see below), and a **deleted-but-tracked** one, which `--cached` still lists but shellcheck cannot read.
This complements [the actionlint gate](#the-actionlint-gate), which covers the inline `run:` blocks in workflows; before this gate the standalone helper scripts (`setup.sh`, `kind-with-registry.sh`, …) shipped unlinted.

Including untracked files is load-bearing (Q432): a tracked-only gate cannot see a brand-new script, so its first `make check` is a false green — same class as Q404 (the gate that compiled no build-tagged file), and it cost a real session on 2026-07-26.
The mechanism, the gitignore opt-out it implies, and the three other gates Q619 swept onto the same selection are covered in full above: [Gates scan present files, not just tracked ones](#gates-scan-present-files-not-just-tracked-ones).

Accepted findings carry a targeted `# shellcheck disable=SCxxxx` directive with a justifying comment immediately above the line (see the dynamic-name `read`/`export` in `scripts/dev/probe-investigations-cd.sh`); everything else is fixed to match the repo bash conventions listed in [`scripts/README.md`](../../scripts/README.md).

### The actionlint gate

`make actionlint` ([`scripts/ci/actionlint-workflows.sh`](../../scripts/ci/actionlint-workflows.sh)) lints every workflow under `.github/workflows/` and is wired into `make check`.
CI runs the same target from the `actionlint` job in [`unit-test.yml`](../../.github/workflows/unit-test.yml), gated on the `workflows` paths-filter it shares with the [path-filter gate](#the-path-filter-gate) — both read the workflows, so both want the same trigger, and one filter is one list to keep current instead of two.

It exists because three docs credited actionlint with exactly this while nothing ran it (Q579).
The `lint` job is golangci-lint; a workflow-only PR shipped unlinted, and the claim was load-bearing enough that the shellcheck gate above was scoped around it.

**What is covered**, measured by planting each defect and requiring red:

| Defect | Verdict |
|---|---|
| `uses: actions/checkout` — no `@ref` at all | `ref is missing` \[action] |
| `uses: actions/checkout@` — empty ref | `owner and repo and ref should not be empty` \[action] |
| Unquoted expansion in an inline `run:` block | `SC2086 … Double quote to prevent globbing` \[shellcheck] |

Plus the workflow schema, expression syntax and context typing, and `runs-on:` labels. **It does not verify that a ref is a SHA rather than a tag** — it enforces that a ref is present and well-formed, which is the precondition for pinning, not the pin itself.
Dependabot's `github-actions` ecosystem is what keeps the SHAs themselves current (see [dependency-updates.md](dependency-updates.md)).

**Self-hosted `runs-on:` labels are declared** in [`.github/actionlint.yaml`](../../.github/actionlint.yaml), because actionlint rejects any label outside GitHub's hosted set.
Declaring each one keeps the check live — a typo'd label still fails — where switching the rule off would not.
The jobs that reach their runner through `fromJSON(vars.GAG_RUNNER)` need no entry: that is an expression actionlint does not resolve.

**actionlint builds from the vendored `tools/` module**, like golangci-lint and controller-gen, so it is not a host dependency and `make actionlint` gives the identical verdict locally and on CI.
There is no version env var to bump; it rides the `tools/` Dependabot channel with the other Go tooling.

**The shellcheck dependency is a false-green hazard, so the gate asserts it.** actionlint delegates inline `run:` blocks to shellcheck and, when shellcheck is absent from `PATH`, silently disables that integration and still exits 0 — half the gate reporting green having linted nothing, the same class as Q404 and Q432.
The script therefore fails outright on a missing shellcheck rather than running degraded. shellcheck is already a `required`-tier tool in [`check-tools.sh`](../../scripts/ci/check-tools.sh), so `make doctor` covers it; the CI job installs the same pinned version the shellcheck job does, off a workflow-level `SHELLCHECK_VERSION`/`SHELLCHECK_SHA256` pair so the two cannot drift apart.

Accepted findings inside a `run:` block carry the same targeted `# shellcheck disable=SCxxxx` directive as a standalone script — placed above a `{ … }` group it covers the whole group, which is how `publish.yml`'s single-quoted `printf` format strings (a genuine SC2016 false positive) are justified once rather than per line.

### The doc-link gate

`make doc-links` runs `scripts/docs/check-doc-links.sh` over every present, non-vendored Markdown file — tracked, or untracked and not gitignored ([the shared file selection](#gates-scan-present-files-not-just-tracked-ones)) — and is wired into `make check`, so the local pre-review gate matches CI.
CI runs the same `make doc-links` target from its **own** workflow, [`.github/workflows/doc-links.yml`](../../.github/workflows/doc-links.yml), scoped (via `on.paths`) to `**.md`, the checker, and the workflow itself.
It is deliberately separate from `unit-test.yml` — that workflow path-ignores docs, so a docs-only change triggers only this lightweight check and never the Go suite (mirroring how `e2e-test.yml` is its own workflow).

It fails on two classes of breakage: **dead relative file links** (a `[text](path)` whose resolved target is neither a present file nor directory — a trailing `:NN` line reference is tolerated and only the file part is resolved) and **dead anchors** (a `#fragment` that matches no heading slug or explicit `<a id>`/`<a name>` in the target Markdown file).
Anchors are resolved with GitHub's heading-slug algorithm (strip inline markdown — respecting code spans — lowercase, drop everything outside `[a-z0-9 _-]`, spaces to hyphens, de-dupe repeats with `-1`/`-2`), so the verdict matches what GitHub renders.
External URLs (http/https/mailto/tel), links inside fenced or inline code, and anchors into non-Markdown or vendored targets are out of scope.

**The script selects the files *and* the existence oracle; [`devtools/docs/doclinks`](../../devtools/docs/doclinks/) does the checking**, over the shared goldmark parse layer in [`devtools/docs/markdown`](../../devtools/docs/markdown/) (Q612).
The `awk` it replaces collected links with a regular expression, which cannot count brackets: `[![badge](img)](target)` matched the inner image, so the outer target went unchecked — three of those are live in `README.md` — and a link whose text wrapped across a line break was collected by neither half (25 of those).
The parse layer also carries the MkDocs dialect the site renders (`!!!` admonitions, `markdown="1"` HTML), because a stock parser reads an admonition body as an indented code block and every link in it disappears.
The checker resolves a relative link against a path list the script hands it, not against the disk, so **what counts as existing is exactly the gate's own file selection**: present on disk *and* git-known, with ancestor directories derived from it.
That keeps a link to a brand-new untracked file green (Q619) and a link to a file deleted from the worktree red (Q663).
Behaviour is asserted beside the packages in Go; the file-selection half is `scripts/docs/check-doc-links-test.sh` under `make scripts-test`.

**This gate speaks for github.com only.** MkDocs resolves slugs and paths differently, so a link can pass here and 404 on the published site — 11 did (Q560). `make docs-build` is the site-side half, running `mkdocs build --strict` over both publication scopes; CI runs it in `pages.yml`'s PR `build` job.
It is not in `make check` (it needs the pinned Python venv, and `pages.yml` is already path-gated on the docs sources), so **run it yourself when a change adds a heading or a relative link under `docs/`**.
What diverges, and how to write links that survive both: [website.md § The two link gates](website.md#the-two-link-gates).

### The release-pin gate

`make release-pins-check` (`scripts/docs/check-release-pins.sh`) fails when an install/upgrade page still pins a release older than the newest stable `vX.Y.Z` tag.
It exists because nothing in `publish.yml` rewrites the docs: the chart version, the image tag, and the release-notes URL are transcribed by hand, and `v1.3.0` shipped with `README.md`, `docs/index.md`, and `install.md` fixed by hand while `upgrade.md` and `gitops.md` still told operators to install `1.2.0` — plus a patch-line hint in `install.md` reading `1.2.z` that no grep for `1.2.0` would have found (Q638).
The runbook step said `X.Y.Z` and named no files, so the bump was remembered rather than run.
It is wired into `make check` and into a second job in the same [`doc-links.yml`](../../.github/workflows/doc-links.yml) workflow, whose checkout uses `fetch-depth: 0` because the gate derives the current release from the tags.

The scan asserts that **every** release-version literal in the five pin-bearing pages names the current release — not a list of known pin sites, so a pin added to one of those pages is covered the day it lands.
That is only tractable because the noise floor there was measured first: two literals, both exempted in the script header (a line beginning `Measured on kind`, whose version records what was actually installed for a measurement, and `v2.0.0`, the announced `v1alpha1`/`v2alpha1` removal release — v-prefixed only, since a chart pin never carries the `v`). **Do not widen the file set to `docs/` at large**: `troubleshooting.md` and `release.md` are full of legitimate "before `v1.3.0`" history, and an exemption list that size would hide the drift the gate exists to catch.

A page that yields **no** pin at all fails rather than passes — an empty result cannot distinguish "this page has no stale pin" from "the pin moved and the scan no longer sees it".
Behaviour is asserted by `scripts/docs/check-release-pins-test.sh` under `make scripts-test`, mostly as planted failures: a checker that silently matches nothing passes a stale tree exactly like a clean one.

### The release-link gate

`make release-links-check` (`scripts/docs/check-release-links.sh`) resolves the release notes' absolute links into the versioned site. `docs/releases/` is the one doc whose links are *all* absolute — those files are excluded from every site version, so a relative link would fail `mkdocs build --strict` — and the doc-link gate above skips external URLs by design, so nothing resolved them (Q636).

The oracle is a local `mkdocs build`, never the network: a gate that fetches URLs fails when a third party sneezes. `mkdocs build` lays the release publication scope out exactly as the site serves it, so `https://actions-gateway.com/1.3.0/operations/upgrade/#gmc-rollback` resolves to `site/operations/upgrade/index.html` carrying `id="gmc-rollback"`.
That also fixes the scope: **the exclusion in `check-doc-links.sh` stays**.
"External" there means "unresolvable", and these are resolvable only because the site is built from this same tree — widening that gate globally would mean fetching URLs.

Three deliberate limits, all of them printed rather than assumed:

- **Only the site host is resolved.** A github.com or third-party link is counted and reported, never failed.
- **Only one version is resolvable** — the newest note in `docs/releases/`.
  The build comes from the working tree, the tip of development after that tag: a faithful oracle for the notes being authored or amended, a wrong one for a frozen older release whose pages have since moved.
  Links naming another version are skipped, with the count and the reason.
- **A missing `site/` is built, not skipped past.** The build *is* the gate, so no-opping without it would make a green verdict meaningless.
  An explicit `GAG_SITE_DIR` that does not exist is an error instead — the caller named a tree, so building a different one would be a lie about what was checked.
  Finding zero site links across every note is also a failure: these notes link the versioned site by convention, so none at all means the extractor stopped matching.

It is the one docs-content gate **outside `make check`** — the fast local gate has no business provisioning a Python venv — and runs as a third job in [`doc-links.yml`](../../.github/workflows/doc-links.yml), whose checkout takes `fetch-depth: 0` for the announce bar's tag-derived version.
Behaviour is asserted by `scripts/docs/check-release-links-test.sh` under `make scripts-test`, against a hand-built site tree that needs no mkdocs: planted dead pages and anchors, plus the controls that must stay green — a third-party URL carrying a bogus anchor, a code-fenced URL, and a link to a version this tree cannot stand in for.

### The roadmap coherence gate

`make roadmap-check` (`scripts/docs/check-roadmap.sh`) fails when the public [roadmap](../roadmap.md) disagrees with the backlog in [`docs/STATUS.md`](../STATUS.md).
It exists because the two drift silently and expensively: a 2026-07-25 audit found **six of seven** "In progress / near-term" roadmap items had already shipped, and the v2 API was still badged `alpha` more than two weeks after `v2beta1` graduated — released that way, because a stable tag deploys that tag's docs permanently.
The `docs/development/doc-update-matrix.md` rule requiring the update already existed; a human-followed convention was not enough.

What makes it mechanical is a property of this repo's backlog: **done Queue rows are deleted** (git is the archive).
So a roadmap bullet naming a Q-ID that `STATUS.md` no longer has is an exact, zero-false-negative signal that the work shipped.
Each bullet under "In progress / near-term" and "Exploring / longer-term" therefore carries an invisible annotation naming its backing rows:

```markdown
- **Capacity-aware job intake.** <!-- q:Q405,Q406 --> Additional opt-in rungs on …
```

HTML comments render nowhere, on github.com or the MkDocs site.
The gate fails on: a bullet with no annotation; an ID that resolves to no row (it shipped — move the bullet to "Available now", or drop just that ID when only part of a multi-item bullet shipped); a near-term bullet whose rows are all in **Deferred** (it was parked); and an exploring bullet whose rows are all in the **Queue** (it is active work).
"Available now" is deliberately ungated — it describes shipped capability, with no row left to point at.
A format change on either side exits 2 rather than passing silently.
Rules are asserted by `scripts/docs/check-roadmap-test.sh` under `make scripts-test`.

**The script selects the files; [`devtools/docs/roadmapcheck`](../../devtools/docs/roadmapcheck/) does the checking**, over the same parse layer as the link gate (Q614).
Two things move with that: a `<!-- q:QN -->` written inside a code fence is an example of the format rather than an annotation, and a bullet's word count now stops where the bullet does.
The line-matching `awk` ran a bullet's span to the next bullet or heading, so 19 words of the paragraph between two roadmap sections were charged to the bullet above them.
Every count fell by 5 to 24 words on the roadmap and by exactly 1 on `features.md`, which is the `- ` marker the old counter spent; nothing crossed either cap under either counter, so the port changed no verdict.

**A word cap measures a list item, which swallows more than a bullet looks like.** Deleting a bullet and leaving its indented continuation lines behind attaches them to the bullet above, so the cap breaks on words that are not that bullet's own: an accurate finding against an innocent bullet, which reads as pre-existing debt rather than as the deletion that caused it.
Rule 12 reports the stray paragraph at its own line and names the bullet it landed on, and both caps state the line span they counted, so a count that disagrees with the bullet on screen can be reconciled without re-parsing the page.
The two capped pages use no multi-paragraph bullets; the grid cards on `index.md` and `why-gag.md` do, and are checked for badges alone.

CI runs the same script from [`status-lint.yml`](../../.github/workflows/status-lint.yml) — alongside the `STATUS.md` format lint and under the same `status-lint-gate` required check — rather than from `unit-test.yml`, which path-ignores docs.
That placement is load-bearing: the drift arrives on docs-only PRs, which never trigger the Go suite.

The one gap it cannot close: deleting the Queue row *and* the annotation together, without moving the bullet.

### The conflict-marker gate

`make conflict-markers-check` (`scripts/ci/check-conflict-markers.sh`) fails when any present, non-vendored file — tracked, or untracked and not gitignored ([the shared file selection](#gates-scan-present-files-not-just-tracked-ones)) — contains a leftover merge-conflict marker line — the seven-character `<<<<<<<` / `=======` / `>>>>>>>` forms or diff3's `|||||||`.
It exists because an edit-based conflict resolution can miss a marker sitting just outside the text it replaced, and format-aware linters skip lines they don't parse; exactly that combination let a stray marker merge to `main` via PR #724 (Q379; fixed same day in PR #730).
Wired into `make check`; CI runs the same target from its own lightweight workflow, [`.github/workflows/conflict-markers.yml`](../../.github/workflows/conflict-markers.yml), on **every** PR with no path filter — a marker can be left in any file type.
Only exact seven-character marker lines are flagged, so Markdown setext underlines of any other length stay legal, as do mid-line mentions like the backticked examples in this paragraph; the vendored trees are excluded.
The pattern logic and the file selection are asserted by `scripts/ci/check-conflict-markers-test.sh` under `make scripts-test`.
When resolving conflicts by hand, `git diff --check` gives the same signal per-file before you stage.

### The v2 API sync gate

`make v2-api-sync-check` ([`scripts/go/check-v2-api-sync.sh`](../../scripts/go/check-v2-api-sync.sh)) holds `api/v2alpha1` and `api/v2beta1` identical wherever they share a file.
Two served versions of one API must duplicate their *versioned types* — Kubernetes requires it — but the shared spec fragments beside them are identical by contract, and a one-sided edit breaks the storage/hub conversion with nothing to catch it.

The gate's default is inverted from the Q345 original, which named two paths (`conditions.go`) and so covered 332 of ~2,550 identical lines.
Now **every** `.go` file present in both packages must match, and a file added to both is covered the day it lands with no edit to the script.
Two differences are normalised away before the diff — the `package` clause and a `+kubebuilder:storageversion` marker (only one version ever carries it).
Files that genuinely differ per version are named in the script's `EXEMPT` list with the reason; a stale entry (naming a file no longer paired) fails, so the list cannot rot into a silent hole.
A file present in one version only is reported but never fails — adding a test to one package is normal.

A file the gate cannot **read** is reported as trouble, not drift: `could not read FILE in both versions`, alongside the reader's own error.
The two are worth distinguishing because they look identical otherwise — the normaliser used to run inside a process substitution, where its exit status is unobservable, so a failed read left that side empty and the diff blamed every line on an edit nobody made (Q596).
A "divergence" you cannot find in `git diff` is this, not drift.

Wired into `make check` and CI's `lint` job.
Its behaviour — including that a body divergence in a previously-unguarded file actually fails, and that an unreadable file reads as trouble — is asserted by `scripts/go/check-v2-api-sync-test.sh` under `make scripts-test`.
That suite's `tree-in-sync` assertion is the one that runs the gate against the live tree; it prints the gate's exit code and full output on failure, so an occurrence is diagnosable from the CI log alone.

### The build-tag gate

`make build-tags-check` ([`scripts/go/go-vet-tags.sh`](../../scripts/go/go-vet-tags.sh)) compiles and vets the Go files that no other fast gate builds: everything behind `//go:build integration`, `e2e`, or `load`. `make lint` and `make test` (and so `make check` and CI's `unit-test` job) build the workspace with the **default** tag set, which excludes those files entirely.
So a refactor could leave an unused import or a stale call signature in an envtest suite, `make check` would pass green, and the break would surface only on CI's path-gated integration/e2e leg, which may not even run on the PR that introduced it (Q404).

It runs one workspace-wide `go vet -tags integration,e2e,load` over every `go.work` module. `go vet` typechecks what it analyses, so a compile break fails the gate, and it needs no envtest assets, no cluster, and runs no tests.
Actually *running* the tagged suites remains the job of [`make test-integration`](#integration-tests), the [e2e tiers](#end-to-end-tests), and [`make load-test-quick`](#load-tests).

Enabling all three tags at once is sound because they select disjoint package trees rather than alternative implementations of one package: no first-party file is constrained on the negation of another's tag.
If that ever changes, say a tag gaining a `!tag` counterpart file, the one-shot invocation stops working and the gate has to vet each tag separately.

**Adding a build tag means editing this gate.** `BUILD_TAGS` in the script is the list, and a coverage assertion keeps it honest: before vetting, the gate asserts (from `go list`'s `IgnoredGoFiles`) that the tag set leaves **no** first-party `.go` file uncompiled.
Introduce a tag it does not list and the gate fails with instructions, rather than silently carving another hole in the same shape as Q404.
Both properties, the coverage guard failing on an unlisted tag and a tagged-file break that an untagged vet reports clean, are asserted by `scripts/go/go-vet-tags-test.sh` under `make scripts-test`.

### The path-filter gate

`make path-filters-check` ([`scripts/ci/check-path-filters.sh`](../../scripts/ci/check-path-filters.sh)) reconciles the hand-maintained `dorny/paths-filter` lists in `.github/workflows/` with what the repo actually contains.
It exists because a filter that omits a directory makes its gate report green **by skipping** — the worst kind of false negative, since nothing is red and `main` ends up green on evidence it never gathered.
That is not hypothetical: `api/` and `scaleset/` were absent from the integration, e2e, and security filters, so changes confined to either module merged without ever meeting envtest, e2e, `govulncheck`, or trivy (Q400, fixed by hand; this gate is the recurrence guard — Q429).

Six assertions, cheapest first:

1. **Registry completeness.** Every filter in every `filters:` block is listed in the script as either `WORKSPACE_FILTERS` (must cover the whole workspace) or `NARROW_FILTERS` (scoped to one gate's inputs, with the reason inline).
   A new workflow, or a new filter in an existing one, fails until someone classifies it — so the hole cannot reopen in a new shape.
   A stale entry naming a filter that no longer exists fails too.
2. **Module coverage.** Every `WORKSPACE_FILTERS` entry matches every `go.work` module.
   Only a recursive glob rooted at the module or an ancestor counts: a bare `api` matches the literal path and nothing beneath it, and `api/config/**` leaves the rest of the module ungated.
   Failures name the module, the workflow, and the exact pattern to add, one per gap.
3. **Live paths.** Every pattern's literal prefix still exists on disk.
   A pattern left behind by a rename matches nothing, which narrows its gate as silently as a missing module does.
4. **Shared-lane agreement.** Two filters gating the same reusable workflow list the same `scripts/` patterns. `SHARED_LANE_FILTERS` pairs them; the failure prints a diff of the two sets. `e2e-test.yml` and `e2e-calico.yml` both call `e2e-reusable.yml` yet disagreed by roughly 60× about which scripts it runs — the Calico lane named two of the six the reusable workflow invokes directly, so a `free-runner-disk.sh` change skipped the lane that exercises it (Q571).
5. **Push-trigger agreement.** A workflow that scopes its post-merge leg with `on.push.paths` lists the same paths as its `changes` filter. `PUSH_TRIGGER_FILTERS` registers the pairs.
   See below for why this one is easy to miss.
6. **Globstar placement.** Every `filters:` pattern spells `**` where picomatch still expands it. `cmd/**.go` reads as every Go file under `cmd/` and matches nothing, and assertion 3 passes it because the literal prefix `cmd` exists.
   Scoped to `filters:` blocks only; see below.

**So adding a workspace module now fails the gate instead of slipping through** — but the gate only knows about *whole-workspace* coverage.
Judgement is still yours for the narrow filters: when you add a module, ask what each gate actually compiles, scans, or bakes, and remember the same applies to a gate that names files individually (`manifest-validate.sh`'s `standalone_manifests` — adding a path there means adding its directory to the filter).
Wired into `make check` and CI's `path-filters` job in `unit-test.yml`, which is gated on the `workflows` filter — the one filter watching all of `.github/workflows/`, so editing any `filters:` block re-runs the gate that lints it.
Behaviour, including that each assertion fails when it should, is asserted by `scripts/ci/check-path-filters-test.sh` under `make scripts-test`.

#### Where a globstar works in a filter glob

`dorny/paths-filter` matches with [picomatch](https://github.com/micromatch/picomatch); `on.push.paths` is matched by GitHub's own trigger matcher.
The two do not agree, so a pattern copied from one list into the other can silently stop matching.
Measured against the pinned `dorny/paths-filter@7b450ff` (v4.0.2) on a branch whose diff was two nested Go files (`cmd/agc/buildinfo.go`, `cmd/agc/config.go`):

| Filter pattern | Matched | Why |
|---|---|---|
| `'**.go'` | both files | a **leading** `**` globstars normally |
| `'**/*.go'` | both files | equivalent to the above |
| `'*.go'` | nothing | a lone `*` never crosses `/`, and matching is on the full path, not the basename |
| `'cmd/**.go'` | nothing | a **mid-pattern** `**` beside non-`/` characters degrades to `*`, which then cannot cross the `/` in `agc/` |

The hazard is narrow but silent: `cmd/**.go` reads as "every Go file under `cmd/`" and gates on nothing.
Write `cmd/**/*.go`.
Assertion 3 above does not catch it — the pattern's literal prefix (`cmd`) exists on disk.

Q594 filed `plan-hygiene.yml`'s `'**.go'` as an instance of this.
It is not: a leading `**` is fine, and that filter matches every Go file in the repo.
The tree carries no instance of the broken shape today; **assertion 6 is the recurrence guard** (Q659).

The rule it enforces, and the boundary that makes it usable: a `**` expands only as a **whole path segment**, or at the **very start** of a pattern.
So `cmd/**/*.go`, `**/*.go` and `**.go` all pass, and `cmd/**.go` fails with the rewrite named.
That leading exception is load-bearing rather than pedantic: `plan-hygiene.yml`'s `plan` filter is `'**.go'` today, so a rule phrased as "`**` must always be its own segment" would fail the tracked tree on arrival.

**It scans `filters:` blocks only.** Those are what `dorny/paths-filter` matches with picomatch; `on.push.paths` and `pull_request.paths` are matched by GitHub's own trigger matcher, which reads the same pattern differently, so applying this rule there could reject a pattern that works.
The two matchers are the distinction the table above measures, and assertion 5 already holds the three duplicated lists in step.

Its own tests pin the boundary from both sides, since a false positive fails the tracked tree: the sound shapes must **not** be flagged, and the degraded one must be.
The assertion was verified by injecting `'cmd/**.go'` into a real filter and requiring red.

#### A path list written twice: the trigger and the filter

Three workflows — `e2e-calico.yml`, `plan-hygiene.yml`, `status-lint.yml` — express the same scoping decision **twice**, because the two legs are gated by different mechanisms:

- **The PR leg** triggers on every `pull_request` with no path filter (so its `gate` job always reports its required check) and is scoped by the internal `changes` filter.
- **The post-merge leg** triggers on `push` to `main` and is scoped by `on.push.paths` — a plain GitHub Actions trigger filter, not a `dorny/paths-filter` block.

GitHub Actions does not reliably resolve YAML anchors, so the list is duplicated rather than shared. **Drift between the two is invisible on a PR.** Every PR classifies correctly off the filter; only the post-merge leg silently stops running, and a leg that does not run leaves nothing red to notice.

Q571 shipped exactly that regression and merged green: it rewrote `e2e-calico.yml`'s filter to `scripts/{e2e,fetch,lib}/**` and left the push list naming the two now-moved files it had always enumerated.
Assertion 5 is the recurrence guard — it compares the two as sorted sets and prints a diff of the difference.

Two more workflows (`doc-links.yml`, `dockerfile-lint.yml`) duplicate a list across `pull_request.paths` and `push.paths` instead.
That is the same hazard in a different shape and is **not** yet gated; both lists agree today (Q572).

#### `scripts/` is grouped by blast radius

The other half of keeping these filters honest is where a script lives. `scripts/` has no top-level files: every script sits in a subdirectory named for the gate that consumes it — `e2e/`, `go/`, `docs/`, `security/`, `manifest/`, `release/`, `ci/`, `fetch/`, `agent/`, `dev/`, plus the pre-existing `dogfood/`, `lib/` and `updatecli/`.
[`scripts/README.md`](../../scripts/README.md) is the map.

That makes every filter a prefix glob (`scripts/e2e/**`) instead of an enumeration that drifts or a catch-all that over-triggers.
Before Q571 the e2e filter took `scripts/*` plus a `scripts/!(dogfood)/**` extglob, so **any** top-level script pulled in a ~13 min e2e run: two docs-only PRs paid for it during Q561 because they touched a MkDocs hook's unit test, and one of them was then blocked by an unrelated migration flake it had no business meeting.

**Adding a script means choosing its group**, and the choice is "which gate runs this?", not "what is it about".
Two consequences worth knowing:

- A `*-test.sh` goes beside its subject, so it inherits its subject's gate for free.
- When a script is shared across gates, it belongs in `fetch/` (the download/retry/pre-pull family) or `lib/`, and **every** consuming filter lists that group.
  Putting a shared helper under one gate's group is the Q400 narrowing hazard in a new shape.

**The map is gated, not a convention.** `make script-docs-check` ([`scripts/docs/check-script-docs.sh`](../../scripts/docs/check-script-docs.sh)) fails when a script under `scripts/` appears nowhere in `scripts/README.md`, either in its own row or named in the row of the script it belongs to, which is how a `*-test.sh` is usually documented.
It was a convention until Q688 measured the drift: sixteen `*-test.sh` files and `check-page-density.sh` had accumulated with no mention, and nothing would have reported the next one.
Mentions are read off the parsed Markdown by [`devtools/docs/scriptdocs`](../../devtools/docs/scriptdocs/), so a filename inside a fenced example counts as an illustration rather than an entry, and `start.sh` is not found inside `e2e-start.sh`.
It runs in `make check` and in the CI `doc-links` workflow, whose triggers are the only ones covering both of its inputs: the `scripts/` tree and a Markdown page.

Wired into `make check` and CI's `lint` job.

**The rest of the linters see the tagged trees too.** `.golangci.yml` sets `run.build-tags: [integration, e2e, load, autoscaler, karpenter]`, so `gosec`, `errcheck`, `staticcheck`, `unused`, `dupl`, and `funlen` cover the envtest suites, the e2e harness, the load harness, and the live drift tests — the same files this gate compiles — rather than skipping them the way `go vet` did before Q404 (Q430).
That list must stay in step with `BUILD_TAGS` in `scripts/go/go-vet-tags.sh`; the coverage assertion above is what catches a new tag, since it fails before either gate can silently skip a file.

Two things to know when a finding lands in a tagged package:

- **`gosec` `G204` is narrowed by *source*, not by path.** A repo-wide exclusion rule drops `G204` only where the launched binary is a string literal (`exec.Command("kubectl"|"gh"|"make", …)`). `os/exec` does not go through a shell, so a variable *argument* to a fixed binary cannot inject a command, and the e2e harness does exactly that ~60 times with generated namespace, pod, and selector names.
  The two forms that can actually go wrong — a shell (`exec.Command("bash", "-c", script)`) and a variable binary name (`exec.Command(somePath(), …)`) — still fail the gate everywhere, including in production code, and the two that exist today carry audited inline accepts.
  Adding a binary to that list means making the same argument for it.
- **Everything else is per-occurrence.** Test scaffolding gets no blanket pass: the `_test.go` exclusions are limited to `dupl`, `funlen`, and `forbidigo` (see the rules in `.golangci.yml`), so a `gosec`, `errcheck`, `staticcheck`, or `unused` finding in a tagged file is fixed or annotated with a justified `//nolint:<linter> // <rule>: <reason>`. `nolintlint` (`allow-unused: false`) then fails the build if that annotation ever stops suppressing anything.

Widening the tag set costs about 4% of lint wall-clock — measured over the full per-module sweep, 3.00s → 3.13s warm and 34.9s → 36.6s cold — so it does not move [the inner loop](#the-inner-loop-cheap-checks-while-iterating-make-check-once-pre-pr) or the CI critical path.

### The gate-list gate

`make gate-lists-check` ([`scripts/ci/gate-list.sh`](../../scripts/ci/gate-list.sh)) keeps two lists from acquiring a second copy: the gates `make check` runs, and the `scripts/` suites its `scripts-test` gate fans out over. `CHECK_FAST_GATES`, `CHECK_HEAVY_GATES` and `SCRIPTS_TESTS` in the root [`Makefile`](../../Makefile) are the source of truth; the `check` recipe, `make list-gates`, `make list-script-tests`, and this page all derive from them.
Adding a gate is one edit: append the target name to `CHECK_FAST_GATES` and give the target its own `.PHONY` line and a `##` help line beside its rule.
Adding a `scripts/` suite is one edit too: append its `group/name-test` path to `SCRIPTS_TESTS`.

The gate exists because a derived list that can go stale quietly is worse than honest duplication.
It fails when:

- a gate in either variable has no `.PHONY` declaration or no `##` help line — `make list-gates` would print it blank, and make would treat the name as a file target;
- `check:`'s sequential phases stop matching `CHECK_HEAVY_GATES`, or its fan-out line runs anything beyond the `CHECK_FAST_GATES` expansion.
  This is what keeps `make list-gates` complete: a gate wired straight into the recipe would run on every `make check` without ever being listed;
- a target is declared `.PHONY` twice — the bulk block that used to restate 53 target names at the top of the `Makefile`, and that every gate-adding branch conflicted on, cannot come back;
- `STATUS_GATES` stops being a subset of `CHECK_FAST_GATES`, so `make status-gates` — the seconds-long verify for a `docs/STATUS.md`-only edit — stays a strict subset of the full gate rather than a second opinion;
- a fast gate *outside* `STATUS_GATES` selects `docs/STATUS.md`, which is the direction that had no enforcement: `em-dash-check` and `page-density-check` both scanned the file for as long as they had existed, while the comment above the variable called the list complete (Q749).
  Membership is derived from the pathspec each gate's script hands git, the same question the gate itself asks.
  A gate whose recipe runs no `scripts/` file has no derivable file set and declares instead, with a `# status-scope: none` comment and its reason directly above its `.PHONY`, as `md-reflow-check` does;
- `SCRIPTS_TESTS` and the `scripts/**/*-test.sh` files on disk name different sets.
  A suite written but never listed is the failure worth catching: `make scripts-test` reports green having never run it, so the assertions it carries are disarmed while looking armed.
  The reverse, a listed suite with no file, fails the fan-out on a missing path.
  The two `*-test.sh` scripts that are runners rather than suites are declared in the checker's `NON_SUITE_TESTS`;
- this page stops citing `make list-gates` or `make list-script-tests`.
  That last rule keeps the pointers alive; it cannot detect a transcribed list added *beside* a pointer, which stays a review concern.

It runs in `make check` and in the CI [`doc-links.yml`](../../.github/workflows/doc-links.yml) `gate-lists` job — that workflow rather than `unit-test.yml` because its triggers are the only ones covering both halves of what the gate reads, the `Makefile` and this Markdown file.
Its own assertions are in `gate-list-test.sh` under `make scripts-test`, each one injecting a single defect into a healthy fixture and requiring a red.

### The codegen drift gate

`make codegen-check` ([`scripts/go/check-codegen-drift.sh`](../../scripts/go/check-codegen-drift.sh)) fails when committed controller-gen output no longer matches what controller-gen generates from today's Go types — either half: any CRD, RBAC role, or webhook YAML under `api/config/`, `cmd/agc/config/`, or `cmd/gmc/config/`, and any `zz_generated.deepcopy.go` in `api/`, `cmd/agc/`, or `cmd/gmc/`.

Nothing ran `make manifests` on a contributor's behalf, so the committed YAML was only ever as fresh as the last person who remembered, and the gap is worst **across modules**: `cmd/gmc`'s `ActionsGateway` CRD embeds AGC types (`RunnerGroupSpec`), so a doc comment edited in `cmd/agc/api` changes the GMC manifest and only `make -C cmd/gmc manifests` propagates it.
PR #793 edited a `quotaRetryDelay` doc comment in the AGC type and the GMC CRD never caught up, so every later GMC contributor got that hunk as unrelated diff noise the moment they regenerated (Q440).
The root `make generate` / `make manifests` now cover all three modules, so a contributor who runs the obvious root target no longer has to know which module embeds which (Q458) — this gate is what holds that property.

Three assertions, cheapest first, each over both halves:

1. **Registry completeness.** Every first-party module whose `Makefile` defines a `manifests:` target is registered in the script's `MODULES` table, and every one defining a `deepcopy:` target in its `DEEPCOPY_MODULES` list.
   A new module generating either fails until someone registers it, so the hole cannot reopen.
2. **Registry fidelity.** Each registration's generator list and explicit `output:` rules match that module's own recipe, so the gate regenerates exactly what `make manifests` / `make deepcopy` would rather than a stale approximation.
3. **Drift.** Every regenerated file matches its committed counterpart; every committed file under a generated output dir is either produced by that module's controller-gen run or named in the script's `EXEMPT` list with the reason; and every committed `zz_generated.deepcopy.go` is still produced, with the same bytes.
   The one exemption today is the GMC-bundled `RunnerGroup` CRD, which is not GMC controller-gen output at all — [`make chart-crds-check`](#install-artifact-validation) holds it byte-identical to the AGC copy (Q73).

It regenerates into a scratch tree, never into the working tree, so it detects drift in the *committed* output (and any uncommitted hand-edit), not merely whether a regen-in-place produced a git diff.
Cost is six controller-gen runs over already-parsed packages plus one ~30 MB copy of the working tree, ~4 s, plus the one-time `.build/controller-gen` build.

**When it fails**, the remedy is `make generate` from the repo root — that regenerates both halves for the same three modules the gate checks, so running it is exactly what makes the gate pass.
Then `make chart-crds` to carry a CRD change into the Helm chart, and commit both. `make manifests` alone covers only the manifests half.
The failure message names the single `make -C <module> manifests`/`deepcopy` if you prefer the narrower run.
Never hand-edit the generated YAML or Go.

**Why the DeepCopy half is here.** It was left out at first, on the reasoning that a type needing new DeepCopy code fails to compile without it.
That holds for a *changed* type and is false for an *added* one: `ClusterCapacity` ([Q470](../STATUS.md), PR #917) shipped with no `DeepCopy`/`DeepCopyInto` at all and an `ActionsGatewaySpec.DeepCopyInto` that never copied the field, so `ActionsGateway.DeepCopy()` returned an object aliasing the caller's pointer — mutating the copy would reach the object in the shared informer cache.
Nothing failed to compile and nothing failed CI; it was found incidentally, by someone running `make generate` for an unrelated change ([Q477](../STATUS.md)).

**The DeepCopy half regenerates into a copy of the working tree**, not into a redirected output dir like the manifests half.
The `object` generator writes `zz_generated.deepcopy.go` beside its source, and controller-gen's output rule for it joins every package onto one path — `api/v2alpha1` and `api/v2beta1` would both land on the same file and the second would win.
So the gate copies the tree (skipping `.git`, `.build`, `.claude`, `tmp/`, and the two vendored trees, which no module's DeepCopy run reads: each module's `go.work.gen` is a single-module workspace, so the repo-root workspace `vendor/` never applies) and regenerates there.
It has to be the whole tree, not one module: `cmd/agc` and `cmd/gmc` reach their first-party dependencies through relative `replace … => ../../api` directives.
Inside the copy each module's committed DeepCopy is deleted before its own run, so a file that survives is one controller-gen no longer produces — and `DEEPCOPY_MODULES` is therefore in dependency order, `api` first.

Wired into `make check` and CI's `lint` job in [`unit-test.yml`](../../.github/workflows/unit-test.yml) — deliberately **not** `manifest-validate.yml`, whose `manifests` filter is scoped to the generated YAML by design.
The drift is caused by a Go type change that need not touch either module's YAML, which is exactly the diff the `code` filter sees.

**The `object` call it runs is the module's own, year included.** All three `deepcopy:` recipes run `object:headerFile="hack/boilerplate.go.txt",year=$(YEAR)`, and the script registers that string verbatim — `$(YEAR)` unexpanded, so assertion 2 can match it against the `Makefile` as text — then resolves the year from `date` at run time when it invokes controller-gen.
A hardcoded year would fail every build the following January.
It reaches no output today, because the boilerplate files are [empty by design](code-generation.md#no-per-file-license-headers), but the gate runs the module's call rather than a simplification of it.

**The recipe parsing is tab-sensitive, so it is tested.** Assertions 1 and 2 read each module's `manifests:` and `deepcopy:` recipes straight out of its `Makefile`, and a make recipe is indented with tabs and wrapped with backslash continuations. `module_recipe` folds those continuations into one line **and converts every tab to a space**, because `assert_registry_fidelity` matches each generator as `" $gen "` with `grep -F`: a generator that begins a wrapped continuation line is preceded by a tab, and without the conversion it is simply not found, so a faithful `MODULES` row gets reported as unfaithful.
PR #886 nearly shipped exactly that, and it would have stayed dormant — every real recipe happens to put all of its generators on the first line today, so the gate would have broken on the first rewrap of a `manifests:` target, not on the change that introduced the bug (Q457).

**A commented-out recipe line is not a recipe line.** `module_recipe` strips shell comments before folding, by the same rule the shell applies when it runs the recipe: an unquoted `#` at the start of a word begins a comment and everything to the end of that physical line is dropped — trailing backslash included, since a backslash inside a comment continues nothing.
A `#` that is quoted (`"… # …"`, `'… # …'`), backslash-escaped, or mid-word (`id#42`) is ordinary text and is kept, so a live call is never truncated at the first `#` in an `echo`.
Before Q464 nothing was stripped, and a commented-out call folded in as live text three ways: its `#` read as a generator, so the module was rejected for *"runs generator `#`"* — a name no generator has, over a call `make` never runs; a generator surviving only in a comment satisfied the registered-generator presence check, so the gate regenerated output `make manifests` no longer writes; and a commented-out `output:` rule satisfied the dir match that the live rule should have, letting a row point at the wrong committed dir.
That bug was found by the fixtures added with the suite itself, one PR after they landed (Q457 → Q464).

Two recipe shapes still truncate the parse: a blank line, and a make comment at column 0 (no tab). `make` ignores both and keeps reading; this parser stops.
Both are tolerable for the same reason — they truncate toward a **loud** failure, because the generators on the dropped lines then read as unregistered rather than being quietly skipped — and both are pinned as such.

That whole class is invisible to review and obvious to a fixture, so [`scripts/go/check-codegen-drift-test.sh`](../../scripts/go/check-codegen-drift-test.sh) (under `make scripts-test`) pins it: recipe fragments covering tabs, wrapping, quoting, a target with prerequisites, a module with no `manifests:` target, and every comment shape above, each with its expected parse, plus end-to-end fidelity cases asserting a faithful row over a tab-wrapped recipe passes and each unfaithful shape fails with an explanatory message.
The `tab-wrapped-generator` cases are the named regression for #886 — delete the `gsub(/\t/, " ", line)` from `module_recipe` and they fail; the comment cases are the named regression for Q464 — delete the `strip_comment` call and six of them fail, in both directions (a commented-out call must not be rejected, and a comment must not satisfy a check the live text should).
The `both-targets-*` cases are the named regression for Q477: `module_recipe` takes the target as a parameter, and a parser that returned the wrong target's recipe would fail *silently*, by regenerating something plausible.
The `deepcopy-generator-*` cases pin the `$(YEAR)` split — one asserts the expansion matches `date +%Y`, the other that the registered constant keeps `$(YEAR)` literal.
The script guards `main` with `if [[ "${BASH_SOURCE[0]}" == "${0}" ]]` so the suite can source it for those helpers without building controller-gen or regenerating anything; **keep that guard, and keep new parsing in a named function** rather than inline in an assertion, or it becomes untestable.
Assertion 3 (drift) still needs a real controller-gen run and stays in `make codegen-check`.

### The claude-usage snapshot gate

`make claude-usage-test` ([`scripts/agent/claude-usage-test.sh`](../../scripts/agent/claude-usage-test.sh)) runs the Python unit tests in [`claude-usage/`](../../claude-usage/README.md) — the only tests in the repo that aren't Go or bash.
That module is the committed record of the project's Claude Code usage, and for days whose session transcripts have since been archived its CSVs are the **only** surviving copy, so `compute_metrics.py`'s merge rule (never revise a recorded day downward, never collapse two machines' shares of one day) is load-bearing. `test_compute_metrics.py` pins it, but the module matched no `dorny/paths-filter` list — it is neither Go code nor a shell script — so the suite ran only when someone remembered to run it by hand, and PR #841 shipped a model-mapping fix that no gate would have caught (Q437).

Wired into `make check` and the `claude-usage-test` job in [`unit-test.yml`](../../.github/workflows/unit-test.yml), gated on a `claude_usage` filter (`claude-usage/**`, the gate script, the `Makefile`, the workflow).
The suite is stdlib-only — no venv, no `pip install`; that is only needed for `make_charts.py`'s matplotlib/numpy — so the job is a checkout, `setup-python`, and one `make` target, in seconds.

It **byte-compiles the module first** (`python3 -m compileall`, venvs and stale caches excluded).
That is the only coverage `make_charts.py` gets: it has no tests of its own and imports matplotlib/numpy, so nothing else parses it and a syntax error in it would reach `main` untouched.
Compiling does not import the file, so the unpinned chart dependencies are not needed — and equally, it proves only that the source parses, not that the charts render.
Bytecode goes to a throwaway tree via `PYTHONPYCACHEPREFIX`, so the gate writes nothing into the worktree it is checking.

Two things it refuses to let pass quietly, both instances of the green-by-skipping class above:

- **`python3` missing.** `python3` is an [extended-tier prerequisite](../../CONTRIBUTING.md), so a local run without it *skips* (like `scripts/docs/release-version-hook-test.sh`).
  On CI (`$CI` set) the same condition is a hard failure — a gate that reports green having run nothing is exactly what this gate exists to remove.
  The documented `CI=1 make check` throttle opt-out therefore fails on a python3-less machine; drop the `CI=1`.
- **Zero tests discovered.** `unittest discover` exits 0 on an empty run, so a suite renamed off the `test*.py` pattern would leave the gate passing while testing nothing.
  The gate fails on `Ran 0 tests`.

### Never foreground-poll CI, logs, or files

Do **not** run a blocking watch/tail on the main thread to wait for something to change — no `gh pr checks --watch`, no `gh run watch`, no `while … sleep` tail loops, no re-running `gh pr checks`/`gh run view`/`kubectl logs -f`/`tail -f` on a timer to see if a result has landed yet.
A foreground poll pins the session doing nothing until it times out or is killed, and it competes with the background machinery that already tracks these signals.
In a two-week sample this pattern alone produced ~130 blocked poll attempts.

Use the asynchronous mechanisms instead:

- **PRs and CI** — the Auto-fix/PR-monitor path watches CI and pushes fixes on its own (see [parallel-dispatch.md](parallel-dispatch.md) § self-healing); let it.
  If you need the current state right now, take **one** non-blocking snapshot (`gh pr checks <n>` without `--watch`, `gh run view <id>`) and move on — schedule a later re-check, don't spin.
- **Long-running local work** — launch it as a background task (a background Bash run, or a background agent) and let the completion notification wake you, rather than blocking the foreground on it.

The rule: a single point-in-time check is fine; a loop or a `--watch`/`-f` that blocks the main thread waiting for change is not.

**A background task already wakes you — don't poll it.** Once work is launched detached, its completion notification is the signal; scheduling `sleep N; tail <log>` as a *second* background task to check on the first is the same anti-pattern wearing a different hat.
It doesn't block the main thread, so it evades both the rule above and the foreground-guard hook, and it buys nothing the notification won't deliver.
Launch, then do unrelated work or end the turn — you will be re-invoked when it exits.

It is worse than merely useless, because a backgrounded `sleep` **is not a wait**: the launch returns immediately, so reading its output back gives you nothing and no time has actually passed.
Any subsequent "N seconds have elapsed, therefore …" is then unfounded — during Q549 that produced a confident wrong reading of a pod's age ("only 19 s old, so something must be recreating it") off a `sleep 150` that had never run.
If you need elapsed time to mean something, take it from a timestamp the system emits (`kubectl` `AGE`, a log `ts`, `date -u` at each end), never from a sleep you did not block on.

**"A single point-in-time check is fine" is not a licence to check fifteen times.** The carve-out above exists for *one* look when you need the state right now; repeating it every turn is a poll loop with the sleep outsourced to the conversation, and it evades the hooks for the same reason.
The Q528 session spent ~15 turns on `grep -c '' <log>` and `ps aux | grep ginkgo` against runs that were already going to wake it.
Two things make this tempting and both are traps: a buffered command (`make e2e-images` emits nothing until it finishes) makes progress look stalled, and a partially-written log invites reading a conclusion out of a line count.
Neither is evidence — the exit notification is.
If you genuinely cannot act until the run lands, end the turn.

### Slow tiers need an explicit timeout or a background run

The Bash tool's default timeout is short (two minutes) and it **kills** anything that overruns — in the same two-week sample, 36 slow runs were killed mid-flight this way, wasting the whole run.
Any invocation that can exceed the default — the envtest integration suites, the kind e2e tiers, and `go test -race` / `make test-race` above all — must therefore either:

- carry an **explicit timeout** on the Bash call generous enough to cover the run (up to the 10-minute Bash ceiling; use `make … TIMEOUT=…`/`go test -timeout …` for the test-level deadline as well), **or**
- be launched as a **background task** so it runs to completion detached and notifies on exit.

Never fire one of these as a default-timeout foreground run and hope it finishes — it will be killed partway and you learn nothing.
Pick the timeout from the tier's real cost (see [Cost & cadence](#cost--cadence-rough-ephemeral-ci-2026-ballparks) below); when in doubt, background it.

**Background it through [`record-launch.sh`](#the-launch-record)**, so the run keeps a stop handle after a compaction drops the task id.

**Read a background run's output, not its reported exit status.** The status you get back is the *last thing the command did* — which is rarely the thing under test.

- **Piped**: a run through `tail`, `head`, or `grep` reports the filter's code. `go test … | tail -30` reports exit 0 even when the suite failed.
  Drop the pipe (the output file is readable in full anyway) or add `set -o pipefail`.
- **Sequenced**: the same trap, one step removed, and easier to miss because nothing looks filtered. `make check > check.log 2>&1; tail -20 check.log` reports **`tail`'s** status, so a failed gate arrives as a green completion notification.
  Redirecting to a log and then peeking at it is the natural way to run a slow gate in the background, which is exactly why this one bites.

The fix for the sequenced form is to record the real status *into* the artifact and read it back, rather than trusting the notification:

```bash
make check > check.log 2>&1; echo "CHECK_EXIT=$?" >> check.log
```

Then confirm with `grep -n CHECK_EXIT check.log` alongside the `ok` / `FAIL` lines.
Either way the rule is the same: confirm a pass by reading the output, never from the reported exit code alone.

**An empty result set is not a pass.** The recorded status can be a clean `0` for a run that never happened — a `$(MAKE)` typo (make syntax; the shell reads it as command substitution) or any unresolvable command leaves a log holding nothing but a `command not found`, and the trailing `echo` dutifully records success.
So verify by presence, not absence of failure: the log must contain the `ok <package>` line for every package the tier was supposed to cover.
Zero `ok` lines and zero `FAIL` lines means the suite did not run.

Both rules in this section are enforced mechanically by the foreground-guard hook: it prompts on foreground watch/`sleep`-poll forms, and its slow-command registry in `.claude/foreground-guard.json` names the tiers above (`make test-race`, `make test-integration`, the `e2e` targets) with their minimum timeouts — keep that registry in sync when a tier's runtime or target name changes.

### Stopping a run: name the target, never the program

Killing a run to reclaim compute is legitimate and often correct.
The machine is shared across parallel worktree sessions, `make check` is long, and the heavy tiers (the kind e2e clusters, the dogfood validation gate) are effectively singletons that do not run concurrently on a small host.
Abandoning a run whose premise died is the right call, not a failure of discipline.

What is never right is naming the **program** rather than the **run**. `pkill -f "usr/bin/make check"` matches every parallel session's gate, not just the one you started.
Across local session transcripts, of 38 shell-kill targets exactly two carried a worktree anchor; the rest were bare program names (`ginkgo run --tags e2e`, `e2e.test`, `.build/ginkgo`, `make scripts-test`).
Q690's load harness cleaned up with `pkill -f 'make scripts-test'`, which matched the `make check` running for verification in the same worktree: the gate died mid-run, a sampled suite that had printed `all assertions passed` was recorded as a failure, and both contaminated results read as genuine red.

In order of preference:

- **Stop the background task by its handle.** The launching task is the only reference that cannot match somebody else's process.
- **Then the launch record**, if the run was started through [`record-launch.sh`](#the-launch-record), which holds the same handle on disk where a compaction cannot reach it. `scripts/agent/record-launch.sh --list` prints what is running and the command that stops it.
- **If neither exists**, run `pgrep -fl <pattern>` first, read what it *would* hit, then kill by PID.
- **If a pattern is unavoidable**, anchor it to the worktree path.
  Every process a session starts carries its worktree directory in the argv or cwd, so `pkill -f "<worktree>/.build/ginkgo"` is safe where `pkill -f ginkgo` is not.

**Never kill another worktree's run to reclaim a singleton.** A process you did not start carries no ownership record you can read, so "it looks stale" is a guess, and a live run and an orphan are indistinguishable from the outside.
[`scripts/dogfood/lib/lease.sh`](../../scripts/dogfood/lib/lease.sh) is the pattern that makes that legible for the billable cluster: a pid plus a command-line marker, host-wide so it survives across worktrees, and no-lease-no-reclaim.
The launch record below is worktree-local and records only your own runs, so it answers "what did I start" and not "may I reclaim this".
No local tier has an ownership lease yet (Q707).
Note that a lease directory has to be host-wide to be worth anything, which puts it outside the worktree: workspace-guard prompts on every access until its opt-in extra-roots ship (workspace-guard Q23).

#### The launch record

The launching task id is normally a run's only handle, and a compaction drops it while the process keeps running.
That is how a session ends up killing by pattern with nothing to aim at.
So put the handle on disk: launch long background work through the wrapper rather than directly.

```bash
scripts/agent/record-launch.sh make check > tmp/check.log 2>&1
```

[`scripts/agent/record-launch.sh`](../../scripts/agent/record-launch.sh) writes one `key=value` record per run under `tmp/launches/` (pid, worktree, the command, and a `stop=` line to run verbatim) and removes it when the run ends, so what is on disk is what is live. `--list` reads them back and `--prune` drops the ones whose process is gone.
Reading a record is not recall, which is the whole point: the fields are there after the context that created them is not.

Two properties make a record safe to act on, and both are asserted against real processes by [`record-launch-test.sh`](../../scripts/agent/record-launch-test.sh):

- **The run is its own process group**, so the recorded `kill -TERM -- -<pgid>` takes its children too.
  A `make` killed alone leaves its whole fan-out behind, which is exactly what sends the next person back to a pattern kill. `set -m` is what buys this; delete that line and the suite goes red on a surviving grandchild.
  If job control does not deliver a new group, the wrapper measures that (`ps -o pgid=`) and records the plain `kill -TERM <pid>` instead, since a group kill aimed at the wrapper's own group would take the session with it.
- **Liveness is pid plus command-line marker**, the `lease.sh` rule: a recycled pid is a live process that is not the run, so `--list` calls it `stale` rather than offering a stop command aimed at a stranger.

Two constraints come with it.
The run must not read stdin, because a background process group that reads the terminal takes SIGTTIN and stops, which is why the example redirects to a log, the shape a background run wants anyway.
And the wrapper owns the run: killing the wrapper stops the run, because a live process whose record has just been deleted is the state the record exists to prevent.

##### The pr-sentinel watcher is the exception: no wrapper, no redirect

Both halves of the shape above break the PR watcher, so it is the one long background run that goes neither through `record-launch.sh` nor into a log.
Launch it as the bare three tokens the pr-sentinel nudge prints, `bash "<absolute path>" <PR>`, as a background task; [parallel-dispatch.md](parallel-dispatch.md#primary-the-pr-sentinel-background-watcher) has the rest of the launch-form rules.
Measured 2026-08-11 against pr-sentinel 0.8.0 (`scripts/pr-sentinel-stop-hook.py`, `scripts/pr-sentinel-guard.py`):

- **A redirect is the silent one.** The stop hook learns each watcher's output file from the `<output-file>` field of the harness task-notification and reads that file directly, accepting a `PR-SENTINEL EVENT: ready`/`closed`/`blocked` marker only above the first CI-log excerpt banner.
  That marker is the entire handoff signal; the notification's status is read only as "the task exited", never for its value.
  A backgrounded command whose stdout is redirected leaves that file **empty** (measured: zero bytes, while the notification still reports `completed`, exit code 0), so the report lands in the log and the hook sees no handoff.
  The watcher is then no longer live and the PR is still owned and unconcluded, so the stop is blocked again over a PR the watcher already reported green.
  Reading the log afterwards does not recover it: the hook's only fallback is a transcript read of the task output file's own path, not of wherever the report was sent.
- **A foreground launch is not recorded at all.** The hook counts a watcher launch only on a Bash call carrying `run_in_background`, so a foreground run leaves the PR with no watcher on record however the run itself goes.
- **A wrapper or a redirect also costs the auto-approval.** The guard auto-allows only a single simple command of exactly three tokens whose `argv[0]` is `bash`, carrying no operator, redirect, substitution, or glob, so either one drops the launch to the base Bash permission prompt, where an unattended worker stalls with its PR unwatched.
  Foregrounding does not: the guard reads the command string alone, so a foreground launch is auto-approved and still invisible to the stop hook.

Skipping the wrapper costs nothing here.
The launch record exists to keep a compute-heavy run killable after a compaction drops its task id, and a watcher that sleeps between polls is neither worth reclaiming nor something to kill by pattern.

### Ad-hoc shell varies: don't rely on word-splitting

Committed scripts under `scripts/` are `#!/usr/bin/env bash` and follow [bash-style.md](bash-style.md), so their behaviour is pinned by the shebang. **Ad-hoc commands are not pinned** — they run in whatever login shell the contributor has: zsh on macOS (the default since Catalina), bash on most Linux distributions and CI images.
Check yours with `echo $0` rather than assuming.

That matters because the shells disagree on **word-splitting of unquoted parameter expansions** — bash and `sh` split, zsh does not:

```sh
FLAGS='-run TestFoo -count 1'
go test $FLAGS ./...   # bash/sh: four arguments.  zsh: ONE argument, the whole string.
```

Under zsh that `go test` receives a single literal argument `-run TestFoo -count 1` and fails to parse it — a confusing "unknown flag" or "no such file" from a snippet a bash reader would call correct.
Because the shell differs per contributor, an unquoted expansion is **not portable in either direction**: a recipe that works on a bash box breaks for the next person on macOS, and vice versa.

**The fix is almost always to drop the variable.** Ad-hoc commands are one-shots — write the arguments literally and there is nothing to split:

```sh
go test -run TestFoo -count 1 ./...
```

Reach for a variable only when something genuinely reuses the list — a loop, or a flag set applied to several commands in one session.
Then pick by where it has to run:

| Context | Form | Portability |
|---|---|---|
| bash or zsh (any interactive shell you'll actually meet) | `flags=(-run TestFoo -count 1)` → `go test "${flags[@]}" ./...` | bash + zsh. **Not POSIX** — `dash` rejects the `(` outright, so don't carry it into an `sh` context |
| must also run under `sh` | `set -- -run TestFoo -count 1` → `go test "$@" ./...` | every shell, including `dash`; costs you the positional parameters |
| worth keeping at all | a script under `scripts/` with a bash shebang | pinned by the shebang, and [shellcheck](#the-shellcheck-gate)-gated |

Note that "write POSIX" is **not** a fix on its own: zsh's not-splitting *is* its deviation from POSIX, so POSIX-style `$FLAGS` still breaks there.
Portability comes from the quoted form you choose, not from avoiding extensions.

Two things to avoid:

- **Don't "fix" a snippet by dropping quotes** and relying on splitting — that is the bash reading, and it silently does the wrong thing under zsh.
- **Don't reach for zsh's `${=VAR}`** (its explicit split-this operator) in anything shared. It is zsh-only: bash rejects it with `${=FLAGS}: bad substitution`, converting a portable command into one that fails for half the team.

## Picking the right test tier

Prefer the narrowest tier that can actually *observe* the bug class — but no narrower:

- **Unit (fake client)** — pure logic and field-level behavior.
  The fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`) reproduces none of the real-apiserver semantics below, so a fake-client test cannot prove claims that depend on them.
- **envtest (integration)** — any claim that depends on real-apiserver semantics: schema/admission defaulting, server-side no-op-write dedup (the apiserver skips the `resourceVersion` bump when a patch's defaulted result is unchanged), admission/validation webhooks and CEL, and `IsConflict` handling.
  What envtest does **not** enforce is RBAC — every test client runs as admin, so a controller write whose verb the shipped role never grants stays green here and 403s only on a real cluster (Q502 found the scale-set claim/completed-at pod patches had shipped that way; see [kubernetes-conventions.md § New controller write verbs](kubernetes-conventions.md#a-new-controller-write-verb-updates-the-role-pair-in-the-same-change)).
  A claim that a controller write works *under the shipped role* therefore needs the kind e2e tier, where the AGC runs under the chart's real `agc-tenant-role` — `E2E_AGC_ScaleSetRecovery` is the Q519 gate for the scale-set disruption-recovery writes, and `E2E_AGC_ScaleSetAcquisition` the Q528 gate for the tier's acquisition half.
  Both `cmd/agc` and `cmd/gmc` already have envtest suites at `internal/controller/integration/` (build tag `integration`, see [Integration tests](#integration-tests)) — add to them rather than concluding none exists; confirm with a directory listing before deciding a tier is missing.
  Example: PR #143 (Q65) migrated the GMC `apply*` helpers to `CreateOrPatch`; a fake-client test could verify field-level behavior, but only `apply_nochurn_test.go` (envtest, asserting `resourceVersion` stability across periodic reconciles) could prove the whole-`Spec` helpers don't churn.
- **cluster-only kind e2e** — behaviors that emerge from real Container Network Interface (CNI), kube-proxy Destination NAT (DNAT), kubelet image-pull policy, or TLS-over-tunnel.
  When a feature crosses one of those boundaries, the cluster-only test (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests) and [End-to-end tests](#end-to-end-tests)) is the only thing that proves it works.
  Example: PR #59 fixed 5 bugs that all unit tests passed for — a single planned-but-unimplemented cluster-only test (`E2E_GMC_TenantProvisioning_ProxyConnectWorks`) would have caught 4 of them locally.
- **Live autoscaler (kind + kwok)** — claims about a string, verb, or field an **upstream project** emits, where our side fails open and therefore stays green when upstream changes it.
  Recorded samples cannot observe that class at all.
  Today that is the capacity gate's autoscaler vocabulary; see [The live-autoscaler drift gate](#the-live-autoscaler-drift-gate).
- **Load (in-process)** — scaling claims about the AGC's own goroutine/memory/throughput footprint, not a functional bug class.
  The load harness (build tag `load`, see [Load tests](#load-tests)) drives the real listener-multiplexing core at thousands of concurrent virtual sessions without a cluster.
  Use it to pin a capacity claim or guard against a concurrency-core regression (goroutine leak, sustained-session collapse); it cannot speak to anything downstream of the AGC process (real pods, apiserver/GitHub latency).

Before concluding a test failure is a code bug, check whether the problem is in the test expectations, the test setup, or the code itself — the intent of the test must match the implementation.

## Diagnosing failures: measure before asserting a root cause

A root-cause claim needs evidence measured from *this* failure, not a resemblance to a remembered one.
Two shortcuts recur and both produce confident-but-wrong diagnoses:

- **Symptom-matching a prior issue.** When a failure looks like a known issue — a flake row on the [backlog](../STATUS.md), a previously fixed bug, a memory of "this is always X" — that match is a **hypothesis, not a diagnosis**.
  The same surface symptom (a scheduling timeout, an egress blip, a wedged run) can have a different cause each time.
  Before acting on the remembered cause — and above all before spending a billable re-run, a fix PR, or a state-changing command on it — take a direct measurement from the failing system: read the actual events, describe the actual pod, pull the actual log line.
  If the environment tears down evidence on failure, capturing diagnostics *before* teardown is part of the fix, not optional (filed from the v1.2.0 release retro, where gate failures had to be re-run just to observe them).
- **Trusting source inspection.** Reading code — or a plan doc's ✅ investigation findings, which usually derive from source-reading — tells you what *should* happen, not what does.
  Treat such findings as unverified until confirmed end-to-end: actually exec the thing.
  Source-reading alone has produced wrong conclusions before (PR #59).
- **Reading CI evidence after re-running the job.** `gh run view <id> --log-failed` reports the **latest attempt**, so re-running a red job destroys the view of the failure you are diagnosing: it returns the new attempt's (empty) failure set, which looks exactly like "the diagnostic never ran".
  Recover the original with `gh run view <id> --attempt N --log`, and — since an empty result is only evidence of absence once the command is known to have run — grep for a string you *know* that log contains before reading any emptiness as a finding.
  Q648 was only findable this way: the attribution banner it turns on was present in attempt 1 and absent from the post-re-run view.
  Capture the evidence **before** spending the re-run where you can.

### The status you report is a claim too

The rules above govern *why* something failed.
The same discipline applies to the plainer statements that carry a decision — "the gate is green", "the pods are stuck", "the drain is converging".
Each is a claim about state, and each has a cheap way of being wrong:

- **An exit code read through a pipe is the pipe's.** `make check | tail -40` reports `tail`'s status, so a failing gate reads as `0`.
  Redirect to a file and read `$?` from the command itself — then reconcile it against the output, because a gate that exits 0 while its own log carries a `FAIL` line is the other half of the same trap. This one has a mechanical check now (Q625): [`scripts/agent/claude-piped-gate-hook.sh`](../../scripts/agent/claude-piped-gate-hook.sh) is a `PreToolUse` hook that denies a Bash call that pipes a registered gate into a filter, or reads `$PIPESTATUS` (which does not exist in zsh, the shell the Bash tool runs, and expands to empty there).
  It denies rather than asks because a deny's reason is shown to the model and an ask's to the user, so the fix lands where the command gets rewritten instead of being relayed by hand (Q697).
  Wanting the output rather than the status is legitimate and indistinguishable from the bug, so every verdict has a break-glass: re-run prefixed with `PIPED_GATE_OVERRIDE=<reason>`, and file a Queue row when the rule, not the call, is what is wrong.
  The gate list is `.claude/piped-gate-guard.json`, and what the hook cannot see is enumerated in the package comment of [`devtools/agent/pipedgate`](../../devtools/agent/pipedgate/), which is where the decision actually lives.
  The same hook carries two warnings about repository state rather than exit status: a `git push` onto a base that moved into this branch's own files (Q665), and a `gh pr create` overlapping an open PR (Q668).
  Both are documented where their rules are, in [CONTRIBUTING.md](../../CONTRIBUTING.md#pushing-to-a-pr-that-is-already-open).
- **Backgrounded, `; echo "EXIT=$?"` reports the `echo`'s status, not the gate's.** The redirect-and-echo idiom above is written for a foreground run, where the echoed line is the thing you read. Run with `run_in_background: true` and the completion notification carries the *chain's* status instead. A chain ending in `echo` always ends 0, so a gate that failed arrives as `completed (exit code 0)`: the most authoritative-looking signal in the session, and the only one that is wrong. Preserve the status explicitly, and still reconcile against the log: `make check > tmp/check.log 2>&1; rc=$?; echo "EXIT=$rc"; exit $rc`.
  Confirmed directly: `( false; echo "EXIT=$?" )` prints `EXIT=1` and exits 0; the `rc=` form prints `EXIT=1` and exits 1. Re-measured through the Bash tool on 2026-08-04, which is the path that matters: a backgrounded `false > /dev/null 2>&1; echo "EXIT=$?"` wrote `EXIT=1` to its output file and notified `completed (exit code 0)`.
  The same hook as above covers this shape (Q681): it asks when a **backgrounded** call names a registered gate and ends in something that cannot carry the gate's failure out (an `echo`, a `||` fallback, a trailing `&`), and stays silent for the `exit $rc` form, for the gate as the last statement, and for the identical command run in the *foreground*, where the echo is the thing you read.
- **A tool that exits 0 having printed nothing may have checked nothing.** `gh attestation verify` writes its summary only to a TTY; captured or redirected it is silent, and silence is indistinguishable from a no-op.
  When a verification's whole value is that it ran, make it emit something assertable (`--format json`, then read the predicate) rather than trusting the status alone.
- **A state observed once is not a steady state.** Pods wedged now may clear in ten minutes; a set that looks static may be churning underneath a stable count.
  Before calling a condition permanent, take a second reading far enough apart to tell the difference, and compare *identities* rather than counts.
- **A count grouped by symptom is not a measurement of cause.** A tool's ranking groups records by whatever its normalizer could see, which is seldom what shares a mechanism: workspace-guard's friction report folds `f=$(ls -t …)`, `for f in <glob>`, and a literal `f=/path` into a single `$f` row, so one count of 31 supported four incompatible explanations — the pattern actually being claimed was 4 of them, none in the previous seven days.
  Re-derive the aggregate along the axis the claim needs (time, construct, actor) before naming a cause; and when the claim is causal, **exercise the system that would produce the effect** rather than counting records that correlate with it.
  Nineteen prompts whose command text contained a scratchpad path read as a guard defect until the guard was fed a payload directly, which showed it already exempts the session's own scratchpad and every one of the 19 was a correctly-flagged cross-session access.
- **A count or a superlative is a claim, not a recollection.** "Six of them", "the only one without a test", "none of the workflows" — each asserts a *population* and a *width of scan*, and both go stale the moment the tree moves.
  A scan run earlier in the session for another purpose does not transfer: re-derive the number as you write the sentence, and say what population it is true of.
  The [markdown-gates plan](../plan/markdown-gates-parser.md) first called `check-doc-links.sh` the only script here with no `-test.sh` companion, from memory of an earlier sweep; re-running it found six of the fourteen scripts in `scripts/docs/` untested, and the claim shipped rescoped to the four gates that plan covers, where it is exactly true.
  Re-derived while writing this line: seven of fourteen, because `gen-api-reference.sh` has landed since.
  The number moved in a day.
- **An instrument's total is bounded by what it can observe, and an event it never saw leaves no gap.** Establish the blind spots before quoting the number, because nothing in the output will.
  The guard friction reports rank PreToolUse decisions, so foreground-guard's 200 prompts — quoted upstream as the largest single source of friction here — is a **floor**: the hook returns ahead of all analysis when the payload carries `run_in_background: true` (`karlkfi/claude-foreground-guard#15`), so every backgrounded poll is absent by construction. pr-sentinel scored near zero for a sharper reason: its defect lives in a watcher script that no PreToolUse analyzer observes at all.
  Quote a total with the shape it cannot see named beside it.
- **A sound instrument still answers only its own question, which is usually narrower than the claim.** The bullets above are corrupted or absent signals; this one is a probe working perfectly and being read for more than it measures.
  Q710 shipped a wrong sentence twice this way.
  A token-multiset reconciliation over a prose edit came back clean and was reported as "no qualifier was lost", which was true, and as "the split preserved the meaning", which it cannot see, because the defect was a pronoun the edit *added*.
  Then a scan for damaged paragraph labels reported 22 sites by finding every label with an unlabelled paragraph after it, never asking which of those *this change* produced; the answer was 8, and two structural edits had already been made on the 22. **A claim about what a change did needs a before-and-after measurement, not a scan of the after.** Run the probe against the base tree too, and diff the two.

- **A gate that fails on your branch is not yours until it fails on the base too.** The bullet above asks for a before-and-after when you are claiming what a change *did*; this is the same move aimed at the more common question, *is this failure even mine*.
  A red gate on a working branch reads as caused by that branch, because that is the only thing you changed, and a plausible mechanism is always available: a test you touched, a list you widened, the runs you had going in parallel.
  Check out the base, run the same gate, and read it before naming a cause.
  Q166 spent two wrong hypotheses on this.
  A red envtest suite was blamed first on the session's own concurrent test runs, then on a cluster-wide List the branch had added, which was narrowed on that theory for no improvement; `origin/main` alone then measured 324.5s against a 5m timeout with none of the branch's code, and the failing test's name had moved between runs because what was actually failing was the suite's wall clock (Q741).
  The tell is a failing test whose identity changes run to run, or one in a file the diff never touches: both say the branch is a bystander.
  The tier now says it for you: a breach of the [envtest suite budget](#the-envtest-suite-budget) reports that the *suite* ran out of time and names the panic's test as a bystander, rather than leaving the reader to notice that the name moved.
- **A check sequenced before a state-changing command does not gate it.** `scripts/docs/lint-backlog.sh > tmp/lint.log; git add docs/STATUS.md && git commit` runs the commit whatever the linter said, because `;` sequences rather than conditions, and the `&&` binds the commit to `git add` instead.
  This is the sibling of the pipe traps above: there the status is misread, here it is read correctly and then ignored.
  Bind the mutation to the check that authorizes it, `scripts/docs/lint-backlog.sh && git add docs/STATUS.md && git commit`, or branch on `$?` explicitly.
  The pre-commit hook backstops this particular file, and a backstop is not a reason to write the chain so that it depends on one.


The failure mode these share is reporting a conclusion from a signal that does not carry it.
The fix is the same each time: name the signal the claim actually depends on, confirm it could have shown you the opposite, and read that one.

### Check for a committed capture before booking a live measurement

"Measure it" does not always mean "run it".
Before scheduling a Tier C run, a dogfood dispatch, or anything else that costs a credential and a wall-clock hour, check whether the repo already holds a **recorded observation** of the same interface. `cmd/probe` exists to capture exactly that, and [`testdata/README.md`](../../testdata/README.md) documents what each capture contains.

Q495 is the worked example.
Its backlog row read "confirm, then fix", and both it and the [Q459 plan](../plan/archive/q459-drained-worker-recovery.md) budgeted a Tier C run that would evict a real job and watch for the skip.
The answer was already committed: `testdata/job_payload.json` is a redacted capture of a live `acquirejob` response, and parsing it shows in seconds that the run identity lives in `contextData.github` and that none of the fields the AGC was reading exist at all.
The measurement was free; only nobody had looked.

The corollary is the reason it stayed unlooked-at for so long: **a committed capture with no test asserting against it is decoration.** It cannot detect the drift it was captured to pin down, and it ages into a file everyone assumes someone else is checking.
When you commit a capture, commit the test that reads it in the same change — and when a capture already exists for an interface you are changing, assert against it rather than a payload you wrote yourself.

**Live observations are filed by when they were taken, not by what they answer.** `cmd/probe` and `testdata/` are the two shelves anyone would think to check, and they are not the only ones a live answer lands on.
A probe's *findings* are written up in the plan doc that commissioned it, which is archived the moment that plan closes, and the durable residue of a live run is often one corrected constant in a fake.
Search all four shelves before booking the run:

- `testdata/` and its [README](../../testdata/README.md) — the committed payload captures.
- `cmd/probe` and [the credential-gated probe scenarios](#the-credential-gated-probe-scenarios) — what each investigation was built to settle.
- `docs/plan/` **and `docs/plan/archive/`** — the results tables.
  Grep the archive; a closed plan's measurements do not expire with it.
- The doubles.
  A constant a live run corrected is a recorded observation wearing a code comment.

Q501's Phase 0 is the worked example, and the second recurrence after Q495.
It asked whether a cancelled run is signalled at all on the ScaleSet tier, and budgeted a live run to find out.
Q468's retention probe had already cancelled a real run and recorded the answer — the job's `JobCompleted` with `result: canceled` on the queue ~0.2 s later — in the results table of [an archived plan](../plan/archive/q468-jobcompleted-retention.md), and that same run's one-L spelling correction was sitting in a comment in `scaleset/scalesetstub/stub.go`.
Both were findable only by someone who already knew they existed.

So when you take a live measurement, **file it where the next reader will look for the interface**, not only in the doc that commissioned it: name the question in the capture's or constant's own comment, and cite the results table from the code its answer constrains.

### A goroutine stack in the output is not always a failure

Read the `--- FAIL` line and the exit code before attributing a stack trace to the test it appears under.
One stack in particular is printed by a **passing** run:

```text
[controller-runtime] log.SetLogger(...) was never called; logs will not be displayed.
Detected at:
	>  goroutine 307 [running]:
	...
```

controller-runtime's root logger begins unfulfilled.
If nothing calls `log.SetLogger` within **30 seconds of process start**, the next call through the root logger fulfills it with a null sink and writes that banner plus the calling goroutine's entire stack to stderr — a passing test, exit code 0, output that reads like a panic.
Because it fires on the first log call *after* the mark, the test it names is decided by how far the binary got in 30 seconds, i.e. by host load: on an idle machine a fast package never reaches the mark at all, which is why the artifact "does not reproduce" (Q455 — reported against `TestReconcileDelete_FailClosedOnDeleteError`, which the same package reproduces against a different test under heavier load).

GMC test binaries close this off by installing a logger before any test runs:

```go
func TestMain(m *testing.M) {
	logtest.Install() // cmd/gmc/internal/logtest
	os.Exit(m.Run())
}
```

**Add that `TestMain` to any new GMC test package whose code under test logs through controller-runtime**, so the package's output never becomes a function of how long it takes to run.
Under `-v` (`V=1 make test`, `V=1 make cover-check`) the installed logger writes to stderr, so controller-runtime output is available when you want it; otherwise it discards.

### A flake that passed on rerun has two logs — diff them

A rerun that passes is not the flake's dismissal; it is **half the evidence**.
GitHub keeps each attempt separately, so one run ID yields both a failing and a passing log over the same test on the same commit:

```bash
gh run view <run-id> --attempt 1 --job <job-id> --log
```

Without `--attempt`, `gh` serves the *latest* attempt — the one that passed — which is why the failing log is easy to miss entirely.

Diff the two across the failing test's window instead of reading the failing one alone.
What the failing side contains **and the passing side does not** is the defect, and it is usually one line.

Q559 is the worked example.
Its row read "a closed capacity gate never rejected the delivery" — a timeout, which reads as a slow wait.
The failing attempt carried an `AcquireJob` line in the test's own namespace at the instant of the enqueue, plus a minted job Secret at teardown; the 17.00s passing attempt carried neither.
So the job had been **admitted**, not slowly rejected.
That reclassifies the fix from "raise the timeout" to "synchronize on the right signal" before any code is read — and rules the timeout out entirely, since on that tier nothing redelivers.

The general form: **a flake's two attempts are a controlled experiment CI already ran for you** — same commit, same suite, one bit different.
Diff them before forming a hypothesis, not after one fails to hold up.

### An assertion against live state must keep its subject's output

The section above assumes the failing log says something.
An assertion that runs its subject with `>/dev/null 2>&1` and prints a fixed message guarantees it does not — and a transient failure never comes back to be re-read.

Distinguish the two kinds of assertion a script test makes:

- **Against a fixture** — the inputs are constructed, so the failure can only be the condition asserted.
  A fixed message is fine.
- **Against live, mutable state** (the tracked tree, the real workflows, the workspace) — the failure need not be the condition asserted.
  It can be a missing directory, an unreadable file, a mid-run abort, or something transient.
  Capture the subject's output and exit code, and print both on failure.

The remedy `run <script> yourself for the report` is not one.
In CI there is nobody at that shell, and if the cause was transient the re-run is green and the evidence is gone for good.

Q596 is the worked example, and it cost a full session. `check-v2-api-sync-test`'s `tree-in-sync` — the one assertion reading the live `api/` tree — ran the gate as `>/dev/null 2>&1` and printed `packages diverge` for *any* non-zero exit.
Replaying it against stubs exiting 1 (real drift), 2 (missing directory) and 3 (mid-run abort) produced three identical, evidence-free lines.
The single occurrence was undiagnosable by construction, and 2,418 reproduction runs never recovered the trigger.
Note the shape: in all three of the scripts that had this bug, the *fixture* helper in the same file captured output correctly and only the live-state assertion threw it away.

### A test's environment assumptions must be probed, not inferred

`runs-on` is an expression here, not a constant.
Nine jobs resolve theirs at run time — seven in `unit-test.yml`, `integration-test`, and the e2e job in `e2e-reusable.yml` — so a `workflow_dispatch` with `target_gag=true` (or, for e2e, merely `vars.GAG_E2E_RUNNER` being set, which is not dispatch-gated) routes them to the self-hosted dogfood runner instead of `ubuntu-latest`. **"It passed in CI" is a statement about one runner image, not about where the test runs next.**

So a test that depends on the environment — the uid it runs as, a tool on `PATH`, whether mode bits bite — must **attempt the operation and branch on the result**:

- **Probe the capability, never a proxy for it.** `os.Geteuid()`, `id -u` and `runtime.GOOS` are proxies, and they are wrong in both directions: a uid-0 container with `drop: ALL` *cannot* read a mode-000 file, and a non-root process holding `CAP_DAC_OVERRIDE` can.
  Reading the uid answers a different question than the one asserted.
- **Skip on the probe, and say what it observed.** A silent skip and a passing test look identical in the log, which is how a gate rots into a no-op.
- **When the capability is a job prerequisite rather than a test variable, provision it instead** — install it in the workflow so the verdict is identical on both runner types.

The reviewer's tell: a test that names an environment fact in a comment or skip message but never reads it.

Both remedies are in the record. **Q482** is the provision case: the `shellcheck` job runs `make scripts-test`, whose `scripts/go/go-vet-tags-test.sh` shells out to a real `go`. `ubuntu-latest` preinstalls one; the dogfood runner image omits it by design.
The dependency was never written down or checked, so every `target_gag=true` dispatch reported a red that was not a failure.
The fix added the same pinned `setup-go` step its six sibling jobs already had. **Q596** is the probe case: `check-v2-api-sync-test`'s `unreadable-file` assertion `chmod 000`s a fixture and requires the gate to call it trouble, gated on whether `cat` actually fails — not on `$EUID`, precisely because uid does not settle whether the mode bits bite on this runner.

Q482 established this for the Go toolchain only.
It covers both languages and every tier those nine jobs carry: Go unit tests, the `scripts/` shell suites under `make scripts-test`, integration, and e2e. `TestBuildTrustPool_PreservesSystemRoots` (`cmd/agc/internal/transport/trustpool_test.go`) and the `python3` guards in `scripts/docs/*-hook-test.sh` are the models in each.
For the mode-bits case in Go, `writeUnreadable` (`cmd/worker/worker_test.go`) is the direct port of Q596's shell probe: it `chmod 000`s the fixture, reads it back, and skips only when that read succeeds (Q641).

### Proving a flake fix: invert it

Repeated passes do not validate a flake fix.
A green `-count=20` is equally consistent with *"the race is closed"* and *"the race didn't fire this time"* — and on an unloaded dev machine the second is the more likely of the two, because the timing that produces the flake on a loaded CI runner often can't be reproduced locally at all.
Passing-after is necessary evidence, not sufficient evidence.

The same holds at the other end of the job: repeated runs rarely *reproduce* one either, so `-count` is the wrong opening move.
Name the interleaving the failure needs, then [widen the window and drive it](#synchronize-on-the-signal-you-assert-on).
Q685 spent 200 clean repetitions on a test CI had just failed, and reproduced it 4 times in 60 as soon as the stop was taken at maximum pressure rather than on the test's own ticker.

Run the **negative control** before concluding: invert the fix — restore the old value, remove the pin, revert the ordering — and confirm the suite *fails*.
A fix you cannot make fail on demand has not been shown to be load-bearing, and shipping it closes the backlog row while leaving the flake live.
When the inverted form refuses to fail either, that is itself the finding: the diagnosis is wrong, or the mechanism isn't the one you think it is.

Q378 is the worked example.
Pinning `BaselineRecheckInterval` in the reaper tests passed 10× under `-race`, which on its own proved nothing; setting the pin to 1s instead — and watching the suite fail — is what established that the pin was the thing closing the race.

The same control applies to any **regression test for a bug fix**, where it is cheaper still: run the new test against the unfixed code and confirm it fails *with the symptom you diagnosed*, not merely that it fails.
Q495's ground-truth test, run against the old parser, returned owner `""`, repo `""`, run `0` — the exact shape observed on the real worker pod, which is what made it a guard rather than a restatement of the fix.

The [structural-ceiling triage](technical-debt.md#distinguish-a-fixable-defect-from-an-external-structural-ceiling) is the same principle at a larger scale: when fixes stop converging, isolate and *measure* the external actor instead of asserting the next on-our-side cause.

### A long `-count` run exhausts the host's ephemeral ports, and it looks like a flake

Stress-running is the first move when chasing a flake, so this trap is reached early and misreads as a second bug.
Every iteration of an `httptest`-backed suite stands up a fresh listener and its clients churn connections; past roughly 200 iterations in **one process** the host runs out of ephemeral ports and dials start failing with:

```text
dial tcp 127.0.0.1:61609: connect: can't assign requested address
```

The failure the test *reports* is nowhere near that line.
The connection error lands on some background poll, and what you see is an unrelated `Eventually` giving up ("Condition never satisfied") on an assertion that has nothing to do with the race you are hunting.

Two things tell it apart from a real flake:

- **`grep` the run for `can't assign requested address`** before reading the `--- FAIL`.
  Its presence means the run is measuring the host, not the code.
- **Run the identical loop against the unmodified tree.** If the baseline fails at the same rate with the same cause, it is environmental.
  This is worth the minutes: it is the difference between a second bug and a saturated laptop.

Avoid it by pacing — many short runs (`for i in $(seq 1 20); do … -count=5; done`) rather than one long `-count=200`, so sockets drain between processes.
Measured on the `brokertest`-backed `cmd/agc/internal/listener` suite (macOS), where `Server.HTTPClient()` hands out `http.DefaultClient`; the mechanism is not specific to that double, so treat any long single-process `-count` over an `httptest` suite as suspect.
Q490 hit it twice while deflaking the Q260 fan-out gate and burned a full baseline comparison to rule it out.

### A `-count≥2` run fails absolute assertions on process-global metrics

The other stress-run look-alike. `runnercore.NewMetrics` registers with the global controller-runtime registry, which panics on duplicate registration, so a test binary builds it once and every test — and every `-count` repetition — shares the instance.
A test asserting a counter's **absolute** value passes at `-count=1` and fails deterministically from the second repetition on, because the previous run already incremented the same series.
Reached exactly when stress-running a flake, and it reads like one, but the tell is the inverse of a real flake's: it reproduces at any load and any `GOMAXPROCS`, always at repetition ≥ 2, with `actual = expected × count`.
Prefer building the metrics the exercised path records into as a fresh, **unregistered** `&runnercore.Metrics{…}` per test, so no series is shared in the first place — `newTestMetrics` (`cmd/agc/internal/provisioner/provisioner_test.go`) and `rerunLoopMetrics` (`eviction_internal_test.go`) are the models, and `p.Metrics` field accesses are nil-guarded for exactly this.
Populate only the fields the path touches; a nil field panics loudly rather than flaking.
When a test genuinely needs the registered global, assert the delta around the action (read the counter before, compare after) rather than an absolute.
Either way, stress with many `-count=1` processes — which the port trap above already requires.

### A negative assertion must be able to fail for only one reason

A test that asserts something *did not happen* passes when the mechanism is absent — and also when the mechanism is present but misdirected, misconfigured, or erroring out before it gets anywhere.
Those are indistinguishable from the assertion's point of view, so a green negative test is much weaker evidence than a green positive one, and it stays green while the thing it guards rots.

**Q504 is the worked example, and it cost a release-blocking bug most of a year.** Every spec on the `rerun-failed-jobs` path asserted an *absence*: the drain specs and the pre-Q497 preemption spec all checked that no re-run fired.
Meanwhile the AGC never assigned `Provisioner.GitHubAPIURL`, so the call silently defaulted to `api.github.com` regardless of `GITHUB_API_BASE_URL` — eviction recovery could not work on GHES at all.
Every one of those specs kept passing, because a re-run posted to the wrong host is exactly as absent, from `fakegithub`'s counter, as a re-run never attempted.
The first spec to assert a **successful** re-run (`E2E_AGC_PreemptedWorkerIsRecovered`, Q497) found it on its first CI run.

So when you write a negative assertion:

- **Pair it with a positive one somewhere in the suite.** If nothing in the repo asserts the mechanism *works*, "it didn't fire" is unfalsifiable.
  This is the cheap fix and usually the right one.
- **Guard the observability first, in the same spec.** Assert that a firing would have been visible before asserting it did not fire — the preemption and drain specs check `GITHUB_API_BASE_URL` addresses fakegithub and pin a payload carrying a complete run identity, precisely so an absent re-run cannot be an absent *instrument*.
  That guard is what keeps the test honest; note it still could not catch Q504, because it validated the AGC's env var rather than where the call actually went.
- **Prefer asserting the specific wrong thing did not happen** over asserting nothing happened. `seq` must not contain `Failed/Evicted` is a claim about a mechanism; "the counter stayed 0" is a claim about the whole world, and the world has many ways to be quiet.
- **A poll or sampler cannot establish "X never happens."** A negative claim about an API-object transition needs watch/informer-level evidence: Q502 designed against "a drained Pending worker publishes no terminal phase," measured by a 200 ms phase sampler — but a real kubelet publishes a *transient* `Failed`-with-`deletionTimestamp` that samplers miss and the production informer sees, and the mark-only rule built on that measurement re-ran a job that never ran.
  A sampler bounds how often X was *observed*, never whether X *happened*.

This is the negative-control idea above, aimed at the test's *premise* rather than its fix: ask what else would produce this same green, and close off the answers.

**A positive assertion can be vacuous too — make the double testify.** The rule above is about "X did not happen"; the mirror case is "X happened" satisfied by state that predated the test.
It bites hardest where the whole chain is fast: `E2E_AGC_ScaleSetAcquisition` drives an in-cluster stub whose long polls wake on enqueue, so its JobAvailable→acquire spec completes in ~34 ms — enqueue, claim, provision, assert.
At that resolution a real pass and one satisfied by a leftover pod or an earlier spec's claim count are indistinguishable from the spec's own timings, and the first green run looked exactly like the vacuous one would have.

The fix is not a sleep or a tighter matcher; it is to assert against the *server's* record of what it was asked to do.
That spec attaches fakegithub's ordered call log — `create-session`, `poll cap=10`, `acquirejobs auth=queue ids=[…]`, `generatejitconfig` — as a report entry.
It is diagnostics, not a gate (the assertions remain the gate), but it is what distinguishes "the AGC did this" from "this was already true."
Reach for it when a spec's subject is a *sequence of calls* rather than a final state: the in-repo doubles all keep an ordered log (`Calls()` on the scale-set stub, `/control/reruns` and `/control/scaleset/state` on fakegithub) precisely so a spec can assert on what the server saw instead of inferring it from the client's logs.

### An aggregate counter cannot count distinct participants

A threshold on a shared counter — `deduped >= n-1`, `retries >= 3`, `errors >= workers` — reads like a claim about *how many actors* did the thing.
It is not.
It is a claim about *how many times* the thing happened, and one actor in a loop satisfies it alone.
The two are indistinguishable in the metric, and the looping actor is usually the fast path, so the threshold clears early and the test goes green well before the shape it names has occurred.

Q260's fan-out regression asserted that a job's duplicate deliveries were deduplicated, via `JobsDuplicateDeliveryTotal >= maxListeners-1` on one registry shared by all five sessions.
A deduped loser sets no `RecycleAgent`, so it returns straight to the poll loop and is deduped again within microseconds.
Measured (Q601): the counter had overshot to 6–24 by the time the assertion first sampled it, and a two-session pool — where only one sibling can ever lose — still cleared the five-session threshold, once from a *single* session.
The claim registry's actual job, spreading the dedup across distinct siblings, was never under test.

Give each actor its own counter and count the actors that moved.
Each session's `Config` now carries its own `newTestMetrics()` registry, keyed by the Multiplexer's monotonic goroutine index, so a non-zero counter names one session and the assertion is `maxListeners-1` *distinct* registries non-zero.

Then close the sequential loophole.
N distinct identities can also be N actors one after another, so pin the population as well: the test asserts exactly `maxListeners` sessions were ever created and all are still alive, which leaves the deduped identities no reading but concurrent siblings.

The negative control is the check that settles it — shrink the population below the threshold and require red.
A pool of two cannot produce four deduped siblings; an assertion that still passes there is counting something else.

### A bulk mechanical change proves itself by reconciliation, not by an empty leftover query

A repo-wide rename or rewrite — moving a directory, renaming a symbol, re-pointing every reference to a path — is finished when *every* site changed.
The natural check is to grep for the old form and see nothing. **That check cannot distinguish "no sites remain" from "my query never matched the sites."** Both print nothing, and a rewrite and its verification query are usually written minutes apart from the same wrong mental model, so they fail together.

Q571 moved 99 scripts and rewrote their references.
The rewrite's regex carried a lookbehind that excluded a preceding `/`, so every `"$REPO_ROOT/scripts/foo.sh"` was silently skipped — 62 files.
The leftover grep returned empty and read as done; the misses surfaced by accident while reading an unrelated file.

Reconciliation is one-directional: it proves nothing was *removed*, and is blind to anything the change *added*.
That is enough for a mechanical rename, whose whole content is removals and replacements, and not enough for a prose edit, where an invented connective word can change what a sentence refers to while every count stays put (Q710).

Two checks, both cheap, and the first is the one that actually catches this:

- **A positive control.** Before trusting the sweep, name one site you *know* must change and assert it did.
  One `grep` for a known-affected line is enough.
  A query that cannot match its own known case is broken, and this is the only check that says so.
- **Reconcile counts.** Count occurrences of the old form before, count the new form after, and require them to agree. `762 rewritten` means nothing on its own; `762 rewritten, 762 found beforehand` is the claim worth making.
  A shortfall names how many sites the sweep missed even when you cannot yet say which.
- **Take the baseline with a query that spans every shape.** Reconciliation compares two numbers, so if both come from the same too-narrow query they agree and the sweep is still half-done — the check reports success at exactly the moment it is blind.
  Splitting the `infra` Queue label counted rows by reading the Labels cell as the fourth `|`-field: right for the Queue and Deferred tables, wrong for the Progress table, where it is the third.
  The baseline read 59 rows against a true 69, and a work-list built from it would have left ten rows untouched and reconciled cleanly at 59/59.
  Enumerate the shapes the change spans — tables, file types, call forms — and confirm the baseline query finds each one before trusting its number.

Then run the leftover query — as a third check, not the only one.
The same applies to the tool doing the work: prefer a script that *reports what it changed* per file over one that edits silently, so the count is available without a second pass.

### A throwaway probe needs a positive control before its silence means anything

The rule above puts a positive control on the *query*.
It belongs on the *harness* too, and for a sharper reason: an ad-hoc probe written to answer "does this still fire?" usually reports a finding when it fires and nothing when it does not — so a broken harness and a clean result are the same output.
The probe is code you wrote minutes ago and have never seen work.

Q616 registered two script paths in the foreground-guard slow-command registry as bare patterns, which made every command that merely *named* those files ask for a two-hour timeout.
Verifying the fix meant driving the hook with hand-built payloads.
All twelve cases came back "no match", including the five that had to match — and the harness was the problem: the payload was missing a field the hook needs, so it exited before reaching any decision.
Nothing distinguished that from a fix that worked.

- **Include one case you know must fire**, ideally one that predates your change — here, a pattern that had been in the registry for months.
  If the control does not fire, you have learned about your harness, not about the subject.
- **Print the raw output once**, not just your pass/fail verdict.
  Empty output where a decision was expected names the failure immediately; a verdict derived from it hides the failure as a pass.
- **When the harness cannot be made to work, verify the layer you actually changed.** The hook applies each pattern as `re.search(pattern, command)`; asserting that step directly is honest and sufficient, as long as you say which layer the evidence covers — and then confirm end-to-end in the smallest possible way (re-running the exact command that had been denied).

`scripts/agent/foreground-guard-patterns-test.sh` is the durable form of that probe, controls included.

### A credential-gated spec that skips is not defending anything

A spec that `Skip`s for want of credentials reports the same colour as one that ran and passed.
Nothing in CI tells the two apart, so the invariant it asserts stops being enforced the moment the credentials are absent, which for the live-GitHub tier is every PR.

Q599 is the case. `E2E_GitHub_CancelledRunLeavesNoDeletionMark` asserted that a cancelled run's worker pod never publishes a `deletionTimestamp`. #1032 then gave the AGC a delete for exactly that pod (the Q501 reclaim of an abandoned job's worker).
The spec's assertion was now false against a *working* gateway, and no gate said so; the contradiction shipped and was found later, by reading.
The code obligation had been written down (`deletion.go` requires any new deletion path to apply the AGC-own exclusion) and #1032 honoured it.
The specs obligation was written down nowhere, so nothing pointed at the assertion that had just gone stale.

**Bind the specs to the code they depend on with something that fails.** A comment naming them rots and is easy to read past.
What works is a gate on the code path itself, so the change that could invalidate a spec is the change that trips it:

- [`deletion_inventory_test.go`](../../cmd/agc/internal/provisioner/deletion_inventory_test.go) inventories every client `Delete` call in the `agc` module.
  Adding, moving, or renaming one fails the test, and the failure prints every spec that pins the deletion boundary, flagging with `[CRED]` the ones that skip without credentials and are therefore not evidence of anything when CI is green.
- A second case asserts each named spec still exists at its recorded path, so the roster cannot decay into names nothing answers to.
- A third sweeps the heavy-tier test trees for files that read the deletion mark and requires each to be accounted for: either it contributes a spec to the roster, or it is written off with a reason. **A roster guarded only against rot is still only as complete as whoever last read the design docs**, a gap found by retro after the first two cases had shipped.
  It classifies per file, not per spec, so a brand-new boundary test file is caught and a new spec inside a roster file is not; the roster's own entry is what points a reader into that file.
  Unit tests stay out of scope on purpose: they run on every `make check`, so they go red on their own and need no pointer.

It generalises.
When a credential-gated spec is the only thing asserting an invariant, find the code change that could break it and make *that* the tripwire.
The gate cannot run the spec; it can make sure nobody changes the code without being handed the spec to read.

### Verify a causation claim by deleting the mechanism

The rules above are read-and-reason checks — ask what else could produce this green.
This one is mechanical, and it is the only way to *settle* the question: **delete the mechanism, re-run the test, and require it to go red for the reason you expect.** Restore it, confirm green.
A test that stays green without the code it claims to exercise is measuring something else, and no amount of reading the test will tell you that.

Reach for it whenever a test's subject is "this code caused that outcome" — a new regression test, a fix whose test you wrote alongside it, a spec whose green arrived on the first try.
It costs one edit and one run.

- **Delete the mechanism, not the assertion.** Comment out the call, drop the retry loop, return early from the reconcile branch.
  Removing the *assertion* proves nothing.
- **Delete one mechanism, not the branch around it.** A deletion coarse enough to redden every assertion has measured only that the code path is reached.
  Q624 first stubbed a whole word-classification branch to `true` and took 22 of 22 red, which discriminated nothing; flipping the single condition that made command position load-bearing took exactly one assertion red, which is the answer.
- **Read the failure, not just the colour.** Red for the wrong reason (a compile error, a fixture that no longer builds, an unrelated timeout) is not evidence.
  The test must fail on the assertion that names the behaviour.
- **A green after the deletion can mean the assertion cannot see the defect class**, not that the mechanism is unnecessary, and the two look identical.
  Ask what an assertion actually observes before believing it: one that compares a *set* of keys is blind to ordering, spacing, formatting and everything else the surrounding bytes carry.
  Q799 deleted the mechanism that holds a roadmap bullet's blank lines outside the merged record, a defect already reproduced by hand, and the suite stayed green, because every assertion compared the surviving bullet IDs and none compared the rendered page.
  Two byte-exact whole-file comparisons made the same deletion red.
  The fix is generally to assert on the artifact the defect damages, not on a projection of it.
- **Round-trip it.** Restore the mechanism and confirm green in the same sitting, so the check cannot end with a half-reverted tree.
- **Assert that the deletion applied**, when a script drives it rather than your hands.
  A mutation that matched nothing leaves the mechanism intact, so the test passes and reads as "this assertion does not bite", which is the exact conclusion the check exists to rule out, reached backwards.
  Diff the file against its backup and fail the check when they match.
  Q690 mutated by `perl -i -pe 's{\Q…\E}{…}'` and three of four patterns silently matched nothing: `\Q` quotes regex metacharacters but does **not** stop interpolation, so a pattern containing `$json` or `$baseline` had those spliced out as undefined Perl variables before matching.
  Anchor a scripted mutation on a line number, or escape every `$`; either way the did-it-apply assertion is what tells you which happened.

**A green after deletion can mean a redundant guard is standing in, not that the mechanism is dead.** Q624's suite passed in full with command position deleted, because an `already_throttled` short-circuit upstream rescued every case that would have exercised it.
The mechanism was load-bearing in production and unasserted by the suite — so the answer is a case pitched into the gap the redundant guard does not cover (there, a wrapper the short-circuit does not recognise), not a conclusion that the code is unnecessary.
Ask which *other* code is keeping the test green before deleting anything else.

Q506 needed two rounds of this before a spec's green meant what it claimed.
Q551's re-offer test is the routine case: disabling the one call the fix added turned it red on "the re-offer must provision the job once the conflict clears" — exactly the sentence the test exists to assert — and it went green again on restore.

**A threshold assertion needs a fixture sized to the threshold.** The common way to fail the check above is a fixture so far from the boundary that it passes either way.
A test for "badge markup is not counted against the 45-word cap" was first written with a ten-word bullet: green with the stripping, green without it, and pinning nothing.
Sized to 38 body words — 45 counted with the spans stripped, 47 without — deleting the `gsub` turned it red, which is what made it a test.
Whenever the behaviour under test is a limit, a cap, a timeout, or a retry count, put the fixture *at* the edge and confirm one step past it fails.

**The mirror, for a gate: inject the defect it is supposed to catch.** "This gate would catch that" is the same causation claim pointed at a green, and reading the matcher only *predicts* the answer — a regex is exactly the kind of thing that looks like it covers a case it does not.
Injecting measures it: break one real input the way the gate should notice, require red, restore.
Pair the injection with a **control that must fail**, for the [reason a throwaway probe needs one](#a-throwaway-probe-needs-a-positive-control-before-its-silence-means-anything) — an injection that produces no red proves the gate blind only if a near-identical one does produce red; otherwise you have measured your own harness.

Q612's blind spot was settled that way in a single pass.
Repointing the README's live license badge at `THIS-FILE-DOES-NOT-EXIST.md` left `check-doc-links` green — `ok (242 files, 5134 links checked)`, exit 0 — while the identical target written as a plain link failed it on the same file.
That is the whole finding: the collection regex matches the inner image first, so a badge-wrapped link's outer destination is never checked, and a total of 5,134 checked links says nothing about the shape the gate cannot see.

### A measurement that reproduces a call is not a test of the code that makes it

The sibling of the rule above, and the one that survives a green **positive**.
A measurement that issues an API call from the harness — `gh api`, `curl`, a scratch client — establishes what the *remote* does with that request.
It establishes nothing about the code path that will make the request in production, and the write-up is where that distinction usually gets lost.

**Q459 is the worked example.** Its plan recorded the re-runnability step as "the exact call the AGC would make … so its answer is the answer," and the measurement returned `201 Created` on five separate live-GitHub runs.
Semantically it *was* the same call.
Operationally it differed from the AGC's in two ways, each of which was a shipped bug a green `201` could not show:

- **Where it went.** The harness used `gh api`, which addresses whatever host the ambient config names.
  The AGC read `Provisioner.GitHubAPIURL` — a field `cmd/agc` never assigned — so its call always went to `api.github.com` whatever `GITHUB_API_BASE_URL` said, and recovery could not work on GHES at all (Q504).
- **When it fired.** The harness POSTed *after* waiting for GitHub to conclude the job.
  The AGC fired `evictionRetryDelay` (5s) after the disruption, ~9.5 minutes before GitHub concludes, and was answered `403 This workflow is already running` (Q503, since fixed by retrying that refusal until the run concludes).

Neither is a flaw in the measurement — "GitHub accepts this call for this run state" was a real question and the answer was real.
The flaw was in the sentence that carried it forward, which let a fact about GitHub read as a fact about us.

So when a harness call stands in for a product code path, **write down what it did not exercise**: the client that will really make it, the configuration that will really route it, and the moment it will really fire.
Then say which of those a follow-up must still confirm.
A measurement whose write-up names its own blind spots is worth far more than one that quietly implies it has none.

### Assert the recovery property, not the mechanism believed to deliver it

A test that exists to pin a safety or recovery property — "the gate cannot starve a tenant", "the queue drains eventually", "the retry budget is bounded" — should assert that property as an observable outcome.
Asserting instead the internal transition *believed* to produce it does something worse than under-testing: if the belief is wrong, the test actively defends the defect, and its docstring argues the defect is the safety feature.

**Q512 is the worked example.** `TestCapacityGate_ClearsWhenThePodIsReaped` asserted that reaping a stuck worker pod flips `WorkerCapacityDeclined` back to `False`, with a docstring stating that without this clearing "the first close would be permanent and the gate would starve a tenant."
The real property was *intake resumes at a bounded rate*; the clearing was one mechanism assumed to deliver it — and on the scale-set tier it did not, because clearing restored the *entire* advertisement and the next batch was claimed whole.
The suite was green and confidently documented while a dogfood measurement (capacity plan §9e) found the gate removed zero wasted claims.
The fix inverted the pinned behavior (the condition now latches), and the test's docstring had to be rewritten from "clearing prevents starvation" to its opposite.

When writing a test for a property of this kind:

- **Name the property in the test, then ask whether the assertion measures it or a proxy for it.** "The condition clears" is a proxy; "one job is admitted per deadline window and no more" is the property.
  If only the proxy is practical at this tier, say so in the docstring — a stated proxy invites re-examination; an argued-from proxy forbids it.
- **Docstrings that justify an assertion with a consequence ("without this, X would happen") are claims, and claims want evidence.** If X has never been observed — only reasoned about — mark it as design intent rather than measured fact. §9e's measurement contradicted exactly such a sentence.
- **When a live measurement falsifies a test's premise, the test is a casualty, not a defense.** Expect the fix to rewrite it, and treat "but the existing test asserts the opposite" as the finding it is.

This is the negative-assertion rule's sibling one level up: there, a green test couldn't distinguish absence from misdirection; here, a green test pinned the wrong thing on purpose, because the mechanism and the property had been conflated at design time.

### Generate a fixture with the producer's own code, never by hand

A hand-written fixture encodes what its author believed the producer emits.
When that belief is wrong the fixture is wrong in exactly the same way as the parser written beside it, so the test passes and the pair is wrong together — the same closed loop as [adjusting a fake to make a test pass](#adjusting-a-fake-to-make-a-test-pass-is-a-finding-about-the-real-interface), one layer earlier.
Prefer, in order: a capture committed under `testdata/`, output generated by calling the real producer, then hand-authored.

Q608 is the case.
A JUnit parser needed a fixture with a failing spec, and no real report had one.
Generating it by calling Ginkgo's own `reporters.GenerateJUnitReport` — a throwaway program, deleted after — showed that Go's `encoding/xml` escapes a quote as the numeric `&#34;`, not the named `&quot;`.
The hand-written fixture would have used the named form, the parser handled only the named form, and the two would have agreed while every spec name containing a quote reached the job summary with raw entities in it.
The generator cost about five minutes; the bug was invisible to every other gate.

The tell that you are in this situation: you are about to write, from memory, a fixture in a format some *other* program defines.
Escaping, quoting, numeric precision, field order, and null-vs-absent are where memory is unreliable, and all five are silent.

### Adjusting a fake to make a test pass is a finding about the real interface

When a test fails because the stub does not send some field, there are two repairs, and they look identical from the test's point of view: teach the stub to send it, or find out what the real service sends.
The first is one line and always works.
Reach for it without asking the second question and the suite stops describing the system and starts describing itself — every test agrees with the fake, the fake agrees with the code, and nothing in the loop has consulted the wire.

Q495 is that failure, and it is visible in this page's own example above.
The drain specs pin "a payload carrying a complete run identity" as an observability guard — correctly, in principle.
But that identity was pinned as `system.github.*` job variables, a shape a real `acquirejob` response has never carried, because the AGC read those variables and the fakes were taught to supply them.
Q421's session recorded the symptom verbatim — "the default fakegithub response carries no run identity, and `handleEviction` returns early without one" — and injected the identity to get its spec green.
That sentence was the whole defect, written down and read as a test-setup detail.
Classic-tier eviction recovery could not fire against real GitHub for as long as the parsing stood, and every tier of the suite was green throughout.

So when you find yourself adding a field to a stub so a test will pass:

- **Ask what the real service sends**, and get the answer from a capture or a live call, not from the code under test — the code is what you are trying to check.
- **Prefer the real shape in the fake.** A stub that emits what GitHub emits makes the whole suite above it load-bearing; one shaped to match the parser makes it a mirror.
- **If the fake genuinely has to differ, say why in the fake**, so the next person meets a documented divergence rather than an assumption.

**The omission case is worse than the wrong-shape case, because no test fails to announce it.** Above, a stub sends the wrong field and something goes red first.
When a stub does not model a piece of state *at all*, every tier is green over a whole bug class and nothing ever points at the gap.
Q550: the scale-set stub's `generatejitconfig` returned a JIT config without recording that it had registered a runner — which is the one durable effect that call has at GitHub.
So a leaked registration was not merely untested, it was **unrepresentable**: no unit, integration, or e2e assertion could have named the state the bug was about, and the suite stayed green through a defect that wedged a release gate.

The tell is in the bug report.
When a defect filed from production describes state your suite cannot express — "22 stale runner records", a row in a table no fake has, a resource that outlives the object that made it — the fake is the first thing to fix, before the code:

- **Teach the double the real effect, then watch the new test fail.** A regression test written against a fake that cannot hold the state is a test that can never fail, which is worse than no test because it reads as coverage.
- **Model the effect, not just the response.** `generatejitconfig` returning a blob is the response; creating a record that holds a name until something deletes it is the effect, and the effect is where this class of bug lives.
- **Expect the faithful fake to reshape the diagnosis.** Q550's Queue row named a mechanism that turned out to be already fixed; only a stub that registered on mint could show which of the two candidate mechanisms actually accumulated records.

### Synchronize on the signal you assert on

"[Wait on the condition, not the clock](#avoiding-shared-stub-flakes-in-the-agc-suite)" has a sharper form: wait on **the same signal you are about to assert on**.
Two observable effects of one operation almost never land together, and a test that blocks on the earlier one and then reads the later one races the gap between them — a real race, not a slow machine, so no amount of extra timeout closes it.

A stub is the usual trap, because it is the most convenient thing to wait on and it typically fires *first*.
In Q350 the scale-set listener's `recordingProvisioner` recorded a job at `Provision` **entry**, while the listener counted `IncJobProvisioned` only after `Provision` **returned**; three tests blocked on the stub's count and then asserted the metric, which was still one behind whenever the poll loop was descheduled in between.
Waiting on the metric fixed all three — and is safe in the other direction, since the metric strictly follows the stub, so the stub-side assertions still hold.

Reach for the ordering deliberately: identify which effect the code emits **last**, synchronize on that, and let the earlier ones be implied.

The gap can also sit **inside a single stub handler**, which is harder to see because one HTTP call looks atomic from the test. `brokertest`'s `handleCompleteJob` incremented `CompleteJobCalls` on entry and committed the fan-out accounting several statements later; `TestAGC_Q260_FanoutCompletionReconciles` waited for the count to reach N and then fired `ExpireUnstartedDeliveries`, which could beat the Nth handler to the accounting lock and cancel a job whose deliveries had all in fact completed (Q490).
Two rules come out of it:

- **Writing a stub: publish the observable counter last**, after every piece of state the handler records, so waiting on the counter is a valid happens-before gate for everything it wrote.
  A counter incremented on entry gates nothing.
- **Writing the test: wait on the state, not the call count.** A count says a call was *served*, not that it *resolved* anything — a request whose body never arrives (cancelled client context) is still served and still counted.
  Q490's test now waits on `DeliveryResults`, the accounting it actually asserts about.

**Publishing the counter last is not enough when the effect you assert on is deliberately deferred to a later cycle.** The scale-set listener honours the rule — `abandonDeferredBefore` calls `settle` before it increments the abandoned counter — but `settle` only marks the job concluded *in memory*, and the wire delete that actually releases the message is issued by the next `flushDeletes` cycle, by design ("runs per cycle rather than only at settle time"). `TestListener_AbandonedJobDoesNotSurviveARestart` waited on the counter and then stopped the listener, so a stop landing before that cycle left the message unreleased and the restart replayed it — the very thing the test asserts must not happen (Q602).
When the code splits an operation across cycles on purpose, the counter marks the *decision*, not the *effect*: wait on the effect, here the stub's own `delete-message` call.

**And the signal can come from a component that is not the system under test at all.** Q602's counter was at least the listener's own. `TestListener_RestartDoesNotReprovisionAConcludedJob` waited on `AssignedJobCount == 0`, the *stub's* record that the job ended, before taking the listener away.
But `recordingProvisioner.provision` is what calls `CompleteAssignedJob`, so that count drops while the listener is still inside `handleMessage` for the assignment, and the `JobCompleted` it appends is a queue message the listener only reads a poll later.
The wait was satisfied by the test's own provisioner, a round trip before the listener knew anything, and a stop landing in that gap left an assignment the listener still believed was owed a worker, which the restart duly re-provisioned (Q685).
Correlated over 60 runs taken at maximum stop pressure: of the 4 that stopped before any delete, every one replayed; of the 56 that issued it, none did.
So ask **which component has to observe the state** for the assertion to mean what it claims, and wait on something that component emits, here `deleteAttempts > 0` again.
(Those 60 runs measured a production defect as well as a test one: the window the test was landing in was real, and closing it is Q689.
The wait still belongs here, because a test that races the bug it asserts against is measuring the bug rather than the invariant.)

To prove a window like this rather than guess at it, widen it: drop a `time.Sleep` between the two effects and confirm the test fails every time.
Q490's probe took the flake from unreproducible in 200 local runs to 5 failures out of 5, and re-running it against the fix — still 8/8 green with the sleep in place — is the [negative control](#proving-a-flake-fix-invert-it) that shows the ordering, not luck, is what closed it.

#### The two effects can belong to two different participants

The gap need not be two effects of one operation.
It can be one effect each from two *participants*, where the test waits on the one that is cheap to reach and asserts on the one that is not.
Q600's `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` waited for the duplicate-delivery metric to reach `maxListeners-1` and then read the peak provisioner count.
Only the **losing** siblings increment that metric, and a loser's work ends at the claim gate; the **winner** still has the claim registry, `SpawnReplacement`, and `StartRenewLoop` to clear before it enters the handler the count comes from.
The losers can therefore satisfy the wait entirely on their own progress, and the assertion reads `0` — which is exactly the shape CI reported.

So when picking the signal, ask **who produces it**, not just when.
A counter fed by the actors you are *not* asserting about gates nothing about the one you are.
The fix is the same as everywhere else in this section: wait on `handlerMax >= 1`, the counter the invariant itself reads.

#### The two effects can be one object read through two clients

There need not be a stub involved at all.
A controller-runtime manager serves reads from its informer cache while an envtest suite's `k8sClient` reads the apiserver directly, so one status condition has **two observers that never update together**: the apiserver first, the cache a watch-delivery later.
A test that waits on `k8sClient` and then does something the *controller* must judge has synchronized on the earlier of the two.

Q559 is the worked example. `TestV2_RunnerSet_CapacityGate_FixedClusterSkipsAcquire` waited for `WorkerCapacityDeclined` through `k8sClient`, then enqueued one job and asserted it was rejected with `reason=capacity`.
The admission rung reads that same condition back through `mgr.GetClient()`'s cache (`runnerSetTarget.capacityGateCondition`), so a delivery landing inside the gap is admitted and **acquired** — the CI failure's own log carries the `AcquireJob` line the passing run does not.
The classic tier delivers a job once, so nothing redelivers and no larger timeout could have helped.

The gap measured **~1 ms** on an idle dev machine.
It reads as ~10 µs if you sample it right after a wait that polls every 100 ms — the poll interval, not the gap, is what hides it.
That width is too narrow to lose the race on an idle machine, and the flake was never reproduced at native timing; what the measurement establishes is that the window exists and that the old wait did not cover it.
Removing the barrier entirely reproduced the CI symptom exactly, which is what tied the two together.

Two rules follow:

- **Wait on the client the code under test reads**, not the one that is convenient.
  Where an object has both, the cached one is the later observer.
- **A one-shot stimulus needs the stronger barrier.** The same file's scale-set test may wait on `k8sClient`: it asserts a per-poll advertisement that re-reads on every poll, so a stale read self-corrects.
  A single delivered job has no such recovery — which is why `waitForCapacityDeclined` takes its reader as an explicit parameter, making the choice visible at each call site.

### Pin the process when the signal comes out of its memory

Synchronizing on the right signal is not enough when the controller produces that signal from state it holds **in memory**, downstream of something durable it has already written.
The outcome then depends on one process surviving a window, and a process replaced inside that window forecloses the outcome permanently — so the wait expires on a decision already made, no timeout reaches it, and the failure reads as exactly the defect the test was written to catch.

Q549 is the worked case. `RecoverEvictedScaleSetWorkers` claims a disrupted scale-set worker by stamping `actions-gateway.com/eviction-handled-at` on the pod, and only then hands off to `handleEviction`, which waits out `evictionRetryDelay` on a goroutine before calling GitHub.
The claim is durable and at-most-once — every later scan skips an annotated pod, deliberately, because the deletion arm's evidence *is* the pod and the deletion removes it.
The re-run is neither durable nor replayable.
On run 30658951388 the GMC rolled the tenant's AGC Deployment mid-window: the outgoing pod won the claim at 20:00:19Z, the incoming pod's only claim attempt lost the optimistic lock a second later and skipped, and the e2e re-run wait spent its 90 s on an outcome already decided.
What rules out the alternative reading — a re-run attempted and refused — is the absence of any `EvictionRerunFailed` event, which `rerunUntilAccepted` records on every terminal failure.

So: **name the process the assertion depends on, pin its identity before the window, and re-read that pin before believing a negative.** `agcPodIdentity` (the AGC Deployment's sorted pod UIDs) is the pin the scale-set recovery spec uses.
An unchanged pin turns "no re-run" into a real failure worth reporting; a changed one says the attempt measured nothing, and the spec re-stages rather than asserting on it.

### Script tests: neutralize the clock, never measure it

The rules above are written for the Go tiers, but the `scripts/` tier needs its own statement of them, because it is the **most load-contended tier in the repo** and the one most likely to be written with a real-clock assertion. `make scripts-test` runs every `scripts/` suite concurrently through [`scripts/ci/run-parallel.sh`](../../scripts/ci/run-parallel.sh) (`make list-script-tests` names them), inside a `make check` that is already saturating the machine with the Go tests.
A bound on real elapsed seconds is least reliable exactly where it is cheapest to write.

**So a script test must never assert on wall-clock time it actually spent.** Stub `sleep` — it is a plain command, so a shell function shadows it — and assert on what the stub recorded.
Two established shapes, both in-tree:

- **Count the sleeps** when the property is "the loop paced itself rather than spinning".
  [`scripts/dogfood/workers-test.sh`](../../scripts/dogfood/workers-test.sh) does this: `sleep() { echo x >>"${STUB_DIR}/sleeps"; }`, and the assertions read the count.
  No notion of elapsed time is needed at all.
- **Shadow the clock as well** when the assertion is genuinely about a *budget* — "this retry stopped inside its timeout".
  Have the stubbed `sleep` advance a counter and have the script read that counter instead of the real clock.
  Elapsed time becomes the script's own accounting of the budget it spent: independent of load, and **exact**, so the bound can be the real budget rather than a slack value padded for jitter.
  [`scripts/e2e/validate-cluster-test.sh`](../../scripts/e2e/validate-cluster-test.sh) does this against [`validate-cluster.sh`](../../scripts/e2e/validate-cluster.sh)'s `retry_until`.

The second shape needs a seam in the script under test: **read the clock through a named function** (`now_seconds`), never an inline `date +%s`, so a test can substitute it.
This is the same constraint as the `BASH_SOURCE` guard and the "keep new parsing in a named function" rule above — an inline call in the middle of a code path is untestable by construction.

Q471 is the worked example, and it shows the cost of getting this wrong is not just a flake: `validate-cluster-test.sh` bounded real seconds (`date +%s`, max 1–5 s) around sleep-based retries, so it passed standalone and failed under a loaded `make check` — a flake that only ever fires where it is hardest to reproduce.
Converting it to a fake clock also took the suite from ~4 s of real sleeping to under 0.5 s, because a test that stubs `sleep` does not wait for anything.

#### The clock is as often a deadline around a process as a `sleep` inside one

The margin can look absurd and still be a race.
[`progress-watch-test.sh`](../../scripts/e2e/progress-watch-test.sh) asserted that `TEST_PROGRESS_INTERVAL=0` turns the watcher off by launching it, arming a 10 s `kill -9`, and reading `wait`'s status.
The watcher exits in milliseconds, so the deadline had a ~2000× margin on an idle machine — and none at all on a busy one, because what it bounds is the scheduler, not the guard.
Measured over 1,760 launches while four concurrent 41-way `make scripts-test` runs saturated an 18-core machine: p50 28 ms, p90 172 ms, p99 469 ms, and **8 launches (0.45%) still alive at the deadline**, each in state `S` with the shell not yet through startup (Q642).
The suite reports that as `exit 137` — its own SIGKILL, indistinguishable from the hang the deadline exists to catch.

So **bound the subject's loop, not its wall clock**.
A stub `sleep` ahead of the real one on `PATH` records the tick and kills the process; the assertion reads the recorded ticks.
"Off" becomes zero ticks, a regression fails on the tick it took instead of spinning for the length of the gate, and both outcomes terminate through the program's own control flow — so neither end needs a deadline.
The kill still surfaces as a non-zero exit, which keeps a genuine hang from passing as a clean one.

Prove it by widening the window, exactly as [Q490](#synchronize-on-the-signal-you-assert-on) does: a `/bin/sleep 12` injected into `main` reproduces the reported `exit 137` on the old assertion every time, and the rewritten one passes against that same delay. `/bin/sleep`, not `sleep`, so the injected delay bypasses the stub on `PATH`.

#### Two clocks in one assertion

A test that waits on one clock and asserts about another has a bug that no amount of margin closes, because the two can move apart by any amount.
[`release-sentinel-test.sh`](../../scripts/dogfood/release-sentinel-test.sh) held the watcher to a 60 s budget (`RELEASE_SENTINEL_TIMEOUT`) and then observed it through a 3 s window (`sleep 3`, then `kill -0`).
The budget is spent against `date +%s`, which is wall clock and can step; the window is spent against `sleep`, which is a timer and cannot.
Any forward step of the wall clock between the two retires the whole budget inside the window, and the watcher reports `timeout` before the test ever looks (Q690).

The 20× margin is what makes this hard to read as a race, so **the margin is the tell, not the reassurance**: a budget that cannot plausibly elapse, elapsing anyway, means the assertion is not measuring the quantity it names.

Contention alone does not explain this one, and it is worth separating the two causes.
Measured 2026-08-05 on an 18-core machine: 240 suite runs under concurrent full `make scripts-test` fan-outs (56 suites each) and repeated `make check` (load average 31 to 59) produced no failure; 102 timed windows stretched to 4 s at worst, never past 5; and 4,000 `date +%s` calls under the same load returned a sane value every time.
A clock that runs ahead of the timer reproduces the reported failure verbatim, down to the assertion name and the `EVENT: timeout` payload.
So Q642 above and Q690 are one class (an assertion bounded by real seconds around a subprocess) reached by two different triggers: scheduler delay there, clock discontinuity here.
Q596's `tree-in-sync` is *not* a third instance of it, whatever the symptom line suggests.
Its measured mechanism was a subprocess whose exit status was unobservable through a process substitution, which left one side of a diff empty and read as drift ([the v2 API sync gate](#the-v2-api-sync-gate)).
An unchecked status, not a clock.
Q703 joins Q596 in that class rather than this one: `release-delta-test` built its fixture as `repo="$(build_repo)"`, where `set -e` does not reach, so the builder ran on past a broken repository and the suite reported the subject under test as the thing that failed ([`set -e` stops at a command substitution](bash-style.md#set--e-stops-at-a-command-substitution)).
Process substitution hid the status there, command substitution here.

The fix is to leave the suite with **one clock, and let the subject drive it**.
The watcher reads time through `date` and paces itself through `sleep`, both `PATH` commands, so stubbing the pair gives a virtual clock that advances by exactly the interval the subject asked to sleep and at no other time:

- **"Still asleep" becomes a count of the subject's own polls**, not a span of seconds.
  The stub caps the loop and kills at the cap, so both outcomes terminate through the subject's control flow and neither end needs a deadline.
- **A transition lands on a poll.** The stub runs a per-case hook, so the event is appended strictly after the watcher captured its baseline and strictly before its next read.
  That is synchronization, where the `sleep 3` it replaced was a guess.
- **The budget becomes exact.** Two polls of a 2 s budget retire it, every run, on any machine.

**A frozen clock needs a positive control, or the freeze hides the assertions it was meant to protect.** With time stopped, every "stays asleep" case passes whatever the subject does, including doing nothing.
One case therefore has to spend the budget and reach the timeout; it is what proves the stub is wired to the thing the other cases depend on.
The same reasoning applies to the stubs themselves: each new assertion here was checked by deleting the mechanism it names from `release-sentinel.sh` and requiring it to go red, and the mutations were first written as text patterns that `perl` silently emptied by interpolating `$json`, so the harness also asserts that the mutation changed the file at all.

Removing the real seconds took the suite from 11.4 s to 3.6 s, which is the same secondary effect Q471 reported: a test that stubs `sleep` does not wait for anything.

#### A backstop shorter than the subject's own budget is not a backstop

Sometimes the wait genuinely is on the signal and only the fallback is a guess.
That fallback is still an assertion, and the question it has to answer is: **if this fires, has the subject done anything wrong?** When the number is smaller than what the subject is configured to spend, the answer is no, and the test reports a verdict it has no grounds for.

`cmd/probe/abandoned_test.go` waited on the verdict channel, which is the right signal, behind a flat `time.After(20 * time.Second)`, at ten sites.
Seven of them ran the probe with `PROBE_ABANDONED_WINDOW=30s`, and the probe's `observe()` loop is entitled to run to `t0 + window` before returning `NO-SIGNAL`.
So the assertion expired ten seconds *inside* the window the same test had just configured, and `TestAbandonedProbe_Redispatched` duly failed on #1357 at 20.24 s under `-race`, green on rerun (Q761).
Adding the delivery timeout and the bounded tails, the longest legitimate path is about 43 s: the bound was never generous, it was simply usually lucky.

**Derive the backstop from the subject's own waits** rather than picking a round number beside them, and bind the two to one variable so they cannot drift:

```go
func verdictBudget(window time.Duration) time.Duration {
	return 3 * (abandonedTestTimeout + window + abandonedTestJobTail + abandonedTestRerunWait)
}
```

The multiplier is the only guess left, and it now covers scheduler delay alone, because every real wait is already in the sum.
Say so in the failure text: "3x the 43.3s its own configuration can spend … so it is wedged, not slow" tells the next reader which of the two they are looking at, where "did not reach a verdict" told them nothing.

The same reasoning retires an iteration cap. `record-launch-test.sh` waited `100 × sleep 0.1` for a launch record and `alloc-queue-id-test.sh` `1000 × sleep 0.01` for its eight-worker fleet, both failing with "within 10s" (Q752, Q706).
Ten seconds is inside the measured startup distribution, not outside it: Q642 put shell startup at p99 469 ms with **0.45% still starting at 10 s**, so across a fleet roughly one run in 28 met the cap, and the suite blamed the subject for the scheduler.
Where a blocking primitive exists, use it: the record-launch suite `wait`s on the wrapper, which waits on the run, so the run's own exit ends the wait with no clock in it at all.
Where none does, keep polling the signal, add the *other* real outcome as its own exit (the wrapper dying without writing a record is a failure you can observe rather than infer), and push the cap far enough out that reaching it means broken rather than busy.

#### A throwaway load harness is a measuring instrument, so calibrate it

Reproducing a load-sensitive flake means writing a scratch harness that generates load and samples the suite.
It is throwaway code, which is exactly why it gets written without the checks the suite itself would get, and every failure mode below produces a **confident, wrong verdict**:

- **The load never started.** Q690's first harness backgrounded its generators with `setsid`, which does not exist on macOS.
  All three generators died instantly, 40 samples passed against an idle machine, and the run looked like strong evidence of no flake.
  A load harness must **assert its own load is running** before it draws any conclusion from a green sample, and print the figure it measured, exactly as [an empty search result is only evidence once the command is known to have run](#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query).
- **The harness killed the thing it was measuring.** That same harness cleaned up with `pkill -f 'make scripts-test'`, which matched the `make check` running for verification in the same worktree.
  The gate died mid-run and reported a non-zero exit, and a sampled suite that had printed `all assertions passed` was recorded as a failure because `wait` returned non-zero.
  Both read as genuine red. **Scope a harness's cleanup to its own processes**, with a stop file the loops poll or a marker in the command line that the pattern anchors on, and never `pkill` a pattern that a real gate's own command line matches.

- **The harness's own verdict was wrong.** Both failure modes above are about the load; this one is about the oracle, and it is the worse kind because the samples are real and only the classification is broken.
  Q703's race probe reported 50 failures in 50 attempts, which read as a spectacular reproduction until the log showed every one was the probe passing `-exist-file /dev/null` to the checker, so every link in the file was dead.
  The rule is the same one that applies to any probe about to justify a decision: **include a case whose answer you already know, and disbelieve the harness when they disagree.** A 100% failure rate is as suspect as a 0% one.

- **The subject changed under the harness.** The first three are about the load, the oracle, and the cleanup; this one is about the thing being measured, and it is the easiest to walk into because the harness runs for minutes while you keep working.
  Q703's loop reported a hit on run 8 that was its own session's in-flight edit: a `trace()` call had been temporarily made unconditional to check that an assertion went red without it, and the loop caught that tree.
  It reads as a reproduction, and it is the flake's own signature, so nothing about the log says otherwise. **Freeze the tree for a load run's duration, and discard every round that overlapped an edit to the subject**, including doc and comment edits, since the gates read those too.
  Land the code first, then measure; the doc and backlog work that fills the wait is exactly the work a running gate does not decide ([run the local gate in the background](parallel-dispatch.md#run-the-local-gate-in-the-background-not-on-the-critical-path)), so do that against a tree the harness is not sampling.

The general rule is the one this whole section is about, turned on the instrument: a measurement you cannot show ran is not a measurement.
Reconcile the harness's status against its output before believing either, and reconcile its verdict against a case you can check by hand.

### Testing a `main`-shaped script: the entry-point seam, and the errexit trap

A script whose logic lives in `main()` needs a seam before it can be sourced at all.
The dogfood scripts use an env guard on the last line — `[[ -n "${START_LIB_ONLY:-}" ]] || main "$@"` in [`start.sh`](../../scripts/dogfood/start.sh), and the same shape in [`e2e-start.sh`](../../scripts/dogfood/e2e-start.sh), [`e2e-stop.sh`](../../scripts/dogfood/e2e-stop.sh), [`delete.sh`](../../scripts/dogfood/delete.sh) and [`validate-release.sh`](../../scripts/dogfood/validate-release.sh) — so a test sources the file for its functions without running it.
It is the `BASH_SOURCE`-guard idea applied to an entry point; **keep the guard when editing these scripts**, or their suites stop being able to load them.

The guard earns most on a script that is destructive at rest.
[`delete.sh`](../../scripts/dogfood/delete.sh) tears down the whole dogfood cluster, so its suite must never be one stray invocation away from running it; sourcing under `DELETE_LIB_ONLY=1` with `gcloud`/`kubectl`/`gh` stubbed is what makes the confirmation, the occupancy probes it quotes, and the delete ordering assertable at all.
Teardown is the half worth asserting hardest: a drain that reports convergence it did not reach deletes an AGC out from under live work, and worker pods outlive their AGC with do-not-disrupt annotations pinning billable nodes (Q581, and the 82 stranded spot node-hours behind Q434).

**Then: never call `main` as an operand of `||` or `if`.** Bash suppresses `errexit` inside a subshell in a condition context, and re-running `set -e` inside that subshell does *not* re-arm it — measured on bash 5.3:

```bash
f() { false; echo REACHED; }
rc=0; ( set -e; f ) || rc=$?   # rc=0 — the subshell sailed past the failure
set +e; ( set -e; f ); rc=$?; set -e   # rc=1 — errexit actually armed
```

This matters because the interesting assertion about a bring-up script is usually that a failure *aborts* it: that a failed readiness wait stops the run before it flips routing at a tenant that never came up.
Written the first way, that test passes no matter what the script does — a false green in the same family as Q404 and Q432.
Use the second form, and mutation-check it: break the abort (append `|| true` to the wait), confirm the assertion goes red, restore.

### A dogfood suite scopes the release progress stream before it sources anything

[`lib/progress.sh`](../../scripts/dogfood/lib/progress.sh) defaults `RELEASE_PROGRESS_FILE` and `RELEASE_STATUS_FILE` to repo-local paths that a live release-validation gate is actively writing.
Any suite that sources a script reaching that library inherits the operator's own stream unless it says otherwise, and sourcing is the whole trigger: `e2e-run-watch.sh` pulls the library in, and `watch_run` records the run it parked on so the sentinel can tell a quiet leg from a wedge (Q630).
So `e2e-run-watch-test.sh` appended three `owner/repo` run events to the real stream during the v1.4.0-rc.2 gate, repointing that stall check at a run that does not exist (Q777).
The gate still passed, which is the part worth noticing: the damage was to the artifact a human was reading to decide whether the candidate was good.

Set `RELEASE_PROGRESS_FILE` to a path under the suite's own `$WORK` **before** the `source` line, as [`release-status-test.sh`](../../scripts/dogfood/release-status-test.sh) and [`release-sentinel-test.sh`](../../scripts/dogfood/release-sentinel-test.sh) do.
The library defaults on *unset*, so an assignment after the source line is too late for anything that line already ran. `RELEASE_STATUS_FILE` defaults beside the stream rather than to a fixed path, so that one assignment scopes both, and `RELEASE_PROGRESS_FILE=` leaves no live path reachable at all for a suite that wants no stream (Q786).
Set `RELEASE_STATUS_FILE` as well when the suite wants it somewhere other than the stream's own directory; both suites above name it explicitly, which is also the clearer thing to read.

Then assert the positive, and name the path rather than the variable.
An assertion that reads `"$RELEASE_PROGRESS_FILE"` passes exactly as well once the scoping is gone and the events went to the live stream, so it guards nothing; reading `"${WORK}/progress.jsonl"` goes red.
Because `progress_run` and `progress_heartbeat` append only to a stream that already exists, a suite that wants that assertion has to call `progress_init` first.

### A path assembled at runtime is only checked by running the script

shellcheck resolves nothing: `"$(dirname "$0")/../fetch/download-verified.sh"` is a string to it, correct or not.
So a suite that asserts only the *pure* half of a script — a regexp, a parser, a mapping — leaves the script itself unexecuted, and its runtime paths uncovered by every gate.
Q605 is the case: `verify-release-test.sh` asserted the signing-identity regexp and nothing else, so when Q571 moved `download-verified.sh` into `scripts/fetch/`, `download-cosign.sh` kept pointing at its old directory and `make verify-release` died at `No such file or directory`.
Nothing in `make check` or CI noticed; the v1.3.0-rc.4 cut did, and only because a release happened to follow the refactor closely — the same break sitting between two releases would have gone unseen for as long as that gap.

**So execute the script, even when its real work needs the network.** Stub the one command that reaches out (`curl`, `cosign`, `gcloud`) on `PATH` and assert on a message only the far side of the path can emit.
[`download-cosign-test.sh`](../../scripts/release/download-cosign-test.sh) serves a fixture from a stub `curl` and asserts the run reaches `download-verified.sh`'s **digest-mismatch** error — the download cannot succeed (a pinned binary has no preimage to serve), but reaching a failure that only the helper reports proves the helper resolved and ran.
A moved helper exits 127 instead, with no such message.
Two further properties came free from running it: the pin table must carry a digest for the test host's platform, so a `COSIGN_VERSION` bump missing its digests now fails `make check` rather than a release cut; and the URL the stub recorded pins the platform mapping.

The same reasoning covers the caller: [`verify-release-test.sh`](../../scripts/release/verify-release-test.sh) runs `verify-release.sh` against a stub `cosign` that logs its arguments, which is what makes the artifact list, the identity/issuer constraints on every check, and "one bad signature fails the run" assertable without a published release.

## Where each tier can physically run (and what it costs)

The tier above says *what* observes a bug; this says *where that tier can run*.
Most validation is local on a dev machine; a short list needs real GitHub, real cloud, or real scale.
The **environment definitions** below are durable; the **Q-item mapping** is a snapshot of the [backlog](../STATUS.md) as of 2026-06 and may lag.

- **Local — `kind` (the default).** Unit, envtest, cluster-only/fake-GitHub e2e, and the load harness need only a Linux-kernel cluster plus a fake or in-cluster GitHub.
  This covers the large majority of work and runs on an Intel Mac under Docker Desktop.
- **Local — `minikube` + gVisor addon (the one thing kind can't do).** A `RuntimeClass=gvisor` node needs `runsc` on the node, which kind's container-nodes can't supply cleanly. minikube can: locally `minikube start --driver=qemu` (a Linux VM) then `minikube addons enable gvisor`; on a Linux CI runner `--driver=none` (or `docker`) + the same addon. gVisor's **systrap platform needs no nested virtualization**, so it works on a stock machine and a stock `ubuntu-latest` runner alike.
  Reach for minikube **only** for gVisor — kind stays the default everywhere else (lighter, already wired into the e2e workflows).
  Full local VMs (Lima/Colima/Multipass) host the same `runsc` setup but unlock nothing beyond gVisor.
- **Needs real GitHub.** live-GitHub e2e and the live broker-compatibility probe (the credential-gated `cmd/probe` binary).
  Free (GitHub API within rate limits); needs a test App/org credential as a CI secret.
  Automatable per-PR or nightly.
  The credential-free counterpart — the `cmd/probe/compat` suite that asserts every documented broker contract against the in-process broker model — runs locally in `make check` with no secrets; its published result is [broker-compatibility.md](broker-compatibility.md).
- **Needs real cloud.** Cloud KMS signing, managed control-plane behavior (EKS/GKE/AKS), and cloud workload-identity binding (IRSA / GKE WI / Azure WI).
  Not reproducible in kind/minikube — needs the actual provider.
  Automatable as a scheduled job that provisions an **ephemeral** cluster (eksctl/Terraform), torn down after.
- **Needs real scale.** The 1,000-pod real-cluster capacity run.
  A 4-core Docker Desktop VM can't host it; needs a multi-node cluster.
  The in-process load harness already covers the AGC-only claim locally for free, so this is release-gated, not routine.

### Cost & cadence (rough, ephemeral CI, 2026 ballparks)

| Validation | Substrate | ~Cost | Cadence |
|---|---|---|---|
| Broker-compat, live-GitHub (Q191; Q11†) | test GitHub App | $0 (free API) | per-PR / nightly |
| gVisor `RuntimeClass` (Q15) | minikube + gvisor addon, stock runner | $0 | per-PR / nightly |
| Cloud KMS + workload-identity legs (Q197 cloud) | KMS key + ephemeral EKS/GKE/AKS | KMS <$5/mo; ~$0.50–1 / run | nightly / weekly |
| Managed-cluster audit paths (Q182) | ephemeral EKS/GKE/AKS ×3 | ~$1–2 / full-matrix run | weekly / release |
| Per-cloud apiserver CIDR (Q183) | ephemeral cluster/cloud, or doc-only | ~$0.20–0.50 / cloud, or $0 | release / manual |
| 1,000-pod scale (Q181 real run; Q193 benchmark) | ~25–50-node ephemeral cluster | ~$10–30 / full run (~$3–8 at 250 pods) | occasional / release |

† Q11 is *also* blocked on a GitHub feature (X25519 ECDH) that does not yet exist — untestable at any cost until then.

*Ephemeral* = provision → test (~20–40 min) → tear down; cost is hourly proration of a small managed control plane (~$0.10/hr) plus a few small/spot nodes. A standing cluster costs more (~$50–100/mo) but CI doesn't need one.
Hosted Linux runners also expose `/dev/kvm` if VM-level isolation (Kata, Firecracker) is ever needed, but gVisor does not require it.

## Integration tests

Integration tests use envtest and are gated by the `integration` build tag.
They live under `internal/controller/integration/` in both `cmd/agc` and `cmd/gmc`.
Use the dedicated Makefile targets — they set `KUBEBUILDER_ASSETS` automatically:

```bash
make test-integration              # runs both cmd/agc and cmd/gmc integration tests
make -C cmd/agc test-integration   # AGC only
make -C cmd/gmc test-integration   # GMC only
```

`RUN=` narrows a module's run to matching test names.
It is passed straight to `go test -run`, so the full regexp syntax works:

```bash
make -C cmd/gmc test-integration RUN='TestCRD_ActionsGateway_LogLevel_DefaultsToInfo'
```

The target adds `-v -count=1` alongside `-run`: a targeted run wants the test's output, and a cached `PASS` prints none.
A name that matches nothing reports `no tests to run` and **passes**.
Unlike `make test` and `make e2e`, these two targets have no zero-match guard yet (Q736), and neither falls back to running the module.
See [Narrowing a run with `RUN=`](#narrowing-a-run-with-run) for the cross-tier picture.

Prefer `RUN=` over exporting `KUBEBUILDER_ASSETS` yourself.
Hand-assembling it has two traps, both of which surface as a confusing envtest failure rather than an obvious mistake (Q582 spent three attempts on them):

- **The version is pinned, not guessed.** `setup-envtest use` needs the version the module pins in `ENVTEST_K8S_VERSION` (`cmd/agc/Makefile`, `cmd/gmc/Makefile`).
  Typing another one gives you a different apiserver than CI runs against, or one that isn't installed, in which case `use` quietly downloads it.
- **`--bin-dir` must be absolute.** `-p path` echoes back the shape you gave it, so a relative `--bin-dir` leaves `KUBEBUILDER_ASSETS` relative, and each test binary then resolves it against its own package directory rather than the repo root.
  The Makefiles pass `$(REPO_ROOT)/.build`.

Unit tests (`make test` / `go test ./...`) do **not** require envtest — the integration packages are excluded by their `//go:build integration` tag.

### The envtest suite budget

`go test -timeout` gives each suite a wall-clock budget.
It is **10m**, single-sourced in [`scripts/go/go-test-integration.sh`](../../scripts/go/go-test-integration.sh), which both module Makefiles and both CI steps run through.
The budget is per test *binary*, so each module's suite gets its own.

Every run reports what it spent:

```
==> github.com/actions-gateway/github-actions-gateway/gmc/internal/controller/integration used 213.9s of its 10m budget (36%)
```

Past 80% that line moves to stderr as a warning.
That is the signal that was missing before Q741: a suite creeping toward the cliff looked exactly like one with room to spare, right up to the run that panicked.

**When the budget is exceeded, the test named in the panic is a bystander.** Go's timeout panic names whichever test held the wall at that instant, so the name moves from run to run and lands on tests the diff never touched.
Q166 spent two wrong hypotheses there; see [A gate that fails on your branch is not yours until it fails on the base too](#the-status-you-report-is-a-claim-too).
The wrapper says so outright on a breach, and on CI emits it as a step annotation as well, because for most readers the annotation *is* the failure.

#### Why the budget was raised rather than the package split

Q741 offered both remedies.
The measurements chose:

| Run | GMC suite | AGC suite |
|---|---|---|
| CI (`ubuntu-latest`, 9 runs, 2026-08-09/10) | 201.5–217.8s | 259.1–263.7s |
| Local, idle dev Mac, alone (2026-08-10) | 197.4–219.5s (5 runs) | 255.9–257.7s (2 runs) |
| Local, the two suites run concurrently | 227.7s | 263.1s |

**No test dominates.** Across 166 top-level GMC tests the largest is 12.4s and the top ten are 43% of the total; the median is 0.73s.
Splitting a uniform tail in half is an arbitrary cut that buys ~100s per package and pays for a second envtest bootstrap, while leaving whatever breached the budget untouched.

**The suite's own spread is not what breaches it either.** Within an environment the range is about 10%, and CI and an idle laptop agree per test to within a few hundredths of a second.
A 300s budget was never marginal for a 200s suite.

So neither remedy the row proposed addresses the thing that actually went wrong, because **the 324.5s Q741 recorded has no measured mechanism.** Three candidates were tested and all three came back too small:

- The local Makefile target runs `./...` where CI runs only the integration package, so the suite could have been contending with its own module's other packages.
  It is not: 221.9s and 198.2s inside a `./...` run, against 197.4–219.5s isolated.
- The two environments could have differed.
  They do not.
- Two suites on one machine could have starved each other.
  At two they cost each other 4% (AGC) and up to 8% (GMC), nowhere near the 1.5x excursion.
  A heavier [parallel-dispatch](parallel-dispatch.md) wave remains plausible and is unmeasured.

That is what settles it.
A budget with real headroom is the remedy that does not depend on knowing which of those it was, and it is the one that keeps the next occurrence cheap: at 10m a breach means a 2.7x slowdown, which is a finding rather than a coin flip, and the run says so in those words instead of naming a test.
The tier also now takes the machine-wide heavy-build slot `make check` takes, which it had never held: a guard on the one hypothesis the measurement could not rule out, not a demonstrated fix.

### Avoiding shared-stub flakes in the AGC suite

The `cmd/agc` integration suite shares one broker stub (`brokertest.Server`, created once in `TestMain`) across every test in the package.
Sessions other tests register stay in the stub's global maps, so the global accessors (`RegisteredSessions()`, `ActiveSessionCount()`) accumulate across the whole package.
Picking a session from that global list — e.g. `RegisteredSessions()[len-1]` — can land a job on a session another test left active, which never spawns a worker pod in your namespace, so the test times out intermittently on a loaded CI runner (this flake class was Q91, Q113, Q120).

Two rules keep a new test deterministic:

- **Scope every session assertion and enqueue to your CR's owner.** Use `ActiveSessionsForOwner("<cr-name>")` and `enqueueJobOnOwnerSession(...)` instead of the global accessors.
  A CR name is unique to one test, so owner-scoping returns exactly the sessions you created — never a sibling's. `enqueueJobOnOwnerSession` also retries until an owner session is present, so it is immune to the picked session having just idle-shut.
  The filter matches `"<cr-name>-<agentIndex>"` with the index segment exact, so a sibling CR whose name *extends* yours keeps its own bucket.
  It does **not** separate kinds: the AGC builds `ownerName` from the bare CR name for a RunnerGroup and a RunnerSet alike, so a same-named pair sends byte-identical owners and shares one bucket (Q677).
  A test that runs both kinds must give them different names.
- **Wait on the condition, not the clock.** Prefer the stub's channel-based waiters (`WaitForFirstPoll`, `WaitForSessionDelete`) over wall-clock sleeps; they return the instant the event happens.
  The timeout you pass is only a safety ceiling, not the expected latency — size it generously for a CPU-starved 2-vCPU CI runner (seconds of headroom, well inside the package's 5m test timeout), since raising a too-tight ceiling alone just moves a flake rather than fixing it.

### Test doubles must long-poll

Both GitHub doubles model the real backend's long poll: an empty poll is **held** until a message lands or a poll window elapses.
A double that answers "nothing to deliver" instantly turns any polling client into a spin loop, which burns CI CPU on every unrelated test in the package and widens the timing windows other tests race against.

- [`scaleset/scalesettest`](../../scaleset/scalesettest/) — `DefaultPollTimeout` (1s) bounds the wait, and a parked poll wakes the instant the queue changes (a job enqueued, acquired, or completed; the session dropped), so delivery stays immediate and tests stay fast. `Server.SetPollTimeout(0)` restores the non-blocking behavior — use it *only* in a test that asserts the 202 itself, never in one that drives a polling client.
  Before this landed (Q287) an idle scale-set listener polled the stub ~5,000×/s; it is now ~1/s.
- [`test/fakegithub`](../../test/fakegithub/) — long-polls job delivery for the same reason (Q148, where an instantly-returning fake collapsed the listener pool: replacement listeners idle-exited in milliseconds).

Every poll loop also enforces its own floor between two consecutive polls that deliver nothing, so a *real* backend that declines to hold the poll cannot spin it either: `broker.MinPollInterval` (100 ms) on the v1 broker loops, and the scale-set listener's own `minPollInterval`.
That is defense in depth, not a substitute — a double that does not long-poll still distorts every timing assertion around it.
[`broker/brokertest`](../../broker/brokertest/) is the one double that still answers 202 at once, which is why the floor is what holds the v1 poll rate down against it: measured 2026-08-11, a single listener polled it at 9,472 req/s before the floor and 9.3 req/s after (`cmd/agc/internal/listener/pollrate_q788_test.go`, Q788).

An idle threshold configured in polls is therefore a **time** budget, not a spin count: `IdleThreshold: N` against a non-blocking double is roughly `N × 100 ms` of session life.
A test that waits for burst goroutines to drain has to size N against its own `Eventually` budget.

### The broker doubles share one protocol core

Three broker doubles implement the GitHub Actions broker wire protocol: [`broker/brokertest`](../../broker/brokertest/) (the in-process integration stub), [`test/fakegithub`](../../test/fakegithub/) (the deployed fake-GitHub e2e image), and the [load harness stub](../../cmd/agc/test/load/broker_stub.go).
They diverge in what a job delivery and an AcquireJob *mean* — fan-out accounting (Q260), single-use JIT consumption (Q114), saturated auto-delivery — but the session and credential *mechanics* are identical: minting `session-<n>` IDs, resolving a DELETE by its `sessionId` query param or bearer token, owner-scoped session listing, and the connection-reuse-safe JSON framing.
Those live once in [`broker/brokerstub`](../../broker/brokerstub/) (Q368); each double layers its own delivery/acquire policy on top. `broker/brokerstub` is deliberately **standard-library-only** so the fakegithub distroless image links no third-party code — do not import the `broker` client (or anything else) into it.

### The scale-set protocol has exactly one model

[`scaleset/scalesetstub`](../../scaleset/scalesetstub/) is the only implementation of the runner-scale-set protocol in the repo.
Keep it that way: a second hand-rolled scale-set stub is a second dialect to keep in sync, and `cmd/probe` exists to catch library-vs-wire divergence — which it cannot do against a stub that agrees with whatever the probe assumes.

Two doubles wrap it, and neither carries protocol logic of its own:

- [`scaleset/scalesettest`](../../scaleset/scalesettest/) serves it over an `httptest.Server`, for the scale-set listener's unit tests, the `cmd/agc` v2 RunnerSet integration suite, and `cmd/probe`'s Investigation E and F scenarios (Q389).
- [`test/fakegithub`](../../test/fakegithub/) mounts it into the deployed fake-GitHub e2e image, next to the classic broker protocol (Q528).
  That is what lets the scale-set tier's acquisition half run on kind at all — see `E2E_AGC_ScaleSetAcquisition`.

The split exists because `httptest` cannot be linked into fakegithub (see the `package main` rule below), which is the same reason `broker/brokerstub` is separate from `broker/brokertest`.
The one thing the wrapper supplies is the base for the protocol's self-referential URLs — the admin connection's tenant URL, a session's `messageQueueUrl`, a `JobAvailable`'s `acquireJobUrl` — fixed for httptest, request-derived for a deployed pod.

The model is *semantic* rather than a scripted response list, so a test states the backend condition it wants and the stub derives the wire from it: `PrequeueJobs` queues jobs against the scale set's label before it registers (for a caller that creates its own scale set mid-run), `SetGHESAcquireFlow` switches between auto-assign and the JobAvailable→acquire path, and the `Fail*` levers (`FailRunnerGroups`, `FailSessionCreate`, `FailSessionRefresh`, `FailStaticAcquireRoute`, `SetRateLimitPolls`) each model one observed backend failure.
Assert on `Calls()`, the ordered call log.
Reach for `SeedMessage`/`SeedRawMessage` only for the shapes the model cannot reach on its own — a lifecycle message with no preceding assignment on that scale set, or a body no client can decode.

Unlike `broker/brokerstub`, `scalesetstub` is not standard-library-only: it encodes responses with the `scaleset` package's own wire types, so a renamed field breaks the build rather than the test.
The cost is that fakegithub's image now links `githubapp` and `golang-jwt/jwt/v5` — already blocking-scanned in the `agc` and `gmc` Trivy legs, so no new module enters the scanned surface.

**No `package main` may reach `net/http/httptest`.** A production binary must never link a test server. `TestNoPackageMainReachesHTTPTest` (in `cmd/probe/compat`) enforces this: it walks every `package main` in the workspace and fails if any transitively imports `net/http/httptest` in its compiled build graph (`go list -deps`, so a `_test.go` file importing httptest — as fakegithub's own tests do — is correctly ignored).
It runs in `make check`; a stray import of a broker double into a shipped binary fails the gate.

## Load tests

The load harness (Q13) pins the design's headline capacity claim — thousands of virtual runner sessions multiplexed per AGC, each costing one re-registration per job (the single-use JIT lifecycle, Q114).
It is gated by the `//go:build load` tag and lives under [`cmd/agc/test/load/`](../../cmd/agc/test/load/); its [README](../../cmd/agc/test/load/README.md) documents every knob and how to read the output, and [milestone-5.md §2](../plan/milestone-5.md) the design rationale.

It needs **no cluster and no GitHub credentials**: it drives the real `listener.Multiplexer` + `agentpool.Pool` + per-goroutine `broker.Client` wiring against an in-process broker stub (single-use JIT + long-poll), a controller-runtime fake client for agent Secrets, and an in-memory registrar.

```bash
make load-test-quick   # 10 tenants × 100 listeners = 1,000 sessions, short window (~1 min)
make load-test-full    # same scale, realistic job hold; writes results/latest.md (~3-5 min)
```

Both run under the same desktop-safety throttle as the rest of the suite (a no-op on CI).
The Service Level Objectives (SLOs) it asserts — sustained concurrent sessions, ≈1 re-registration per job, no goroutine leak — are the faithful results; absolute throughput and recycle latency are bounded by the in-process control-plane stand-ins and are reported for trend, not as production figures (see the README's fidelity boundaries).
It is **not** wired into `make check` or per-PR CI — run it when changing the concurrency core (listener/multiplexer/agentpool) or validating a capacity claim.

## The live-autoscaler drift gate

One tier exists to catch a change **upstream**, not a change here: the capacity gate's elastic-cluster signal (Q406) recognizes cluster-autoscaler by two Event reasons and a reporter name, pinned in a unit table from recorded samples.
Those strings belong to upstream, and a reword there fails *open* — an unrecognized vocabulary yields "not declined", which is exactly the ungated behaviour — so the mode would silently become a no-op on every elastic cluster with every existing test still green.
Nothing that runs against recorded samples can observe that, by construction.

The gate has two arms, one per autoscaler project the matcher recognizes, each against events a **real** upstream build emits — the autoscaler, its scheduling evaluation, and its events are genuine, only the nodes are fake (kwok), so each fits in a kind cluster on a laptop:

- `make test-autoscaler` — **cluster-autoscaler**, via its own [kwok cloud provider](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/kwok) (Q474).
- `make test-karpenter` — **Karpenter**, via [karpenter-provider-kwok](https://github.com/kubernetes-sigs/karpenter/tree/main/kwok) (Q479).
  This is the arm that needs a live counterpart most: Karpenter's declination shares kube-scheduler's reason string (`FailedScheduling`), so the whole arm is the reporter discrimination, and an upstream attribution change would disable it with nothing else looking different.
  Upstream publishes no image for its kwok provider, so the recipe clones the pinned tag and builds it — the one extra minute the arm costs.

```bash
make autoscaler-cluster        # one-time: kind cluster + kwok + cluster-autoscaler (~2 min)
make test-autoscaler           # three cases, ~30 s
make autoscaler-cluster-delete # tear down when done

make karpenter-cluster         # one-time: kind cluster + kwok + Karpenter built from source (~4 min)
make test-karpenter            # three cases, ~1 min
make karpenter-cluster-delete  # tear down when done
```

- **Each arm gets its own cluster, and neither is the e2e one.** A live autoscaler creating and deleting nodes underneath the e2e suite would perturb every spec in it, and two autoscalers contending for the same pending pods would make both arms flaky. `AUTOSCALER_CLUSTER` (default `gag-autoscaler`) and `KARPENTER_CLUSTER` (default `gag-karpenter`) name them; `CA_VERSION`, `KARPENTER_VERSION` and `KWOK_VERSION` pin what gets installed, in [`scripts/e2e/autoscaler-cluster.sh`](../../scripts/e2e/autoscaler-cluster.sh) and [`scripts/e2e/karpenter-cluster.sh`](../../scripts/e2e/karpenter-cluster.sh).
  Manifests live in [`test/autoscaler/`](../../test/autoscaler/) and [`test/karpenter/`](../../test/karpenter/).
- **Build tags `autoscaler` and `karpenter`**, in `cmd/agc/internal/controller/autoscaler_verdict_live_test.go` and `karpenter_verdict_live_test.go` (shared plumbing in `live_harness_test.go`), in-package so they call the unexported matcher directly rather than widening its API for a test.
- **They fail rather than skip when the cluster is absent.** A drift detector that skips itself detects nothing; the failure message names the make target.
- **Not in `make check`, and change-triggered rather than scheduled in CI** — see below.

### Its cadence: the version bump, not a clock

[`autoscaler-drift.yml`](../../.github/workflows/autoscaler-drift.yml) runs each arm on pull requests that touch its pins (`scripts/e2e/autoscaler-cluster.sh`, `scripts/e2e/karpenter-cluster.sh`), its manifests (`test/autoscaler/`, `test/karpenter/`), or the shared matcher and its tests (which re-run both arms) — classified by a `changes` job like every other gate here, not by a top-level path filter.
Plus `workflow_dispatch`, whose `ca_version` / `karpenter_version` inputs probe a release without committing to it.

It is deliberately **not** on a cron.
A weekly sweep would re-run one fixed experiment: `CA_VERSION` is a pin, so a scheduled run installs the same image and asserts the same strings every week until something in the repo changes.
The drift it exists to catch arrives in a cluster-autoscaler *release*, which a pinned sweep never installs.
The version move is the event, so the version move is the trigger.

What makes that fire without anyone remembering to run it is a coupling worth knowing:

> The harness pins no `KIND_NODE_IMAGE`, so its cluster runs **kind's default node image** — Kubernetes 1.36.1 for kind v0.32.0. cluster-autoscaler is released per Kubernetes minor, so the kind release chooses the Kubernetes minor, which chooses the CA minor.
> That is why `CA_VERSION` (v1.36.x) tracks kind's default rather than the deliberately-pinned-down `KIND_NODE_IMAGE` the e2e tier uses (v1.35.5).

`KIND_VERSION` is pinned in this workflow as well as [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml), and [`updatecli.d/kind.yaml`](../../updatecli.d/kind.yaml) rewrites both weekly.
So the kind bump PR trips this workflow's `changes` filter and runs the gate; when that bump moves the default node image's minor, `CA_VERSION` must move to the matching CA minor in the same PR — and a CA minor is where a vocabulary reword lands. **A kind bump PR whose autoscaler-drift job fails on version skew is telling you to bump `CA_VERSION`, not to pin the node image.**

`KARPENTER_VERSION` has no such coupling: Karpenter is not released per Kubernetes minor (one release supports a wide range), so no kind bump ever prompts that pin.
Its trigger is [`updatecli.d/karpenter.yaml`](../../updatecli.d/karpenter.yaml) (Q529), which weekly resolves the **latest** upstream release — minor or patch, since with no minor coupling there is no skew to guard and a minor is where a reword most likely lands — and opens a PR moving the pin.
That PR edits `scripts/e2e/karpenter-cluster.sh`, which is in the `changes` filter above, so the gate runs on it.

#### The patch releases in between (Q483)

The kind coupling only fires on a *minor*, which left every cluster-autoscaler **patch** release untested until the next minor came round — and a patch can reword an event string as easily as a minor can.
[`updatecli.d/cluster-autoscaler.yaml`](../../updatecli.d/cluster-autoscaler.yaml) closes that: weekly it reads the current `CA_VERSION`, resolves the newest patch published *inside that same minor*, and opens a PR moving the pin.
That PR edits `scripts/e2e/autoscaler-cluster.sh` — the first path in the `changes` filter above — so the gate runs on it.
The manifest is structurally incapable of crossing a minor ([`latest-cluster-autoscaler-patch.sh`](../../scripts/updatecli/latest-cluster-autoscaler-patch.sh) takes the pin and can only return a version in its minor, asserted under `make scripts-test`), because crossing one would manufacture the very node-image skew this gate reports.

This is not the cron the section above rules out.
The weekly run resolves a version and opens nothing when the answer is the pin it already has, so what reaches the gate is still a release, never a calendar tick.
There are now two version-move triggers and they divide cleanly: **updatecli moves the patch, the kind bump PR moves the minor.**

Two consequences to plan for:

- **The gate is not a required status check.** A failure wants a human decision — adopt the new vocabulary (update the matcher and the recorded unit table) or hold the bump — the same posture as the shellcheck and polaris bump PRs.
  The workflow still ends in an `autoscaler-drift-gate` job of the usual shape, so it can be required later without restructuring ([required-status-checks.md](../plan/archive/required-status-checks.md)).
- **An updatecli PR arrives with no checks at all.** GitHub never triggers workflows on a `GITHUB_TOKEN`-authored PR, so both triggers above only pay off if the checks are re-run — close and reopen the PR during the weekly dependency triage pass ([dependency-updates.md](dependency-updates.md#operating-notes)).
  On a cluster-autoscaler bump that step is the entire PR: nothing else in it needs testing.

What each arm measured on first run, and the findings that outlived them, are in [the plan §9c](../plan/capacity-aware-intake.md#9c-the-live-autoscaler-harness-and-what-it-measured-q474) (cluster-autoscaler) and [§9i](../plan/capacity-aware-intake.md#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479) (Karpenter — including the recorder-generation premise it corrected).

## End-to-end tests

E2E tests run on a local `kind` cluster, are gated by the `//go:build e2e` tag, and live under `cmd/gmc/test/e2e/`.
They split into three tiers (see [design §7.3](../design/07-test-plan.md#73-end-to-end-tests)):

- **cluster-only** — GMC infrastructure (no GitHub required).
- **fake-GitHub** — AGC lifecycle against the in-cluster `test/fakegithub/` server.
- **live-GitHub** — real GitHub workflow dispatch (requires App credentials).

Typical local run:

```bash
make e2e-cluster        # one-time: create the kind cluster
make e2e-images         # builds gmc/agc/proxy/worker/fakegithub, loads into kind
make e2e                # runs cluster-only + fake-GitHub
make e2e-clean          # tear down when done
```

For iterating against a single spec without re-creating the cluster, see [kind-iteration.md](kind-iteration.md).
It also covers pointing AGC at fakegithub vs. real GitHub via the `AGC_EXTRA_*` env vars and using `E2E_SKIP_TEARDOWN=true` to keep state between runs.

### Questions about spec selection are answerable without a cluster

`ginkgo run --dry-run` walks the spec tree and reports what *would* run, executing no node: no `BeforeSuite`, no cluster, no images.
It still compiles the suite under the `e2e` tag and still applies `--focus`, `--label-filter`, `--fail-on-empty` and the rest, so anything whose answer is "which specs does this select, and what does the suite do about it" is a seconds-long question rather than a tier:

```bash
make ginkgo
(cd cmd/gmc && ../../.build/ginkgo run --tags e2e --dry-run --fail-on-empty \
    --focus 'E2E_GMC_ProxyServiceCreated' ./test/e2e/...)
```

Measured 2026-08-08 while wiring `RUN=` onto `make e2e` (Q679): a real spec name gave `Ran 1 of 74 Specs` / `SUCCESS!` and exit 0, a nonsense one gave `Ran 0 of 74 Specs` / `FAIL! - Detected no specs ran and --fail-on-empty is set` and exit 1, in 13 seconds total.
That settled both directions of a filter's behaviour against the actual suite, ahead of CI, with no cluster standing.

Reach for it whenever the claim under test is about **selection** rather than behaviour: a filter that should match nothing, a label expression, a new `SUITE` mapping, whether an `Ordered` container's specs are reachable at all.
It proves nothing about whether a spec *passes*, since no node runs, so it is a complement to the tier and never a substitute.

### `Ordered` containers run whole, in one process — which is why a suite can hold package state

Every e2e suite is an `Ordered` container, and several assign the package-level `fakegithubLocalPort` from their own base port in their own `BeforeAll` without being `Serial`.
That is safe, and it is worth knowing *why* before writing a suite that relies on it — or one that assumes more than it grants.

Two independent guarantees hold it up, both verifiable in the vendored source:

1. **Parallel processes are separate OS processes.** `ginkgo --procs N` execs the compiled test binary N times ([`ginkgo/internal/run.go:44`](../../vendor/github.com/onsi/ginkgo/v2/ginkgo/internal/run.go)) and coordinates them over an RPC/HTTP server (`internal/parallel_support/`).
   Package-level variables are therefore **per process** — never shared across them, so two processes cannot race on one.
2. **An `Ordered` container is one scheduling unit.** Ginkgo groups specs for execution and "ordered containers must be preserved as a single group" ([`internal/ordering.go:96`](../../vendor/github.com/onsi/ginkgo/v2/internal/ordering.go)).
   The parallel counter hands out an index into *groups*, not specs, and each is run whole by `newGroup(suite).run(...)` ([`internal/suite.go:499-519`](../../vendor/github.com/onsi/ginkgo/v2/internal/suite.go)).
   So no other container can interleave between a suite's specs and reassign what its `BeforeAll` set.

**What this does not grant: mutual exclusion.** Two different `Ordered` containers still run *concurrently* in different processes.
The package var is safe because each process has its own copy and each suite writes a distinct base port before use — not because Ginkgo serialises anything. `Ordered` orders specs within a container; it does not order containers against each other.
A resource that is genuinely shared *outside* the process — a cluster object, a fixed host port, a GitHub session — gets no protection from `Ordered` and needs `Serial`, an owner-scoped filter, or a per-process derivation such as `GinkgoParallelProcess()`.

Dropping `Serial` from a suite is therefore a claim about *external* isolation, never about package state.
Worked example, including the owner-prefix filter that made one such drop safe: [e2e-ci-speed-round-2.md](../plan/e2e-ci-speed-round-2.md#5-de-serialize-e2e_agc_workerpodlifecycle-).

### Every e2e suite dumps cluster state before it tears down

An e2e suite that deletes its namespace in teardown destroys the only evidence of why it failed.
The specs here wait minutes for a Deployment, a condition, or a NetworkPolicy to settle, so a timeout's failure message is a bare "never became ready" — and by the time anyone reads it in CI the namespace is gone.
Seven suites shipped that way, which is what made Q664 undiagnosable and Q666 the fix.

So a suite that creates a namespace hooks the failure before teardown:

```go
AfterEach(func() {
    if CurrentSpecReport().Failed() {
        utils.DumpProvisioningDiagnostics(gmcNamespace, managerDeployment, tenantNS)
    }
})
```

Two dumps exist, both in [`cmd/gmc/test/utils/diagnostics.go`](../../cmd/gmc/test/utils/diagnostics.go).
Pick by what the suite is waiting on:

| Suite waits on | Helper | Adds |
|---|---|---|
| A broker session, a worker pod, a job | `DumpAGCSessionDiagnostics` | RunnerGroup status, AGC log tail, ReplicaSet templates, fakegithub |
| Provisioning, a CR condition, RBAC, a NetworkPolicy verdict | `DumpProvisioningDiagnostics` | Namespace labels, ActionsGateway status, NetworkPolicies, per-pod log tails, the manager's policies and log tail |

**Watch the volume when you extend one.** A dump nobody can read is a dump that did not happen. `DumpProvisioningDiagnostics` samples the NetworkPolicy `ipBlock` lists for exactly this reason: measured on a forced-failure run of `E2E_GMC_Teardown`, the tenant workload policy's GitHub meta ranges were 7352 entries filling 14704 of that section's 14901 lines, and the whole dump was 15450 lines.
Sampling five and printing the elided count brought it to 758 with every section intact.
Before adding a `-o yaml` of anything, check what it looks like on a real tenant rather than what it looks like in the type definition.

Both are best-effort — a failed command prints one line and the dump continues, so it can never mask the real failure — and neither reads a Secret.
Keep it that way when extending them: these dumps run against live tenant namespaces, and credentials reach a tenant pod as volume mounts, which `kubectl describe` renders as a Secret *name*. `TestDumpProvisioningDiagnosticsNeverReadsASecret` fails the build if a `get secret` is ever added.

**`AfterEach` is the right node, and `DeferCleanup` is not.** Ginkgo runs `JustAfterEach`, then `AfterEach`/`AfterAll`, and only on a later pass the `DeferCleanup` nodes ([`internal/group.go:249-258`](../../vendor/github.com/onsi/ginkgo/v2/internal/group.go)).
An `AfterEach` dump therefore beats *every* cleanup the suite registered — the `DeleteNamespace` in an `AfterAll`, and also the per-spec `DeferCleanup` that deletes a probe pod, so the probe's logs are still fetchable when the dump runs.
Putting the dump in a `DeferCleanup` gives up that ordering: cleanups run last-registered-first, so a probe pod registered after it is deleted first and the dump captures nothing.

### Watching an e2e run in progress

At `--procs 6` Ginkgo's own output is close to silent: it suppresses spec-start entirely in parallel mode, prints a passing spec as a bare `•`, and two measured CI runs went 98 s and 78 s between any output. `make e2e` therefore runs [`scripts/e2e/progress-watch.sh`](../../scripts/e2e/progress-watch.sh) alongside the suite, which prints one line per 30 s:

```
[e2e t+04:12] 31/73 specs | 29 ok, 1 failed, 1 skipped | running: E2E_GMC_Isolation cross... (3m58s), E2E_AGC_WorkerDrain a dr... (2m01s)
```

The suite appends spec start/end events to `E2E_PROGRESS_FILE` (default `tmp/e2e-progress.jsonl`) and the watcher renders them.
The two halves are split that way because **Ginkgo intercepts spec stdout**: anything a spec or a `ReportAfterEach` prints is captured and replayed when the spec ends, so the suite cannot narrate its own progress — only write a file something outside it reads.

Knobs: `TEST_PROGRESS_INTERVAL` (seconds between lines — the same knob paces [the unit tier](#watching-a-unit-run-in-progress), and `0` turns both off), `E2E_PROGRESS_SPEC_WIDTH` (chars of spec text per running spec), and `E2E_PROGRESS_FILE=` (empty) to disable both halves.

Two things to know when changing the event format:

- **Event lines must stay under `PIPE_BUF` (4 KiB).** All six processes append to one file with no lock; below that size an `O_APPEND` write lands atomically, above it two processes interleave bytes and corrupt both records silently. `TestProgressEventFitsPipeBuf` guards the budget — spec text is truncated to keep it.
- **Render from the event stream, never from Ginkgo's log.** Reporter output is not a stable contract, so a regex scraper over it drifts silently — the same failure mode [the live-autoscaler drift gate](#the-live-autoscaler-drift-gate) exists to catch.

After the run, [`scripts/e2e/e2e-report-summary.sh`](../../scripts/e2e/e2e-report-summary.sh) renders `tmp/e2e-report.xml` into the job summary — counts, every failure with its message, and the ten slowest specs — and emits one `::error::` annotation per failed spec.
It runs `if: always()` in `e2e-reusable.yml` and never exits non-zero, because it runs on the path where the suite may have died before writing a report.
Run it locally against any report to get the same table.

**Egress-enforcing CNI profile.** `make e2e-cluster KIND_CNI=calico` builds the cluster with Calico instead of kindnet (see [kind-iteration.md § CNI selection](kind-iteration.md#cni-selection-kindnet-default-vs-calico)).
The two runtime egress-negative specs (`E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod`, `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI`) and the two manager metrics-NP specs (`E2E_GMC_ManagerMetricsNP_DeniesUnlabeledNamespace`, `E2E_GMC_ManagerMetricsNP_AllowsLabeledNamespace`) skip themselves on kindnet — whose enforcer does not drop egress — and only assert real packet drops on a Calico/Cilium cluster.
Run them with the Calico profile when validating NetworkPolicy enforcement changes (Q7b/Q83).
CI runs this profile per-PR whenever a change touches NetworkPolicy/proxy code — see [the Calico e2e lane](#the-calico-e2e-lane) below.

**Curl test image.** The connectivity, isolation, and metrics specs run a `curlimages/curl` pod.
It defaults to the upstream Docker Hub ref (`curlimages/curl:8.10.1`), which is fine locally.
CI sets `E2E_CURL_IMAGE` to a local-registry mirror (`127.0.0.1:5000/curlimages/curl:8.10.1`, populated by the workflow's mirror step) so the kind nodes never pull from Docker Hub — anonymous Hub rate limits (HTTP 429) were starving these pods and flaking three specs.

**Test labels and the `multi-node` suite.** Three Ginkgo labels annotate the suite.
CI runs the **full** suite — `make e2e` with no `SUITE`, so no `--label-filter` — on the default 2-worker cluster (`test/kind-config-2worker.yaml`), so every labelled spec runs in CI:

- `multi-node` — specs that need the 2-worker cluster shape to be meaningful: `E2E_GMC_ProxyPodScheduledOnWorker` (pod-to-worker placement), `E2E_GMC_PDBPreventsEvictionBelowMinAvailable` (PodDisruptionBudget (PDB) blocks eviction while a replica survives on another node), `E2E_GMC_GMCRestartPreservesState`, and `E2E_Migration_MigratedTenantReconcilesIntoAWorkingControlPlane` (it stands up two coexisting proxy pools, each free to autoscale, and the 2-worker shape is the one it is measured on — see the anti-affinity notes below).
  Two whole containers carry it too: `E2E_AGC_WorkerNodeDrain` and `E2E_AGC_WorkerPreemption`.
- `github-real` — the live-GitHub specs that dispatch against real GitHub (`E2E_GitHub_RealDispatch`); they self-skip when the `GITHUB_E2E_*` env vars are unset.
  The container is `Ordered` and every live-GitHub spec belongs in it: the suite runs `--procs 6`, so a second top-level container would run *concurrently* with this one, and two live gateways registered on the same org runner group is the Q511 collision inside a single run.
- `scaleset-live` — the one scale-set spec inside that container (`E2E_GitHub_ScaleSetEvictedWorkerLatencyAndRerun`), additive to `github-real` rather than a replacement.
  It is declared last, and Ginkgo skips the remainder of an `Ordered` container after a failure, so any of the six specs ahead of it failing costs it the whole run — twice on 2026-08-03, at ~55 minutes each.
  Run it alone with `SUITE=live-github-scaleset` once the container is known green.
  It still must not run beside the rest of the suite, for the `AGC_EXTRA_*` reason above.
- `real-github-egress` — the specs whose traffic terminates at the live `api.github.com`: the v1/v2 `ProxyConnectWorks` CONNECT specs, the two `E2E_V2_DirectEgress` specs (their NP ipBlock-peer waits also depend on the GMC's live `/meta` fetch), and the live-GitHub container.
  Not a filter label: a suite-level `AfterEach` (`cmd/gmc/test/e2e/github_egress_preflight_test.go`) uses it for failure-time attribution — see [Runner→GitHub egress attribution](#runnergithub-egress-attribution-q352).

For a faster local inner loop on a 1-worker cluster, `make e2e SUITE=single-node` maps to `--label-filter '!multi-node'` and skips the multi-node specs; unset `SUITE` runs everything (matching CI).
To narrow further, `RUN='<regex>'` adds a `--focus` over the spec text within whatever `SUITE` selected; see [Narrowing a run with `RUN=`](#narrowing-a-run-with-run).
Both filters are covered by `--fail-on-empty`, so a `SUITE` or `RUN` that selects no spec fails the run instead of reporting a green e2e.
The HPA scale-up spec (`E2E_GMC_HPADrivesScaleUp`) is unlabelled and CI-safe: it patches `HPA.spec.minReplicas` to drive the HPA→Deployment control path deterministically rather than burning CPU to trigger autoscaling, so it runs everywhere.

**A proxy replica costs a whole worker node, so a pool sized past the cluster strands replicas.** Every proxy pod — v1's inline `actions-gateway-proxy` pool and v2's `<proxy>-proxy` pool alike — carries a `requiredDuringScheduling` `podAntiAffinity` on `kubernetes.io/hostname`, so a pool's N replicas need N worker nodes and the default cluster has two.
The pods request `10m` CPU, so 60 % utilization is 6 millicores and any startup burst trips the HPA; a pool whose `maxReplicas` exceeds the worker count will therefore park the surplus in `Pending`.
That is harmless where a spec only needs the pool *reachable* (`utils.WaitForDeploymentReady` is satisfied by one ready replica), and fatal where it waits on the full count — pin such a pool with `utils.TenantFixture.WithProxyReplicas(1, 1)`.
Raising the wait does nothing: the pod is deadlocked, not slow, and `kubectl describe pod` says so in one line (`FailedScheduling … didn't satisfy existing pods anti-affinity rules`).

**The two pools no longer repel each other (Q582).** They used to: both stamped `app: actions-gateway-proxy`, the sole key of v1's PDB selector, v1's Deployment selector, and v1's anti-affinity term, so each pool's pods were claimed by all three of the other's — coexistence cost v1+v2 nodes, each pool's pods fell under the other's PDB, and both HPAs wedged on `AmbiguousSelector`/`FailedComputeMetricsReplicas` so neither could scale back down.
That was Q570's proximate cause: the v1 pool's HPA scaled to 2 about 15 s in, and the v2 pool `gag-migrate` created a minute later was unschedulable for good.
A v2 pool is now selected solely by `actions-gateway.com/egress-proxy: <proxy>`, which no v1 pod carries, so each anti-affinity term repels only its own pool's replicas and two coexisting pools fit on `max(v1, v2)` nodes rather than `v1+v2`.

**Waiting for the AGC, not just its Deployment.** A spec that waits for a broker session (or anything else that needs the AGC operational) must gate on `utils.WaitForRunnerGroupReconciled`, not only `utils.WaitForDeploymentReady`.
Deployment readiness means only that the AGC's health server is up — it binds within seconds of pod start and is deliberately decoupled from the GitHub-App token fetch (`cmd/agc/main.go`), whose budget alone is up to ~2 minutes. `WaitForRunnerGroupReconciled` waits for `RunnerGroup.status.observedGeneration` to be set, which the AGC does only after token + agent registration + listener-multiplexer start all succeed.
Gating on Deployment readiness alone folds the AGC's whole startup into the session wait's budget, which under parallel CI load (token/registration/session round-trips to the shared single-replica fakegithub) can exhaust it and surface as a misleading "no session registered" timeout (Q134).

**An `AfterAll` deletes the tenant CRs in dependency order — waiting on each — before the namespace.** `utils.DeleteNamespace` is `--wait=false`, so a bare namespace delete races the cascade against the tenant's own AGC pod.
Every `agentpool-cleanup` finalizer is cleared by an AGC that lives *in that namespace*, so if the AGC loses the race the finalizer is stranded with nothing left to clear it and the namespace wedges in `Terminating` — past the suite's 3-minute `drainTenantNamespaces` budget, and cleared only by hand (11+ minutes observed on the migration spec, Q585; the same bug hit `E2E_AGC_ScaleSet*` first).
Pools first, gateway second, cluster-scoped objects last:

- **v1** — `utils.DeleteActionsGatewayCR` is sufficient on its own.
  The v1 gateway's `reconcileDelete` deletes the tenant's `RunnerGroup`s and requeues until they are gone *before* it removes the AGC Deployment, so the pools drain while their controller is still up.
- **v2** — delete the `RunnerSet`s explicitly first.
  The v2 gateway's `reconcileDelete` deliberately does **not** delete them (they reference the gateway but are not owned by it, and degrade to `Ready=False/GatewayNotFound` instead), so nothing else drains them while the AGC still exists.
  Deregistration also needs a live token, so the AGC's NetworkPolicies must still be in place — another reason the gateway goes second.
- **`EgressProxy`** needs no explicit delete: it carries no finalizer (§H.8) and is reclaimed by the namespace cascade.

A migration spec has both a v1 and a v2 tenant in one namespace and drains both.
Cluster-scoped objects (`ClusterRunnerTemplate`, the per-gateway `ClusterRoleBinding`) survive namespace deletion entirely and are reclaimed last, by provenance label.

**live-GitHub.** Set `GITHUB_E2E_APP_ID`, `GITHUB_E2E_INSTALLATION_ID`, `GITHUB_E2E_PRIVATE_KEY` (a PEM path or the PEM body), `GITHUB_E2E_ORG`, and `GITHUB_E2E_REPO` in the environment, then run `make e2e SUITE=live-github` (live-GitHub specs skip themselves at runtime when any variable is missing).
The GitHub App key is in the macOS keychain; see the GitHub App reference memory for the retrieval command.

**`SUITE=live-github`, not a bare `make e2e`** — it selects the `github-real` label and raises the suite budget to 90m, and both halves are load-bearing:

- **The rest of the suite cannot run alongside it.** The container's `BeforeAll` strips the GMC's `AGC_EXTRA_*` fakegithub overrides cluster-wide and holds them off until its `AfterAll`, so any fakegithub-backed spec that stands up a tenant in that window gets an AGC pointed at real GitHub.
  It never registers a session and times out after 4 minutes on `no live session for this RunnerGroup` — a signature that reads as a defect in the spec rather than as contention.
  Measured 2026-08-03: five specs failed exactly that way in one full-suite run, with the GMC confirmed carrying `AGC_EXTRA_GITHUB_ORG_URL` alone at the time.
- **30m does not fit the container.** It is `Ordered`, so its specs are serial, and two of them wait out GitHub's ~10-minute post-eviction conclusion. `--timeout` is a whole-suite budget: Ginkgo interrupts whatever is running and skips the rest, so an under-set value surfaces as a failure in spec N and silence about N+1.
  Measured 2026-08-03: a 30m run was interrupted in the sixth of seven live specs and the seventh never ran.
  Override further with `E2E_TIMEOUT=<dur>` if specs are added.

**A live-GitHub spec identifies its worker by the run-id annotation and nothing else.** `runningWorkerForRun` used to fall back to "the sole Running worker that was not there before this spec dispatched", from when Q495 left the annotation absent and freshness was all there was.
That fallback resolves the *wrong* pod once specs trigger re-runs, which they all now do: an earlier spec's second attempt provisions a worker mid-spec, which makes someone else's pod look fresh.
Measured 2026-08-03 — the cancel-path spec dispatched run `30856065695` and was handed a worker annotated `30856024324`.
The fallback is gone; a spec now waits for its own annotated worker and its diagnostic separates "no run-id at all" (the Q495 regression) from "annotated for another run" (keep waiting).

Read the outcome from `E2E_EXIT`/the `Ran N of M Specs` line, never from the shell's status alone: `make e2e … | tee` reports `tee`'s 0 while the suite fails, and `FAIL! -- Suite Timeout Elapsed` is a budget problem wearing a failure's clothes.

**Run live-GitHub on a throwaway cluster, not the shared `actions-gateway-e2e` one.** The live-GitHub container swaps the GMC's GitHub env vars cluster-wide and holds them for the length of the run, and that `kubectl set env` is itself what makes a later `helm upgrade` conflict on server-side-apply field ownership.
A parallel session reinstalling the chart underneath it (observed 2026-07-29) invalidates the run either way.
Create one per run — `make e2e-cluster KIND_CLUSTER=<name>` shares the existing local registry, so no image rebuild is needed — and point the run at it with a private `KUBECONFIG` (`kind get kubeconfig --name <name>`) rather than the ambient context, which every other session shares.
A private `KUBECONFIG` is not optional either: the suite's own `kubectl` calls carry no `--context`, so they follow `current-context` in the file every parallel session shares.

**live-GitHub is a singleton, enforced at suite start.** A throwaway cluster per run removes only half the collision.
The other half is on GitHub's side: two concurrent runs dispatch the same fixture workflows in the same repo and register identically-named runners, and because runner names are unique per registration scope, the second registration *deletes the first*.
That is `agentpool`'s conflict path — the correct repair when an AGC restarts, and mutual sabotage when two live runs take turns applying it to each other.
Neither side errors; the only symptom is a job that never gets a worker.
Diagnosing that from inside one of the two runs cost ~2.5 h (Q511).

The suite's `BeforeAll` therefore refuses to start — before it swaps the GMC's env vars cluster-wide — while the fixture repo holds a runner it owns or a workflow run that has not completed.
The failure names what it found and both remedies, because it cannot tell a live peer from a killed run's leftovers.
One live-GitHub run at a time, across all worktrees, each on its own cluster.

**Read a long run's clock from the suite, not from the host.** A live-GitHub run takes long enough to straddle a laptop sleep, and afterwards every host-side clock lies in the same direction: `ps` elapsed, pod `AGE`, and "no output for a while" all read as a stall.
On 2026-07-29 a four-spec run finished its specs in 19m04s inside a `ginkgo` process that had been alive 94 minutes, with the tenant namespace still up 75 minutes after the last spec ended — nothing had hung.
The suite's own numbers are measured from its own clock and stay correct across a sleep: `Ran N of M Specs`, and the per-spec `• [N seconds]` line.
Check those before concluding a spec is wedged.

The other half of the same trap is time**zone**, not wall clock: Ginkgo timestamps its output in **local** time while the GitHub API returns **UTC**, so comparing a `@ 07/29/26 06:20:20` progress line against a `created_at` of `13:20:19Z` manufactures a seven-hour anomaly out of a one-second one.
Convert before you reason about a latency that spans both.

**Stop a live-GitHub run with SIGTERM, never `kill -9`.** Ginkgo runs its `AfterAll` on SIGTERM, which deletes the `ActionsGateway` CR while the tenant's AGC is still up — the only window in which the `agentpool-cleanup` finalizer can deregister that tenant's runners from the org.
Kill the process outright and the namespace wedges in `Terminating` on a finalizer whose controller has already gone with it, and force-removing that finalizer strands the runner registrations: they keep accepting job assignments, so the *next* run's job goes `in_progress` against a runner that no longer exists and no worker pod is ever provisioned (observed 2026-07-29).

If it has already happened, clear the GitHub side with:

```bash
make e2e-github-cleanup
```

It deregisters the suite's runners from the fixture repo and cancels any run still in flight there — a job assigned to a runner that no longer exists never completes on its own, and the preflight blocks on it either way.
It reads `GITHUB_E2E_ORG`/`GITHUB_E2E_REPO`, confirms before acting, and takes `ARGS='--dry-run'` to report without changing anything.
It is destructive against real GitHub and cannot distinguish a peer session's live run from wreckage, so confirm no live-GitHub run is in flight anywhere first.

The live-GitHub tier is the only one that hands the harness a **live** App key — every other tier stamps the same Secret with a throwaway RSA key. `utils.CreateGitHubAppSecret` therefore routes the PEM through a `0600` temp file and `--from-file`, per [the credential rule](github-app-credentials.md#creating-the-kubernetes-secret).
Never switch it back to `--from-literal`: `utils.Run` echoes each command's argv to the `GinkgoWriter` and folds it into the failure message, so a literal PEM would land in the run log, the JUnit report, and any `ps` snapshot taken mid-run (Q493).

### The credential-gated probe scenarios

Some questions are about GitHub's behaviour rather than ours, and no tier that runs against a double can answer them — a stub answers with whatever we assumed.
Those live in the `cmd/probe` binary as numbered investigations, each selected by an environment variable and each documented by the plan doc it exists to settle.
They are operator-run, never CI-run: they need live App credentials and, in one case, hours of wall clock.

| Scenario | Selector | Question | Plan doc |
|---|---|---|---|
| Investigation E | `PROBE_SCALESET_TEST=true` | The scale-set wire protocol end to end — auth chain, queue/message semantics, the acquire route matrix, rate-limit headers. `PROBE_SCALESET_JOB_TEST=true` adds the live-job arm that also verifies run identity on a real `JobAssigned` (Q417). | [q264-scale-set-protocol.md](../plan/q264-scale-set-protocol.md) |
| Investigation F | `PROBE_RETENTION_TEST=arm\|check\|cleanup` | Does GitHub redeliver an unacknowledged `JobCompleted` to a session created after a multi-hour gap with no session at all? The Q435 replay path depends on it and the contract does not cover it. **Answered 2026-07-29: yes at a 13 h gap** — re-arm it against a future GitHub rather than trusting the number indefinitely. | [q468-jobcompleted-retention.md](../plan/archive/q468-jobcompleted-retention.md) |

| Investigation G | `PROBE_REPLAY_TEST=true` | Does a **cursor-acked but undeleted** message replay to a fresh session polling from cursor 0, and does `DeleteMessage` stop it?
The Q583 fix rests on both, and the `DeleteMessage` wire shape is the P2-surfaced P4 unknown Q264 left open. | [q583-restart-replay.md](../plan/archive/q583-restart-replay.md) | | Investigation H | `PROBE_ABANDONED_TEST=true` | What does the run service do with a completion for an acquired-but-never-run assignment? **Answered 2026-08-04 across four arms** (`PROBE_ABANDONED_RESULT` selects; `none` sends nothing): `abandoned` and `canceled` conclude the run `success` in one second (a false green), `failed` is refused 401, and silence gets an honest run+job `cancelled` at the ~15-minute unstarted-job horizon — so the listener reports nothing (Q676). `PROBE_ABANDONED_FORCECANCEL=true` adds the Q683 remedy arm, **answered 2026-08-05: a standalone REST force-cancel in the told-nothing state concludes run and job `cancelled` in ~1 s and unpins the runner record**, so the provisioner now ships it. `PROBE_ABANDONED_RERUN_CHECK=true` adds a `rerun-failed-jobs` measurement after a concluded-run verdict. | [q645-abandoned-completion.md](../plan/q645-abandoned-completion.md) |

Investigation G is one run, three session generations, so it needs no state file and no multi-hour gap.
Its `DeleteMessage` verdict turns on **whether the wire deleted anything**, not on the client's error: a 404/410 completes an ack (for a listener, a message already gone is nothing left to do) but deletes nothing, and a backend that does not serve the endpoint answers 404 too. `Client.DeleteMessage` returns that distinction as its first result (Q609); the response status is recorded alongside the verdict as the evidence it rests on.

Investigation F is three phases around a state file rather than one run, because the gap it measures has to pass with **no session in existence** — so it must outlive the process, and the experiment lives on disk.
Its `arm` phase leaves the message under test deliberately unacknowledged; do not "tidy up" by acknowledging it, and do not leave a session behind between phases, or the next gap measures something shorter than it claims.

All four register against the repo, not the org (E/F/G a scale set, H two plain JIT runners on the classic broker v2 flow the AGC's listener ships): this repo is public and the org's `Default` runner group sets `allows_public_repositories: false`, so an org-scoped registration never receives the job.
Each has a dispatch-only fixture workflow ([`scaleset-probe.yml`](../../.github/workflows/scaleset-probe.yml), [`q468-retention-probe.yml`](../../.github/workflows/q468-retention-probe.yml), [`q583-replay-probe.yml`](../../.github/workflows/q583-replay-probe.yml), [`q645-abandoned-probe.yml`](../../.github/workflows/q645-abandoned-probe.yml)) that queues jobs on its label and never runs in normal CI.
Dispatch the fixture *before* starting the probe — a job queued against a not-yet-registered label waits server-side and is assigned the moment the scale set appears.

**A probe's absence verdict must reconcile every level of the observable.** "Nothing happened" is the strongest claim a probe can print, and it holds only if every record the outcome could land on was watched.
GitHub splits job state across records that can disagree: Investigation H's first live run watched the fixture *job*, which stayed `in_progress` indefinitely, and printed `NO-SIGNAL` while the *run* had concluded `success` twenty minutes earlier; only a post-run read of the run endpoint caught the real answer ([the recorded artifact](../plan/q645-abandoned-completion.md#findings)).
Before trusting a quiet window, enumerate the records the question spans (the run and its jobs at minimum) and watch them all.
This is the multi-record sibling of the empty-output rule in [Diagnosing failures](#diagnosing-failures-measure-before-asserting-a-root-cause): silence is evidence of absence only once every place the signal could appear has been read.

The App private key stays in the macOS keychain and reaches the probe as a **file path**, never as an env-var value or a process argument ([github-app-credentials.md](github-app-credentials.md)).

### The chart uninstall/reinstall gate (Q444/Q492)

Every tier above starts from a cluster that has never had the chart installed, so nothing exercises the day-two operation an operator actually performs: `helm uninstall` followed by a reinstall.
That gap hid **Q444** — an apiserver that stops resolving the PriorityClass guard's parameter and denies **every** `runnergroups`/`runnersets`/`runnertemplates` write cluster-wide.

[`scripts/e2e/chart-reinstall-check.sh`](../../scripts/e2e/chart-reinstall-check.sh) closes it.
It runs against a cluster that already has the release installed, captures the release's own values, cycles `helm uninstall` -> `helm install`, and probes admission — printing a diagnostic dump (policy, binding, `paramRef` target, the param object's existence and UID) that separates a broken manifest from a broken apiserver.

```bash
make chart-reinstall-check KIND_CLUSTER=actions-gateway-e2e
```

It ran as a **reproducer, not a gate**, while Q444 was open — the defect was unfixed, so wiring it in would have pinned every run red. **Q492 made it a gate**: the guard's `paramKind` moved from a `ConfigMap` to the cluster-scoped `PriorityClassAllowlist` CRD, for which the apiserver allocates a fresh dynamic informer per context rather than tearing down a shared one, so the cycle is now expected to pass. `e2e-reusable.yml` runs it after the Ginkgo suite and after the `helm upgrade` gate — `E2E_SKIP_TEARDOWN` leaves the release up, and this step destroys and recreates it, so only the [released-chart upgrade gate](#the-released-chart-upgrade-gate-q507) (which replaces the release entirely) may follow it.
Kindnet only (`e2e-calico.yml` passes `chart_reinstall_check: false`): the property is a kube-apiserver informer behaviour, not a CNI one.

**What it protects.** A regression to any core-type `paramKind` — in this policy or a new one — brings the outage back.
This is where that surfaces.

Note the historical caveat, which still shapes how to read a pass on an old release: in the ConfigMap era this check could pass *while* broken, because a torn-down informer's frozen cache could still answer for an object that no longer existed.
That ambiguity is why the deterministic reproducer below exists alongside it.

#### The deterministic reproducer

For the apiserver defect itself, use [`scripts/e2e/vap-param-informer-check.sh`](../../scripts/e2e/vap-param-informer-check.sh) — no chart, no product CRDs, three arms on one apiserver:

| arm | `paramKind` | binding set | expected |
|---|---|---|---|
| 1 | ConfigMap | a second binding held throughout | `FRESH-PARAM` |
| 3 | cluster-scoped CRD | emptied for the gap | `FRESH-PARAM` — the Q492 shape |
| 2 | ConfigMap | emptied for the gap | `STALE-PARAM` or `NO-PARAMS` |

Arm 1 vs arm 2 isolates the trigger to the empty-set transition. **Arm 3 vs arm 2** — the byte-identical transition one GVK apart — is the measured evidence that moving the `paramKind` to a CRD is a fix rather than an inference from reading the apiserver source.
Arm 3 runs before arm 2 so a pass cannot be attributed to contamination.

```bash
KUBE_CONTEXT=kind-q444-lab scripts/e2e/vap-param-informer-check.sh
```

**Disposable clusters only** — arm 2 permanently breaks ConfigMap param resolution for the target apiserver process, and the script refuses a non-`kind-` context unless `ALLOW_NON_KIND=1`.
It stays out of CI for that reason, and because it pins an *upstream* defect our own code no longer depends on; re-run it by hand on a new Kubernetes minor to confirm arm 3 still holds.
Mechanism and measurements: [`q444-vap-param-resolution.md`](../plan/archive/q444-vap-param-resolution.md).

### The chart `helm upgrade` gate (Q475)

The other half of the same gap. `make deploy` runs `helm upgrade --install`, but only ever against a cluster with no prior release, so the `--install` half was the only half any tier exercised — day-2 upgrade over a **live** release was untested.

What rests on it: the chart ships its CRDs under `templates/crds/` rather than the chart-root `crds/` directory **because Helm installs `crds/` once and never upgrades it**.
Move them, and every existing installation silently stops receiving CRD field changes — a failure with no error message and no symptom until a tenant sets a field the apiserver then prunes.

[`scripts/e2e/chart-upgrade-check.sh`](../../scripts/e2e/chart-upgrade-check.sh) closes it.
Against a cluster that already has the release installed, it captures the release's own values and upgrades to a copy of the chart that differs in exactly two deliberate ways, then upgrades back:

| Injected change | What its arrival proves |
|---|---|
| A new optional property on the RunnerGroup v1alpha1 spec schema | `helm upgrade` delivers CRD field changes to an existing install. Asserted end-to-end — a RunnerGroup carrying the field is pruned before the upgrade and round-trips after — so it fails closed if the CRDs ever move to `crds/`, and also if the CRD object updates but the served schema does not. |
| A pod-template annotation on the GMC Deployment | An ordinary template change reaches an existing release, and the manager rollout it forces comes back healthy. |

It also asserts a pre-existing RunnerGroup survives with its **UID** intact (an upgrade that recreated CRs would destroy tenant state), that the validating webhook still denies an `ActionsGateway` in a reserved namespace at every step, and that upgrading back to the real chart **removes** both markers — so the cluster is left as it was found.

```bash
make chart-upgrade-check KIND_CLUSTER=actions-gateway-e2e
```

Like the reinstall gate above, this one is wired into CI: `e2e-reusable.yml` runs it after the Ginkgo suite, which leaves the release up under `E2E_SKIP_TEARDOWN`, and before the reinstall gate (which destroys the release).
It runs only on the kindnet leg — `helm upgrade` semantics are CNI-independent, so `e2e-calico.yml` passes `chart_upgrade_check: false` and the calico leg pays nothing for it.
The step is `if: success()`: on a suite failure the diagnostic dump matters more, and mutating the release first would destroy it.

The script fails loudly rather than silently injecting nothing if the controller-gen CRD layout or the Deployment's anchor annotation ever moves — the error names the awk anchors to re-point.

### The released-chart upgrade gate (Q507)

The upgrade gate above answers "does HEAD's chart upgrade to a copy of itself?" — never "does the chart an operator is actually **running** upgrade to HEAD?".
Those differ whenever a change interacts with what Helm does *between* two chart versions, and **Q492** proved the gap: shipping the `PriorityClassAllowlist` CRD in the chart-root `crds/` dir (the only placement Helm applies early enough for a CR in the same release) broke **every** v1.2.0 upgrade with Helm's bare `ensure CRDs are installed first` — while `make check`, the full e2e suite, and both chart gates stayed green, because nothing in CI had an older release to upgrade from.
It was caught in review, not by a gate.

[`scripts/e2e/chart-released-upgrade-check.sh`](../../scripts/e2e/chart-released-upgrade-check.sh) closes it.
Against a cluster that already has the release installed, it replaces the live release with the **last released chart — the published OCI artifact operators actually install, pulled from GHCR, not a rebuild from an old git ref** (a packaging difference between chart source and published artifact is exactly the kind of escape this gate exists to catch) — and walks the operator's upgrade path to HEAD:

| Step | What it asserts |
|---|---|
| Upgrade with a values key HEAD removed (today: `priorityClassAllowlist.configMapName`) | Fails **at render** with the migration message pointing at `docs/operations/upgrade.md` — never midway through applying, and never silently accepted. The probe is anchored on the guard still existing in HEAD's templates, so retiring the guard after its deprecation window retires the probe with it. |
| Plain `helm upgrade` to HEAD, no preparation | Succeeds outright, **or** fails at render with a message naming the documented pre-upgrade step. Any other failure — above all the bare `ensure CRDs are installed first` — is the Q492 shape reintroduced, and fails the gate with remediation guidance. |
| The documented step (`helm show crds <chart> \| kubectl apply -f -`), then the upgrade | Must succeed; then every CRD HEAD ships in `crds/` must exist in the cluster (an upgrade can *succeed* while silently never delivering one), the restarted manager must come back, the validating webhook must enforce, and the PriorityClass guard's params must resolve. |
| An upgrade with the PriorityClass allowlists **set** rather than defaulted (Q646) | Reaches the CRD-**schema** preflight: with a stored CRD that predates `allowedInfraPriorityClasses`, it must fail at render naming the field and `docs/operations/upgrade.md`, without advancing the revision; after the documented step the same upgrade must succeed **and both lists must round-trip into the `PriorityClassAllowlist` CR**. Doubly anchored — on HEAD still carrying the guard, and on the stored CRD actually being stale — so a release that ships the field skips the negative half instead of reddening. |

**Why the values-set step exists.** Every step above replays the release's own values, which for a stock install are the chart defaults — so any preflight gated on a value being **set** was dead code in CI.
Measured on kind against the published v1.3.0 chart, the default-values upgrade reaches **none** of the three guards in [`priorityclass-allowlist.yaml`](../../charts/actions-gateway/templates/priorityclass-allowlist.yaml): the removed-key guard has its own dedicated probe, the CRD-kind-absent preflight does not fire because v1.3.0 already ships the CRD (which also makes the documented-step branch above unreachable today), and Q298's schema guard had no coverage at all — it shipped verified by hand.
The generalisation for chart authors: **a preflight gated on a non-default value needs a step that sets it**, because the gate's default path renders as if the field did not exist.

The consequence for chart authors: **a deliberately upgrade-blocking change must fail at render with a message that names the pre-upgrade step and points at [`docs/operations/upgrade.md`](../operations/upgrade.md)** — the preflight in [`priorityclass-allowlist.yaml`](../../charts/actions-gateway/templates/priorityclass-allowlist.yaml) is the pattern — because those message anchors ("pre-upgrade step", the doc path) are what this gate accepts as a documented failure.

**How "last released" is discovered.** The highest stable `vX.Y.Z` tag on the origin remote (`git ls-remote`; prerelease tags are excluded), which is the tag `publish.yml` keys the chart's OCI version on (`v` stripped).
A new release re-points the gate automatically — nothing to bump.
Two deliberate consequences: a stable tag whose chart publish failed **fails this gate loudly** at `helm pull` (operators can't install that release either — fix the publish, see [release.md](../operations/release.md)); and a repo with no stable tag yet (a fresh fork) **skips cleanly**.
The chart package and the released GMC image must be publicly pullable from GHCR (they are — see release.md's one-time setup); both pulls are retried.
Override `RELEASED_TAG` / `RELEASED_CHART_OCI` to test a specific tag or registry.

```bash
make chart-released-upgrade-check KIND_CLUSTER=actions-gateway-e2e
```

Wired into `e2e-reusable.yml` **last** among the chart checks — it uninstalls the live release and leaves the cluster on a fresh released→HEAD release, which would invalidate the other checks' baseline.
Kindnet leg only (`e2e-calico.yml` passes `released_upgrade_check: false`): the property is pure Helm semantics, CNI-independent.

## CI workflows and scripts

CI must use the same commands as [Running tests](#running-tests) above — per-module invocations or the explicit multi-module patterns `scripts/go/go-test.sh` builds; never `go test ./...` from the repo root, which does not work with the Go workspace layout.

### Pinned tool installs: always via `download-verified.sh`

Every CI step that installs a pinned third-party binary — kind (`e2e-reusable.yml`, `autoscaler-drift.yml`), shellcheck (`unit-test.yml`), kubeconform (`manifest-validate.yml`), polaris (`security-scan.yml`) — and the local `$(COSIGN)` rule fetch it through [`scripts/fetch/download-verified.sh`](../../scripts/fetch/download-verified.sh) (`<url> <sha256> <output-path>`).
Do not hand-roll `curl` + `sha256sum -c` in a new step; use the script, and keep the version and its digest pinned side by side in the workflow `env:` block.

The script exists because both halves of that fetch are easy to get subtly wrong:

- **Retry.** `curl --retry` covers 408/429/5xx and connection failures **only**, so a GitHub releases-CDN **403** — the denial the CDN actually serves under load — fails the download instantly, in well under a second, with `curl: (22)`.
  That reddened a whole PR run via `security-scan-gate` (Q433, PR #828). `--retry-all-errors` widens the retry to any error, including that 403; the script always passes it (`DOWNLOAD_RETRIES`/`DOWNLOAD_RETRY_DELAY`, default 5×2s).
- **Integrity.** GitHub release assets are mutable for an existing tag, so the bytes must be checked against a pinned digest (Q126/Q127).
  The digest is a required argument, the download lands in a temp file, and the output path is written only after the digest matches — there is no flag or environment variable that skips the check.
  [`scripts/fetch/download-verified-test.sh`](../../scripts/fetch/download-verified-test.sh) (under `make scripts-test`) asserts that: a mismatch fails and leaves nothing at the output path, a malformed digest is rejected outright, and the `curl` line still carries `--retry-all-errors`.

### Registry pulls: always via `pull-image-with-retry.sh`

`docker pull` has no retry of its own, so every CI step that pulls an external image goes through [`scripts/fetch/pull-image-with-retry.sh`](../../scripts/fetch/pull-image-with-retry.sh) (`<image-ref>`) — the buildkit builder pre-pull in `e2e-reusable.yml`, `security-scan.yml` and `publish.yml`, the curl and Vault mirror steps, and [`prepull-manifest-images.sh`](../../scripts/fetch/prepull-manifest-images.sh).
Do not hand-roll a bare `docker pull` in a new step.

**The backoff is exponential and jittered, not fixed (Q460).** `PULL_RETRY_ATTEMPTS`/`PULL_RETRY_DELAY`/`PULL_RETRY_MAX_DELAY` default to 6 attempts, a 5s base doubling to a 60s cap, plus up to 50% jitter on every sleep — 135–202s of backoff, ~5 minutes worst case including the attempts themselves.
The former fixed 5×5s schedule failed twice over on #895: it exhausted the whole budget in ~95s, shorter than the Docker Hub brown-out that caused it, and it retried the six `trivy` matrix shards **in lockstep**, landing six synchronised requests per round on an IP-shared anonymous rate limit.
Jitter is what de-correlates concurrent callers; the cap and the finite attempt count are what keep a genuinely unreachable registry a clear, bounded failure rather than a hang.
[`scripts/fetch/pull-image-with-retry-test.sh`](../../scripts/fetch/pull-image-with-retry-test.sh) (under `make scripts-test`) asserts the schedule: the doubling, the cap, that the jitter actually varies, and that the total budget stays inside 300s.

**Prefer removing the pull outright where a cache can.** [`prepull-image-cached.sh`](../../scripts/fetch/prepull-image-cached.sh) (`<image-ref> <cache-dir> <local-tag>`) is the single-image sibling of `prepull-manifest-images.sh`: restore the image from an `actions/cache` tarball, or pull it once (retried) and save it.
The `trivy` job uses it because its six shards otherwise pull the same ~200 MB builder image concurrently on every run; the single-pull sites (e2e, publish) keep the plain retried pull.

One constraint drives its shape, and it is easy to get wrong: **`docker load` cannot restore a manifest digest.** A saved-and-reloaded image comes back with its `RepoTags` but no `RepoDigests`, so a digest-pinned `name:tag@sha256:…` ref never resolves from the cache and the consumer silently pulls from the registry anyway — a cache that looks like it works and does nothing.
The cached image is therefore re-exposed under a local-only tag (`BUILDKIT_LOCAL_IMAGE`) and the consumer points at that.
This does not weaken the pin: the cold-path pull is still digest-verified against the pinned ref, and the cache key carries that same digest, so the local tag can only ever name the bytes the pin selected.

### Path-gated workflows: verify the heavy gates actually ran

Most code-exercising workflows keep unrelated PRs cheap by **skipping their expensive jobs internally** rather than skipping the whole workflow.
The build/lint/test/security gates (`unit-test.yml`, `integration-test.yml`, `e2e-test.yml`, `e2e-calico.yml`, `security-scan.yml` — trivy + govulncheck, `manifest-validate.yml`, `license-notices.yml`, plus `status-lint.yml`, `plan-hygiene.yml` and `autoscaler-drift.yml`) trigger on **every** `pull_request` (no top-level path filter), then a `dorny/paths-filter` `changes` job classifies the diff and each real job's `if:` guard skips it when nothing it covers changed.
Each workflow ends with a small **`<workflow>-gate`** job (`unit-test-gate`, `security-scan-gate`, …; `if: always()`, `needs:` every real job) that passes only when each concluded `success` or `skipped` — this is the job whose check context is (or is intended to be) the branch's **required status check**.
The ids are unique per workflow on purpose: a normal job's check-run name **is its job id**, GitHub matches required checks by that name, so nine jobs all named `gate` would collapse to one indistinguishable entry in the ruleset UI.
See [required-status-checks.md](../plan/archive/required-status-checks.md).

**Why not the simpler top-level `paths-ignore`:** a workflow skipped by a top-level path filter reports **no check at all**, which leaves a *required* check **Pending forever** and wedges the merge.
Triggering on every PR and gating internally means the `gate` context always reports — green (all jobs skipped) on an unrelated PR, red when a real job fails — so it is safe to require.

**The same pattern serves the merge queue's `merge_group` event.** The nine workflows behind required checks also trigger on `merge_group`, so their gate contexts report on the queue's candidate merge commit.
That is the queue analogue of the Pending-wedge above: a required check that never reports on the merge-group ref stalls the entry until `check_response_timeout_minutes` expires it.
The `changes` job needs no per-event configuration: on `merge_group`, paths-filter's `base`/`ref` default to the event's commit hashes and detection runs via git against the checkout, so a docs-only queue entry skips the heavy legs exactly as a docs-only PR does.
The queue is active on `main` (2026-08-03, `merge_queue` rule in the `default-protect` ruleset); [merge-queue.md](../plan/merge-queue.md) records the parameters, the rollback, and the activation-ordering constraint it satisfied.

**The historical gotcha this closes — a PR going green/`CLEAN` without ever testing its code.** Under the old top-level `paths-ignore`, a PR **opened while docs-only** with code **added in a later push** could leave the path-gated workflows **skipped** (the `synchronize` did not reliably re-trigger them; see [actions/runner#2324](https://github.com/actions/runner/issues/2324)), so the PR showed all-green with the code never built or tested.
Because the workflows above now trigger on every PR and re-evaluate the diff via the `changes` job on each push, this specific skip-through no longer applies to them.
The `gate` contexts are now marked required in the ruleset, so a red or Pending gate blocks the merge.

**The `changes` job fails open (Q363).** `dorny/paths-filter` resolves the changed-file list through a **single un-retried GitHub API call** — `@actions/github`'s Octokit carries no retry plugin — so one transient 5xx or reset used to fail the `changes` job and, through `needs:`, the whole gate.
Worse, a JavaScript action reports failure *only* as an `::error::` annotation, and a rerun replaces the check run and destroys that annotation: the surviving log simply stopped after `Invoking listFiles` with no error recorded anywhere, which is why the original occurrence looked like a silent failure.
Each `changes` job now carries `continue-on-error: true` on the paths-filter step and derives its outputs as `steps.filter.outcome == 'success' && steps.filter.outputs.<name> || 'true'`, so an unclassifiable diff **runs every gated job** instead of skipping it — fail-open on *detection*, still fail-closed on *validation*.
A step that trips it also emits a `::warning::` so the degradation is visible in the run log rather than inferred.
Note this is deliberately not a retry: re-running the classifier could still return a wrong answer, whereas running the gated jobs is always safe.

**Adding a Go module means editing every filter that covers it (Q400).** The filters are hand-maintained lists, and a module absent from a gate that does compile it makes that gate stay green by *skipping*, not by passing.
Both modules added after the initial filters were written hit this: `api/` and `scaleset/` were in `unit-test.yml` but in none of `integration-test.yml`, `security-scan.yml`, or `e2e-test.yml`, so an `api`- or `scaleset`-only change skipped the envtest tier (whose AGC ScaleSet suite imports `scaleset/scalesettest` directly), govulncheck, trivy, and e2e. `manifest-validate.yml` had the same gap for the five v2 CRDs under `api/config/crd/`, which `scripts/manifest/manifest-validate.sh` validates by name.

The whole-workspace half of that is now mechanical: the [path-filter gate](#the-path-filter-gate) (`make path-filters-check`, in `make check` and CI) fails when a `go.work` module is missing from a filter whose jobs exercise the whole workspace, when a filter is not classified, or when a pattern points at a path that no longer exists.
The judgement half is not automatable and remains yours: for the deliberately narrow filters, walk every `filters:` block in `.github/workflows/` and ask what that gate actually compiles, scans, or bakes — not what its filter happens to list today.
The same applies in reverse to a gate that names files individually (`manifest-validate.sh`'s `standalone_manifests`): adding a path there means adding its directory to the filter.

**Verify before declaring a PR review-ready and before merging it:** confirm the gates that exercise the change actually executed **on the PR's head commit** — green is not enough if a gate was skipped, and *no red checks* is not the same as *the checks ran*.
For any Go / CRD / chart change you should see runs for `build`, `lint`, `integration-test`, `e2e`, `security-scan` (trivy + govulncheck), and `manifest-validate`:

```bash
gh pr view <n> --json headRefOid --jq .headRefOid   # the SHA every check must be attached to
gh pr checks <n>                                    # are the expected gates PRESENT — not merely un-red?
gh run list --commit <sha> --limit 30               # which workflows actually ran, for THAT commit only
```

Read the output as a **checklist against the expected set above**, not as a pass/fail summary.
A PR with zero rows, or with only the lightweight docs workflows listed, has not been tested — it just has nothing to fail.

**Filter the runs by commit, not by branch.** `gh run list --branch <branch>` lists every run the branch has ever had, so after a rebase heal and force-push it still shows the superseded runs, and a success from the pre-rebase head reads as a success on the code you are about to merge. `--commit <sha>` is the flag that answers the question asked.
Do not hand-roll the equivalent as a `--jq` filter on `headSha`: a malformed expression matches everything and exits 0, which produces a confident, wrong "the heavy gates ran".

**Check at the sentinel's `ready` wake, not straight after opening the PR.** For the first minute or so every check is `pending` or `queued`, which looks identical to the gates being absent and invites a `sleep`-then-recheck loop, which [Never foreground-poll CI, logs, or files](#never-foreground-poll-ci-logs-or-files) forbids and foreground-guard blocks.
The watcher's `ready` event already means checks concluded, so it is both the earliest and the cheapest moment for this verification.

**A skipped job does not mean its steps did not run — resolve the step's owning job, not its file.** `gh pr checks` reports *job* names, and a `make` target's home is a job, not a workflow file: grepping `unit-test.yml` for `scripts-test` finds it, but that step lives in the same file's **`shellcheck`** job, so a skipped `unit-test` says nothing about whether the scripts tests ran.
Reading the workflow source to answer "did my test run?" gives the wrong answer in exactly the case you are checking.
Resolve it from the run:

```bash
gh run view <run-id> --json jobs --jq '.jobs[] | [.name, .conclusion] | @tsv'   # which jobs, which verdict
gh run view <run-id> --log --job <job-id> | grep '<an assertion you expect>'    # did it actually execute
```

The grepped log line is the proof the assertion ran; the job's conclusion only proves nothing in it failed — and a job that ran nothing also fails nothing.

**Absence of checks is not green (Q383).** There are two distinct ways a PR ends up under-tested, and they need different fixes:

- **Gates skipped by the diff classifier** — the workflow ran, its `changes` job classified the diff as irrelevant, and the real jobs were skipped. `gh pr checks` shows the `gate` contexts as green.
  Confirm against the expected-gate list above; if a gate that should cover the change is missing, the classifier's path filters are wrong.
- **No workflow runs attached at all** — the push registered, but GitHub never dispatched any workflow for the head SHA.
  Observed on this repo for **~10 minutes** after a push: `gh run list --branch <branch>` returned nothing for that commit while the PR page showed no checks section whatsoever.
  Nothing is red, so the PR reads as clean at a glance; in fact nothing has run.

**Fix for both:** `gh pr close <n> && gh pr reopen <n>`.
The `reopened` event re-dispatches the workflows and re-evaluates the path filters against the full PR diff.
Then re-verify with the commands above (asynchronously — see [Never foreground-poll CI, logs, or files](#never-foreground-poll-ci-logs-or-files)) and confirm the expected gates are now present and concluded before treating the PR as tested.

### A focused run is a path filter you applied yourself

The rule above is about a gate that narrowed the work for you. `--focus` is the same green with the filter written by hand: `ginkgo run --focus '<spec>'` is the correct inner loop for iterating on a new spec ([kind-iteration.md § Tightening the inner loop](kind-iteration.md#tightening-the-inner-loop)) and it is not a verification, because it reports on the specs you named and stays silent about every sibling you excluded.

**Run a new spec's whole family once before calling it done** — the family being whatever shares mutable state with it.
In the e2e suite that state is the cluster, and the sharpest instance of it is the GMC's `AGC_EXTRA_*` env: it is set deployment-wide and reaches every tenant AGC, so a spec that bends it to reach a double bends it for all its siblings too.
A focused run cannot observe that, and neither can the sibling's own focused run — both are green, and only a run holding both is not.

Q528 is the worked example. `E2E_AGC_ScaleSetAcquisition` needed the AGC's scale-set endpoints re-pointed at the in-cluster stub, and the first form rewrote them for **every** gateway whenever the `STUB_AUTH_URL`/`STUB_BROKER_URL` pair was set.
Its sibling `E2E_AGC_ScaleSetRecovery` depends on a listener that stays *down*: its gateway named fakegithub's plaintext port over `https` so the bootstrap would die on the TLS handshake — precisely the scheme the new rewrite swapped.
The new spec was green on three consecutive focused runs against a local kind cluster; the sibling went red in CI on `scale-set listener active`.
The repair was to narrow the rewrite to gateways already naming the stub's `host:port`, and to move the recovery spec onto a host that does not resolve, which no rewrite can re-point.

The reviewer's tell: a PR whose "how it was tested" names only the specs it added.

### The e2e workflows: kindnet and Calico

The cluster/image/test plumbing for the e2e suite lives in one reusable workflow, [`.github/workflows/e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`workflow_call`, parameterized by a `kind_cni` input).
Two callers drive it so a kind bump, image-tag change, or flake mitigation is made once and both lanes inherit it:

- **[`e2e-test.yml`](../../.github/workflows/e2e-test.yml)** — the per-PR / push-to-main leg, `kind_cni: kindnet`.
  Path-gated (skips PRs touching no e2e-relevant files) and `cancel-in-progress` on PRs.
  This is the merge gate.

**Infrastructure image caching.** External images that the kind *nodes* would otherwise pull on every run — a recurring flake source under registry rate limits and a latency cost on the critical path — are pre-pulled on the runner (cached via `actions/cache`, retried) and seeded onto the nodes so the in-cluster pull is a local hit.
This covers, on **both** legs, the `curlimages/curl` test image (mirrored into the local registry), the **cert-manager** controller/webhook/cainjector images (pre-pulled + `kind load`ed before `make apply-cert-manager`, whose rollout is then waited on), and the **metrics-server** image (pre-pulled + `kind load`ed before the suite runs, since the suite applies the pinned `components.yaml` itself in `setupMetricsServer` — Q150); and on the **Calico** lane, the Calico CNI images (see below).
The pinned versions live in the workflow env (`E2E_CURL_UPSTREAM`, `CERTMANAGER_VERSION`, `METRICSSERVER_VERSION`, `CALICO_VERSION`) and must be kept in sync with their source of truth (`CERTMANAGER_VERSION` in `cmd/gmc/Makefile`, `metricsServerVersion` in `cmd/gmc/test/e2e/e2e_suite_test.go`, `CALICO_VERSION` in the root `Makefile`) — bump together.
The first-party images (`gmc`/`agc`/`proxy`/`fakegithub`/`worker`) are built and pushed to the local registry by the bake step, whose layers are cached via buildx `GHA_CACHE`.

All of this runner-side caching is **hosted-lane-only**: on a self-hosted runner the `actions/cache` steps, the bake's `GHA_CACHE`, and `azure/setup-helm` are skipped (`runner.environment` gates in `e2e-reusable.yml`), because the Actions cache data plane and `get.helm.sh` are job-time egress the managed GitHub egress rule does not admit (the Kata untrusted-PR posture keeps that lane's non-registry fetches GitHub-only, Q408).
The pull steps fall back to retried upstream pulls there (helm is baked into the e2e runner image), and the in-cluster registry mirror is what will serve those pulls warm.

#### Runner→GitHub egress attribution (Q352)

A handful of specs deliberately reach the **live** `api.github.com` (see the `real-github-egress` label above), so a transient outage of the CI runner's own GitHub egress kills them with signatures that look like product regressions — observed as a proxy CONNECT 502 (kindnet lane, 2026-07-14) and curl exit-28 timeouts including the proxy-less DirectEgress spec (Calico lane, 2026-07-19), both green on re-run.
Two probes make such blips self-attribute instead of costing a triage:

- **In-suite, at failure time (the authoritative signal):** when a `real-github-egress`-labelled spec fails, a suite-level `AfterEach` immediately issues an HTTPS GET to `api.github.com/zen` from the test process — the runner host, the segment every in-cluster path NATs through — and stamps a `RUNNER-HOST GITHUB PREFLIGHT: <verdict>` banner into the spec's failure output.
  A non-fatal baseline probe logs the same verdict at suite start; it deliberately does **not** fail fast, since a start-time blip may clear before those specs run and a fatal preflight would add a flake surface rather than remove one.
- **In the workflow's failure-diagnostic step:** `e2e-reusable.yml` curls `api.github.com/zen` from the runner alongside the cluster dumps and applies the same table in shell, covering the case where the suite process itself died before the `AfterEach` could report.

**How the probe scores a response.** The probe goes straight from the test process to GitHub — no proxy, no cluster, nothing this repo ships — so its result can never be caused by a product regression.
It shares exactly two things with the in-cluster path: the runner's egress address and the internet between it and GitHub.
Anything that refuses the probe therefore refuses the traffic the failing spec depends on, which is what makes each response decidable ([`ScoreGitHubEgress`](../../cmd/gmc/test/utils/github_egress.go), table-tested in `github_egress_test.go`):

| Probe result | Verdict | What to do |
|---|---|---|
| Transport error (DNS, dial, TLS, timeout) | `BLOCKED` | Nothing answered. Infra — re-run the job. |
| 2xx | `REACHABLE` | GitHub served the runner. Treat the failure as real; inspect the in-cluster path (workload NP → proxy → egress NP → GitHub). |
| 403, 429 | `BLOCKED` | `/zen` carries no credentials and needs none, so a refusal is not about who asked — GitHub is throttling or blocking this source address. The in-cluster path NATs through that same address. Infra — re-run. |
| 408, 5xx | `BLOCKED` | The path reaches GitHub (or an intermediary) but it will not serve the request. Not something this repo can regress. Infra — re-run. |
| Anything else (3xx surviving redirect following, 401, 404, other 4xx) | `INCONCLUSIVE` | GitHub answers an unauthenticated `/zen` with 200 and nothing else, so an intermediary intercepted the request or the endpoint moved. Read the body excerpt in the banner: not GitHub's → interception, infra, re-run; GitHub's → the probe is stale, fix it and triage the spec on its own output. |

A non-2xx banner also carries `x-ratelimit-remaining`, `retry-after`, and a 200-byte body excerpt — GitHub's own words for the refusal.
Those make the banner concrete but never change a verdict.

Scoring `403` as `REACHABLE` was the original rule and the bug behind Q648: on 2026-08-03 the probe stamped `PREFLIGHT: OK (HTTP 403)` three times into a failing run, telling the operator to treat a rate-limited runner as a product regression; the re-run was green.
Getting it wrong the other way is just as costly — a verdict of `BLOCKED` on a genuinely broken spec means re-running it forever — which is why `INCONCLUSIVE` exists and stays narrow: it covers only the responses that say *something other than GitHub* answered, and it names the one artifact (the body) that resolves it.

#### The Calico e2e lane

- **[`e2e-calico.yml`](../../.github/workflows/e2e-calico.yml)** — `kind_cni: calico` (Q119). kindnet accepts `NetworkPolicy` but its bundled enforcer does not drop egress, so the NetworkPolicy-enforcement specs self-skip on the per-PR kindnet leg; on Calico they assert real packet drops.
  The full suite runs on both CNIs — these specs simply activate under Calico: the two `TenantProvisioning` egress negatives (`WorkloadEgressBlockedToNonProxyPod`, `WorkerCannotReachK8sAPI`), `ProxyConnectWorks` (which runs on both but is only truly enforced here), and the two `ManagerMetricsNP` specs (Q83).
  No per-lane spec selection is needed — the suite's runtime `egressEnforcingCNI()` self-skip does the routing.

  **When it runs:** **per-PR (and on push to main) only when the diff touches NetworkPolicy/proxy code** — the GMC (`cmd/gmc/**`, which generates the tenant + manager policies and the proxy), the egress proxy (`cmd/proxy/**`), the chart's policy templates (`charts/actions-gateway/**`), or the CNI/cluster plumbing (`scripts/e2e/**`, `scripts/fetch/**`, `scripts/lib/**`, `Makefile`, the two e2e workflows — the script groups held identical to `e2e-test.yml`'s by [assertion 4](#the-path-filter-gate) of the path-filter gate).
  PRs that cannot regress enforcement stay on the fast kindnet leg and pay no Calico cost.
  The path filter is the *sole* automatic gate (there is no nightly catch-all), so it deliberately errs toward the components that produce or police the enforced traffic. **Trigger it manually** any time from the Actions tab → *e2e (calico)* → *Run workflow* (`workflow_dispatch`).
  Because it triggers on the PR's own files, a change to the lane itself (or to NP/proxy code) is validated on that PR rather than only post-merge.

  **It gates merge via its `gate` job.** The workflow triggers on every PR (no top-level path filter) and skips the expensive Calico leg internally via the `changes` job, so a non-matching PR still reports a green `e2e-calico-gate` — the required-check-safe pattern (see [required-status-checks.md](../plan/archive/required-status-checks.md)).

  **Calico image caching.** The Calico manifest pulls `calico/node`, `calico/cni`, and `calico/kube-controllers` from quay.io/docker.io on every node during install — and those pulls happen *before* the local registry is wired into the nodes, so they cannot be mirrored the way the curl image is.
  Instead the lane pre-pulls the exact image refs the pinned manifest references into the runner's Docker daemon (cached via `actions/cache`, keyed on `CALICO_VERSION`, retried), and `scripts/e2e/kind-with-registry.sh` `kind load`s whatever is present onto the nodes so the rollout never touches quay.io.
  This keeps the per-PR Calico cost bounded and quay.io off the critical path.
  Calico still gets a 60-minute timeout vs. the kindnet leg's 45 for rollout headroom. `CALICO_VERSION` is pinned in both the root `Makefile` and the workflow env — bump them together.

### The Dockerfile-lint gate

[`.github/workflows/dockerfile-lint.yml`](../../.github/workflows/dockerfile-lint.yml) runs `hadolint` over the root `Dockerfile` (one leg; it holds every first-party image as a named stage — `gmc`, `agc`, `proxy`, `worker`, `wrapper`, `fakegithub`) and `scripts/dogfood/runner/Dockerfile` (a dev/reference image, not a shipped one), path-gated on `**/Dockerfile`.
The failure threshold is `style` — the strictest level, which both currently pass clean — so a regression such as an unpinned base tag, a dropped digest pin, or a relaxed non-root `USER` fails at PR time.
It is its own lightweight workflow (like `doc-links.yml` and `status-lint.yml`), so a Dockerfile-only change does not trigger the Go suite.
There is no local `make` target; reproduce a run with `docker run --rm -i hadolint/hadolint hadolint --failure-threshold style - < Dockerfile`.

## Security scanning

The `security-scan.yml` workflow runs three gates on every PR (and on push to `main`), independent of the unit/integration/e2e suites — two supply-chain scans plus a Kubernetes posture scan.
All three have local equivalents so you can reproduce a CI verdict before pushing.

**govulncheck** — scans each workspace module for vulnerabilities reachable from our code (Go stdlib + dependency CVEs).
It is symbol-precise: a CVE in a dependency only fails the gate if our code actually calls the affected path.
Run it locally with:

```
make vulncheck
```

A finding usually means bumping the Go toolchain (`go` directive in `go.work` + every `go.mod`, kept in lockstep) for a stdlib CVE, or `go get`-ing the fixed dependency version for a module CVE.

**trivy** — builds each of the six images and scans it for fixable HIGH/CRITICAL CVEs in OS packages and bundled libraries.
Run it locally (requires `trivy` and `docker` on `PATH`) with:

```
make trivy-scan
```

The five images we build from a minimal/distroless or scratch base (`gmc`, `agc`, `proxy`, `fakegithub`, and the `FROM scratch` `wrapper`) **block** the gate — every package in them is one we chose, so a finding is actionable by bumping a dependency or the base digest.
The `worker` image is built `FROM` the upstream `ghcr.io/actions/actions-runner` and inherits CVEs in the bundled node20 runtime and the runner's own Go binaries that we cannot fix without forking the runner; its leg is the sole **report-only** one (findings printed, never blocks).
Runner-base CVEs are reduced by bumping the pinned tag — automated via the `docker` ecosystem in `dependabot.yml` and tracked in [`STATUS.md`](../STATUS.md) Q70.

The same `trivy` job also generates an **SBOM** (Software Bill of Materials, SPDX-JSON, via [`syft`](https://github.com/anchore/syft)) for each image it builds and uploads it as a `sbom-<image>.spdx.json` build artifact.
This runs on every code PR purely so the SBOM-generation path can't silently break before a release — it does **not** sign or publish anything.
On a `v*` release tag, the separate [`publish.yml`](../../.github/workflows/publish.yml) workflow pushes the five first-party images (`gmc`, `agc`, `proxy`, `worker`, `wrapper`) to GHCR, regenerates each SBOM for the pushed image, signs every image **keyless** with [`cosign`](https://docs.sigstore.dev/) (sigstore/Fulcio via GitHub Actions OIDC — no signing key or stored secret), and attaches the SBOM as a keyless cosign attestation.
Operator-facing verification (`cosign verify`, SBOM retrieval) is documented in [security-operations.md § Image provenance](../operations/security-operations.md#image-provenance-signature--sbom-verification).
The signing/attestation steps run only on publish, so PR CI does not exercise them.

**polaris** — audits the Kubernetes security/best-practice posture of the **shipped install artifact**: it renders the [Helm chart](../../charts/actions-gateway) (digest-pinned, matching the production posture) and checks the rendered manifests.
The gate **fails on `danger` findings only** (privileged container, host namespace, dangerous capabilities, missing `securityContext`, a floating `:latest` image tag) — a real posture regression in the chart cannot merge — while `warning`s are reported for visibility.
False-positive warnings against a Helm-packaged operator chart are tuned to `ignore` in [`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml) (via `--merge-config`, so every default `danger` check stays active), each with a justifying comment.
Run it locally (requires `helm` and `polaris` on `PATH`) with:

```
make polaris-scan
```

This `polaris` job is path-gated on the chart (and `Makefile`).
The operator-facing writeup — including the manual `kube-bench` CIS scan that complements polaris at the live-cluster layer — is in [security-operations.md](../operations/security-operations.md#posture-scanning-preventive).

The three gates are path-gated (they skip when a PR touches only unrelated files); the two Go scans use `go-version-file: go.work`, so the toolchain version flows automatically.

## Install-artifact validation

The `manifest-validate.yml` workflow checks that the **shipped install artifact** — the [`actions-gateway` Helm chart](../../charts/actions-gateway), the sole install path (Q142) — is well-formed and schema-valid, so a malformed RBAC/CRD/policy file cannot merge silently.
It is independent of the security gates above (validity, not posture) and path-gated on the manifests, the chart, and the `Makefile`.
Run the exact gate locally (requires `yamllint`, `kubeconform`, and `helm` on `PATH`) with:

```
make manifest-validate
```

It first runs the chart CRD/RBAC drift gates (`make chart-crds-check` + `make chart-rbac-check`: the chart's CRD templates and `manager-role` rules are generated from the controller-gen sources under `cmd/*/config/`, so a marker change that isn't propagated fails here), then runs two layers over `cmd/*/config/**` and [`charts/actions-gateway`](../../charts/actions-gateway):

- **yamllint** lints the `controller-gen` YAML and the chart metadata against [`.yamllint.yaml`](../../.yamllint.yaml).
  The config targets real defects (tabs, trailing whitespace, duplicate keys, a missing final newline, truthy typos) and relaxes the purely cosmetic rules that would only ever fire on machine-generated style — `line-length` (CRD `description` lines are verbatim Go doc comments well over 200 chars) and `indentation` (the generated YAML mixes block-sequence indent styles).
  Helm templates are excluded — they embed `{{ ... }}` and are not parseable YAML; their rendered output is validated below instead.
- **kubeconform** schema-validates against the cluster API at the chart's `kubeVersion` floor (1.30.0 — validating the oldest supported version catches a field that does not exist there): the controller-gen manifests + the two ValidatingAdmissionPolicies under `cmd/*/config/` (the codegen + envtest substrate; there is no longer a kustomize overlay to render), and `helm template` output in digest-pinned, dev/test opt-out (`allowFloatingImageTags=true`), and all-optional-features form, plus `helm lint` on the chart and a fail-closed check that a render with any of the four image digests (`gmc`/`agc`/`proxy`/`wrapper`) empty is **rejected** — each image is tested independently with the other three pinned (all four required — Q96/Q307 secure-by-default; the check fails if any rejection ever stops happening). `-ignore-missing-schemas` skips only third-party/custom kinds whose schema is not in the upstream Kubernetes set (cert-manager `Certificate`/`Issuer`, the Prometheus Operator `ServiceMonitor`, and our own `ActionsGateway`/`RunnerGroup` CRs); the `CustomResourceDefinition`s that define them **are** validated, since that is a native `apiextensions` kind.

It also asserts that **no `ValidatingAdmissionPolicyBinding` carries `helm.sh/resource-policy: keep`**.
A binding is what makes a policy enforce, so retaining one across `helm uninstall` would leave the guard active after the release is gone and make `admissionPolicy.enabled=false` a silent no-op.
There is deliberately no matching assertion on the *policies*: retaining the `paramKind`-bearing policy was the first attempted fix for Q444 and it did not work, so asserting it here would re-freeze a wrong answer.

The tool versions are pinned in the workflow (`KUBECONFORM_VERSION`, `YAMLLINT_VERSION`); bump them deliberately, since a new kubeconform can change validation behaviour.
CI persists kubeconform's downloaded JSON schemas in an `actions/cache` keyed on the validated Kubernetes version so runs do not re-fetch the schema set from GitHub.
