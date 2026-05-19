# Appendix E — Capacity Planning & RunnerGroup Design

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)

---

This appendix is a practical guide for operators and tenant teams deciding how to structure their `RunnerGroup`s, size their replica counts, and plan for growth. The raw constraint numbers live in [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget) and [Appendix A](appendix-a-capacity-slos.md); this appendix explains how to reason about them in practice.

---

## E.1. The Three Binding Constraints

Every capacity decision is governed by three independent ceilings. Hitting any one of them limits throughput regardless of the others.

| Constraint | Ceiling | Where it comes from |
| --- | --- | --- |
| GitHub App rate limit | ~250 concurrent sessions per installation | [§3.5](03-api-contracts.md#35-github-api-rate-limit-budget): each idle session polls `GET /message` ~72 times/hour against a 15,000/hour budget |
| AGC pod memory | ~1,000 sessions per AGC pod | [Appendix A](appendix-a-capacity-slos.md): ~60 KiB per goroutine; 1,000 sessions ≈ 60 MiB working set |
| Namespace ResourceQuota | Operator-configured | `ActionsGateway.spec.namespaceQuota` caps aggregate worker pod resources and pod count |

In practice the **GitHub App rate limit is almost always the binding constraint** at the per-installation level. The AGC memory budget allows 4× more sessions than the rate limit permits, so the rate limit bites first. The `namespaceQuota` is the binding constraint on how many jobs can run concurrently — it is a separate ceiling from how many sessions can wait for work.

The key formula:

```
total concurrent sessions = sum of (RunnerGroup.replicas) across all RunnerGroups in the ActionsGateway
```

Keep this sum at or below 250 per `ActionsGateway` CR to stay within the rate limit budget.

---

## E.2. What RunnerGroups Are (and Aren't) For

A `RunnerGroup` represents a **pool of virtual runner sessions sharing a common pod shape**. It is not a per-repo, per-team, or per-workflow construct.

GitHub routes jobs to a RunnerGroup by matching the job's `runs-on` labels against the RunnerGroup's `runnerLabels`. Any workflow in any repository with access to the GitHub App installation can target a RunnerGroup — repo boundaries are invisible to the routing layer.

This means:

- **Multiple repos → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple workflows → one RunnerGroup** (if they share the same pod shape and labels).
- **Multiple pod shapes → multiple RunnerGroups** (GPU count, memory, CPU profile, or special volumes differ).

---

## E.3. The Per-Workflow RunnerGroup Question

Because virtual runner sessions are goroutines rather than pods, the marginal cost of adding a new RunnerGroup is low: a small amount of configuration and a handful of additional goroutines in the AGC. This raises a natural question: should each workflow get its own RunnerGroup so that its pod shape can be optimized exactly?

The argument for it is real:

- Each workflow gets the minimum GPU count it actually needs, eliminating over-provisioning at the pod level.
- Teams own their runner shapes independently without coordinating with other teams.
- Metrics are naturally scoped per workflow via the `runner_group` label.

The argument against is also real, and it is rooted in the rate limit ceiling:

**Each additional RunnerGroup consumes sessions from the 250-session budget.** A RunnerGroup with `replicas: 5` consumes 5 of those 250 sessions even when it has no jobs running. Multiply across many fine-grained RunnerGroups and the budget fills quickly:

| RunnerGroups | Replicas each | Total sessions | Headroom remaining |
| --- | --- | --- | --- |
| 10 | 5 | 50 | 200 |
| 25 | 5 | 125 | 125 |
| 50 | 5 | 250 | 0 — at ceiling |
| 50 | 10 | 500 | Over — must shard |

A tenant with 50 distinct workflow types, each wanting 5 concurrent slots, is already at the ceiling. Every RunnerGroup beyond that point requires sharding to a new GitHub App installation — which means a new `ActionsGateway` CR, new credentials, and new operational overhead. The per-workflow RunnerGroup model trades away the budget headroom that would otherwise absorb growth.

**The practical guidance:** prefer per-pod-shape RunnerGroups rather than per-workflow. Workflows that happen to need the same GPU count, memory, and tooling can share a RunnerGroup freely — GitHub's label routing handles the targeting. Reserve finer granularity for cases where the resource difference is real and meaningful (a 1-GPU training job genuinely cannot share a pod shape with an 8-GPU job).

---

## E.4. Choosing Replica Counts

`RunnerGroup.replicas` controls how many goroutines stand ready to accept jobs at any moment. A job that arrives when all sessions are busy must wait for a session to free up before it can be acquired.

Sizing replicas is a throughput problem, not a resource problem — goroutines are cheap. The goal is to avoid jobs queueing for a session when the cluster has spare capacity to run them.

A practical starting approach:

1. **Measure peak concurrent jobs** for this runner shape across a representative week of CI traffic.
2. **Set replicas to peak + 20% headroom** to absorb spikes without session starvation.
3. **Monitor `actions_gateway_active_sessions`** relative to `replicas`. If it consistently hits the ceiling during working hours, increase replicas. If it rarely exceeds 30% of replicas, decrease them to recover session budget.

Keep in mind that replicas count against the 250-session installation budget regardless of whether jobs are running. Under-provisioning wastes job throughput; over-provisioning wastes session budget.

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

Shard to a new `ActionsGateway` CR (and therefore a new GitHub App installation) when:

- The `RateLimited` condition appears on any `RunnerGroup` for more than a few minutes — the installation is over budget.
- You need more than ~250 total concurrent sessions within a single tenant namespace.
- Repos in a different GitHub organization need to share the same Kubernetes tenant namespace.
- A team wants fully isolated credentials — a separate GitHub App installation with no shared rate-limit budget.

Each `ActionsGateway` CR requires its own namespace. If multiple shards are needed within a single team, the standard pattern is one namespace per installation:

```
team-a/                    ← namespace 1, ActionsGateway CR, GitHub App install 1
team-a-overflow/           ← namespace 2, ActionsGateway CR, GitHub App install 2
```

Label the RunnerGroups consistently across installations (`gpu-2x`, `gpu-8x`, etc.) and split workflows between them based on priority or throughput class. There is no cross-installation load balancing built into this system; job routing is determined solely by which repos are covered by each installation's scope.

---

## E.7. Per-Tenant vs. Per-Team Partitioning

The GMC's multi-tenant model provisions one `ActionsGateway` per namespace. Within an organization, two common partitioning patterns emerge:

**One gateway per team.** Each team owns a namespace and an `ActionsGateway` CR. Runner shapes, replica counts, and quota are fully self-managed per team. This is the recommended default — it aligns operational ownership with the team boundary, keeps the session budget independent, and eliminates cross-team coordination on RunnerGroup configuration.

**One gateway per environment (shared by multiple teams).** A single tenant namespace serves multiple teams, with RunnerGroups differentiated by label convention (e.g. `team-a-gpu-2x`, `team-b-gpu-4x`). This reduces total AGC instances and GitHub App installations but reintroduces the coordination cost the self-service model is designed to avoid. The shared session budget becomes a point of contention if any team's replica count grows without regard for the others. Use this pattern only when the number of teams is small and the platform team is comfortable arbitrating RunnerGroup configuration.

---

## E.8. Decision Guide

```
New runner requirement arriving:
│
├─ Does an existing RunnerGroup have the same GPU count, memory,
│  and tooling requirements?
│   ├─ Yes → Add the new workflow's label to the existing RunnerGroup,
│   │         or just target the existing label from the workflow.
│   │         No new RunnerGroup needed.
│   └─ No  → Create a new RunnerGroup with the appropriate pod shape.
│             Check that total replicas across the ActionsGateway
│             stays below 250.
│
├─ Will adding replicas push the installation over 250 sessions?
│   ├─ No  → Increase replicas on the relevant RunnerGroup.
│   └─ Yes → Shard: create a new namespace + ActionsGateway CR +
│             GitHub App installation for the overflow capacity.
│
└─ Are the repos in a different GitHub organization?
    ├─ No  → Same ActionsGateway CR can serve them all.
    └─ Yes → Separate ActionsGateway CR required (separate installation).
```

---

← [Appendix D](appendix-d-alternatives-considered.md) | [Back to index](README.md)
