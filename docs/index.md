---
hide:
  - navigation
  - toc
---

<div class="gag-hero" markdown>

<p class="gag-eyebrow">Multi-tenant runner platform for Kubernetes</p>

# Self-hosted GitHub Actions runners with zero idle compute

<p class="gag-tagline">An Actions Runner Controller (ARC) alternative for multi-tenant Kubernetes. Free up GPU nodes the moment a job finishes, keep critical jobs scheduling even on a full cluster, and let tenants self-manage runners under safe per-tenant quotas.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Why GAG?](why-gag.md){ .md-button }
[View on GitHub](https://github.com/actions-gateway/github-actions-gateway){ .md-button }

<p class="gag-reassure" markdown="span">:material-check-circle: Drop-in for your existing setup — jobs target the same runner labels, so nothing in your `.github/workflows` changes.</p>

</div>

```sh
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.0.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy>
```

## What GAG gives you

Most of these ladder up to one outcome — **lower cost**: no idle GPUs, fewer
always-on resources, and guaranteed throughput instead of blocked critical jobs.

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-check:{ .lg .middle } __Tenant self-service under quotas__

    ---

    When a worker is evicted — preempted, OOM-killed, or blocked by a full
    `ResourceQuota` — GAG fast-cancels the GitHub job lock and reruns it
    automatically. That's what makes per-tenant quotas safe to enforce: the
    platform team caps each tenant, and tenants self-manage their runners with no
    manual reruns.

-   :material-layers-triple:{ .lg .middle } __No blocked critical jobs__

    ---

    Reserve at least N slots for each runner type, so a flood of small fast tests
    can't starve the big expensive ones. Every PR's full test battery finishes —
    even on a full cluster — instead of the GPU and e2e jobs sitting pending.

-   :material-arrow-collapse-down:{ .lg .middle } __No idle GPUs__

    ---

    Worker pods exist only while a job runs and are deleted on completion, so GPU
    nodes return to the scheduler the moment a job finishes — no idle runners
    pinned to mask cold starts. (ARC can scale to zero too; GAG makes it the
    default.)

-   :material-ip-network:{ .lg .middle } __Isolated egress IPs__

    ---

    Each tenant's GitHub traffic exits through its own proxy pool, so you can
    allow-list just your runners on GitHub EMU — no cluster-wide allow-list or NAT
    gateway needed. A tenant that gets throttled or flagged doesn't take the
    others down with it.

-   :material-feather:{ .lg .middle } __Lower listener overhead__

    ---

    Every runner group's listener is a ~60 KiB goroutine in one shared pod, not a
    ~256 MiB pod per scale set — roughly 600 KiB versus 2.5 GiB across ten groups.
    It adds up when memory is expensive.

-   :material-chart-line:{ .lg .middle } __Per-tenant utilization metrics__

    ---

    Prometheus metrics scoped per tenant and runner group, so teams can see their
    own GPU utilization and make the case for quota changes — without needing
    cluster-wide visibility.

</div>
</div>

## How it fits together

A four-tier system: one cluster-scoped manager provisions a fully isolated
gateway per tenant from each `ActionsGateway` resource.

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

Read the [architecture overview](design/02-architecture.md) for the full
breakdown, or jump to [why GAG over ARC](why-gag.md).
