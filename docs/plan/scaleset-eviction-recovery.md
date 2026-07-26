# Scale-Set Eviction Recovery

Port eviction recovery to the scale-set acquisition tier, then decide whether the
AGC can release an evicted job's lock early instead of waiting out GitHub's
timeout.

Filed 2026-07-25 from a feasibility review of
[issue #811](https://github.com/actions-gateway/github-actions-gateway/issues/811),
which proposed the fast-release optimization. The review found the optimization
sits on top of a prerequisite that does not exist yet, so this plan covers both
and sequences them.

## Why this matters

`v2beta1` is ScaleSet-only, so the scale-set tier is the product's only future
acquisition path. Classic is on the removal track for `v2.0.0`
([release-1.3.md](release-1.3.md)). Eviction recovery works on classic and does
not exist on scale-set, which means the capability is scheduled to disappear with
the protocol it was built for.

Worker right-sizing ([runner-sizing-profiles.md](runner-sizing-profiles.md))
compounds this. Tighter memory limits raise OOM-kill probability, which is one of
the two ways a worker pod gets evicted. The 1.3 headline feature increases
exposure to a gap that only the deprecated tier covers.

## What exists today

### Classic: full recovery, no fast release

`provision()` waits for pod terminal state and branches on eviction at
[provisioner.go:528](../../cmd/agc/internal/provisioner/provisioner.go):

```go
if phase == corev1.PodFailed && reason == "Evicted" {
    p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay)
}
```

[`handleEviction`](../../cmd/agc/internal/provisioner/eviction.go) reserves a slot
from the sharded per-run retry budget (Q106), waits `evictionRetryDelay`, and
calls `rerun-failed-jobs`. Renewal has already stopped, so GitHub concludes the
job when the lock lapses.

### Scale-set: no recovery at all

[`ProvisionScaleSetWorker`](../../cmd/agc/internal/provisioner/provisioner.go) is
fire-and-forget by design: the runner pulls and completes its own job through its
own session, so the AGC neither hands off a payload nor blocks on completion. It
never calls `waitForCompletion`, so it never observes `PodFailed`/`Evicted`,
never reserves a retry slot, and never calls the rerun API.

`handleEviction` is reached from exactly one call site, the classic one above.

An evicted scale-set worker today therefore gets no fast release **and no
rerun**. The job fails and a human reruns it. Q264's transition plan listed
eviction retry under "Reworked, carried over"
([archive/q264-scale-set-protocol-phases.md](archive/q264-scale-set-protocol-phases.md)
§3); that port was designed, not implemented.

### The rerun target is unidentified on scale-set

`rerun-failed-jobs` needs `owner`, `repo`, and `run_id`. Classic extracts all
three from the acquire payload via `jobMetaFrom`. Scale-set has no payload, and
the provisioner passes an empty `jobMeta{}` when building the worker pod.

The modeled [`JobMessage`](../../scaleset/types.go) carries `messageType`,
`runnerRequestId`, `acquireJobUrl`, `jobId`, `runnerId`, `runnerName`, and
`result`. No run ID, no repository. There is no recorded observation anywhere in
the repo of GitHub sending either field on a scale-set queue message.

This is a hard prerequisite: without run identity there is nothing to rerun, no
matter how fast the lock is released.

## The baseline number is confounded

Issue #811 opens from the U5 probe's ~9.5 minutes and asserts the delay is
"purely the job lock's TTL lapsing." The probe does not support that reading.
From §2b-5 of the archived Q264 phases doc, the runner was SIGKILLed at 16:19:06Z
and GitHub concluded `failure` at 16:28:40Z, and the doc itself notes this
coincided with the job's 10-minute `timeout-minutes` boundary.

Three mechanisms are indistinguishable in that measurement: the lock TTL,
GitHub's own liveness detection, and the workflow timeout. Optimizing against an
unattributed number risks building a mechanism that removes none of it.

[Q396](../STATUS.md#Q396) already tracks a clean dogfood benchmark for classic.
Extending it to scale-set, with no `timeout-minutes` confound, is the cheapest
gate on the whole effort.

## Why classic-only is not worth shipping

The fast-release mechanism `#811` proposes does exist on classic:
`broker.Client.CompleteJob` posts a terminal `TaskResult` with the job-scoped
token the AGC already holds for renewal, and its 404/410 handling is already a
documented benign no-op.

Wiring it is real work rather than a one-liner. `handleEviction` receives only
`owner`, `repo`, `runID`; the `planID`, `runServiceURL`, and job token live in
the listener (`cmd/agc/internal/listener/job.go`). `SiblingDelivery` already
carries the right shape, so the plumbing is tractable, but it crosses a layer
boundary and moves a job-scoped credential into the provisioner.

Against a tier scheduled for removal at `v2.0.0`, that cost buys a capability
that ships and then is deleted. Do not implement classic-only.

## Plan

Three gates, cheapest first. Each produces a recorded answer whether or not the
next phase proceeds.

### Phase 1: measure the real baseline (gate)

Tracked by [Q396](../STATUS.md#Q396), whose scope now covers both tiers. Evict a
worker mid-job with no `timeout-minutes` set and timestamp eviction, GitHub's
conclusion, and the annotation text.

**Decides:** whether there is meaningful latency to remove, and which mechanism
owns it. If GitHub's own detection dominates, the fast-release idea is dead and
only Phase 2 proceeds.

### Phase 2: port eviction recovery to scale-set

Independently valuable, and correct to do regardless of Phase 1's outcome, since
it closes a functional regression rather than an optimization gap.

1. **Establish run identity.** Dump a raw `JobAssigned` message body and check
   what GitHub sends beyond the modeled fields. If run ID and repository are
   absent, fall back to correlating `jobId` with the workflow-run API, or
   stamping identity onto the worker pod at provision time from whatever the
   listener does know. This step gates the rest of the phase.
2. **Detect the eviction.** Choose between a pod watch (mirrors classic, keeps
   the existing terminal-phase logic) and keying off the queue's terminal
   `JobCompleted` (the signal Q264 §2b-6 identified, but which arrives only after
   the very delay this plan is trying to remove). The pod watch is likely correct
   precisely because it is the early signal.
3. **Reuse the retry budget.** `reserveEvictionRetry` and the sweeper are
   protocol-agnostic. Keep the Q106 sharded-reservation invariant authoritative:
   at most `maxEvictionRetries` reruns per run, across both tiers.

### Phase 3: fast release, if Phase 1 justifies it

Probe order, per #811, cheapest first:

1. **Deregistration.** `scaleset.Client.DeregisterRunnerByName` already exists,
   and the manual workaround ARC users apply is deleting the stuck runner
   ([ARC#4155](https://github.com/actions/actions-runner-controller/issues/4155),
   [ARC#4307](https://github.com/actions/actions-runner-controller/issues/4307)).
   The open question is only how fast GitHub reacts to an explicit deregistration
   versus a runner going silent.
2. **A reachable completion endpoint.** `RawServiceCall` is the escape hatch, but
   there is no obvious job-scoped credential on this tier: the listener holds a
   queue-scoped `messageQueueAccessToken` and an admin JWT, and the per-job
   credentials generated by `generatejitconfig` go into the worker Secret and are
   not retained. Lower confidence.
3. **Neither works.** Record that scale-set eviction latency is bounded by
   GitHub, and close #811.

`cmd/probe` already has the scale-set harness and a cleanup mode, so both probes
are cheap to run.

## Design constraints for the implementer

- **Do not double-report.** Fire only when the pod died without the runner
  reporting. Q385's SIGTERM relay already covers every case where something in
  the pod gets to run. The 404/410 no-op covers the race, but make the intent
  explicit rather than relying on it.
- **Ordering and budget.** Report the terminal result, then rerun, consuming
  exactly one slot from the per-run budget.
- **Observability.** A distinct metric or Event so a fast release is
  distinguishable from a lapse in the field. Without it there is no way to tell
  whether the mechanism works.
- **Which `TaskResult`.** `failed` versus `canceled`, chosen by which one keeps
  `rerun-failed-jobs` working afterwards. Probe both; the rerun is the point.

## Acceptance criteria

Phase 2 is the shippable unit:

- An evicted scale-set worker triggers `rerun-failed-jobs` for the correct run.
- The per-run retry budget is shared with classic and holds under concurrent
  evictions of the same run (the Q106 invariant, envtest-covered as classic is).
- Operator docs describe eviction recovery on both tiers, with the measured
  latency from Phase 1 rather than an inferred one.

Phase 3 additionally requires a measured before/after latency on dogfood, and a
metric that distinguishes the two paths.

## Release positioning

Not a 1.3 gate. [release-1.3.md](release-1.3.md) is scoped to the right-sizing
headline, the `v2.0.0` deprecation notice, and release mechanics, and every
gating row is S-sized with a known outcome. This work is probe-first with an
unknown outcome, and its live-GitHub probes would contend for the same dogfood
session the release's headline validation needed. That validation is now done
(Q359 closed 2026-07-25), so the contention argument no longer applies; the
probe-first/unknown-outcome argument still does.

Per
[maintaining-backlog.md](../development/maintaining-backlog.md#dont-pre-assign-release-versions-to-backlog-items),
no release label is assigned until a release is concretely scoped in a plan doc.
Revisit when 1.4 is scoped.
