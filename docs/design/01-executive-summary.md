# 1. Executive Summary & Problem Statement

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)

---

## Executive Summary

### For Executive Leadership: GPU Utilization & Cost Justification

**Every idle GPU runner pod is burning money with nothing to show for it.** ARC's minimum replica count is 1, so a tenant with 10 GPU runner sets holds at least 10 GPUs allocated at all times — whether jobs are running or not. At 1–8 GPUs per runner, a single tenant can idle 80 GPUs between pull requests and scheduled tests, paying full cloud rates for hardware that isn't doing work.

- **Idle runner allocation — GPUs held by runner pods between jobs — drops to zero.** At ARC's minimum of 1 pod per runner set, 10 clusters with 5 tenants and 5 runner sets each holds 250 GPUs allocated around the clock. At a conservative 50% idle time and 1 GPU per runner, that's ~90,000 wasted GPU-hours per month. At typical on-demand rates, this system eliminates roughly **$180K–$360K in monthly idle GPU spend** — over $2M annually. *(Estimate based on 1–2 GPUs per runner set at $2/GPU-hour; to be validated with observed utilization data.)*
- **The ask:** engineering investment to build and operate this system. At this scale of idle spend, the build cost is expected to pay back within weeks of production operation.

### For Tenant Teams: Self-Service & Cost Ownership

**Tenants today depend on the platform team for every runner set change.** Adding a test suite, rebalancing GPU quota between runner sets, or adjusting scale limits requires filing a request and waiting. Teams iterating quickly — especially those adding new GPU test suites frequently — are blocked by this overhead and can't move at their own pace.

- **Full self-service via a single custom resource.** Teams declare all their runner sets, GPU allocations, and scale limits in one `ActionsGateway` CR they own and manage in their own namespace. No platform team involvement after initial onboarding.
- **Rebalancing is a one-line config change.** Shifting GPU quota between runner sets to make room for a new test suite is a self-managed edit, not a ticket.
- **Utilization data to compete for scarce quota.** Tenants need to show high utilization to be approved for more GPU quota. This system makes utilization a first-class per-tenant metric, giving teams the data to make their case.

### For Platform Engineering: Operational Leverage & Shift Left

**The platform team is the bottleneck for every tenant runner change.** Each new test suite, quota adjustment, or scaling tweak lands as a ticket. First-line debugging — why isn't my runner scaling? — escalates to platform instead of being ownable by the tenant who knows their workload.

- **A clear multi-tenant abstraction shifts ownership to tenants.** The Gateway Manager Controller provisions a fully isolated gateway instance per tenant from a single namespace-scoped custom resource. Once deployed, tenants manage their own runner sets end-to-end.
- **Fewer escalations.** Tenants who own their own configuration can diagnose their own runner behavior. Platform's job shrinks to operating the controller — not hand-holding individual runner configs.
- **One operator to manage, not N per-tenant deployments.** The platform team deploys the Gateway Manager Controller once at the cluster level. Tenants handle everything beneath it.

---

## Overview for Architects & Engineers

### The Problem: Scheduling Fairness in a Shared Namespace

In a multi-tenant Kubernetes cluster, teams running multiple types of GitHub Actions self-hosted runners face a scheduling fairness problem that existing solutions — including Actions Runner Controller (ARC) — cannot address. When a namespace's `ResourceQuota` is shared across runner groups, smaller and cheaper runner pods can exhaust available quota before larger pods have a chance to schedule. The result is that GPU runners — which carry the most expensive hardware requirements and the longest queue times — are systematically starved by a flood of small CPU runner pods claiming quota first.

ARC provides no mechanism to express minimum scheduling guarantees across runner sets within a shared pool. Each `RunnerScaleSet` has an independent `maxRunners` cap, making it impossible to declare "GPU runners must always be able to claim at least N slots, regardless of how many CPU runners are active." The only workaround — lowering CPU runner caps to protect headroom — still cannot guarantee that headroom is actually available for GPU runners when they need it, and introduces a separate per-set configuration burden that grows with the number of runner types.

This design addresses the problem through a `priorityTiers` field on each `RunnerGroup`: a ranked list of Kubernetes `PriorityClass` assignments, cumulative pod-count thresholds, and a `preemptionPolicy` per tier. The first N pods of a GPU runner group are assigned a preempting `PriorityClass` (`preemptionPolicy: PreemptLowerPriority`) and will displace lower-priority CPU runner pods when the namespace is contended — guaranteeing they schedule. All subsequent tiers use `preemptionPolicy: Never`, so burst and best-effort pods gain scheduling priority over lower-priority pending work without evicting any running jobs. A final threshold caps total concurrency per group. This confines eviction risk to the minimum guaranteed floor pods only, and lets a platform team express "GPU runners always get at least 5 slots, can burst to 20 without evicting anything, and are capped at 30," all enforced by the Kubernetes scheduler against a single shared namespace `ResourceQuota`.

When eviction does occur — either from the floor tier preempting lower-priority pods, or from external pressure such as node memory exhaustion — the AGC automatically re-queues the affected job without user intervention. The Job Lock Renewer detects the `Evicted` pod status, immediately stops renewal to prompt a fast GitHub cancellation, and calls GitHub's rerun API to reschedule the job. A configurable retry budget prevents infinite loops on persistently failing workloads. Jobs that exhaust their retry budget surface via a dedicated metric rather than silently disappearing.

### GPU Node Utilization

