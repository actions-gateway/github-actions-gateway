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
  <p class="gag-vs-hero__proof-cap">When a worker is evicted, preempted, or drained mid-job</p>
  <div class="gag-vs-row gag-vs-row--arc"><span class="gag-vs-row__tag">ARC</span><span class="gag-vs-row__text">the runner registration is removed and the job is given up on; re-running it is a manual step</span></div>
  <div class="gag-vs-row gag-vs-row--gag"><span class="gag-vs-row__tag">GAG</span><span class="gag-vs-row__text">the run is concluded and re-run automatically, no manual rerun: seconds for a preemption or drain (measured 15&ndash;26&nbsp;s), ~10 min at worst for a hard eviction that waits out the job lock</span></div>
</div>
</div>

## The problem ARC leaves you with

These trace back to one root, and it is not a missing feature. ARC models a
cluster with **one owner**: the team that runs the cluster is the team that runs
the runners. That is a coherent product, and on a single-tenant cluster it is a
reasonable choice. It is also why ARC has no primitive separating what the
platform owns from what a tenant owns, and why each item below is really the
same gap seen from a different angle.

<div class="gag-pillars gag-pillars--problem gag-cols-2" markdown>
<div class="grid cards" markdown>

-   :material-lock-alert:{ .lg .middle } __`ResourceQuota` is unsafe__

    ---

    A quota-blocked or evicted job can't recover on its own:

    - claimed before the quota is known, so the runner cannot start
    - its single-use registration is already spent when quota is found
    - ARC retries every 30 s, recycling every 10 min ([0.13.1](https://github.com/actions/actions-runner-controller/pull/4305))
    - the job reads *assigned*, so the 24 h queue timeout never fires

-   :material-trending-down:{ .lg .middle } __Critical jobs starve__

    ---

    No way to reserve capacity for expensive runners:

    - each `AutoscalingRunnerSet` only caps itself with `maxRunners`
    - no primitive for "GPU always keeps N slots"
    - cheap CPU pods exhaust the quota; big tests stall

-   :material-memory:{ .lg .middle } __Listener pods pile up__

    ---

    One always-on listener pod per scale set, running 24/7:

    - a pod slot and a pod IP each
    - held alive just to long-poll GitHub
    - 10 scale sets ≈ 10 always-on pods before a job runs

-   :material-ticket-confirmation:{ .lg .middle } __Platform team is the bottleneck__

    ---

    Every tenant is a manual checklist:

    - namespace, quota, RBAC, scale sets, NetworkPolicies, egress
    - per-team setup; every later change is a ticket

</div>
</div>

## What changes with GAG

"Safe to share" means two different things, and GAG needs both. **Operationally
safe** is quota-aware intake and automatic re-run. **Safe to run other people's
code on** is the isolation stance: Pod Security Admission, default-deny egress,
and Kata micro-VM workers. The first is finished; the second is finished for
trusted CI and still in progress for untrusted pull requests.

Enforcing a tight per-tenant quota is only reasonable if a blocked job waits
instead of stalling. Bin-packing tenants onto the same expensive nodes is only
reasonable if a preempted job comes back on its own. Running on preemptible or
spot capacity is only reasonable if the cluster taking a worker away is a
non-event. Take either away and the platform team's options collapse to
over-provisioning, loose quotas, and a ticket queue, which is the path back to a
cluster per team.

So the outcomes compound in one direction:

<div class="gag-flow gag-flow--wide">
  <div class="gag-flow__row">
    <div class="gag-flow__node">
      <span class="gag-flow__tier">Capability</span>
      <span class="gag-flow__title">Quota-aware intake</span>
      <span class="gag-flow__sub">a job the quota cannot place is never claimed</span>
    </div>
    <div class="gag-flow__node">
      <span class="gag-flow__tier">Capability</span>
      <span class="gag-flow__title">Automatic re-run</span>
      <span class="gag-flow__sub">a worker the cluster takes away comes back</span>
    </div>
    <div class="gag-flow__node gag-flow__node--wip">
      <span class="gag-flow__tier">Capability · partly shipped</span>
      <span class="gag-flow__title">Isolation by default</span>
      <span class="gag-flow__sub">PSA, default-deny egress, and Kata micro-VM workers ship today; <a href="../roadmap/">untrusted-PR network isolation</a> does not yet</span>
    </div>
  </div>
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; together they remove the reasons sharing is risky</div>
  <div class="gag-flow__node gag-flow__node--lead">
    <span class="gag-flow__tier">What that unlocks</span>
    <span class="gag-flow__title">Shared capacity is safe to actually use</span>
    <span class="gag-flow__sub">tight per-tenant quotas, bin-packing, and preemptible nodes stop being risks</span>
  </div>
  <div class="gag-flow__arrow" aria-hidden="true">↓&nbsp; which is what the platform team feels</div>
  <div class="gag-flow__row">
    <div class="gag-flow__node">
      <span class="gag-flow__title">Less ops toil</span>
      <span class="gag-flow__sub">no manual reruns, no stuck-job tickets</span>
    </div>
    <div class="gag-flow__node">
      <span class="gag-flow__title">Fewer pages</span>
      <span class="gag-flow__sub">cluster churn stops being an incident</span>
    </div>
    <div class="gag-flow__node">
      <span class="gag-flow__title">Higher utilization</span>
      <span class="gag-flow__sub">hardware you already pay for gets shared</span>
    </div>
    <div class="gag-flow__node">
      <span class="gag-flow__title">More throughput</span>
      <span class="gag-flow__sub">jobs finish per node-hour instead of stalling</span>
    </div>
  </div>
</div>

The [cost model](design/appendix-f-cost-model.md) works the utilization argument
through in numbers; a published benchmark at scale is
[on the roadmap](roadmap.md) and not yet done, so treat those figures as a model
rather than a measurement.

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
| Workers scale to zero between jobs | :material-check-circle:{ .gag-yes } yes, by default | :material-check-circle:{ .gag-yes } yes, by default |
| Safe under a per-tenant `ResourceQuota` | :material-close-circle:{ .gag-no } **claims first, then discovers the quota**<br><span class="gag-cont">burns a single-use runner registration every 10 min, indefinitely; the job reads as *assigned*, so GitHub's 24 h queue timeout never fires</span> | :material-check-circle:{ .gag-yes } **never claims a job it can't place**<br><span class="gag-cont">[reads quota headroom before claiming](design/04-operational-flows.md#42-job-execution-flow-agc), so nothing is spent and the job stays visibly queued until there is room</span> |
| Auto-re-run jobs disrupted mid-flight (eviction / preemption / drain) | :material-close-circle:{ .gag-no } **no re-run mechanism exists**<br><span class="gag-cont">the runner registration is removed and the job is given up on; 0.13.1 recovers the pod, never the job</span> | :material-check-circle:{ .gag-yes } **[re-runs itself in seconds](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do)**<br><span class="gag-cont">15&ndash;26 s measured for a preemption or drain; evictions, drains and hand-deleted workers all covered, under a per-run budget</span> |
| Stop claiming jobs when the cluster can't place the worker | :material-close-circle:{ .gag-no } **acquires every available job unconditionally**<br><span class="gag-cont">the seat exists; no cluster state is consulted in it</span> | :material-check-circle:{ .gag-yes } **opt-in [`capacityGate`](operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)**<br><span class="gag-cont">off by default. Bounds the *rate* of wasted claims to roughly one per `pendingPodDeadline`; it does not eliminate the first</span> |
| Guaranteed floor for critical runner types | :material-close-circle:{ .gag-no } no per-quota primitive | :material-check-circle:{ .gag-yes } [priority tiers per runner set](design/02-architecture.md) |
| Throttle the *rate* new workers start (anti-stampede) | :material-close-circle:{ .gag-no } **no per-set start-rate control**<br><span class="gag-cont">its throttles are controller-wide and meter API calls, not worker onset</span> | :material-check-circle:{ .gag-yes } **opt-in [per-set creation-rate limit](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)**<br><span class="gag-cont">for shared-egress onset (NAT, firewall, VPN)</span> |
| Per-tenant dedicated egress IPs | :material-close-circle:{ .gag-no } **points at a proxy you already run**<br><span class="gag-cont">provisions no pool, manages no lifecycle, gives no per-tenant addresses</span> | :material-check-circle:{ .gag-yes } **provisioned per-tenant pool, [live-validated on GKE](design/network-architecture.md#per-tenant-egress-ip-the-source-ip-mechanism)**<br><span class="gag-cont">2026-07-13: one distinct, stable per-tenant NAT IP. A stable source IP needs Cilium Egress Gateway or cloud NAT beneath the pool, both specified <span class="gag-v2-badge">v2</span></span> |
| Listener footprint, 10 runner sets at rest | :material-close-circle:{ .gag-no } 10 always-on listener pods, each holding a pod IP<br><span class="gag-cont">they run in the controller's namespace, so the cost lands on platform pod density rather than the tenant's quota</span> | :material-check-circle:{ .gag-yes } 1 shared pod, ~12 KiB per listener session |
| Per-tenant utilization metrics | :material-close-circle:{ .gag-no } **opt-in, per scale set**<br><span class="gag-cont">ships commented out; carries a `namespace` label but nothing aggregates across sets, and no quota-headroom series</span> | :material-check-circle:{ .gag-yes } **[per tenant and per runner set](operations/observability.md)**<br><span class="gag-cont">plus a [dashboard and 20 alert rules as code](operations/observability-dashboards.md#tenant-dashboard)</span> |
| Right-size runner resources from measured usage | :material-close-circle:{ .gag-no } no feedback loop, so runner `resources` stay a guess | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [measured recommendations in `RunnerSet` status](operations/worker-rightsizing.md)<br><span class="gag-cont">+ opt-in profiles that auto-apply them at pod build (`Binpack`/`Throughput`/`NodeShare`), a `SizingDrift` condition, and per-job peak metrics</span> |
| Cross-tenant fleet health view (platform admin) | :material-close-circle:{ .gag-no } **one sample dashboard, no alert rules**<br><span class="gag-cont">nothing aggregates across scale sets or tenants</span> | :material-check-circle:{ .gag-yes } **[single-pane fleet rollups](operations/observability-metrics.md#full-metrics-reference)**<br><span class="gag-cont">degraded, egress-stale and quota per gateway, plus a [platform dashboard](operations/observability-dashboards.md#platform-dashboard)</span> |
| Multiple gateways per namespace | :material-check-circle:{ .gag-yes } multiple `AutoscalingRunnerSet`s | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [multiple scoped gateways per namespace](operations/migration-v1-to-v2.md) |
| Reusable runner pod templates | :material-close-circle:{ .gag-no } template inlined per `AutoscalingRunnerSet` | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> shared [`RunnerTemplate`](operations/migration-v1-to-v2.md)<br><span class="gag-cont">cluster-wide [`ClusterRunnerTemplate`](operations/migration-v1-to-v2.md)</span> |

Every GAG capability above is available today.

!!! note "When the ARC column was measured, and why that matters"

    **Measured against ARC `gha-runner-scale-set` 0.14.2 (released 2026-05-22)
    and the `master` branch, on 2026-08-06**, by reading the controller source,
    the chart values, and the release notes rather than the documentation.

    ARC moves, and an undated comparison rots into a false one. Two rows here
    changed at datable releases: 0.13.1 (2025-12-23) changed how a quota-blocked
    pod creation is retried, and 0.14.0 (2026-03-19) added multi-label scale
    sets, which GAG does not have. If you are evaluating on a later ARC than the one
    stamped above, re-check the column rather than trusting it, and
    [tell us what changed](https://github.com/actions-gateway/github-actions-gateway/issues).

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

!!! warning "Where ARC is ahead"

    Some of it is capability, not only maturity. Measured 2026-08-06:

    - **A GitHub Support entitlement**, covering ARC installed via the official
      Helm charts, GitHub Enterprise Server 3.9 and later. GAG has none. Read the
      scope exclusions before relying on it: Kubernetes orchestration, policy
      application, and template customization are explicitly out of scope, which
      is much of what a multi-tenant platform team actually pages about.
    - **Multi-label scale sets** since 0.14.0. A workflow using
      `runs-on: [linux, gpu]` needs one edit per target to move to GAG, which
      admits exactly one label per runner set.
    - **`containerMode: kubernetes`**, which runs `container:` and `services:`
      steps as separate pods with a provisioned volume. GAG runs one worker pod
      per job, so that path is Docker-in-Docker (under Kata, unprivileged) rather
      than a non-privileged pod-per-step model.
    - **GitHub runner groups** (`runnerGroup`), the forge-side control over which
      repositories may target a runner set.
    - **GHES that is actually tested.** GAG serves GHES gateways and marks both
      of its GHES features untested against a real appliance.

    And the maturity gap is real: ARC is GA and widely deployed, while GAG's v2
    API has only just reached beta (`v2beta1`, its first stability contract). That
    is why the v1 → v2 migration runs on a committed, documented schedule with a
    working [`gag-migrate`](operations/migration-v1-to-v2.md) tool. The discipline
    is the "won't strand you" signal while the track record accumulates.

    One thing that is **not** a differentiator either way: both ride the same
    Public-Preview runner-scale-set protocol, through the same
    [`actions/scaleset`](https://github.com/actions/scaleset) client library.

## Secure by default

Built for shared clusters running other teams' code: the multi-tenant hardening
ships as reconciled defaults, not a post-install project.

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-lock:{ .lg .middle } __Risk reduction__

    ---

    Untrusted job code is boxed in by default:

    - `baseline` Pod Security Admission per namespace
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
    - Signed images, SBOM, and SLSA provenance

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
    its single `runs-on` match target (`runs-on: gpu`), unique across the sets under
    one gateway. ARC scale sets have supported multiple labels since 0.14.0
    (2026-03-19), so a workflow targeting `runs-on: [linux, gpu]` needs one edit
    per target to move across: split it into a runner set with a single
    descriptive label. A single-name `runs-on` carries over unchanged.
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
