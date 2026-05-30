# High-Scale Virtualized GitHub Actions Gateway — Design Documentation

This folder contains the full system design for the GitHub Actions Gateway, organized into focused documents with cross-references. All documents are intended to render correctly on GitHub.

---

## Table of Contents

1. [Executive Summary & Problem Statement](01-executive-summary.md)
   - [For Executive Leadership: GPU Utilization & Cost Justification](01-executive-summary.md#for-executive-leadership)
   - [For Tenant Teams: Self-Service & Cost Ownership](01-executive-summary.md#for-tenant-teams)
   - [For Platform Engineering: Operational Leverage & Shift Left](01-executive-summary.md#for-platform-engineering)
   - [Overview for Architects & Engineers](01-executive-summary.md#overview-for-architects--engineers)
2. [Core Architectural Components](02-architecture.md)
   - [2.1 Tier 1 — Gateway Manager Controller (GMC)](02-architecture.md#21-tier-1--gateway-manager-controller-gmc)
   - [2.2 Tier 2 — Actions Gateway Controller (AGC)](02-architecture.md#22-tier-2--actions-gateway-controller-agc)
   - [2.3 Tier 3 — Egress Proxy Pool](02-architecture.md#23-tier-3--egress-proxy-pool)
   - [2.4 Tier 4 — Ephemeral Worker Pod](02-architecture.md#24-tier-4--ephemeral-worker-pod)
   - [2.5 Observability](02-architecture.md#25-observability)
   - [2.6 Upgrade Strategy](02-architecture.md#26-upgrade-strategy)
3. [API & Data Contract Specifications](03-api-contracts.md)
   - [3.1 Kubernetes CRD Schemas](03-api-contracts.md#31-kubernetes-crd-schemas)
   - [3.2 GitHub App Credentials Secret Schema](03-api-contracts.md#32-github-app-credentials-secret-schema)
   - [3.3 Re-implemented Broker API Endpoints](03-api-contracts.md#33-re-implemented-broker-api-endpoints)
   - [3.4 Broker Payload Blueprints (Go Structs)](03-api-contracts.md#34-broker-payload-blueprints-go-structs)
   - [3.5 GitHub API Rate Limit Budget](03-api-contracts.md#35-github-api-rate-limit-budget)
4. [Operational Lifecycle Execution Flows](04-operational-flows.md)
   - [4.1 Tenant Provisioning Flow (GMC)](04-operational-flows.md#41-tenant-provisioning-flow-gmc)
   - [4.2 Job Execution Flow (AGC)](04-operational-flows.md#42-job-execution-flow-agc)
5. [Security & Threat Risk Assessment](05-security.md)
   - [5.1 GMC-Level Threats (Cluster-Scoped)](05-security.md#51-gmc-level-threats-cluster-scoped)
   - [5.2 AGC & Proxy-Level Threats (Namespace-Scoped)](05-security.md#52-agc--proxy-level-threats-namespace-scoped)
   - [5.3 Security Profiles and the Privileged Opt-In](05-security.md#53-security-profiles-and-the-privileged-opt-in)
6. [Implementation Phasing & Delivery Milestones](06-implementation-phases.md)
   - [Milestone 1: Wire Protocol Probe (Days 1–4)](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14)
   - [Milestone 2: AGC Controller & Reconciler (Days 5–10)](06-implementation-phases.md#milestone-2-agc-controller--reconciler-days-510)
   - [Milestone 3: Worker Pod & Pipe Handoff (Days 11–16)](06-implementation-phases.md#milestone-3-worker-pod--pipe-handoff-days-1116)
   - [Milestone 4: Gateway Manager Controller + Proxy (Days 17–22)](06-implementation-phases.md#milestone-4-gateway-manager-controller--proxy-days-1722)
   - [Milestone 5: Hardening & Load Testing (Days 23–26)](06-implementation-phases.md#milestone-5-hardening--load-testing-days-2326)
7. [Test Plan](07-test-plan.md)
   - [7.1 Unit Tests](07-test-plan.md#71-unit-tests)
   - [7.2 Integration Tests](07-test-plan.md#72-integration-tests)
   - [7.3 End-to-End Tests](07-test-plan.md#73-end-to-end-tests)
8. [Glossary](08-glossary.md)
- [Appendix A — Capacity Targets & SLOs](appendix-a-capacity-slos.md)
- [Appendix B — Worker Isolation Runtime (Optional)](appendix-b-worker-isolation.md)
- [Appendix C — AI-Assisted Implementation Notes (Optional)](appendix-c-ai-implementation.md)
- [Appendix D — Alternatives Considered](appendix-d-alternatives-considered.md)
- [Appendix E — Capacity Planning & RunnerGroup Design](appendix-e-capacity-planning.md)
- [Appendix F — Cost Model](appendix-f-cost-model.md)
- [Appendix G — Optional Future Enhancements](appendix-g-future-enhancements.md)
- [Network Architecture](network-architecture.md)

**Operations**

- [Getting Started](../getting-started.md) — initial setup, credential rotation
- [Observability](../operations/observability.md) — metrics reference, alert rules
- [Troubleshooting](../operations/troubleshooting.md) — symptom → diagnosis → resolution
- [Runbook](../operations/runbook.md) — day-2 operations, incident response
- [Upgrade & Rollback](../operations/upgrade.md) — per-component upgrade procedures
- [Tenant Onboarding](../operations/tenant-onboarding.md) — checklist for onboarding a new tenant team

---

## Reading Paths by Role

**Architect or engineer** reviewing the overall design: start with [01-executive-summary.md](01-executive-summary.md), then [02-architecture.md](02-architecture.md), then [03-api-contracts.md](03-api-contracts.md). Read [04-operational-flows.md](04-operational-flows.md) and [05-security.md](05-security.md) for depth.

**Platform engineer** deploying or operating the system: read [Getting Started](../getting-started.md) first, then [02-architecture.md §2.1](02-architecture.md#21-tier-1--gateway-manager-controller-gmc) (GMC), [Appendix A](appendix-a-capacity-slos.md) (SLOs), [Observability](../operations/observability.md), [Runbook](../operations/runbook.md), and [Upgrade & Rollback](../operations/upgrade.md).

**Security engineer** reviewing trust boundaries and threats: read [05-security.md](05-security.md), [02-architecture.md §2.4](02-architecture.md#24-tier-4--ephemeral-worker-pod) (worker isolation), [03-api-contracts.md §3.2](03-api-contracts.md#32-github-app-credentials-secret-schema) (credentials), and [Appendix B](appendix-b-worker-isolation.md) (runtime hardening).

**Tenant team** authoring RunnerGroup configs: read [Getting Started](../getting-started.md), [03-api-contracts.md §3.1](03-api-contracts.md#31-kubernetes-crd-schemas) (CRD schemas), and [Appendix E](appendix-e-capacity-planning.md) (sizing guidance).

---

## System Overview

The gateway is a **four-tier system** for running GitHub Actions self-hosted runners at scale on Kubernetes:

| Tier | Component | Scope | Role |
| --- | --- | --- | --- |
| 1 | [Gateway Manager Controller (GMC)](02-architecture.md#21-tier-1--gateway-manager-controller-gmc) | Cluster | Watches `ActionsGateway` CRs, provisions per-tenant resources |
| 2 | [Actions Gateway Controller (AGC)](02-architecture.md#22-tier-2--actions-gateway-controller-agc) | Namespace | Multiplexes GitHub broker sessions, acquires jobs, spawns worker pods |
| 3 | [Egress Proxy Pool](02-architecture.md#23-tier-3--egress-proxy-pool) | Namespace | Stateless HTTPS CONNECT proxy pool; isolated egress IPs per tenant |
| 4 | [Ephemeral Worker Pod](02-architecture.md#24-tier-4--ephemeral-worker-pod) | Namespace | Single-use pod that executes exactly one workflow job |

For a quick orientation, start with [01-executive-summary.md](01-executive-summary.md), then follow links from there.