For teams running GPU-accelerated workflows, runner pod lifecycle is a direct driver of hardware utilization. GPU runner pods hold GPU node allocations while waiting for work — even between jobs. Those GPU resources are unavailable to other workloads during the wait, wasting some of the most expensive capacity in the cluster.

This design eliminates idle GPU allocation entirely. Worker pods are provisioned on-demand after a job is acquired and garbage-collected immediately on completion. The AGC itself runs on a CPU-only node pool, so no GPU capacity is consumed by the controller. GPU nodes are returned to the cluster scheduler the moment each job completes and remain available for other workloads until the next job arrives. Across a shared GPU node pool, this translates directly into higher effective utilization without requiring additional hardware.

### For Teams Migrating from Host-Machine or VM Runners

The arguments above apply equally to teams already running Kubernetes-native runners. For teams migrating from runners on host machines or virtual machines — where runners are registered as persistent processes rather than Kubernetes pods — the Kubernetes model itself introduces an additional overhead worth quantifying.

Each idle runner pod carries the full weight of the `Runner.Listener` process: a minimum of ~256 MiB of reserved memory per slot. A team running 1,000 concurrent runner slots must hold ~256 GiB of memory in reserve across the cluster just to keep runners available, regardless of whether any jobs are pending. In contrast, the goroutine-based session model this system uses averages ~60 KiB resident per virtual runner slot (see [Appendix A](appendix-a-capacity-slos.md)) — a reduction of over 4,000× per session. At the same 1,000-session ceiling, the AGC's working set is roughly 60 MiB.

The IP address problem compounds this. Every runner pod consumes a cluster IP. In clusters already dense with application workloads, 1,000 idle runner pods exhaust a significant fraction of the available address space — a hard limit that cannot be worked around without re-addressing the cluster. Each pod's long-poll connection also generates sustained polling noise through network firewalls, adding to the operational burden of teams managing cluster egress.

### Design Goals

The system is designed to satisfy four requirements that existing solutions do not address together:

1. **Shared quota with per-group priority guarantees.** All `RunnerGroup`s within a tenant draw from a single namespace-level `ResourceQuota`. Each group optionally defines a `priorityTiers` list to express a preemption floor, an opportunistic burst range, and a hard concurrency ceiling — enforced by the Kubernetes scheduler without idle resource reservation. Only the floor tier carries `preemptionPolicy: PreemptLowerPriority`; all higher tiers use `Never`, confining eviction risk to the minimum guaranteed slots.
2. **Automatic eviction retry.** When a worker pod is evicted, the AGC detects it, fast-cancels the job lock, and re-queues the job via GitHub's rerun API — no user action required. A configurable retry budget caps automatic retries per job; exhausted budgets are surfaced as a metric rather than silent failures.
3. **Eliminate idle resource overhead.** Virtual runner sessions are goroutines, not pods. Compute is provisioned only when a job is acquired and released immediately on completion — including GPU allocations.
4. **Per-tenant egress IP isolation.** Each tenant's GitHub traffic exits through a dedicated proxy pool, enabling IP-based allowlisting, clean audit trails, and contained blast radius.
5. **Self-service multi-tenant onboarding.** A team creates one `ActionsGateway` CR in their existing namespace and receives a fully isolated gateway instance. No cluster-admin involvement is required after initial GMC installation.

### The Solution: A Four-Tier Virtualized Gateway

This document outlines the design for a **four-tier system** that addresses these problems at their root:

* **Tier 1 — Gateway Manager Controller (GMC):** A cluster-scoped operator that watches namespace-scoped `ActionsGateway` Custom Resources across all namespaces and provisions isolated, fully independent gateway instances — one per CR. It owns the lifecycle of all AGC-related resources within the tenant's existing namespace: RBAC, network policies, resource quotas, and the AGC deployment itself.

* **Tier 2 — Actions Gateway Controller (AGC):** A Go-based operator deployed once per tenant by the GMC. It acts as a highly concurrent, virtualized gateway, scaling lightweight **Go routines** to multiplex thousands of virtual runner sessions. Compute resources (Pods) are provisioned purely on-demand, executing jobs ephemerally and tearing down immediately upon completion.

* **Tier 3 — Egress Proxy Pool:** A horizontally autoscaled pool of stateless HTTPS CONNECT proxy pods, deployed per tenant by the GMC. All GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs isolated from other tenants. This enables per-team IP allowlisting on the GitHub side, produces clean per-tenant audit trails, and limits the blast radius if any one tenant's traffic is flagged.

* **Tier 4 — Ephemeral Worker Pod:** A short-lived, single-use pod that executes exactly one workflow job. Provisioned on-demand by the AGC after a job is acquired and garbage-collected immediately on completion. Because worker pods exist only while a job is running, there are zero idle compute resources between jobs — the cluster pays only for work actually being done.

### Operational Model

This architecture makes it practical to operate GitHub Actions self-hosted runners in a multi-tenant Kubernetes cluster: a platform team deploys the GMC once at the cluster level, while individual teams create an `ActionsGateway` resource in their own existing namespace and receive fully isolated runner capacity — no cluster-admin involvement required after initial GMC installation.

The four-tier design is intentionally more complex than a simple self-hosted runner deployment. That complexity is load-bearing: it is what makes goroutine-level multiplexing, per-tenant egress isolation, and zero-idle compute possible simultaneously in a shared cluster. For a detailed evaluation of simpler alternatives and the reasons they fall short at multi-tenant scale, see [Appendix D](appendix-d-alternatives-considered.md).

---

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)
