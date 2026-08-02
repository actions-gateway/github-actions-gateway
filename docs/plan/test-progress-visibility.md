# Test Progress Visibility

Make a long test run legible while it is still running — which tests are
executing, which have finished, how many remain — for every audience that waits
on one: a human at a terminal, a reviewer reading a CI log, and an agent running
the job in a background task and reporting back.

The end state is that **release validation is run as a background task by
default**, with the agent reporting progress to the operator as it goes, and the
same run is equally legible if a human runs it in their own terminal instead.

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

Each piece is small. What makes them one body of work is a shared shape that
only becomes visible once you have built two of them:

> A long-running job emits a structured event stream to a file. Renderers read
> that file. The renderers are cheap and disposable; the stream is the contract.

Phase 0 shipped that shape for the e2e suite. Everything after it is another
producer or another renderer over the same idea, and the value compounds — the
release-validation view in Phase 1 is largely *relaying* the stream Phase 0
already produces, and the agent-facing view in Phase 2 is a third renderer over
the stream Phase 1 adds.

Building these as isolated tasks would have produced three incompatible progress
formats and three polling loops.

## Status

| Phase | Scope | State |
|---|---|---|
| 0 | The e2e suite reports itself: heartbeat + JUnit summary + annotations | ✅ Shipped — [#1152](https://github.com/actions-gateway/github-actions-gateway/pull/1152), detail in [archive/e2e-progress-visibility.md](archive/e2e-progress-visibility.md) |
| 1 | `validate-release.sh` reports phase and spec progress in the terminal | ❌ Open — [Q615](../STATUS.md#Q615) |
| 2 | Background-task mode + status file + sentinel; documented as the default | ❌ Open — [Q616](../STATUS.md#Q616), [Q617](../STATUS.md#Q617) |
| 3 | Unit `-race` progress via `go test -json` | ❌ Open — [Q618](../STATUS.md#Q618) |
| — | Migrating other suites to Ginkgo | ⛔ Rejected on measurement — [see below](#evaluated-and-rejected-migrating-other-suites-to-ginkgo) |
| — | Integration-tier progress | ⛔ Not needed on measurement — 30–64 % output density, already self-narrating |

## Phase 0 — the e2e run reports itself

Shipped. The suite appends spec start/end events to `E2E_PROGRESS_FILE`;
[`progress-watch.sh`](../../scripts/e2e/progress-watch.sh) renders one heartbeat
line per 30 s; [`e2e-report-summary.sh`](../../scripts/e2e/e2e-report-summary.sh)
renders the JUnit report into the job summary plus per-failure annotations.
Covers the `e2e` and `e2e-calico` lanes and both the hosted and GKE dogfood
runners, because all four resolve to the same `e2e-reusable.yml` job.

Full rationale, measurements, and the three things the build changed from its
plan: [archive/e2e-progress-visibility.md](archive/e2e-progress-visibility.md).

## Phase 1 — release validation reports itself

[`validate-release.sh`](../../scripts/dogfood/validate-release.sh) is an
~hour-long, billable, prod-touching gate that runs eight phases and then watches
a dispatched e2e run. Today it prints a line per phase and then goes quiet
behind `gh run watch`, which renders job-level status only — the spec-level
heartbeat Phase 0 produces is in the dispatched run's log, which the operator
has to leave the terminal to see.

Three pieces:

1. **A phase event stream.** The script appends `{"phase":…,"state":…}` events
   to a status file as it moves through deploy → route CI → e2e tenant → dispatch
   → e2e run → sizing assertion → CRD smoke → teardown. Same JSONL shape as
   Phase 0, so one renderer can consume both.
2. **Relay the remote heartbeat.** The dispatched run's log already carries the
   `[e2e t+…]` lines. `gh run view --log` refuses on an in-progress run, but
   `gh api repos/…/actions/jobs/<id>/logs` returns partial logs for a running
   job — measured, and the mechanism that makes this phase possible at all.
   Poll it on the existing `E2E_POLL_INTERVAL` and re-emit new heartbeat lines
   locally.
3. **Render the JUnit report on the way out.** After the watch returns, download
   the run's `e2e-junit-report-*` artifact and pipe it through
   `e2e-report-summary.sh`, so a red gate names the failing specs in the
   operator's terminal instead of handing them a URL.

Plus a one-liner worth taking regardless: print the run URL before the watch.

**Open sub-question, to settle by measurement at build time:** whether
`gh run watch` emits ANSI cursor control when stdout is not a TTY. If it does,
running this gate as a background task is *already* producing garbled captured
output today, independent of anything here — which would make replacing it with
our own append-only poller a fix rather than a preference. The measurement was
blocked in-session by the pr-sentinel hook; take it first.

## Phase 2 — background-task mode becomes the standard

The goal: `run release validation` means the agent launches the gate in the
background and keeps the operator informed, and this is the documented default
rather than a power-user trick.

**Periodic reporting must be event-driven, not clock-driven.** The literal ask
is "report status periodically", but a fixed-interval poll is the wrong
implementation: it burns tokens on every tick whether or not anything changed,
and this repo forbids foreground polling outright. The meaningful cadence is
*phase transitions* — eight of them across the run, each a real thing the
operator wants to hear about — plus a long fallback so a wedged phase still
surfaces. That yields roughly the same reporting frequency with none of the
idle cost.

The pattern already exists in this repo and should be copied rather than
reinvented: **pr-sentinel** sleeps at zero token cost and wakes the session on
meaningful events. A `release-sentinel` doing the same for a validation run is
the natural sibling.

Three deliverables:

- **A status file** (`tmp/release-validation-status.json`) holding current
  phase, elapsed, the latest e2e heartbeat, and any failure — a single read
  that answers "where is it" without replaying the whole stream. This is the
  agent-facing renderer; the human-facing one is the terminal stream from
  Phase 1.
- **`release-sentinel`**, modeled on pr-sentinel: sleeps, wakes the session on
  phase transition, failure, or completion, and prints a status block on wake.
- **[`docs/operations/release.md` § Validate the release candidate on
  dogfood](../operations/release.md)** rewritten so the background-task flow is
  the documented default path, with the manual terminal invocation kept as the
  alternative. Per the [doc-update matrix](../development/doc-update-matrix.md)
  this is an operator-facing change and the docs move with it, not after.

## Phase 3 — the remaining tiers, by measurement

Measured output density and longest silence, isolated to the actual test steps
rather than whole jobs:

| Step | Span | Output density | Longest silence | Verdict |
|---|---|---|---|---|
| e2e suite (hosted) | 420 s | 3.8 % | 98 s | ✅ fixed in Phase 0 |
| e2e suite (dogfood) | 464 s | 8.6 % | 78 s | ✅ fixed in Phase 0 |
| unit tests (`-race`) | 200 s | 8 % | 58 s | worth doing — [Q618](../STATUS.md#Q618) |
| integration (AGC) | 223 s | 30 % | 31 s | ⛔ not needed |
| integration (GMC) | 280 s | 64 % | 75 s | ⛔ not needed |

**Integration is excluded on evidence, not oversight.** At 30–64 % density the
log already narrates itself continuously; GMC's single 75 s gap is one slow test
in an otherwise dense stream. A 30 s heartbeat there would compete with real
output instead of replacing silence.

**Unit `-race` earns it for the pathological case, not the happy path.** On a
healthy 3.3-minute run the heartbeat adds ~7 lines nobody needs. Its value is
the deadlock: today a hung `-race` run emits nothing until the timeout fires and
leaves you inferring which test hung.

The mechanism is different and *simpler* than Phase 0's — plain Go tests are
already visible to `go test -json`, so this is a wrapper over the existing
event stream with **no test-code changes at all**.

One honest limitation: `go test -json` has no denominator. Go does not know the
total test count up front, so the line reads "37 done, 4 running" rather than
"37/412". Recovering it needs a `go test -list '.*'` pre-pass. See
[Open decisions](#open-decisions).

## Evaluated and rejected: migrating other suites to Ginkgo

**Conclusion: no. For progress reporting specifically, migrating to Ginkgo would
make things materially worse, which is the opposite of the intuition.**

The measurement. `go test -json` against the e2e suite in dry-run, and against
one plain package:

| Suite | Tests visible to `go test -json` |
|---|---|
| e2e suite — 73 Ginkgo specs | **1** (`TestE2E`) |
| `broker` — one plain package | **68** (one event per test) |

Ginkgo specs are closures executed inside a single Go test function, so the
toolchain sees one test no matter how many specs run. That is precisely why
Phase 0 needed bespoke reporter instrumentation: there was no `-json` stream to
consume. Plain Go tests hand you per-test run/pass/fail events for free.

The repo today is **1 Ginkgo suite against 1,832 plain test functions**. A
migration would convert 1,832 individually-visible tests into a handful of
opaque suite functions, then require per-suite instrumentation to claw back
what the toolchain was already giving away.

The non-progress arguments, assessed honestly:

| Claimed benefit | Assessment |
|---|---|
| Parallelism | `go test` already parallelizes across packages, `t.Parallel()` within. Ginkgo's `--procs` matters for e2e because specs share one expensive cluster and need process isolation — not a gap elsewhere |
| Labels / filtering | Already covered by `-run` and the `integration`/`e2e`/`load`/`autoscaler` build tags the repo uses extensively |
| JUnit output | Obtainable from `go test -json` without a DSL migration |
| Slow-spec progress reports (`--poll-progress-after`) | Genuinely good, and Go has no equivalent — it names the current `By` step. But a `-json` watcher naming the running test covers most of the value at none of the cost |
| BDD readability | Subjective, and a poor fit for the table-driven unit tests that dominate this repo |

Against that: the coverage ratchet, the `-race` gate, `-count` soak runs, and
ordinary `-run` and IDE workflows all assume plain Go tests.

**Where Ginkgo does earn its place — and keeps it — is the e2e suite:** a shared
expensive cluster fixture, `Serial`/`Ordered` constraints, label-filtered
subsets, and parallel process isolation are exactly its strengths. Keep it
there; do not spread it.

## Design rules this milestone commits to

Derived from Phase 0 and from the [30 s cadence
discussion](#phase-3--the-remaining-tiers-by-measurement); they bind later
phases.

1. **The event stream is the contract; renderers are disposable.** Never parse a
   human-readable log to derive progress — reporter output is not a stable
   interface and a scraper over it drifts silently.
2. **Append-only, no ANSI cursor control.** It is correct in the CI log
   (append-only by nature), in a captured background-task file (where escape
   sequences become garbage), and under `grep`/`awk`. And at a 30 s cadence the
   accumulated history *is* the diagnostic signal — three identical counts in a
   row is how a stall becomes visible. Redraw would delete that.
3. **One default interval, one knob.** 30 s across tiers, overridable by env.
   No per-tier defaults without a measurement demanding one.
4. **Progress reporting never fails the job it reports on.** Every writer is
   best-effort; a torn read skips a tick rather than killing the watcher.
5. **Measure the silence before building a fix for it.** Every tier in this plan
   was included or excluded on a density number, not on intuition.

## Open decisions

| # | Decision | Notes |
|---|---|---|
| 1 | Does `gh run watch` emit ANSI when not a TTY? | Decides whether Phase 1 replaces it or wraps it. Measure first — it may already be garbling background runs today |
| 2 | Denominator for `go test -json` tiers | A `go test -list '.*'` pre-pass buys "37/412" for the cost of a second (cache-warm) invocation on the slowest tier. Alternative: ship without a denominator |
| 3 | Rename `E2E_PROGRESS_INTERVAL` | If Phase 3 lands, the `E2E_` prefix is wrong. Rename with the generalization, not before |
| 4 | Does the release sentinel belong in-repo or as a plugin? | pr-sentinel is a plugin outside this repo; a release sentinel is repo-specific and probably belongs in `scripts/dogfood/` |
