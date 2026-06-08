# GitHub Actions Gateway

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

> An Actions Runner Controller (ARC) alternative for self-hosted GitHub Actions runners on multi-tenant Kubernetes — oversubscribe a shared quota for higher utilization and lower cost, recover evicted jobs automatically, and isolate every tenant's GitHub egress for per-team IP allowlisting and contained blast radius.

Each tenant operates many runner groups (CPU, GPU, large-memory, …) inside their own namespace under a single `ResourceQuota`, all driven by one Kubernetes operator.

GitHub Actions Gateway (GAG) brings four properties that Actions Runner Controller (ARC) scale-set mode does not provide together:

- **Priority-tiered scheduling across a shared `ResourceQuota`.** Reserve a floor of slots for expensive GPU runners that a flood of cheap CPU pods can't starve, then let higher tiers burst opportunistically into spare capacity — so you can oversubscribe the quota for higher utilization and lower cost instead of holding idle headroom in reserve.
- **Automatic eviction retry.** When a worker pod is preempted, OOM-killed, or lost to a node failure, GAG fast-cancels the GitHub-side job lock and calls the rerun API, with a per-job retry budget — no manual rerun needed.
- **Per-tenant dedicated egress IP pool.** Every tenant's GitHub traffic exits through a tenant-specific HTTPS CONNECT proxy pool, enabling per-team IP allowlisting on the GitHub side and containing rate-limit or abuse blast radius to one tenant.
- **Self-service multi-tenant onboarding via one CR.** A team creates a single `ActionsGateway` CR in their own namespace and receives a fully isolated gateway instance: RBAC, NetworkPolicies, `ResourceQuota`, egress proxy, controller, and every runner group they declared.

GAG also **scales workers to zero between jobs** — the same property ARC scale-set mode provides with `minRunners: 0` — but with substantially less always-on overhead. ARC's listener is a per-scale-set pod running a full .NET runtime (~256 MiB resident, plus a cluster IP, held open 24/7 to long-poll GitHub). GAG hosts every `RunnerGroup`'s listener as a goroutine in one shared controller pod, at ~60 KiB per group. A tenant with 10 runner groups holds ~600 KiB of listener state in one pod instead of ~2.5 GiB across 10 pods.

## The Problem

Running many runner groups for one tenant in a shared Kubernetes namespace creates four compounding problems that ARC scale-set mode does not address together:

**Scheduling starvation under a shared `ResourceQuota`.** Each ARC `AutoscalingRunnerSet` has its own `maxRunners` cap, but there is no primitive for "GPU runners must always be able to claim at least N slots, regardless of how many CPU runners are active." When cheap CPU pods exhaust namespace quota first, the most expensive hardware reliably loses the race.

**Listener overhead at scale.** ARC's scale-set listener is one pod per scale set running a full .NET runtime — roughly 256 MiB resident, plus a cluster IP, held alive 24/7 to long-poll GitHub. A tenant with 10 scale sets pays ~2.5 GiB of memory and 10 pod slots at rest, before any job runs. Teams that also pin `minRunners > 0` to mask runner-pod cold-start latency multiply this further with idle runner pods on expensive hardware.

**No automatic recovery from worker eviction.** When a runner pod is preempted, OOM-killed, or lost to a node failure, ARC has no built-in flow to fast-cancel the GitHub job lock and rerun. The job sits until GitHub's lock expires (typically ~10 minutes), then surfaces as a failed workflow that needs manual rerun.

**Platform team as bottleneck.** Onboarding a tenant means provisioning namespace, quotas, controller scope, scale sets, NetworkPolicies, and egress — a platform-team checklist per team. Subsequent changes (new runner type, quota adjustment, scaling tweak) land as tickets.

## The Solution

**Scheduling priority tiers per `RunnerGroup`.** The `priorityTiers` field maps Kubernetes `PriorityClass` objects to cumulative pod-count thresholds. The first N pods of a GPU runner group get a preempting `PriorityClass` and will displace lower-priority CPU pods when quota is contended — guaranteeing they schedule. Higher tiers use `preemptionPolicy: Never`, so burst capacity gains scheduling preference without evicting running jobs. A final threshold caps total concurrency per group.

