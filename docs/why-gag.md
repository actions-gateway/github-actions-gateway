---
hide:
  - navigation
  - toc
---

<div class="gag-vs-hero" markdown>
<div class="gag-vs-hero__lead" markdown>

<p class="gag-eyebrow">Comparison · ARC alternative</p>

# Why GitHub Actions Gateway over ARC?

<p class="gag-vs-hero__lede">Actions Runner Controller (ARC) scale-set mode struggles with one job: running <strong>many runner groups, for many tenants, in one shared cluster — cost-effectively, with each tenant safely capped by its own <code>ResourceQuota</code></strong>. GAG was built for exactly that, without giving up the self-service that makes a shared cluster worth running.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
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
    - cleared and rerun by hand — so teams avoid enforcing quotas

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

    - namespace, quota, RBAC, scale sets, NetworkPolicies, egress
    - per-team setup; every later change is a ticket

</div>
</div>

## What changes with GAG

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">600&nbsp;KiB</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener memory for 10 runner groups</strong> — one shared pod, versus ~2.5 GiB across 10 on ARC</span>
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
    <span class="gag-stat__label"><strong class="gag-stat__lead">Resource a tenant declares</strong> — controller, proxy pool, RBAC, and network policies, provisioned to run within the platform-owned quota</span>
  </div>
</div>

## GAG vs ARC (scale-set mode)

| Capability | ARC (scale-set mode) | GitHub Actions Gateway |
| --- | --- | --- |
| Ephemeral, single-use runner pods | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Custom runner pod template & image | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Workers scale to zero between jobs | :material-check-circle:{ .gag-yes } yes, with `minRunners: 0` | :material-check-circle:{ .gag-yes } yes, by default |
| Safe under a per-tenant `ResourceQuota` | :material-close-circle:{ .gag-no } quota-blocked jobs stall; manual cleanup + rerun | :material-check-circle:{ .gag-yes } [auto lock-cancel + rerun, per-job budget](design/04-operational-flows.md) |
| Guaranteed floor for critical runner types | :material-close-circle:{ .gag-no } no per-quota primitive | :material-check-circle:{ .gag-yes } [priority tiers per runner group](design/02-architecture.md) |
| Per-tenant dedicated egress IPs | :material-close-circle:{ .gag-no } shared cluster egress | :material-check-circle:{ .gag-yes } [per-tenant HTTPS CONNECT proxy pool](design/network-architecture.md) |
| Listener memory, 10 runner groups at rest | :material-close-circle:{ .gag-no } ~2.5 GiB across 10 pods | :material-check-circle:{ .gag-yes } ~600 KiB in 1 shared pod |
| Per-tenant utilization metrics | :material-close-circle:{ .gag-no } scale-set metrics, not tenant-scoped | :material-check-circle:{ .gag-yes } [Prometheus, scoped per tenant + runner group](operations/observability.md) |

Every capability above is driven by the single `ActionsGateway` resource shown
below.

For limits and Service Level Objectives behind these claims, see
[Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md); for
the utilization-and-cost argument, [Appendix F — Cost model](design/appendix-f-cost-model.md).

## Secure by default

Built for shared clusters running other teams' code: the multi-tenant hardening
ships as reconciled defaults, not a post-install project.

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-lock:{ .lg .middle } __Risk reduction__

    ---

    Untrusted job code is boxed in by default:

    - `baseline` Pod Security Admission per namespace
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
    - Signed images + SBOM + SLSA provenance

</div>
</div>

For the full threat model, per-profile controls, and the abuse-response
playbooks, see [Security](design/05-security.md) and
[Security operations](operations/security-operations.md).

## One resource, a whole gateway

A tenant declares what they want in a single namespace-scoped resource. The
platform marks the namespace and sets its `ResourceQuota` once; from there the
Gateway Manager Controller (GMC) provisions the controller, proxy pool, RBAC, and
network policies to match — all operating within that platform-owned quota, which
the GMC never creates or mutates. No per-tenant cluster-admin involvement after
the initial GMC install.

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
    - name: gpu-runners
      runnerLabels: ["self-hosted", "gpu"]
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
    - name: cpu-runners
      runnerLabels: ["self-hosted", "linux"]
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
3.  The per-tenant egress proxy pool is HPA-managed between these bounds; all
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

Ready to try it? Follow the [getting-started guide](getting-started.md).
