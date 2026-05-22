# GitHub Actions Gateway

A high-scale, virtualized GitHub Actions self-hosted runner system for multi-tenant Kubernetes clusters. It replaces per-tenant runner pods with lightweight goroutine sessions, eliminates idle GPU allocation, and gives every tenant a fully isolated egress identity — without involving the platform team after initial setup.

## The Problem

Running GitHub Actions self-hosted runners in a shared Kubernetes cluster creates two compounding problems:

**Idle resource waste.** ARC (Actions Runner Controller) keeps at least one runner pod per scale set alive at all times. A tenant with ten GPU runner sets holds ten GPU-backed pods perpetually — whether or not a job is queued. At typical on-demand GPU rates, a modest deployment can idle $180K–$360K/month in hardware that isn't doing work.

**Scheduling starvation.** In a namespace with a shared `ResourceQuota`, cheap CPU runner pods can exhaust quota before GPU runner pods have a chance to schedule. ARC provides no mechanism to express minimum scheduling guarantees across runner sets, so the most expensive hardware reliably loses the race.

**Platform team bottleneck.** Every runner set change — new test suite, quota adjustment, scaling tweak — lands as a ticket to the platform team. Teams can't move at their own pace.

## The Solution

A four-tier system that addresses these problems at their root:

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

**Tier 1 — Gateway Manager Controller (GMC).** A cluster-scoped operator deployed once by the platform team. It watches namespace-scoped `ActionsGateway` CRs across all namespaces and provisions a fully isolated gateway instance for each tenant — RBAC, network policies, resource quotas, egress proxy, and AGC — entirely within the tenant's existing namespace.

**Tier 2 — Actions Gateway Controller (AGC).** A Go-based operator deployed once per tenant. Instead of one pod per runner slot, it multiplexes thousands of virtual runner sessions as goroutines. Compute is provisioned only when a job is acquired and garbage-collected immediately on completion. At steady state each goroutine costs ~60 KiB resident — a reduction of over 4,000× compared to a full .NET `Runner.Listener` process.

**Tier 3 — Egress Proxy Pool.** A horizontally autoscaled pool of stateless HTTPS CONNECT proxy pods per tenant. All GitHub traffic from the AGC and worker pods routes through this pool, giving each tenant a dedicated set of egress IPs never shared with other tenants. Supports per-team IP allowlisting, clean audit trails, and contained blast radius.

**Tier 4 — Ephemeral Worker Pod.** A short-lived pod that executes exactly one workflow job and is immediately deleted on completion. Because worker pods exist only while a job is running, zero compute is idle between jobs — GPU nodes return to the cluster scheduler the moment a job finishes.

## Key Properties

**Zero idle GPU allocation.** GPU nodes are only consumed while a job is actively running. The AGC itself runs on CPU-only nodes.

**Scheduling priority tiers.** The `RunnerGroup` `priorityTiers` field maps Kubernetes `PriorityClass` objects to cumulative pod-count thresholds. The first N pods of a GPU runner group get a preempting priority class and will displace lower-priority CPU pods when quota is contended — guaranteeing they schedule. Higher tiers use `preemptionPolicy: Never`, so burst capacity gains scheduling preference without evicting running jobs.

**Automatic eviction retry.** When a worker pod is evicted (preemption or OOM), the AGC detects the `Evicted` status, immediately stops lock renewal so GitHub cancels the job quickly, and calls GitHub's rerun API to reschedule. A configurable retry budget prevents loops on persistently failing workloads.

**Self-service tenant onboarding.** A team creates one `ActionsGateway` CR in their existing namespace and manages all their runner sets from that single resource. No cluster-admin involvement after initial GMC installation.

## Quick Start

### Prerequisites

- Kubernetes 1.11.3+
- Go 1.24+
- A GitHub App with a private key and installation ID

### Create a GitHub App credential Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-github-app
  namespace: team-a
type: Opaque
stringData:
  appId: "123456"
  installationId: "78901234"
  privateKey: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
    -----END RSA PRIVATE KEY-----
```

### Create an ActionsGateway resource

```yaml
apiVersion: actions.gateway/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app
  proxy:
    minReplicas: 2
    maxReplicas: 10
  namespaceQuota:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "50"
  runnerGroups:
    - name: gpu-runners
      runnerLabels: ["self-hosted", "gpu"]
      maxListeners: 10
      priorityTiers:
        - priorityClassName: runner-critical
          threshold: 5
          preemptionPolicy: PreemptLowerPriority
        - priorityClassName: runner-standard
          threshold: 20
          preemptionPolicy: Never
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                limits:
                  nvidia.com/gpu: "1"
    - name: cpu-runners
      runnerLabels: ["self-hosted", "linux"]
      maxWorkers: 30
      podTemplate:
        spec:
          containers:
            - name: runner
```

The GMC will provision the AGC, proxy pool, RBAC, and network policies in `team-a` automatically.

### Deploy the GMC

```sh
# Build and push the GMC image
make docker-build docker-push IMG=<registry>/gmc:tag

# Install CRDs
make install

# Deploy the GMC
make deploy IMG=<registry>/gmc:tag
```

## Architecture

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

## Observability

Both the GMC and AGC expose Prometheus metrics at `/metrics`. Key metrics for production operation:

| Metric | Description |
| --- | --- |
| `actions_gateway_active_sessions` | Currently open long-poll sessions per runner group |
| `actions_gateway_jobs_acquired_total` | Jobs successfully acquired |
| `actions_gateway_job_duration_seconds` | Wall time from acquire to pod completion |
| `actions_gateway_pod_creation_latency_seconds` | Time from acquire to pod scheduled |
| `actions_gateway_eviction_retries_total` | Jobs automatically re-queued after eviction |
| `actions_gateway_eviction_retries_exhausted_total` | Evicted jobs where retry budget was exhausted |
| `actions_gateway_token_refresh_errors_total` | Failed GitHub App token refreshes |
| `actions_gateway_renewjob_errors_total` | RenewJob failures (leading indicator for cancelled jobs) |

## Capacity Reference

| Limit | Value | Notes |
| --- | --- | --- |
| Concurrent sessions per AGC pod (peak) | ≤ 1,000 | ~60 KiB resident per goroutine; 1,000 sessions ≈ 60 MiB |
| Concurrent sessions per GitHub App installation | ≤ 250 | Bounded by GitHub's 15,000 requests/hr rate limit |
| Pod-creation latency (p95) | ≤ 15s | Sub-second on warm nodes; dominated by image pull on cold |
| AGC recovery after restart | ≤ 2 min | GitHub redelivers unacquired jobs within this window |

Tenants requiring more than 250 concurrent sessions should shard across multiple `ActionsGateway` CRs, each backed by a separate GitHub App installation.

## Development

```sh
# Build AGC and probe binaries
make build

# Build tool binaries (controller-gen, setup-envtest)
make tools

# Run unit tests
go test ./...

# Run integration tests
go test -race -tags integration ./...
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
tools/           Vendored build tools (controller-gen, setup-envtest)
```

## License

Apache 2.0
