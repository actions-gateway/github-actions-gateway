# Release 1.6 Milestone Definition

> **Status: scope decided 2026-08-25.** 1.6 is the [ARC parity](arc-parity.md) release the [ladder](release-ladder.md) assigned it, narrowed to one gating row.
> Q719 closed 2026-08-24, leaving its reference architecture behind in [worker-shared-storage.md](../operations/worker-shared-storage.md), and Q727 — the last `1.6-gate` row — closed 2026-08-25.
> Its plan doc ([q727-container-steps.md](q727-container-steps.md)) costed the build against the decline and chose the decline, so 1.6's parity content is a docs change on top of nine merged features.
> Every gating row is now closed; what remains is the docs sweep, the API surface review, and release mechanics.
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

## The gating row: Q727, decided as a documented decline

**Resolved 2026-08-25.** Q727 was the last open row on the [ARC parity](arc-parity.md) short list that a scheduled release could close.
ARC's `containerMode: kubernetes` runs `container:` and `services:` steps as separate pods on a provisioned volume.
GAG runs one worker pod per job, so that path is Docker-in-Docker under Kata rather than a non-privileged pod-per-step model.

The storage half is settled.
Q719 validated a `ReadWriteMany` volume mounted into the pod the provisioner really builds, across two nodes against a live class, and wrote [worker-shared-storage.md](../operations/worker-shared-storage.md).
What Q727 had left was the pod-per-step model itself.

