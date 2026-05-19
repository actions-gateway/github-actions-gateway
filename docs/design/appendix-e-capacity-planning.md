# Appendix E — Capacity Planning & RunnerGroup Design

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)

---

This appendix is a practical guide for operators and tenant teams deciding how to structure their `RunnerGroup`s, size their `maxListeners` counts, and plan for growth. The raw constraint numbers live in [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget) and [Appendix A](appendix-a-capacity-slos.md); this appendix explains how to reason about them in practice.

> **Protocol dependency.** The rate-limit analysis below assumes the adaptive listener model described in [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc) — specifically, that a session can be reused after `acquirejob` and that GitHub delivers jobs opportunistically to any registered session. Both behaviors must be confirmed during [Milestone 1](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14). If session reuse is not permitted, steady-state cost rises to one session per concurrent job rather than one per RunnerGroup. If delivery throttling is confirmed, a small warm standby pool may be needed. This appendix will be updated once Milestone 1 findings are in.

---

## E.1. The Three Binding Constraints

Every capacity decision is governed by three independent ceilings. Hitting any one of them limits throughput regardless of the others.

| Constraint | Steady-state cost | Peak cost | Where it comes from |
| --- | --- | --- | --- |
| GitHub App rate limit | 1 session per RunnerGroup (~72 req/hr each) | Up to `maxListeners` sessions per RunnerGroup | [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget): each active session polls `GET /message` ~72 times/hour against a 15,000/hour budget |
| AGC pod memory | ~60 KiB per active listener goroutine | Negligible at realistic listener counts | [Appendix A](appendix-a-capacity-slos.md): goroutine stack + HTTP buffer |
| Namespace ResourceQuota | — | Caps concurrent running worker pods | `ActionsGateway.spec.namespaceQuota` |

With the adaptive listener model, **the GitHub App rate limit is no longer a steady-state concern for most tenants.** One session per RunnerGroup means 10 RunnerGroups consume ~720 req/hour against a 15,000/hour budget — 5% utilization at rest. The rate limit becomes relevant only at sustained peak burst when many RunnerGroups are simultaneously at their `maxListeners` ceiling.

The key formulas:

```
Steady-state sessions   = number of RunnerGroups in the ActionsGateway
Peak sessions (worst case) = sum of maxListeners across all RunnerGroups
```

The `namespaceQuota` remains the binding constraint on how many jobs run concurrently — it is independent of listener count.

---

## E.2. What RunnerGroups Are (and Aren't) For

A `RunnerGroup` represents a **pool of listener goroutines sharing a common pod shape**. It is not a per-repo, per-team, or per-workflow construct.

GitHub routes jobs to a RunnerGroup by matching the job's `runs-on` labels against the RunnerGroup's `runnerLabels`. Any workflow in any repository with access to the GitHub App installation can target a RunnerGroup — repo boundaries are invisible to the routing layer.

This means:

- **Multiple repos → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple workflows → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple pod shapes → multiple RunnerGroups** (GPU count, memory, CPU profile, or special volumes differ).

---

## E.3. The Per-Workflow RunnerGroup Question

Because the steady-state rate-limit cost of a RunnerGroup is one session (~72 req/hour), adding a new RunnerGroup is genuinely cheap. This makes fine-grained RunnerGroup topologies much more practical than they would be under a fixed-session model.

The argument for per-workflow RunnerGroups is now strong:

- Each workflow gets the minimum GPU count it actually needs, eliminating over-provisioning at the pod level.
- Teams own their runner shapes independently without coordinating with other teams.
- Metrics are naturally scoped per workflow via the `runner_group` label.
- Adding a new RunnerGroup for a new test suite is a self-service config change with negligible operational cost.

The remaining constraints are practical rather than rate-limit-driven:

**`maxListeners` must be sized per RunnerGroup.** A RunnerGroup that receives large simultaneous job bursts needs a higher `maxListeners` ceiling to acquire them all within the 2-minute window. Misconfigured ceilings can cause missed acquisitions during bursts, not just queuing delays.

