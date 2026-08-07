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

# <span class="gag-hero__phrase">Self-hosted GitHub Actions</span> with <span class="gag-hero__phrase">zero idle compute</span>

</div>

</div>

<p class="gag-tagline">GitHub Actions Gateway (GAG) is an Actions Runner Controller (ARC) alternative for shared, multi-tenant clusters. <strong>Nothing idles between jobs. Jobs the cluster disrupts re-run themselves. Quotas are safe to enforce</strong>, so several teams can share expensive nodes without the platform team becoming a ticket queue.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Watch the demo](demo.md){ .md-button }
[Why GAG?](why-gag.md){ .md-button }
[View on GitHub](https://github.com/actions-gateway/github-actions-gateway){ .md-button }

<p class="gag-reassure" markdown="span">:material-check-circle: Drop-in for your existing setup: jobs target the same runner labels, so nothing in your `.github/workflows` changes.</p>

</div>

<div class="gag-install" markdown>

```sh
helm install gag \
  oci://ghcr.io/actions-gateway/charts/actions-gateway \
  --version 1.3.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>

# v2 API (recommended): apply the signed, pre-rendered CRDs
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/v1.3.0/actions-gateway-crds-v2.yaml
```

</div>

<div class="gag-section-intro" markdown>

## What GAG gives you

These ladder up one way: **safe quotas and self-healing disruption make shared capacity usable**, which is what lets you bin-pack expensive nodes and run on preemptible capacity. [Estimate your savings vs ARC →](design/appendix-f-cost-model.md#f5-savings-calculator-this-system-vs-arc) · [See every feature →](features.md)

</div>

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">1&nbsp;pod</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener footprint for 10 runner sets</strong>: every listener is a goroutine in one shared pod, against 10 always-on pods on ARC</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">15&ndash;26&nbsp;s</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Measured recovery from a preemption or drain</strong>: the run concludes and re-runs itself, with no manual rerun and no ticket</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">0</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Idle GPU pods between jobs</strong>: workers exist only while a job runs, deleted on completion</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">20</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Alert rules shipped as code</strong>, with a tenant dashboard and a platform dashboard beside them</span>
  </div>
</div>

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-check:{ .lg .middle } __Tenant self-service under quotas__

    ---

    Quotas you can safely enforce:

    - Platform-owned quota cap
    - Blocked jobs auto-recover
    - Zero manual reruns
    - Self-serve `ActionsGateway`, no ticket per change

-   :material-layers-triple:{ .lg .middle } __No blocked critical jobs__

    ---

    Reserve capacity for key runners:

    - Reserve N slots per runner type
    - CPU tests can't starve GPU jobs
    - Critical tests always schedule

-   :material-arrow-collapse-down:{ .lg .middle } __No idle GPUs__

    ---

    Pods live only for the job:

    - Created on acquire
    - Deleted on completion
    - GPU freed the instant a job ends
    - Scale-to-zero by default

-   :material-ip-network:{ .lg .middle } __Isolated egress IPs__

    ---

    Each tenant's own proxy pool:

    - Allow-list runners on Enterprise Managed Users
    - Live-validated on GKE, 2026-07-13
    - [Stable IP needs a gateway or cloud NAT under it](design/network-architecture.md#per-tenant-egress-ip-the-source-ip-mechanism)
    - v2: proxy optional

-   :material-feather:{ .lg .middle } __Lower listener overhead__

    ---

    Listeners are goroutines, not pods:

    - ~12 KiB per listener session
    - One shared pod per tenant
    - 1 pod vs 10 always-on pods for ten groups

-   :material-chart-line:{ .lg .middle } __Per-tenant observability__

    ---

    Scoped visibility, no cluster access:

    - Prometheus per tenant + group
    - Grafana dashboards + alerts, as code
    - Job counts in `kubectl get`
    - K8s Events on job transitions
    - Cross-tenant fleet rollups for platform admins

-   :material-file-document-multiple:{ .lg .middle } __Shared runner templates__ <span class="gag-v2-badge">v2</span>

    ---

    Define once, reference by name:

    - `RunnerTemplate` per many sets
    - Platform `ClusterRunnerTemplate`
    - Identical templates collapse
    - Migrate v1→v2 with `gag-migrate`

-   :material-shield-lock:{ .lg .middle } __Secure by default__

    ---

    Hardening reconciled by default:

    - `baseline` Pod Security Admission per namespace
    - Default-deny NetworkPolicies
    - Credentials never in env vars
    - [Workload identity](design/05-security.md#57-workload-identity-the-no-pem-delegation-model) keeps the App key out
    - Signed images, SBOM, and SLSA provenance
    - [Kata micro-VM workers](operations/kata-dind-workloads.md), proven in our own CI

-   :material-tape-measure:{ .lg .middle } __Right-size from measured usage__ <span class="gag-v2-badge">v2</span>

    ---

    No more guessed `resources`:

    - Per-job usage peaks sampled
    - [Recommendations in `RunnerSet` status](operations/worker-rightsizing.md)
    - Opt-in profiles auto-apply at pod build
    - `SizingDrift` warns; GPUs never touched

</div>
</div>

<div class="gag-section-intro" markdown>

## Who GAG is for

GAG targets a specific audience: teams that **must** self-host runners and run them for **many tenants on one cluster**. If that's you, here's the value per segment.

</div>

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-account-group:{ .lg .middle } __Platform & developer-experience teams__

    ---

    Multi-tenant CI on a shared cluster:

    - Enforce a per-team quota without stranding jobs
    - Tenants self-serve from one `ActionsGateway`
    - No ticket queue for every runner change

-   :material-shield-account:{ .lg .middle } __Orgs that must self-host__

    ---

    Driven by a hard constraint, not preference:

    - Compliance or data-residency requirements
    - EMU or firewalled-service IP allow-lists
    - Per-tenant egress IPs you allow-list directly

-   :material-expansion-card:{ .lg .middle } __GPU / ML platform teams__

    ---

    Done paying for accelerators between jobs:

    - Workers scale to zero, so no idle GPU
    - GPU nodes return to the scheduler on completion
    - Priority tiers keep critical GPU jobs scheduling

</div>
</div>

<p class="gag-fit-note" markdown="span">:material-information-outline: **Not your setup?** Three cases where something else is the better answer, and we would rather say so. If you are happy running on a vendor's infrastructure, a managed runner service wins on speed and setup. If managed Kubernetes is cheap where you are and your CI fits in one cloud, a **cluster per tenant** in its own project isolates harder than any shared cluster can. And if you run one team on one cluster, [ARC](https://github.com/actions/actions-runner-controller) is a reasonable choice and is what GAG is built on the same protocol as. GAG is for the case where the nodes are big and expensive, several teams have to share them, and that sharing has to be safe.</p>

<div class="gag-section-intro" markdown>

## How it fits together

A four-tier system: a cluster-scoped manager gives each tenant an isolated gateway from its `ActionsGateway`. Jobs are acquired with the **same single-acquirer runner-scale-set protocol ARC uses**, through the same [`actions/scaleset`](https://github.com/actions/scaleset) client library, and it is the shipped default. So `runs-on` keeps working and the protocol is not the difference. What differs is that the acquisition decision lives in the control plane rather than in the runner pod, which is what lets a job be declined before it is claimed.

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
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; provisions the Actions Gateway Controller (AGC) + proxy per tenant</div>
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

<p class="gag-flow__caption" markdown="span">Read the [architecture overview](design/02-architecture.md) for the full breakdown, jump to [why GAG over ARC](why-gag.md), browse [every feature](features.md), or see the [public roadmap](roadmap.md) for what's next.</p>
