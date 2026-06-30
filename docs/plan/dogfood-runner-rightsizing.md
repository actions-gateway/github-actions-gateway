# GAG Dogfood CI Runner Right-Sizing

> **Status: ❌ Open — measurement pending.** Tracked as [Q248](../STATUS.md#Q248).
> The worker pod's current `requests`/`limits` (CPU 2/4, memory 4Gi/8Gi) were
> never measured — they are a guess. This plan replaces them with values derived
> from observed peak usage, and sizes the worker node pool to bin-pack them.

## Goal

Right-size the GAG dogfood worker pods (CPU/memory `requests` and `limits`) and
the worker node pool from **measured peak** usage, so CI jobs run on GAG without
OOM or CPU throttling at the fewest spot-node-hours.

## Scope note — this is cost/correctness, not speed

Establish up front what this is *not*: it is **not** a play to make CI faster than
GitHub-hosted. The 2026-06-30 baseline measurement (last ~25 runs each of
unit/integration/e2e) showed:

- **GitHub-hosted has no queue to eliminate** — median job pickup was **2s**
  (p90 3s; 0% of 150 jobs queued over 2 min). The repo's fan-out sits far under
  GitHub's concurrency cap, so GAG's main potential advantage (absorbing a queue)
  does not apply here.
- **The global long pole is `e2e` (~9 min)**, which is `kind`-cluster-spin-up
  bound (not CPU-bound), runs on its own nested-virt pool, and is kept on
  GitHub-hosted. So routing unit/integration jobs to GAG cannot reduce total PR
  wall-clock — the binding constraint isn't on GAG and isn't node-size-sensitive.

Right-sizing therefore serves the **isolation + dogfooding** use case: running
those jobs *correctly and cheaply* on GAG when GAG is used to dogfood itself, not
to beat GitHub on throughput.

## Design decision — do we need multiple runner types?

Separate two ideas that get conflated: **node pools** (VMs) and **runner pod
tiers** (per-job resource requests). Kubernetes `requests` + bin-packing already
let different-sized pods share one node size, so size variance alone is *not* a
reason to multiply node pools.

| Driver | Decision |
|---|---|
| `e2e` needs nested virtualization (Kata/DinD: N-series + `/dev/kvm`) | **Distinct node pool — mandatory, hardware-driven.** Already exists (`dogfood-e2e-setup.sh`). |
| All other jobs (lint, unit-test, coverage, integration, trivial) | **One general worker node size** bin-packs them all — a 10s `shellcheck` pod and a 4-vCPU `-race` pod schedule onto the same node. Do **not** create node pools per job size. |
| Trivial jobs (`shellcheck`/`vendor-check`/`tidy-check`, 10–20s) holding large slots | **Optional 2nd "small" pod tier** — only if Phase 1 shows the packing waste is material. |

**Conclusion: two runner types to start — general + e2e — split by the
nested-virt requirement, not by size.** Add a "small" tier only if the
measurement earns it. Resist per-workflow runner proliferation: every extra
runner label couples the workflow files to the runner taxonomy and is a
maintenance tax. Let `requests` and the autoscaler absorb size variance on a
single general pool.

## Baseline job profile (GitHub-hosted, 2026-06-30)

| Job | Median run | Class |
|---|---|---|
| `shellcheck` | 10s | trivial (I/O-bound) |
| `tidy-check` | 16s | trivial |
| `vendor-check` | 20s | trivial |
| `coverage` | 112s | heavy CPU |
| `unit-test` (`-race`) | 174s | heavy CPU + memory (race detector) |
| `lint` (golangci-lint) | 232s | heavy CPU (longest unit-test job) |
| `integration-test` | 294s | envtest (real apiserver + etcd) |
| `e2e` | 530s | nested-virt (`kind` in DinD) — separate pool |

These are *durations*, not resource usage. Phase 1 measures the actual peak CPU
and memory each consumes, which is what `requests`/`limits` must be set from.

## Resource-model principles

- **`requests` drive scheduling/packing; `limits` drive throttling (CPU) and
  OOM-kill (memory).**
- **Memory is non-compressible** → set `limit ≥ measured peak × ~1.3`; exceeding
  the memory limit kills the job. Start the headroom factor at 1.3 and widen if
  any run OOMs.
- **CPU is compressible** → a CPU `limit` *throttles* bursty jobs (slows them for
  no packing benefit). Prefer **CPU `requests` only, no CPU limit** for CI workers:
  requests still drive packing, while jobs burst to fill otherwise-idle node
  capacity. Keep memory limits for OOM / noisy-neighbor safety. (The current
  template sets a 4-vCPU CPU limit — likely worth removing.)
- **Measure peak, not average** — `-race` and envtest spike well above their
  steady state.

## Plan

**Phase 1 — Measure (needs the live cluster).** Dispatch each job class to GAG
and sample the runner container's usage during the run:

```bash
# while a job pod is Running, poll every ~3s and keep the peak
kubectl top pod -n gag-dogfood --containers <worker-pod> --no-headers
```

Record peak CPU + peak memory + duration per job. Watch `kubectl get events
-n gag-dogfood` for `OOMKilled`, and watch for CPU usage pinned at the limit
(throttling). Sample the **runner** container, not the injected wrapper sidecar.
Output: a measurement table appended to this plan.

**Phase 2 — Derive `requests`/`limits`** per the principles above (memory limit =
peak × 1.3, CPU requests = ~p90, no CPU limit). Decide tiering from the observed
spread: a "small" tier is justified only if trivial jobs land at, say, ≤1 vCPU /
≤1 GiB while heavy jobs need ~4 vCPU / 8–10 GiB.

**Phase 3 — Size the general worker node** as a clean multiple of the dominant
(heavy) pod so stranded capacity is minimal — e.g. a `*-standard-8` spot node
holds 2× a 4-vCPU pod. `c3`/`n2` for faster cores on the CPU-bound jobs, `e2` for
the cost floor. Keep `e2e` on its `n2-standard-4` nested-virt pool, sized for
`kind`-in-DinD memory (measured separately).

**Phase 4 — Validate.** Run the full suite on GAG; confirm zero OOM and no
throttle-induced slowdown; compare job durations and total spot-node-hours to the
baseline; adjust.

**Phase 5 — Persist.** Bake the final `requests`/`limits` into the
`RunnerTemplate`(s) and the node sizes into
[`scripts/dogfood-setup.sh`](../../scripts/dogfood-setup.sh) + the
[runbook](gke-dogfood.md); record the measurement table here, then archive this
plan.

## Open questions

- CPU `limit` vs requests-only — lean requests-only; confirm no noisy-neighbor
  problem on a shared node.
- Memory headroom factor — start at 1.3×, widen on any OOM.
- A 2nd "small" pod tier — decide after Phase 1, not before.
- `e2e` pod sizing (`kind` cluster memory) — measure on the e2e pool separately.
- Spot preemption — a preempted job re-provisions on a fresh pod; confirm the AGC
  re-provisions cleanly (interacts with the [Q247](../STATUS.md#Q247) session-
  recovery investigation).
