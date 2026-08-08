<p align="center">
  <img src="docs/assets/logo.svg" alt="GitHub Actions Gateway logo" width="140" height="140">
</p>

# GitHub Actions Gateway

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Website](https://img.shields.io/badge/Website-actions--gateway.com-2563eb.svg)](https://actions-gateway.com)
[![Issues](https://img.shields.io/badge/GitHub-Issues-purple.svg)](https://github.com/actions-gateway/github-actions-gateway/issues)

> **Multi-tenant self-hosted GitHub Actions runners on Kubernetes, for clusters where many teams run runners side by side.**

Actions Runner Controller (ARC) scale-set mode is the common starting point. Once many teams share one cluster, three gaps open that ARC does not close. GitHub Actions Gateway (GAG) closes them:

| Gap at multi-tenant scale | How GAG closes it | Detail |
| --- | --- | --- |
| A worker the cluster takes away leaves its job needing a **manual rerun** | The run concludes and **re-runs itself**, with a per-run retry budget, on both acquisition tiers | [flow](docs/design/04-operational-flows.md#worker-pod-eviction-and-auto-retry) |
| Tenants **can't be given isolated GitHub egress IPs** | A per-tenant proxy pool as the choke point, over a specified and live-validated path to a distinct, stable per-tenant source IP | [mechanism](docs/design/network-architecture.md#per-tenant-egress-ip-the-source-ip-mechanism) |
| **Idle runner and listener compute** stays provisioned between jobs | Workers scale to zero. Every listener is a goroutine in one shared pod, not an always-on pod per scale set | [capacity](docs/design/appendix-a-capacity-slos.md) |

Each team self-serves a fully isolated gateway from a single `ActionsGateway` custom resource (CR), running many runner groups (CPU, GPU, large-memory, and so on) under one shared `ResourceQuota` the tenant cannot raise.

### Measured, not projected

| | |
| --- | --- |
| **15–26 s** | Recovery from a preemption or drain. The run concludes at GitHub and re-runs itself, with no ticket and no manual rerun |
| **1 pod** | Listener footprint at rest, whatever the runner-set count. ARC runs one always-on listener pod per set |
| **0** | Idle GPU pods between jobs. Workers exist only while a job runs |
| **20** | Alert rules shipped as code, with a tenant dashboard and a platform dashboard beside them |

ARC-side claims were measured on 2026-08-06 against the then-current ARC `gha-runner-scale-set` chart release and its `master` branch. [Capability by capability](docs/why-gag.md) names that exact version and carries the method.

## Is this for you?

**GAG is for the case where the nodes are big and expensive, several teams have to share them, and that sharing has to be safe.** If that is not the shape of your problem, something simpler wins, and we would rather say so:

| If | Use instead |
| --- | --- |
| A vendor may run your jobs | a managed runner service (Blacksmith, Namespace, Depot, WarpBuild, Ubicloud) |
| Managed Kubernetes is cheap and your CI fits one cloud | a cluster per team, in its own project. A project boundary isolates harder than any shared cluster can |
| One team owns the cluster and the runners | [ARC](https://github.com/actions/actions-runner-controller). GAG speaks the same protocol through the same client library, so this is not a protocol argument |
| Your compute is elastic cloud capacity and you want no cluster | [RunsOn](https://runs-on.com), [terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner), [Actuated](https://actuated.com) |

[Choosing how to run self-hosted CI](docs/alternatives.md) argues each one, and states where GAG loses and to whom.

## The problem

Running many runner groups for one tenant in a shared Kubernetes namespace creates four problems.

**No automatic recovery when the cluster takes a worker away.** When a runner pod is preempted, drained, or evicted under node pressure, ARC has no re-run flow: `deleteEphemeralRunnerOrPod` deletes the `EphemeralRunner` and calls `RemoveRunner`, so the job is given up on (measured against ARC `master`, 2026-08-06). Re-running it is a manual step. GitHub's own backstop is the queue timeout: a job can sit queued for 24 hours before it is automatically cancelled.

**Platform team as bottleneck.** Onboarding a tenant means provisioning namespace, quotas, controller scope, scale sets, NetworkPolicies, and egress: a platform-team checklist per team. Subsequent changes (new runner type, quota adjustment, scaling tweak) land as tickets.

**Scheduling starvation under a shared `ResourceQuota`.** Each ARC `AutoscalingRunnerSet` has its own `maxRunners` cap. Nothing expresses the reservation you actually want: GPU runners must always be able to claim at least N slots, no matter how many CPU runners are active. When cheap CPU pods exhaust namespace quota first, the most expensive hardware reliably loses the race.

**Listener overhead at scale.** ARC runs one listener pod per scale set, held alive 24/7 to long-poll GitHub. Each one costs a pod slot, a pod IP, a scheduling unit, an image to pull, and an upgrade surface. A tenant with 10 scale sets holds 10 always-on pods at rest, before any job runs, in the controller's namespace rather than the tenant's. Teams that also pin `minRunners > 0` to mask runner-pod cold-start latency add idle runner pods on expensive hardware on top of that.

## What GAG does

### Jobs the cluster disrupts re-run themselves

When a worker loses its job to infrastructure, the run concludes at GitHub and the AGC calls the rerun API to reschedule it. A preemption or a drain concludes in **15 to 26 seconds** (measured), because the runner keeps its grace period and reports. A hard eviction has no report to send, so the job lock has to lapse first, about 10 minutes at worst.

Three marks qualify a disruption:

| Mark | What GAG sees |
| --- | --- |
| Kubelet **node-pressure eviction** (memory or disk exhaustion) | `Evicted` pod status |
| **Scheduler preemption** by a higher `priorityTiers` tier | a `DisruptionTarget` condition, reason `PreemptionByScheduler` (Q497) |
| **External graceful deletion** by a drain or a bare `kubectl delete pod` | a `deletionTimestamp` the AGC reads as its terminal phase publishes (Q502) |

**The scope is drawn on purpose.** A job that ran and **failed on its own merits**, a run you **cancelled**, and workers the gateway's own cleanup reaped as stuck are never re-run; the reaper stamps `actions-gateway.com/deletion-reason` on a pod before deleting it, which is what keeps those out. A configurable per-job retry budget, one per workflow run and shared across every cause, prevents loops on persistently failing workloads.

Both acquisition tiers run the flow, with different mechanisms: the classic tier also stops renewing the job lock the moment it detects the loss, while on the default scale-set tier the runner owns renewal, so the scale-set path records the workflow run on the worker pod and detects the disruption from the owning reconciler instead. [The full boundary, with detection marks and metrics](docs/operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do) · [design detail](docs/design/04-operational-flows.md#worker-pod-eviction-and-auto-retry)

### Quotas are safe to enforce, because admission happens before the claim

GAG gates admission at the **broker-claim layer**: it decides whether to take on a job *before* the job is claimed, so a job it cannot place is left queued at GitHub rather than claimed-then-cancelled. The gate reads both limits that can block a worker: the group's own configured ceiling, and live namespace `ResourceQuota` headroom (`hard − used`) for the pod the job would need.

Both acquisition tiers enforce it in the form each one allows. The default scale-set protocol folds both limits into the single capacity integer it advertises to GitHub, so surplus jobs are never assigned. The classic protocol declines the claim per delivered job.

An opt-in `capacityGate.mode` adds a third rung, *placeability*, for the case where quota has room but the cluster does not: a worker shape that no node can take and the node autoscaler will not grow for. Without it, a set with a drained node pool or vanished spot capacity keeps claiming, and each job spends a single-use runner registration and ends as a cancelled run. With it, those jobs stay queued at GitHub. `Off` by default ([runbook](docs/operations/troubleshooting.md#jobs-not-being-acquired-despite-queued-work-capacity-gate-saturated)).

A Kubernetes job-queue manager such as Kueue operates one layer below this, on pod creation *after* the job is already claimed, and structurally cannot make that call ([Appendix D.5](docs/design/appendix-d-alternatives-considered.md#d5-kueue-and-kubernetes-job-queue--quota-managers)).

### Tenants self-serve, inside a quota they cannot raise

The Gateway Manager Controller (GMC) watches `ActionsGateway` CRs in tenant namespaces and provisions what the tenant needs: role-based access control (RBAC), NetworkPolicies, egress proxy, AGC, and every runner group declared in the CR. All of it lands inside the platform-owned namespace `ResourceQuota`. The platform admin owns the quota, the GMC operates within it, and after the initial GMC install no step needs a cluster admin. Tenants control their own configuration, so they can diagnose their own runner behavior without escalating to the platform team.

### Each tenant gets its own egress identity

All GitHub traffic from the AGC and worker pods routes through a per-tenant proxy pool ([Tier 3](#architecture)), which is the per-tenant choke point and is pinned with `spec.scheduling`. The pool alone does not decide the source IP GitHub sees; a *distinct, stable* per-tenant address comes from Cilium Egress Gateway or per-tenant cloud NAT beneath it, both specified in the [egress-IP mechanism](docs/design/network-architecture.md#per-tenant-egress-ip-the-source-ip-mechanism) and live-validated on GKE Dataplane V2 (2026-07-13). That is what makes per-team allowlisting, per-tenant audit attribution, and a contained blast radius coherent.

### Also in the box

**Scheduling priority tiers per `RunnerGroup`.** The `priorityTiers` field maps Kubernetes `PriorityClass` objects to cumulative pod-count thresholds. The first N pods of a GPU runner group get a preempting `PriorityClass`, so they displace lower-priority CPU pods when quota is contended and are guaranteed to schedule (measured, Q423). Higher tiers use `preemptionPolicy: Never`, so burst capacity gains scheduling preference without evicting running jobs, and a final threshold caps total concurrency per group. The job a preempting tier displaces concludes on GitHub rather than hanging, and is re-run automatically (measured, Q497).

**Scale workers to zero with low listener overhead.** The AGC creates a worker pod only when a job is acquired and deletes it on completion, the same scale-to-zero behavior as ARC scale-set mode with `minRunners: 0`. The difference is the listener. GAG runs every `RunnerGroup`'s listener as a goroutine inside one shared AGC pod (~12 KiB of measured session state, see [Appendix A](docs/design/appendix-a-capacity-slos.md)) instead of one always-on listener pod per scale set, so 10 runner sets is still one pod at rest. (The ~12 KiB figure is measured against the classic listener; the pod-count claim holds on both tiers.) Tenants also do not need to pin `minRunners > 0` to mask cold-start latency, the pattern that quietly puts idle GPU pods back on the cluster.

**Measured worker right-sizing.** Every worker's CPU and memory `requests` start as a guess in the tenant's `RunnerTemplate`. The AGC samples per-job peaks from `metrics.k8s.io` and publishes recommended requests and limits on the `RunnerSet` status, raising a `SizingDrift` condition when the guess and the measurement diverge. Opt-in profiles then apply the measurement at pod-build time: `Binpack` and `Throughput` derive from observed usage, `NodeShare` from a node's allocatable capacity. All of them clamp to `minRequests`/`maxRequests`, fall back to the template until there are enough samples, and never touch GPU resources. Validated on GKE: a worker took a derived `1500m` where its templates asked for 2 and 3 CPU. Ephemeral runner pods are structurally out of reach for stock Vertical Pod Autoscaler (one job, minutes long, nothing to group them), so the loop closes inside the controller that builds the pods ([guide](docs/operations/worker-rightsizing.md), [Appendix D.7](docs/design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on)).

**Per-tenant utilization metrics.** Both the GMC and AGC expose Prometheus metrics scoped per tenant and runner group. Teams can see their own GPU utilization and argue for quota adjustments without cluster-wide visibility.

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

**Tier 1: Gateway Manager Controller (GMC).** A cluster-scoped operator deployed once by the platform team. It watches namespace-scoped `ActionsGateway` CRs across all namespaces and provisions a fully isolated gateway instance for each tenant (RBAC, network policies, resource quotas, egress proxy, and AGC) entirely within the tenant's existing namespace.

**Tier 2: AGC.** A Go-based operator deployed once per tenant. Instead of one pod per runner slot, it multiplexes virtual runner sessions as goroutines, designed to scale to thousands per AGC pod. It provisions compute only when a job is acquired and releases it on completion (the finished pod object is deleted after a short configurable TTL).

**Tier 3: Egress Proxy Pool.** A Horizontal Pod Autoscaler (HPA)-managed pool of stateless HTTPS CONNECT proxy pods, one pool per tenant. Every GitHub request from the AGC and from worker pods leaves the cluster through it, on IPs dedicated to that tenant.

**Tier 4: Ephemeral Worker Pod.** A short-lived pod that executes exactly one workflow job and is immediately deleted on completion. Because worker pods exist only while a job is running, zero compute is idle between jobs, and GPU nodes return to the cluster scheduler the moment a job finishes.

> [!NOTE]
> **The thousands-of-sessions-per-AGC ceiling is a design target, not yet validated at scale.** An in-process load test holds ~1,000 concurrent sessions in a single AGC with zero goroutine leak, at a measured ~12 KiB of AGC state per session (~60 KiB as the conservative design bound, including live HTTP-connection buffers). The real-cluster load test that would confirm the ceiling is deferred post-1.0. See [Capacity Targets and SLOs](docs/design/appendix-a-capacity-slos.md), which also carries a published retraction of this project's own best number once it proved unmeasurable.

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

The chart is published to the GHCR OCI registry and signed with cosign. The current release is **`1.3.0`** (GA; charts carry no leading `v`, images are tagged `v1.3.0`). Install it straight from the registry:

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.3.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

Copy the four image digests from the [release notes](https://github.com/actions-gateway/github-actions-gateway/releases/tag/v1.3.0) and verify the signatures before installing. The [Installation guide](docs/operations/install.md) covers prerequisites, image-digest pinning, the cert-manager toggle, healthy-install verification, and uninstall; the [chart README](charts/actions-gateway/README.md) is the full values reference.

> [!IMPORTANT]
> **Upgrading is not `helm upgrade` alone.** Helm installs the chart-root `crds/` directory on a fresh install only, so every upgrade starts by piping `helm show crds` for the target version into `kubectl apply`. The 1.3.0 upgrade also moves a `priorityClassAllowlist.configMapName` into the new cluster-scoped `PriorityClassAllowlist` CR, which fails closed. Both steps are guarded, and the [upgrade guide](docs/operations/upgrade.md) covers rolling back.

For day-2 operations (`helm upgrade` and rollback, per-component upgrades, runbooks) see the [operations docs](docs/operations/).

## Quick start

See [docs/getting-started.md](docs/getting-started.md) for the full walkthrough: GMC deployment, GitHub App Secret, and your first tenant.

**New tenants should onboard on `actions-gateway.com/v2beta1`**, the graduated, ScaleSet-only storage and hub version of the decomposed v2 API (`ActionsGateway` + `RunnerSet` + `RunnerTemplate`, with an optional standalone `EgressProxy`). It is v2's first stability contract and where new capability lands.

| API version | Status |
| --- | --- |
| `v2beta1` | **Start here.** Stability contract, unaffected by the removals below |
| `v2alpha1` | Deprecated, apiserver warns on every apply. Stays served as the `gag-migrate` on-ramp so a migrating v1 tenant can keep Classic until it no longer needs it. [Removed at `v2.0.0`](docs/operations/v1alpha1-deprecation.md) |
| `v1alpha1` | Deprecated single-CR API, still served. [Removed at `v2.0.0`](docs/operations/v1alpha1-deprecation.md) |
| Classic protocol | Deprecated. [Removed at `v2.0.0`](docs/operations/v1alpha1-deprecation.md) |

Removals are announced one release ahead, per the project's removal policy.

**Already on v1?** [`gag-migrate`](docs/operations/migration-v1-to-v2.md) moves a tenant to v2 without changing how jobs are acquired.

**Coming from Actions Runner Controller (ARC)?** The [Migrating from ARC guide](docs/operations/migration-from-arc.md) maps ARC scale-set concepts onto v2 and walks one scale set across with zero downtime, on the same single-name `runs-on` routing, so your workflows need no edit.

## Where to go next

| | |
| --- | --- |
| What GAG does today, and what it does not | [Features](docs/features.md) |
| What is next | [Public roadmap](docs/roadmap.md) |
| Capability by capability against ARC | [Why GAG?](docs/why-gag.md) |
| Whether GAG is the right choice at all | [Choosing how to run self-hosted CI](docs/alternatives.md) |
| Metrics, dashboards, and the 20 shipped alert rules | [Observability](docs/operations/observability.md) |
| Per-AGC, per-installation, and per-proxy limits and SLO targets | [Capacity Targets and SLOs](docs/design/appendix-a-capacity-slos.md) |
| Who owns what, and what each role cannot do | [Personas](docs/operations/personas.md) |

## Community

Questions, ideas, or running GAG in a real cluster?
[Open an issue](https://github.com/actions-gateway/github-actions-gateway/issues).
It is the place for setup help, bug reports, and feature requests. Issues
opened by operators are the adoption signal this project cares about most.

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

<details>
<summary><strong>Running tests</strong> (this repo uses a <code>go.work</code> workspace, so <code>go test ./...</code> from the root does <em>not</em> discover all modules)</summary>

<br>

Use the per-module commands:

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

</details>

## Repository layout

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
