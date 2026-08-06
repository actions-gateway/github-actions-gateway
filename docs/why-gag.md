---
hide:
  - navigation
  - toc
---

<div class="gag-vs-hero" markdown>
<div class="gag-vs-hero__lead" markdown>

<p class="gag-eyebrow">Comparison · ARC alternative</p>

# Why GitHub Actions Gateway over ARC?

<p class="gag-vs-hero__lede">Actions Runner Controller (ARC) scale-set mode struggles with one job: running <strong>many runner sets, for many tenants, in one shared cluster, cost-effectively, with each tenant safely capped by its own <code>ResourceQuota</code></strong>. GitHub Actions Gateway (GAG) was built for exactly that, without giving up the self-service that makes a shared cluster worth running.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Migrating from ARC](operations/migration-from-arc.md){ .md-button }
[See the architecture](design/02-architecture.md){ .md-button }

</div>
<div class="gag-vs-hero__proof">
  <p class="gag-vs-hero__proof-cap">When a worker is evicted, preempted, or blocked by a full <code>ResourceQuota</code></p>
  <div class="gag-vs-row gag-vs-row--arc"><span class="gag-vs-row__tag">ARC</span><span class="gag-vs-row__text">the runner is marked <code>Failed</code> and the job sits in GitHub's queue until someone reruns it by hand</span></div>
  <div class="gag-vs-row gag-vs-row--gag"><span class="gag-vs-row__tag">GAG</span><span class="gag-vs-row__text">the job is concluded (~10 min lock-lapse bound at worst) and re-run automatically: no manual rerun, and it runs as soon as capacity frees up</span></div>
</div>
</div>

## The problem ARC leaves you with

The failures compound, but they all trace back to one root: ARC's poor fit with
`ResourceQuota` makes per-tenant quotas unsafe, and unsafe quotas are what block
letting tenants run their own runners.

<div class="gag-pillars gag-pillars--problem gag-cols-2" markdown>
<div class="grid cards" markdown>

