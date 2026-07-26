# Local `make check` throughput

**Problem.** `make check` is the one-command pre-review gate every session runs
before opening a PR. On a small dev machine it costs ~21 minutes on a fresh
worktree, and because the heavy phases hold a machine-wide *exclusive* lock, a
second session cannot start its gate until the first finishes. With half a dozen
parallel worktree sessions the gate, not the work, sets the pace — observed
waits up to 5 h ([Q376](../STATUS.md)).

**Goal.** Cut both the cost of one run and the queue depth across concurrent
runs, without weakening any gate and without regressing the desktop-safety
properties the throttle exists to protect.

## Baseline measurement (2026-07-26)

Machine: MacBook Pro, Intel i7-1068NG7, **4 physical / 8 logical cores**, 32 GB.
`scripts/local-throttle.sh` sizes parallelism at `physical - 2 = 2`, so one run
uses ~2 of 8 hardware threads at `utility` QoS while holding the exclusive lock.

Every `make check` prerequisite, timed individually, in a worktree checked out at
`origin/main` with no local diff:

| Phase | Cold worktree | Warm re-run |
|---|---|---|
| `cover-check` | **1131 s** | 87 s |
| `lint` | 85 s | 1 s |
| `scripts-test` | 24 s | 20 s |
| `shellcheck` | 15 s | 12 s |
| 12 remaining gates | ~13 s | ~8 s |
| **total** | **~21 min** | **~2 min** |

Two facts follow:

1. **`cover-check` is the gate.** Everything else together is under 2.5 minutes
   even cold.
2. **The 13× cold/warm gap is cache residency, not test cost.** Go's test-result
   cache works fine — a second run in the same worktree replays it. The problem
   is that a *new* worktree never inherits it.

## Finding: `-trimpath` makes the test-result cache path-independent

[testing.md § Build and lint caches across worktrees](../development/testing.md#build-and-lint-caches-across-worktrees)
(Q343) recorded that the `go test` result cache is path-keyed and concluded
"there is no supported knob to share test *results* across paths". That
conclusion was measured at the default flags and is wrong once `-trimpath` is in
play: the flag removes the absolute worktree path from the test binary, so the
cache key becomes identical across checkouts of the same content.

Measured, `cmd/agc` with `-coverprofile`:

| Run | Elapsed | Cached packages | Profile |
|---|---|---|---|
| worktree A, cold | 226 s | 0 | 2577 lines, 75.3 % |
| worktree B, **first ever run** | **5 s** | 12 | 2577 lines, 75.3 % |

The cached run emits a byte-identical coverage profile, so the ratchet is
unaffected. `broker` reproduces the same result: without `-trimpath` worktree B
re-runs the suite; with it, B prints `(cached)` on its first invocation.

**`-trimpath` must not be set globally** (`go env -w GOFLAGS`, or the e2e
targets): [`cmd/gmc/test/e2e/e2e_suite_test.go`](../../cmd/gmc/test/e2e/e2e_suite_test.go)
resolves the v2 CRD chart directory from `runtime.Caller(0)`, which returns a
trimmed, non-existent path under the flag. It is applied in the unit-tier
scripts only (`scripts/go-test.sh`, `scripts/coverage.sh`), which the e2e and
integration tiers do not use. The release images already build with `-trimpath`
(`cmd/*/Dockerfile`), so the flag is not new to the repo.

## Changes

### 1. `-trimpath` in the unit tier ✓

`scripts/go-test.sh` (backing `make test` and `make test-race`) and
`scripts/coverage.sh` (backing `make cover*`) pass `-trimpath`. A fresh worktree
at an already-tested commit pays ~0 for the unit suite instead of ~19 min; only
the packages whose content actually changed re-run.

This largely subsumes [Q377](../STATUS.md) (change-scope `make test`/coverage to
affected modules): content-keyed caching skips unchanged modules *soundly*,
where scoping has to reason about which module a diff can affect and leaves the
unscoped modules' coverage floors ungated.

### 2. Heavy-build lock becomes an N-slot semaphore ✓

`serialize_heavy_build` held one exclusive lock, so exactly one session could run
a heavy phase at a time — using `physical - 2` of the machine's threads while
every sibling blocked. It now acquires one of **N** slots (default 2 on a machine
with ≥ 4 physical cores, 1 below that; override with `GAG_HEAVY_BUILD_SLOTS`).

Two holders at `jobs = physical - 2` each can oversubscribe the physical cores —
deliberately. The desktop-safety property the throttle defends is the QoS
demotion (CPU **and** I/O below the compositor), which every holder still
carries; the parallelism cap is a secondary bound. Trading a bounded
oversubscription for halved queue depth is the right side of that trade on a
machine running several sessions, and the env override exists for anyone who
wants the old strict serialization (`GAG_HEAVY_BUILD_SLOTS=1`).

### 3. The cheap gates run in parallel ✓

The 14 non-heavy `check` gates took no lock and ran strictly serially (~50 s).
They now run through `scripts/run-parallel.sh` (labeled, attributable output —
GNU make 3.81 ships on macOS and has no `-O` output sync, so `make -j` would
interleave failures unreadably). `scripts-test`'s 11 assertion scripts are
parallelized the same way.

## Follow-ups (not in this change)

- **The coverage loop is sequential.** `scripts/coverage.sh` runs its 10 modules
  one at a time at `-p 2`, where `make test` issues a single workspace-wide
  invocation that parallelizes across modules. Worth measuring whether the
  coverage tier should adopt the same shape (or run modules concurrently under
  the slot budget) now that caching makes the common case cheap.

  Per-module execution with the result cache forced off (`-count=1`, warm build
  cache) is dominated by four modules — `cmd/agc` 52 s, `cmd/proxy` 37 s,
  `cmd/gmc` 30 s, `cmd/probe` 15 s, every other module ≤ 6 s, 162 s total. A
  head-to-head against the single-invocation shape in the same session measured
  322 s, but the two phases ran back to back and the second inherited
  dependency compiles from the first, so that number is **confounded and should
  not be quoted** — re-measure in separate cold worktrees before acting on it.
- **Worktree hygiene.** 58 live worktrees × ~226 MB ≈ 13 GB, all Spotlight-indexed.
  `git worktree prune` plus a Spotlight exclusion on `.claude/worktrees` removes
  background I/O contention that the QoS demotion cannot help with.
