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

## Open questions

- CPU `limit` vs requests-only — lean requests-only; confirm no noisy-neighbor
  problem on a shared node.
- Memory headroom factor — start at 1.3×, widen on any OOM.
- A 2nd "small" pod tier — decide after Phase 1, not before.
- `e2e` pod sizing (`kind` cluster memory) — its own track (see *e2e track* above):
  validate the privileged-DinD path works first, then size, then Kata.
- Spot preemption — a preempted job re-provisions on a fresh pod; confirm the AGC
  re-provisions cleanly (interacts with the [Q247](../STATUS.md#Q247) session-
  recovery investigation).
