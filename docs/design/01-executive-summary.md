# 1. Executive Summary & Problem Statement

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)

---

## Executive Summary

### For Executive Leadership

**Make GitHub Actions self-hosted runners safe and economical to operate at multi-tenant enterprise scale.** This system turns the shared Kubernetes cluster hosting CI/CD into a managed internal platform: one operator, deployed once, that lets dozens of tenant teams onboard themselves and run thousands of jobs per hour — without the platform team becoming the bottleneck and without any one team's incident becoming everyone's outage.

- **Engineering velocity for product teams.** Tenant teams onboard themselves via a single custom resource in their own namespace and own every subsequent change — new runner types, GPU quota rebalancing, scale limits. Changes that today wait days for a platform-team ticket ship in minutes. Teams iterating quickly — especially those adding new GPU test suites — are no longer rate-limited by platform availability.

- **GPU utilization that holds under contention.** GPU runners are guaranteed scheduling slots even when cheap CPU runners flood a shared quota, so the most expensive hardware actually runs instead of losing the race. GPU nodes return to the cluster scheduler the moment each job completes, freeing them for other workloads between CI jobs. Per-tenant utilization metrics give finance and platform leadership the data to justify GPU allocations and reclaim under-used capacity.

- **No manual recovery from infrastructure incidents.** Preempted, OOM-killed, and node-lost jobs are fast-cancelled at GitHub and rerun automatically, with a per-job retry budget. The class of "my CI job hung for ten minutes and then failed mysteriously — please rerun" support tickets is closed by construction, eliminating a recurring source of toil for both tenant teams and on-call.

- **Per-tenant security and audit isolation.** Every tenant's GitHub traffic exits through a dedicated egress IP pool, enabling per-team IP allowlisting on the GitHub side and per-tenant audit attribution. A rate limit, abuse flag, or IP ban triggered by one team is contained to that team — other tenants are unaffected. For organizations with GitHub Enterprise IP allowlist requirements or regulated workloads, this is what makes a shared cluster viable instead of one cluster per tenant.

- **Operational leverage for the platform team.** One operator at the cluster level replaces a per-tenant runner deployment for every team. The platform team's job shrinks to running one controller; first-line debugging shifts to the tenant teams who know their own workloads. Fewer escalations, fewer on-call pages, no per-tenant configuration drift to manage.

**On cost.** The largest idle-GPU savings come from eliminating the per-team `minRunners > 0` pattern frequently used in production runner deployments to mask runner-pod cold-start latency — a pattern that silently holds GPUs around the clock. Teams migrating from older runner deployments see the largest absolute reductions; teams already on modern auto-scaling runners gain primarily from the operational, scheduling, and security benefits above, plus reduced always-on listener overhead. A second, independent lever comes from priority tiers: because GPU runners are guaranteed to schedule on demand, the shared quota can be packed with cheap work instead of holding idle headroom in reserve for GPU jobs — raising utilization and throughput of capacity you already pay for, and lowering cost per job. See [Appendix F — Cost Model](appendix-f-cost-model.md) for a configuration-by-configuration breakdown.

**The ask.** Engineering investment to build and operate the Gateway Manager Controller. Payback comes from platform-team leverage, tenant self-service, GPU utilization under contention, eliminated incident-recovery toil, and the security posture needed to host regulated and high-value tenants on a shared cluster.

### For Tenant Teams

**Own your CI infrastructure end-to-end, without waiting on the platform team.** Declare every runner type your team needs in one custom resource in your own namespace and iterate on it as your test matrix changes. Get the GPU slots you ask for, see exactly how much capacity you actually use, and stop losing afternoons to mysteriously-failed CI jobs.

- **One CR, one source of truth.** All your runner sets, GPU allocations, scale limits, and priority tiers live in a single `ActionsGateway` resource in your own namespace. Changes are a `kubectl edit` away, owned by the team that needs them.

- **GPU slots that are actually guaranteed under contention.** Priority tiers let you declare a minimum number of GPU runner pods that schedule even when the cluster's GPU quota is otherwise saturated. The cheap CPU runners that today crowd you out of the queue cannot push your GPU jobs out of the schedule.

- **Failed jobs from infrastructure issues just disappear.** When a worker pod is preempted, OOM-killed, or lost to a node failure, the system fast-cancels the job at GitHub and reruns it automatically. No more "did this fail for a real reason or was it just infrastructure?" investigations.

