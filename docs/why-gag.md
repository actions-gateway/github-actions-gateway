---
hide:
  - navigation
  - toc
---

<div class="gag-vs-hero" markdown>
<div class="gag-vs-hero__lead" markdown>

<p class="gag-eyebrow">Comparison · ARC alternative</p>

# Why GitHub Actions Gateway over ARC?

<p class="gag-vs-hero__lede">Actions Runner Controller (ARC) scale-set mode struggles with one job: running <strong>many runner sets, for many tenants, in one shared cluster — cost-effectively, with each tenant safely capped by its own <code>ResourceQuota</code></strong>. GAG was built for exactly that, without giving up the self-service that makes a shared cluster worth running.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Migrating from ARC](operations/migration-from-arc.md){ .md-button }
[See the architecture](design/02-architecture.md){ .md-button }

</div>
<div class="gag-vs-hero__proof">
  <p class="gag-vs-hero__proof-cap">When a worker is evicted or blocked by a full <code>ResourceQuota</code></p>
  <div class="gag-vs-row gag-vs-row--arc"><span class="gag-vs-row__tag">ARC</span><span class="gag-vs-row__text">the runner is marked <code>Failed</code> and the job sits in GitHub's queue until someone reruns it by hand</span></div>
  <div class="gag-vs-row gag-vs-row--gag"><span class="gag-vs-row__tag">GAG</span><span class="gag-vs-row__text">the job lock is fast-cancelled and the job re-queued automatically — it runs as soon as capacity frees up, no manual rerun</span></div>
</div>
</div>

## The problem ARC leaves you with

The failures compound, but they all trace back to one root: ARC's poor fit with
`ResourceQuota` makes per-tenant quotas unsafe — and unsafe quotas are what block
letting tenants run their own runners.

<div class="gag-pillars gag-pillars--problem gag-cols-2" markdown>
<div class="grid cards" markdown>

