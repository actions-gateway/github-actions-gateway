# GPU and Accelerated CI

> **Status: goal stated 2026-08-07, foundations shipped, first-class support open.** This is a map and a definition of done, not a design.
> Both open deliverables are deferred pending a concrete workload to design against, which is deliberate: the shapes below are guesses until a real GPU CI job exists to measure.

## Why this is a goal

GPU is the reason multi-tenancy is worth securing.
The [secure multi-tenant OSS CI](secure-multi-tenant-oss-ci.md) argument turns on nodes that are big and expensive enough that a cluster per tenant strands capacity, and accelerators are the sharpest version of that: an idle A100 costs more per hour than most teams' entire CPU fleet, and partitioned GPU capacity cannot be shifted to whoever needs it at 3am.

So GPU is not one workload among many here.
It is the case that makes the whole product's economics work, and the case where the competition is thinnest, because a managed runner service cannot run on the accelerators you already bought.

## What ships today

More than the roadmap implies.
These are foundations, not placeholders:

| | Mechanism | Evidence |
|---|---|---|
| **Zero idle accelerator** | A worker pod is created when a job is acquired and deleted on completion, so a GPU node returns to the scheduler the instant a job ends. No `minRunners` floor is needed to mask cold start | [features.md](../features.md) |
| **GPU work cannot be starved by CPU work** | `priorityTiers` maps `PriorityClass` objects to cumulative pod-count thresholds, so the first N pods of a GPU set get a preempting class and are guaranteed to schedule when quota is contended | measured, Q423 |
| **A displaced GPU job is not lost** | The job a preempting tier displaces concludes at GitHub and re-runs itself, rather than hanging until the 24-hour queue timeout | measured, Q497 |
| **Right-sizing never touches accelerators** | The sizing profiles derive CPU and memory from measured per-job peaks and leave GPU resources alone by construction | [worker-rightsizing.md](../operations/worker-rightsizing.md) |
| **Per-tenant GPU utilization** | Metrics scoped per tenant and runner group, so a team can argue for its own quota without cluster-wide visibility | [observability-metrics.md](../operations/observability-metrics.md) |
| **Capacity planning for mixed GPU shapes** | Worked scenarios for 8-GPU, 2-GPU, and CPU-only sets sharing one namespace quota | [appendix-e](../design/appendix-e-capacity-planning.md#scenario-1-team-with-3-runnergroups-and-20-concurrent-gpu-jobs-at-peak) |

What is missing is not the plumbing.
It is the conventions that make a GPU runner set feel native ([Q216](../queue/Q216.md)) and the ability to express a job that needs more than one node at once ([Q718](../queue/Q718.md)).

## The collision with the security goal

**On managed cloud GPU, isolation and the accelerator do not compose the way GAG's defaults assume**, and no Queue row says so.

Kata micro-VM workers are the shipped answer for giving a job root without a shared kernel, and they are the default in GAG's own end-to-end CI.
Nested virtualization is *not* what blocks them on GPU nodes: A2, A3, and G2 all take `--enable-nested-virtualization` (measured 2026-08-02, [machine-family row](../operations/kata-dind-workloads.md#prerequisite--nested-virtualization-nodes)).
The constraint sits one layer down, and it leaves an operator two real options.

**gVisor runs on cloud GPU, and stops at the driver.** GKE Sandbox supports GPUs: GA on H100 80GB, A100 80GB/40GB, L4, and T4, preview on H200, B200, GB200, and RTX PRO 6000.
Google bounds its own claim, "GKE Sandbox doesn't mitigate all NVIDIA driver vulnerabilities, but retains protection against Linux kernel vulnerabilities", and gVisor says the same from the other side: it "is much less effective at mitigating vulnerabilities within the NVIDIA GPU drivers themselves, because gVisor passes through calls to be handled by the kernel module" ([GKE Sandbox](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods), [gVisor GPU support](https://gvisor.dev/docs/user_guide/gpu/), both fetched 2026-08-08).

That uncovered half is where the recent critical escapes are.
CVE-2024-0132 (2024-09) is a time-of-check/time-of-use race in the NVIDIA Container Toolkit that lets a crafted image reach the host filesystem; CVE-2025-23359 (2025-02, CVSS 9.0) is its incomplete fix, reopening the same race; CVE-2025-23266 "NVIDIAScape" (2025-07, CVSS 9.0) makes the privileged `createContainer` OCI hook inherit environment from the image, so an `LD_PRELOAD` pointing into the container's own filesystem loads inside a host process, in three lines of Dockerfile (fixed in Container Toolkit 1.17.8).
All three are toolkit bugs rather than kernel bugs, which is exactly the half a sandbox does not take.
That is a statement about coverage boundaries, not a claim that gVisor is unsafe or that any of these was exploited against a GAG deployment.

**Kata covers the driver and asks for hardware you control.** NVIDIA documents the Kata passthrough path as needing hardware virtualization and Access Control Services (ACS) enabled in BIOS, IOMMU groups, *no* NVIDIA driver installed on the host (the GPU binds to `vfio-pci` instead), and every GPU on the node assigned to one Kata VM. vGPU is unsupported, containerd only, and "support for Kata Containers is limited to the implementation described on this page" ([NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/deploy-kata-containers.html), fetched 2026-08-08).
A managed node pool exposes none of those knobs, which is why [appendix-b](../design/appendix-b-worker-isolation.md) rates device passthrough as limited under a micro-VM.

**Both paths are NVIDIA-shaped, which makes AMD a different problem rather than a bigger one.** gVisor's GPU support is `nvproxy`: it "supports running most CUDA applications on preselected versions of NVIDIA's open source driver", and there is no AMD or ROCm path in it.
Kata's kernel packaging takes a GPU vendor flag that accepts only `intel` and `nvidia`, dying with "GPU vendor only support intel and nvidia" on anything else, and its own passthrough guide still reads "PLACE HOLDER: for other GPU vendors (e.g., AMD, Intel)".
Measured 2026-08-12 against [gVisor GPU support](https://gvisor.dev/docs/user_guide/gpu/) at release `release-20260803.0`, and against Kata [`build-kernel.sh`](https://github.com/kata-containers/kata-containers/blob/4d1e78da2f7d5ff6616d5674510a134556a1e97d/tools/packaging/kernel/build-kernel.sh) at `4d1e78d` (2026-07-20) and its [GPU passthrough guide](https://github.com/kata-containers/kata-containers/blob/978f40d631d25b18f258baacfd4db497dacdedf3/docs/use-cases/GPU-passthrough-and-Kata.md) at `978f40d` (2026-04-22), both the current `main` state of those files after the 4.0.0 release.
So an isolated AMD worker is not the NVIDIA work with a different device plugin.
It is raw VFIO passthrough into a guest kernel no upstream project builds, which is a driver stack GAG would own rather than a configuration it would document.
That is worth stating because [appendix-f](../design/appendix-f-cost-model.md#comparing-accelerators-memory-per-dollar-not-dollars-per-gpu-hour) rates Instinct 2–3× ahead of NVIDIA on memory per dollar.
The rate is real; what nobody ships is an isolation runtime to spend it under, so the same appendix now carries the caveat next to the table.

The consequence is a positioning constraint, not just an engineering one.
The strongest audience for GAG's security story and the strongest audience for its GPU story are the same people, and on managed cloud they still have to choose between a sandbox that stops at the driver and a VM boundary they cannot configure.
On-premises and reserved hardware is where both hold at once, which is also where the [location filter](../alternatives.md#location-location-location) already removes most of the competition.
That is a coherent story, but only if it is stated rather than discovered by an operator after they buy A3 nodes.

## Why gang scheduling is structurally hard, and why that matters

[Q718](../queue/Q718.md) is not a bigger version of Q216.
A multi-node training or distributed-test job needs **N pods co-scheduled in one topology domain** (NVLink, InfiniBand).
The runner-scale-set protocol advertises capacity to GitHub as **a single integer**.
A gang requirement is a placement predicate, not a count, so there is no integer that expresses "four pods, same domain, or none".

This is the one place GAG's pre-claim admission gate is **structurally** defensible rather than merely ahead.
Elsewhere, an implementation that wanted to gate intake could reach the same outcome by capping the advertised number ([competitive-analysis-2026-08](competitive-analysis-2026-08.md#the-pre-claim-seat-is-not-structural-and-the-docs-must-stop-implying-it-is) sets out why that makes the seat contested rather than a moat).
A gang has no such workaround: the decision has to be made where the placement information lives, which is the control plane, before the claim.

So Q718 is worth more than its GPU utility.
It is the deliverable that turns a contested differentiator into an uncontested one, and it should be weighed that way when it competes for priority.

## Definition of done

Each demonstrated rather than argued:

1. A GPU runner set is declared without hand-writing `nodeSelector`, tolerations, and `runtimeClassName`, and the declaration is checked at admission rather than failing at schedule time.
2. A GPU node's accelerators are discovered rather than declared, so a set does not silently request a resource the node pool cannot serve.
3. A multi-node job either gets all its pods in one topology domain or is not claimed, and the run stays queued at GitHub rather than being claimed and cancelled.
4. An isolated GPU job runs under Kata on a validated bare-metal or dedicated reference architecture, with the kernel and machine family named.
5. The quota model answers what a GPU gang costs a tenant, since a partially admitted gang consumes quota while doing no work.
6. A tenant can see its own accelerator utilization and idle time, because the entire economic argument is that idle accelerator time approaches zero, and an unmeasured claim is not one an operator can act on.

## Explicitly out of scope

- **Scheduling research.** If Kueue or Volcano can gang-schedule and place by topology, the deliverable is integrating one, not writing a scheduler.
  GAG's contribution is the intake decision, which is upstream of both.
- **GPU sharing between jobs** (MIG partitioning, time-slicing, MPS).
  A worker runs one job and is deleted; a shared accelerator crosses that boundary and crosses a tenant boundary with it.
  Not without the isolation review [caching-and-worker-storage.md](caching-and-worker-storage.md) reframes for the cache.
- **Cloud GPU under Kata.** Not a gap to close, a platform fact to state: the BIOS, host-driver, and whole-GPU-per-guest requirements above are not knobs a managed node pool exposes.
  It reopens if a managed cloud offers Kata with GPU passthrough, or if gVisor gains meaningful coverage of the NVIDIA driver surface.
- **Non-NVIDIA accelerators as a first target.** None of TPUs, Gaudi, or AMD has a workload asking.
  They are not one problem either: AMD is the only one the cost model actively recommends, and it is the one with no isolation runtime at all, per the section above.
- **Benchmarking against managed GPU CI.** Those services do not run on hardware you already own, which is the entire premise here.

## What the site may claim today

Current state only:

> Workers exist only while a job runs, so a GPU node returns to the scheduler the moment a job ends, and priority tiers keep cheap CPU jobs from starving expensive GPU work under a shared quota.
> Both are measured.
> First-class GPU conventions and multi-node gang scheduling are on the roadmap and are not claimed.

Do not claim GPU Operator or Node Feature Discovery awareness, gang scheduling, topology-aware placement, or isolated GPU workers on cloud.

## Deliverables

Shipped: scale-to-zero workers, priority tiers with reserved floors, disruption re-run for preempted GPU jobs, GPU-safe right-sizing, per-tenant utilization metrics, mixed-shape capacity planning.

Open, with rows: [Q216](../queue/Q216.md) (first-class GPU conventions, GPU Operator and NFD awareness), [Q718](../queue/Q718.md) (gang scheduling and topology-aware placement), [Q407](../queue/Q407.md) (`ProvisioningRequest` capacity probe, which is how a gang would ask an autoscaler whether it can be placed before the claim).

Both open rows are **demand-gated on purpose**: they need a real GPU CI workload to design against, and Q216's own note says so.
Designing them against an imagined one is how the wrong abstraction ships.

## Gaps with no row yet

Each needs a decision before it needs a row:

1. **What a GPU gang costs against a tenant quota.** Kubernetes `ResourceQuota` counts pods that exist.
   A gang that is admitted but unplaceable consumes quota while doing nothing, and the pre-claim gate has no concept of a reservation it holds across several pods.
2. **Whether isolated GPU CI is a supported configuration or a documented one.** The bare-metal Kata path is described but has never been run.
   Claiming it as a reference architecture requires actually standing one up, which needs hardware nobody has allocated.
3. **How a GPU set expresses its accelerator requirement.** Today it is a raw `nvidia.com/gpu` request inside a pod template.
   Whether the CRD should carry a first-class field is an API-surface decision with a compatibility cost, and [api-review.md](../development/api-review.md) is the gate it has not been through.
