# GitHub Actions Gateway

A Kubernetes operator for managing self-hosted GitHub Actions runners on multi-tenant clusters that scales to zero when the job queue is empty.

Unlike Actions Runner Controller (ARC), which co-locates the queue listener and the job worker, GitHub Actions Gateway (GAG) runs listeners as goroutines in a separate pod and only creates worker pods when a job is acquired from the queue. This reduces waste from idle workers, especially when they need expensive GPUs or lots of resources.

In addition to saving money, GAG uses its unique architecture to solve several other problems encountered when using ARC at scale in production enterprise environments, like consolidating egress IPs for allowlisting, tolerating eviction with auto-retries, and gradually reducing priority as horizontal scale increases to ensure fair use of limited resources across multiple runner groups.

## The Problem

Running GitHub Actions self-hosted runners in a shared Kubernetes cluster creates three compounding problems:

**Idle resource waste.** ARC keeps at least one runner pod per scale set alive at all times. A tenant with ten GPU runner sets holds ten GPU-backed pods perpetually — whether or not a job is queued.

**Scheduling starvation.** In a namespace with a shared `ResourceQuota`, cheap CPU runner pods can exhaust quota before GPU runner pods have a chance to schedule. ARC provides no mechanism to express minimum scheduling guarantees across runner sets, so the most expensive hardware reliably loses the race.

**Platform team bottleneck.** Every runner set change — new test suite, quota adjustment, scaling tweak — lands as a ticket to the platform team. Teams can't move at their own pace.

## The Solution

**Zero idle GPU allocation.** GPU nodes are only consumed while a job is actively running. The Actions Gateway Controller (AGC) itself runs on CPU-only nodes.

**Scheduling priority tiers.** The `RunnerGroup` `priorityTiers` field maps Kubernetes `PriorityClass` objects to cumulative pod-count thresholds. The first N pods of a GPU runner group get a preempting priority class and will displace lower-priority CPU pods when quota is contended — guaranteeing they schedule. Higher tiers use `preemptionPolicy: Never`, so burst capacity gains scheduling preference without evicting running jobs.

**Automatic eviction retry.** When a worker pod is evicted (preemption or out-of-memory (OOM)), the AGC detects the `Evicted` status, immediately stops lock renewal so GitHub cancels the job quickly, and calls GitHub's rerun API to reschedule. A configurable retry budget prevents loops on persistently failing workloads.

**Self-service tenant management.** Teams declare all their runner sets in one `ActionsGateway` CR they own in their own namespace — no cluster-admin involvement after initial setup. Because tenants control their own configuration, they can diagnose their own runner behavior without escalating to the platform team.

**Per-tenant utilization metrics.** Both the GMC and AGC expose Prometheus metrics scoped per tenant and runner group. Teams have the data to understand their own GPU utilization and make the case for quota adjustments without relying on cluster-wide visibility.

## Architecture

A four-tier system:

```
  Tenant namespace                           System namespace
  ----------------                           ----------------
  +-----------------------+                  +----------------------------+
  |  ActionsGateway CR    |  ──watch──────>  |  Gateway Manager Controller|
  |  (namespace-scoped)   |                  |          (GMC)             |
  +-----------------------+                  +----------------------------+
          │                                          │
          │              ┌──── provisions ───────────┘
          ▼              ▼
  +------------------------------------------------------------+
  |  Tenant namespace                                          |
  |  • Egress Proxy Pool  (HPA-managed, per-tenant egress IPs) |
  |  • Actions Gateway Controller  (AGC, goroutine multiplexer)|
  |  • Ephemeral Worker Pods  (one per job, GC'd on completion)|
  +------------------------------------------------------------+
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

## Quick Start

See [docs/getting-started.md](docs/getting-started.md) for the full walkthrough: GitHub App Secret, `ActionsGateway` CR, and GMC deployment.

## Observability

Both the GMC and AGC expose Prometheus metrics at `/metrics`. See [docs/operations/observability.md](docs/operations/observability.md) for the full metrics reference.

## Capacity Reference

See [docs/design/appendix-a-capacity-slos.md](docs/design/appendix-a-capacity-slos.md) for per-AGC, per-installation, and per-proxy limits and Service Level Objective (SLO) targets.

## Development

Run `make` (or `make help`) for the full list of targets. The most common ones:

```sh
# Build AGC and probe binaries
make build

# Build tool binaries (controller-gen, setup-envtest, ginkgo, kubebuilder)
make tools

# Bring up a kind cluster + local registry, build+push images, and run the standard e2e suite
make e2e-up

# Tear down the kind cluster when done
make e2e-clean

# Run unit tests (per-module — `go test ./...` from the repo root does not
# work; see CLAUDE.md "Go workspaces" section for the workspace mechanics)
GOWORK=off go test ./...        # root: broker, githubapp
(cd cmd/agc && go test ./...)
(cd cmd/gmc && go test ./...)

# Run integration tests (envtest-backed; requires KUBEBUILDER_ASSETS)
make setup-envtest
export KUBEBUILDER_ASSETS=$(.build/setup-envtest use 1.30.x --bin-dir /tmp/envtest-bins -p path)
(cd cmd/agc && go test -tags integration ./internal/controller/integration/...)
```

## Repository Layout

```
broker/          GitHub broker client (session management, crypto, metrics)
cmd/agc/         Actions Gateway Controller binary
cmd/gmc/         Gateway Manager Controller binary (kubebuilder-generated)
cmd/proxy/       Egress proxy binary
cmd/worker/      Worker pod entrypoint
cmd/probe/       Diagnostic probe for live investigations
docs/design/     Full system design documentation
docs/plan/       Implementation milestone plans
internal/        Shared test helpers
test/            E2E test infrastructure (fakegithub stub, kind configs)
tools/           Vendored build tools (controller-gen, setup-envtest)
vendor/          Workspace-vendored runtime dependencies (`go work vendor`)
```
