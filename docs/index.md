---
hide:
  - navigation
  - toc
---

<div class="gag-hero" markdown>

<div class="gag-hero__intro" markdown>

<img class="gag-hero__logo" src="assets/logo.svg" alt="GitHub Actions Gateway logomark" width="132" height="132">

<div class="gag-hero__headline" markdown>

<p class="gag-eyebrow">Multi-tenant runner platform for Kubernetes</p>

# Self-hosted GitHub Actions with zero idle compute

</div>

</div>

<p class="gag-tagline">An Actions Runner Controller (ARC) alternative for multi-tenant Kubernetes. Free up GPU nodes the moment a job finishes, keep critical jobs scheduling even on a full cluster, and let tenants self-manage runners under safe per-tenant quotas.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Why GAG?](why-gag.md){ .md-button }
[View on GitHub](https://github.com/actions-gateway/github-actions-gateway){ .md-button }

<p class="gag-reassure" markdown="span">:material-check-circle: Drop-in for your existing setup — jobs target the same runner labels, so nothing in your `.github/workflows` changes.</p>

</div>

<div class="gag-install" markdown>

```sh
helm install gag \
  oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.0.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

</div>

<div class="gag-section-intro" markdown>

## What GAG gives you

Most of these ladder up to one outcome — **lower cost**: no idle GPUs, fewer always-on resources, and guaranteed throughput instead of blocked critical jobs.

</div>

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-check:{ .lg .middle } __Tenant self-service under quotas__

    ---

    Per-tenant quotas you can safely enforce:

    - Platform caps each tenant's `ResourceQuota`
    - Evicted or quota-blocked jobs auto-recover
    - Auto re-queued — zero manual reruns
    - Tenants self-manage their own runners

-   :material-layers-triple:{ .lg .middle } __No blocked critical jobs__

    ---

    Reserve capacity for expensive runners:

    - Reserve N slots per runner type
    - Fast CPU tests can't crowd out GPU or e2e
    - Every PR's full battery still finishes

-   :material-arrow-collapse-down:{ .lg .middle } __No idle GPUs__

    ---

    Worker pods exist only while a job runs:

    - Created on acquire, deleted on completion
    - GPU nodes freed the moment a job ends
    - No idle runners masking cold starts
    - Scale-to-zero by default (ARC's opt-in)

-   :material-ip-network:{ .lg .middle } __Isolated egress IPs__

    ---

    Each tenant exits via its own proxy pool:

    - Allow-list just your runners on GitHub EMU
    - No cluster-wide allow-list or NAT gateway
    - One flagged tenant can't take others down

-   :material-feather:{ .lg .middle } __Lower listener overhead__

    ---

    Listeners are goroutines, not pods:

    - ~60 KiB per runner group, one shared pod
    - vs ARC's ~256 MiB pod per scale set
    - ~600 KiB vs ~2.5 GiB across ten groups

-   :material-chart-line:{ .lg .middle } __Per-tenant utilization metrics__

    ---

    Per-tenant, per-group Prometheus metrics:

    - Teams see their own GPU utilization
    - Data-backed case for quota changes
    - No cluster-wide visibility required

</div>
</div>

<div class="gag-section-intro" markdown>

## How it fits together

A four-tier system: a cluster-scoped manager gives each tenant an isolated gateway from its `ActionsGateway`.

</div>

<div class="gag-flow">
  <div class="gag-flow__node gag-flow__node--input">
    <span class="gag-flow__tier gag-flow__tier--input">Tenant input</span>
    <span class="gag-flow__title">ActionsGateway resource</span>
    <span class="gag-flow__sub">one per tenant · namespace-scoped</span>
  </div>
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; watched by</div>
  <div class="gag-flow__node gag-flow__node--lead">
    <span class="gag-flow__tier">Tier 1</span>
    <span class="gag-flow__title">Gateway Manager Controller</span>
    <span class="gag-flow__sub">cluster-scoped · installed once</span>
  </div>
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; provisions the AGC + proxy per tenant</div>
  <div class="gag-flow__row">
    <div class="gag-flow__node">
      <span class="gag-flow__tier">Tier 2</span>
      <span class="gag-flow__title">Actions Gateway Controller</span>
      <span class="gag-flow__sub">goroutine multiplexer</span>
    </div>
    <div class="gag-flow__node">
      <span class="gag-flow__tier">Tier 3</span>
      <span class="gag-flow__title">Egress proxy pool</span>
      <span class="gag-flow__sub">per-tenant egress IPs</span>
    </div>
  </div>
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; AGC spawns one pod per job</div>
  <div class="gag-flow__node">
    <span class="gag-flow__tier">Tier 4</span>
    <span class="gag-flow__title">Ephemeral worker pods</span>
    <span class="gag-flow__sub">one per job · GC'd on completion</span>
  </div>
</div>

<p class="gag-flow__caption" markdown="span">Read the [architecture overview](design/02-architecture.md) for the full breakdown, or jump to [why GAG over ARC](why-gag.md).</p>
