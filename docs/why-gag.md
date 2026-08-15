---
hide:
  - navigation
  - toc
---

<div class="gag-vs-hero" markdown>
<div class="gag-vs-hero__lead" markdown>

<p class="gag-eyebrow">Comparison · ARC alternative</p>

# Why GitHub Actions Gateway over ARC?

<p class="gag-vs-hero__lede">Actions Runner Controller (ARC) struggles with one job: <strong>many runner sets, many tenants, one shared cluster, each tenant safely capped by its own <code>ResourceQuota</code></strong>. GitHub Actions Gateway (GAG) is built for that job.</p>

[Get started](getting-started.md){ .md-button .md-button--primary } [Migrating from ARC](operations/migration-from-arc.md){ .md-button } [See the architecture](design/02-architecture.md){ .md-button }

</div>
<div class="gag-vs-hero__proof">
  <p class="gag-vs-hero__proof-cap">When the job is disrupted</p>
  <div class="gag-vs-row gag-vs-row--arc"><span class="gag-vs-row__tag">ARC</span><span class="gag-vs-row__text">gives up on it. You re-run it by hand.</span></div>
  <div class="gag-vs-row gag-vs-row--gag"><span class="gag-vs-row__tag">GAG</span><span class="gag-vs-row__text">re-runs it. Measured 15&ndash;26&nbsp;s, preemption or drain.</span></div>
</div>
</div>

## The problem ARC leaves you with

All four trace to one root, and it is not a missing feature.
**ARC models a cluster with one owner**, so it has no primitive separating what the platform owns from what a tenant owns.
That is a reasonable product for a single-tenant cluster, and the same gap from four angles once teams share one.

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
    - the count tracks runner sets, not job volume

-   :material-ticket-confirmation:{ .lg .middle } __Platform team is the bottleneck__

    ---

    Every tenant is a manual checklist:

    - namespace, quota, RBAC, scale sets, NetworkPolicies, egress
    - per-team setup; every later change is a ticket

</div>
</div>

## What changes with GAG

Three capabilities that only pay off together, and what they unlock:

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

