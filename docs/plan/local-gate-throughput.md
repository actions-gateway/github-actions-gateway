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

## Finding: the QoS clamp, not the parallelism knobs, sets the local ceiling

Measured 2026-07-26 on the replacement dev machine: Apple M5 Max, **18 physical /
18 logical cores** (performance levels "Super" ×6 and "Performance" ×12), 128 GB.
There `local-throttle.sh` sizes `jobs` at `physical - 2 = 16`, and `slots` stays
at its hardcoded default of 2.

The question was how far to raise those two numbers. Neither is the binding
constraint.

### Step 1 — what each prefix can reach

[`scripts/qos-cluster-probe.sh`](../../scripts/qos-cluster-probe.sh) saturates one
thread per logical CPU under a candidate prefix and samples per-cluster HW active
residency and clock with `powermetrics`, reporting effective compute as
`sum(residency × cores × clock)`:

| Prefix | Per-cluster residency | GHz-cores | % of max |
|---|---|---:|---:|
| `<none>` | P0=100% P1=100% S=100% | 68.0 | 100 % |
| `taskpolicy -c utility` (current) | P0=100% P1=2% S=9% | 14.3 | 21 % |
| `taskpolicy -d throttle` | P0=100% P1=100% S=100% | 65.5 | 96 % |
| `nice -n 10 taskpolicy -d throttle` | P0=100% P1=100% S=100% | 60.3 | 89 % |
| `taskpolicy -c background` | P0=1% P1=100% S=11% | 11.0 | 16 % |

Under `-c utility` every spin thread lands on a **single 6-core performance
cluster** pinned to ~2.65 GHz against a 4.38 GHz ceiling. Two runs pegged
*different* clusters (P1, then P0) and neither touched the Super cluster, ruling
out incidental scheduler packing.

`-c` is a *clamp*: it only ratchets QoS down, so there is no higher tier to
select. The alternative is `taskpolicy -d throttle`, which sets the disk I/O
policy (`IOPOL_TYPE_DISK`) **without** a QoS clamp — preserving the property this
design calls load-bearing, the I/O demotion `nice` cannot express on macOS, while
returning the idle cores.

**This table measures synthetic spin threads and overstates the confinement.**
Real compile workloads spill across clusters: under the cold-cache lint below,
`-c utility` reaches 37–39% mean CPU, not 21%. Treat 21% as the floor for
pure-CPU synthetic load, not as the ceiling for real builds.

### Step 2 — what it costs the desktop, under a real build

Spin threads generate no I/O, no memory pressure, and no process churn, so the
sweep cannot show whether a faster prefix keeps the desktop responsive — and
`-d throttle` gives up CPU priority, which is the protection being traded.
[`scripts/validate-throttle.sh`](../../scripts/validate-throttle.sh) runs a real
gate phase under each candidate while [`scripts/uijitter.c`](../../scripts/uijitter.c)
samples scheduling latency at `QOS_CLASS_USER_INTERACTIVE`, where the compositor
runs. Idle floor on this machine: p50 2.9 ms, p99 4.1 ms.

**Picking a workload that can discriminate took three attempts, and the two
failures are the reusable lesson.** `cmd/agc` tests alone: all three candidates
at an identical 14 s, jitter at the idle floor. The full workspace `-race` tier:
all three at an identical 42 s and only 12–14% mean CPU — about 2.3 of 18 cores.
Neither had loaded the machine, so neither could separate the candidates, yet
both read as a clean bill of health. The harness now samples CPU busy% and marks
a trial INVALID when no candidate reaches 50%.

Two corollaries worth keeping:

- **`-race` is not the heavy tier here** and is not part of `make check` at all.
  The cost sits in lint and coverage, and `golangci-lint` is the component that
  saturates — it fans out one worker per logical CPU and ignores `GOMAXPROCS`.
- **`GOCACHE` is the load-bearing cache, not `GOLANGCI_LINT_CACHE`.** Busting
  only the latter leaves the linter reading type information from the warm Go
  build cache, and a whole module then lints in ~1 s.

Cold-cache `golangci-lint` on `cmd/agc`, three trials, candidates interleaved:

| Prefix | Wall (median) | CPU% | p99 jitter | max jitter | >50 ms | swapins | WindowServer |
|---|---:|---:|---:|---:|---:|---:|---:|
| `taskpolicy -c utility` | 62 s | 37–39 % | 4.11–4.15 ms | 6.7 ms | 0 | 0 | 0 |
| **`nice -n 10 taskpolicy -d throttle`** | **17 s** | 66–74 % | 5.37–5.56 ms | 12.1 ms | 0 | 0 | 0 |
| `taskpolicy -d throttle` | 18 s | 63–70 % | 6.18–7.07 ms | 10.9 ms | 0 | 0 | 0 |

**3.6× on the phase that actually costs, for ~1.4 ms of added p99 latency.** The
worst single wake was 12.1 ms — inside one 60 Hz frame — with no stutter events
past 50 ms, no swapins, and no WindowServer reports in any of the nine runs.

