# Release 1.8 Milestone Definition

> **Status: scoped 2026-09-07; every gating item is met.
> `v1.8.0-rc.1` was cut and validated on dogfood 2026-09-12 and still covers `main`, so the tag is a decision rather than a blocked one.** What remains is the riding evidence: criteria 2 and 3 have no reading, and taking them needs another window against this same candidate, not a new one.
> The one gating row, Q1029, the scale-set drain recovery that was lost when no reconcile started inside a terminating worker's window, closed the same day: recovery now runs off the worker-pod watch event ([below](#the-gating-row-q1029)).
> Three rows rode without gating: the two v2 GA soak readings, Q1059 and Q1060, both now closed on positive readings, and [Q1085](../queue/Q1085.md)'s release-notes line.
> The Phase 2 alias decision, Q452, closed 2026-09-08 and is what put Q1085 on the ledger ([the decision](v2-ga.md#decided-v2-omits-ciliumfqdncalicofqdn)).
> The bump is measured rather than assumed: `semver-floor.sh v1.7.0` read 99 commits and **FLOOR: MINOR** on 2026-09-12, so the tag is `v1.8.0`.
> Four `feat`s on the released surface set it: Q1062, Q1011, Q988 and Q994, the last of which landed after this was scoped and rides unlabelled.
> Four patches sit beside them: Q1064's and Q1029's fixes, the gRPC bump for CVE-2026-84445, and a provisioner test.
> Fifteen `feat`/`fix` subjects are withheld because they ship in no image and no chart, which is the gap between counting subjects and reading what a release contains.

## Why this is a release rather than a row that lands whenever

The [release ladder](release-ladder.md) reads 1.7 → 2.0, and 2.0 is [parked on a soak](v2-ga.md#phase-1--the-soak-what-well-validated-means) whose evidence nobody has gathered.
Read on 2026-09-07: criterion 1 has elapsed on the record rather than by re-derivation here, since `v1.4.0` through `v1.7.0` all shipped on `v2beta1` and each release's pre-flight API review returned *ship as-is* over additive surface ([1.4](archive/release-1.4.md#pre-flight-the-api-surface-this-tag-publishes), [1.5](archive/release-1.5.md#pre-flight-the-api-surface-this-tag-publishes), [1.6](archive/release-1.6.md), [1.7](release-1.7.md#pre-flight-verdicts)); criterion 2 is unmet on one kind, since the dogfood overlays and `scripts/dogfood/setup.sh` between them apply `ActionsGateway`, `RunnerSet`, `ClusterRunnerTemplate` and `RunnerTemplate` while `setup.sh` deliberately creates no `EgressProxy` on that cluster, and the e2e lane that does exercise one runs on a kind cluster; criterion 3 has no recorded round-trip across the served versions, the nearest reading being [Q415](archive/q415-migrate-dogfood-validation.md)'s live migration, which drove the webhook on one kind in one direction.
So the GA rung cannot be labelled without publishing a commitment on unmeasured criteria, and the roadmap's near-term section has been empty since 2026-08-29.

1.8 is the release that gathers the evidence, on the venue that produces it: a release candidate books the dogfood window criterion 2 needs, criterion 3 can be read on the same cluster at any time, and the same window is the one two rows have waited for since 1.7 (Q1038, whose row is closed, and [Q1048](../queue/Q1048.md)).
The theme is v2 GA readiness; what makes it a release an operator upgrades for is the gating row.

## The gating row: Q1029

Q1029 was a measured product defect on the scale-set tier.
`RecoverEvictedScaleSetWorkers` lists from the informer cache at the top of a `RunnerSet` reconcile, so a gracefully deleted worker was judged only if a reconcile began in the seconds between the kubelet publishing the terminal phase and removing the object.
When none did, the job was silently never re-run.
The row carried three CI sightings with the AGC log each captured, one of them the control that recovered because its reconcile loop happened to turn over inside the window.

**Closed 2026-09-07.** The reconciler's worker-pod watch now hands each phase-changing or newly preempted scale-set worker straight to the same judge and the same optimistic-lock claim the scan uses, so the claim no longer waits on the reconcile queue; the scan stays for what no event reaches ([04-operational-flows.md](../design/04-operational-flows.md#detecting-a-disruption-is-not-the-same-as-claiming-it)).
The deletion-proof is at the envtest tier: `TestAGC_Drain_ScaleSetWorkerRecovers_WhileTheReconcileQueueIsHeld` parks the controller's only reconcile worker in a second set's listener bootstrap for the whole drain and asserts the claim lands while the reconcile count does not move, and it went red on a build where the watch path was inert.
The e2e drain spec is unchanged: it already re-stages rather than fails on a missed window ([q549-scaleset-rerun-flake.md](q549-scaleset-rerun-flake.md#mode-c-closed-2026-09-07-recovery-runs-off-the-worker-pod-watch)), so a stricter e2e assertion is a call this plan still owes an answer on.

It gates because the exposure is an operator's, not the e2e venue's: a node drain terminates many workers at once, so one missed window there is many lost jobs.
The design already records that this arm cannot be made restart-safe ([04-operational-flows.md](../design/04-operational-flows.md#detecting-a-disruption-is-not-the-same-as-claiming-it)), and the row argues that Q844's orphan recovery does not cover a worker the scan simply missed.

**Why the gap opened was unverified**, and the row said so: the reconciler runs at `MaxConcurrentReconciles: 1` and the listener bootstrap does DNS and an HTTP POST inside `Reconcile`, which was a hypothesis read off timestamps.
What the fix established is narrower than a duration: on the vendored controller-runtime the pod's own phase-change event moves a waiting key to ready at once, so the 8-second gap held a reconcile in flight rather than a backoff, and its duration is still not measured directly ([the reading](q549-scaleset-rerun-flake.md#mode-c-closed-2026-09-07-recovery-runs-off-the-worker-pod-watch)).
The fix does not depend on which step inside it was slow.

## What rides: the soak readings and the alias decision

| Row | What it delivers | Why it rides rather than gates |
|---|---|---|
| Q1059 | Every `v2beta1` kind applied and reconciled on the dogfood cluster, recorded against criterion 2 | A soak reading closes when the evidence exists; a tag cannot wait on a measurement that may come back negative |
| Q1060 | The conversion webhook round-tripped over real objects across the v2 CRD's two served versions, recorded against criterion 3 | Same |
| Q452 | Whether GA `v2` defines `CiliumFQDN`/`CalicoFQDN`, written into [v2-ga.md § Phase 2](v2-ga.md#decided-v2-omits-ciliumfqdncalicofqdn) | A design decision, not a shipped change; deciding it here lets the hop start without one pending |

A reading that comes back negative is the release working: it names the shape fix `v2beta1` still needs, which resets the soak clock and is exactly what GA is gated on finding first.

**[Q1085](../queue/Q1085.md)'s notice rides here too, and only part of it is done.** Deciding Q452 shipped the notice's docs half — the operator pages, both enum godocs and the admission warning now name `v2.0.0` as the removal release — which is what makes this tag the one-release-ahead announcement for `v2beta1` and the aliases.
What 1.8 still owes is the release-notes line naming that removal, since a notice nobody reads in the release body is a notice by technicality.
Q1085's other two halves, the admission reject and the pre-upgrade alias check, are deadlined at 1.9 and land here if there is room ([why the deadline is 1.9](release-ladder.md#why-19-exists-the-storage-version-cannot-advance-in-the-same-release-that-introduces-v2)).

## Scope ledger

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1029 | Drain recovery lost when no reconcile starts inside the window | `1.8-gate` | ✅ closed 2026-09-07 |
| Q1059 | Every `v2beta1` kind on the dogfood cluster (soak criterion 2) | rides | ✅ closed 2026-09-14, reading positive |
| Q1060 | Conversion round-trips on real dogfood objects (soak criterion 3) | rides | ✅ closed 2026-09-14, reading positive |
| Q452 | GA `v2` and the deprecated FQDN aliases | rides | ✅ closed 2026-09-08 |
| [Q1085](../queue/Q1085.md) | The `v2beta1` removal notice: operator docs, enum godoc and admission warning name `v2.0.0` | rides | ✅ docs half shipped with Q452; release-notes line open |
| — | RC validated on dogfood | gates | ✅ `v1.8.0-rc.1` validated 2026-09-12 and again 2026-09-14; still covers `main` |

## Explicitly out of scope

- **[Q413](../queue/Q413.md) itself, and 2.0's coupled removals** ([Q273](../queue/Q273.md), [Q264](../queue/Q264.md)).
  This release supplied the readings and did not pre-empt the verdict; both came back positive on 2026-09-14, which made Q413's Phase 2 ready and moved its Phase 4 half to Q1107.
- **The `feature` rows in the ready queue.** Q988 (the registry read behind `RunnerVersionTooOld`; its row is closed, so it is no longer linked) landed after this was scoped and rides, and Q725 and Q1011 are unrelated to the theme (both rows are closed, so neither is linked).
  Any of them landing before the tag rides in the floor's reading of the window, with no label.
- **The proxy-hardening cluster** and everything else the ladder punts past 2.0, unchanged.

## Definition of done

1. ✅ **Q1029 closed** (2026-09-07), with the queue mechanism established before the fix, the reconcile's duration left unmeasured and said so, and an envtest assertion that fails when the watch-path recovery is inert; the e2e assertion the criterion asked for is still open, per [the gating row](#the-gating-row-q1029).
2. ✅ **Criterion 2 and criterion 3 have a recorded reading** in [v2-ga.md](v2-ga.md#soak-readings)'s Phase 1 table (2026-09-14), both positive, both naming the `v1.8.0-rc.1` window they were taken in.
   All five `v2beta1` kinds reconciled on the dogfood cluster, the manufactured `EgressProxy` among them, and the standing tenant's `ActionsGateway` round-tripped both served versions with identical specs.
   The table is the renderer's output pasted unedited rather than a retyping of the gate's terminal output, so what the plan claims and what the window measured cannot drift.
3. ✅ **Q452 decided** (2026-09-08): `v2` omits both aliases, because the premise the question rested on was itself revisited and `v2beta1` is no longer served past `v2.0.0`.
   The losing option's cost is recorded beside it in [v2-ga.md](v2-ga.md#decided-v2-omits-ciliumfqdncalicofqdn), and the work the answer puts on the critical path is [Q1085](../queue/Q1085.md).
   The API surface review in item 5 no longer expects no change: this release carries the enum godoc and admission-warning corrections that follow from it.
4. ◐ **The two dogfood-window rows** got their window from the `v1.8.0-rc.1` candidate, and it produced one of the two readings; the second window on 2026-09-14 did not change this item's verdict, because the row still outstanding is the one that needs live workers.
   Q1038's `mirror-timing` probe took its first live run against a real mirror in the candidate's Kata e2e leg and returned `SEPARATED` (hits ≤46ms, misses ≥147ms, 4 references, one cold and one warm fetch each) without failing the job, which closes it and settles the placement question as `e2e-reusable.yml`.
   [Q1048](../queue/Q1048.md)'s mirror client census was **not** taken in the first window, because nothing invoked `scripts/dogfood/e2e-mirror-clients.sh` at all.
   It ran in the second and still did not report: of the two client addresses it found, one was a labelled workload pod and the other resolved to no pod and no node, which the script grades as a refusal rather than a pass.
   So the row stays open, and its reason has moved from "nothing calls it" to "it called and could not grade what it saw".
   Q1039's shared-tenants topology was the third until its 2026-09-03 scoping found a dogfood leg to be the worse venue rather than the dearer one, since that cluster has one tenant and so cannot produce either negative; it shipped on the Calico kind lane instead and needs no window.
5. ✅ **The API surface review**, from `scripts/release/api-surface-since.sh` over `v1.7.0..<rc commit>`, run 2026-09-12 and recorded [below](#pre-flight-verdicts).
   When the release was scoped, this item expected *exactly* the `egressPolicyMode` description change item 3 names.
   That was written before Q1062 and Q1011 landed, and no longer describes the window.
   What binds is the narrower claim underneath it: the enum members are unchanged, so a reported member add or removal is a finding.
   Q1062's five condition reasons and its one metric are additive publications expected here; Q1011's new `api/apinames` package is exported Go surface the checker has no category for, so it is reviewed by hand.
6. **Release mechanics**: a candidate tagged, artifacts verified, and the dogfood validation in [release.md](../operations/release.md) passing on the candidate that becomes the tag.

## Critical path

Q1029's measurement → its fix (done) → a candidate.
The soak readings and the alias decision run beside it and need the candidate's window, not each other.
The window is the schedule risk, as it was in 1.7: a reading whose venue is a booked cluster run cannot be compressed the way a code change can.

## Pre-flight verdicts

Each verdict below names the commit it was measured at, because a verdict covers that commit and nothing later ([release.md](../operations/release.md#1-pre-flight)).
Re-run any whose window has moved before the stable tag.

| Check | Measured at | Verdict |
|---|---|---|
| Gating rows | `c99137ea6` | **PASS.** No `1.8-gate` row remains in the store; Q1029 took the label with it when it closed. The empty result was trusted only after the same pattern, widened to any `X.Y-gate`, returned six live rows (Q413 and Q1085 on `1.9-gate`; Q264, Q273, Q1068 and Q1086 on `2.0-gate`), so it can still match a label that exists. |
| `main` green | `c99137ea6` | **PASS with one path-skipped lane.** Nine of the ten required gates ran and passed on the SHA. `e2e-calico` path-skipped, and `check-artifact-unchanged.sh` against the last commit that ran it in full (`96ca227f1`) exits 1 on `cmd/agc/internal/provisioner/admission.go`. That change is comment-only, and `cmd/agc/**` is not in that lane's path list at all, so the lane could not have covered it either way. Dispatched manually on the target rather than reasoning around the check, and [run 34733367705](https://github.com/actions-gateway/github-actions-gateway/actions/runs/34733367705) ran the `e2e-calico / e2e` job in full on `c99137ea6` and passed, so all ten gates are covered on the tag target. |
| Semver floor | `c99137ea6` | **MINOR**, over 99 commits, set by eight touching the released surface: four `feat`s and four patches. `v1.8.0` is forced by merged work rather than chosen. |
| API surface | `c99137ea6` | **PASS, ship as-is.** Additive only: no added wire fields, no enum constraint changes, no default changes. Five new condition reasons (`EgressAuditDisabled`, `EgressAuditJoined`, `EgressAuditUnattributed`, `ProxySourceAuditDisabled`, `WorkerAuditDisabled`) and one new metric (`actions_gateway_egress_audit_unattributed`), all Q1062's. No new Event reasons, labels, annotations, CLI flags or chart values. |

**The `egressPolicyMode` enum members are unchanged**, which is the claim [Definition of done #5](#definition-of-done) rests on: `CiliumFQDN` and `CalicoFQDN` are still defined in both `v2alpha1` and `v2beta1`, and what moved is their godoc, from *removable no earlier than v3.0.0* to *removed at v2.0.0*, the correction Q452's decision forced.
That is a published-documentation change to a served API, not a surface change.

**Reviewed by hand, because the checker has no category for it:** Q1011 adds `api/apinames`, a new exported package in the `api` module (`agentidentity.go`, 69 lines).
It publishes helper functions rather than API types, so it widens the module's Go surface without touching the CRD surface the checker reads.

**The notes are drafted**, at `2de55f74f`, in [docs/releases/v1.8.0.md](../releases/v1.8.0.md).
The operator-caveat pass ran into it: `operator-caveats-since.sh v1.7.0` reports one new `upgrade.md` section and one new `troubleshooting.md` section, both the agent-identity guard, and both are carried into the notes as a `WARNING` banner and an **Upgrading** entry with the two `kubectl` commands that find a collision in advance.
The draft's `Everything since v1.7.0` count is 109, which `check-release-notes.sh` can only verify once `v1.8.0` is a tag; re-derive it at the cut if anything merges first.
The **Container images** section is deliberately absent, because the digests do not exist until `publish.yml` builds them ([why](../releases/README.md#image-digests-are-a-deliberate-post-tag-amendment)).

**Deferred to the stable tag, deliberately.** The marketing reconciliation, the roadmap and `features.md` reconciliation, and the three prose passes (`readability`, `deslop`, `semantic-remediation`) all bind when the text publishes, and a prerelease deploys no docs and generates rather than curates its Release body.
Each verdict names the commit it is measured at ([release.md](../operations/release.md#1-pre-flight)).

## Candidate validation

### `v1.8.0-rc.1`

Cut 2026-09-12 at `c99137ea6`, the same commit every pre-flight verdict above was measured at.
The tag was compared against `origin/main` after creation and before the push, per the [rc.2 postmortem](../postmortems/2026-08-15-rc2-tagged-a-stale-commit.md).

| Step | Verdict |
|---|---|
| Tag points at the target | **PASS.** `v1.8.0-rc.1^{commit}` and `origin/main` both `c99137ea62340be4e228b74291c488175faefc7f`. |
| Publish pipeline | **PASS.** All six images, the chart and the CRD chart published. |
| Artifacts and provenance | **PASS.** 9/9 assets, `draft: false`, `immutable: true`; all eight cosign signatures verified. Provenance `buildSignerURI` ends `publish.yml@refs/tags/v1.8.0-rc.1` and `sourceRepositoryDigest` equals the tag target. The digest check was confirmed able to fail by re-running it against `unit-test.yml` as the signer identity, which exits 1. |
| Dogfood validation | **PASS**, 2026-09-12. e2e matrix GREEN; both sizing profiles actuating (Throughput on 270 real samples); the quota rung bound at zero headroom (`withheldCapacity[quota]=2`) and released on restore; the signed CRD manifest verified, applied and all five CRDs registered. Recorded at `refs/validated/v1.8.0-rc.1`. |

**The candidate still covers `main`.** `check-artifact-unchanged.sh c99137ea6 origin/main` exits 0 at `f771594d1`: 20 files changed since the tag, none on the released surface.
So the eight commits that merged after the candidate — the Q1058, Q1104, Q1105 and soak-wiring work — do not require a new one.

**That validation did not produce the soak readings**, and the reason was structural rather than an oversight in the run: nothing invoked them.
The gate validates the candidate and had no step that takes a reading, `release.md` asked for none, and nothing outside their own rows referenced Q1059 or Q1060 at all.
Q1038's reading was taken in the same window only because `mirror-timing.sh` already had a step in `e2e-reusable.yml`.
[#1922](https://github.com/actions-gateway/github-actions-gateway/pull/1922) wired the other three, which is why the readings took a second window against this same candidate rather than a new one.

### `v1.8.0-rc.1`, second window (2026-09-14)

Run against the same tag to take the readings the first window had no step for.
PASS in 30m39s, e2e matrix 3/3.
The readings are in [v2-ga.md](v2-ga.md#soak-readings); criterion 2 and criterion 3 both came back positive, and Q1048's census did not report.

**The first attempt at this window died in the deploy leg**, which is worth recording because the defect was on `main` rather than in the candidate.
`setup.sh` authored the dogfood `RunnerSet` at `v2beta1` while leaving `spec.acquisitionProtocol` in it; that field is `v2alpha1`-only, so the apply failed strict decoding with the nodes already up.
Three assertions covered that manifest and all three passed throughout, because none of them read the schema.
Fixed in [#1926](https://github.com/actions-gateway/github-actions-gateway/pull/1926) along with a reconciliation that checks every authored spec field against the matching `v2beta1` Go type, which catches the class rather than the instance.
The candidate itself was never in question: the failure was in the tooling that deploys it.