- **No cold-start tax to pay around.** You don't need to pin a minimum runner count to mask first-job latency — the listener is always warm, and worker pods are created on demand. Configure for the load you actually have, not the cold start you're trying to hide.

- **First-class utilization metrics in your own namespace.** Per-tenant, per-runner-group GPU-hours, job counts, queue times, and pod-creation latency are exposed as Prometheus metrics. Use them to right-size your quota, justify increases when you need more, and identify runner shapes that are over- or under-used.

- **Contained blast radius for cross-tenant incidents.** Another team's CI incident — a runaway job hitting GitHub rate limits, an abuse flag, an IP ban triggered by their traffic — does not propagate to your pipeline. Your egress IPs are your own.

### For Platform Engineering

**Stop being the bottleneck for every tenant runner change and the first responder for every tenant runner failure.** Deploy one operator at the cluster level; tenants own their own configuration, debugging, and capacity planning beneath it. Onboarding a new team is approving a namespace, not standing up a deployment.

- **One operator, many tenants.** The Gateway Manager Controller watches `ActionsGateway` resources cluster-wide and provisions everything a tenant needs — RBAC, NetworkPolicies, egress proxy, the AGC, and every runner group declared in the CR — operating within the platform-owned namespace `ResourceQuota`. No per-tenant Helm releases or bespoke YAML to maintain.

- **First-line debugging shifts to the tenant.** Tenants who own their own configuration can answer their own "why isn't my runner scaling?" and "why did my job fail?" questions. The escalation path back to platform shrinks to the controller itself.

- **Tenant security policies enforced by construction, not convention.** Per-tenant egress IP pools, namespace-scoped RBAC, and NetworkPolicies are part of what the controller provisions, all operating within the platform-owned namespace `ResourceQuota`. Adding a tenant does not require manual security review of per-tenant network rules — the controller emits the policy, and security review focuses on the controller once.

- **Worker eviction is no longer a paging event.** Preempted, OOM-killed, and node-lost jobs are recovered automatically. The recurring "tenant X's CI is failing intermittently, please look at the runner pod" ticket pattern largely goes away.

- **Per-tenant cost and capacity visibility out of the box.** Prometheus metrics scoped per tenant and per runner group make it straightforward to spot under-used GPU quota, hot tenants approaching their limits, and which runner shapes are driving the most cost — without per-tenant deployment-level instrumentation to assemble.

- **One thing to upgrade.** One controller binary at the cluster level instead of N per-tenant runner deployments to roll forward and verify.

---

## Overview for Architects & Engineers

### The Problem: Scheduling Fairness in a Shared Namespace

In a multi-tenant Kubernetes cluster, teams running multiple types of GitHub Actions self-hosted runners face a scheduling fairness problem that existing solutions — including Actions Runner Controller (ARC) — cannot address. When a namespace's `ResourceQuota` is shared across runner groups, smaller and cheaper runner pods can exhaust available quota before larger pods have a chance to schedule. The result is that GPU runners — which carry the most expensive hardware requirements and the longest queue times — are systematically starved by a flood of small CPU runner pods claiming quota first.

ARC provides no mechanism to express minimum scheduling guarantees across runner sets within a shared pool. Each `RunnerScaleSet` has an independent `maxRunners` cap, making it impossible to declare "GPU runners must always be able to claim at least N slots, regardless of how many CPU runners are active." The only workaround — lowering CPU runner caps to protect headroom — still cannot guarantee that headroom is actually available for GPU runners when they need it, and introduces a separate per-set configuration burden that grows with the number of runner types.

This design addresses the problem through a `priorityTiers` field on each `RunnerGroup`: a ranked list of Kubernetes `PriorityClass` assignments, cumulative pod-count thresholds, and a `preemptionPolicy` per tier. The first N pods of a GPU runner group are assigned a preempting `PriorityClass` (`preemptionPolicy: PreemptLowerPriority`) and will displace lower-priority CPU runner pods when the namespace is contended — guaranteeing they schedule. All subsequent tiers use `preemptionPolicy: Never`, so burst and best-effort pods gain scheduling priority over lower-priority pending work without evicting any running jobs. A final threshold caps total concurrency per group. This confines eviction risk to the minimum guaranteed floor pods only, and lets a platform team express "GPU runners always get at least 5 slots, can burst to 20 without evicting anything, and are capped at 30," all enforced by the Kubernetes scheduler against a single shared namespace `ResourceQuota`.

