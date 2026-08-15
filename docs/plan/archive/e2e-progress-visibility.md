# E2E Progress Visibility

Make a running e2e suite legible from the GitHub Actions log — which specs are running, which have completed, and how many remain — on both the hosted and the GKE dogfood self-hosted lanes.

Q608.
Shipped in one PR, so no Queue row was ever filed.

## Table of Contents

- [Background — what the log does today](#background--what-the-log-does-today)
- [Two constraints that shape the design](#two-constraints-that-shape-the-design)
- [Scope](#scope)
- [1. The progress event stream](#1-the-progress-event-stream)
- [2. The heartbeat watcher](#2-the-heartbeat-watcher)
- [3. Post-run summary and annotations](#3-post-run-summary-and-annotations)
- [Rejected alternatives](#rejected-alternatives)
- [Status](#status)
- [What the build changed from the plan](#what-the-build-changed-from-the-plan)

## Background — what the log does today

Measured on two real runs, one per lane, both 73 specs at `--procs 6`:

| Lane | Run | Suite wall clock | Distinct seconds with output | Longest silence |
|---|---|---|---|---|
| Hosted (`ubuntu-latest`) | [30750280196](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30750280196) | 7m00s | 16 | 98 s |
| Dogfood (`gag-ci-e2e`) | [30750226276](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30750226276) | 7m44s | 40 | 78 s |

Both lanes stream at second granularity, so the dogfood self-hosted path — where the runner is itself a gateway worker pod — has no log-batching pathology.
One mechanism serves both, which is expected: `runs-on` resolves from the same expression in [e2e-reusable.yml](../../../.github/workflows/e2e-reusable.yml), so the two lanes run the same job definition.

What the log contains during those silences:

- **Nothing is printed when a spec starts.** `DefaultReporter.WillRun` returns early on `report.RunningInParallel`, so at `--procs 6` spec starts are invisible by construction.
- **Completion is a bare `•`** unless the spec failed or carries a `ReportEntry`.
  Naming is therefore inconsistent across specs.
- **`--poll-progress-after 30s` is the only live per-spec signal.** It names the spec, the current `By` step, and both runtimes — genuinely the right data — but it fired 8 times in the hosted run and each report trails a goroutine and GinkgoWriter dump.
- **`Will run 73 of 73 specs` prints once at suite start.** The denominator is already available; only the numerator is missing.
- **`tmp/e2e-report.xml` is uploaded as an artifact and never rendered.** [e2e-tests-speed.md §13](../e2e-tests-speed.md#13-junit-report-for-pr-test-summary) planned the upload on the premise that "GitHub Actions can render this as a test summary table in the PR sidebar using the built-in test reporter".
  There is no such built-in reporter.
  The upload shipped, the rendering never did, and reading the report still means downloading an artifact.

## Two constraints that shape the design

1. **Live progress cannot be printed from inside the suite.** Ginkgo installs a `dup2`-based output interceptor around spec execution, so anything a spec or a `ReportAfterEach` writes to stdout is captured and replayed when the spec ends — precisely the buffering we are trying to escape.
   `--output-interceptor-mode=none` would stream it at the cost of per-spec failure capture, which is a bad trade.
   **File writes are not intercepted**, so the suite can emit to a file and a process *outside* Ginkgo can do the printing.
2. **The Actions log is append-only.** No cursor addressing, so no redrawing progress bar; heartbeat lines must each stand alone.
   `GITHUB_STEP_SUMMARY` renders after the job, not during it, so it serves the post-run view and cannot serve the live one.

Together these force the shape: **the suite writes a structured event stream to a file; a separate watcher process renders it to the step log.**

## Scope

Three pieces, one PR.
Deliberately excluded: teeing `By()` steps into the stream (Ginkgo's Progress Report already carries that data — tune `--poll-progress-after` first), and interleaving cluster state into the heartbeat.
Both are follow-ons once the heartbeat shows what it does and does not answer.

## 1. The progress event stream

`ReportBeforeSuite` / `ReportBeforeEach` / `ReportAfterEach` in [e2e_suite_test.go](../../../cmd/gmc/test/e2e/e2e_suite_test.go) append one JSON object per line to `$E2E_PROGRESS_FILE` (default `tmp/e2e-progress.jsonl`, gitignored).
Unset disables the whole mechanism, so a plain `go test` run is unaffected.

Events: `total` (from `PreRunStats.SpecsThatWillRun`, once), `start`, `end` (with state and duration).

**The atomicity constraint is load-bearing.** Six parallel processes append to one file.
`O_APPEND` writes to a regular file are atomic only below `PIPE_BUF` (4 KiB), so event lines carry spec text and state — never captured output — and the writer truncates each line defensively.
A spec name long enough to breach the limit would interleave two processes' bytes and corrupt both records.

Rendering must consume *this* stream, not Ginkgo's human-readable log.
Reporter output is not a stable contract; a regex scraper over it would drift silently, the same failure mode the autoscaler event-reason matcher exists to catch.

## 2. The heartbeat watcher

`scripts/e2e/progress-watch.sh`, started in the background before the ginkgo run and stopped after it, emits one line per interval (default 30 s):

```
[e2e t+04:12] 31/73 specs | 29 ok, 1 failed, 1 skipped | running: E2E_AGC_WorkerDrain a dr... (3m58s), E2E_GMC_Isolation cross... (2m01s)
```

Derived state: running = started-minus-ended; counts by terminal state; elapsed from the first event.
Silent until the `total` event lands, so bring-up noise is not competing with it.

The aggregation is a pure function of the stream, so `scripts/e2e/progress-watch-test.sh` asserts it against synthetic fixtures — the convention every other `scripts/**-test.sh` follows.

## 3. Post-run summary and annotations

`scripts/e2e/e2e-report-summary.sh` reads `tmp/e2e-report.xml` and writes:

- **`$GITHUB_STEP_SUMMARY`** — counts, every failure with its message, and the ten slowest specs (the input to any future speed work).
- **`::error::` annotations** per failed spec, so failures surface at the top of the run and inline on the diff rather than 3,500 log lines down.

Runs `if: always()` so a failed suite — the case that matters — still gets both.

## Rejected alternatives

- **`-v` on the ginkgo run.** Names every completed spec, but streams all 361 `By()` steps across 73 specs and still reports neither starts nor remaining count.
  Reasonable as a per-run debugging opt-in; wrong as a default.
- **`--output-interceptor-mode=none`.** Would let the suite print live, at the cost of the per-spec output capture that failure diagnosis depends on.
- **A live-updating Check Run via the API.** A live view outside the log, for the price of auth and rate-limit surface.
  Marginal gain over a heartbeat line.
- **A Go devtool instead of shell.** Justified if this grows an ETA model or more report rendering; not yet, and shell keeps it beside the other e2e scripts under the same shellcheck gate.

## Status

| Piece | State |
|---|---|
| Progress event stream (§1) | ✅ Shipped |
| Heartbeat watcher (§2) | ✅ Shipped |
| Step summary + annotations (§3) | ✅ Shipped |
| `By()` step teeing | Not pursued — Ginkgo's Progress Report already carries the current `By` step and its runtime; tune `--poll-progress-after` before building a second path to the same data |
| Cluster state in the heartbeat | Not pursued — wants evidence that "stuck at 31/73" is insufficient on its own before adding `kubectl` calls to a 30 s loop |

## What the build changed from the plan

Three things the plan did not anticipate, each found by measuring rather than by review:

- **`E2E_PROGRESS_FILE=… cd cmd/gmc && …` scopes the variable to the `cd` alone.** The Makefile's `_GINKGO_RUN` macro opens with a `cd`, so the obvious prefix placement left the suite emitting nothing at all — a heartbeat that would have stayed permanently silent while every test passed.
  Caught by running the target against a stub ginkgo that echoed its environment.
  The variable now sits with the other env vars *inside* the macro, after the `cd`.
- **`encoding/xml` writes `&#34;`, not `&quot;`.** The JUnit fixture was generated with Ginkgo's own `GenerateJUnitReport` rather than hand-written, which is the only reason this surfaced: a hand-rolled fixture would have used the named entity, passed its own test, and left raw `&#34;` in every quoted spec name in the job summary.
- **A bare `sleep` in the watcher delays its own teardown by up to a full interval.** Bash defers a trapped signal until the running command completes, so `kill -TERM` during `sleep 30` waits out the sleep.
  Backgrounding the sleep and `wait`-ing on it drops teardown to ~11 ms, measured.