A tenant declares only namespaced resources; the GMC provisions the rest inside a platform-owned quota it cannot write, with no cluster admin after the install ([the object set](getting-started.md#4-create-your-gateway-and-runner-set-v2-recommended)).
The [cost model](design/appendix-f-cost-model.md) runs the utilization argument in numbers, but a benchmark at scale is [on the roadmap](roadmap.md) and not yet done: treat those figures as a model, not a measurement.

## GAG vs ARC (scale-set mode)

**This is not a protocol argument.** GAG acquires jobs with the same runner-scale-set protocol ARC uses, shipped as the v2 default, so every row below is additive rather than a different-architecture trade-off.
What differs is what surrounds that shared core.

Each ARC cell carries the chart version it was read at and the date it was read.
A cell with no stamp asserts nothing; see [how to read the ARC column](#how-to-read-the-arc-column) under the table.

| Capability | ARC (scale-set mode) | GitHub Actions Gateway |
| --- | --- | --- |
| Runner-scale-set acquisition (single-acquirer, no fan-out) | :material-check-circle:{ .gag-yes } yes<br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } yes, by default <span class="gag-v2-badge">v2</span> |
| Ephemeral, single-use runner pods | :material-check-circle:{ .gag-yes } yes<br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } yes |
| Custom runner pod template & image | :material-check-circle:{ .gag-yes } yes<br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } yes |
| Bind a runner set to a named GitHub runner group | :material-check-circle:{ .gag-yes } yes, `runnerGroup` per scale set<br><span class="gag-cont">a name that resolves to no group fails the reconcile rather than falling back to the installation default</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [`runnerGroup`, or one `defaultRunnerGroup` per gateway](operations/tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group)<br><span class="gag-cont">the same failure, moved to admission, so the set is refused before it exists</span> |
| Multi-label runner sets (a `runs-on` array matches) | :material-check-circle:{ .gag-yes } yes, since 0.14.0<br><span class="gag-cont">`runnerScaleSetLabels`, deduplicated against the set name</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [every `runnerLabel` is registered](operations/troubleshooting.md#jobs-targeting-one-of-a-runner-sets-labels-never-start-runnerlabelsincomplete) |
| Workers scale to zero between jobs | :material-check-circle:{ .gag-yes } yes, by default<br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } yes, by default |
| Safe under a per-tenant `ResourceQuota` | :material-close-circle:{ .gag-no } **claims first, then discovers the quota**<br><span class="gag-cont">retries pod creation every 30 s and burns a fresh single-use registration every 10 min, indefinitely; the job reads as *assigned*, so GitHub's 24 h queue timeout never fires</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **never claims a job it can't place**<br><span class="gag-cont">[reads quota headroom before claiming](design/04-operational-flows.md#42-job-execution-flow-agc), so nothing is spent and the job stays visibly queued until there is room</span> |
| Auto-re-run jobs disrupted mid-flight (eviction / preemption / drain) | :material-close-circle:{ .gag-no } **no re-run mechanism exists**<br><span class="gag-cont">the runner registration is removed and the job is given up on; 0.13.1 recovers the pod, never the job</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **[re-runs itself in seconds](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do)**<br><span class="gag-cont">15&ndash;26 s measured for a preemption or drain; evictions, drains and hand-deleted workers all covered, under a per-run budget</span> |
| Stop claiming jobs when the cluster can't place the worker | :material-close-circle:{ .gag-no } **acquires every available job unconditionally**<br><span class="gag-cont">the seat exists; no cluster state is consulted in it</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **opt-in [`capacityGate`](operations/troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs)**<br><span class="gag-cont">off by default. Bounds the *rate* of wasted claims to roughly one per `pendingPodDeadline`; it does not eliminate the first</span> |
| Bound a stranded worker with no live controller | :material-close-circle:{ .gag-no } **pass-through only**<br><span class="gag-cont">`activeDeadlineSeconds` is copied from the pod template if an operator wrote one; nothing derives, reconciles or defaults a bound</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[`maxWorkerLifetime` stamped on every worker](design/02-architecture.md)**<br><span class="gag-cont">as its `activeDeadlineSeconds`, so the *kubelet* enforces it and workers stay bounded with the controller down</span> |
| Guaranteed floor for critical runner types | :material-close-circle:{ .gag-no } **no reserved floor**<br><span class="gag-cont">ordering works, since `priorityClassName` reaches the runner pods; `minRunners` and `maxRunners` are per scale set, so neither holds a slice another set cannot spend ([the two gaps](design/appendix-d-alternatives-considered.md#d3-actions-runner-controller-arc))</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } [priority tiers per runner set](design/02-architecture.md) |
| Throttle the *rate* new workers start (anti-stampede) | :material-close-circle:{ .gag-no } **no per-set start-rate control**<br><span class="gag-cont">its throttles are controller-wide and meter API calls, not worker onset</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **opt-in [per-set creation-rate limit](operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource)**<br><span class="gag-cont">for shared-egress onset (NAT, firewall, VPN)</span> |
| Pod Security Admission on tenant namespaces | :material-close-circle:{ .gag-no } **neither scale-set chart sets one**<br><span class="gag-cont">no `pod-security.kubernetes.io/*` label is templated, so a namespace keeps whatever policy the cluster already applied and the chart asserts nothing about it</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[`baseline` per tenant namespace](design/05-security.md)**<br><span class="gag-cont">reconciled rather than opt-in, in-tree PodSecurity, so no Kyverno or Gatekeeper is required</span> |
| Default-deny worker network isolation | :material-close-circle:{ .gag-no } **ships no `NetworkPolicy` at all**<br><span class="gag-cont">neither scale-set chart templates one, so worker pods reach whatever the cluster default allows, cloud metadata included; the isolation is a separate project you build and keep in sync</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[default-deny, reconciled per tenant](design/05-security.md)**<br><span class="gag-cont">DNS and the tenant's own proxy only, re-derived as tenants come and go</span> |
| Two tenants claiming one scale-set name | :material-close-circle:{ .gag-no } **binds to the existing set instead of failing**<br><span class="gag-cont">a set whose name already exists in the runner group is reused, with no check on who owns it, so both namespaces' listeners acquire from one queue</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[admission refuses the pair](operations/troubleshooting.md#actionsgateway-reports-scalesetnamecollision)**<br><span class="gag-cont">GitHub-wide; a collision carried in from an older release is reported on the gateway instead</span> |
| Per-tenant dedicated egress IPs | :material-close-circle:{ .gag-no } **points at a proxy you already run**<br><span class="gag-cont">provisions no pool, manages no lifecycle, gives no per-tenant addresses</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **provisioned per-tenant pool, [live-validated on GKE](design/network-architecture.md#per-tenant-egress-ip-the-source-ip-mechanism)**<br><span class="gag-cont">2026-07-13: one distinct, stable per-tenant NAT IP. A stable source IP needs Cilium Egress Gateway or cloud NAT beneath the pool, both specified <span class="gag-v2-badge">v2</span></span> |
| GitHub App private key kept out of the cluster | :material-close-circle:{ .gag-no } **the listener reads the key either way**<br><span class="gag-cont">the PEM sits in `githubConfigSecret` and is copied into the generated listener config Secret too; opt-in Azure Key Vault keeps it out of etcd, but the listener still fetches the key itself</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> **opt-in [`workloadIdentity`](design/05-security.md#57-workload-identity-the-no-pem-delegation-model)**<br><span class="gag-cont">an external signer signs the App JWT, so no App key exists in the cluster in any form. Its default `githubApp` member is the same in-cluster PEM as ARC's</span> |
| Listener footprint at rest | :material-close-circle:{ .gag-no } one always-on listener pod per scale set, each holding a pod IP<br><span class="gag-cont">they run in the controller's namespace, so the cost lands on platform pod density rather than the tenant's quota</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } 1 shared pod, ~12 KiB per listener session |
| Control-plane availability out of the box | :material-close-circle:{ .gag-no } **one replica, no `PodDisruptionBudget`**<br><span class="gag-cont">`replicaCount: 1`, with leader election documented as enabled only above one; neither scale-set chart ships a PDB</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[two replicas, leader election, PDB, anti-co-location](operations/install.md#verify-a-healthy-install)**<br><span class="gag-cont">by default, and the lease is released on shutdown, so failover takes seconds rather than a lease timeout</span> |
| Per-tenant utilization metrics | :material-close-circle:{ .gag-no } **opt-in, per scale set**<br><span class="gag-cont">ships commented out; carries a `namespace` label but nothing aggregates across sets, and no quota-headroom series</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **[per tenant and per runner set](operations/observability.md)**<br><span class="gag-cont">plus a [dashboard and 20 alert rules as code](operations/observability-dashboards.md#tenant-dashboard)</span> |
| Right-size runner resources from measured usage | :material-close-circle:{ .gag-no } no feedback loop, so runner `resources` stay a guess<br><span class="gag-cont">`AutoscalingRunnerSet` status carries runner counts and a phase, and nothing else</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [measured recommendations in `RunnerSet` status](operations/worker-rightsizing.md)<br><span class="gag-cont">+ opt-in profiles that auto-apply them at pod build (`Binpack`/`Throughput`/`NodeShare`), a `SizingDrift` condition, and per-job peak metrics</span> |
| Warn before the runner agent falls below GitHub's minimum | :material-close-circle:{ .gag-no } **no version check exists**<br><span class="gag-cont">scale sets are created with `DisableUpdate: true`, so a pinned image stays pinned; the `Outdated` phase tracks spec drift, not agent version</span><br><span class="gag-asof">0.14.2 · 2026-08-15</span> | :material-check-circle:{ .gag-yes } **[reported before GitHub enforces it](operations/troubleshooting.md#worker-image-runner-version)**<br><span class="gag-cont">and an image reference naming no version says so rather than passing silently</span> |
| Cross-tenant fleet health view (platform admin) | :material-close-circle:{ .gag-no } **one sample dashboard, no alert rules**<br><span class="gag-cont">nothing aggregates across scale sets or tenants</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } **[single-pane fleet rollups](operations/observability-metrics.md#full-metrics-reference)**<br><span class="gag-cont">degraded, egress-stale and quota per gateway, plus a [platform dashboard](operations/observability-dashboards.md#platform-dashboard)</span> |
| Multiple gateways per namespace | :material-check-circle:{ .gag-yes } multiple `AutoscalingRunnerSet`s<br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> [multiple scoped gateways per namespace](operations/migration-v1-to-v2.md) |
| Reusable runner pod templates | :material-close-circle:{ .gag-no } template inlined per `AutoscalingRunnerSet`<br><span class="gag-cont">the chart takes one `template:` PodSpec per release; nothing shares it between sets</span><br><span class="gag-asof">0.14.2 · 2026-08-12</span> | :material-check-circle:{ .gag-yes } <span class="gag-v2-badge">v2</span> shared [`RunnerTemplate`](operations/migration-v1-to-v2.md)<br><span class="gag-cont">cluster-wide [`ClusterRunnerTemplate`](operations/migration-v1-to-v2.md)</span> |

Every GAG capability above is available today.
Rows marked <span class="gag-v2-badge">v2</span> need the `actions-gateway.com/v2beta1` API, which is where new tenants start.
Which release a capability arrived in is on [Features](features.md); the version selector at the top of the page switches between what each release shipped.

### How to read the ARC column

Every ARC cell renders one of two things, and the difference is what it rests on:

- a **verdict** (:material-check-circle:{ .gag-yes } or :material-close-circle:{ .gag-no }) always carries a stamp naming the chart version it was read at and the date it was read;
- **unverified** (:material-help-circle:{ .gag-unverified }) means we believe the claim and have not checked it.
  No verdict is being asserted, and the cell carries no stamp.

There is no third case.
A verdict without its stamp fails our own build, so the page cannot quietly go back to asserting things nobody measured.

ARC moves, and an undated comparison rots into a false one.
This page used to carry one blanket measurement note for the whole column, which made a checked claim and an assumed one look identical, and two rows went false at datable releases with nothing to notice it: 0.13.1 (2025-12-23) changed how a quota-blocked pod creation is retried, and 0.14.0 (2026-03-19) added multi-label scale sets, which GAG matched in Q726.
Under the per-cell stamp an aging claim degrades to unverified instead of to wrong.

!!! note "What the current stamps were read against"

    Every cell above was read against ARC `gha-runner-scale-set` **0.14.2**
    (released 2026-05-22), at commit
    [`9bb16ae`](https://github.com/actions/actions-runner-controller/tree/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8):
    controller source and chart values, not the documentation. The stamps carry
    two dates because there were two passes: the original column on
    **2026-08-12**, and the eight rows added on **2026-08-15**, when 0.14.2 was
    still the newest published chart. The tag is what
    the stamp names; the commit is what makes it re-checkable, since a link to a
    branch drifts out from under the claim it was cited for. Per-cell evidence,
    file by file, is in the [competitive analysis](plan/competitive-analysis-2026-08.md#per-cell-evidence-for-the-arc-column-2026-08-12).

    If you are evaluating on a later ARC than a cell names, re-check that cell
    rather than trusting it, and
    [tell us what changed](https://github.com/actions-gateway/github-actions-gateway/issues).

### Two rows with fine print

<!-- The canonical fires/doesn't-fire matrix is
     docs/operations/troubleshooting.md § Which Disruptions Auto-Re-Run a Job.
     When a case is added or removed there, update this paragraph too — and when
     a case changes which acquisition tier it reaches, which is the same drift
     and is what this paragraph missed for a whole release: the Pending-reap
     re-run read "classic tier" from Q691 until Q766 had made it both, in
     v1.4.0. Neither added nor removed a case, so the instruction above did not
     fire. -->
**Auto-re-run covers disruption, never failure.** Eviction, preemption, a drain and a stray `kubectl delete pod` all come back, on both acquisition tiers.
A job that *failed* and a run you *cancelled* never do.
Nor do workers the reaper took, with one exception: a worker reaped while still `Pending` is re-run on both tiers once capacity returns.
[The full boundary](operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do).

**Right-sizing is structural, not a feature race.** An ephemeral pod runs one job and lives minutes, so stock Vertical Pod Autoscaler cannot size it.
The loop only closes inside the controller that builds the pods.
[Appendix D.7](design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on).

## Where ARC is ahead

Some of it is capability, not only maturity.
Measured 2026-08-06.
Each gap below is tracked, and what closes it and when is on the [public roadmap](roadmap.md); the support entitlement is the one we do not plan to match.

- **A GitHub Support entitlement**, covering ARC installed via the official Helm charts, GitHub Enterprise Server 3.9 and later.
  GAG has none.
  Read the scope exclusions before relying on it: Kubernetes orchestration, policy application, and template customization are explicitly out of scope, which is much of what a multi-tenant platform team actually pages about.
- **`containerMode: kubernetes`**, which runs `container:` and `services:` steps as separate pods with a provisioned volume.
  GAG runs one worker pod per job, so that path is Docker-in-Docker (under Kata, unprivileged) rather than a non-privileged pod-per-step model.
- **GHES that is actually tested.** GAG serves GHES gateways and marks both of its GHES features untested against a real appliance.

The maturity gap is real too: ARC is GA and widely deployed, while GAG's v2 API has only just reached beta (`v2beta1`, its first stability contract).
That is why the v1 to v2 migration runs on a committed, documented schedule with a working [`gag-migrate`](operations/migration-v1-to-v2.md) tool.
The discipline is the "won't strand you" signal while the track record accumulates.

One thing is **not** a differentiator either way: both ride the same Public-Preview runner-scale-set protocol, through the same [`actions/scaleset`](https://github.com/actions/scaleset) client library.

## Secure by default

Built for shared clusters running other teams' code: the multi-tenant hardening ships as reconciled defaults, not a post-install project.

<div class="gag-pillars" markdown>
<div class="grid cards" markdown>

-   :material-shield-lock:{ .lg .middle } __Risk reduction__

    ---

    Untrusted job code is boxed in by default:

    - `baseline` Pod Security Admission per namespace
    - Default-deny network: DNS + own proxy only
    - App keys read-only; never in env, never cached
    - Or opt in to [no App key in the cluster at all](design/05-security.md#57-workload-identity-the-no-pem-delegation-model)
    - Controller writes confined to tenant namespaces
    - [Sharing a proxy across namespaces needs the owner's consent](operations/security-operations.md#sharing-an-egress-proxy-across-namespaces)

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
    - [Three validated worker templates](operations/runner-template-library.md), one `kubectl apply -k`

</div>
</div>

### Sandboxing is not the `runtimeClassName` field

Both GAG and ARC can set one.
The differentiator is the layer underneath.

**Kata bounds the kernel, not the pod network**, so cloud metadata still answers from inside the guest.
GAG's default-deny NetworkPolicies close that path.

**GAG's own CI runs this way**, building a `kind` cluster inside a worker pod with **zero** `privileged: true` ([how, and what Kata does not buy you](operations/kata-dind-workloads.md#what-kata-does-not-buy-you)).

**Not yet a claim about untrusted pull requests**, which need the egress work on the [roadmap](roadmap.md#in-progress--near-term).

Threat model and abuse-response playbooks: [Security](design/05-security.md), [Security operations](operations/security-operations.md).
