# Scale-Set Eviction Recovery

Port eviction recovery to the scale-set acquisition tier, then decide whether the AGC can release an evicted job's lock early instead of waiting out GitHub's timeout.

Filed 2026-07-25 from a feasibility review of [issue #811](https://github.com/actions-gateway/github-actions-gateway/issues/811), which proposed the fast-release optimization.
The review found the optimization sits on top of a prerequisite that does not exist yet, so this plan covers both and sequences them.

## Why this matters

`v2beta1` is ScaleSet-only, so the scale-set tier is the product's only future acquisition path.
Classic is on the removal track for `v2.0.0` ([release-1.3.md](release-1.3.md)).
Eviction recovery works on classic and does not exist on scale-set, which means the capability is scheduled to disappear with the protocol it was built for.

Worker right-sizing ([runner-sizing-profiles.md](runner-sizing-profiles.md)) compounds this.
Tighter memory limits raise OOM-kill probability, which is one of the two ways a worker pod gets evicted.
The 1.3 headline feature increases exposure to a gap that only the deprecated tier covers.

## Status

| Phase | State |
|---|---|
| 1 — measure the real baseline | ❌ Open, tracked by Q396. Gates Phase 3 only. |
| 2 — port eviction recovery to scale-set | ✅ **Shipped 2026-07-26** (Q417). See [Phase 2 as built](#phase-2-as-built). |
| 3 — fast release | ❌ Open, tracked by [Q418](../STATUS.md#Q418), still gated on Phase 1. |

Phase 2 did not wait on Phase 1, as planned: it closes a functional regression rather than an optimization gap, so it was correct to land regardless of what the baseline measurement says.

## What exists today

### Classic: full recovery, no fast release

`provision()` waits for pod terminal state and branches on eviction at [provisioner.go:528](../../cmd/agc/internal/provisioner/provisioner.go):

```go
if phase == corev1.PodFailed && reason == "Evicted" {
    p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay)
}
```

[`handleEviction`](../../cmd/agc/internal/provisioner/eviction.go) reserves a slot from the sharded per-run retry budget (Q106), waits `evictionRetryDelay`, and calls `rerun-failed-jobs`.
Renewal has already stopped, so GitHub concludes the job when the lock lapses.

### Scale-set: no recovery at all

[`ProvisionScaleSetWorker`](../../cmd/agc/internal/provisioner/provisioner.go) is fire-and-forget by design: the runner pulls and completes its own job through its own session, so the AGC neither hands off a payload nor blocks on completion.
It never calls `waitForCompletion`, so it never observes `PodFailed`/`Evicted`, never reserves a retry slot, and never calls the rerun API.

`handleEviction` is reached from exactly one call site, the classic one above.

An evicted scale-set worker today therefore gets no fast release **and no rerun**.
The job fails and a human reruns it.
Q264's transition plan listed eviction retry under "Reworked, carried over" ([archive/q264-scale-set-protocol-phases.md](archive/q264-scale-set-protocol-phases.md) §3); that port was designed, not implemented.

### The rerun target was unidentified on scale-set — resolved 2026-07-26

`rerun-failed-jobs` needs `owner`, `repo`, and `run_id`.
Classic extracts all three from the acquire payload via `jobMetaFrom`.
Scale-set has no payload, and the provisioner passed an empty `jobMeta{}` when building the worker pod.

The modeled [`JobMessage`](../../scaleset/types.go) carried `messageType`, `runnerRequestId`, `acquireJobUrl`, `jobId`, `runnerId`, `runnerName`, and `result` — no run ID, no repository.
That was a gap in **GAG's model**, not in the wire.

**What settled it.** The official Public-Preview `actions/scaleset` client — whose wire types this package deliberately mirrors — embeds `JobMessageBase` in `JobAvailable`, `JobAssigned`, `JobStarted`, and `JobCompleted` alike, and that base carries `ownerName`, `repositoryName`, `workflowRunId`, `jobWorkflowRef`, `jobDisplayName`, and `eventName` ([types.go](https://github.com/actions/scaleset/blob/main/types.go)).
The identity was always on the wire; GAG simply did not decode it.

Corroborating evidence from GAG's own live probe: Investigation E observed `scaleSetAssignTime` on a real `JobAssigned` from the dotcom broker-host backend ([archive/q264-scale-set-protocol-phases.md](archive/q264-scale-set-protocol-phases.md) §2a-3).
That is another `JobMessageBase` field this client did not model, so the raw body demonstrably *is* a `JobMessageBase` and not a narrower shape.

**Measured live 2026-07-26 — the fields are on the wire.** A repo-scoped probe run against `actions-gateway/github-actions-gateway` observed a real `JobAssigned` from the dotcom broker-host backend carrying every field the port depends on:

```json
{"messageType":"JobAssigned","repositoryName":"github-actions-gateway",
 "ownerName":"actions-gateway","workflowRunId":30225287326,
 "jobDisplayName":"probe-fast","eventName":"workflow_dispatch",
 "jobWorkflowRef":"actions-gateway/github-actions-gateway/.github/workflows/scaleset-probe.yml@refs/heads/main",
 "requestLabels":["gag-probe-scaleset"],"scaleSetAssignTime":"2026-07-26T23:29:36Z",
 "jobId":"bd59f43d-2e1a-567a-b5a4-c147b94d0766","runnerRequestId":0}
```

The probe's verdict line read `run identity present on JobAssigned — scale-set eviction recovery has a rerun target (Q417)`, resolving to `owner=actions-gateway repo=github-actions-gateway runId=30225287326`.

The decisive detail is that `workflowRunId` is **exactly** the run id of the workflow dispatched to produce the job ([run 30225287326](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30225287326)).
That is not merely "a number arrived in the field" — it is the same run `rerun-failed-jobs` addresses, which was the whole question.
The wider `JobMessageBase` shape came with it (`eventName`, `jobWorkflowRef`, `requestLabels`, `queueTime`, `runnerAssignTime`, `finishTime`), so the parity argument above held.

**What this does and does not change.** The identity-optional design stays exactly as built.
One observation, on one backend, is not a guarantee for GHES, for a different event type, or for a future protocol revision — and the `ok` branch costs one `if`.
What it changes is the confidence ordering: the fields are measured rather than inferred, so `actions_gateway_eviction_recovery_identity_unknown_total` should read zero in practice, and a non-zero value is a real regression signal rather than an expected state.

### Reproducing the run

The probe reports the verdict itself, so a run answers the question rather than merely containing the answer — grep the output for `run identity present` or `GAP`.
It needs live GitHub App credentials; the private key stays in the Keychain and reaches the probe as a file path, never as an env-var value or a process argument ([github-app-credentials.md](../development/github-app-credentials.md)).

Two corrections to the obvious recipe, both learned the hard way:

- **Register repo-scoped, not org-scoped.** This repo is public and the org's `Default` runner group has `allows_public_repositories: false`, so an org-scoped scale set never receives the job — §2a-6 burned two three-minute windows on exactly this.
  A repo config URL bypasses runner groups entirely.
  Do **not** "fix" it by enabling public repositories on the runner group: that exposes self-hosted runners to fork pull requests.
- **Dispatch first, then start the probe.** A job queued against a not-yet-existing scale-set label waits server-side and is assigned the moment the scale set registers (~1 s in this run), which removes the three-minute-window race the probe's own `dispatch … NOW` prompt implies.

```bash
gh workflow run scaleset-probe.yml --repo actions-gateway/github-actions-gateway
```

```bash
KEY_FILE=$(mktemp -t gag-probe-key.XXXXXX) && trap 'rm -f "$KEY_FILE"' EXIT INT TERM && security find-generic-password -a actions-gateway-test -s github-app-private-key -w | xxd -r -p > "$KEY_FILE" && GITHUB_APP_ID=3752347 GITHUB_APP_INSTALLATION_ID=135739122 GITHUB_ORG_URL=https://github.com/actions-gateway/github-actions-gateway GITHUB_APP_PRIVATE_KEY="$KEY_FILE" PROBE_SCALESET_TEST=true PROBE_SCALESET_JOB_TEST=true go run -C cmd/probe . 2>&1 | grep -E 'run identity present|GAP|job test message'
```

**Cancel the dispatched run afterwards** (`gh run cancel <id>`).
The probe deletes its throwaway scale set on exit, so any job still queued against that label is stranded until GitHub times it out — the fixture queues two jobs and the probe consumes neither.

Phase 2 was therefore built **identity-optional** rather than assuming presence: `JobMessage.RunIdentity` returns an `ok` flag, an incomplete identity is refused rather than defaulted, and the absence is counted (`actions_gateway_eviction_recovery_identity_unknown_total`) and surfaced as a Warning Event.
The cost is one branch; the benefit is that if the fields ever do not arrive, the failure is loud and localized instead of a rerun posted against run `0`.
That also demotes the live confirmation from a **gate** to a **verification**: it is worth doing on the next dogfood session, and nothing waits on it.

## The baseline number is confounded

Issue #811 opens from the U5 probe's ~9.5 minutes and asserts the delay is "purely the job lock's TTL lapsing."
The probe does not support that reading.
From §2b-5 of the archived Q264 phases doc, the runner was SIGKILLed at 16:19:06Z and GitHub concluded `failure` at 16:28:40Z, and the doc itself notes this coincided with the job's 10-minute `timeout-minutes` boundary.

Three mechanisms are indistinguishable in that measurement: the lock TTL, GitHub's own liveness detection, and the workflow timeout.
Optimizing against an unattributed number risks building a mechanism that removes none of it.

Q396 already tracks a clean dogfood benchmark for classic.
Extending it to scale-set, with no `timeout-minutes` confound, is the cheapest gate on the whole effort.

## Why classic-only is not worth shipping

The fast-release mechanism `#811` proposes does exist on classic: `broker.Client.CompleteJob` posts a terminal `TaskResult` with the job-scoped token the AGC already holds for renewal, and its 404/410 handling is already a documented benign no-op.

Wiring it is real work rather than a one-liner.
`handleEviction` receives only `owner`, `repo`, `runID`; the `planID`, `runServiceURL`, and job token live in the listener (`cmd/agc/internal/listener/job.go`).
`SiblingDelivery` already carries the right shape, so the plumbing is tractable, but it crosses a layer boundary and moves a job-scoped credential into the provisioner.

Against a tier scheduled for removal at `v2.0.0`, that cost buys a capability that ships and then is deleted.
Do not implement classic-only.

## Plan

Three gates, cheapest first.
Each produces a recorded answer whether or not the next phase proceeds.

### Phase 1: measure the real baseline (gate)

Tracked by Q396, whose scope now covers both tiers.
Evict a worker mid-job with no `timeout-minutes` set and timestamp eviction, GitHub's conclusion, and the annotation text.

**Decides:** whether there is meaningful latency to remove, and which mechanism owns it.
If GitHub's own detection dominates, the fast-release idea is dead and only Phase 2 proceeds.

### Phase 2: port eviction recovery to scale-set

Independently valuable, and correct to do regardless of Phase 1's outcome, since it closes a functional regression rather than an optimization gap.

1. **Establish run identity.** Dump a raw `JobAssigned` message body and check what GitHub sends beyond the modeled fields.
   If run ID and repository are absent, fall back to correlating `jobId` with the workflow-run API, or stamping identity onto the worker pod at provision time from whatever the listener does know.
   This step gates the rest of the phase.
2. **Detect the eviction.** Choose between a pod watch (mirrors classic, keeps the existing terminal-phase logic) and keying off the queue's terminal `JobCompleted` (the signal Q264 §2b-6 identified, but which arrives only after the very delay this plan is trying to remove).
   The pod watch is likely correct precisely because it is the early signal.
3. **Reuse the retry budget.** `reserveEvictionRetry` and the sweeper are protocol-agnostic.
   Keep the Q106 sharded-reservation invariant authoritative: at most `maxEvictionRetries` reruns per run, across both tiers.

### Phase 2 as built

Shipped 2026-07-26 (Q417).
Each planned step and what it became:

1. **Establish run identity** — resolved above.
   `scaleset.JobMessage` now models `ownerName`, `repositoryName`, `workflowRunId`, and `jobDisplayName`, with `RunIdentity()` returning the `(owner, repo, run_id, ok)` triple.
   The listener passes it through `scalesetlistener.Job`; `ProvisionScaleSetWorker` stamps it as the existing `actions-gateway.com/run-id` / `/repository` annotations, so both tiers share one worker-pod annotation vocabulary.
   The `scalesettest` fake now delivers the fields too, because the real backend does — a fake that answered with them empty would have hidden the gap until a live run.
2. **Detect the eviction** — pod watch, not the terminal `JobCompleted`, and via the **owning reconciler** rather than a per-job goroutine.
   The reconciler already watches worker pods for phase changes and already lists them every reconcile to reap them, so detection costs one cached List.
   Choosing the reconciler over a goroutine is the same call Q420 made and for the same reason: a fire-and-forget tier has no process-scoped place to keep the state, so the pod has to carry it, and recovery then survives an AGC restart between the eviction and the rerun.
   `RecoverEvictedScaleSetWorkers` runs **before** the reaper, since the reaper would otherwise delete the evidence.
3. **Reuse the retry budget** — unchanged.
   `handleEviction`, `reserveEvictionRetry`, the sharded per-`run_id` lock, and the sweeper are all shared; the only addition is a `tier` argument that labels the metrics.
   The Q106 invariant now holds *across* tiers, which the concurrency regression test exercises by driving half its evictions down each path.

Against the design constraints listed below:

| Constraint | How it was met |
|---|---|
| Do not double-report | `PodFailed` + `reason == "Evicted"` only — the kubelet's node-pressure kill, the one case where nothing in the pod ran. Made explicit in `evictedAwaitingRecovery` rather than left to the rerun call's benign 404/410 handling. Classic pods are excluded by the `acquisition-protocol` label, so one eviction never spends two budget slots. |
| Cover deletion, not only terminal phase | Deliberately **not** covered, with the reasoning recorded rather than inherited: a graceful deletion (drain, reaper) is exactly the case Q385's SIGTERM relay owns, and re-running it would double-report. The gap that remains is a *force* delete with no grace, which classic shares — see the residual note in [v2-ga.md](v2-ga.md#phase-3--the-coupled-removals). Q421 measured the drain path on 2026-07-27 and confirmed the exclusion holds on both tiers: a drained pod publishes no terminal phase at all, so nothing reaches this predicate and no rerun fires. Whether that *should* stay true was [Q459](archive/q459-drained-worker-recovery.md), decided 2026-07-29: **close, gated on `deletionTimestamp`** — a drained worker carries the mark, a cancelled or genuinely failed one does not. Q502 shipped that implementation, recovering the terminal-phase-with-mark shape while the no-terminal-phase deletion this row describes stays excluded. |
| Ordering and budget | Claim first (`eviction-handled-at`, optimistic lock), then rerun, consuming exactly one slot. Claiming before the GitHub call makes recovery at-most-once per evicted pod across reconciles, restarts, and replicas — the safe direction, since a duplicate rerun silently spends budget while a missed one is visible in the metric. |
| Observability | `tier` label on both eviction counters; a dedicated `eviction_recovery_identity_unknown_total` plus `EvictionRecoveryIdentityUnknown` Event for the one mode that makes the mechanism inert. |
| Which `TaskResult` | Not applicable to Phase 2 — that question belongs to the Phase 3 fast release, which is what posts a terminal result. Phase 2 posts nothing to the run service. |

Coverage: unit tests for the wire decode, the pod stamping, the detection predicate, the set-once claim (including a deterministic stale-writer race), and the identity-unknown path; an envtest that drives a worker pod provisioned by the real path to `Failed`/`Evicted` against a real apiserver and asserts the rerun fires once for the run the pod recorded.

### Phase 3: fast release, if Phase 1 justifies it

Probe order, per #811, cheapest first:

1. **Deregistration.** `scaleset.Client.DeregisterRunnerByName` already exists, and the manual workaround ARC users apply is deleting the stuck runner ([ARC#4155](https://github.com/actions/actions-runner-controller/issues/4155), [ARC#4307](https://github.com/actions/actions-runner-controller/issues/4307)).
   The open question is only how fast GitHub reacts to an explicit deregistration versus a runner going silent.
2. **A reachable completion endpoint.** `RawServiceCall` is the escape hatch, but there is no obvious job-scoped credential on this tier: the listener holds a queue-scoped `messageQueueAccessToken` and an admin JWT, and the per-job credentials generated by `generatejitconfig` go into the worker Secret and are not retained.
   Lower confidence.
3. **Neither works.** Record that scale-set eviction latency is bounded by GitHub, and close #811.

`cmd/probe` already has the scale-set harness and a cleanup mode, so both probes are cheap to run.

## Design constraints for the implementer

- **Do not double-report.** Fire only when the pod died without the runner reporting.
  Q385's SIGTERM relay already covers every case where something in the pod gets to run.
  The 404/410 no-op covers the race, but make the intent explicit rather than relying on it.
- **Cover deletion, not only terminal phase.** Classic's branch fires on `PodFailed`/`Evicted`, which is the kubelet's node-pressure signal.
  API-initiated eviction (`kubectl drain`) deletes the pod instead, and `waitForPodCompletion` reads a vanished pod as `PodSucceeded`, so the drain path appears to reach no recovery at all.
  **Measured 2026-07-27 (Q421) and confirmed on both tiers** — a drained worker pod publishes no terminal phase before it disappears, so no rerun fires ([result](eviction-oversubscription-validation.md#result-measured-2026-07-27)).
  The pod watch this phase adds should not inherit the gap; whether to close it at all is [Q459](archive/q459-drained-worker-recovery.md).
- **Ordering and budget.** Report the terminal result, then rerun, consuming exactly one slot from the per-run budget.
- **Observability.** A distinct metric or Event so a fast release is distinguishable from a lapse in the field.
  Without it there is no way to tell whether the mechanism works.
- **Which `TaskResult`.** `failed` versus `canceled`, chosen by which one keeps `rerun-failed-jobs` working afterwards.
  Probe both; the rerun is the point.

## Acceptance criteria

Phase 2 is the shippable unit:

- ✅ An evicted scale-set worker triggers `rerun-failed-jobs` for the correct run.
- ✅ The per-run retry budget is shared with classic and holds under concurrent evictions of the same run (the Q106 invariant, envtest-covered as classic is).
- ⚠️ Operator docs describe eviction recovery on both tiers — done — but with the latency still *inferred* rather than measured, because Phase 1 has not run.
  The docs say so where they discuss latency; Q396 closes it.

Phase 3 additionally requires a measured before/after latency on dogfood, and a metric that distinguishes the two paths.

## Release positioning

Not a 1.3 gate.
[release-1.3.md](release-1.3.md) is scoped to the right-sizing headline, the `v2.0.0` deprecation notice, and release mechanics, and every gating row is S-sized with a known outcome.
This work is probe-first with an unknown outcome, and its live-GitHub probes would contend for the same dogfood session the release's headline validation needed.
That validation is now done (Q359 closed 2026-07-25), so the contention argument no longer applies; the probe-first/unknown-outcome argument still does.

Per [maintaining-backlog.md](../development/maintaining-backlog.md#dont-pre-assign-release-versions-to-backlog-items), no release label is assigned until a release is concretely scoped in a plan doc.
Revisit when 1.4 is scoped.
