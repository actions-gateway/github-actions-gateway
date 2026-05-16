# 1. Executive Summary & Problem Statement

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)

---

The traditional deployment pattern for GitHub Actions self-hosted runners on Kubernetes requires maintaining a 1:1 mapping between active runner processes and long-polling HTTP connections. This introduces significant operational overhead: thousands of idle runner pods must sit waiting for work, consuming memory overhead, exhausting Kubernetes IP spaces, and creating massive polling noise on network firewalls.

This document outlines the design for a **four-tier system**:

* **Tier 1 — Gateway Manager Controller (GMC):** A cluster-scoped operator that watches namespace-scoped `ActionsGateway` Custom Resources across all namespaces and provisions isolated, fully independent gateway instances — one per CR. It owns the lifecycle of all AGC-related resources within the tenant's existing namespace: RBAC, network policies, resource quotas, and the AGC deployment itself.

* **Tier 2 — Actions Gateway Controller (AGC):** A Go-based operator deployed once per tenant by the GMC. It acts as a highly concurrent, virtualized gateway, scaling lightweight **Go routines** to multiplex thousands of virtual runner sessions. Compute resources (Pods) are provisioned purely on-demand, executing jobs ephemerally and tearing down immediately upon completion.

* **Tier 3 — Egress Proxy Pool:** A horizontally autoscaled pool of stateless HTTPS CONNECT proxy pods, deployed per tenant by the GMC. All GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs isolated from other tenants.

* **Tier 4 — Ephemeral Worker Pod:** A short-lived, single-use pod that executes exactly one workflow job. Provisioned on-demand by the AGC after a job is acquired and garbage-collected immediately on completion.

This architecture makes it practical to operate GitHub Actions self-hosted runners in a multi-tenant Kubernetes cluster: a platform team deploys the GMC once at the cluster level, while individual teams create an `ActionsGateway` resource in their own existing namespace and receive fully isolated runner capacity — no cluster-admin involvement required after initial GMC installation.

---

← [Back to index](README.md) | Next: [Core Architectural Components →](02-architecture.md)