**Automatic eviction retry with fast lock cancel.** When the AGC sees a worker pod in `Evicted` status, it immediately stops lock renewal so GitHub cancels the job in seconds instead of waiting the full lock expiry, then calls GitHub's rerun API to reschedule. A configurable per-job retry budget prevents loops on persistently failing workloads.

**Per-tenant dedicated egress IP pool.** A Horizontal Pod Autoscaler (HPA)-managed pool of stateless HTTPS CONNECT proxy pods per tenant. All GitHub traffic from the AGC and worker pods routes through this pool, so each tenant gets egress IPs never shared with other tenants. Enables per-team allowlisting on the GitHub side, clean per-tenant audit attribution, and contained blast radius for rate limits or abuse flags.

**Self-service tenant management via one CR.** The Gateway Manager Controller (GMC) watches `ActionsGateway` CRs in tenant namespaces and provisions everything the tenant needs — RBAC, NetworkPolicies, `ResourceQuota`, egress proxy, AGC, and every runner group declared in the CR. No cluster-admin involvement after initial GMC install. Because tenants control their own configuration, they can diagnose their own runner behavior without escalating to the platform team.

**Scale workers to zero with low listener overhead.** Worker pods are created only when a job is acquired and deleted immediately on completion — the same scale-to-zero behavior as ARC scale-set mode with `minRunners: 0`, so GPU nodes return to the cluster scheduler the moment a job finishes. The difference is the listener: GAG runs every `RunnerGroup`'s listener as a goroutine (~60 KiB resident) inside one shared AGC pod, instead of one ~256 MiB .NET listener pod per scale set. Tenants do not need to pin `minRunners > 0` to mask cold-start latency, so the silent re-introduction of idle GPU pods that pattern causes does not happen.

**Per-tenant utilization metrics.** Both the GMC and AGC expose Prometheus metrics scoped per tenant and runner group. Teams have the data to understand their own GPU utilization and make the case for quota adjustments without relying on cluster-wide visibility.

## Architecture

A four-tier system:

```
  Tenant namespace                         System namespace
  ════════════════                         ════════════════

  ┌──────────────────────┐               ┌──────────────────────────────┐
  │  ActionsGateway CR   │──── watch ───▶│  Gateway Manager Controller  │
  │  (namespace-scoped)  │               │            (GMC)             │
  └──────────────────────┘               └───────────────┬──────────────┘
                ┌────────────── provisions ──────────────┘
                ▼
  ┌──────────────────────────────────────────────────────────────────────┐
  │  Tenant namespace                                                    │
  │    • Egress Proxy Pool           HPA-managed, per-tenant egress IPs  │
  │    • Actions Gateway Controller  AGC, goroutine multiplexer          │
  │    • Ephemeral Worker Pods       one per job, GC'd on completion     │
  └──────────────────────────────────────────────────────────────────────┘
```

**Tier 1 — Gateway Manager Controller (GMC).** A cluster-scoped operator deployed once by the platform team. It watches namespace-scoped `ActionsGateway` CRs across all namespaces and provisions a fully isolated gateway instance for each tenant — role-based access control (RBAC), network policies, resource quotas, egress proxy, and AGC — entirely within the tenant's existing namespace.

**Tier 2 — AGC.** A Go-based operator deployed once per tenant. Instead of one pod per runner slot, it multiplexes thousands of virtual runner sessions as goroutines. Compute is provisioned only when a job is acquired and garbage-collected immediately on completion. At steady state each goroutine costs ~60 KiB resident — a reduction of over 4,000× compared to a full .NET `Runner.Listener` process.

**Tier 3 — Egress Proxy Pool.** A Horizontal Pod Autoscaler (HPA)-managed pool of stateless HTTPS CONNECT proxy pods per tenant. All GitHub traffic from the AGC and worker pods routes through this pool, giving each tenant a dedicated set of egress IPs never shared with other tenants. Supports per-team IP allowlisting, clean audit trails, and contained blast radius.

