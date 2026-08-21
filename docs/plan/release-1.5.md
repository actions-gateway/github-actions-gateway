# Release 1.5 Milestone Definition

> **Status: scope opening 2026-08-06.** [Release 1.4](release-1.4.md) is already scoped and its gating rows are fixed; 1.5 is where work identified after that line lands.
> Four gating Queue rows so far, labelled `1.5-gate`: Q712, Q713, and Q726, admitted 2026-08-09 from the candidate list below, plus Q715, admitted the same day off an external date.
> All four shipped 2026-08-11, and the marketing reconciliation closed 2026-08-12 (Q801, Q821).
> Two more were admitted afterwards, both on the [parity axis](#the-parity-axis-what-closed-and-what-backs-it): Q776 on 2026-08-13, to back the parity claim the other four earned, and Q844 on 2026-08-14, a classic-only capability the badge set never recorded and the marketing surface already claims for both tiers.
> Q844 shipped 2026-08-14 and Q776 has now shipped too, so every gating row is closed.
> **All eight rows from the reopened scope shipped on 2026-08-15**, so the Queue again carries no `1.5-gate` row and [the rc.2 pre-flight](#the-rc2-pre-flight-2026-08-15) is recorded below.
> The [API surface review](#pre-flight-the-api-surface-this-tag-publishes) is recorded below, which cleared the last item binding at the release candidate.
> `v1.5.0-rc.1` was cut from `ff3b3ef1` on 2026-08-14 and [its dogfood validation PASSED](#the-rc1-validation-verdict-2026-08-14), and the [stable-tag pre-flight](#the-stable-tag-pre-flight-2026-08-14) then ran and closed.
> **Scope reopened the same day**, on [eight further gating rows](#scope-reopened-2026-08-14-what-a-question-cost), two of which change shipped code, so `v1.5.0-rc.1` is superseded and the line will tag from an rc.2.

## Why these gate a release rather than riding along

Q712 and Q713 came out of the 2026-08-06 competitive analysis, and both were defects in capabilities the project already claims, not new features.
That is what makes them release-gating: shipping another minor while either is open means shipping a claim the product does not honour.

Q726 is the same shape read from the other side.
It is the one [ARC parity](arc-parity.md) gap that breaks the zero-edit migration claim the front door makes, so it gates for the same reason: the claim is published and the product does not honour it for a `runs-on` array.

### Q712 — the runner-group binding is declared and never wired ✅ shipped

`RunnerGroupName` exists on the scale-set listener config (`cmd/agc/internal/scalesetlistener/listener.go`) and is resolved when non-empty, but the sole production construction site in `cmd/agc/internal/controller/runnerset_scaleset.go` never set it.
Every scale set therefore registered into the installation's default runner group.

The GitHub runner group is the **GitHub-side authorization point** for which repositories may target which runners.
With every tenant's scale set in one group, a repository outside a tenant could name that scale set in `runs-on` and route work into the tenant's namespace, quota, and egress IP.
GAG's pod-level isolation was unaffected; what was unbounded was *who could cause a job to run there*.

This is also the one place ARC was ahead on GAG's own core claim: its `gha-runner-scale-set` chart exposes `runnerGroup` as a first-class value.

**Two defects the row did not name, found by measuring before coding.** Wiring the field alone would have shipped a boundary that looks declared and is not:

- The resolver **fell back to the default group** when the name did not resolve (`ok == false` kept `groupID = 1`), so a typo widened the boundary to the whole installation at exactly the moment the operator was narrowing it.
- `ensureScaleSet` **adopts an existing scale set by name without reading its group**, so declaring `runnerGroup` on a set registered by an earlier run would have left the original group in force with nothing reporting it.

**What shipped.** `RunnerSet.spec.runnerGroup`, inheriting `ActionsGateway.spec.defaultRunnerGroup` when unset (the `templateRef`/`proxyRef` shape, so a tenant declares its boundary once on the gateway), both `v2alpha1` and `v2beta1`.
An unresolvable name fails the set closed at `Ready=False`/`RunnerGroupNotFound` rather than falling back; an adopted scale set is moved into its declared group; and changing the declared group restarts the set's listener rather than waiting for an AGC rollout.
An **undeclared** group still leaves an existing scale set where it is, so widening is always explicit.

**The gateway-assertion question, settled.** The gateway carries the group as an inherited default, not as an enforced ceiling.
GAG does not create runner groups and does not manage their repository access, so there is nothing for it to assert *against*: a tenant naming a group only volunteers its own runners to that group's repositories, which costs the tenant and escalates nothing across tenants.
What the platform admin still owns at GitHub is documented in [tenant onboarding](../operations/tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group) and [appendix H](../design/appendix-h-v2-api-decomposition.md).

Adjacent hole found while measuring, filed rather than fixed at the time, and since closed (Q791): the `RunnerSet` webhook's scale-set label-uniqueness guard was **namespace-scoped**, so two tenants under one GitHub org could claim one scale-set name across namespaces.
It is now keyed on the gateway's GitHub binding and enforced cluster-wide from both admission paths ([appendix H](../design/appendix-h-v2-api-decomposition.md), [security](../design/05-security.md#cross-tenant-job-acquisition-via-a-shared-scale-set-name)).

### Q713 — the shipped tier emits no duration or latency series ✅ landed 2026-08-11

`waitForCompletion` and the pod waiter ran only inside `provision()` (`cmd/agc/internal/provisioner/provisioner.go:557` and `:583`).
`ProvisionScaleSetWorker` registered neither, and `v2beta1` is ScaleSet-only, so the tier every new tenant runs emitted both series empty.

The blast radius was entirely downstream: two Appendix A SLOs, a severity-critical alert, four recording rules, both shipped Grafana dashboards, the runbook, and the cost-attribution guide all read blank.
The failure mode is worst-case for a pre-adoption project: the first external operator to apply the shipped `PrometheusRule` sees a product that looks broken, and [go-to-market § 8](go-to-market.md#8-launch-sequence-phased) records that first impressions from cold traffic are one-shot.

**How it was fixed.** Both observations moved onto the shared pod informer, which sees a scale-set worker pod and a classic one identically.
The latency observation had been gated behind a registered waiter, the thing a fire-and-forget provision never creates, so the tier seam was the gate itself rather than a missing call site.

That made the span a decision rather than a detail, since the two tiers had no common "acquisition" moment to measure from.
`job_duration_seconds` is now **worker pod lifetime** on both tiers: creation to the last container finishing, or to the deletion request for a worker removed mid-run.
Every documented consumer of the series is cost attribution ([appendix-f](../design/appendix-f-cost-model.md) multiplies it by an hourly node rate), and a pod is billed from creation, so the classic tier's old span was measuring the wrong thing: it charged the staging and `spec.scaleUp` throttle window during which no pod existed.
Correcting it was therefore part of the fix rather than a follow-up.

Two consequences worth keeping: observations fire once per pod when its lifetime ends, so a long job's latency observation arrives at completion rather than at start; and a pod already terminal when an AGC restarts and re-lists it is claimed without being re-observed, so a restart cannot spike either histogram with one duplicate per pod still inside `completedPodTTL`.

### Q715 — the runner version reported to GitHub is a constant, and the too-old warning is classic-only

Two halves, and only together do they leave the shipped tier blind.

The version sent at session creation is `BrokerConfig.RunnerVersion`, the pinned default the project ships (`cmd/agc/internal/agentpool/pool.go:467`, reached from `runner_shared.go:555`).
It is the same value whatever `spec.workerImage` holds, so a tenant running an older runner image reports the newer number GAG was built against.
The pod's `app.kubernetes.io/version` label *is* derived from the image ref (`provisioner/pod.go:47`), which makes the two disagree without either being consulted for admission.

The signal that would otherwise catch it, `RunnerVersionTooOld`, is produced in the classic listener goroutine only.
That is structural rather than an oversight: the scale-set protocol carries no runner version at session creation, so the condition cannot occur on the tier `v2beta1` exposes ([gap analysis](v2-api-gap-analysis.md#agc)).
The consequence is still that no tier both knows the real version and can warn about it.

**What makes it gate rather than wait:** GitHub raises the enforced minimum runner version on GHEC on 2026-09-25.
On that date a tenant whose worker image is behind starts failing at GitHub, and GAG has told nobody, on the tier every new tenant runs.
This is the only gating row here with a date it does not control, which is why it was admitted without going through the candidate list.

**Shipped 2026-08-11.** Two corrections to the framing above came out of measuring it.

The runway is shorter than the row's date: GHEC runs brownouts from 2026-08-24 through 2026-09-18 before full enforcement on 2026-09-25 ([changelog, 2026-06-12](https://github.blog/changelog/2026-06-12-github-actions-minimum-version-enforcement-timeline-for-self-hosted-runners/)).
A brownout is a four-hour window, so the first symptom is intermittent — a job that fails at midday and works by evening reads as a flake, not a version problem, which is exactly what a named condition is for.
The floor is `2.329.0` for registration, plus a rolling requirement that each release be installed within 30 days of publication to keep executing jobs; a compiled-in constant can hold the first and not the second, so `names.MinRunnerVersion` is documented as the registration floor only.

`agent.version` was left alone, against the row's own wording.
There are two versions with different jobs: on the classic path the AGC *is* the registered agent, so `agent.version` describes the listener protocol the AGC implements and not whatever image a worker runs.
Reporting the tenant's image version there would have GitHub refuse sessions for a listener that is current, converting a silent risk into an immediate outage.
The fix adds the second number instead of changing the first: both reconcilers read the runner version off the effective worker image each reconcile and publish `RunnerVersionTooOld` from it (`WorkerImageBelowMinimum` / `WorkerImageCurrent` / `WorkerImageVersionUnknown`), which needs no session and so reaches the ScaleSet tier.

What the image reference cannot answer, the worker now does: the upstream runner image carries no `RUNNER_VERSION` env var and no version label — its `org.opencontainers.image.version` is the Ubuntu base's `24.04` — but `bin/Runner.Listener.deps.json` names the real version, so the injected wrapper reads it and logs it once per pod.
Reporting that reading back to the AGC, rather than only to `kubectl logs`, is deferred: it needs a channel out of the worker and a status field, and it reports nothing until a set's first job finishes.

## The parity axis: what closed, and what backs it

The four gating rows above were each argued on their own terms, but they land on one axis.
Every one is a capability the classic tier had and the ScaleSet tier did not, or a shape `v1alpha1` allowed and `v2beta1` refused.
Recorded here because the release notes need the through-line, and because a future audit should not re-derive it.

**Two axes, measured 2026-08-13.**

Classic to ScaleSet acquisition tier: **closed on the tracked inventory, and the inventory turned out to be incomplete.** [features.md](../features.md) marks any capability that does not reach the ScaleSet tier with a `partly classic-only` badge, and no capability carries one; the last two came off with Q713, on Alerting and SLOs and on Grafana dashboards.
[v2-ga.md § Capability parity](v2-ga.md#capability-parity-is-a-precondition-of-the-removal), which exists because removing classic at `v2.0.0` must not delete a capability along with it, reads Both tiers on all five rows.

Neither surface caught Q844, found 2026-08-14 by asking whether the axis was really shut.
Restart-safe disruption recovery was classic-only: the classic provisioning goroutine reads the disruption markers off the resolving event it is already watching, while a scale-set worker is readable only while it terminates, so an AGC down for that window never issued the re-run.
`features.md` claimed automatic re-run for eviction, preemption, drain and a bare `kubectl delete pod` with **no tier badge**, and the troubleshooting matrix said outright that every firing case "works on **both acquisition tiers**".
It was admitted as a `1.5-gate` row on the same test Q712 and Q713 met — the claim is published and the shipped tier does not honour it — and shipped the same day: the ScaleSet tier now persists the run behind every worker it builds, and re-runs any whose pod is gone when it comes back.

That the badge set and the parity table both read clean while this was open is the sharpest available argument for Q776 below, and it is why the tier result is stated here as *measured against the inventory* rather than as parity full stop.

`v1alpha1` to `v2beta1` API: **closed, but mostly before the 1.4 tag.** All eight gaps in the [gap analysis](v2-api-gap-analysis.md) are closed and it declared its own scope closed 2026-08-09; the last v2 API milestone, Q166 cross-namespace sharing, is an ancestor of `v1.4.0`.
1.5 contributed the one surviving capability drop, Q726: `v1alpha1` set `MinItems=1` with no ceiling while `v2beta1` CEL-enforced `size(self) == 1`, and that field's godoc offered staying on a `v2alpha1` Classic set as the migration path, which `v2.0.0` removes.

### What parity does not yet have: a backstop

Both results rested on a one-time manual walk, and on the tier axis that walk had already gone stale three times when this section was written.
Q683, Q691 and Q713 each arrived classic-only from birth *after* the 2026-07-26 tier-seam walk declared parity, and nothing re-walks it.

The tier badge gate is one-directional by construction.
It fails a badge whose Queue row already shipped, a badge with no `<!-- tier:QN -->` annotation, and a badge linking no `operations/` page, so a badge cannot outlive its gap.
It cannot see the case that actually recurs: a capability that is classic-only and was never badged at all.

**Q776 was admitted as a `1.5-gate` row on 2026-08-13** for that reason.
It reconciles the `actions_gateway_*` names across both sides against the absent-by-design list in [v2-ga.md](v2-ga.md#capability-parity-is-a-precondition-of-the-removal), which both re-establishes the measurement as current and leaves behind the gate that keeps it so.

Q844 found the fourth instance a day later, and by hand rather than by any gate, which is the argument for Q776 restated as evidence.
It has since closed the gap it found; Q776 is what makes the next one visible without someone thinking to ask, and landing it is what lets the release notes say parity rather than name four ports.

**✅ Q776 shipped.** The re-walk covered all 53 `actions_gateway_*` series the AGC defines, and the [acquisition-tier ledger](../operations/observability-metrics.md#acquisition-tier-reach) is where each one's tier now lives: 26 reach both tiers, 16 are classic-only, 10 are scale-set-only, and one is tier-neutral.
`make metric-tiers-check` holds the ledger to the source in both directions, so a series added on one tier fails until someone answers the tier question, and a row the source refutes fails too.

It found two things the one-time walk had not, which is the argument for the gate rather than a fifth walk:

- `renew_job_errors_total` and `renew_job_teardowns_total` are classic-only by construction, since a scale-set runner renews and completes its own job and the AGC never calls `renewjob`.
  Neither was on the absent-by-design list, and neither row said which tier emits it, so both read as system-wide.
- `eviction_recovery_evidence_lost_total` (Q809) reached no operator doc at all.
  It was described in the design docs and the troubleshooting runbook, and the metrics reference an operator actually reads never gained a row.

Neither is a capability gap, so the parity result above stands: **the tier axis is closed on the full metric inventory, not only on the tracked one.** The gate covers metrics rather than capabilities, so a capability with no series behind it still joins the parity table by hand.
That residual is recorded in [v2-ga.md](v2-ga.md#what-this-audit-checked-and-found-already-covered) and belongs to [Q774](../queue/Q774.md).

The v1 to v2 axis gets no equivalent row, and that is a decision rather than an omission.
`cmd/agc/api/v1alpha1/conditions_parity_test.go` already pins the listener vocabulary across all three packages by value (Q309), and new drift can only come from someone adding to `v1alpha1`, which is frozen and comes out in the `v2.0.0` bundle ([Q264](../queue/Q264.md)).

### Differences that survive parity, and should

Not gaps, and not for the next audit to re-litigate.
Preemption and drain recovery were listed here until 2026-08-14, on the grounds that only restart-safety differed; Q844 moved them out, because the difference was one the published claim does not make and the classic tier does not have, and then closed it:

- GitHub's session rejection for a too-old runner version stays classic-only, because the scale-set protocol carries no runner version at session creation.
  Q715 gives the ScaleSet tier a reconcile-time warning instead, so both tiers carry a signal and only classic has the GitHub-side rejection.
- The capacity ceiling uses a different pre-check and a different fallback on the scale-set tier (Q576), because a ScaleSet states its capacity as one integer per poll rather than as a decision per job.
- Several counters are absent from the scale-set tier by construction, being artifacts of the many-acquirers and JIT-agent models `ScaleSet` removes.
  [v2-ga.md](v2-ga.md#capability-parity-is-a-precondition-of-the-removal) holds the list, and Q776 reconciles against it.

### What the notes may claim

The bar was that the notes name the ports rather than asserting parity until Q776 landed, because a published parity claim resting on a walk with a four-times-stale record is the same failure Q801 spent this release fixing, one surface over.
Q844 is the case in point: the claim was already published, on `features.md` and in the troubleshooting matrix, before anyone checked whether the shipped tier honoured it.

Q776 has landed, so the notes may now say parity **on the metric surface**, and should say it in those words.
The ledger is the evidence and the gate is what keeps it true; a capability with no series behind it is still covered by a manual walk, so an unqualified "full parity" would overstate what is actually enforced.

## Pre-flight: the API surface this tag publishes

Recorded 2026-08-14 from `scripts/release/api-surface-since.sh` over `v1.4.0..feabacdc4`, per [release.md § Pre-flight](../operations/release.md#1-pre-flight).
**Verdict: ship as-is.** Two wire fields, one condition type and five condition reasons are published for the first time; no enum constraint, no default and no label or annotation key changed, and nothing is wire-breaking.

The surface stopped moving before the review: Q844 landed controller code on 2026-08-14 and added none of it.

| Addition | Carried on | Why the shape is right |
|---|---|---|
| `runnerGroup` (Q712) | `RunnerSet` | `+optional` with `omitempty` and `MinLength=1`, so an explicit `""` is rejected and unset cannot diverge from empty; no pointer and no CRD default needed. Length bounds sit on the field rather than in a spec-level rule. |
| `defaultRunnerGroup` (Q712) | `ActionsGateway` | Inherited only by RunnerSets that set no `runnerGroup`, so the narrower field always wins, matching `templateRef`/`proxyRef`. |
| `RunnerLabelsIncomplete` (Q726) | `RunnerSet` condition | Abnormal-true, like the existing `RunnerVersionTooOld`, and deliberately outside the gateway's `RunnerSetsDegraded` rollup: a declared label missing at GitHub is a configuration mismatch, not an outage. |
| `LabelsNotRegistered` / `LabelsRegistered` (Q726) | `RunnerLabelsIncomplete` | The True and False reasons of one condition, named for the state each reports. |
| `RunnerGroupNotFound` (Q712) | `Ready` | Fail-closed is the design, not an error path: falling back to the default group would widen the GitHub-side boundary at the moment an operator narrows it. |
| `WorkerImageBelowMinimum` / `WorkerImageCurrent` / `WorkerImageVersionUnknown` (Q715) | `RunnerVersionTooOld` | Three states rather than two. A digest-only or tenant-tagged image declares no version, and that reports `Unknown` rather than `False` because a custom image is where a stale runner hides. |

### Two that were decisions rather than ticks

**The `runnerGroup` name collides, and keeps the name.** It shares a word with the deprecated `v1alpha1` `RunnerGroup` CR, which is a different concept, and the field's godoc says so in its first sentence.
Two things settle it: GitHub's own term is "runner group" and ARC's `gha-runner-scale-set` chart already exposes `runnerGroup` for exactly this, so an operator migrating from ARC meets the name they expect; and the colliding CR comes out in the `v2.0.0` bundle ([Q264](../queue/Q264.md)), so the ambiguity is time-boxed.
`githubRunnerGroup` would trade ecosystem familiarity for a collision that resolves itself.

**The default is the wide group, and that is accepted rather than clean.** Unset inherits the gateway's default and then GitHub's own default group, which typically admits the whole organization, so the default is the less isolated value.
No narrower default exists to choose: GAG does not create runner groups.
The safety therefore sits where GAG does decide the outcome — an unresolvable name fails closed rather than falling back, and an undeclared group leaves an existing scale set where it is, so widening is always explicit.

One bound was not verified and does not need to be: `MaxLength=255` was not checked against GitHub's real limit for a runner group name.
Loosening a length bound later is non-breaking and tightening one is, so a generous bound is the safe error.

Nothing was deferred, so this review adds no gate row.

## The rc.1 validation verdict, 2026-08-14

`validate-release.sh v1.5.0-rc.1` **PASSED**, exit 0, and the cluster was confirmed back at 0 nodes by asking `ops.sh at-rest` rather than by reading the gate's own teardown line.
These are the receipts the notes' Validation section draws on.

| Leg | Result |
|---|---|
| deploy | RC deployed and CI routed to GAG in ~3m |
| e2e matrix | **75 passed, 0 failed, 12 skipped** in 7m28s ([run 31805454168](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31805454168)) |
| sizing, `NodeShare` | `sizingProfileState=Active`; worker CPU request derived to **1500m** where the templates ask 2 and 3 |
| sizing, `Throughput` | `Active`, `sampleCounts=[188]` |
| CRD smoke | blob signature `Verified OK`; all five v2 CRDs server-side applied and registered |

Artifact verification cleared separately and before the gate: 7/7 OCI signatures (five images, both charts), the `verify-blob` signature on the v2 CRD manifest, an SBOM attestation on the amd64 manifest, and a build-provenance attestation whose `buildSignerURI` ends `publish.yml@refs/tags/v1.5.0-rc.1` with a `sourceRepositoryDigest` equal to `ff3b3ef1`, the tagged commit.
Following 1.4's practice, the two identity checks were re-run against deliberately wrong identities and both failed: a `refs/heads/` regexp took cosign to exit 12 naming the real subject, and `unit-test.yml` as `--signer-workflow` took `gh attestation verify` to exit 1.
The provenance pass was read out of `--format json` rather than off the exit status, since that command prints nothing when its output is redirected.

`Throughput` actuating was not expected on this run.
The [runbook](../operations/release.md#validate-the-release-candidate-on-dogfood) treats it as reported-never-fatal because it needs ~20 samples per template container and the gate's matrix is about seven jobs.
It reported `Active` on 188 samples, because the sampler tracks every worker pod whatever `spec.sizing` holds and the aggregate re-seeds from the persisted `status.sizingRecommendation`, so the CI tenant's ordinary traffic had already earned them.

### The gate could not start, and the defect was its own

The first launch failed in preflight, before anything was spent, reporting the `e2e` pool as having no autoscale ceiling.
The cluster was configured correctly: GKE omits `minNodeCount` when it holds its 0 default, so the projected row led with an empty field and a default-IFS `read` slid the ceiling into the floor.
No IFS could have recovered it, because tab is IFS whitespace and `read` drops a leading run of it unconditionally, so the projection now asks for a comma.
Fixed in [#1498](https://github.com/actions-gateway/github-actions-gateway/pull/1498) and confirmed against the live cluster before the gate was re-run.

Two things about *when* it was found are worth keeping.
The reservation preflight merged 2026-08-12 and the previous dogfood run was 2026-08-09, so this code had never executed against a real cluster; both test doubles modelled a literal `0` the API never sends, which is why nine assertions over the arithmetic downstream of that read stayed green.
That is recorded as the null-vs-absent case in [testing.md](../development/testing.md#generate-a-fixture-with-the-producers-own-code-never-by-hand).
The class of defect only surfaces when a release is actually being cut, which is the least convenient moment for it.

### `release-sentinel.sh` reported a PASS that was not this run's

Recorded because it nearly ended the session with a false verdict on the gate that decides whether an RC may be promoted.

Launched while the gate was still in preflight, the sentinel read the `v1.4.0-rc.2` stream left in `tmp/` from 2026-08-09, found its terminal `passed` event, and reported `Gate: passed` with a next action of "report the result to the operator".
The stale RC tag, a 101-hour elapsed time and the placeholder `owner/repo/actions/runs/42` were all in its own report, and none of them stopped it.
A run that has not yet written its first event is indistinguishable, to the sentinel, from one that finished.
Worked around at the time by archiving the three stale files under `v1.4.0-rc.2` names, then fixed at the writer rather than at the sentinel, because the same stale stream had already misreported the gate's phase through `release-status.sh` when it was asked directly.
The gate now empties its stream before preflight instead of after it, so no reader can meet a spent run's terminal event.

The `stalled` event later in the run was **not** a defect.
It fired once the dispatched run was no longer live while the stream was quiet, which is exactly Q630's reconciliation behaving as designed; the gate was mid-transition into `e2e-stop.sh` when it was checked.
The quiet itself was the documented case where GitHub will not serve the job log: `heartbeat` stayed `null` for the whole 22-minute leg on a run that passed, and the run record was the signal that worked.

## The stable-tag pre-flight, 2026-08-14

The four items that bind at the stable tag rather than at the candidate, plus the curated notes they feed.
Recorded here because [release.md § Pre-flight](../operations/release.md#1-pre-flight) asks for a verdict, not a checkmark.

**The marketing reconciliation found one under-claim and one stale claim, and the stale one was found by a question rather than by the pass.**

Question 3 settles itself this cycle: the newest `gha-runner-scale-set` release is **0.14.2, from 2026-05-22**, which is exactly what all 17 competitor cells were stamped at on 2026-08-12.
Upstream has not moved since the stamps, so no cell has aged out and none needed re-reading.
That is the third state Q801 built the format for, doing its job on the first release to use it.

Question 1 found **Q726 on no marketing surface at all**.
Multi-label runner sets had no `features.md` line, and their only mention anywhere was a provenance footnote in `why-gag.md` explaining a stale ARC cell.
The capability is the one that backs the zero-edit migration claim the front door makes, which is what makes the omission worth recording rather than just fixing: the inventory missed the feature that a published claim rests on.
Everything else shipped this cycle already had a line, or was covered by a claim that Q844 made true rather than new.

Question 2 was run too thinly, and the miss is worth more than the correction.
Asked afterwards whether the two parity axes were actually closed, `why-gag.md` turned out to say a worker reaped while still `Pending` is re-run "on the classic tier", which Q766 had made both tiers in `v1.4.0`.
The page **understated the product by a full release**, and it did so directly above a link to the canonical matrix that says both.

The maintenance comment on that paragraph is why it survived: it asks for an update when a case is *added or removed* from the matrix, and this case was neither.
Its acquisition tier changed, which is the same drift and is not what the instruction names.
Both the claim and the instruction are corrected, and the under-claim half of this pass is the same shape one level up: a capability that moves tier changes what every surface may say about it, and nothing watches for that.
`make metric-tiers-check` now covers the metric half; prose keeps needing the eye.

**The operator-caveat pass found a missing migration note.** Q791 tightened scale-set name uniqueness from namespace-scoped to the whole GitHub scope, which rejects a configuration `v1.4.0` accepted, and it had reached `troubleshooting.md` and `tenant-onboarding.md` but not `upgrade.md`.
The gap matters because of *when* the rejection lands: admission runs on create and update, so nothing is re-validated at upgrade time and a colliding pair keeps running until someone re-applies either set, possibly a tenant who changed nothing.
`upgrade.md` now carries the note and the one-command check that finds a collision in advance.

Reading the report itself needed care worth recording.
`operator-caveats-since.sh` extracts new sections and new bullets by line, and the sentence-per-line adoption (#1357) landed inside this window, so its bullet half reported over a hundred items citing Q-numbers as old as Q114.
The **new sections** were the signal: four of them, mapping exactly onto Q844, Q715, Q726 and Q712.

**The roadmap needed nothing.** `make roadmap-check` is green and its near-term section holds only Q719, Q727, Q408 and Q564, all pointing at later releases; the 1.5 rows are correctly absent because they are closed and the gate fails a bullet naming a dead row.

**The announce-bar highlight is updated and verified rendered**, not just edited: `GAG_DOCS_RELEASE=v1.5.0 make docs-build` renders `v1.5.0 is here` followed by the new highlight.
`publish.yml`'s `announce-bar` job fails the release if the rendered banner does not name the tag, so this is a gate rather than a nicety.

**The notes are authored in [`docs/releases/v1.5.0.md`](../releases/v1.5.0.md)**, in-repo so each fix is a diff.
Every surface an operator can see was diffed mechanically rather than read off the changelog: two CRD fields, seven condition reasons, one metric, three Event reasons, and no chart values change, with nothing removed or renamed anywhere.

Two extraction traps bit and were caught.
A CRD property scan reported a field called `identity` that does not exist, having matched a wrapped godoc line.
An Event-reason scan keyed on `recordEvent(` missed two of the three additions, because `provisioner/` records through `RecordEvent(` instead; keying on the argument after the event type finds all three and cannot mistake the adjacent action string for a reason.

**Still outstanding at the tag:** the notes carry no `Container images` section, because the index digests do not exist until `publish.yml` has run.
[Step 4](../operations/release.md#4-record-the-published-digests) is where they are read, and the section is appended before `gh release edit --notes-file` publishes the body.

## Scope reopened 2026-08-14: what a question cost

The pre-flight above closed and the tag was the next step.
Then a question — *did we finally reach v1→v2 and classic→ScaleSet parity, and should that go in the notes, docs, or marketing?* — reopened the release on eight rows.
Recorded because the sequence, not the rows, is the lesson: each answer was measured, and each measurement found something the pass before it had not.

**Both axes are closed, and only one of them is a 1.5 story.** The [capability parity table](v2-ga.md#capability-parity-is-a-precondition-of-the-removal) reads Both tiers on all six rows, `features.md` carries no `partly classic-only` badge, and the [tier ledger](../operations/observability-metrics.md#acquisition-tier-reach) reconciles all 53 series (26 Both, 16 classic-only, 10 scale-set-only, 1 tier-neutral) under a gate.
The v1 to v2 axis closed mostly before the 1.4 tag; 1.5 contributed Q726 alone.

**Asking the question found the stale claim the pass had missed**, recorded in the pre-flight section above, and then the same reasoning applied one level up.
A completeness claim inherits the blind spots of its inventory, so "no capability is lost" cannot rest on a hand-kept list — that is the shape Q844 hid in, and [testing.md](../development/testing.md) states the rule outright.
Q776 made metric *series* derived and gated.
Q850 and Q851 extend that to condition reasons, Event reasons, and label values, which is the surface a completeness claim can honestly cover; the marketing claim waits for them.
**Q850 has since shipped**: 45 condition reasons and 26 Event reasons carry a tier under `make reason-tiers-check`, and its walk found two Events an operator could meet in `kubectl describe` with no runbook entry, but no condition reason single-tier by accident ([plan](archive/q850-reason-tier-ledger.md)).
Q851 has since shipped too, so all three signal surfaces are now derived and gated and nothing is left blocking the claim.
What the label-value pass found is why the claim should still be worded off the gates rather than off a summary of them.
Measured while scoping Q851: `eviction_retries_total` reads Both while `cause="vanished"` is scale-set-only, and `abandoned_run_force_cancels_total` reads Both while `outcome="identity_unknown"` is unreachable there.
The gate sees series, not values, so two live tier differences sit inside rows marked Both.

**Q851 shipped, and the count was wrong in both directions.** Deriving the label values out of the source found seven single-tier rows, not two: `cause="vanished"` on all three eviction series rather than one, and three of `worker_pods_reaped_total`'s seven reap reasons — `completed_pending` and `orphaned_running` read a completion annotation the classic tier never stamps, and `job_abandoned` rides the classic renew loop the scale-set tier does not have.
The operator-facing half was worse than the row said and in a different place: seven `Help` strings named a vocabulary the code had outgrown, or named none at all, and that is what an operator reads off `/metrics` with no docs open.
And a claim in three places was simply wrong — `worker_pods_reaped_total`'s `runner_set` label was documented as scale-set-only, in the `Help`, the reference table and a Grafana panel, while the code keys it on the owning kind, so a Classic-protocol `RunnerSet` carries it and `{runner_set!=""}` is not the scale-set filter the dashboard calls it.

The lesson repeats the section's: the hand-written claim was not merely incomplete, it was confidently wrong in the direction nobody re-reads.
Only three of the seven rows were derivable from file layout alone; the other four are held by a guard in a file both tiers run, so those rows cite the guard and the gate holds the citation to a real source file.
That is the honest floor for a completeness claim at this granularity — derived where the source can prove it, anchored to named code where it cannot, and never a bare list.

**Then a second question — operators do not read release notes, so what are this release's landmines?** Q849 was already admitted for it: Q791's guard runs at admission, which never re-validates a pair that already exists.
Two more came out of asking the same thing of every caveat.
Q852 is the sharpest and was not on anyone's list: `helm upgrade` never applies CRDs, and a structural schema prunes unknown fields with no error, so a skipped step 1 leaves `spec.runnerGroup` declared and inert — a security control that silently does nothing.
It generalises past this release, since every future field has the same exposure.
Q853 covers the other silent one, Q713's classic-tier span change, which renamed nothing and shifts every dashboard and cost figure at upgrade.

Worth keeping: the two caveats that are *not* landmines, Q844's budget spend and Q726's inert appended label, each ship a condition or Event that fires exactly when an operator would otherwise be confused.
That is the pattern Q849, Q852 and Q853 copy.

**Finally, the bar for rc.2: pass without anything needing interpretation.** Q854, Q855 and a promoted Q773 come from that.
Q854 is the one that already cost a run — a single `HTTP 401` at 645s of the settle wait killed the first `v1.5.0-rc.1` attempt after 43 good polls, and the gate has no retry anywhere, while `download-verified.sh` has had jittered backoff since Q829.
Q855 is the 22-minute blind spot on the leg that passed.
Q773 was filed on 2026-08-09 and says the runbook calls a normal `Throughput: Active` result a surprise; it happened again on rc.1, and this document called it unexpected before the row was found.

**One gate changed on the way.** Labelling release-process work `1.5-gate` made `roadmapcheck` demand a public roadmap bullet for a `gh` retry and a runbook line.
Rule 7 now obliges a bullet only for a gated row carrying `feature` or `security`, so a release can wait on process work without advertising it to adopters ([maintaining-backlog.md](../development/maintaining-backlog.md#a-gate-label-and-its-roadmap-bullet-are-two-commits-and-the-first-one-is-red)).

## The rc.2 pre-flight, 2026-08-15

All eight rows admitted when scope reopened have shipped, so the Queue carries no `1.5-gate` row.
Four of the commits behind them change the shipped binaries, which is what makes a second candidate necessary rather than optional: Q849 and Q852 (`gmc`), Q851's metric `Help` strings (`agc`), and Q811, a fix admitted outside the gate set.

**`main` was green on the tag target.** Unit, integration and `security-scan` passed on `53283cd9f`, and e2e passed on that same SHA through the merge-queue run for #1541 rather than through the `push` lane, whose run for it sat behind the previous push's per-ref concurrency group.
`make check` was green locally.
Reading the `push` run's pending state as "e2e has not run" would have been wrong, and is the reason the SHA rather than the lane is what to check (Q675).

**The API surface review was re-run, because the recorded [1.4→rc.1 verdict](#pre-flight-the-api-surface-this-tag-publishes) no longer covers the tag.** Q849 published surface `v1.5.0-rc.1` does not carry.
**Verdict: ship as-is.**

| Addition | Carried on | Why the shape is right |
|---|---|---|
| `ScaleSetNameCollision` (Q849) | `ActionsGateway` condition | Abnormal-true and outside the `Ready` rollup, like `RunnerVersionTooOld` and `RunnerLabelsIncomplete`: a name shared with another tenant is a configuration mismatch the platform admin resolves, not an outage of this gateway. Advisory is the design, not a weaker enforcement — GAG cannot pick which tenant loses the name, and failing closed would take down the tenant that was there first. |
| `ScaleSetNameShared` / `ScaleSetNamesUnique` (Q849) | `ScaleSetNameCollision` | The True and False reasons of one condition, each named for the state it reports, following `LabelsRegistered`/`LabelsNotRegistered` from Q726. |
| `scale_set_name_collision` (Q849) | GMC gauge | A gauge rather than a counter because the condition describes a standing state, and one an operator must be able to alert on rather than only meet in `kubectl describe`. |
| `eviction_rerun_withheld_total` (Q811) | AGC counter | Keyed by `reason`, so a re-run the AGC declined is distinguishable from one it never reached — the withheld and failed paths stay separate series. |

No wire field, enum constraint, default, or label/annotation key changed, and nothing is wire-breaking: the additions are status vocabulary and telemetry.
Nothing was deferred, so this review adds no gate row.

**The marketing reconciliation was re-run too, and it had the same staleness the API review did.** The [stable-tag pass](#the-stable-tag-pre-flight-2026-08-14) ran 2026-08-14, before the reopened rows shipped, so Question 1 had never been asked of them.

It found the cross-tenant scale-set name guard on **no marketing surface at all**: zero hits across `features.md`, `README.md`, `index.md` and `why-gag.md`, checked on five phrasings and both anchor targets.
Q791 shipped 2026-08-13, so the 2026-08-14 pass missed it rather than predating it; Q849 shipped after.
`features.md` now carries it under Tenant isolation, stating both halves — admission refuses the pair GitHub-scope-wide, and a pair carried in from an older release is reported as a condition, an Event, and an alertable gauge.

Two adjacent findings came out of writing that entry.
The `new in 1.5` badge convention arrived with Q852 and marked exactly one capability, while `v1.4.0`'s `features.md` carried no such badge at all, so a reader scanning the page concluded the release's one addition was a startup check.
The runner group, multi-label registration and the runner-version warning now carry it too, and the page's own legend said **three** badges while four were in use, having never been updated when the fourth was introduced.

**Two claims were checked against ARC and *not* written, which is the point of the pass.** `gha_job_startup_duration_seconds` and `gha_job_execution_duration_seconds` both exist in [`cmd/ghalistener/metrics/metrics.go`](https://github.com/actions/actions-runner-controller/blob/9bb16ae49d0ce585d8e682aa7e2668a6e832d5d8/cmd/ghalistener/metrics/metrics.go) at 0.14.2 (`9bb16ae`), so a "job duration metrics are unique to GAG" claim would have been false on publication, the same failure the 2026-08-06 review found eleven times.
What GAG has that ARC does not is the pod-creation latency series and a pod-lifetime span chosen for cost attribution, which is a different claim and a narrower one.
The `RunnerVersionTooOld` claim stays off the comparison table for a different reason: the ARC-side fact is about the `actions/runner` binary and GitHub's brownout schedule rather than about ARC, so an ARC version stamp would attest to the wrong thing.
The [NOTE in the release notes](../releases/v1.5.0.md) already carries the claim without naming a competitor, and cannot go false when ARC moves.

**The notes were interrogated again, and the pass found a claim that had gone false.** [`docs/releases/v1.5.0.md`](../releases/v1.5.0.md) still told an operator that a colliding scale-set name is never re-validated at upgrade time and lands at some later unrelated apply.
Q849 had made that untrue four commits earlier, on the same day the notes were last touched.
The upgrade note in `docs/operations/upgrade.md` was correct throughout; only the release notes were stale, which is the direction that reaches an adopter first and is checked last.

That is the same shape as the [stable-tag pre-flight's](#the-stable-tag-pre-flight-2026-08-14) under-claim, one release surface over, and it is the argument for re-running the notes interrogation at **every** candidate rather than only the first: the notes go stale against the product between candidates, and nothing gates prose.

## Re-deriving the highlight claims, 2026-08-15 (Q876)

Every claim in the six `v1.5.0` highlights, checked against the code it describes rather than against a neighbouring doc.
Q876 was admitted because one of the six had been inverted on both halves and survived a full pre-flight and `rc.1`; the other five had never been checked, and #1542 rewrote the section again afterwards.

**One defect, and it was a number rather than a direction.** The acquisition-tier ledger claim read *53 series, 26 both tiers*.
The gated ledger holds **54 and 27**: Q811 added `eviction_rerun_withheld_total`, tiered Both, after the figure was written.
It appeared twice, in the highlight and in the API-surface section, and the arithmetic stayed self-consistent in both places (26+16+10+1 = 53), which is why nothing caught it — a stale total and a stale addend move together.

The other five hold, and the mechanisms behind them are where the checking went:

| Claim | Re-derived from |
|---|---|
| An unresolvable runner group fails closed rather than falling back | `scalesetlistener/listener.go:765` returns `ErrRunnerGroupNotFound` on `!ok`; the default-ID return above it is reached only by an *undeclared* group, which is the documented widening-is-explicit case |
| An adopted scale set is moved into its declared group | `listener.go:798-810`, which reads the adopted set's group and moves it |
| `job_duration_seconds` blast radius: two SLOs, one severity-critical alert, four recording rules, both dashboards | Four recording rules across the two series, and two alerts referencing them of which exactly one is `severity: critical` (`ActionsGatewayPodCreationLatencyP99`; the P95 is `warning`), so the singular is right. Two dashboard files. |
| A run whose pod is gone at startup is re-run | `recoverOrphanedScaleSetWorkers` runs "once per process" over pods "already gone when this process started" |
| `ScaleSet` is the only tier `v2beta1` exposes | `api/v2beta1/runnerset_types.go` carries no `AcquisitionProtocol` at all, against seven references in `v2alpha1` |

**What this says about the counts, and the reason the row was worth its cost.** Every wrong number in this release has been a *derived* one going stale, not a mistyped one: the ledger figure here, the 120-commits and 9-shipped figures #1542 re-derived, and the `features.md` entry that outgrew its word cap.
The gates hold the ledger to the source and the notes to nothing, so a figure copied out of a gated artifact is correct exactly until that artifact moves.
Re-deriving at the tag is what closes that window, and it is cheap enough to repeat.

## Candidates not yet accepted

Held here so the reasoning is not lost, not committed to the release:

- **Fold the scale-up token bucket into the advertised capacity.** The bucket is waited on at `provisioner.go:532` and `:793`, after the claim, with the job holding its GitHub lock, which the CRD godoc states outright (`api/v2beta1/runnerset_types.go:394-400`).
  Expressing free tokens as a fourth `min()` rung in `AdvertiseCapacity` would make the anti-stampede claim structurally honest.

*(Multi-label runner sets were held here too, and were accepted on 2026-08-09: the row is Q726, now labelled `1.5-gate`, and the gap inventory it belongs to is [arc-parity.md](arc-parity.md).)*

## In scope: reconcile the marketing surfaces

1.5 carries the marketing-claim work identified by the 2026-08-06 competitive review.
It touches no shipped artifact, so individual corrections can land continuously as docs PRs, but the release does not tag until the reconciliation has been done and its verdict recorded here.

Three bodies of work, in dependency order:

1. **Corrections.** Claims that are wrong or stale today.
   The largest are the ARC-side cells of the `why-gag.md` comparison table: 11 of them assert a gap with no ARC version and no measurement date, and two went false at datable upstream releases (0.13.1 fixed quota-blocked pod creation; 0.14.0 added multi-label scale sets, which GAG does not have).
   Also the listener-footprint wording, which is substantively right but uses "cluster IP" to mean a pod IP, inviting a reader to check `Service` objects and conclude the table is wrong.
2. **Under-claims** ✅ **reconciled 2026-08-12 (Q821).** The count below is wrong and the [inventory](competitive-analysis-2026-08.md#under-claims-not-yet-fixed) now records why: three of the five outstanding items reached *no* surface at all rather than only `features.md`, and one had already shipped to all three.
   The message-queue conclusion-durability body, the durability programme with its 16-hour/82-spot-node-hour incident, and the GitHub protocol dependency register are now on `features.md` and `README.md`; the worker-quota footprint arithmetic is the one still outstanding.
   The original framing, kept because it is what the release was scoped against: Nine capabilities shipped and appear only in `features.md`.
   The largest are no-PEM workload identity (the GitHub App private key never enters the cluster), the live-validated per-tenant egress IP result, and the durability programme whose motivating incident was five worker pods running 16 hours on 82 spot node-hours.
3. **Structure.** Q713 blocked any number-bearing claim, since the shipped tier emitted no latency or duration series to measure; it landed 2026-08-11, so latency and cost claims are now measurable on the default tier.
   Q712 blocked publishing tenant-isolation marketing and landed the same day, so that claim is available too: state it as the runner-group *binding*, not as GAG controlling repository access, which stays the platform admin's at GitHub.

The recurring form of this is now [release.md § Pre-flight](../operations/release.md#1-pre-flight), which asks the same three questions before every tag.
1.5 is the first release to run it, and the backlog it produced is the reason the step exists.

## The scoping question, settled

Whether the comparison table keeps its verdict-table shape at all.
The 2026-08-06 review traced the undated cells to the format: a green-check/red-X table needs a definite cell in every row, and the working notes it was built from had marked most competitor-side facts as unverified.
The format had nowhere to put "we believe this but have not checked it", so unverified became a red X. Patching cells does not fix that; either every competitor-side cell carries a version and a date, or the page stops rendering competitor claims as verdicts.

**Settled 2026-08-11: the table keeps its shape and gains a third state.** A competitor-side cell carries a verdict only when it also carries an ARC version and a measurement date; without both it renders as unverified.
Tracked as Q801, and **shipped 2026-08-12**: `make comparison-stamps-check` enforces the rule, `.gag-unverified` is the state, and all 17 ARC cells were re-read at 0.14.2 / `9bb16ae` and stamped.
The rule is [documentation-standards.md § A competitor-side verdict carries its own stamp](../development/documentation-standards.md#a-competitor-side-verdict-carries-its-own-stamp); the per-cell evidence is [in the competitive analysis](competitive-analysis-2026-08.md#per-cell-evidence-for-the-arc-column-2026-08-12).

Measured the same day, which is what makes this a format decision rather than a cleanup: **15 of the 17 comparison rows carry neither an ARC version nor a date.** Only two do.
Dating them is the work either way, so the question is only what the page asserts while that is outstanding.

**How the dating actually went, 2026-08-12.** All 17 came back with a verdict rather than some falling to unverified, because re-reading them at one pinned commit was cheaper than the row assumed: eleven of the twelve gap-asserting cells were settled from two chart `values.yaml` files, four controller source files and one recursive file listing.
Two corrections fell out of it.
The quota row now names the 30 s retry beside the 10 min registration burn, both read at `9bb16ae`, and the "three rules, all `patch`" reading behind the pre-claim-seat argument is two rules at that commit.
The unverified state therefore ships with no cell using it, which is the right way round: the format can express doubt before it needs to, and the gate is what stops the next unmeasured cell rendering as a verdict.
The one claim that stayed weaker than the rest is the auto-re-run negative, which is a keyword-and-surface reading rather than an exhaustive one and says so in the evidence table.

The argument is the asymmetry between the two ways of being wrong.
Two cells have already gone false at datable upstream releases, and a reader who checks one and finds it wrong has no reason to trust the other sixteen; the page's whole value is that a sceptical evaluator can spot-check it.
An unverified cell costs nothing by comparison, and next to cells that do carry a version and a date it reads as rigour rather than as a gap.

It also makes the debt visible instead of hidden.
Today an unmeasured claim and a measured one are indistinguishable on the page, so staleness accumulates silently; under the third state an expiring cell degrades to unverified rather than to wrong.
That turns [release.md § Pre-flight](../operations/release.md#1-pre-flight) from "re-verify everything" into "re-check what went stale", and the rule is mechanical enough to gate: no version and date, no verdict.

Not chosen, and why.
Dating all 15 first keeps the strongest-looking page but blocks every correction behind one large measurement pass, and leaves the format unable to express doubt the next time ARC moves.
Dropping the verdicts entirely cannot go false, but it gives up the at-a-glance comparison on the page most likely to be read by somebody evaluating alternatives.

One caveat on the record: this was decided from the markup and the row counts, not from the rendered page at a real viewport.
If the unverified state reads badly in practice, that is a design finding worth acting on rather than a decision to relitigate.