Framed defensively, priority tiers stop expensive runners from being starved. Framed offensively, they are a utilization-and-cost lever. Without a scheduling guarantee, the only way to keep GPU runners schedulable under a shared quota is to hold headroom in reserve — cap cheap runners below the quota, or leave slack unused — so a GPU pod always has room. That reserved headroom is idle, paid-for capacity. The floor tier removes the need for it: because guaranteed pods preempt their way in on demand, cheap and burst runners can fill the quota to capacity and yield the instant a GPU job arrives. This is safe oversubscription of the shared quota — more runner demand is admitted than the guaranteed floors reserve, with the Kubernetes scheduler arbitrating by priority instead of an operator pre-reserving idle slack. The same provisioned cluster runs closer to full utilization and clears more jobs per hour, which is what lowers cost per unit of work.

When eviction does occur — either from the floor tier preempting lower-priority pods, or from external pressure such as node memory exhaustion — the AGC automatically re-queues the affected job without user intervention. The Job Lock Renewer detects the `Evicted` pod status, immediately stops renewal to prompt a fast GitHub cancellation, and calls GitHub's rerun API to reschedule the job. A configurable retry budget prevents infinite loops on persistently failing workloads. Jobs that exhaust their retry budget surface via a dedicated metric rather than silently disappearing.

### GPU Node Utilization

For teams running GPU-accelerated workflows, runner pod lifecycle is a direct driver of hardware utilization. Where GAG provides headroom over ARC depends on how ARC is configured:

* **ARC scale-set mode with `minRunners: 0`** (the current best practice for scale sets) scales runner pods to zero between jobs. GPU allocation between jobs is comparable to GAG: GPU nodes are consumed only while a job is running, and the listener pod sits on a CPU node. GAG's advantage in this configuration is listener footprint (one shared pod versus N per-scale-set pods) and the fact that tenants do not need to pin `minRunners > 0` to avoid cold-start latency — not GPU allocation per se.
* **ARC scale-set mode with `minRunners > 0`**, commonly configured per scale set to mask runner-pod cold-start latency, holds N runner pods continuously per scale set even with no work queued. A tenant with 10 GPU scale sets at `minRunners: 1` holds 10 GPU pods around the clock. GAG eliminates this entirely — the goroutine listener never goes cold, so no minimum-pod tuning is needed.
* **Legacy ARC (`RunnerDeployment` + HRA)** had a per-pod `Runner.Listener` and no clean scale-to-zero parity. Teams migrating from this configuration see the largest absolute idle-GPU reductions.

In all configurations, the AGC itself runs on a CPU-only node pool, so no GPU capacity is consumed by the controller. Worker pods are provisioned on-demand after a job is acquired and release their compute — including the GPU — the moment the job completes (the terminal pod object is deleted after a short configurable TTL and holds no resources in the meantime). Across a shared GPU node pool, GAG keeps GPU nodes available to other workloads up until the moment a job arrives, with no per-scale-set baseline tuning to maintain.

### For Teams Migrating from Host-Machine or VM Runners

The arguments above apply equally to teams already running Kubernetes-native runners. For teams migrating from runners on host machines or virtual machines — where runners are registered as persistent processes rather than Kubernetes pods — the Kubernetes model itself introduces an additional overhead worth quantifying.

In a traditional self-hosted runner setup (host or VM), each registered runner slot runs a full `Runner.Listener` process: a minimum of ~256 MiB of reserved memory per slot. A team running 1,000 concurrent slots must hold ~256 GiB of memory in reserve across the cluster just to keep runners available, regardless of whether any jobs are pending. ARC's `RunnerScaleSet` mode improves on this — it uses one `Runner.Listener` process per scale set rather than one per slot — but that listener is still a full .NET runtime process (~256 MiB per scale set). A tenant operating 10 RunnerScaleSets holds ~2.5 GiB in listener processes alone at rest. In contrast, the goroutine-based session model this system uses averages ~60 KiB resident per virtual runner session (see [Appendix A](appendix-a-capacity-slos.md)) — a reduction of over 4,000× per active session. At the same 1,000-session burst ceiling, the AGC's goroutine working set is roughly 60 MiB; the steady-state cost at rest is ~60 KiB per RunnerGroup regardless of how many slots are configured.

The IP address problem compounds this for pod-per-slot deployments. Every runner pod consumes a cluster IP. In clusters already dense with application workloads, 1,000 idle runner pods exhaust a significant fraction of the available address space — a hard limit that cannot be worked around without re-addressing the cluster. Each pod's long-poll connection also generates sustained polling noise through network firewalls, adding to the operational burden of teams managing cluster egress.