**The criterion admitted two answers, and the costing chose the second.** [Criterion 3](arc-parity.md#definition-of-done) closes either by the steps running without privilege, or by the docs stating plainly and permanently that Docker-in-Docker under Kata is the supported answer and why.
The plan doc landed first as this section required, and [q727-container-steps.md](q727-container-steps.md) records what it found: ARC's mechanism needs a pod-`create` grant that RBAC cannot scope below a namespace, and GAG refuses that token at `RunnerTemplate` admission and again when the provisioner overwrites the tenant template.
Because a per-job Secret holding a job's `jitconfig` and payload sits in the tenant namespace, a pod-`create` right there would let one job take another job's credentials — an escalation between jobs of one tenant, not the tenant boundary namespaces already defend.
The mechanism is therefore declined permanently rather than deferred; the durable rationale is [D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes).

**Pod-per-step as a capability is deferred, not declined.** A broker-mediated design that keeps the token invariant — a GAG hooks implementation asking the AGC for step pods — was costed and is [Q998](../queue/Q998.md), behind a demand trigger.
The two paths that were not chosen, and why, are in the plan doc's options table.

**The fact the decline turns on.** A decline is honest exactly where Kata is available, and Kata needs nested virtualization.
Measured against the GCP API on 2026-08-02 and recorded in [kata-dind-workloads.md § Prerequisite](../operations/kata-dind-workloads.md#prerequisite--nested-virtualization-nodes), GKE Standard takes the flag on A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3 and M4, which includes the GPU families; E2, C2D and N2D are absent, and Autopilot does not allow it at all.
On AWS it is a per-instance opt-in confined to selected Intel families, so an AMD, Graviton or GPU instance means `.metal` or nothing ([runner-template-library.md](../operations/runner-template-library.md)).
A team on Autopilot, on AMD or Arm nodes, or on most AWS fleets therefore has no Kata, and a decline hands them `privileged-dind` where their ARC setup needed no *pod* privilege.
That comparison flatters ARC if it stops there: ARC buys the unprivileged pod with a namespace-wide API grant that needs no exploit to use, so the choice is between two privileges rather than between privilege and none.
That population is named as the decline's cost, on every comparison surface that claims the gap and in [D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes).

## Where the ARC parity definition of done stands

All four criteria are in [arc-parity.md](arc-parity.md#definition-of-done); two closed in 1.5 and one is satisfied in its declared fallback form.

| Criterion | State |
|---|---|
| 1. A workflow moves with no `.github/workflows` edit | ✅ Closed by Q726, 2026-08-11 |
| 2. Repository-scoped targeting is expressible | ✅ Closed by Q712, 2026-08-11 |
| 3. `container:` and `services:` run without privilege, or a permanent documented decline | ✅ Closed by Q727, 2026-08-25, in its decline form |
| 4. The GHES claims carry evidence | ✅ Satisfied in fallback: the untested markers hold |

**Criterion 4 needs no work and [Q765](../queue/Q765.md) stays deferred.** Its revive trigger is an Event, a real GHES appliance becoming reachable, and that has not fired.
The criterion's alternative is that the two capabilities keep their untested marker and the comparison says so, and that is intact on all three surfaces: [features.md](../features.md) marks the `gitHubURL` path and `spec.githubCABundleRef` untested against real hardware, [why-gag.md](../why-gag.md) repeats it, and [alternatives.md](../alternatives.md) awards the row to ARC.
Verified 2026-08-25.
A release cannot close this one by deciding to, so it is not a gate.

## Why untrusted-PR CI on Kata is 1.7

Weighed for this release and moved, so the reasoning is recorded rather than lost.

The goal is [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md): open-source CI running fork pull requests on shared nodes with reasonable isolation.
Its critical path is Q408 Phases 2 to 5, which build the in-cluster registry pull-through mirror, wire the job-side clients to it, and delete the e2e tenant's open-egress NetworkPolicy.
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
- Q986 is the gap under Definition of Done #5, a per-tenant and per-job record of which host each job reached.
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

1. **Q727 resolved.** ✅ 2026-08-25, by a merged documented decline naming the no-Kata population ([q727-container-steps.md](q727-container-steps.md), [D.15](../design/appendix-d-alternatives-considered.md#d15-pod-per-step-container-execution-arcs-containermode-kubernetes)).
   The residual capability is deferred as [Q998](../queue/Q998.md).
2. **`arc-parity.md` criterion 3 flipped**, and its row in [docs/plan/README.md](README.md) with it. ✅ 2026-08-25, in the same change.
3. **The docs sweep.** Nine user-facing changes merged before any gating row, and [release.md § Pre-flight](../operations/release.md#1-pre-flight) question 1 exists to catch a feature that reaches `features.md` and stops there.
   Q564 in particular adds an operator-visible field and a log format.
4. **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.5.0..<rc commit>`. ✅ 2026-08-25, measured at `fe06ec682` and recorded below.
5. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Candidate record

### `v1.6.0-rc.1`, tagged at `fe06ec682`

Cut 2026-08-25 from `origin/main`, with the tag's commit compared against the target before the push rather than after it.

**`main` green.** `check-gates-green.sh` reports 0 gates not green and 7 path-skipped, which is the ordinary shape of a docs-only tip.
The verdict relies on **`190a5f081`**, where `e2e-test`, `integration-test`, `security-scan`, `plan-hygiene` and `status-lint` all ran in full, and `check-artifact-unchanged.sh 190a5f081 origin/main` exits 0 across the 23 files merged since.
`make check` is green on a branch cut from the target.

**Version.** `semver-floor.sh v1.5.0` reports **FLOOR: MINOR** over 158 commits, 14 of them touching the released surface.
No breaking marker, and the published CRDs took **zero deleted lines** across the window, so the surface is purely additive.

**API surface review: ship as-is.** Six wire fields, four condition reasons and one Event reason publish for the first time.
`auditLogging` names its method rather than an `On` state, is a string enum rather than a bool, and defaults to `Off`, which is the safe direction for a record of a tenant's egress.
`advertisedCapacity` is a pointer because zero is a real advertisement and has to differ from "never advertised".
`withheldCapacity` carries `listType=map` on `reason`, and that key is deliberately open rather than an enum, because the ladder grows a rung at a time and the field is controller-written status a tenant cannot author.
`observedRunnerVersion` is documented as a self-report rather than an attestation and deliberately does not move `RunnerVersionTooOld`.

One item is flagged rather than blocking: `auditLogging` borrows a word that means *report-only instead of enforce* in Pod Security Admission and Gatekeeper.
The godoc disclaims that reading explicitly, so the collision is handled in prose rather than avoided in the name.

**Publish verified by content**, not by green jobs: `draft: false`, `immutable: true`, 9 assets, and all 8 signatures verified.
The discriminating check is the provenance, whose signer URI ends `publish.yml@refs/tags/v1.6.0-rc.1` and whose `sourceRepositoryDigest` equals `fe06ec682`.
Re-run against a wrong signer workflow it exits 1, so the pass discriminates rather than merely exiting 0.

**Dogfood validation:** never run.
The gate was stopped in pre-flight, before it scaled a node, when the decision was taken to merge the open dependency bumps first.

**Superseded 2026-08-26.** #1725, #1726 and #1674 merged, and `check-artifact-unchanged.sh v1.6.0-rc.1 origin/main` exits 1 on 11 files of the released surface, all `go.mod`/`go.sum` across `broker`, `cmd/agc`, `cmd/proxy`, `cmd/worker`, `githubapp` and `scaleset`.
The tag stays published and immutable; the next candidate is `rc.2`.

### `v1.6.0-rc.2`, tagged at `24d8b0d5f`

Cut 2026-08-26 from `origin/main`, with the tag's commit compared against the target before the push rather than after it.

**`main` green.** `check-gates-green.sh` reports 0 gates not green and 7 path-skipped.
The verdict relies on **`f174e70ae`**, where `e2e-test`, `e2e-calico`, `integration-test`, `license-notices`, `manifest-validate`, `plan-hygiene` and `security-scan` ran in full alongside `unit-test`, `lint`, `coverage`, `tidy-check` and `vendor-check`.
That is the commit that last moved the released surface, and `check-artifact-unchanged.sh f174e70ae 24d8b0d5f` exits 0 across the 4 files merged since, so nothing this tag ships is unvalidated.
`make check` is green on a branch cut from the target.

**Version.** `semver-floor.sh v1.5.0` reports **FLOOR: MINOR** over 165 commits, 14 of them touching the released surface.

**API surface review: ship as-is**, carried over from `rc.1` after re-derivation rather than assumed.
The dependency bumps moved `api/go.mod` and `api/go.sum` and no wire declaration, so `api-surface-since.sh v1.5.0` reports the same six fields, four condition reasons and one Event reason.

**Publish verified by content**, not by green jobs: `draft: false`, `prerelease: true`, `immutable: true`, 9 assets, and all 8 signatures verified.
The discriminating check is the provenance, whose signer URI ends `publish.yml@refs/tags/v1.6.0-rc.2` and whose `sourceRepositoryDigest` equals `24d8b0d5f`.
Re-run against a wrong signer workflow it exits 1, so the pass discriminates rather than merely exiting 0.

**Dogfood validation: PASSED**, 2026-08-26, on the tag itself.
The e2e matrix is green at 75 passed, 0 failed and 13 skipped (run `33018548757`), covering cross-tenant network isolation, worker preemption and recovery, a drained worker that must trigger no rerun, a ceiling-held job cancelled rather than redelivered, the v1 to v2 migration dry run, and Vault workload identity with no PEM Secret.
Both sizing profiles actuated on real workers: `NodeShare` on `ci-e2e` and `Throughput` on `ci` each report `sizingProfileState=Active`, the latter at `sampleCounts=[211]`.
The signed v2 CRD artifact verifies and applies, with all five CRDs established.
Cluster preflight passed with two warnings that are properties of the dogfood cluster rather than the candidate: no cert-manager and no metrics-server.
The cluster returned to 0 nodes, confirmed by asking it after the `e2e` pool's autoscale lag rather than by reading the teardown's own line.

### Two claims the notes review should have caught

Both were taken from this document's own table rather than measured, which is the failure mode CLAUDE.md names for a row's asserted mechanism.

`Q344` was described as "new operator tooling in a published image".
Its only caller is `cmd/probe`, which the publish matrix (`gmc agc proxy worker wrapper build-runner`) does not build and no release asset carries; it runs from a source checkout.
It is a maintainer diagnostic, and the notes no longer list it as a feature.

`Q793` was described by its investigation half, the probe that measured the labels `PATCH`.
Its operator-facing half is the AGC message telling an operator that a scale set's labels are fixed at creation, which is what the notes now say.

## Critical path

Scope is decided and every gating row is closed.

1. ~~Q727 plan doc: cost the pod-per-step build against the decline, and record the choice.~~ ✅ 2026-08-25.
2. ~~Q727 executed.~~ ✅ 2026-08-25, as the decline.
3. Docs sweep and the pre-flight questions.
4. Candidate, validation, tag.

The decline is what it concluded, so 1.6's parity content is a docs change on top of nine merged features.
That is a real minor and a thin theme, and it is worth saying out loud at the tag rather than discovering in the notes.
