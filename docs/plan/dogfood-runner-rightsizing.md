# GAG Dogfood CI Runner Right-Sizing

> **Status: ◐ Partial — node-pool disk class RESOLVED (2026-07-05); general-worker
> pod `requests`/`limits` RIGHT-SIZED (2026-07-06); e2e-track pod sizing + an
> optional "small" tier remain.** Tracked as [Q248](../STATUS.md#Q248).
> The worker pod's original `requests`/`limits` (CPU 2/4, memory 4Gi/8Gi) were
> never measured — a guess. They are now replaced with values derived from the
> Phase 1 peak measurement — see [§ Phase 2 — derived requests/limits](#phase-2--derived-requestslimits-2026-07-06-general-workers) below.
> **The dominant capacity ceiling turned out to be the node-pool *disk class*,
> not the pod requests** — see [§ Node-pool disk class](#node-pool-disk-class-the-real-maxworkers-ceiling-q248-2026-07-05) below (resolved 2026-07-05: `pd-balanced`→`pd-standard`, no quota bump).

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
| `e2e` needs nested virtualization (Kata/DinD: N-series + `/dev/kvm`) | **Distinct node pool — mandatory, hardware-driven.** Already exists (`dogfood/e2e-setup.sh`). |
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
[`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh) + the
[runbook](gke-dogfood.md); record the measurement table here, then archive this
plan.

## e2e track — validate, then size (Kata deferred)

e2e is the heaviest, most mis-sizing-sensitive job (`kind`-in-DinD OOMs mid
cluster-bringup if under-sized) and the priciest pool, so it gets its own track.
The chosen sequence is **functional validation first, security hardening later**:

1. **Validate e2e works on GAG via privileged DinD — no Kata.** Privileged DinD
   needs no nested virtualization (dockerd uses the host kernel directly), so it
   runs on a normal spot pool with no N2 / no Kata DaemonSet. The empirical risk
   is `kind`-in-DinD on GKE COS **cgroup v2** — validate by running it, not by
   inspection. This is a **deliberate, platform-gated, dogfood-only** use of the
   `privileged` profile (v2 gates it by the platform-set namespace label
   `actions-gateway.com/security-profile=privileged` + PSA `enforce=privileged`;
   tenants cannot self-elevate). It is **never** a shipped default.
2. **Right-size** the DinD/`kind` pod and the e2e node from measured peak usage
   (same Phase 1–3 method, on the e2e pool).
3. **Re-introduce Kata** (`baseline` profile, KVM micro-VM) for isolation once the
   functional path and sizing are settled — the secure end-state. Tracked as the
   follow-up; the privileged path is the validation scaffold, not the destination.

Setup for step 1 (no nested virt): a dedicated `e2-standard-8` spot e2e pool
(taint `dedicated=e2e`, autoscale 0→2); a `gag-dogfood-e2e` namespace labelled
`security-profile=privileged`; a v2 `ActionsGateway` + a cluster-scoped
`ClusterRunnerTemplate` (runner + a `privileged: true` `docker:dind` sidecar,
`DOCKER_HOST=tcp://localhost:2375`) + `RunnerSet` (`gag-ci-e2e`); and
`e2e-reusable.yml` wired to `GAG_E2E_RUNNER`.

### Validation findings (2026-06-30)

- **Privileged DinD works on GKE COS cgroup v2.** The DinD daemon came up clean
  (`storage-driver=overlay2`, daemon initialized, listening on `:2375`) — the main
  unknown is cleared. GAG *can* host DinD e2e without Kata.
- **v2 routes privileged through a platform-owned `ClusterRunnerTemplate`, gated
  four ways.** A namespaced (tenant) `RunnerTemplate` rejects privileged
  containers; the shape must be a cluster-scoped `ClusterRunnerTemplate`. The
  namespace needs `tenant=managed`, `security-profile=privileged`, the
  `allow-profile-downgrade=allowed` annotation, **and** the platform
  `privileged-profile=allowed` label — none tenant-settable (secure-by-default
  working as intended).
- **e2e needs broad non-GitHub egress.** The job pulls from `get.helm.sh`
  (`setup-helm`), Docker Hub (curl/vault/buildkit), `quay.io` (Calico), and
  `registry.k8s.io` (metrics-server) — all blocked by GAG's default-deny +
  GitHub-only workload `NetworkPolicy`, and **v2 has no managed-NP opt-out**.

### Egress: interim workaround + deferred hardening

- **Interim (accepted):** an **additive allow-all-egress `NetworkPolicy`** on the
  `gag-dogfood-e2e` workload pods (unions with GAG's managed default-deny to open
  egress). This is a **deliberate, documented property of the DinD variant**
  (trusted CI only) — **never** for untrusted PRs (that's the Kata variant's job).
- **Collecting the allowlist:** the e2e job's external destinations are gathered
  (from the job + `dockerd` logs; the deps are also pinned in `e2e-reusable.yml`)
  to seed a future precise allowlist.
- **Durable hardening (deferred, backlog):** the destinations are CDN-fronted
  (Docker Hub→Cloudflare, helm→Azure, quay→Fastly), so an IP allowlist rots and a
  precise **FQDN** allowlist can't be *enforced* on GKE Dataplane V2 today — its
  managed Cilium has no `CiliumNetworkPolicy`, and GKE's `FQDNNetworkPolicy` is
  alpha and GAG doesn't emit it ([Q245](../STATUS.md#Q245)). The durable answer is
  an **in-cluster pull-through mirror** (collapses e2e egress to one in-cluster
  destination — air-gappable, no CDN rot), pairing with the Kata variant for
  untrusted jobs.

## Phase 1 results — general workers (2026-06-30, first pass)

Re-dispatched `unit-test` + organic PR traffic on GAG; `kubectl top` sampled every
5s, peak tracked per pod. Two heavy-job pods captured (lifetime ~145–165s):

| Pod (heavy job class) | Peak CPU | Peak memory | Lifetime |
|---|---|---|---|
| `…6bbf7ca` | 3794m | 1467Mi | ~165s |
| `…6d8f81d` | 3802m | 2134Mi | ~145s |

Envelope: **max 3802m CPU, 2134Mi memory** across all runner pods.

Findings:
- **CPU-bound and throttled.** Peak ~3.8 vCPU sits right against the 4-vCPU
  *limit* → the heavy jobs hit the limit and throttle. Confirms they want ≥4
  cores; switching to **requests-only (no CPU limit)** should let them burst and
  finish faster.
- **Memory is ~4× over-provisioned.** Peak ~2.1 GiB against an 8 GiB limit. Drop
  the memory limit to ~3 GiB (peak × ~1.4) and request ~2 GiB → much better
  bin-packing.

Caveats (first pass): only 2 pods captured; `lint` (the longest job) and the full
`-race` memory peak may not be among them, and `-race` can spike memory higher —
confirm with a targeted run before finalizing. Attribution is by lifetime only.

## Node-pool disk class: the real `maxWorkers` ceiling (Q248, 2026-07-05)

Phase 1 assumed the worker **pod** requests were the binding constraint. Live
capacity work (re-routes #4–#6) surfaced a bigger, unrelated ceiling: the worker
**node** boot **disk class**.

**Root cause.** Each worker node (`workers`/`workers-od`, `e2-standard-4`) had a
100 GB **`pd-balanced`** boot disk. `pd-balanced` is SSD-class and counts against
the regional **`SSD_TOTAL_GB` = 500 GB** quota. With the system pool (2×50 GB) and
base usage also on SSD, ~4 worker nodes exhausted the 500 GB → `maxWorkers ≈ 4`.
This read as a quota shortage but is **self-inflicted**: the CI workload is tiny
(peak ~3.8 GiB mem, 34 % node CPU), so SSD-class disks for worker nodes buy
nothing.

**Fix (no quota bump).** Recreate the worker pool with **`--disk-type=pd-standard`**
(HDD). `pd-standard` counts against **`DISKS_TOTAL_GB` = 4096 GB**, *not* the SSD
quota, so worker capacity becomes **CPU/mem-bound** (`CPUS` = 200 quota), not
SSD-bound. Disk size stays 100 GB for container-image pull scratch (pd-standard is
a *quota class* change, not a size cut, so no scratch-space risk). The CI job
classes (Go build/test/lint/envtest) are CPU/mem-bound, not scratch-IOPS-bound, so
HDD is appropriate; a job class that genuinely needed SSD scratch IOPS would keep
`pd-balanced` and pay the SSD quota.

**Live proof (2026-07-05).** Recreated `workers-od` as `pd-standard` ×4
(`e2-standard-4`). With 4 worker nodes + 2 system nodes up: `DISKS_TOTAL_GB` usage
= **400** (= the 4×100 GB worker disks, now HDD), `SSD_TOTAL_GB` = 220/500 (system
only — workers no longer counted), `CPUS` = 24/200. Under the old `pd-balanced`
config those same 4 workers would have pushed SSD to ~620 > 500 → blocked. The
SSD ceiling is gone; `maxWorkers` is now limited by CPU/mem (≈ 48 `e2-standard-4`
nodes' worth of headroom), not disk quota.

**Persisted.** [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh)
`create_worker_pool` and the mirrored recipe in [`gke-dogfood.md`](gke-dogfood.md)
now provision the `workers` pool with `--disk-type=pd-standard` and `max-nodes=8`.
`RunnerSet.maxWorkers` raised 4→8 (still far under the CPU-bound ceiling; the
dogfood matrix is ~7 concurrent jobs). Remaining Q248 work: the pod
`requests`/`limits` refinement (drop the CPU limit, memory limit → peak×1.3) is
still open — orthogonal to the disk-class fix.

## Phase 2 — derived `requests`/`limits` (2026-07-06, general workers)

Applying the [resource-model principles](#resource-model-principles) to the
[Phase 1 envelope](#phase-1-results--general-workers-2026-06-30-first-pass)
(peak **3802m CPU, 2134Mi memory** across all runner pods):

| Field | Old (guess) | New (measured) | Derivation |
|---|---|---|---|
| CPU `request` | `2` | `2` | Kept. On an `e2-standard-4` worker this packs one heavy pod per node (see allocatable check below), which the ~3.8 vCPU peak wants. |
| CPU `limit` | `4` | **removed** | CPU is compressible — a limit only *throttles* bursty Go build/test/lint jobs (Phase 1 saw peak pinned against the old 4-vCPU limit). Requests-only lets a heavy pod burst to the whole node. |
| memory `request` | `4Gi` | **`2Gi`** | ≈ measured peak (2134Mi). The old 4Gi was ~2× over-provisioned. |
| memory `limit` | `8Gi` | **`3Gi`** | peak × ~1.4 (2134Mi × 1.4 ≈ 3Gi) for OOM headroom on the non-compressible resource. The old 8Gi was ~4× over-provisioned. |

**Why keep CPU `request` at `2` rather than lower it.** A burst-tuning experiment
(gke-dogfood.md re-route) tried `request=1` to pack ~3 pods per node; it packed
cleanly, but the serialization it was chasing turned out to be an AGC
agent-pool-recycling bug (Q259, since resolved), **not** node capacity. At `request=1` with no CPU limit, three co-scheduled heavy jobs (each
peaking ~3.8 vCPU) would contend for one node's ~3.4 allocatable vCPU → ~1.1 vCPU
each → ~3× throttle-induced slowdown, exactly what Phase 4 says to avoid. The
measured peak says a heavy job wants ~a whole `e2-standard-4`; `request=2` reflects
that (one heavy pod per node) while staying schedulable. Trivial jobs
(`shellcheck`/`tidy`/`vendor`, 10–20s, near-zero CPU) over-request under this single
tier — see the deferred "small" tier in [Open questions](#open-questions); the
packing waste (a few short-lived spot-node-slots) is not yet material enough to
justify a second runner label.

### Node-allocatable validation (`maxWorkers` vs the worker pool)

Worker node = `e2-standard-4` (4 vCPU / 16 GiB), `--disk-type=pd-standard`,
autoscale 0→8. GKE's [reserved-resource formula](https://cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes#memory_cpu_reservations)
gives node allocatable ≈ **3920m CPU** (reserve 6%/1%/0.5% tiers = 80m) and
**≈13.3 GiB memory** (reserve 25%/20%/10% tiers + 100Mi eviction). GKE system
DaemonSets that tolerate the `dedicated=workers` taint (kube-proxy, DPv2 `anetd`,
metadata, logging) consume a further ~0.4–0.6 vCPU / ~0.4–0.6 GiB, leaving
**~3.3–3.5 vCPU and ~12.7 GiB available for worker pods per node**.

- **CPU is binding.** `request=2` → `2 + 2 = 4 > ~3.4`, so **exactly one worker pod
  schedules per node**. `maxWorkers=8` therefore fans out to **≤ 8 worker nodes**,
  matching the pool's `max-nodes=8`. Consistent; no over- or under-commit.
- **Memory is slack, not binding.** At `request=2Gi` a node could hold ~6 pods by
  memory, but CPU caps it at 1 — so the 2Gi request / 3Gi limit never gates
  scheduling and leaves ample OOM headroom.

The sizing is therefore **CPU-bound at one heavy pod per node**, and `maxWorkers=8`
is validated against `max-nodes=8`. (This is orthogonal to — and downstream of —
the disk-class fix that lifted the node ceiling off the SSD quota.)

### AGC control-plane pod — no change (defaults are right-sized)

The per-tenant AGC pod uses the platform default footprint
(`defaultAGCResources`: requests `500m`/`2Gi`, limits `2`/`4Gi`; overridable via
`spec.agcResources`). The dogfood `ActionsGateway` sets **no** `agcResources`
override, so it inherits those defaults, and it schedules on the `default-pool`
(`e2-standard-2`, alongside the single-replica GMC and Athens — the taint keeps it
off the worker pool). Across the extensive live dogfood runs (Q224/Q259/Q260/Q267
re-routes, up to `maxListeners=16` / `maxWorkers=8`) the AGC has never OOMed or
CPU-starved at these defaults, and its ~2Gi request fits comfortably beside the GMC
(`10m`/`64Mi`) and Athens on the ~5.9 GiB-allocatable system node. **Decision: keep
the AGC at platform defaults for dogfood** — a smaller override is unwarranted
(2Gi is already modest for a session-multiplexing control plane) and any change
should follow an AGC-specific `kubectl top` measurement, not a guess.

## Phase 5 — persisted (2026-07-06)

The right-sized general-worker `requests`/`limits` are baked into the dogfood
`RunnerTemplate` in [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh)
(`apply_cr`) and mirrored in the [`gke-dogfood.md`](gke-dogfood.md) runbook. The
node sizes and `maxWorkers=8` were already persisted with the disk-class fix. The
tenant-onboarding quota formula is unchanged and remains correct — it sums only
declared container `limits`, so dropping the worker CPU limit simply drops that
term (documented there as "a term with no value drops out"). **Remaining Q248
work** is the e2e-track pod sizing (deferred to live e2e validation — see
[§ e2e track](#e2e-track--validate-then-size-kata-deferred)) and the optional
"small" tier; the general-worker refinement is complete.

## Open questions

- ~~CPU `limit` vs requests-only~~ — **RESOLVED (general workers):** requests-only,
  no CPU limit. No noisy-neighbor risk because `request=2` packs one heavy pod per
  `e2-standard-4`, so a bursting pod has the node to itself.
- Memory headroom factor — set to ~1.4× (peak 2134Mi → 3Gi limit); widen on any OOM.
- A 2nd "small" pod tier — **still deferred.** Phase 1 measured only heavy jobs;
  trivial jobs over-request a full node under the single tier, but the waste (a few
  short-lived spot-node-slots) isn't yet material. Add only if it becomes so.
- `e2e` pod sizing (`kind` cluster memory) — its own track (see *e2e track* above):
  validate the privileged-DinD path works first, then size, then Kata.
- Spot preemption — a preempted job re-provisions on a fresh pod; confirm the AGC
  re-provisions cleanly (interacts with the [Q247](../STATUS.md#Q247) session-
  recovery investigation).
