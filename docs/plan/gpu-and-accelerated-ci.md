# GPU and Accelerated CI

> **Status: goal stated 2026-08-07, foundations shipped, first-class support
> open.** This is a map and a definition of done, not a design. Both open
> deliverables are deferred pending a concrete workload to design against, which
> is deliberate: the shapes below are guesses until a real GPU CI job exists to
> measure.

## Why this is a goal

GPU is the reason multi-tenancy is worth securing. The
[secure multi-tenant OSS CI](secure-multi-tenant-oss-ci.md) argument turns on
nodes that are big and expensive enough that a cluster per tenant strands
capacity, and accelerators are the sharpest version of that: an idle A100 costs
more per hour than most teams' entire CPU fleet, and partitioned GPU capacity
cannot be shifted to whoever needs it at 3am.

So GPU is not one workload among many here. It is the case that makes the whole
product's economics work, and the case where the competition is thinnest,
because a managed runner service cannot run on the accelerators you already
bought.

## What ships today

More than the roadmap implies. These are foundations, not placeholders:

| | Mechanism | Evidence |
|---|---|---|
| **Zero idle accelerator** | A worker pod is created when a job is acquired and deleted on completion, so a GPU node returns to the scheduler the instant a job ends. No `minRunners` floor is needed to mask cold start | [features.md](../features.md) |
| **GPU work cannot be starved by CPU work** | `priorityTiers` maps `PriorityClass` objects to cumulative pod-count thresholds, so the first N pods of a GPU set get a preempting class and are guaranteed to schedule when quota is contended | measured, Q423 |
| **A displaced GPU job is not lost** | The job a preempting tier displaces concludes at GitHub and re-runs itself, rather than hanging until the 24-hour queue timeout | measured, Q497 |
| **Right-sizing never touches accelerators** | The sizing profiles derive CPU and memory from measured per-job peaks and leave GPU resources alone by construction | [worker-rightsizing.md](../operations/worker-rightsizing.md) |
| **Per-tenant GPU utilization** | Metrics scoped per tenant and runner group, so a team can argue for its own quota without cluster-wide visibility | [observability-metrics.md](../operations/observability-metrics.md) |
| **Capacity planning for mixed GPU shapes** | Worked scenarios for 8-GPU, 2-GPU, and CPU-only sets sharing one namespace quota | [appendix-e](../design/appendix-e-capacity-planning.md#scenario-1-team-with-3-runnergroups-and-20-concurrent-gpu-jobs-at-peak) |

What is missing is not the plumbing. It is the conventions that make a GPU
runner set feel native ([Q216](../STATUS.md#Q216)) and the ability to express a
job that needs more than one node at once ([Q718](../STATUS.md#Q718)).

## The collision with the security goal

**GPU and Kata do not compose on cloud today**, and no Queue row says so.

Kata micro-VM workers are the shipped answer for giving a job root without a
shared kernel, and they are the default in GAG's own end-to-end CI. They need
nested virtualization. GKE's GPU machine families (A2, A3, G2) **do not support
nested virtualization**, so the two defaults are mutually exclusive on managed
cloud GPU nodes. PCIe passthrough of an NVIDIA or AMD GPU into a Kata guest
works from bare metal, which makes bare metal or dedicated instances the
reference architecture for isolated GPU CI. Both facts are already documented
([kata-dind-workloads.md](../operations/kata-dind-workloads.md#caveats-and-limitations),
[appendix-b](../design/appendix-b-worker-isolation.md) rates device passthrough
as limited under a micro-VM).

The consequence is a positioning constraint, not just an engineering one. The
strongest audience for GAG's security story and the strongest audience for its
GPU story are the same people, and today they must choose. On-premises and
reserved hardware is where both hold at once, which is also where the
[location filter](../alternatives.md#location-location-location) already removes
most of the competition. That is a coherent story, but only if it is stated
rather than discovered by an operator after they buy A3 nodes.

## Why gang scheduling is structurally hard, and why that matters

[Q718](../STATUS.md#Q718) is not a bigger version of Q216. A multi-node training
or distributed-test job needs **N pods co-scheduled in one topology domain**
(NVLink, InfiniBand). The runner-scale-set protocol advertises capacity to
GitHub as **a single integer**. A gang requirement is a placement predicate, not
a count, so there is no integer that expresses "four pods, same domain, or none".

This is the one place GAG's pre-claim admission gate is **structurally**
defensible rather than merely ahead. Elsewhere, an implementation that wanted to
gate intake could reach the same outcome by capping the advertised number
([competitive-analysis-2026-08](competitive-analysis-2026-08.md#the-pre-claim-seat-is-not-structural-and-the-docs-must-stop-implying-it-is)
sets out why that makes the seat contested rather than a moat). A gang has no
such workaround: the decision has to be made where the placement information
lives, which is the control plane, before the claim.

So Q718 is worth more than its GPU utility. It is the deliverable that turns a
contested differentiator into an uncontested one, and it should be weighed that
way when it competes for priority.

## Definition of done

Each demonstrated rather than argued:

1. A GPU runner set is declared without hand-writing `nodeSelector`,
   tolerations, and `runtimeClassName`, and the declaration is checked at
   admission rather than failing at schedule time.
2. A GPU node's accelerators are discovered rather than declared, so a set does
   not silently request a resource the node pool cannot serve.
3. A multi-node job either gets all its pods in one topology domain or is not
   claimed, and the run stays queued at GitHub rather than being claimed and
   cancelled.
4. An isolated GPU job runs under Kata on a validated bare-metal or dedicated
   reference architecture, with the kernel and machine family named.
5. The quota model answers what a GPU gang costs a tenant, since a partially
   admitted gang consumes quota while doing no work.
6. A tenant can see its own accelerator utilization and idle time, because the
   entire economic argument is that idle accelerator time approaches zero, and
   an unmeasured claim is not one an operator can act on.

## Explicitly out of scope

- **Scheduling research.** If Kueue or Volcano can gang-schedule and place by
  topology, the deliverable is integrating one, not writing a scheduler. GAG's
  contribution is the intake decision, which is upstream of both.
- **GPU sharing between jobs** (MIG partitioning, time-slicing, MPS). A worker
  runs one job and is deleted; a shared accelerator crosses that boundary and
  crosses a tenant boundary with it. Not without the isolation review
  [caching-and-worker-storage.md](caching-and-worker-storage.md) reframes for
  the cache.
- **Cloud GPU under Kata.** Not a gap to close, a hardware fact to state. It
  reopens if a cloud ships nested virtualization on an accelerator family.
- **Non-NVIDIA accelerators as a first target.** TPUs, Gaudi, and AMD are the
  same shape of problem and none of them has a workload asking.
- **Benchmarking against managed GPU CI.** Those services do not run on
  hardware you already own, which is the entire premise here.

## What the site may claim today

Current state only:

> Workers exist only while a job runs, so a GPU node returns to the scheduler
> the moment a job ends, and priority tiers keep cheap CPU jobs from starving
> expensive GPU work under a shared quota. Both are measured. First-class GPU
> conventions and multi-node gang scheduling are on the roadmap and are not
> claimed.

Do not claim GPU Operator or Node Feature Discovery awareness, gang scheduling,
topology-aware placement, or isolated GPU workers on cloud.

## Deliverables

Shipped: scale-to-zero workers, priority tiers with reserved floors,
disruption re-run for preempted GPU jobs, GPU-safe right-sizing, per-tenant
utilization metrics, mixed-shape capacity planning.

Open, with rows: [Q216](../STATUS.md#Q216) (first-class GPU conventions, GPU
Operator and NFD awareness), [Q718](../STATUS.md#Q718) (gang scheduling and
topology-aware placement), [Q407](../STATUS.md#Q407) (`ProvisioningRequest`
capacity probe, which is how a gang would ask an autoscaler whether it can be
placed before the claim).

Both open rows are **demand-gated on purpose**: they need a real GPU CI workload
to design against, and Q216's own note says so. Designing them against an
imagined one is how the wrong abstraction ships.

## Gaps with no row yet

Each needs a decision before it needs a row:

1. **What a GPU gang costs against a tenant quota.** Kubernetes `ResourceQuota`
   counts pods that exist. A gang that is admitted but unplaceable consumes
   quota while doing nothing, and the pre-claim gate has no concept of a
   reservation it holds across several pods.
2. **Whether isolated GPU CI is a supported configuration or a documented
   one.** The bare-metal Kata path is described but has never been run. Claiming
   it as a reference architecture requires actually standing one up, which needs
   hardware nobody has allocated.
3. **How a GPU set expresses its accelerator requirement.** Today it is a raw
   `nvidia.com/gpu` request inside a pod template. Whether the CRD should carry
   a first-class field is an API-surface decision with a compatibility cost, and
   [api-review.md](../development/api-review.md) is the gate it has not been
   through.