### Design Goals

The system is designed to satisfy four requirements that existing solutions do not address together:

1. **Shared quota with per-group priority guarantees.** All `RunnerGroup`s within a tenant draw from a single namespace-level `ResourceQuota`. Each group optionally defines a `priorityTiers` list to express a preemption floor, an opportunistic burst range, and a hard concurrency ceiling — enforced by the Kubernetes scheduler without idle resource reservation. Only the floor tier carries `preemptionPolicy: PreemptLowerPriority`; all higher tiers use `Never`, confining eviction risk to the minimum guaranteed slots.
2. **Automatic eviction retry.** When a worker pod is evicted, the AGC detects it, fast-cancels the job lock, and re-queues the job via GitHub's rerun API — no user action required. A configurable retry budget caps automatic retries per job; exhausted budgets are surfaced as a metric rather than silent failures.
3. **Eliminate idle resource overhead.** Virtual runner sessions are goroutines, not pods. Compute is provisioned only when a job is acquired and released immediately on completion — including GPU allocations.
4. **Per-tenant egress IP isolation.** Each tenant's GitHub traffic — both AGC control-plane calls and worker data-plane traffic — exits through a dedicated proxy pool. This gives operators IP-based allowlisting at the GitHub side, per-tenant audit attribution, and a per-tenant kill-switch: GitHub-side rate limits, abuse flags, or IP bans against one tenant's egress IPs do not affect other tenants. See [§2.3](02-architecture.md#23-tier-3--egress-proxy-pool) for why worker egress also flows through the proxy.
5. **Self-service multi-tenant onboarding.** A team creates one `ActionsGateway` CR in their existing namespace and receives a fully isolated gateway instance. No cluster-admin involvement is required after initial GMC installation.

### The Solution: A Four-Tier Virtualized Gateway

This document outlines the design for a **four-tier system** that addresses these problems at their root:

* **Tier 1 — Gateway Manager Controller (GMC):** A cluster-scoped operator that watches namespace-scoped `ActionsGateway` Custom Resources across all namespaces and provisions isolated, fully independent gateway instances — one per CR. It owns the lifecycle of all AGC-related resources within the tenant's existing namespace: RBAC, network policies, resource quotas, and the AGC deployment itself.

* **Tier 2 — Actions Gateway Controller (AGC):** A Go-based operator deployed once per tenant by the GMC. It acts as a highly concurrent, virtualized gateway, scaling lightweight **Go routines** to multiplex thousands of virtual runner sessions. Compute resources (Pods) are provisioned purely on-demand, executing jobs ephemerally and tearing down immediately upon completion.

* **Tier 3 — Egress Proxy Pool:** A horizontally autoscaled pool of stateless HTTPS CONNECT proxy pods, deployed per tenant by the GMC. All GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs isolated from other tenants. This enables per-team IP allowlisting on the GitHub side, produces clean per-tenant audit trails, and contains GitHub-side incident impact: a rate limit, abuse flag, or IP ban triggered by one tenant's egress IPs does not propagate to other tenants on different IPs. (Note: this is a containment property at the *network attribution* layer; it is not a substitute for per-installation authorization, which already scopes what a compromised worker can do at GitHub.)

* **Tier 4 — Ephemeral Worker Pod:** A short-lived, single-use pod that executes exactly one workflow job. Provisioned on-demand by the AGC after a job is acquired; its compute is released the moment the job completes, and the terminal pod object is deleted after a short configurable TTL. Because worker pods consume resources only while a job is running, there are zero idle compute resources between jobs — the cluster pays only for work actually being done.

### Operational Model

This architecture makes it practical to operate GitHub Actions self-hosted runners in a multi-tenant Kubernetes cluster: a platform team deploys the GMC once at the cluster level, while individual teams create an `ActionsGateway` resource in their own existing namespace and receive fully isolated runner capacity — no cluster-admin involvement required after initial GMC installation.

The four-tier design is intentionally more complex than a simple self-hosted runner deployment. That complexity is load-bearing: it is what makes goroutine-level multiplexing, per-tenant egress isolation, and zero-idle compute possible simultaneously in a shared cluster. For a detailed evaluation of simpler alternatives and the reasons they fall short at multi-tenant scale, see [Appendix D](appendix-d-alternatives-considered.md).

---

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)