-   :material-lock-alert:{ .lg .middle } __`ResourceQuota` is unsafe__

    ---

    A quota-blocked or evicted job can't recover on its own:

    - ARC retries the same runner ([30 s loop](https://github.com/actions/actions-runner-controller/pull/4305)), then marks it `Failed`
    - the job sits in GitHub's queue until GitHub's queue timeout cancels it (at least 24 h, up to 48 h)
    - cleared and rerun by hand ([#4155](https://github.com/actions/actions-runner-controller/issues/4155), [#4203](https://github.com/actions/actions-runner-controller/issues/4203)), so teams avoid enforcing quotas

-   :material-trending-down:{ .lg .middle } __Critical jobs starve__

    ---

    No way to reserve capacity for expensive runners:

    - each `AutoscalingRunnerSet` only caps itself with `maxRunners`
    - no primitive for "GPU always keeps N slots"
    - cheap CPU pods exhaust the quota; big tests stall

-   :material-memory:{ .lg .middle } __Listener pods pile up__

    ---

    One always-on listener pod per scale set, running 24/7:

    - a pod slot + a cluster IP each
    - held alive just to long-poll GitHub
    - 10 scale sets ≈ 10 always-on pods before a job runs

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
    <span class="gag-stat__num">1&nbsp;pod</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener footprint for 10 runner sets</strong>: every listener is a ~12&nbsp;KiB goroutine in one shared pod, versus 10 always-on listener pods (and 10 cluster IPs) on ARC</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">0</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Idle GPU pods between jobs</strong>: workers exist only while a job runs, deleted on completion</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">Auto</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Handling for quota-blocked and disrupted jobs</strong>: a job the quota has no room for is never taken on, so it stays queued at GitHub until there is capacity; a worker lost to eviction, scheduler preemption, or a node drain has its job re-run automatically, no manual rerun. Both run on both acquisition tiers</span>
  </div>
  <div class="gag-stat">
    <span class="gag-stat__num">1</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Namespace a tenant self-serves in</strong>: declare your gateway and runner sets; the Gateway Manager Controller (GMC) provisions the controller, proxy pool, RBAC, and network policies to run within the platform-owned quota, no per-tenant cluster-admin</span>
  </div>
</div>

## GAG vs ARC (scale-set mode)

GAG acquires jobs with the **same runner-scale-set protocol ARC uses**: a single
acquirer per runner set, capacity-gated assignment, no many-acquirers fan-out. It
is the **shipped default** in the v2 API. So the comparison below is
capability-for-capability against ARC's *own* model: every GAG row is **additive**,
not a different-architecture trade-off. The difference is what surrounds the shared
acquisition core: quota safety, priority tiers, per-tenant egress, and control-plane
footprint.

| Capability | ARC (scale-set mode) | GitHub Actions Gateway |
| --- | --- | --- |
| Runner-scale-set acquisition (single-acquirer, no fan-out) | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes, by default <span class="gag-v2-badge">v2</span> |
| Ephemeral, single-use runner pods | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Custom runner pod template & image | :material-check-circle:{ .gag-yes } yes | :material-check-circle:{ .gag-yes } yes |
| Workers scale to zero between jobs | :material-check-circle:{ .gag-yes } yes, with `minRunners: 0` | :material-check-circle:{ .gag-yes } yes, by default |
| Safe under a per-tenant `ResourceQuota` | :material-close-circle:{ .gag-no } quota-blocked jobs stall; manual cleanup + rerun | :material-check-circle:{ .gag-yes } [won't take on a job it can't place](design/04-operational-flows.md#42-job-execution-flow-agc)<br><span class="gag-cont">live quota headroom is read *before* the job is claimed. On the default tier it bounds the capacity advertised to GitHub, on classic it declines the claim, so the job stays queued at GitHub. If headroom is lost afterwards, the pod create is retried in place while the lock is held</span> |
| Auto-re-run jobs disrupted mid-flight (eviction / preemption / drain) | :material-close-circle:{ .gag-no } runner marked `Failed`; the job waits in GitHub's queue for a manual rerun | :material-check-circle:{ .gag-yes } [re-run automatically, with a per-run retry budget](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do)<br><span class="gag-cont">kubelet evictions, scheduler preemptions under a `priorityTiers` floor, node drains, and hand-deleted workers all re-run through GitHub's own re-run API, retried until GitHub accepts it, with `maxEvictionRetries` capping the budget per run</span> |
| Stop claiming jobs when the cluster can't place the worker | :material-close-circle:{ .gag-no } the runner claims its own job, so there is no seat before the claim to decide at | :material-check-circle:{ .gag-yes } opt-in [`capacityGate` on the runner set](operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)<br><span class="gag-cont">off by default. When on, an unplaceable worker shape (drained pool, changed taint, spot gone) stops the gateway taking on more work, so jobs stay queued at GitHub instead of being claimed and cancelled. It bounds the *rate* of wasted claims, roughly one per `pendingPodDeadline` window. It does not eliminate the first one. The tenant turns it on; the platform states once, on the gateway, whether the cluster has a node autoscaler, so a runner set can never gate on a signal that is wrong for the cluster it runs in. Where a node may still arrive, the gate waits for cluster-autoscaler or Karpenter to say it will not add one</span> |
| Guaranteed floor for critical runner types | :material-close-circle:{ .gag-no } no per-quota primitive | :material-check-circle:{ .gag-yes } [priority tiers per runner set](design/02-architecture.md) |
| Throttle the *rate* new workers start (anti-stampede) | :material-close-circle:{ .gag-no } only `maxRunners` caps the count, and a burst starts all at once | :material-check-circle:{ .gag-yes } opt-in [`scaleUp` creation-rate limit per set](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)<br><span class="gag-cont">for shared-egress onset (NAT / firewall / VPN)</span> |
| Per-tenant dedicated egress IPs | :material-close-circle:{ .gag-no } shared cluster egress | :material-check-circle:{ .gag-yes } [per-tenant proxy pool](design/network-architecture.md)<br><span class="gag-cont"><span class="gag-v2-badge">v2</span> proxy optional</span> |
| Listener footprint, 10 runner sets at rest | :material-close-circle:{ .gag-no } 10 always-on pods + 10 cluster IPs | :material-check-circle:{ .gag-yes } 1 shared pod, ~12 KiB per listener session |
| Per-tenant utilization metrics | :material-close-circle:{ .gag-no } scale-set metrics, not tenant-scoped | :material-check-circle:{ .gag-yes } [Prometheus per tenant + group](operations/observability.md)<br><span class="gag-cont">job counts in `kubectl get`; ready-to-apply [tenant dashboard + alerts as code](operations/observability-dashboards.md#tenant-dashboard)</span> |
| Right-size runner resources from measured usage | :material-close-circle:{ .gag-no } no feedback loop, so runner `resources` stay a guess | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [measured recommendations in `RunnerSet` status](operations/worker-rightsizing.md)<br><span class="gag-cont">+ opt-in profiles that auto-apply them at pod build (`Binpack`/`Throughput`/`NodeShare`), a `SizingDrift` condition, and per-job peak metrics</span> |
| Cross-tenant fleet health view (platform admin) | :material-close-circle:{ .gag-no } controller + per-scale-set metrics, aggregated by hand; no bundled dashboard | :material-check-circle:{ .gag-yes } [single-pane GMC fleet rollups](operations/observability-metrics.md#full-metrics-reference)<br><span class="gag-cont">degraded / egress-stale / quota per gateway, + a [platform dashboard](operations/observability-dashboards.md#platform-dashboard)</span> |
| Multiple gateways per namespace | :material-check-circle:{ .gag-yes } multiple `AutoscalingRunnerSet`s | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [multiple scoped gateways per namespace](operations/migration-v1-to-v2.md) |
| Reusable runner pod templates | :material-close-circle:{ .gag-no } template inlined per `AutoscalingRunnerSet` | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> shared [`RunnerTemplate`](operations/migration-v1-to-v2.md)<br><span class="gag-cont">cluster-wide [`ClusterRunnerTemplate`](operations/migration-v1-to-v2.md)</span> |

Every capability above is available today.

<!-- The canonical fires/doesn't-fire matrix is
     docs/operations/troubleshooting.md § Which Disruptions Auto-Re-Run a Job.
     When a case is added or removed there, update this box too. -->
!!! info "Disruptions re-run themselves, but failures and cancels never do"

    The auto-re-run claim is scoped on purpose. What re-runs itself: a job whose
    worker the *cluster* took away: a kubelet eviction, a scheduler preemption
    under a `priorityTiers` floor, a node drain, even a stray
    `kubectl delete pod`. Each is retried until GitHub accepts it, within a
    per-run budget (`maxEvictionRetries`). What never does, by design: a job
    that ran and **failed** (re-running it would mask real breakage), a run you
    **cancelled** (a cancel is the intended stop), and workers the gateway's own
    cleanup reaped as stuck (a re-run would loop them). The full boundary, with
    the detection marks and metrics:
    [which disruptions auto-re-run a job](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do).

!!! info "Why measured right-sizing can't be bolted onto ARC"

    The right-sizing row is structural, not a feature race. Ephemeral runner
    pods, ARC's and GAG's alike, run one job, live minutes, and have no
    `/scale`-style controller to group them, so stock Vertical Pod Autoscaler
    (and the dashboards built on it) cannot size them: its grouping, its
    evict-and-resize actuation, and its long-running-service statistics all
    miss this workload shape. The only place the loop can close is *inside the
    controller that creates the pods*, at pod-build time, which is where GAG
    runs it: sample per-job peaks, publish the recommendation on the
    `RunnerSet`, and (opt-in) apply it to the next job's pod, with GPUs never
    touched. The full alternatives analysis is in
    [Appendix D §D.7](design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on).

!!! note "Onboarding: start on v2"

    New tenants should onboard on the **recommended v2 API** at
    `actions-gateway.com/v2beta1`: a decomposed `ActionsGateway` + `RunnerSet` +
    `RunnerTemplate`, with an optional standalone `EgressProxy`; the rows marked
    <span class="gag-v2-badge">v2</span> are v2-only. The single-custom-resource (CR)
    `v1alpha1` shape shown below is still fully served but
    **[deprecated, and removed at `v2.0.0`](operations/v1alpha1-deprecation.md)**. See the
    [v1 → v2 migration guide](operations/migration-v1-to-v2.md) and the
    [getting-started walkthrough](getting-started.md) for the v2 object set.

!!! info "The numbers behind these claims"

    For limits and Service Level Objectives, see
    [Appendix A — Capacity Targets & SLOs](design/appendix-a-capacity-slos.md); for
    the utilization-and-cost argument,
    [Appendix F — Cost model](design/appendix-f-cost-model.md).

!!! warning "Where GAG is behind ARC"

    It's **maturity, not capability.** ARC is GA and widely deployed; GAG's v2 API
    has only just reached beta (`v2beta1`, its first stability contract) and rides a
    Public-Preview runner-scale-set
    protocol. That is precisely why the v1 → v2 migration is handled on a committed,
    documented schedule with a working
    [`gag-migrate`](operations/migration-v1-to-v2.md) tool. The discipline is the
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
    - Default-deny network: DNS + own proxy only
    - App keys read-only; never in env, never cached
    - Controller writes confined to tenant namespaces

-   :material-clipboard-check:{ .lg .middle } __Lower operational cost__

    ---

    What you'd hand-build around ARC, reconciled from one CR:

    - NetworkPolicies · PSA · RBAC · egress
    - No Kyverno/OPA required: in-tree PodSecurity
    - Kept in sync as tenants come and go

-   :material-check-decagram:{ .lg .middle } __Ready out of the box__

    ---

    Secure by default; looser is an explicit opt-in:

    - Default-deny ingress, cluster-only DNS
    - Per-tenant egress IPs, mutual-TLS metrics
    - Signed images + Software Bill of Materials (SBOM) + Supply-chain Levels for Software Artifacts (SLSA) provenance

</div>
</div>

!!! info "Sandboxed runtimes compose with these defaults, and GAG's own CI runs that way"

    A sandboxed runtime is just a `runtimeClassName` on the worker pod template,
    so GAG and ARC can both set one. The differentiator is not that field. It is
    the layer underneath it, and the evidence that the combination holds.

    **Kata bounds the kernel, not the pod network.** A micro-VM does not change
    the pod's network identity: the cloud metadata service still answers from
    *inside* the guest, so a compromised job can mint node credentials over the
    pod network even though the kernel it escaped into is disposable. GAG's
    default-deny NetworkPolicies are what close that path, reconciled per tenant
    rather than hand-built alongside. Kata is one layer, not a posture.

    **GAG's own end-to-end CI runs under it.** The suite creates a `kind` cluster
    inside a worker pod with `runtimeClassName: kata` and **zero**
    `privileged: true`, validated on a nested-virtualization GKE node pool (node
    kernel `6.8.0-1054-gke`, guest kernel `6.18.35`), and the default for that
    suite ever since. The cluster-side how-to is written down, including
    the capability set an unprivileged `dockerd` needs and
    [what Kata does not buy you](operations/kata-dind-workloads.md#what-kata-does-not-buy-you).

    **This is not yet a claim about untrusted pull requests.** That CI variant
    still runs a permissive egress policy, because its jobs pull from CDN-fronted
    public registries no CIDR allowlist can pin. Closing it takes an in-cluster
    pull-through mirror plus egress scoped to the mirror, GitHub, and DNS: see
    the [roadmap](roadmap.md#in-progress--near-term).

For the full threat model, per-profile controls, and the abuse-response
playbooks, see [Security](design/05-security.md) and
[Security operations](operations/security-operations.md).

## Composable building blocks, not one giant CR

A tenant still declares only namespace-scoped resources, and the GMC provisions the
controller, proxy pool, RBAC, and network policies to match, all within the **platform-owned `ResourceQuota`** the GMC never creates
or mutates, with no per-tenant cluster-admin after the initial install. What
changed with the **recommended v2 API** is that the single-CR monolith is
decomposed into small, reusable kinds, and that decomposition *is* a
differentiator ARC's inlined, per-scale-set model structurally can't express:

<div class="gag-pillars gag-cols-2" markdown>
<div class="grid cards" markdown>

-   :material-content-copy:{ .lg .middle } __Reuse the pod shape__

    ---

    One `RunnerTemplate`, or cluster-wide `ClusterRunnerTemplate`, is referenced
    by every `RunnerSet`. ARC inlines the pod template into each
    `AutoscalingRunnerSet`, so N runner types means N copies to keep in sync.

-   :material-account-key:{ .lg .middle } __Clean ownership boundary__

    ---

    Platform owns the quota, the `PriorityClass` allowlist, and cluster templates;
    the tenant composes `RunnerSet`s within them. ARC has no primitive to separate
    platform-owned from tenant-owned concerns.

-   :material-transit-connection-variant:{ .lg .middle } __Egress on purpose__

    ---

    A standalone `EgressProxy` is referenced by the gateway (or per `RunnerSet`),
    or dropped entirely for direct egress, which stays `NetworkPolicy`-restricted.
    ARC has no per-tenant egress primitive at all.

-   :material-view-grid-plus:{ .lg .middle } __Many gateways, one namespace__

    ---

    Multiple scoped `ActionsGateway`s coexist in a namespace, each with its own
    GitHub binding and runner sets, not one CR that must own everything.

</div>
</div>

The v2 object set below is feature-equivalent to the legacy single-CR example, a
proxied gateway with a GPU runner set (priority tiers) and a Linux runner set:

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
    across every namespace. The Pod Security Admission level is a **namespace
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
    its single `runs-on` match target (`runs-on: gpu`), unique across the sets under
    one gateway. This is the same single-name routing ARC scale sets use, so
    `runs-on` lines carry across from ARC unedited.
6.  The first 5 GPU pods get the higher-priority `PriorityClass`; the next tier
    bursts opportunistically; the final threshold caps total concurrency. The
    `priorityClassName` values must be on the platform's allowlist (the GMC
    `--allowed-priority-classes` flag), and whether a tier preempts is set on the
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
