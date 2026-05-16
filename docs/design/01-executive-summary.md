# 1. Executive Summary & Problem Statement

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)

---

## The Problem: Idle Runners at Scale

The traditional deployment pattern for GitHub Actions self-hosted runners on Kubernetes requires maintaining a 1:1 mapping between active runner processes and long-polling HTTP connections. At any meaningful scale, this model becomes operationally untenable.

Each idle runner pod carries the full weight of the `Runner.Listener` process — a minimum of ~256 MiB of reserved memory per slot. A team running 1,000 concurrent runner slots must therefore hold ~256 GiB of memory in reserve across the cluster just to keep runners available, regardless of whether any jobs are actually pending. In contrast, the goroutine-based session model this system uses averages ~60 KiB resident per virtual runner slot (see [Appendix A](appendix-a-capacity-slos.md)), a reduction of over 4,000× per session. At the same 1,000-session ceiling, the AGC's working set is roughly 60 MiB.

The IP address problem compounds this. Every runner pod consumes a cluster IP. In clusters already dense with application workloads, 1,000 idle runner pods exhaust a significant fraction of the available address space — a hard limit that cannot be worked around without re-addressing the cluster. Alongside the memory and IP pressure, each pod's long-poll connection generates sustained polling noise through network firewalls, adding to the operational burden of teams managing cluster egress.

For teams running GPU-accelerated workflows, the cost of idle runners is especially acute. GPU runner pods hold GPU node allocations while waiting for work, even between jobs. Those GPU resources are unavailable to other workloads during the wait — wasting some of the most expensive capacity in the cluster. The on-demand model described here eliminates this entirely: no GPU resources are consumed between jobs. The AGC itself runs on a CPU-only node pool and manages GPU runner scheduling dynamically, so GPU nodes are returned to the cluster scheduler the moment a job completes and remain available for other workloads until the next job arrives. Across a shared GPU node pool, this translates directly into higher effective GPU utilization without requiring additional hardware.

## Design Goals

The system is designed to satisfy four requirements that existing solutions do not address together:

1. **Eliminate idle resource overhead.** Virtual runner sessions are goroutines, not pods. Compute is provisioned only when a job is acquired and released immediately on completion — including GPU allocations.
2. **Shared quota across runner groups.** A tenant may define multiple `RunnerGroup`s (e.g. one for CPU jobs, one for GPU jobs, one for sandboxed external PRs) and have all of them draw from a single namespace-level resource pool. There is no per-group cap to coordinate; the Kubernetes `ResourceQuota` on the namespace is the single shared ceiling enforced by the scheduler across all groups simultaneously.
3. **Per-tenant egress IP isolation.** Each tenant's GitHub traffic exits through a dedicated proxy pool, enabling IP-based allowlisting, clean audit trails, and contained blast radius.
4. **Self-service multi-tenant onboarding.** A team creates one `ActionsGateway` CR in their existing namespace and receives a fully isolated gateway instance. No cluster-admin involvement is required after initial GMC installation.

## The Solution: A Four-Tier Virtualized Gateway

This document outlines the design for a **four-tier system** that addresses these problems at their root:

* **Tier 1 — Gateway Manager Controller (GMC):** A cluster-scoped operator that watches namespace-scoped `ActionsGateway` Custom Resources across all namespaces and provisions isolated, fully independent gateway instances — one per CR. It owns the lifecycle of all AGC-related resources within the tenant's existing namespace: RBAC, network policies, resource quotas, and the AGC deployment itself.

* **Tier 2 — Actions Gateway Controller (AGC):** A Go-based operator deployed once per tenant by the GMC. It acts as a highly concurrent, virtualized gateway, scaling lightweight **Go routines** to multiplex thousands of virtual runner sessions. Compute resources (Pods) are provisioned purely on-demand, executing jobs ephemerally and tearing down immediately upon completion.

* **Tier 3 — Egress Proxy Pool:** A horizontally autoscaled pool of stateless HTTPS CONNECT proxy pods, deployed per tenant by the GMC. All GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs isolated from other tenants. This enables per-team IP allowlisting on the GitHub side, produces clean per-tenant audit trails, and limits the blast radius if any one tenant's traffic is flagged.

* **Tier 4 — Ephemeral Worker Pod:** A short-lived, single-use pod that executes exactly one workflow job. Provisioned on-demand by the AGC after a job is acquired and garbage-collected immediately on completion. Because worker pods exist only while a job is running, there are zero idle compute resources between jobs — the cluster pays only for work actually being done.

## Operational Model

This architecture makes it practical to operate GitHub Actions self-hosted runners in a multi-tenant Kubernetes cluster: a platform team deploys the GMC once at the cluster level, while individual teams create an `ActionsGateway` resource in their own existing namespace and receive fully isolated runner capacity — no cluster-admin involvement required after initial GMC installation.

The four-tier design is intentionally more complex than a simple self-hosted runner deployment. That complexity is load-bearing: it is what makes goroutine-level multiplexing, per-tenant egress isolation, and zero-idle compute possible simultaneously in a shared cluster. For a detailed evaluation of simpler alternatives and the reasons they fall short at multi-tenant scale, see [Appendix D](appendix-d-alternatives-considered.md).

---

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)
