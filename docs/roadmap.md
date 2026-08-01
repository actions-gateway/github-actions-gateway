---
hide:
  - toc
---

# Roadmap

This page is about what GAG does **not** do yet. For what it does today, see
[Features](features.md) — every shipped capability, with a link to the doc that
explains it — and [Why GAG?](why-gag.md) for the capability-by-capability
comparison against Actions Runner Controller (ARC).

GitHub Actions Gateway (GAG) is **1.0, generally available, and installable from
the GitHub Container Registry (GHCR)**. It is Apache-2.0, vendor-neutral, and
built for one outcome: real operators running multi-tenant self-hosted runners in
real clusters. There is no paid tier and no commercial roadmap — the plan below is
about capability and adoption, not revenue.

It is a direction-of-travel snapshot, not a dated commitment. Priorities move with
what adopters actually hit first, so the surest way to influence what comes next is
to [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
describing your setup. Every open item, in priority order, is in the
[working backlog](https://actions-gateway.com/dev/STATUS/).

## In progress / near-term

Work that is scoped and actively being built — adoption-enabling polish and the
last gaps an outside operator hits.

- **CI for untrusted pull requests on Kata workers.** <!-- q:Q408 --> [Kata
  workers](operations/kata-dind-workloads.md) are validated today, but only for
  *trusted* CI: the isolation bounds the guest kernel while the runner's egress
  stays permissive, because its jobs pull from CDN-fronted public registries that
  no CIDR allow-list can pin. Running an external contributor's pull request
  safely takes two pieces on top of what ships: an in-cluster pull-through
  registry mirror, so a worker needs no direct registry egress at all, and an
  egress policy scoped to that mirror, GitHub, and DNS. An operator asked for this
  as a supported posture, so it is scheduled work with a phased plan; the
  remaining phases are measurement, the mirror deployment, and live-validated
  enforcement on our own end-to-end CI.

- **A curated runner template library.** <!-- q:Q554 --> Every tenant writes its
  own worker pod template from scratch today, including the fiddly parts: the
  Docker-in-Docker sidecar, the Kata `runtimeClassName`, the volume and
  security-context wiring. The templates our own end-to-end CI exercises on every
  run become a shipped kustomize base you patch, rather than a snippet you copy
  out of the docs. No new API surface, and the bar for publishing a template is
  that CI actually runs it.

- **Opt-in auto-retry for flaky jobs.** <!-- q:Q555 --> A job the cluster
  disrupts already
  [re-runs automatically](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do).
  A job that simply failed flakily does not, so today that is someone re-running
  it by hand. The same machinery can cover it, opted in per runner set and given
  its own retry budget so a genuinely broken test cannot loop. Detection comes
  first: the gateway has to measure re-run-then-pass rates before acting on them
  is honest.

- **Richer egress proxy.** <!-- q:Q564,Q565,Q566,Q567 --> Four additions to the
  [per-tenant proxy](design/network-architecture.md), each opt-in: a
  per-connection audit trail of which tenant reached which destination,
  per-tenant rate limiting so one looping workflow can be slowed before it
  exhausts a shared GitHub quota, TLS on the in-cluster hop so the CONNECT target
  is not readable by a cluster-wide network tap, and dedicated proxy pools so a
  bandwidth-heavy runner group cannot crowd out a quieter one. Destination
  allow-listing is deliberately absent from this list: it
  [already ships](features.md#security-posture) as an opt-in control.

## Exploring / longer-term

Directions we expect to pursue as demand and validated evidence accumulate. These
are intentionally unscheduled — each waits on a real operator need, a measured
limit, or a gating release before it becomes scheduled work. The first entry is
the exception that proves the rule: it is a firm commitment, waiting only on the
release that carries it.

- **Retiring `v1alpha1`, `v2alpha1`, and the classic acquisition protocol.** <!-- q:Q273 --> The
  graduation that made this possible has shipped, so what remains is the removal
  itself — committed, but not yet started. Policy is that a removal lands on a named
  release announced at least one release ahead: `v1.3.0` is that announcement, and
  **`v2.0.0` is the named release** that removes all three. They are one bundle
  because `v2beta1` is already ScaleSet-only, so classic acquisition exists solely to
  serve `v1alpha1` and `v2alpha1` objects. `v2.0.0` is itself gated on the `v2` GA API
  being available and validated, not on a date, so the work stays parked until that
  gate clears. Detail: the
  [deprecation and removal notice](operations/v1alpha1-deprecation.md).

- **Controller horizontal scaling / high availability.** <!-- q:Q169 --> The per-tenant
  controller is [single-replica by design](design/appendix-e-capacity-planning.md)
  today; distributed session state would enable multi-replica HA if a single
  controller becomes a measured bottleneck.
- **Bring-your-own proxy TLS certificate.** <!-- q:Q174 --> Supply the
  [proxy's certificate](design/network-architecture.md) from your managed PKI or
  Vault instead of the GMC's self-signed default. (The autoscaler half has
  shipped: `managedAutoscaling: false` hands the pool to KEDA, VPA, or a custom
  HorizontalPodAutoscaler.)
- **Cross-namespace proxy sharing.** <!-- q:Q166 --> Share an
  [egress proxy pool](design/network-architecture.md) across namespaces with
  explicit consent (same-namespace sharing already works).
- **First-class GPU runner support.** <!-- q:Q216 --> Priority tiers and the
  [`NodeShare` sizing profile](operations/worker-rightsizing.md) already carry the
  GPU cases, but GPU Operator / Node Feature Discovery awareness, and
  `nodeSelector` / toleration / `RuntimeClass` conventions that make a GPU runner
  set feel native, wait on a concrete GPU workload to design against.
- **A worker cache backend.** <!-- q:Q215 --> Workers are storage-less by design, so
  `actions/cache` and Docker layer caching have no home. Adding an optional
  PVC or object-store cache needs a
  [security review](design/05-security.md) of cross-job cache isolation first: a
  shared cache between tenants' jobs is an obvious exfiltration path.
- **A warm worker pool.** <!-- q:Q268 --> An opt-in pool of
  [idle pods per runner set](design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers),
  for teams that still hit pod-schedule latency after image pre-pull and caching.
- **gVisor validation.** <!-- q:Q15 --> The `runtimeClassName` path is
  [validated end-to-end with Kata](operations/kata-dind-workloads.md); gVisor is
  documented but unproven on a real cluster. It waits on an operator who wants
  lightweight syscall filtering for compute-only, non-Docker-in-Docker jobs, since
  Kata already covers the DinD case.
- **SPIFFE / SPIRE workload identity.** <!-- q:Q214 --> A keyless, SPIRE-backed signer
  slots behind the
  [existing signer interface](design/05-security.md#57-workload-identity-the-no-pem-delegation-model)
  alongside the deferred cloud-KMS providers, for operators who want no GitHub App
  private key anywhere.
- **An Operator Lifecycle Manager bundle.** <!-- q:Q217 -->
  [Helm-only](operations/install.md) is the deliberate install stance; an
  OperatorHub catalog entry waits on OpenShift demand.
- **A published benchmark and case study.** <!-- q:Q198 --> Real GitHub-at-scale
  numbers behind the [cost model](design/appendix-f-cost-model.md), which needs a
  funded scale run rather than a local cluster.

## How priorities are set

GAG's success metric is **external operators running it and telling us what
breaks** — not stars or downloads. That feedback drives the ordering above far
more than any internal plan. If something here is in your way, or missing
entirely, [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
— it's the fastest way to move it up.

The page above is the adopter-facing summary. The day-to-day ordering behind it
is the **[working backlog](https://actions-gateway.com/dev/STATUS/)** — every
open item, filterable by label, status, and size. It tracks the unreleased
`main` branch, so it is published only on the `dev` version of this site and
carries no commitment: rows are added, reordered, and deleted as work lands.
