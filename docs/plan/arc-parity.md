# ARC Feature Parity

> **Status: goal stated 2026-08-09.
> One deliverable gates 1.5, one is admitted to 1.5, two are sequenced behind it.** This is a map and a definition of done, not a design.
> The inventory below is only as good as its last measurement against a real ARC release, which is why every row carries a version and a date.

## Why this is a goal, and why "parity" is the wrong word for most of it

GitHub Actions Gateway (GAG) is positioned as an Actions Runner Controller (ARC) alternative, so every capability ARC has and GAG does not is a migration blocker for somebody.
That is a narrower claim than "catch up with ARC".
Most of the ARC surface is already at parity or a GAG superset, recorded per capability in [why-gag.md](../why-gag.md); the decomposed v2 API deliberately diverges from ARC's single-CR shape, and that divergence is the product.

What matters is the short list where a team running ARC today cannot move without changing their workflows or losing a capability.
This plan owns that list, keeps it measured, and sequences it across releases.

It exists because the list had no owner.
The 2026-08-06 competitive review found 11 rows in `why-gag.md` asserting an ARC gap with no version and no measurement date, and two of them had gone false at datable upstream releases without anyone noticing.
The corrections shipped; the *forward* half, deciding which gaps GAG closes and when, was never written down.

## Where ARC is actually ahead

Measured against ARC `gha-runner-scale-set` 0.14.2 (released 2026-05-22) and the `master` branch, on 2026-08-06.

| Gap | Why it blocks a migration | Row | Release |
|---|---|---|---|
| **Bound GitHub runner group** (`runnerGroup`) | The forge-side control over which repositories may target a runner set. GAG declares the binding and never wires it, so every scale set registers into the installation's default group | [Q712](../STATUS.md#Q712) | 1.5, gating |
| **`containerMode: kubernetes`** | Runs `container:` and `services:` steps as separate pods on a provisioned volume. GAG runs one worker pod per job, so that path is Docker-in-Docker under Kata rather than a non-privileged pod-per-step model | [Q727](../STATUS.md#Q727) | after 1.5 |
| **GHES validated on a real appliance** | GAG serves GitHub Enterprise Server (GHES) gateways and marks both of its GHES capabilities untested against real hardware, so an enterprise evaluator has no evidence either way | [Q765](../STATUS.md#Q765) | unscheduled, needs an appliance |

## What is deliberately not parity

**A GitHub Support entitlement.** ARC installed via the official Helm charts is covered on GHES 3.9 and later.
GAG has none and none is planned: there is no paid tier and no commercial roadmap, which is stated on the [roadmap](../roadmap.md).
Worth reading the scope exclusions before treating it as decisive, since Kubernetes orchestration, policy application, and template customization are explicitly outside it, and that is much of what a multi-tenant platform team actually pages about.
This row belongs in the comparison as a permanent concession, not on a backlog.

**Install base and maturity.** ARC is General Availability and widely deployed; GAG's v2 API has only just reached its first stability contract.
That closes with adoption and time, not with a deliverable.

## The collision the individual rows do not state

**Q727 cannot start before [Q719](../STATUS.md#Q719).** ARC's `containerMode: kubernetes` depends on a `ReadWriteMany` volume, and GAG's workers are storage-less by design with nothing validating an RWX volume mounted into one.
So the stance on persistent worker storage is undocumented rather than decided, and Q727 would be designing on top of an unvalidated substrate.

Sequencing follows: Q719 validates RWX and writes the reference architecture, then Q727 designs the pod-per-step path against it.
Attempting Q727 first means either building the storage validation inside it, which hides an M-sized piece of work inside an L-sized one, or guessing.

This is also why Q727 is not admitted to 1.5.
Q719 is itself unscheduled.

## Definition of done

ARC parity is done when all of the following hold:

1. **A workflow moves with no `.github/workflows` edit.** ✅ Closed by Q726 (2026-08-11): a runner set registers every `runnerLabel` on its scale set, so a single-name `runs-on` and an array both carry over unchanged.
   The first label names the scale set, which is the ARC scale-set name carried across.
2. **Repository-scoped targeting is expressible.** A platform team can bind a runner set to a named GitHub runner group rather than the installation default (Q712).
3. **`container:` and `services:` steps run without privilege**, or the docs state plainly and permanently that Docker-in-Docker under Kata is the supported answer and why (Q727 resolves either way; a documented decline is a valid outcome).
4. **The GHES claims carry evidence.** Either a real-appliance validation, or the two capabilities keep their untested marker and the comparison says so (Q765).

Criteria 3 and 4 can both be satisfied by a decision rather than a build.
That is deliberate: parity is about removing surprises for a migrating team, and a documented "we do not do this, here is the alternative" removes the surprise as well as a feature does.

## Explicitly out of scope

- **Matching ARC's API shape.** The v2 decomposition into `ActionsGateway`, `EgressProxy`, `RunnerSet`, and `RunnerTemplate` is the product, not an accident to be reconciled.
  The intentional v1 to v2 differences are recorded in [v2-api-gap-analysis.md](v2-api-gap-analysis.md).
- **Commercial support.** See above.
- **In-cluster cache.** It appears on the comparison table as something GAG lacks, but ARC lacks it too ([Q215](../STATUS.md#Q215) tracks it against managed services and GitLab Runner, not ARC).

## What the site may claim today

The comparison surfaces may say GAG is an ARC alternative for shared, multi-tenant clusters, and that a single-name `runs-on` carries over unchanged.

The zero-edit claim is now unqualified for `runs-on` shape, closed by Q726; on GitHub Enterprise Server below 3.21 it is still conditional on the `DistributedTask.AllowRunnerScaleSetCustomLabels` appliance flag, which [migration-from-arc.md](../operations/migration-from-arc.md) names.
They may not claim GHES support is validated while Q765 is open; [features.md](../features.md) carries the untested marker on both GHES capabilities and it stays until an appliance run exists.

## Keeping this current

The inventory rots the same way the comparison table did, and for the same reason: ARC ships, and an undated claim silently becomes false rather than becoming visibly stale.
Two habits hold it:

- Every row above carries the ARC version and the date it was measured.
  A row without both is not evidence.
- [release.md § Pre-flight](../operations/release.md#1-pre-flight) question 3 asks whether every claim about an alternative still holds, before every tag.
  This page is what that question reads and updates.

The failure mode to watch for is the pleasant one: a gap closing upstream because ARC added something, which grows this list without any commit landing in this repo.
Both 2026-08 corrections were of exactly that kind.
