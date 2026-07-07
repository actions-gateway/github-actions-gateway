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

Everything here ships in the released chart and is covered by the design and
operations docs.

- **One resource per tenant.** A single `ActionsGateway` custom resource
  provisions an isolated gateway — controller, egress proxy pool, role-based
  access control (RBAC), and network policies — inside the platform-owned quota.
- **Automatic recovery for blocked and evicted jobs.** A quota-blocked or
  evicted job has its GitHub lock fast-cancelled and is re-queued, with a
  per-job retry budget — no manual rerun.
- **Priority tiers per runner group.** Reserve a guaranteed floor of slots for
  expensive runner types so cheap CPU jobs can't starve critical GPU work.
- **Worker scale-up rate limiting (opt-in).** An optional per-runner-group token
  bucket (`scaleUp.maxPerSecond`/`burst`) that caps how *fast* worker pods start —
  distinct from the count ceiling — to smooth cold-start stampedes on shared
  egress (NAT / firewall / VPN) when many jobs land at once. Off by default; ARC
  exposes only a `maxRunners` count cap, not a creation-rate control.
- **Scale-to-zero workers with low listener overhead.** Worker pods exist only
  while a job runs; listeners are goroutines (~60 KiB each) in one shared pod
  rather than a .NET pod per runner group.
- **Per-tenant isolated egress IPs.** A dedicated proxy pool per tenant gives
  each team its own GitHub egress IPs to allow-list, with a contained blast
  radius. <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">alpha</span> In the v2 API the proxy is a standalone,
  optionally shared `EgressProxy` (or omitted for direct egress), with a
  DNS-aware (FQDN) egress-policy mode on Cilium/Calico.
- **Observability, per tenant and fleet-wide.** Prometheus metrics scoped per
  tenant and runner group, plus ready-to-apply Grafana dashboards and alerts as
  code, and a cross-tenant rollup for platform admins.
- **Secure by default.** Pod Security Admission, default-deny network policies,
  credentials kept out of environment variables, and signed images with a
  Software Bill of Materials (SBOM) and SLSA provenance — reconciled, not opt-in.
- **The v2 API — the recommended shape for new tenants.** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">alpha</span> A decomposed
  `v2alpha1` (`actions-gateway.com`) API ships *beside* `v1alpha1` and is the path
  new tenants should onboard on: the single-acquirer **runner-scale-set** acquisition
  protocol as the default (the same model ARC uses, no many-acquirers fan-out),
  reusable `RunnerTemplate` and cluster-wide `ClusterRunnerTemplate`, multiple scoped
  gateways per namespace, an optional standalone `EgressProxy`, per-gateway
  control-plane sizing (`agcResources`), and a `v1 → v2` migration tool. The
  single-CR `v1alpha1` API (and its classic acquisition protocol) is still fully served
  but **[deprecated](operations/v1alpha1-deprecation.md)** — it is removed on a
  schedule tied to v2 reaching beta, not on an adoption count. See the migration
  guide's [Why upgrade to v2](operations/migration-v1-to-v2.md#why-upgrade-to-v2)
  for the full list. Graduation toward `v2beta1` is near-term work (below).
- **Day-2 operations.** Helm upgrade and rollback paths, a backup/restore and
  disaster-recovery runbook, and troubleshooting guides.
- **Workload-identity credentials.** <span class="gag-v2-badge">v2</span> <span class="gag-maturity-badge">alpha</span> Mint short-lived GitHub credentials through
  an external signer (`credentials.type: WorkloadIdentity`), so the GitHub App
  private key never enters the cluster. Available in the `v2alpha1` API only —
  v1's flat credential shape has no equivalent.

See [Why GAG?](why-gag.md) for the capability-by-capability comparison against
Actions Runner Controller (ARC), and the [operations docs](operations/README.md)
for how to run each of the above.

## In progress / near-term

Work that is scoped and actively being built — adoption-enabling polish and the
last gaps an outside operator hits.

- **Coming-from-ARC migration guide.** A "switching from ARC" walkthrough mapping
  scale sets to runner groups and runner labels, with the gotchas — the single
  biggest switching-friction blocker today.
- **Quantified cost story.** Real per-job and dollar figures plus an interactive
  savings calculator versus ARC, replacing today's qualitative "lower cost"
  framing.
- **End-to-end demo.** A short screencast of a kind-cluster deploy showing a job
  flow from GitHub to worker pod and back.
- **Onboarding polish.** A first-time GitHub App setup walkthrough and a
  `ResourceQuota` sizing helper so the first install lands cleanly.
- **Install pre-flight check.** Validate the cluster (network-policy-enforcing
  CNI, Kubernetes version, cert-manager, metrics-server) before install, so a
  misconfigured cluster fails loudly instead of silently voiding tenant
  isolation.
- **Air-gapped / private-registry install.** Image-pull-secret support and a
  mirror-the-images guide for egress-restricted enterprises.
- **API graduation.** Promote the v2 API from `v2alpha1` toward `v2beta1` with a
  conversion webhook and storage migration. The `v2beta1` cut is **ScaleSet-only** —
  it retires the deprecated classic acquisition protocol and the `v1alpha1` API
  together, on a named release announced at least one release ahead.

## Exploring / longer-term

Directions we expect to pursue as demand and validated evidence accumulate. These
are intentionally uncommitted — each waits on a real operator need or a measured
limit before it becomes scheduled work.

- **Controller horizontal scaling / high availability.** The per-tenant
  controller is single-replica by design today; distributed session state would
  enable multi-replica HA if a single controller becomes a measured bottleneck.
- **Richer egress proxy.** Optional allow-listing, rate-limiting, audit logging,
  and per-runner-group proxy pools.
- **Bring-your-own proxy infrastructure.** Supply your own proxy autoscaler
  (KEDA / VPA / custom HPA) or TLS certificate instead of the managed defaults.
- **Cross-namespace proxy sharing.** Share an egress proxy pool across namespaces
  with explicit consent (same-namespace sharing already works).

## How priorities are set

GAG's success metric is **external operators running it and telling us what
breaks** — not stars or downloads. That feedback drives the ordering above far
more than any internal plan. If something here is in your way, or missing
entirely, [open an issue](https://github.com/actions-gateway/github-actions-gateway/issues)
— it's the fastest way to move it up.
