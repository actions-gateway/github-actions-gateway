<p align="center">
  <img src="docs/assets/logo.svg" alt="GitHub Actions Gateway logo" width="140" height="140">
</p>

# GitHub Actions Gateway

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Website](https://img.shields.io/badge/Website-actions--gateway.com-2563eb.svg)](https://actions-gateway.com)
[![Issues](https://img.shields.io/badge/GitHub-Issues-purple.svg)](https://github.com/actions-gateway/github-actions-gateway/issues)

> **Multi-tenant self-hosted GitHub Actions runners on Kubernetes, designed for shared clusters where many teams run runners side by side.**

Actions Runner Controller (ARC) scale-set mode is the common starting point. Once many teams share one cluster, three gaps open up that ARC doesn't address together — GitHub Actions Gateway (GAG) is built to close them:

| Gap at multi-tenant scale | How GAG closes it |
| --- | --- |
| An evicted runner pod leaves its job stuck in GitHub's queue until a manual rerun (GitHub's own queue timeout: at least 24 h, up to 48 h) | Cancels the job within the remaining lock window (~10 min worst case) and reruns, with a per-job retry budget — on the classic acquisition tier today ([detail](docs/design/04-operational-flows.md#worker-pod-eviction-and-auto-retry)) |
| Tenants can't be given isolated GitHub egress IPs | Dedicated per-tenant egress IP pool for allowlisting and contained blast radius |
| Idle runner and listener compute stays provisioned between jobs | Workers scale to zero between jobs; every listener is a goroutine in one shared pod — not an always-on pod per scale set |

Each team self-serves a fully isolated gateway from a single `ActionsGateway` custom resource (CR), running many runner groups (CPU, GPU, large-memory, …) under one shared `ResourceQuota`. The sections below cover **the problem**, **how GAG solves it**, and **how it works**.

## The Problem

Running many runner groups for one tenant in a shared Kubernetes namespace creates four compounding problems that ARC scale-set mode does not address together:

**Scheduling starvation under a shared `ResourceQuota`.** Each ARC `AutoscalingRunnerSet` has its own `maxRunners` cap, but there is no primitive for "GPU runners must always be able to claim at least N slots, regardless of how many CPU runners are active." When cheap CPU pods exhaust namespace quota first, the most expensive hardware reliably loses the race.

**Listener overhead at scale.** ARC runs one listener pod per scale set — held alive 24/7 to long-poll GitHub, each costing a pod slot, a cluster IP, a scheduling unit, an image to pull, and an upgrade surface. A tenant with 10 scale sets holds 10 always-on pods and 10 cluster IPs at rest, before any job runs. Teams that also pin `minRunners > 0` to mask runner-pod cold-start latency multiply this further with idle runner pods on expensive hardware.

**No automatic recovery from worker eviction.** When a runner pod is preempted, OOM-killed, or lost to a node failure, ARC has no built-in flow to cancel the GitHub job lock and rerun. The runner is left orphaned and the job stays stuck in GitHub's queue until someone manually clears the runner and reruns the workflow; the only automatic backstop is GitHub's own queue timeout, documented as at least 24 hours with queue times reaching up to 48.

**Platform team as bottleneck.** Onboarding a tenant means provisioning namespace, quotas, controller scope, scale sets, NetworkPolicies, and egress — a platform-team checklist per team. Subsequent changes (new runner type, quota adjustment, scaling tweak) land as tickets.

## The Solution

**Scheduling priority tiers per `RunnerGroup`.** The `priorityTiers` field maps Kubernetes `PriorityClass` objects to cumulative pod-count thresholds. The first N pods of a GPU runner group get a preempting `PriorityClass` and will displace lower-priority CPU pods when quota is contended — guaranteeing they schedule (measured, Q423). Higher tiers use `preemptionPolicy: Never`, so burst capacity gains scheduling preference without evicting running jobs. A final threshold caps total concurrency per group. The job a preempting tier displaces concludes on GitHub rather than hanging *and* is re-run automatically (measured, Q497) — see the disruption-retry bullet below. Crucially, GAG gates admission at the **broker-claim layer** — it decides whether to take on a job *before* the job is claimed, so a job it cannot place is left queued at GitHub rather than claimed-then-cancelled. That gate reads both limits that can block a worker: the group's own configured ceiling, and live namespace `ResourceQuota` headroom (`hard − used`) for the pod the job would need. A Kubernetes job-queue manager such as Kueue operates one layer below this, on pod creation *after* the job is already claimed, and structurally cannot make that call (see [Appendix D.5](docs/design/appendix-d-alternatives-considered.md#d5-kueue-and-kubernetes-job-queue--quota-managers)). Both acquisition tiers enforce it, in the form each one can: the default scale-set protocol folds both limits into the single capacity integer it advertises to GitHub, so surplus jobs are never assigned; the classic protocol declines the claim per delivered job.

**Automatic disruption retry with fast lock cancel.** When a worker loses its job to infrastructure, the AGC immediately stops lock renewal, so GitHub cancels the job when the outstanding lock lapses: within the remaining lock window, ~10 minutes at worst, instead of stranding it until someone manually clears the runner. The AGC then calls GitHub's rerun API to reschedule. Two disruptions qualify: the kubelet's **node-pressure eviction** (memory or disk exhaustion), seen as `Evicted` pod status, and **scheduler preemption** by a higher `priorityTiers` tier, which deletes its victim and is seen instead as a `DisruptionTarget` condition with reason `PreemptionByScheduler` (Q497). A configurable per-job retry budget — one budget per workflow run, shared across both causes — prevents loops on persistently failing workloads. Runs on both acquisition tiers: the scale-set path records the workflow run on the worker pod and detects the disruption from the owning reconciler, since it provisions fire-and-forget and has no goroutine watching the pod ([design detail](docs/design/04-operational-flows.md#worker-pod-eviction-and-auto-retry)). Scope, measured rather than assumed: an operator's `kubectl drain` or `kubectl delete pod` also takes the graceful-termination path, where the runner reports its own result and the job concludes on GitHub, but is **not** re-run automatically — those removals are indistinguishable from a human deliberately cancelling a run, whereas `PreemptionByScheduler` has exactly one writer ([drain](docs/plan/eviction-oversubscription-validation.md#result-measured-2026-07-27), [preemption](docs/plan/eviction-oversubscription-validation.md#result-measured-2026-07-29-preemption-is-not-eviction)).

**Per-tenant dedicated egress IP pool.** A Horizontal Pod Autoscaler (HPA)-managed pool of stateless HTTPS CONNECT proxy pods per tenant. All GitHub traffic from the AGC and worker pods routes through this pool, so each tenant gets egress IPs never shared with other tenants. Enables per-team allowlisting on the GitHub side, clean per-tenant audit attribution, and contained blast radius for rate limits or abuse flags.

**Self-service tenant management via one CR.** The Gateway Manager Controller (GMC) watches `ActionsGateway` CRs in tenant namespaces and provisions everything the tenant needs — RBAC, NetworkPolicies, egress proxy, AGC, and every runner group declared in the CR — all within the platform-owned namespace `ResourceQuota` (the platform admin owns the quota; the GMC operates inside it). No cluster-admin involvement after initial GMC install. Because tenants control their own configuration, they can diagnose their own runner behavior without escalating to the platform team.

**Scale workers to zero with low listener overhead.** Worker pods are created only when a job is acquired and deleted immediately on completion — the same scale-to-zero behavior as ARC scale-set mode with `minRunners: 0`, so GPU nodes return to the cluster scheduler the moment a job finishes. The difference is the listener: GAG runs every `RunnerGroup`'s listener as a goroutine (~12 KiB of measured session state; see [Appendix A](docs/design/appendix-a-capacity-slos.md)) inside one shared AGC pod, instead of one always-on listener pod per scale set — 10 runner sets is still one pod and one cluster IP at rest. Tenants do not need to pin `minRunners > 0` to mask cold-start latency, so the silent re-introduction of idle GPU pods that pattern causes does not happen.

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

**Tier 2 — AGC.** A Go-based operator deployed once per tenant. Instead of one pod per runner slot, it multiplexes virtual runner sessions as goroutines — designed to scale to thousands per AGC pod. Compute is provisioned only when a job is acquired and released immediately on completion (the finished pod object is deleted after a short configurable TTL). An in-process load test holds ~1,000 concurrent sessions in a single AGC with zero goroutine leak, at a measured ~12 KiB of AGC state per session (~60 KiB as the conservative design bound including live HTTP-connection buffers). The thousands-per-AGC ceiling is a **design target, not yet validated at scale**; the real-cluster load test that would confirm it is deferred post-1.0 (see [Appendix A — Capacity Targets & SLOs](docs/design/appendix-a-capacity-slos.md)).

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

## Installation

GAG ships as the **`actions-gateway` Helm chart**, which installs the Gateway Manager Controller (GMC) and its cluster prerequisites. The GMC then provisions per-tenant gateways at runtime from each `ActionsGateway` CR.

The chart is published, cosign-signed, to the GHCR OCI registry. The current release is **`1.2.0`** (GA) — install it straight from the registry:

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.2.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

Copy the four image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.2.0) and verify the signatures before installing. See the [Installation guide](docs/operations/install.md) for prerequisites, image-digest pinning, the cert-manager toggle, healthy-install verification, and uninstall — and the [chart README](charts/actions-gateway/README.md) for the full values reference.

For day-2 operations — `helm upgrade` / rollback, per-component upgrades, and runbooks — see the [operations docs](docs/operations/), in particular the [upgrade guide](docs/operations/upgrade.md).

## Quick Start

See [docs/getting-started.md](docs/getting-started.md) for the full walkthrough: GMC deployment, GitHub App Secret, and your first tenant.

**New tenants should onboard on `actions-gateway.com/v2beta1`** — the graduated, ScaleSet-only storage and hub version of the decomposed v2 API (`ActionsGateway` + `RunnerSet` + `RunnerTemplate`, with an optional standalone `EgressProxy`). It is v2's first stability contract and where new capability lands. `v2alpha1` stays served as the `gag-migrate` on-ramp, so a migrating v1 tenant can keep the deprecated Classic protocol until it no longer needs it; a new tenant has no reason to start there. The older single-CR `v1alpha1` API is still served but deprecated. `v1alpha1`, `v2alpha1`, and Classic are all **[removed at `v2.0.0`](docs/operations/v1alpha1-deprecation.md)**, announced one release ahead per the project's removal policy; `v2beta1` is not affected. Already on v1? [`gag-migrate`](docs/operations/migration-v1-to-v2.md) moves a tenant to v2 without changing how jobs are acquired.

**Coming from Actions Runner Controller (ARC)?** The [Migrating from ARC guide](docs/operations/migration-from-arc.md) maps ARC scale-set concepts onto v2 and walks one scale set across with zero downtime — same single-name `runs-on` routing, so your workflows need no edit.

## Observability

Both the GMC and AGC expose Prometheus metrics at `/metrics`. See [docs/operations/observability.md](docs/operations/observability.md) for the full metrics reference.

## Capacity Reference

See [docs/design/appendix-a-capacity-slos.md](docs/design/appendix-a-capacity-slos.md) for per-AGC, per-installation, and per-proxy limits and Service Level Objective (SLO) targets.

## Community

Questions, ideas, or running GAG in a real cluster?
[Open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
— it's the place for setup help, bug reports, and feature requests. Issues
opened by operators are the adoption signal the project cares about most.

See [Features](docs/features.md) for what GAG does today, and the
[public roadmap](docs/roadmap.md) for what's next.

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
in its `org.opencontainers.image.licenses` label. Copyright is asserted in the
[NOTICE](NOTICE) file.

---

<p align="center">
  <img src="docs/assets/wormhole-animation.webp" width="480"
       alt="Animated logomark — the faceted gateway ring opens into a wormhole that erupts a plasma burst, then shutters closed">
  <br>
  <sub><em>A secure, dedicated gateway to GitHub for each tenant.<br>Don't let noisy neighbors or secret exfiltrators ruin your sleep.</em></sub>
</p>