-   :material-lock-alert:{ .lg .middle } __`ResourceQuota` is unsafe__

    ---

    A quota-blocked or evicted job can't recover on its own:

    - ARC retries the same runner ([30 s loop](https://github.com/actions/actions-runner-controller/pull/4305)), then marks it `Failed`
    - the job sits in GitHub's queue up to its 24-hour timeout
    - cleared and rerun by hand ([#4155](https://github.com/actions/actions-runner-controller/issues/4155), [#4203](https://github.com/actions/actions-runner-controller/issues/4203)) — so teams avoid enforcing quotas

-   :material-trending-down:{ .lg .middle } __Critical jobs starve__

    ---

    No way to reserve capacity for expensive runners:

    - each `AutoscalingRunnerSet` only caps itself with `maxRunners`
    - no primitive for "GPU always keeps N slots"
    - cheap CPU pods exhaust the quota; big tests stall

-   :material-memory:{ .lg .middle } __Listener memory piles up__

    ---

    One .NET listener pod per scale set, running 24/7:

    - ~256 MiB resident + a cluster IP each
    - held alive just to long-poll GitHub
    - 10 scale sets ≈ 2.5 GiB before a job runs

-   :material-ticket-confirmation:{ .lg .middle } __Platform team is the bottleneck__

    ---

    Every tenant is a manual checklist:

    - namespace, quota, Role-Based Access Control (RBAC), scale sets, NetworkPolicies, egress
    - per-team setup; every later change is a ticket

</div>
</div>

## What changes with GAG

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">600&nbsp;KiB</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener memory for 10 runner sets</strong> — one shared pod, versus ~2.5 GiB across 10 on ARC</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">0</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Idle GPU pods between jobs</strong> — workers exist only while a job runs, deleted on completion</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">Auto</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Recovery for quota-blocked and evicted jobs</strong> — the lock is fast-cancelled and the job re-queued, no manual rerun</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">1</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Namespace a tenant self-serves in</strong> — declare your gateway and runner sets; the GMC provisions the controller, proxy pool, RBAC, and network policies to run within the platform-owned quota, no per-tenant cluster-admin</span>
  </div>
</div>

## GAG vs ARC (scale-set mode)

GAG acquires jobs with the **same runner-scale-set protocol ARC uses** — a single
acquirer per runner set, capacity-gated assignment, no many-acquirers fan-out — and
it is the **shipped default** in the v2 API. So the comparison below is
capability-for-capability against ARC's *own* model: every GAG row is **additive**,
not a different-architecture trade-off. The difference is what surrounds the shared
acquisition core — quota safety, priority tiers, per-tenant egress, and control-plane
footprint.

| Capability | ARC (scale-set mode) | GitHub Actions Gateway |
| --- | --- | --- |
| Runner-scale-set acquisition (single-acquirer, no fan-out) | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes, by default <span class="gag-v2-badge">v2</span> |
| Ephemeral, single-use runner pods | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Custom runner pod template & image | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Workers scale to zero between jobs | :material-check-circle:{ .gag-yes } yes, with `minRunners: 0` | :material-check-circle:{ .gag-yes } yes, by default |
| Safe under a per-tenant `ResourceQuota` | :material-close-circle:{ .gag-no } quota-blocked jobs stall; manual cleanup + rerun | :material-check-circle:{ .gag-yes } [auto lock-cancel + re-queue](design/04-operational-flows.md) |
| Guaranteed floor for critical runner types | :material-close-circle:{ .gag-no } no per-quota primitive | :material-check-circle:{ .gag-yes } [priority tiers per runner set](design/02-architecture.md) |
| Throttle the *rate* new workers start (anti-stampede) | :material-close-circle:{ .gag-no } only `maxRunners` caps the count — a burst starts all at once | :material-check-circle:{ .gag-yes } opt-in [`scaleUp` creation-rate limit per set](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)<br><span class="gag-cont">for shared-egress onset (NAT / firewall / VPN)</span> |
| Per-tenant dedicated egress IPs | :material-close-circle:{ .gag-no } shared cluster egress | :material-check-circle:{ .gag-yes } [per-tenant proxy pool](design/network-architecture.md)<br><span class="gag-cont"><span class="gag-v2-badge">v2</span> proxy optional</span> |
| Listener memory, 10 runner sets at rest | :material-close-circle:{ .gag-no } ~2.5 GiB across 10 pods | :material-check-circle:{ .gag-yes } ~600 KiB in 1 shared pod |
| Per-tenant utilization metrics | :material-close-circle:{ .gag-no } scale-set metrics, not tenant-scoped | :material-check-circle:{ .gag-yes } [Prometheus per tenant + group](operations/observability.md)<br><span class="gag-cont">job counts in `kubectl get`; ready-to-apply [tenant dashboard + alerts as code](operations/observability.md#tenant-dashboard)</span> |
| Cross-tenant fleet health view (platform admin) | :material-close-circle:{ .gag-no } controller + per-scale-set metrics, aggregated by hand; no bundled dashboard | :material-check-circle:{ .gag-yes } [single-pane GMC fleet rollups](operations/observability.md#full-metrics-reference)<br><span class="gag-cont">degraded / egress-stale / quota per gateway, + a [platform dashboard](operations/observability.md#platform-dashboard)</span> |
| Multiple gateways per namespace | :material-check-circle:{ .gag-yes } multiple `AutoscalingRunnerSet`s | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [multiple scoped gateways per namespace](operations/migration-v1-to-v2.md) |
| Reusable runner pod templates | :material-close-circle:{ .gag-no } template inlined per `AutoscalingRunnerSet` | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> shared [`RunnerTemplate`](operations/migration-v1-to-v2.md)<br><span class="gag-cont">cluster-wide [`ClusterRunnerTemplate`](operations/migration-v1-to-v2.md)</span> |

Every capability above is available today.

!!! note "Onboarding: start on v2"

    New tenants should onboard on the **recommended v2 API**
    (`actions-gateway.com/v2alpha1` — a decomposed `ActionsGateway` + `RunnerSet` +
    `RunnerTemplate`, with an optional standalone `EgressProxy`); the rows marked
    <span class="gag-v2-badge">v2</span> are v2-only. The single-CR `v1alpha1` shape
    shown below is still fully served but
    **[deprecated](operations/v1alpha1-deprecation.md)** — see the
    [v1 → v2 migration guide](operations/migration-v1-to-v2.md) and the
    [getting-started walkthrough](getting-started.md) for the v2 object set.

!!! info "The numbers behind these claims"

    For limits and Service Level Objectives, see
    [Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md); for
    the utilization-and-cost argument,
    [Appendix F — Cost model](design/appendix-f-cost-model.md).

!!! warning "Where GAG is behind ARC"

    It's **maturity, not capability.** ARC is GA and widely deployed; GAG's
    recommended v2 API is still alpha and rides a Public-Preview runner-scale-set
    protocol. That is precisely why the v1 → v2 migration is handled on a committed,
    documented schedule with a working
    [`gag-migrate`](operations/migration-v1-to-v2.md) tool — the discipline is the
    "won't strand you" signal while the track record accumulates.

## Secure by default

Built for shared clusters running other teams' code: the multi-tenant hardening
ships as reconciled defaults, not a post-install project.

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-lock:{ .lg .middle } __Risk reduction__

    ---

    Untrusted job code is boxed in by default:

    - `baseline` Pod Security Admission (PSA) per namespace
    - Default-deny network — DNS + own proxy only
    - App keys read-only; never in env, never cached
    - Controller writes confined to tenant namespaces

-   :material-clipboard-check:{ .lg .middle } __Lower operational cost__

    ---

    What you'd hand-build around ARC, reconciled from one CR:

    - NetworkPolicies · PSA · RBAC · egress
    - No Kyverno/OPA required — in-tree PodSecurity
    - Kept in sync as tenants come and go

-   :material-check-decagram:{ .lg .middle } __Ready out of the box__

    ---

    Secure by default; looser is an explicit opt-in:

    - Default-deny ingress, cluster-only DNS
    - Per-tenant egress IPs, mutual-TLS metrics
    - Signed images + Software Bill of Materials (SBOM) + Supply-chain Levels for Software Artifacts (SLSA) provenance

</div>
</div>

For the full threat model, per-profile controls, and the abuse-response
playbooks, see [Security](design/05-security.md) and
[Security operations](operations/security-operations.md).

## One declaration, a whole gateway

A tenant declares what they want in namespace-scoped resources. The platform marks
the namespace and sets its `ResourceQuota` once; from there the Gateway Manager
Controller (GMC) provisions the controller, proxy pool, RBAC, and network policies
to match — all operating within that platform-owned quota, which the GMC never
creates or mutates. No per-tenant cluster-admin involvement after the initial GMC
install.

The **recommended v2 API** decomposes this into small reusable kinds (a shared
`RunnerTemplate`, a `RunnerSet` per runner type, an optional standalone
`EgressProxy`) — see the [getting-started walkthrough](getting-started.md). The
legacy `v1alpha1` shape below expresses the same gateway in one CR:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  gitHubAppRef:
    name: my-github-app          # (1)!
  gitHubURL: https://github.com/team-a-org
  securityProfile: baseline      # (2)!
  proxy:
    minReplicas: 2               # (3)!
    maxReplicas: 10
  # No namespaceQuota field: the ResourceQuota is platform-owned (4)!
  runnerGroups:
    - runnerLabels: ["gpu", "self-hosted"]   # first label → derived RunnerGroup name
      maxListeners: 10
      priorityTiers:             # (5)!
        - priorityClassName: runner-critical
          threshold: 5
        - priorityClassName: runner-standard
          threshold: 20
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                limits:
                  nvidia.com/gpu: "1"
    - runnerLabels: ["linux", "self-hosted"]   # distinct first label ⇒ distinct name
      maxWorkers: 30
      podTemplate:
        spec:
          containers:
            - name: runner
```

1.  References a `Secret` in the same namespace holding the GitHub App `appId`,
    `installationId`, and `privateKey`. The GMC watches the reference name, not
    the Secret contents — see [credential rotation](getting-started.md#rotating-github-app-credentials).
2.  Selects the Pod Security Admission level the GMC stamps on the namespace.
    Defaults to `baseline`; use `restricted` for stricter isolation or
    `privileged` only for workloads like docker-in-docker. See
    [Security](design/05-security.md).
3.  The per-tenant egress proxy pool is Horizontal Pod Autoscaler (HPA)-managed between these bounds; all
    GitHub traffic exits through it on dedicated IPs.
4.  The single `ResourceQuota` every runner group shares is **platform-owned** —
    the platform admin sets it on the namespace, not on this CR, so it is a real
    cap the tenant cannot raise. Priority tiers decide who wins when it is
    contended.
5.  The first 5 GPU pods get the higher-priority `PriorityClass`; the next tier
    bursts opportunistically; the final threshold caps total concurrency. The
    `priorityClassName` values must be on the platform's allowlist (the GMC
    `--allowed-priority-classes` flag), and whether a tier preempts is set on the
    platform-owned `PriorityClass` object — a tenant cannot name a class that
    evicts other tenants' pods.

Ready to try it? Follow the [getting-started guide](getting-started.md). Already
running ARC? The [Migrating from ARC guide](operations/migration-from-arc.md) maps
every concept above onto GAG and walks one runner group across with zero downtime.