`nice -n 10 taskpolicy -d throttle` was faster in all three trials *and* lower on
p99 in all three, so there is no speed-versus-safety trade to adjudicate: the
variant that keeps a CPU-priority demotion is also the quicker one.

### What this means for the two knobs

1. **`slots` was never adding hardware.** Every holder's heavy phases share the
   clamped band, so raising it divides a fixed ceiling rather than lifting it.
2. **`jobs = 16` was fictional** against a 6-core confinement — 2.7×
   oversubscription costing memory and context switches for no throughput.
3. **The prefix is the lever**, worth 3.6× measured end to end.

Both knobs should be re-derived *after* the prefix changes, against the ceiling
that then exists. Done below.

## Change 4: the prefix switches, and the two knobs are re-derived ✓

The macOS prefix is now `nice -n 10 taskpolicy -d throttle`
([`scripts/local-throttle.sh`](../../scripts/local-throttle.sh)), on the evidence
above. Linux is untouched — `nice -n 19` + `ionice -c 3` already expresses the
two demotions separately, which is exactly what macOS was missing.

With the ceiling lifted, both parallelism knobs were re-measured against it.
[`scripts/validate-throttle.sh`](../../scripts/validate-throttle.sh) grew the two
axes needed for that: `GAG_THROTTLE_CANDIDATES` entries take `|jobs=N` (so a
parallelism sweep interleaves within a trial, and thermal drift hits every level
equally) and `|holders=M` (M copies run concurrently, as M sessions holding M
semaphore slots would). A `test` workload was added alongside `lint`, because
`jobs` is spent in two different places — `golangci-lint -j` in
`scripts/go-lint.sh`, `go test -p` plus `GOMAXPROCS` in `scripts/go-test.sh` and
`scripts/coverage.sh` — and a lint-only sweep cannot speak for the other.

### `jobs`: not a lever for lint at any level from 8 to 24

Cold-cache `golangci-lint` on `cmd/agc` under the new prefix, 3 interleaved
trials (wall seconds per trial, then the jitter range across all three):

| jobs | Wall (3 trials) | CPU% | p99 jitter | max jitter | >50 ms | swapins | WindowServer |
|---:|---|---:|---:|---:|---:|---:|---:|
| 8 | 18 / 21 / 21 | 59–73 % | 5.44–5.73 ms | 11.0 ms | 0 | 0 | 0 |
| 12 | 18 / 19 / 19 | 63–79 % | 5.45–6.47 ms | 11.0 ms | 0 | 0 | 0 |
| 16 (current) | 18 / 23 / 21 | 66–81 % | 5.73–6.38 ms | 12.1 ms | 0 | 0 | 0 |
| 18 | 18 / 19 / 23 | 68–82 % | 5.71–5.89 ms | 12.2 ms | 0 | 0 | 0 |
| 24 | 18 / 19 / 19 | 66–80 % | 5.97–7.29 ms | 9.8 ms | 0 | 0 | 0 |

Every level lands in 18–23 s. **The spread *within* one level (16: 18→23 s) is
wider than the spread between the extremes**, so there is no signal here to fit a
curve to — `jobs` is simply not what bounds a per-module lint on this machine.
Two things follow, and they point in opposite directions from the ones the
section above anticipated:

- Raising it buys nothing. `go-lint.sh` lints modules **serially**, one
  `golangci-lint -j jobs` per module, and a single module's lint stops scaling
  below 8. CPU% climbs with `jobs` (59 % → 80 %) while wall time does not — that
  is the oversubscription cost showing up with no throughput to pay for it.
- Lowering it costs nothing *here*, but `jobs` is not lint's alone (see the unit
  tier below), so a cut would have to be justified against that consumer too.

Desktop cost is flat as well, and stays close to the idle floor (p50 3.16 ms,
p99 4.17 ms) at every level: worst p99 7.29 ms, worst single wake 12.2 ms — still
inside one 60 Hz frame — with zero stutter events past 50 ms, zero swapins and
zero WindowServer reports in all 15 runs, including the 1.33× oversubscribed
`jobs=24`.

### The unit tier had to go cold too — the fourth null

The first attempt at the `test` workload kept the build cache **warm** and used
`-count=1`, on the theory that this isolates `go test -p` from a compile storm.
It reproduced the harness's signature failure exactly: 46/40/40/40 s at
`jobs=4/8/16/24` and 18–36 % mean CPU, correctly marked `INVALID`. With the build
cache warm, `-count=1` defeats only the *result* cache, so what remains is test
execution — a handful of suites waiting on timers and envtest, which no amount of
`-p` makes faster.

That is now **four** undersized workloads across this investigation (`cmd/agc`
alone, the warm `-race` tier, the warm unit tier, and — from the other
direction — a `GOLANGCI_LINT_CACHE`-only bust). Every one of them returned
identical numbers for every candidate and would have read as "the knob doesn't
matter" without the saturation guard. **The generalization: on this repo, warm
caches make every tier execution-bound and unable to discriminate; `GOCACHE` is
what has to be cold for a parallelism knob to show a curve at all.** `run_test`
therefore busts `GOCACHE` per run like `run_lint` does, which is also the regime
that matters — a fresh worktree is exactly when the local gate is expensive.

