# Test Progress Visibility

Make a long test run legible while it is still running — which tests are executing, which have finished, how many remain — for every audience that waits on one: a human at a terminal, a reviewer reading a CI log, and an agent running the job in a background task and reporting back.

The end state is that **release validation is run as a background task by default**, with the agent reporting progress to the operator as it goes, and the same run is equally legible if a human runs it in their own terminal instead.

## Table of Contents

- [Why this is a milestone and not a task](#why-this-is-a-milestone-and-not-a-task)
- [Status](#status)
- [Phase 0 — the e2e run reports itself](#phase-0--the-e2e-run-reports-itself)
- [Phase 1 — release validation reports itself](#phase-1--release-validation-reports-itself)
- [Phase 2 — background-task mode becomes the standard](#phase-2--background-task-mode-becomes-the-standard)
- [Phase 3 — the remaining tiers, by measurement](#phase-3--the-remaining-tiers-by-measurement)
- [Evaluated and rejected: migrating other suites to Ginkgo](#evaluated-and-rejected-migrating-other-suites-to-ginkgo)
- [Design rules this milestone commits to](#design-rules-this-milestone-commits-to)
- [Open decisions](#open-decisions)

## Why this is a milestone and not a task

Each piece is small.
What makes them one body of work is a shared shape that only becomes visible once you have built two of them:

> A long-running job emits a structured event stream to a file.
> Renderers read that file.
> The renderers are cheap and disposable; the stream is the contract.

Phase 0 shipped that shape for the e2e suite.
Everything after it is another producer or another renderer over the same idea, and the value compounds — the release-validation view in Phase 1 is largely *relaying* the stream Phase 0 already produces, and the agent-facing view in Phase 2 is a third renderer over the stream Phase 1 adds.

Building these as isolated tasks would have produced three incompatible progress formats and three polling loops.

## Status

| Phase | Scope | State |
|---|---|---|
| 0 | The e2e suite reports itself: heartbeat + JUnit summary + annotations | ✅ Shipped — [#1152](https://github.com/actions-gateway/github-actions-gateway/pull/1152), detail in [archive/e2e-progress-visibility.md](archive/e2e-progress-visibility.md) |
| 1 | `validate-release.sh` reports phase and spec progress in the terminal | ✅ Shipped — Q615 |
| 2 | Background-task mode + status file + sentinel; documented as the default | ✅ Shipped — status file + sentinel (Q616), documented as the default path in [release.md](../operations/release.md#run-it-detached-the-sentinel-reports-it-back) (Q617) |
| 3 | Unit `-race` progress via `go test -json` | ✅ Shipped — Q618 |
| — | Migrating other suites to Ginkgo | ⛔ Rejected on measurement — [see below](#evaluated-and-rejected-migrating-other-suites-to-ginkgo) |
| — | Integration-tier progress | ⛔ Not needed on measurement — 30–64 % output density, already self-narrating |

## Phase 0 — the e2e run reports itself

Shipped.
The suite appends spec start/end events to `E2E_PROGRESS_FILE`; [`progress-watch.sh`](../../scripts/e2e/progress-watch.sh) renders one heartbeat line per 30 s; [`e2e-report-summary.sh`](../../scripts/e2e/e2e-report-summary.sh) renders the JUnit report into the job summary plus per-failure annotations.
Covers the `e2e` and `e2e-calico` lanes and both the hosted and GKE dogfood runners, because all four resolve to the same `e2e-reusable.yml` job.

Full rationale, measurements, and the three things the build changed from its plan: [archive/e2e-progress-visibility.md](archive/e2e-progress-visibility.md).

## Phase 1 — release validation reports itself

[`validate-release.sh`](../../scripts/dogfood/validate-release.sh) is an ~hour-long, billable, prod-touching gate that runs eight phases and then watches a dispatched e2e run.
Today it prints a line per phase and then goes quiet behind `gh run watch`, which renders job-level status only — the spec-level heartbeat Phase 0 produces is in the dispatched run's log, which the operator has to leave the terminal to see.

Three pieces:

1. **A phase event stream.** The script appends `{"phase":…,"state":…}` events to a status file as it moves through deploy → route CI → e2e tenant → dispatch → e2e run → sizing assertion → CRD smoke → teardown.
   Same JSONL shape as Phase 0, so one renderer can consume both.
2. **Relay the remote heartbeat.** The dispatched run's log already carries the `[e2e t+…]` lines. `gh run view --log` refuses on an in-progress run, but `gh api repos/…/actions/jobs/<id>/logs` returns partial logs for a running job — measured, and the mechanism that makes this phase possible at all.
   Poll it on the existing `E2E_POLL_INTERVAL` and re-emit new heartbeat lines locally.
3. **Render the JUnit report on the way out.** After the watch returns, download the run's `e2e-junit-report-*` artifact and pipe it through `e2e-report-summary.sh`, so a red gate names the failing specs in the operator's terminal instead of handing them a URL.

Plus a one-liner worth taking regardless: print the run URL before the watch.

**Measured 2026-08-02: `gh run watch` emits no ANSI when stdout is not a TTY.** Piped, it degrades to plain append-only text with UTF-8 status glyphs — so background-task capture was **not** garbled, and there was no pre-existing bug to fix.
Replacing it is therefore justified by the relay requirement alone: gh's watch *blocks*, and a heartbeat cannot be interleaved from inside it.
Recorded because the opposite conclusion was the tempting one and would have shipped an overstated claim.

## Phase 2 — background-task mode becomes the standard

The goal: `run release validation` means the agent launches the gate in the background and keeps the operator informed, and this is the documented default rather than a power-user trick.

**Periodic reporting must be event-driven, not clock-driven.** The literal ask is "report status periodically", but a fixed-interval poll is the wrong implementation: it burns tokens on every tick whether or not anything changed, and this repo forbids foreground polling outright.
The meaningful cadence is *phase transitions* — eight of them across the run, each a real thing the operator wants to hear about — plus a long fallback so a wedged phase still surfaces.
That yields roughly the same reporting frequency with none of the idle cost.

The pattern already exists in this repo and should be copied rather than reinvented: **pr-sentinel** sleeps at zero token cost and wakes the session on meaningful events.
A `release-sentinel` doing the same for a validation run is the natural sibling.

Three deliverables:

- ✅ **A status file** (`tmp/release-validation-status.json`) holding current phase, elapsed, the latest e2e heartbeat, and any failure — a single read that answers "where is it" without replaying the whole stream.
  This is the agent-facing renderer; the human-facing one is the terminal stream from Phase 1.
  Shipped in Q616 as `progress_status_json` in [`lib/progress.sh`](../../scripts/dogfood/lib/progress.sh), rewritten atomically after every event and also available as [`release-status.sh`](../../scripts/dogfood/release-status.sh).
- ✅ **`release-sentinel`**, modeled on pr-sentinel: sleeps, wakes the session on phase transition, failure, or completion, and prints a status block on wake.
  Shipped in Q616 as [`release-sentinel.sh`](../../scripts/dogfood/release-sentinel.sh).
- ✅ **[`docs/operations/release.md` § Validate the release candidate on dogfood](../operations/release.md#validate-the-release-candidate-on-dogfood)** rewritten so the background-task flow is the documented default path, with the terminal invocation kept as the alternative.
  Per the [doc-update matrix](../development/doc-update-matrix.md) this is an operator-facing change and the docs move with it, not after — Q616 documented the two new commands where the gate's other knobs already live; Q617 is the restructure that makes the flow the default.
  It also surfaced a requirement the mechanism half had not: **a detached run must carry `ASSUME_YES=1`**, because the gate's target-confirmation reads stdin and a detached run has none — measured, it exits 1 before spending anything.
  The default path is unusable without it.

**Three things the build settled that the plan did not anticipate.**

1. **The e2e heartbeat had to enter the stream, not just the terminal.** The e2e leg is one ~25-minute phase, so a status object built from phase events alone reports "e2e, 18 minutes" for the whole of it — the phase is the only thing that does not change. `e2e-run-watch.sh` now folds the newest relayed line into the stream as a `heartbeat` record. **This was also load-bearing for the stall detector, and that part was wrong** (Q630): the heartbeat needs a fetchable job log, GitHub served `BlobNotFound` for a whole 30-minute run that passed, and since the stall threshold is shorter than a healthy leg the detector then reported a false stall on every poll.
   The stream cannot be the only witness to a phase whose only writer depends on someone else's storage — the detector now reconciles silence against the run's own status.
2. **A gate's failure is reported by its first failing phase, not its last.** The teardown trap records `gate fail` after the phase that actually broke records its own, so taking the newest failure would answer every failed run with "the gate exited 1".
3. **A wedge is the absence of events, so the sentinel needs a stall event.** Waking only on transitions cannot report a gate that stops transitioning — which is exactly the failure a walk-away command hides. `idle` (age of the newest event) crossing `RELEASE_SENTINEL_STALL` is a wake in its own right, and only for a `running` gate: preflight has no stream to be quiet on and a finished gate is expected to be silent.
   Q630 added the other half: absence of events is a *hypothesis* of a wedge, confirmed against the run status before it is reported, and remembered afterwards so a relaunched watcher does not re-report the same silence the instant it starts.

## Phase 3 — the remaining tiers, by measurement

Measured output density and longest silence, isolated to the actual test steps rather than whole jobs:

| Step | Span | Output density | Longest silence | Verdict |
|---|---|---|---|---|
| e2e suite (hosted) | 420 s | 3.8 % | 98 s | ✅ fixed in Phase 0 |
| e2e suite (dogfood) | 464 s | 8.6 % | 78 s | ✅ fixed in Phase 0 |
| unit tests (`-race`) | 200 s | 8 % | 58 s | ✅ shipped in Phase 3 |
| integration (AGC) | 223 s | 30 % | 31 s | ⛔ not needed |
| integration (GMC) | 280 s | 64 % | 75 s | ⛔ not needed |

**Integration is excluded on evidence, not oversight.** At 30–64 % density the log already narrates itself continuously; GMC's single 75 s gap is one slow test in an otherwise dense stream.
A 30 s heartbeat there would compete with real output instead of replacing silence.

**Unit `-race` earns it for the pathological case, not the happy path.** On a healthy 3.3-minute run the heartbeat adds ~7 lines nobody needs.
Its value is the deadlock: today a hung `-race` run emits nothing until the timeout fires and leaves you inferring which test hung.

The mechanism is different and *simpler* than Phase 0's — plain Go tests are already visible to `go test -json`, so this is a wrapper over the existing event stream with **no test-code changes at all**.

Shipped as [`devtools/gotest/progress`](../../devtools/gotest/progress/main.go), which [`go-test.sh`](../../scripts/go/go-test.sh) pipes `go test -json` through.
It reconstructs the plain test log and interleaves the heartbeat.
Full behaviour, the three measured properties of the `-json` stream it stands on, and the two deliberate differences from plain `go test` output: [testing.md § Watching a unit run in progress](../development/testing.md#watching-a-unit-run-in-progress).

Two things the build changed from this plan:

- **The renderer had to reconstruct `go test`'s output, not just add to it.** `-json` replaces the plain log rather than accompanying it, so the wrapper owns what a green package and a failing package print.
  That is what makes it a Go program in `devtools/` rather than the bash+jq the earlier phases used — `Output` fields are JSON-escaped free text, and getting a compile error's bytes back intact is not a job for awk.
- **The denominator arrived at the package level, not the test level** — see [Open decisions](#open-decisions) #2.

A property worth recording, because it was not the reason for the change and is larger than the heartbeat: plain `go test` releases package output **in command-line order**, so a slow early package holds back every package behind it. `-json` has no such barrier (measured), so package results now appear as they complete.

Not covered: `make check` reaches the unit tests through `coverage.sh`, a separate invocation that still runs plain `go test`.
Its silence has not been measured, and design rule 5 says that measurement comes before the fix.

## Evaluated and rejected: migrating other suites to Ginkgo

**Conclusion: no. For progress reporting specifically, migrating to Ginkgo would make things materially worse, which is the opposite of the intuition.**

The measurement. `go test -json` against the e2e suite in dry-run, and against one plain package:

| Suite | Tests visible to `go test -json` |
|---|---|
| e2e suite — 73 Ginkgo specs | **1** (`TestE2E`) |
| `broker` — one plain package | **68** (one event per test) |

Ginkgo specs are closures executed inside a single Go test function, so the toolchain sees one test no matter how many specs run.
That is precisely why Phase 0 needed bespoke reporter instrumentation: there was no `-json` stream to consume.
Plain Go tests hand you per-test run/pass/fail events for free.

The repo today is **1 Ginkgo suite against 1,832 plain test functions**.
A migration would convert 1,832 individually-visible tests into a handful of opaque suite functions, then require per-suite instrumentation to claw back what the toolchain was already giving away.

The non-progress arguments, assessed honestly:

| Claimed benefit | Assessment |
|---|---|
| Parallelism | `go test` already parallelizes across packages, `t.Parallel()` within. Ginkgo's `--procs` matters for e2e because specs share one expensive cluster and need process isolation — not a gap elsewhere |
| Labels / filtering | Already covered by `-run` and the `integration`/`e2e`/`load`/`autoscaler` build tags the repo uses extensively |
| JUnit output | Obtainable from `go test -json` without a DSL migration |
| Slow-spec progress reports (`--poll-progress-after`) | Genuinely good, and Go has no equivalent — it names the current `By` step. But a `-json` watcher naming the running test covers most of the value at none of the cost |
| BDD readability | Subjective, and a poor fit for the table-driven unit tests that dominate this repo |

Against that: the coverage ratchet, the `-race` gate, `-count` soak runs, and ordinary `-run` and IDE workflows all assume plain Go tests.

**Where Ginkgo does earn its place — and keeps it — is the e2e suite:** a shared expensive cluster fixture, `Serial`/`Ordered` constraints, label-filtered subsets, and parallel process isolation are exactly its strengths.
Keep it there; do not spread it.

## Design rules this milestone commits to

Derived from Phase 0 and from the [30 s cadence discussion](#phase-3--the-remaining-tiers-by-measurement); they bind later phases.

1. **The event stream is the contract; renderers are disposable.** Never parse a human-readable log to derive progress — reporter output is not a stable interface and a scraper over it drifts silently.
2. **Append-only, no ANSI cursor control.** It is correct in the CI log (append-only by nature), in a captured background-task file (where escape sequences become garbage), and under `grep`/`awk`.
   And at a 30 s cadence the accumulated history *is* the diagnostic signal — three identical counts in a row is how a stall becomes visible.
   Redraw would delete that.
3. **One default interval, one knob.** 30 s across tiers, overridable by env.
   No per-tier defaults without a measurement demanding one.
4. **Progress reporting never fails the job it reports on.** Every writer is best-effort; a torn read skips a tick rather than killing the watcher.
5. **Measure the silence before building a fix for it.** Every tier in this plan was included or excluded on a density number, not on intuition.

## Open decisions

| # | Decision | Notes |
|---|---|---|
| 1 | ~~Does `gh run watch` emit ANSI when not a TTY?~~ | **Settled 2026-08-02: no.** Piped output is plain append-only text; nothing was garbled. Phase 1 replaced it anyway, because gh's watch blocks and the relay needs the foreground |
| 2 | ~~Denominator for `go test -json` tiers~~ | **Settled 2026-08-02: packages, not tests.** The `-list '.*'` pre-pass measured 22 s wall / 131 s CPU *with a warm build cache* — it compiles every test binary before any test runs, which also serializes the compile that go-test.sh's single invocation exists to overlap (Q17: 189 s → 163 s). `go list` gives a package denominator for ~0.3 s and no compilation |
| 3 | ~~Rename `E2E_PROGRESS_INTERVAL`~~ | **Settled 2026-08-02: renamed to `TEST_PROGRESS_INTERVAL`** with Phase 3, and both tiers read it. `0` now means "off" in both — sharing the knob without that would have made 0 a spin-loop in the e2e watcher. `E2E_PROGRESS_FILE`/`_SPEC_WIDTH` keep their prefix; they are genuinely Ginkgo-specific |
| 4 | ~~Does the release sentinel belong in-repo or as a plugin?~~ | **Settled with Q616: in-repo.** It reads this gate's own event stream and renders this gate's phases; there is nothing in it a second repo could use. `scripts/dogfood/release-sentinel.sh` |
