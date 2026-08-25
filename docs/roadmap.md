---
hide:
  - toc
---

# Roadmap

This page is about what GitHub Actions Gateway (GAG) does **not** do yet.
For what it does today, see [Features](features.md) for every shipped capability, with a link to the doc that explains it, and [Why GAG?](why-gag.md) for the capability-by-capability comparison against Actions Runner Controller (ARC).

GAG is **generally available and installable from the GitHub Container Registry (GHCR)**; the [releases page](https://github.com/actions-gateway/github-actions-gateway/releases) names the current version.
It is Apache-2.0, vendor-neutral, and built for one outcome: real operators running multi-tenant self-hosted runners in real clusters.
There is no paid tier and no commercial roadmap, so the plan below is about capability and adoption, not revenue.

It is a direction-of-travel snapshot, not a dated commitment.
Priorities move with what adopters actually hit first, so the surest way to influence what comes next is to [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues) describing your setup.
Every open item, in priority order, is in the [working backlog](https://actions-gateway.com/dev/STATUS/).

## In progress / near-term

Scoped work, none of it waiting on an outside signal.
Anything that waits on demand, on an unbuilt prerequisite, or on hardware sits under [Exploring / longer-term](#exploring--longer-term) with the event that revives it.

Some of this is committed to a named release, and a pill beside the title says which.
The pill is read from the backlog rather than typed here, so it cannot outlive the commitment.
No pill means the item is on the near-term plan with no release decided for it yet.


- **[A non-privileged path for `container:` and `services:` steps](plan/arc-parity.md#the-collision-the-individual-rows-do-not-state)** <!-- q:Q727 --> ARC runs these as separate pods on a shared volume under `containerMode: kubernetes`.
  One worker pod per job means the path here is Docker-in-Docker, unprivileged only under Kata.
  Its `ReadWriteMany` dependency is closed: [shared worker storage](operations/worker-shared-storage.md) is validated and documented. Documenting Kata Docker-in-Docker as the permanent answer is still a valid outcome.

- **[CI for untrusted pull requests on Kata workers](plan/q408-untrusted-pr-egress.md)** <!-- q:Q408 --> [Kata workers](operations/kata-dind-workloads.md) are validated for *trusted* CI only: the micro-VM bounds the guest kernel, the runner's egress stays permissive.
  Untrusted PRs need an in-cluster pull-through registry mirror plus egress scoped to it, GitHub, and DNS.
  Phase 1 gates hosted-only egress; a measurement grades the posture before Phase 2 builds the mirrors.

- **[Proxy-side audit logging](design/appendix-g-future-enhancements.md#g3-proxy-side-audit-logging)** <!-- q:Q564 --> A structured line per accepted CONNECT: tenant, host and port, bytes each way, duration.
  The proxy emits counters only today, so per-tenant egress is reconstructable just from cluster flow logs.
  Off by default, and the audit-persona dashboard depends on it.

## Exploring / longer-term

Directions we expect to pursue as demand and validated evidence accumulate.
These are intentionally unscheduled.
Each waits on a real operator need, a measured limit, or a gating release before it becomes scheduled work.
The first entry is the exception: a firm commitment, waiting only on the release that carries it.

- **[Retiring `v1alpha1`, `v2alpha1`, and the classic acquisition protocol](operations/v1alpha1-deprecation.md)** <!-- q:Q273 --> Committed, but not yet started. `v1.3.0` is the one-release-ahead announcement; **`v2.0.0`** is the named release that removes all three together, since `v2beta1` is already ScaleSet-only.
  Gated on the `v2` GA API being validated, not on a date.

- **[Validate GHES against a real appliance](plan/arc-parity.md#where-arc-is-actually-ahead)** <!-- q:Q765 --> Both GitHub Enterprise Server (GHES) capabilities ship marked untested against real hardware: the appliance-addressing path and the private-CA bundle.
  They are believed correct and unproven, which is not the same thing.
  Waits on access to an appliance.

- **[Opt-in auto-retry for flaky jobs](design/appendix-g-future-enhancements.md#g17-opt-in-auto-retry-for-flaky-jobs-beyond-disruptions)** <!-- q:Q555 --> A job the cluster disrupts already [re-runs itself](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do); a flaky failure does not.
  Same machinery, opted in per runner set with its own budget so a broken test cannot loop.
  Waits on detection, which needs a real job outcome.

- **Controller horizontal scaling / high availability.** <!-- q:Q169 --> The per-tenant controller runs [one replica by design](design/02-architecture.md): the session registry is in-memory, and HA comes from GitHub redelivering an unacquired job.
  Distributed session state would enable multi-replica HA if a single controller becomes a measured bottleneck.
- **[Bring-your-own proxy TLS certificate](plan/v2-api.md#deferred-out-of-the-critical-path)** <!-- q:Q174 --> Supply the proxy's certificate from your managed PKI or Vault instead of the self-signed default the Gateway Manager Controller (GMC) issues.
  (The autoscaler half has shipped: `managedAutoscaling: false` hands the pool to KEDA, VPA, or a custom HorizontalPodAutoscaler.)
- **[First-class GPU runner support](design/appendix-e-capacity-planning.md)** <!-- q:Q216 --> Priority tiers and the [`NodeShare` sizing profile](operations/worker-rightsizing.md) already carry the GPU cases, but GPU Operator / Node Feature Discovery awareness, and `nodeSelector` / toleration / `RuntimeClass` conventions that make a GPU runner set feel native, wait on a concrete GPU workload to design against.
- **[Multi-node GPU jobs](design/appendix-e-capacity-planning.md)** <!-- q:Q718 --> One job needing several co-scheduled workers in one NVLink or InfiniBand domain is a different problem from one job needing one GPU: capacity is a single integer to GitHub, and a gang requirement is a placement predicate rather than a count.
  Waits on a real multi-node workload, and would interact with Kueue or Volcano.
- **[A worker cache backend](plan/ecosystem-integration-landscape.md#j-registry-build-cache--images-runner-workload-plane)** <!-- q:Q215 --> `actions/cache` already works.
  What is missing is a cache *inside* the cluster, to cut egress cost and restore latency.
  Docker layer caching has no home either way, because workers are storage-less by design.
  It waits on a [security review](design/05-security.md) of cross-job cache isolation: a shared cache between tenants is an obvious exfiltration path.
- **[A warm worker pool](design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers)** <!-- q:Q268 --> An opt-in pool of idle pods per runner set, for teams that still hit pod-schedule latency after image pre-pull and caching.
- **[gVisor validation](plan/milestone-5.md)** <!-- q:Q15 --> The `runtimeClassName` path is [validated end-to-end with Kata](operations/kata-dind-workloads.md); gVisor is documented but unproven on a real cluster.
  It waits on an operator who wants lightweight syscall filtering for compute-only, non-Docker-in-Docker jobs, since Kata already covers the DinD case.
- **[SPIFFE / SPIRE workload identity](plan/v2beta1.md#workload-identity-a-different-config-vault-first)** <!-- q:Q214 --> A keyless, SPIRE-backed signer slots behind the [existing signer interface](design/05-security.md#57-workload-identity-the-no-pem-delegation-model) alongside the deferred cloud-KMS providers, for operators who want no GitHub App private key anywhere.
- **An Operator Lifecycle Manager bundle.** <!-- q:Q217 --> [Helm-only](operations/install.md) is the deliberate install stance; an OperatorHub catalog entry waits on OpenShift demand.
- **A published benchmark and case study.** <!-- q:Q198 --> Real GitHub-at-scale numbers behind the [cost model](design/appendix-f-cost-model.md), which needs a funded scale run rather than a local cluster.

The last three are opt-in additions to the [per-tenant proxy](design/network-architecture.md), shelved together because none has demand recorded against it.
A coherent theme is not a release.

- **[Per-tenant proxy rate limiting](design/appendix-g-future-enhancements.md#g2-proxy-enforced-per-tenant-rate-limiting)** <!-- q:Q565 --> A token bucket at the proxy, so one looping tenant is slowed before it reaches GitHub's ceiling; today the only feedback is a 429 and Actions Gateway Controller (AGC) backoff.
  Per-pod state, since global limits would need a shared backend.

- **[TLS on the in-cluster proxy hop](design/appendix-g-future-enhancements.md#g4-tls-between-agcworkers-and-the-proxy)** <!-- q:Q566 --> The CONNECT target is cleartext between the AGC or workers and the proxy, readable by an eBPF tap, though the tunnelled payload stays TLS to GitHub.
  Mount a cert-manager certificate and move to an `https://` proxy URL.

- **[A dedicated proxy pool per runner group](design/appendix-g-future-enhancements.md#g5-per-runnergroup-dedicated-proxy-pool)** <!-- q:Q567 --> One pool per gateway today, so a bandwidth-heavy group can saturate a co-tenant's.
  Give an opted-in group its own Deployment, Service, and autoscaler.
  Largest of the three; needs a plan doc before code.

## How priorities are set

GAG's success metric is **external operators running it and telling us what breaks**, not stars or downloads.
That feedback drives the ordering above far more than any internal plan.
If something here is in your way, or missing entirely, [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues).
That is the fastest way to move it up.

The page above is the adopter-facing summary.
The day-to-day ordering behind it is the **[working backlog](https://actions-gateway.com/dev/STATUS/)**: every open item, filterable by label, status, and size.
It tracks the unreleased `main` branch, so it is published only on the `dev` version of this site and carries no commitment: rows are added, reordered, and deleted as work lands.