**Peak rate-limit consumption scales with RunnerGroup count × `maxListeners`.** At extreme scale — many RunnerGroups all bursting simultaneously — the peak session count can approach the installation budget. For most tenants this is not a practical concern; see [E.6](#e6-when-to-shard-across-installations) for shard triggers.

**Configuration overhead grows with RunnerGroup count.** Each RunnerGroup requires a pod shape definition, label assignment, and `maxListeners` tuning. At very high RunnerGroup counts this becomes a maintenance surface.

**The practical guidance:** per-workflow RunnerGroups are a reasonable default for teams with meaningfully different resource requirements between workflows. Consolidate by pod shape only when workflows are resource-identical — there is no longer a strong rate-limit reason to force consolidation.

---

## E.4. Sizing `maxListeners`

`RunnerGroup.maxListeners` caps the number of listener goroutines that can run concurrently during a burst. The AGC always maintains at least one listener; additional goroutines spawn as jobs arrive and shut down when the queue drains.

This field is a **burst ceiling, not a steady-state count.** Setting it higher than needed costs nothing at rest — idle listener goroutines do not exist. The cost of setting it too low is missed job acquisitions: if 20 jobs arrive simultaneously and `maxListeners` is 5, only 5 can be acquired in the first wave; the remaining 15 must wait for sessions to free up, potentially timing out the 2-minute acquisition window if the burst is sustained.

A practical starting approach:

1. **Estimate peak simultaneous job arrivals** for this RunnerGroup. A team that pushes to many PRs at once at the start of the day may see 20–30 jobs arrive in a few seconds; a team with staggered pipelines may never exceed 5.
2. **Set `maxListeners` to cover that peak** with a small margin (e.g. peak + 2–3).
3. **Monitor `actions_gateway_active_sessions`** relative to `maxListeners`. If it consistently hits the ceiling during burst periods and jobs are being cancelled for acquisition timeout, increase it. If it never exceeds 3–4, the default of 10 is more than sufficient.

For most RunnerGroups the default of 10 is the right starting point and requires no tuning.

---

## E.5. Multi-Repo Usage

A GitHub App installation is scoped to an organization or a specific set of repositories. Within that scope, all repos can target any RunnerGroup by label — there is no per-repo RunnerGroup configuration required.

```
Organization: my-org
  ├── repo-a  (workflow: runs-on: [self-hosted, gpu-2x])   ──┐
  ├── repo-b  (workflow: runs-on: [self-hosted, gpu-2x])   ──┤── same RunnerGroup
  └── repo-c  (workflow: runs-on: [self-hosted, gpu-2x])   ──┘
```

The only case that requires separate `ActionsGateway` CRs for repo-boundary reasons is when repos live in **different GitHub organizations** — because each org needs its own App installation, and each installation maps to exactly one `ActionsGateway` CR.

---

## E.6. When to Shard Across Installations

With the adaptive listener model, sharding is a much rarer need than under a fixed-session design. Shard to a new `ActionsGateway` CR (and therefore a new GitHub App installation) when:

- The `RateLimited` condition appears on any `RunnerGroup` during sustained peak load — the installation's 15,000 req/hour budget is being exhausted by simultaneous burst activity across many RunnerGroups.
- You need more than ~200 RunnerGroups in a single `ActionsGateway` (steady-state sessions approach the rate limit budget even at one session each).
- Repos in a different GitHub organization need access to the same Kubernetes tenant namespace.
- A team wants fully isolated credentials — a separate GitHub App installation with an independent rate-limit budget.

As a rough check: `number of RunnerGroups × 72 req/hr` should stay well below 15,000/hr at rest. At 200 RunnerGroups that is 14,400 req/hr — already tight, with no headroom for burst. Keep steady-state RunnerGroup count comfortably below 150 per installation to preserve burst headroom.

Each `ActionsGateway` CR requires its own namespace. If multiple shards are needed within a single team, the standard pattern is one namespace per installation:

```
team-a/                    ← namespace 1, ActionsGateway CR, GitHub App install 1
team-a-overflow/           ← namespace 2, ActionsGateway CR, GitHub App install 2
```

Label the RunnerGroups consistently across installations (`gpu-2x`, `gpu-8x`, etc.) and split workflows between them based on priority or throughput class. There is no cross-installation load balancing built into this system; job routing is determined solely by which repos are covered by each installation's scope.

---

## E.7. Per-Tenant vs. Per-Team Partitioning

The GMC's multi-tenant model provisions one `ActionsGateway` per namespace. Within an organization, two common partitioning patterns emerge:

**One gateway per team.** Each team owns a namespace and an `ActionsGateway` CR. Runner shapes, `maxListeners` counts, and quota are fully self-managed per team. This is the recommended default — it aligns operational ownership with the team boundary, gives each team an independent rate-limit budget, and eliminates cross-team coordination on RunnerGroup configuration.

**One gateway per environment (shared by multiple teams).** A single tenant namespace serves multiple teams, with RunnerGroups differentiated by label convention (e.g. `team-a-gpu-2x`, `team-b-gpu-4x`). This reduces total AGC instances and GitHub App installations at the cost of reintroducing the coordination the self-service model is designed to avoid. Use this pattern only when the number of teams is small and the platform team is comfortable arbitrating RunnerGroup configuration and quota allocation.

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

The AGC is a single-pod controller that holds all listener goroutine state in memory. It cannot be horizontally scaled to multiple replicas without distributing that state — a significant complexity increase that is not in scope for this design. The scaling levers available to operators are vertical scaling, optional VPA right-sizing, and sharding across multiple `ActionsGateway` CRs.

**Vertical scaling (manual).** The primary tuning surface is the AGC pod's memory limit. The working memory consumed by listener goroutines at peak burst is:

```
peak_goroutine_memory ≈ sum(maxListeners across all RunnerGroups) × 60 KiB
```

For example, 50 RunnerGroups each with `maxListeners: 10` → 500 concurrent goroutines at peak → ~30 MiB of goroutine working set. The 2 GiB default memory request (see [Appendix A](appendix-a-capacity-slos.md)) is deliberately generous to absorb Go runtime overhead, heap churn during reconcile storms, and headroom for growth. If an operator adds many RunnerGroups with high `maxListeners` values and begins observing `container OOMKilled` events or high GC pressure (visible via `go_gc_duration_seconds` in Prometheus), increasing the AGC pod's `resources.limits.memory` is the correct first response.

CPU consumption is predominantly I/O-bound — goroutines spend nearly all of their time blocked on `GET /message` long-polls. CPU pressure appears only during reconcile churn (many RunnerGroups being created or deleted simultaneously) or during a token refresh storm. The 2-core CPU limit default is sufficient for most deployments; increase it only if `container_cpu_throttled_seconds_total` shows sustained throttling during peak reconcile activity.

**Optional VPA right-sizing.** A [Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) in `Auto` mode will observe the AGC's actual CPU and memory usage over time and adjust its resource requests automatically. This is useful for operators who want the AGC to self-tune rather than set limits manually, especially in early-production environments where workload shape is still stabilizing. The AGC handles VPA-initiated restarts gracefully: when killed, in-flight listener goroutines deregister their sessions via `DELETE /sessions`, and the AGC re-registers them within GitHub's 2-minute redelivery window on restart (see [§4.2](04-operational-flows.md#42-job-execution-flow-agc) and the `SessionReacquisition` SLO in [Appendix A](appendix-a-capacity-slos.md)).

> **Note:** No `agcResources` field is currently defined on `ActionsGatewaySpec` for tenant-controlled AGC resource overrides. If tenant teams consistently need different AGC sizing, consider adding this field in a future revision. For now, the platform team manages AGC resource limits as part of the Helm chart or Kustomize overlay.

**Horizontal sharding.** When the number of RunnerGroups or their aggregate `maxListeners` exceeds what a single vertically-scaled AGC pod can comfortably handle — or when rate-limit pressure appears (see [E.6](#e6-when-to-shard-across-installations)) — the correct scale path is to shard into a second `ActionsGateway` CR in a new namespace with a separate GitHub App installation. Each shard has its own AGC pod, its own rate-limit budget, and its own independent listener goroutine pool. See [E.6](#e6-when-to-shard-across-installations) for shard triggers and the standard namespace partitioning pattern.

---

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)
