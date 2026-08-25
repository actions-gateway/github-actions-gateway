# Release 1.6 Milestone Definition

> **Status: scope decided 2026-08-25.** 1.6 is the [ARC parity](arc-parity.md) release the [ladder](release-ladder.md) assigned it, narrowed to one gating row.
> Q719 closed 2026-08-24, leaving its reference architecture behind in [worker-shared-storage.md](../operations/worker-shared-storage.md), so [Q727](../queue/Q727.md) is the only `1.6-gate` row left.
> Q727 resolves through a plan doc before it resolves through code, and that plan doc is what decides whether the parity criterion closes as a build or as a documented decline.
> The bump is not in question: `semver-floor.sh v1.5.0` already reports **MINOR** off work that merged before this scope was written.
> Untrusted-PR CI on Kata was weighed for this release and moved to 1.7 on 2026-08-25.

## The minor was already forced, and this time an instrument reported it

`scripts/release/semver-floor.sh v1.5.0` reports **FLOOR: MINOR** over 156 commits.
It names the 14 that touch the released surface, 11 of them minor, and lists the 56 whose Conventional Commit type would have raised the floor but which ship in no image and no chart.

That is the instrument [release-1.4.md](release-1.4.md#the-minor-was-forced-before-anyone-chose-it) recorded as missing.
The 1.4 scope had to hand-classify 120 commit subjects to find its floor, and wrote down that a raw `feat` count answers the wrong question.
This window makes the same point mechanically: 39 commits carry a `feat` subject and 11 of them set the floor, so the subject count overstates the shipped work by more than three to one.

**Two of the eleven do not change a build, and that is the tool being deliberately careful rather than wrong.** Q719 and Q736 touch only test files and a `Makefile` inside a released package directory.
`release.md` [§ When to cut](../operations/release.md#when-to-cut) states the bias: the floor is a floor, the only costly error is dropping a commit that did change behaviour, so anything less certain than a comment-only Go edit is kept.
Nine commits change code that ships, so the floor is MINOR without them either way.

## What 1.6 carries before its gating row

Nine merged changes alter the shipped artifact.
This is what the release contains today, with no gating row closed.

| Change | Row | PR | Why it is user-facing |
|---|---|---|---|
| Per-connection egress audit record on the proxy, off by default | Q564 | [#1705](https://github.com/actions-gateway/github-actions-gateway/pull/1705) | New `EgressProxy.spec.auditLogging` field and a new operator log surface |
| Advertised capacity and withheld reasons on `RunnerSet` status | Q721 | [#1711](https://github.com/actions-gateway/github-actions-gateway/pull/1711) | New status fields, an operator contract |
| The runner version a worker pod actually ran | Q792 | [#1721](https://github.com/actions-gateway/github-actions-gateway/pull/1721) | New status reporting where a constant stood before |
| Intake gated on workers that bind and never start | Q714 | [#1626](https://github.com/actions-gateway/github-actions-gateway/pull/1626) | New admission behaviour and condition reasons |
| Workers that bind and never start reported, gate or no gate | Q906 | [#1708](https://github.com/actions-gateway/github-actions-gateway/pull/1708) | Condition surface an operator reads |
| Scale-up token bucket folded into the admission ladder | Q717 | [#1702](https://github.com/actions-gateway/github-actions-gateway/pull/1702) | Runtime admission behaviour operators observe |
| GitHub's queued-job count published as a scale-set gauge | Q720 | [#1699](https://github.com/actions-gateway/github-actions-gateway/pull/1699) | New metric series |
| Scale-set listing, so an orphan can be found and pruned | Q344 | [#1706](https://github.com/actions-gateway/github-actions-gateway/pull/1706) | New operator tooling in a published image |
| A probe for whether a scale-set labels PATCH is honoured | Q793 | [#1700](https://github.com/actions-gateway/github-actions-gateway/pull/1700) | Probe capability plus an AGC code path |

Q564 is worth calling out.
It is the first of the four proxy-hardening items [release-1.4.md](release-1.4.md#deferred-out-of-14-and-why) shelved as a coherent theme with no demand recorded against any of them.
Its demand arrived as [Q725](../queue/Q725.md), the ladder recorded the revive on 2026-08-13, and it shipped.
The remaining three ([Q565](../queue/Q565.md), [Q566](../queue/Q566.md), [Q567](../queue/Q567.md)) stay parked, which is the trigger list working rather than the theme dissolving.

## The gating row: Q727, and why the decision comes after the plan doc

[Q727](../queue/Q727.md) is the last open row on the [ARC parity](arc-parity.md) short list that a scheduled release can close.
ARC's `containerMode: kubernetes` runs `container:` and `services:` steps as separate pods on a provisioned volume.
GAG runs one worker pod per job, so that path is Docker-in-Docker under Kata rather than a non-privileged pod-per-step model.

The storage half is settled.
Q719 validated a `ReadWriteMany` volume mounted into the pod the provisioner really builds, across two nodes against a live class, and wrote [worker-shared-storage.md](../operations/worker-shared-storage.md).
What Q727 has left is the pod-per-step model itself.

**The parity criterion admits two answers and the choice is not pre-made.** [Criterion 3](arc-parity.md#definition-of-done) closes either by the steps running without privilege, or by the docs stating plainly and permanently that Docker-in-Docker under Kata is the supported answer and why.
Q727 is size `L` and has never had a plan doc, so neither answer has been costed.
Committing to the build now would commit the release to an uncosted `L` and to a second execution model maintained beside the Kata one; committing to the decline now would foreclose without looking.
So the first deliverable is the phased plan doc the backlog conventions already require of an `L`, and the scope line below stays open until it lands.

**The fact the decline turns on.** A decline is honest exactly where Kata is available, and Kata needs nested virtualization.
Measured against the GCP API on 2026-08-02 and recorded in [kata-dind-workloads.md § Prerequisite](../operations/kata-dind-workloads.md#prerequisite--nested-virtualization-nodes), GKE Standard takes the flag on A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3 and M4, which includes the GPU families; E2, C2D and N2D are absent, and Autopilot does not allow it at all.
On AWS it is a per-instance opt-in confined to selected Intel families, so an AMD, Graviton or GPU instance means `.metal` or nothing ([runner-template-library.md](../operations/runner-template-library.md)).
A team on Autopilot, on AMD or Arm nodes, or on most AWS fleets therefore has no Kata, and a decline hands them `privileged-dind` where their ARC setup needed no privilege at all.
Whatever the plan doc concludes, that population has to be named rather than left to a reader to discover.

## Where the ARC parity definition of done stands

All four criteria are in [arc-parity.md](arc-parity.md#definition-of-done); two closed in 1.5 and one is satisfied in its declared fallback form.

| Criterion | State |
|---|---|
| 1. A workflow moves with no `.github/workflows` edit | ✅ Closed by Q726, 2026-08-11 |
| 2. Repository-scoped targeting is expressible | ✅ Closed by Q712, 2026-08-11 |
| 3. `container:` and `services:` run without privilege, or a permanent documented decline | ❌ [Q727](../queue/Q727.md), this release |
| 4. The GHES claims carry evidence | ✅ Satisfied in fallback: the untested markers hold |

**Criterion 4 needs no work and [Q765](../queue/Q765.md) stays deferred.** Its revive trigger is an Event, a real GHES appliance becoming reachable, and that has not fired.
The criterion's alternative is that the two capabilities keep their untested marker and the comparison says so, and that is intact on all three surfaces: [features.md](../features.md) marks the `gitHubURL` path and `spec.githubCABundleRef` untested against real hardware, [why-gag.md](../why-gag.md) repeats it, and [alternatives.md](../alternatives.md) awards the row to ARC.
Verified 2026-08-25.
A release cannot close this one by deciding to, so it is not a gate.

## Why untrusted-PR CI on Kata is 1.7

Weighed for this release and moved, so the reasoning is recorded rather than lost.

The goal is [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md): open-source CI running fork pull requests on shared nodes with reasonable isolation.
Its critical path is [Q408](../queue/Q408.md) Phases 2 to 5, which build the in-cluster registry pull-through mirror, wire the job-side clients to it, and delete the e2e tenant's open-egress NetworkPolicy.
Phase 1 validated on 2026-08-24 against four green self-hosted Kata runs, graded with a control that fires on both signals, so the non-registry residual is measured gone rather than argued away.
Phase 5 is where "Kata-isolated runners are only suitable for trusted CI" leaves the docs.

**It is a separate axis from parity, not a bigger version of it.** Q727 is a migration blocker for a team leaving ARC; Q408 is a threat-model deliverable for a team running untrusted code.
Kata already makes Docker-in-Docker unprivileged, so closing Q727 moves the untrusted-PR posture forward by nothing, and closing Q408 removes no ARC migration blocker.
Bundling them would give 1.6 two `L` items on unrelated axes and a tag waiting on the slower.

**Two rows on that axis need a decision before 1.7 is scoped, and neither is this release's.**

- [Q215](../queue/Q215.md) reads as revived and has not been moved.
  Its trigger is demand for an in-cluster cache **or** Q408 Phase 1 landing and removing the working one, and Phase 1 gated every `actions/cache` step and the bake's `type=gha` cache to the hosted lane on 2026-08-05.
  Whether that counts as the trigger firing turns on whether "the working one" means GAG's own self-hosted lane or a tenant's, which the row does not say.
  1.7 scoping has to settle it; leaving a `deferred` row whose trigger may have fired is the failure mode [release-ladder.md](release-ladder.md#the-rule-this-establishes) names.
- [Q986](../queue/Q986.md) is the gap under Definition of Done #5, a per-tenant and per-job record of which host each job reached.
  Q564 shipped the record and attributes per pool, so the tenant half holds only on an unshared pool and the job half holds nowhere.
  An untrusted-PR claim without it is a claim about controls with no evidence behind them.

**One criterion in that goal doc conflicts with a decision already taken, and 1.7 should reconcile it rather than inherit it.** Definition of Done #1 requires the isolated posture to be the default rather than an opt-in.
[runner-template-library.md § Nothing ships as a cluster default](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default) declines exactly that, on the ground that a shipped default template would silently hand a privileged pod shape to sets that never asked for one.
Both positions are defensible and they cannot both stand as written.

## Explicitly out of scope

- **Workload identity.** The no-PEM delegation model ([05-security.md §5.7](../design/05-security.md)) stays opt-in with its in-cluster PEM default, and 1.6 makes no change to it and no new claim about it.
  Decided 2026-08-25.
- **[Q765](../queue/Q765.md) as a build.** Blocked on hardware nobody has; see criterion 4 above.
- **[Q539](../queue/Q539.md) and [Q540](../queue/Q540.md).** Both are blocked behind Q408 by design: the mirror contract is validated on the simple implementation before variants are graded against it.
- **The remaining proxy-hardening cluster.** Q565, Q566 and Q567 keep their demand triggers.

## Definition of done

1. **Q727 resolved**, by a merged pod-per-step implementation or by a merged documented decline that names the no-Kata population.
   The plan doc lands first and the choice is recorded in it.
2. **`arc-parity.md` criterion 3 flipped**, and its row in [docs/plan/README.md](README.md) with it.
3. **The docs sweep.** Nine user-facing changes merged before any gating row, and [release.md § Pre-flight](../operations/release.md#1-pre-flight) question 1 exists to catch a feature that reaches `features.md` and stops there.
   Q564 in particular adds an operator-visible field and a log format.
4. **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.5.0..<rc commit>`.
   The window already carries additions to `RunnerSet`, `EgressProxy` and the shared condition set, and the review binds at the candidate, not here.
5. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Critical path

Q727's plan doc is the only thing between here and a decided scope.
Everything else in the release is merged.

1. Q727 plan doc: cost the pod-per-step build against the decline, and record the choice.
2. Q727 executed, whichever way that went.
3. Docs sweep and the pre-flight questions.
4. Candidate, validation, tag.

Step 1 is the one that can change this document.
If it concludes in a decline, 1.6's parity content is a docs change on top of nine merged features, which is a real minor and a thin theme, and that is worth saying out loud at the tag rather than discovering in the notes.
