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

<p class="gag-claims"><span>No idle workers.</span> <span>Disrupted jobs auto-retry.</span> <span>Enforceable quotas.</span></p>

<p class="gag-tagline">GitHub Actions Gateway (GAG) is an Actions Runner Controller (ARC) alternative for shared, multi-tenant clusters.</p>

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
  --version 1.4.0 \
  --namespace gmc-system --create-namespace \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>

# v2 API (recommended): apply the signed, pre-rendered CRDs
kubectl apply --server-side -f \
  https://github.com/actions-gateway/github-actions-gateway/releases/download/v1.4.0/actions-gateway-crds-v2.yaml
```

</div>

<div class="gag-section-intro" markdown>

## What GAG gives you

These ladder up one way: **safe quotas and self-healing disruption make shared capacity usable**, which is what lets you bin-pack expensive nodes and run on preemptible capacity. [Estimate your savings vs ARC →](design/appendix-f-cost-model.md#f5-savings-calculator-this-system-vs-arc) · [See every feature →](features.md)

</div>

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">1&nbsp;pod</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener footprint at rest</strong>: all listeners are goroutines in one shared pod; ARC runs one always-on pod per set</span>
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
    - [Share a pool across namespaces, by consent](operations/security-operations.md#sharing-an-egress-proxy-across-namespaces)
    - v2: proxy optional

-   :material-feather:{ .lg .middle } __Lower listener overhead__

    ---

    Listeners are goroutines, not pods:

    - ~12 KiB per listener session
    - One shared pod per tenant
    - ARC: one always-on pod per set

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
    - [Three validated templates ship in-box](operations/runner-template-library.md)
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

<div class="gag-fit-note" markdown>

:material-information-outline: **Not your setup?** Three cases where something else wins, and we would rather say so.

- **A vendor can run your jobs** → a managed runner service, on speed and setup
- **Managed Kubernetes is cheap, CI fits one cloud** → a cluster per team isolates harder
- **One team owns the cluster and the runners** → [ARC](https://github.com/actions/actions-runner-controller), whose protocol GAG is built on

GAG is for big, expensive nodes that several teams must share, safely. [Compare the options →](alternatives.md)

</div>

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

## What a tenant actually declares

The whole object set for a proxied gateway with a GPU runner set (priority
tiers) and a Linux runner set. Every resource is namespaced, none is
cluster-scoped, and no `ResourceQuota` field appears anywhere: the quota is
platform-owned and set on the namespace, so it is a cap the tenant cannot raise.

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy               # (1)!
metadata:
  name: team-a-egress
  namespace: team-a
spec:
  minReplicas: 2
  maxReplicas: 10
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate            # (2)!
metadata:
  name: default
  namespace: team-a
spec:
  podTemplate:
    spec:
      containers:
        - name: runner
---
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway            # (3)!
metadata:
  name: team-a-gateway
  namespace: team-a
spec:
  credentials:
    type: GitHubApp
    githubApp:
      name: my-github-app       # name-only Secret ref in this namespace
  githubURL: https://github.com/team-a-org
  defaultProxyRef:
    name: team-a-egress         # every RunnerSet inherits this unless it sets proxyRef
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: gpu
  namespace: team-a
spec:
  gatewayRef:  { name: team-a-gateway }
  templateRef: { name: default }   # (4)!
  runnerLabels: ["gpu"]         # (5)!
  priorityTiers:                # (6)!
    - priorityClassName: runner-critical
      threshold: 5
    - priorityClassName: runner-standard
      threshold: 20
---
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: linux
  namespace: team-a
spec:
  gatewayRef:  { name: team-a-gateway }
  templateRef: { name: default }
  runnerLabels: ["linux"]
  maxWorkers: 30
```

1.  Optional. A standalone per-tenant egress proxy pool, Horizontal Pod Autoscaler
    (HPA)-managed between these bounds; all GitHub traffic exits through it on
    dedicated IPs. Drop it (and `defaultProxyRef`) for direct, still
    `NetworkPolicy`-restricted egress, collapsing the minimum to three objects.
2.  A reusable pod shape referenced by both `RunnerSet`s below via `templateRef`.
    Define it once; a cluster-scoped `ClusterRunnerTemplate` shares one shape
    across every namespace. The Pod Security Admission (PSA) level is a **namespace
    label** in v2, not a CR field. All gateways in a namespace share one level.
3.  `credentials.githubApp.name` references a `Secret` in this namespace holding
    the GitHub App `appId`, `installationId`, and `privateKey`. The GMC watches the
    reference name, not the Secret contents. See
    [credential rotation](getting-started.md#rotating-github-app-credentials).
    `WorkloadIdentity` is the opt-in no-PEM credential member.
4.  Both runner sets reference the **same** `RunnerTemplate`. There is no
    `ResourceQuota` field on any of these CRs. The single quota every runner set
    shares is **platform-owned**, set on the namespace by the platform admin, so it
    is a real cap the tenant cannot raise. Priority tiers decide who wins when it
    is contended.
5.  Exactly one label per runner set: it is the set's scale-set name at GitHub and
    its single `runs-on` match target (`runs-on: gpu`), unique across the sets
    under one gateway. A single-name `runs-on` carries over from ARC unchanged; a
    workflow targeting an array needs one edit per target, covered in
    [migrating from ARC](operations/migration-from-arc.md).
6.  The first 5 GPU pods get the higher-priority `PriorityClass`; the next tier
    bursts opportunistically; the final threshold caps total concurrency. The
    `priorityClassName` values must be on the platform's allowlist (a watched
    `PriorityClassAllowlist` CR, grown without a GMC restart; the
    `--allowed-priority-classes` flag remains the fail-safe baseline), and whether a tier preempts is set on the
    platform-owned `PriorityClass` object, so a tenant cannot name a class that
    evicts other tenants' pods.

The legacy single-CR `v1alpha1` shape, which expresses this whole gateway in one
`ActionsGateway` CR, is still fully served but
**[deprecated, and removed at `v2.0.0`](operations/v1alpha1-deprecation.md)**; see the
[getting-started walkthrough](getting-started.md#legacy-the-v1alpha1-api-deprecated)
for it and the [v1 → v2 migration guide](operations/migration-v1-to-v2.md) to move
across without changing how your jobs are acquired.

Ready to try it? Follow the [getting-started guide](getting-started.md). Already
running ARC? The [Migrating from ARC guide](operations/migration-from-arc.md) maps
every concept above onto GAG and walks one runner group across with zero downtime.

