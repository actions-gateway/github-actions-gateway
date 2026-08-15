# Appendix E — Capacity Planning & RunnerGroup Design

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)

---

This appendix is a practical guide for operators and tenant teams deciding how to structure their `RunnerGroup`s, size their `maxListeners` counts, and plan for growth.
The raw constraint numbers live in [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget) and [Appendix A](appendix-a-capacity-slos.md); this appendix explains how to reason about them in practice.

> **Milestone 1 protocol findings** (see [docs/plan/milestone-1.md §8](../plan/milestone-1.md#8-investigation-findings)):
>
> *Session reuse confirmed* (Investigation C) — a session remains live after `acquirejob`; goroutines loop without a delete→create cycle.
> The steady-state cost remains **one session per RunnerGroup**.
>
> *One session per registered runner agent enforced* (Investigation D) — `POST /sessions` returns 409 if the agentId already has a session.
> Each concurrent listener goroutine requires a distinct pre-registered agent.
> The AGC must provision up to `maxListeners` agents per RunnerGroup at setup time.
> This does not change the rate-limit math (one active session per agent, same session-count formula), but it adds an agent-registration step to RunnerGroup provisioning — see [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc).
>
> *Opportunistic delivery supported* (inferred from Investigation C timing) — a newly dispatched job arrived in `GetMessage` within ~1 second of dispatch, consistent with delivery to any active polling session.
> No warm standby pool is needed.

---

## E.1. The Three Binding Constraints

Every capacity decision is governed by three independent ceilings.
Hitting any one of them limits throughput regardless of the others.

| Constraint | Steady-state cost | Peak cost | Where it comes from |
| --- | --- | --- | --- |
| GitHub App rate limit | 1 session per RunnerGroup (~72 req/hr each) | Up to `maxListeners` sessions per RunnerGroup | [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget): each active session polls `GET /message` ~72 times/hour against a 15,000/hour budget |
| AGC pod memory | ~60 KiB per active listener goroutine | Negligible at realistic listener counts | [Appendix A](appendix-a-capacity-slos.md): goroutine stack + HTTP buffer |
| Namespace ResourceQuota | — | Caps concurrent running worker pods | Platform-owned `ResourceQuota` on the tenant namespace (not a CR field — see [tenant-onboarding](../operations/tenant-onboarding.md)) |

With the adaptive listener model, **the GitHub App rate limit is no longer a steady-state concern for most tenants.** One session per RunnerGroup means 10 RunnerGroups consume ~720 req/hour against a 15,000/hour budget — 5% utilization at rest.
The rate limit becomes relevant only at sustained peak burst when many RunnerGroups are simultaneously at their `maxListeners` ceiling.

The key formulas:

```
Steady-state sessions   = number of RunnerGroups in the ActionsGateway
Peak sessions (worst case) = sum of maxListeners across all RunnerGroups
```

The platform-owned namespace `ResourceQuota` remains the binding constraint on how many jobs run concurrently — it is independent of listener count.
It is set on the namespace by the platform admin (or your GitOps / tenant-operator stack), not on the `ActionsGateway` CR.
For the arithmetic that turns runner shapes and `maxWorkers` into the actual quota numbers — including the native sidecars, Kata `RuntimeClass` overhead, and per-worker PVCs that the per-container asks alone do not account for — see [sizing the platform-owned `ResourceQuota`](../operations/resourcequota-sizing.md).

---

## E.2. What RunnerGroups Are (and Aren't) For

A `RunnerGroup` represents a **pool of listener goroutines sharing a common pod shape**.
It is not a per-repo, per-team, or per-workflow construct.

GitHub routes jobs to a RunnerGroup by matching the job's `runs-on` labels against the RunnerGroup's `runnerLabels`.
Any workflow in any repository with access to the GitHub App installation can target a RunnerGroup — repo boundaries are invisible to the routing layer.

This means:

- **Multiple repos → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple workflows → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple pod shapes → multiple RunnerGroups** (GPU count, memory, CPU profile, or special volumes differ).

---

## E.3. The Per-Workflow RunnerGroup Question

Because the steady-state rate-limit cost of a RunnerGroup is one session (~72 req/hour), adding a new RunnerGroup is genuinely cheap.
This makes fine-grained RunnerGroup topologies much more practical than they would be under a fixed-session model.

The argument for per-workflow RunnerGroups is now strong:

- Each workflow gets the minimum GPU count it actually needs, eliminating over-provisioning at the pod level.
- Teams own their runner shapes independently without coordinating with other teams.
- Metrics are naturally scoped per workflow via the `runner_group` label.
- Adding a new RunnerGroup for a new test suite is a self-service config change with negligible operational cost.

The remaining constraints are practical rather than rate-limit-driven:

**`maxListeners` must be sized per RunnerGroup.** A RunnerGroup that receives large simultaneous job bursts needs a higher `maxListeners` ceiling to acquire them all within the 2-minute window.
Misconfigured ceilings can cause missed acquisitions during bursts, not just queuing delays.

**Peak rate-limit consumption scales with RunnerGroup count × `maxListeners`.** At extreme scale — many RunnerGroups all bursting simultaneously — the peak session count can approach the installation budget.
For most tenants this is not a practical concern; see [E.6](#e6-when-to-shard-across-installations) for shard triggers.

**Configuration overhead grows with RunnerGroup count.** Each RunnerGroup requires a pod shape definition, label assignment, and `maxListeners` tuning.
At very high RunnerGroup counts this becomes a maintenance surface.

**The practical guidance:** per-workflow RunnerGroups are a reasonable default for teams with meaningfully different resource requirements between workflows.
Consolidate by pod shape only when workflows are resource-identical — there is no longer a strong rate-limit reason to force consolidation.

---

## E.4. Sizing `maxListeners`

`RunnerGroup.maxListeners` caps the number of listener goroutines that can run concurrently during a burst.
The AGC always maintains at least one listener; additional goroutines spawn as jobs arrive and shut down when the queue drains.

This field is a **burst ceiling, not a steady-state count.** Setting it higher than needed costs nothing at rest — idle listener goroutines do not exist.
The cost of setting it too low is missed job acquisitions: if 20 jobs arrive simultaneously and `maxListeners` is 5, only 5 can be acquired in the first wave; the remaining 15 must wait for sessions to free up, potentially timing out the 2-minute acquisition window if the burst is sustained.

A practical starting approach:

1. **Estimate peak simultaneous job arrivals** for this RunnerGroup.
   A team that pushes to many PRs at once at the start of the day may see 20–30 jobs arrive in a few seconds; a team with staggered pipelines may never exceed 5.
2. **Set `maxListeners` to cover that peak** with a small margin (e.g. peak + 2–3).
3. **Monitor `actions_gateway_active_sessions`** relative to `maxListeners`.
   If it consistently hits the ceiling during burst periods and jobs are being cancelled for acquisition timeout, increase it.
   If it never exceeds 3–4, the default of 10 is more than sufficient.

For most RunnerGroups the default of 10 is the right starting point and requires no tuning.

---

## E.5. Multi-Repo Usage

A GitHub App installation is scoped to an organization or a specific set of repositories.
Within that scope, all repos can target any RunnerGroup by label — there is no per-repo RunnerGroup configuration required.

```
Organization: my-org
  ├── repo-a  (workflow: runs-on: [self-hosted, gpu-2x])   ──┐
  ├── repo-b  (workflow: runs-on: [self-hosted, gpu-2x])   ──┤── same RunnerGroup
  └── repo-c  (workflow: runs-on: [self-hosted, gpu-2x])   ──┘
```

The only case that requires separate `ActionsGateway` CRs for repo-boundary reasons is when repos live in **different GitHub organizations** — because each org needs its own App installation, and each installation maps to exactly one `ActionsGateway` CR.

---

## E.6. When to Shard Across Installations

With the adaptive listener model, sharding is a much rarer need than under a fixed-session design.
Shard to a new `ActionsGateway` CR (and therefore a new GitHub App installation) when:

- The `RateLimited` condition appears on any `RunnerGroup` during sustained peak load — the installation's 15,000 req/hour budget is being exhausted by simultaneous burst activity across many RunnerGroups.
- You need more than ~200 RunnerGroups in a single `ActionsGateway` (steady-state sessions approach the rate limit budget even at one session each).
- Repos in a different GitHub organization need access to the same Kubernetes tenant namespace.
- A team wants fully isolated credentials — a separate GitHub App installation with an independent rate-limit budget.

As a rough check: `number of RunnerGroups × 72 req/hr` should stay well below 15,000/hr at rest.
At 200 RunnerGroups that is 14,400 req/hr — already tight, with no headroom for burst.
Keep steady-state RunnerGroup count comfortably below 150 per installation to preserve burst headroom.

Each `ActionsGateway` CR requires its own namespace.
If multiple shards are needed within a single team, the standard pattern is one namespace per installation:

```
team-a/                    ← namespace 1, ActionsGateway CR, GitHub App install 1
team-a-overflow/           ← namespace 2, ActionsGateway CR, GitHub App install 2
```

Label the RunnerGroups consistently across installations (`gpu-2x`, `gpu-8x`, etc.) and split workflows between them based on priority or throughput class.

> **v2 `ScaleSet` sets shard differently.** The advice above holds for `Classic` acquisition, where a label is only a match target.
> Under the `ScaleSet` protocol a set's **first** `runnerLabel` is its scale-set *name* at GitHub, and that name is unique per org, enterprise, or repo rather than per namespace.
> Two shards of one org therefore cannot reuse a first label, however far apart their namespaces are: they would drive one scale set from two controllers, each shard acquiring the other's jobs.
> Admission rejects it (Q791).
> Give each shard a distinct first label (`gpu-2x-a`, `gpu-2x-b`) and put the shared class in a later label, which may overlap freely.
> There is no cross-installation load balancing built into this system; job routing is determined solely by which repos are covered by each installation's scope.

---

## E.7. Per-Tenant vs. Per-Team Partitioning

The Gateway Manager Controller (GMC) multi-tenant model provisions one `ActionsGateway` per namespace.
Within an organization, two common partitioning patterns emerge:

**One gateway per team.** Each team owns a namespace and an `ActionsGateway` CR.
Runner shapes, `maxListeners` counts, and quota are fully self-managed per team.
This is the recommended default — it aligns operational ownership with the team boundary, gives each team an independent rate-limit budget, and eliminates cross-team coordination on RunnerGroup configuration.

**One gateway per environment (shared by multiple teams).** A single tenant namespace serves multiple teams, with RunnerGroups differentiated by label convention (e.g.
`team-a-gpu-2x`, `team-b-gpu-4x`).
This reduces total AGC instances and GitHub App installations at the cost of reintroducing the coordination the self-service model is designed to avoid.
Use this pattern only when the number of teams is small and the platform team is comfortable arbitrating RunnerGroup configuration and quota allocation.

---

## E.8. Decision Guide

```
New runner requirement arriving:
│
├─ Does an existing RunnerGroup have the same GPU count, memory,
│  and tooling requirements?
│   ├─ Yes → Target the existing RunnerGroup's label from the workflow.
│   │         No new RunnerGroup needed.
│   └─ No  → Create a new RunnerGroup with the appropriate pod shape
│             and set maxListeners to cover the expected burst size.
│             Check that total steady-state RunnerGroup count across
│             the ActionsGateway stays below ~150.
│
├─ Are simultaneous job bursts being lost (acquisition timeout)?
│   ├─ No  → Default maxListeners (10) is sufficient.
│   └─ Yes → Increase maxListeners on the affected RunnerGroup to
│             cover the observed peak simultaneous arrival rate.
│
├─ Is the RateLimited condition appearing during peak periods?
│   ├─ No  → No action needed.
│   └─ Yes → Either reduce RunnerGroup count, reduce maxListeners on
│             high-burst groups, or shard to a second ActionsGateway
│             CR with a separate GitHub App installation.
│
└─ Are the repos in a different GitHub organization?
    ├─ No  → Same ActionsGateway CR can serve them all.
    └─ Yes → Separate ActionsGateway CR required (separate installation).
```

---

## E.9. Scaling the AGC Itself

The AGC is a single-pod controller that holds all listener goroutine state in memory.
It cannot be horizontally scaled to multiple replicas without distributing that state — a significant complexity increase that is not in scope for this design.
The scaling levers available to operators are vertical scaling, optional VPA right-sizing, and sharding across multiple `ActionsGateway` CRs.

**Vertical scaling (manual).** The primary tuning surface is the AGC pod's resource footprint.
The working memory consumed by listener goroutines at peak burst is:

```
peak_goroutine_memory ≈ sum(maxListeners across all RunnerGroups) × 60 KiB
```

For example, 50 RunnerGroups each with `maxListeners: 10` → 500 concurrent goroutines at peak → ~30 MiB of goroutine working set.
The recommended sizing (see [Appendix A](appendix-a-capacity-slos.md)) is a `2Gi` memory **request** with a `4Gi` memory **limit** — the request is a generous scheduling reservation absorbing Go runtime overhead and heap churn during reconcile storms, and the limit sits well above the working set so transient bursts don't OOMKill the single-pod control plane while a true runaway is still bounded.
On the v1 API the GMC stamps no resources on the AGC pod, so the platform applies this sizing on the AGC Deployment through its Helm/Kustomize overlay (or a namespace `LimitRange` that supplies default requests).
If an operator adds many RunnerGroups with high `maxListeners` values and begins observing `container OOMKilled` events or high GC pressure (visible via `go_gc_duration_seconds` in Prometheus), raising the AGC Deployment's `resources.limits.memory` is the correct first response.

CPU consumption is predominantly I/O-bound — goroutines spend nearly all of their time blocked on `GET /message` long-polls.
CPU pressure appears only during reconcile churn (many RunnerGroups being created or deleted simultaneously) or during a token refresh storm.
The recommended `2`-core CPU **limit** is sufficient for most deployments; raise the AGC Deployment's `resources.limits.cpu` only if `container_cpu_throttled_seconds_total` shows sustained throttling during peak reconcile activity.

**Optional VPA right-sizing.** A [Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) (VPA) observes the AGC's actual CPU and memory usage over time and adjusts its resource requests automatically.
This is useful for operators who want the AGC to self-tune rather than set limits manually, especially in early-production environments where workload shape is still stabilizing.
The AGC handles VPA-initiated restarts gracefully: when killed, in-flight listener goroutines deregister their sessions via `DELETE /sessions`, and the AGC re-registers them within GitHub's 2-minute redelivery window on restart (see [§4.2](04-operational-flows.md#42-job-execution-flow-agc) and the `SessionReacquisition` SLO in [Appendix A](appendix-a-capacity-slos.md)).

Under v2 this is a first-class opt-in rather than something the operator hand-rolls: see [E.11](#e11-managed-vertical-right-sizing-of-the-control-planes) for the two managed opt-ins the system ships, the precedence rules against `agcResources`, and what happens on a cluster where the VPA CRDs are not installed.

> **v2 promotes AGC sizing to a CR field — `spec.agcResources` (Q171).** This appendix describes the v1 API (`actions-gateway.github.com/v1alpha1`), where AGC sizing is platform-applied on the Deployment as above.
> The v2 `ActionsGateway` (`actions-gateway.com/v2alpha1`) instead exposes an optional `agcResources` field of the standard `corev1.ResourceRequirements` shape for tenant-controlled per-gateway sizing, and makes the [Appendix A](appendix-a-capacity-slos.md) sizing (requests `cpu: 500m` / `memory: 2Gi`, limits `cpu: 2` / `memory: 4Gi`) the GMC-stamped default: the override is additive per key, so setting one knob keeps the default for the rest and omitting the field reproduces the default unchanged.
> See [Appendix H](appendix-h-v2-api-decomposition.md) for the v2 decomposition and [tenant-onboarding](../operations/tenant-onboarding.md#tuning-agc-control-plane-resources) for the operator-facing how-to and the recommended floor.

**Horizontal sharding.** When the number of RunnerGroups or their aggregate `maxListeners` exceeds what a single vertically-scaled AGC pod can comfortably handle — or when rate-limit pressure appears (see [E.6](#e6-when-to-shard-across-installations)) — the correct scale path is to shard into a second `ActionsGateway` CR in a new namespace with a separate GitHub App installation.
Each shard has its own AGC pod, its own rate-limit budget, and its own independent listener goroutine pool.
See [E.6](#e6-when-to-shard-across-installations) for shard triggers and the standard namespace partitioning pattern.

---

## E.10. Worked Examples

The following concrete scenarios show how to apply the formulas and decision guide above to real configurations.

### Scenario 1: Team with 3 RunnerGroups and 20 Concurrent GPU Jobs at Peak

**Context.** A machine learning team runs three workload shapes: model training (8-GPU pods, up to 10 concurrent), model evaluation (2-GPU pods, up to 20 concurrent), and CPU-based preprocessing (no GPU, up to 50 concurrent).

**Configuration.**

The namespace quota is platform-owned — the platform admin applies it on the namespace, sized to the maximum simultaneous pod count:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: ml-team-quota
  namespace: ml-team
spec:
  hard:
    requests.cpu: "80"
    requests.memory: "320Gi"
    pods: "85"           # 10 + 20 + 50 + 5 headroom
```

The tenant's `ActionsGateway` then declares the runner groups (no quota field).
An embedded `runnerGroups[]` entry has no `name` field — the GMC derives each RunnerGroup's name from its first `runnerLabels` entry, so keep the distinguishing label first to give every group a distinct name:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: ml-team-gateway
  namespace: ml-team
spec:
  gitHubAppRef:
    name: github-app
  gitHubURL: https://github.com/my-org
  runnerGroups:
    - runnerLabels: ["gpu-8x", "self-hosted"]   # → RunnerGroup ml-team-gateway-gpu-8x
      maxListeners: 12   # peak 10, +2 margin
      maxWorkers: 10
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                limits:
                  nvidia.com/gpu: "8"

    - runnerLabels: ["gpu-2x", "self-hosted"]   # → RunnerGroup ml-team-gateway-gpu-2x
      maxListeners: 22   # peak 20, +2 margin
      priorityTiers:
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
                  nvidia.com/gpu: "2"

    - runnerLabels: ["cpu-preprocess", "self-hosted"]   # → RunnerGroup ml-team-gateway-cpu-preprocess
      maxListeners: 10   # rarely bursts, default is fine
      maxWorkers: 50
```

**Rate-limit check.** Steady-state: 3 sessions × 72 req/hr = 216 req/hr — negligible against the 15,000/hr budget.
Peak burst: (12 + 22 + 10) = 44 sessions × 72 req/hr = 3,168 req/hr — well within budget.
No sharding needed.

**Peak goroutine memory.** 44 concurrent goroutines × 60 KiB ≈ 2.6 MiB — trivial.

**Namespace quota.** Sized to the maximum simultaneous pod count (10 + 20 + 50 = 80) plus 5 AGC/proxy headroom.
The `requests.cpu` and `requests.memory` fields must sum to cover the worker pod requests for all concurrent jobs.

---

### Scenario 2: CPU-Only Team, 100 Jobs/Day, Bursty (Up to 10 Concurrent)

**Context.** A backend team runs integration tests.
Jobs arrive in bursts at the start of PRs — up to 10 jobs may land simultaneously — but total daily volume is modest.

**Configuration.** Minimal; no GPU, no priority tiers.
The platform admin owns the namespace quota:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: backend-team-quota
  namespace: backend-team
spec:
  hard:
    requests.cpu: "20"
    requests.memory: "40Gi"
    pods: "15"           # 10 workers + 3 proxy + 1 AGC + 1 headroom
```

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: backend-team-gateway
  namespace: backend-team
spec:
  gitHubAppRef:
    name: github-app
  gitHubURL: https://github.com/my-org
  runnerGroups:
    - runnerLabels: ["linux", "self-hosted"]   # → RunnerGroup backend-team-gateway-linux
      maxListeners: 12   # peak burst 10, +2 margin
      maxWorkers: 10
      podTemplate:
        spec:
          containers:
            - name: runner
              resources:
                requests:
                  cpu: "2"
                  memory: "4Gi"
```

**Rate-limit check.** 1 session × 72 req/hr = 72 req/hr at rest.
Burst: 12 × 72 = 864 req/hr — well within budget.

**Note on simplicity.** Because there is no `priorityTiers`, no `PriorityClass` objects need to be pre-created.
`maxWorkers` alone is sufficient for a flat concurrency ceiling.
This is the recommended starting point for teams that do not have competing priority requirements.

---

### Scenario 3: Large Tenant Hitting the 250-Session Ceiling — Sharding Walkthrough

**Context.** A platform team provides shared runners across 200 repositories in the same organization.
They have 180 RunnerGroups (one per workflow shape), each with `maxListeners: 10`.
At peak load, 60 RunnerGroups are simultaneously bursting.

**Problem.** The burst peak is 60 RunnerGroups × 10 sessions × 72 req/hr = 43,200 req/hr — nearly 3× the 15,000/hr installation budget.
The `RateLimited` condition appears during business hours.

**Steady-state check.** 180 RunnerGroups × 72 req/hr = 12,960 req/hr.
Even at rest, this is 86% of the budget — no headroom for any burst.

**Solution: shard to two ActionsGateway CRs.**

```
namespace: platform-runners-1   → GitHub App installation 1
  RunnerGroups: 90 (the first half)

namespace: platform-runners-2   → GitHub App installation 2
  RunnerGroups: 90 (the second half)
```

Split strategy: partition RunnerGroups by workflow class.
If model-training workflows always burst together, put them in the same shard so their burst pressure is contained within one budget.
If CPU and GPU workflows burst independently, split them across shards to spread peak load.

After sharding, each installation has 90 × 72 = 6,480 req/hr at rest — 43% of the 15,000/hr budget with headroom for burst.

**Steps.**

1. Create a second GitHub App installation in the same organization.
2. Apply a second `ActionsGateway` CR in a new namespace (`platform-runners-2`) referencing the new installation's Secret.
3. Move 90 RunnerGroup definitions from the first CR to the second.
4. Update workflow files to target the correct label sets.
5. Confirm `RateLimited` condition clears on both CRs.

---

## E.11. Managed Vertical Right-Sizing of the Control Planes

Both control planes — the cluster-wide Gateway Manager Controller (GMC) and each per-tenant AGC — are single-shape, long-lived workloads whose correct resource footprint is exactly the thing an operator cannot know in advance.
Manual sizing is therefore either wasteful (a 2Gi request for a 60 MiB working set) or dangerous (a limit below the working set OOMKills a single-pod control plane).
Q360 ships two opt-ins that hand that judgement to a Vertical Pod Autoscaler instead.

Both are **opt-in and off by default**, and both default to recommendation-only, so turning them on is observable before it is disruptive.

| Surface | Opt-in | Target |
| --- | --- | --- |
| GMC (cluster-wide) | Helm value `vpa.enabled: true` | the `*-controller-manager` Deployment |
| AGC (per tenant) | `ActionsGateway.spec.agcAutoscaling` | that gateway's `<gw>-agc` Deployment |

The per-gateway opt-in is the presence of the block; there is no `enabled` flag, so removing `agcAutoscaling` deletes the managed `VerticalPodAutoscaler` again.
`spec.agcAutoscaling.mode` is the autoscaler's `updateMode`, restricted to three values:

| `mode` | Behavior |
| --- | --- |
| `Off` (default) | The autoscaler publishes a recommendation in its own status and never mutates a pod. |
| `Initial` | The recommendation is applied at pod creation only. A running AGC is never evicted by the autoscaler. |
| `Recreate` | The autoscaler may evict the AGC pod to apply a new recommendation — safe per the restart analysis in [E.9](#e9-scaling-the-agc-itself), but still a control-plane restart, so it is opt-in. |

Upstream's fourth value, `Auto`, is deliberately **not** exposed.
It is an alias whose actuation mechanism is version-dependent (today it evicts like `Recreate`; in-place pod resize is still alpha), so a gateway pinned to `Auto` would silently change its restart behavior on a VPA upgrade.
Naming `Recreate` makes the eviction explicit.

### E.11.1. Precedence against `agcResources`

Both `agcResources` and the autoscaler influence the AGC container's resources, so the division is fixed and explicit rather than last-writer-wins.
**They compose; they do not compete.**

- **`agcResources` alone decides what the GMC stamps on the Deployment.** That is unchanged by the opt-in, and it is the sizing actually in effect whenever the autoscaler is not actuating: `mode: Off`, the VPA components absent or down, or before the first recommendation exists.
- **The managed autoscaler is pinned to `controlledValues: RequestsOnly`.** It moves *requests* only; the limits from `agcResources` (or the platform default) are never raised or lowered by it.
  On a single-pod control plane, an autoscaler that also drifted the memory *limit* would erode the OOM ceiling that bounds a runaway — the property the limit exists for.
- **A request the tenant explicitly set becomes the autoscaler's `minAllowed`.** An explicit floor is honored as a floor rather than silently overwritten.
  Leave `agcResources.requests` unset to let the autoscaler size the AGC all the way down to its own global floor — that is the configuration where right-sizing has the most to win.
- **The effective limits become `maxAllowed`.** This is a correctness requirement, not a preference: a container whose request exceeds its own limit is rejected by the apiserver, so the autoscaler must never recommend above the stamped limit.
  It is also the pairing upstream requires — `RequestsOnly` on a container that *has* limits is only safe with a `maxAllowed` at most equal to them — which is why the ceiling is always emitted rather than optional.

**Resolved at reconcile, not at admission.** Setting both fields is a coherent configuration (sizing plus bounds), not a contradiction, so rejecting the combination at admission would remove a useful and idiomatic way to configure a VPA.
What admission-time rejection buys — "no silent surprises" — is bought instead by making the resolution visible: the derived bounds are on the stamped `VerticalPodAutoscaler` object, the GMC emits a Normal Event naming them when the autoscaler first appears, and the `AGCAutoscalingUnavailable` condition states which knob owns what.
The chart-level opt-in follows the same rules against the chart's `resources` value.

### E.11.2. When the VPA CRDs are absent

The `autoscaling.k8s.io` CRDs are not part of core Kubernetes, so an opt-in can be unsatisfiable through no fault of the CR.
The GMC probes its RESTMapper for the `VerticalPodAutoscaler` kind and, when it is missing:

- **provisions the gateway normally** — the AGC is created with its `agcResources` sizing and reaches `Ready=True`.
  A missing optional right-sizing prerequisite is not a reason to withhold a tenant's runners;
- **does not fail or retry the write** — nothing is applied against a kind the apiserver does not serve, so there is no error to hot-loop on;
- **surfaces it** — the advisory `AGCAutoscalingUnavailable=True` condition (reason `VPACRDNotInstalled`, naming the remediation) plus one Warning Event per transition.
  The condition is advisory: it never gates `Ready`, exactly like `EgressUnattributed`.
  Like every other v2 gateway condition it also exports a Prometheus gauge, `actions_gateway_agc_autoscaling_unavailable` (Q390), so the unsatisfiable opt-in is alertable rather than visible only via `kubectl describe`;
- **converges on its own** — the gateway re-probes every 10 minutes, so installing the vertical-pod-autoscaler later takes effect without an operator edit.
  Ten minutes is a poll, not a hot loop: one cached-discovery lookup per gateway per interval.

The chart-level opt-in has no equivalent runtime path — Helm renders the object at install time, so `vpa.enabled: true` without the CRDs fails `helm install` loudly, the same prerequisite contract as `metrics.serviceMonitor.enabled`.

---

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)