**Tier 4 — Ephemeral Worker Pod.** A short-lived pod that executes exactly one workflow job and is immediately deleted on completion. Because worker pods exist only while a job is running, zero compute is idle between jobs — GPU nodes return to the cluster scheduler the moment a job finishes.

For the full design, see [docs/design/](docs/design/README.md).

| Section | |
| --- | --- |
| Executive Summary & Problem Statement | [01-executive-summary.md](docs/design/01-executive-summary.md) |
| Core Architectural Components | [02-architecture.md](docs/design/02-architecture.md) |
| API & Data Contract Specifications | [03-api-contracts.md](docs/design/03-api-contracts.md) |
| Operational Lifecycle Execution Flows | [04-operational-flows.md](docs/design/04-operational-flows.md) |
| Security & Threat Risk Assessment | [05-security.md](docs/design/05-security.md) |
| Capacity Targets & SLOs | [appendix-a-capacity-slos.md](docs/design/appendix-a-capacity-slos.md) |
| Alternatives Considered | [appendix-d-alternatives-considered.md](docs/design/appendix-d-alternatives-considered.md) |
| Optional Future Enhancements | [appendix-g-future-enhancements.md](docs/design/appendix-g-future-enhancements.md) |

## Quick Start

See [docs/getting-started.md](docs/getting-started.md) for the full walkthrough: GitHub App Secret, `ActionsGateway` CR, and GMC deployment.

## Observability

Both the GMC and AGC expose Prometheus metrics at `/metrics`. See [docs/operations/observability.md](docs/operations/observability.md) for the full metrics reference.

## Capacity Reference

See [docs/design/appendix-a-capacity-slos.md](docs/design/appendix-a-capacity-slos.md) for per-AGC, per-installation, and per-proxy limits and Service Level Objective (SLO) targets.

## Development

Run `make` (or `make help`) for the full list of targets. The most common ones:

```sh
# Build all binaries (agc, gmc, probe, proxy) into .build/
make build

# Build tool binaries (controller-gen, setup-envtest, ginkgo, kubebuilder)
make tools

# Bring up a kind cluster + local registry, build+push images, and run the standard e2e suite
make e2e-up

# Tear down the kind cluster when done
make e2e-clean
```

### Running tests

This repo uses a `go.work` workspace, so `go test ./...` from the repo root
does **not** discover all modules. Use the per-module commands:

```sh
(cd broker     && go test ./...)    # broker module
(cd githubapp  && go test ./...)    # githubapp module
(cd cmd/agc   && go test ./...)     # AGC module
(cd cmd/gmc   && go test ./...)     # GMC module
(cd cmd/probe && go test ./...)     # probe module
```

Integration tests require the envtest binaries staged via
`KUBEBUILDER_ASSETS`:

```sh
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.30.x \
    --bin-dir /tmp/envtest-bins -p path)

(cd cmd/agc && go test -v -tags integration -timeout 5m -count=1 \
    ./internal/controller/integration/...)
(cd cmd/gmc && go test -v -tags integration -timeout 5m -count=1 \
    ./internal/controller/integration/...)
```

## Repository Layout

```
broker/          GitHub broker client (session management, crypto, metrics)
githubapp/       GitHub App authentication and runner registration
cmd/agc/         Actions Gateway Controller binary
cmd/gmc/         Gateway Manager Controller binary (kubebuilder-generated)
cmd/proxy/       Egress proxy binary
cmd/worker/      Worker pod entrypoint
cmd/probe/       Diagnostic probe for live investigations
docs/            Documentation hub — see docs/README.md
docs/design/     Full system design documentation
docs/development/ Developer workflow guides
docs/operations/ Operator runbooks and references
docs/plan/       Implementation plans and audits
test/            E2E test infrastructure (fakegithub stub, kind configs)
tools/           Vendored build tools (controller-gen, setup-envtest)
vendor/          Workspace-vendored runtime dependencies (`go work vendor`)
```

## License

GitHub Actions Gateway is licensed under the [Apache License 2.0](LICENSE)
(SPDX identifier `Apache-2.0`). Each published container image also carries this
in its `org.opencontainers.image.licenses` label.
