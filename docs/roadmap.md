---
hide:
  - toc
---

# Roadmap

GitHub Actions Gateway (GAG) is **1.0, generally available, and installable from
the GitHub Container Registry (GHCR)**. It is Apache-2.0, vendor-neutral, and
built for one outcome: real operators running multi-tenant self-hosted runners in
real clusters. There is no paid tier and no commercial roadmap — the plan below is
about capability and adoption, not revenue.

This page is a direction-of-travel snapshot, not a dated commitment. Priorities
move with what adopters actually hit first, so the surest way to influence what
comes next is to [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
describing your setup.

## Available now (1.0)

Everything here is merged and covered by the design and operations docs. Use the
**version selector** at the top of the page to switch between the latest
**stable [release](https://github.com/actions-gateway/github-actions-gateway/releases)**
(the default you land on) and **`dev`** (the unreleased `main` branch): an item
that appears under `dev` but not yet under a numbered release hasn't shipped in a
tagged chart. Check the release notes for the exact image digests to pin.

- **One resource per tenant.** A single `ActionsGateway` custom resource
  provisions an isolated gateway — controller, egress proxy pool, role-based
  access control (RBAC), and network policies — inside the platform-owned quota.
- **Automatic recovery for blocked and evicted jobs.** A job the namespace
  `ResourceQuota` has no room for is never taken on, so it stays queued at GitHub
  until there is capacity; an evicted job is cancelled at GitHub when its lock
  lapses (~10 min at worst) and re-queued, with a per-run retry budget. No manual
  rerun either way. Both run on **both acquisition tiers**: the default scale-set
  protocol folds live quota headroom into the capacity it advertises to GitHub,
  and the classic tier declines the claim per delivered job.
- **Capacity gate for unplaceable workers (opt-in).** A runner set can set
  `capacityGate.mode` so the gateway stops taking on jobs while the cluster cannot
  place its worker shape — a drained pool, a changed taint, spot capacity gone.
  Jobs stay queued at GitHub instead of being claimed and cancelled. It bounds the
  *rate* of wasted claims rather than eliminating the first one. The tenant turns it
  on; the platform states once, on the gateway, whether the cluster has a node
  autoscaler — because an unschedulable pod means opposite things depending on the
  answer, and where a node may still arrive the gate waits for the autoscaler (or
  Karpenter) to say it will not add one. Off by default.
- **Priority tiers per runner group.** Reserve a guaranteed floor of slots for
  expensive runner types so cheap CPU jobs can't starve critical GPU work.
- **Worker scale-up rate limiting (opt-in).** An optional per-runner-group token
  bucket (`scaleUp.maxPerSecond`/`burst`) that caps how *fast* worker pods start —
  distinct from the count ceiling — to smooth cold-start stampedes on shared
  egress (NAT / firewall / VPN) when many jobs land at once. Off by default; ARC
  exposes only a `maxRunners` count cap, not a creation-rate control.
- **Scale-to-zero workers with low listener overhead.** Worker pods exist only
  while a job runs; listeners are goroutines (~12 KiB each) in one shared pod
  rather than an always-on listener pod per runner group.
- **Per-tenant isolated egress IPs.** A dedicated proxy pool per tenant gives
  each team its own GitHub egress IPs to allow-list, with a contained blast
  radius. <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span> In the v2 API the proxy is a standalone,
  optionally shared `EgressProxy` (or omitted for direct egress), with a
  DNS-aware (FQDN) egress-policy mode on Cilium/Calico.
- **Observability, per tenant and fleet-wide.** Prometheus metrics scoped per
  tenant and runner group, plus ready-to-apply Grafana dashboards and alerts as
  code, and a cross-tenant rollup for platform admins.
- **Measured worker right-sizing.** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span> The gateway samples every
  worker pod's CPU/memory, publishes per-runner-set, per-job usage peaks, and
  derives the recommendation for you: recommended `requests`/`limits` surfaced
  in `RunnerSet` status (with sample count and observed p95/max for confidence)
  plus an advisory `SizingDrift` condition when your template's ask is far off
  the measurement. Opt-in **sizing profiles** then apply the measurement at
  pod-build time: `Binpack` (Guaranteed QoS, max workers per expensive node),
  `Throughput` (burst-friendly, no CPU limit), or `NodeShare` (a declared
  per-node share — the GPU bin-packing case, no history needed), with clamps,
  a confidence fallback, and GPUs never touched — see the
  [right-sizing guide](operations/worker-rightsizing.md). ARC has no sizing
  feedback loop.
- **Secure by default.** Pod Security Admission, default-deny network policies,
  credentials kept out of environment variables, and signed images with a
  Software Bill of Materials (SBOM) and SLSA provenance — reconciled, not opt-in.
  Sandboxed worker runtimes compose with all of it: **Kata Containers is
  validated** on a nested-virtualization node pool and is the default for GAG's
  own end-to-end CI, which builds a `kind` cluster inside an unprivileged worker
  pod. See [Running DinD workloads under Kata](operations/kata-dind-workloads.md).
- **The v2 API — the recommended shape for new tenants.** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span> A decomposed
  `actions-gateway.com` API ships *beside* `v1alpha1` and is the path
  new tenants should onboard on: the single-acquirer **runner-scale-set** acquisition
  protocol as the default (the same model ARC uses, no many-acquirers fan-out),
  reusable `RunnerTemplate` and cluster-wide `ClusterRunnerTemplate`, multiple scoped
  gateways per namespace, an optional standalone `EgressProxy`, per-gateway
  control-plane sizing (`agcResources`, optionally handed to a Vertical Pod Autoscaler
  with `agcAutoscaling`), and a `v1 → v2` migration tool. It has reached its first
  stability contract: **`v2beta1`** is the graduated, ScaleSet-only storage and hub
  version, `v2alpha1` stays served as the `gag-migrate` on-ramp, and the apiserver
  converts between them through a webhook the GMC hosts. The single-CR `v1alpha1`
  API, `v2alpha1`, and the classic acquisition protocol are all still fully served
  but **[deprecated](operations/v1alpha1-deprecation.md)**, and all three are removed
  at **`v2.0.0`**; that removal is the near-term work below. See the migration
  guide's [Why upgrade to v2](operations/migration-v1-to-v2.md#why-upgrade-to-v2)
  for the full list.
- **Day-2 operations.** Helm upgrade and rollback paths, a backup/restore and
  disaster-recovery runbook, and troubleshooting guides.
- **A `ResourceQuota` sizing guide.** Turn a tenant's runner shapes and
  concurrency ceilings into the quota numbers a platform admin sets, so the
  first install is a calculation rather than a guess:
  [sizing the platform-owned `ResourceQuota`](operations/resourcequota-sizing.md)
  covers what the quota actually counts (native sidecars and Kata `RuntimeClass`
  overhead both do), which keys are safe to constrain, and a worked
  Docker-in-Docker example.
- **An onboarding and migration path that is already written.** A
  [switching-from-ARC walkthrough](operations/migration-from-arc.md) that moves one
  runner group across with zero downtime, a
  [getting-started guide](getting-started.md) covering first-time GitHub App setup
  and credential rotation, a cluster [pre-flight check](operations/install.md)
  (`scripts/validate-cluster.sh`) that fails loudly on a network-policy-less CNI
  rather than silently voiding tenant isolation, an
  [air-gapped / private-registry install](operations/air-gapped-install.md), a
  [recorded demo](demo.md) of one real job on a local kind cluster, and an
  [interactive savings calculator](design/appendix-f-cost-model.md#f5-savings-calculator-this-system-vs-arc)
  for the cost argument.
- **Workload-identity credentials.** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">beta</span> Mint short-lived GitHub credentials through
  an external signer (`credentials.type: WorkloadIdentity`), so the GitHub App
  private key never enters the cluster. Available in the v2 API only —
  v1's flat credential shape has no equivalent.

See [Why GAG?](why-gag.md) for the capability-by-capability comparison against
Actions Runner Controller (ARC), and the [operations docs](operations/README.md)
for how to run each of the above.

## In progress / near-term

Work that is scoped and actively being built — adoption-enabling polish and the
last gaps an outside operator hits.

Nothing sits here today. Every capability on this roadmap has either shipped
(above) or is waiting on a gate rather than on engineering time (below); the
active backlog is bug-fix, measurement, and test work behind capability that
already exists. This section fills again when the next capability is scoped.

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
  gate clears. The deprecation window opened at v1.1.0, and `gag-migrate` already
  moves a tenant across without changing how jobs are acquired.
  Detail: the [deprecation and removal notice](operations/v1alpha1-deprecation.md).

- **CI for untrusted pull requests on Kata workers.** <!-- q:Q408 --> Kata micro-VM workers are
  validated today, but only for *trusted* CI: the isolation bounds the guest
  kernel, while the runner's egress stays permissive because its jobs pull from
  CDN-fronted public registries that no CIDR allowlist can pin and that GKE
  Dataplane V2 cannot enforce by fully-qualified domain name. The path to
  running an external contributor's pull request safely is two pieces on top of
  what already ships: an in-cluster pull-through registry mirror, so a worker
  needs no direct registry egress at all, and a tight egress policy scoped to
  that mirror, GitHub, and DNS. Designed, not built. If you run public OSS CI
  and want this, say so on an issue: it is the trigger that schedules the work.
- **Controller horizontal scaling / high availability.** <!-- q:Q169 --> The per-tenant
  controller is single-replica by design today; distributed session state would
  enable multi-replica HA if a single controller becomes a measured bottleneck.
- **Richer egress proxy.** <!-- q:Q19 --> Optional allow-listing, rate-limiting, audit logging,
  and per-runner-group proxy pools.
- **Bring-your-own proxy infrastructure.** <!-- q:Q174 --> Supply your own proxy autoscaler
  (KEDA / VPA / custom HPA) or TLS certificate instead of the managed defaults.
- **Cross-namespace proxy sharing.** <!-- q:Q166 --> Share an egress proxy pool across namespaces
  with explicit consent (same-namespace sharing already works).
- **First-class GPU runner support.** <!-- q:Q216 --> Priority tiers and the `NodeShare` sizing
  profile already carry the GPU cases, but GPU Operator / Node Feature Discovery
  awareness, and `nodeSelector` / toleration / `RuntimeClass` conventions that
  make a GPU runner set feel native, wait on a concrete GPU workload to design
  against.
- **A worker cache backend.** <!-- q:Q215 --> Workers are storage-less by design, so
  `actions/cache` and Docker layer caching have no home. Adding an optional
  PVC or object-store cache needs a security review of cross-job cache
  isolation first: a shared cache between tenants' jobs is an obvious
  exfiltration path.
- **A warm worker pool.** <!-- q:Q268 --> An opt-in pool of idle pods per runner set, for teams
  that still hit pod-schedule latency after image pre-pull and caching.
- **gVisor validation.** <!-- q:Q15 --> The `runtimeClassName` path is validated end-to-end
  with Kata; gVisor is documented but unproven on a real cluster. It waits on an
  operator who wants lightweight syscall filtering for compute-only,
  non-Docker-in-Docker jobs, since Kata already covers the DinD case.
- **SPIFFE / SPIRE workload identity.** <!-- q:Q214 --> A keyless, SPIRE-backed signer slots
  behind the existing signer interface alongside the deferred cloud-KMS
  providers, for operators who want no GitHub App private key anywhere.
- **An Operator Lifecycle Manager bundle.** <!-- q:Q217 --> Helm-only is the deliberate install
  stance; an OperatorHub catalog entry waits on OpenShift demand.
- **A published benchmark and case study.** <!-- q:Q198 --> Real GitHub-at-scale numbers behind
  the cost model, which needs a funded scale run rather than a local cluster.

## How priorities are set

GAG's success metric is **external operators running it and telling us what
breaks** — not stars or downloads. That feedback drives the ordering above far
more than any internal plan. If something here is in your way, or missing
entirely, [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
— it's the fastest way to move it up.
