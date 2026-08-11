# Release 1.5 Milestone Definition

> **Status: scope opening 2026-08-06.** [Release 1.4](release-1.4.md) is already scoped and its gating rows are fixed; 1.5 is where work identified after that line lands.
> Four gating Queue rows so far, labelled `1.5-gate`: [Q712](../STATUS.md#Q712), Q713, and Q726, admitted 2026-08-09 from the candidate list below, plus Q715, admitted the same day off an external date and [shipped 2026-08-11](#q715--the-runner-version-reported-to-github-is-a-constant-and-the-too-old-warning-is-classic-only).

## Why these gate a release rather than riding along

Q712 and Q713 came out of the 2026-08-06 competitive analysis, and both were defects in capabilities the project already claims, not new features.
That is what makes them release-gating: shipping another minor while either is open means shipping a claim the product does not honour.

Q726 is the same shape read from the other side.
It is the one [ARC parity](arc-parity.md) gap that breaks the zero-edit migration claim the front door makes, so it gates for the same reason: the claim is published and the product does not honour it for a `runs-on` array.

### Q712 — the runner-group binding is declared and never wired

`RunnerGroupName` exists on the scale-set listener config (`cmd/agc/internal/scalesetlistener/listener.go:372`) and is resolved when non-empty (`:690`), but the sole production construction site in `cmd/agc/internal/controller/runnerset_scaleset.go` never sets it.
Every scale set therefore registers into the installation's default runner group.

The GitHub runner group is the **forge-side authorization point** for which repositories may target which runners.
With every tenant's scale set in one group, a repository outside a tenant can name that scale set in `runs-on` and route work into the tenant's namespace, quota, and egress IP.
GAG's pod-level isolation is unaffected; what is unbounded is *who can cause a job to run there*.

Scope note, to be settled in the work rather than assumed: exploitability depends on the default group's repository-access configuration at GitHub, which GAG does not manage today.
The row covers wiring the field, deciding whether the gateway should also assert group membership, and documenting what the platform admin still owns at GitHub.

This is also the one place ARC is ahead on GAG's own core claim: its `gha-runner-scale-set` chart exposes `runnerGroup` as a first-class value.

### Q713 — the shipped tier emits no duration or latency series ✅ landed 2026-08-11

`waitForCompletion` and the pod waiter ran only inside `provision()` (`cmd/agc/internal/provisioner/provisioner.go:557` and `:583`). `ProvisionScaleSetWorker` registered neither, and `v2beta1` is ScaleSet-only, so the tier every new tenant runs emitted both series empty.

The blast radius was entirely downstream: two Appendix A SLOs, a severity-critical alert, four recording rules, both shipped Grafana dashboards, the runbook, and the cost-attribution guide all read blank.
The failure mode is worst-case for a pre-adoption project: the first external operator to apply the shipped `PrometheusRule` sees a product that looks broken, and [go-to-market](go-to-market.md) §8 records that first impressions from cold traffic are one-shot.

**How it was fixed.** Both observations moved onto the shared pod informer, which sees a scale-set worker pod and a classic one identically.
The latency observation had been gated behind a registered waiter, the thing a fire-and-forget provision never creates, so the tier seam was the gate itself rather than a missing call site.

That made the span a decision rather than a detail, since the two tiers had no common "acquisition" moment to measure from. `job_duration_seconds` is now **worker pod lifetime** on both tiers: creation to the last container finishing, or to the deletion request for a worker removed mid-run.
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

## Candidates not yet accepted

Held here so the reasoning is not lost, not committed to the release:

- **Fold the scale-up token bucket into the advertised capacity.** The bucket is waited on at `provisioner.go:532` and `:793`, after the claim, with the job holding its GitHub lock, which the CRD godoc states outright (`api/v2beta1/runnerset_types.go:394-400`).
  Expressing free tokens as a fourth `min()` rung in `AdvertiseCapacity` would make the anti-stampede claim structurally honest.
- **Gate intake on workers that schedule but never start.** `podUnschedulable()` keys on `PodScheduled=False`, so an `ImagePullBackOff` worker trips no rung: it binds to a node, never starts, and each claim spends a JIT record and a lock until `pendingPodDeadline`.
- **Assert a worker pod cannot reach the cloud metadata server.** Three docs advise denying `169.254.169.254/32`, Q226 measured HTTP 200 from inside a Kata guest, and no test names the address. *(Multi-label runner sets were held here too, and were accepted on 2026-08-09: the row is Q726, now labelled `1.5-gate`, and the gap inventory it belongs to is [arc-parity.md](arc-parity.md).)*

## In scope: reconcile the marketing surfaces

1.5 carries the marketing-claim work identified by the 2026-08-06 competitive review.
It touches no shipped artifact, so individual corrections can land continuously as docs PRs, but the release does not tag until the reconciliation has been done and its verdict recorded here.

Three bodies of work, in dependency order:

1. **Corrections.** Claims that are wrong or stale today.
   The largest are the ARC-side cells of the `why-gag.md` comparison table: 11 of them assert a gap with no ARC version and no measurement date, and two went false at datable upstream releases (0.13.1 fixed quota-blocked pod creation; 0.14.0 added multi-label scale sets, which GAG does not have).
   Also the listener-footprint wording, which is substantively right but uses "cluster IP" to mean a pod IP, inviting a reader to check `Service` objects and conclude the table is wrong.
2. **Under-claims.** Nine capabilities shipped and appear only in `features.md`.
   The largest are no-PEM workload identity (the GitHub App private key never enters the cluster), the live-validated per-tenant egress IP result, and the durability programme whose motivating incident was five worker pods running 16 hours on 82 spot node-hours.
3. **Structure.** Q713 blocked any number-bearing claim, since the shipped tier emitted no latency or duration series to measure; it landed 2026-08-11, so latency and cost claims are now measurable on the default tier.
   Q712 blocks publishing tenant-isolation marketing, because the runner-group binding it would be claiming is unwired.

The recurring form of this is now [release.md § Pre-flight](../operations/release.md#1-pre-flight), which asks the same three questions before every tag.
1.5 is the first release to run it, and the backlog it produced is the reason the step exists.

## Open question for scoping

Whether the comparison table keeps its verdict-table shape at all.
The 2026-08-06 review traced the 11 undated cells to the format: a green-check/red-X table needs a definite cell in every row, and the working notes it was built from had marked most competitor-side facts as unverified.
The format had nowhere to put "we believe this but have not checked it", so unverified became a red X. Patching cells does not fix that; either every competitor-side cell carries a version and a date, or the page stops rendering competitor claims as verdicts.