Cold, the unit tier does saturate (55–74 % CPU, `VALID`) and answers the other
half of the `jobs` question — full workspace, `-count=1`, 2 interleaved trials:

| jobs | Wall (2 trials) | CPU% | p99 jitter | max jitter | >50 ms | swapins | WindowServer |
|---:|---|---:|---:|---:|---:|---:|---:|
| 8 | 57 / 61 | 55–64 % | 4.19–4.25 ms | 10.4 ms | 0 | 0 | 0 |
| 16 (current) | 59 / 64 | 67–72 % | 5.14–6.04 ms | 11.6 ms | 0 | 0 | 0 |
| 24 | 66 / 63 | 61–74 % | 5.33–5.65 ms | 13.0 ms | 0 | 0 | 0 |

The trend is mildly *negative*: more parallelism is very slightly slower, which
is the oversubscription cost again. But the whole range spans 57–66 s against a
4–5 s within-level spread, so this is a weak signal at best — and it certainly
contains no case for raising `jobs`.

### `jobs` stays at `physical - 2`

Neither consumer shows a throughput gain anywhere in 8–24, and the unit tier
leans slightly *against* the high end. That is not, however, evidence for
lowering it, because **`jobs` is a formula, not the number 16**: on this 18-core
machine `physical - 2` happens to yield 16, but on a 4-core laptop it yields 2,
and that is the case where it actually binds. The measurements say the formula's
exact output stops mattering once the machine is wide — not that its
GUI-headroom rationale (leave two physical cores for the compositor) is wrong.
Replacing it with a tuned constant would trade a rationale that scales for a
number fitted to one machine, to buy an effect inside the noise.

What did change is the reason it is safe. Before, `jobs = 16` was
[fictional](#what-this-means-for-the-two-knobs) — 2.7× oversubscription of a
6-core confinement. Now the cores are really there, `jobs=24` genuinely
oversubscribes them, and the desktop still never registered a stutter event.

### `slots` stays at 2 — the knee is exactly there

M concurrent cold-cache lints, 3 interleaved trials. `WALL_S` is the time until
the **last** holder finishes, so aggregate throughput is M/wall — that, not wall
time, is what a slot count buys:

| holders | Wall (3 trials) | Mean | Throughput | vs 1 slot | CPU% | p99 jitter | max jitter | >50 ms |
|---:|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 19 / 25 / 25 | 23.0 s | 0.043 /s | 1.00× | 81–85 % | 5.3–6.5 ms | 11.8 ms | 0 |
| **2 (current)** | 33 / 39 / 38 | 36.7 s | 0.055 /s | **1.25×** | 92–94 % | 7.1–8.2 ms | 14.9 ms | 0 |
| 3 | 48 / 59 / 52 | 53.0 s | 0.057 /s | 1.30× | 91–95 % | 8.1–8.2 ms | 14.0 ms | 0 |
| 4 | 64 / 76 / 71 | 70.3 s | 0.057 /s | 1.31× | 89–94 % | 8.5–9.2 ms | 16.8 ms | 0 |

**The second slot earns its place; the third and fourth do not.** Slot 2 adds
25 % aggregate throughput for ~1.7 ms of p99. Slot 3 adds 4 %, slot 4 adds 1 % —
both inside the noise — while the tail keeps growing, and at 4 holders the worst
single wake (16.8 ms) finally crosses a 60 Hz frame. No configuration produced a
stutter event past 50 ms, a swapin, or a WindowServer report.

This confirms the current default, but it retires the reasoning that was
[recorded for it](#what-this-means-for-the-two-knobs). That reasoning said
`slots` "was never adding hardware" because every holder shared the clamped band.
The band is gone, and the conclusion survives for the opposite reason: **one
holder now reaches 81–85 % CPU on its own**, so there is genuinely little machine
left for a second to claim. Under `-c utility` a single run took ~37 %, and the
second slot was dividing a ceiling; now it is competing for a nearly-full one.

The throughput column also understates what slot 2 is for. The problem it was
added to solve ([Q376](../STATUS.md)) was *queue depth* — a sibling session
blocked for a full run before it could start. Two holders finish in 36.7 s where
strict serialization needs 23.0 + 23.0 = 46 s, so the second session both starts
immediately and finishes sooner. That is the argument for slot 2, and it does not
extend to slot 3.

`GAG_HEAVY_BUILD_SLOTS` still overrides the default in both directions for anyone
whose machine or taste differs.

### Scope of these numbers

All of the above is one machine (M5 Max, 18 physical cores) and one repo. The
`jobs` formula and the `MIN_CORES_FOR_MULTIPLE_SLOTS=4` floor exist for narrow
machines, where both knobs actually bind — nothing here measures that case, and
nothing here should be read as evidence about it.

### Scope: this only matters cold

Warm, the whole local gate runs at 12–14% CPU and no setting is load-bearing —
`-trimpath` cache sharing made that the common case. The throttle is a
fresh-worktree concern, which is also precisely when several sessions collide.

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
