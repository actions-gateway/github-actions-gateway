# Q438: a durable reap deadline for a worker orphaned while the AGC is down

**Status:** decided and implemented 2026-07-27.
The job's declared timeout does **not** reach the AGC on either tier, so deriving a deadline from it is not available.
Shipped instead: a **12-hour default worker lifetime**, stamped at provision time as the pod's `activeDeadlineSeconds` and enforced by the kubelet, overridable per RunnerGroup / RunnerSet.

This closes the residual [Q435](q435-restart-orphan-reclaim.md#residual-and-why-it-is-not-fixed-here) left open: a `Running` worker whose job ended while the AGC was down carries no deadline of any kind, and only a redelivered `JobCompleted` reclaims it.

## The problem, restated

Q435 measured four orphan classes.
Three recover from durable pod state.
The fourth — `Running`, no `actions-gateway.com/job-completed-at` stamp, AGC down when the job ended — is **identical in cluster state** to a worker running a legitimately long job.
No AGC-side reconciliation can tell them apart, which is why the deadline has to be stamped at provision time rather than inferred later.

The motivating dogfood incident ran 16 hours and stranded 82 spot node-hours.

## Research

### 1. Does the acquired-job message carry the job's timeout? No — on either tier.

This was the pivotal question: a derived deadline is principled, an invented one is not.
The answer is no, and it is no on both tiers for different reasons.

**Scale-set tier.** The message that says "provision a worker" is `JobAssigned`, whose body is a `JobMessageBase`.
Upstream [`actions/scaleset/types.go`](https://github.com/actions/scaleset/blob/main/types.go) defines that base as exactly:

```go
type JobMessageBase struct {
	JobMessageType
	RunnerRequestID    int64
	RepositoryName     string
	OwnerName          string
	JobID              string
	JobWorkflowRef     string
	JobDisplayName     string
	WorkflowRunID      int64
	EventName          string
	RequestLabels      []string
	QueueTime          time.Time
	ScaleSetAssignTime time.Time
	RunnerAssignTime   time.Time
	FinishTime         time.Time
}
```

No timeout, no deadline, no expiry.
The only durations are timestamps of things that already happened (`QueueTime`, `ScaleSetAssignTime`, `RunnerAssignTime`, `FinishTime`).

Per `CLAUDE.md`, source inspection is a hypothesis until confirmed against the real thing, so this is checked against GAG's own **captured live wire evidence** rather than left at the upstream struct.
The [scale-set eviction-recovery plan](../scaleset-eviction-recovery.md) records a full raw `JobAssigned` body dumped from the dotcom broker-host backend on 2026-07-26:

```json
{"messageType":"JobAssigned","repositoryName":"github-actions-gateway",
 "ownerName":"actions-gateway","workflowRunId":30225287326,
 "jobDisplayName":"probe-fast","eventName":"workflow_dispatch",
 "jobWorkflowRef":"actions-gateway/github-actions-gateway/.github/workflows/scaleset-probe.yml@refs/heads/main",
 "requestLabels":["gag-probe-scaleset"],"scaleSetAssignTime":"2026-07-26T23:29:36Z",
 "jobId":"bd59f43d-2e1a-567a-b5a4-c147b94d0766","runnerRequestId":0}
```

That is the whole body, not a projection through GAG's narrower model — the probe logs the raw wire through the client's `ResponseObserver` precisely so the typed API cannot hide a field.
There is no timeout on it.
The observed shape matches `JobMessageBase` field-for-field, which is also how Q417 established that the run identity was on the wire all along.

**Classic tier.** Two independent reasons.
First, the AGC's `AcquireJobResponse` ([`broker/types.go`](../../../broker/types.go)) models `plan.planId` and `resources.endpoints` only; the rest of the body is forwarded opaquely to the worker and never parsed for scheduling.
Second and more decisive, on the scale-set tier — the tier dogfood runs and the tier the entire Q435 residual lives on — the AGC issues no acquire call at all: it mints a JIT config and the worker's own runner acquires.
There is no acquire response for the AGC to read.

**Consequence: option 3 (derive from the declared timeout) is not viable.** It was the preferred shape going in.
The evidence removes it.

### 2. GitHub Actions job-timeout semantics

- `jobs.<job_id>.timeout-minutes` — *"The maximum number of minutes to let a job run before GitHub automatically cancels it.
  Default: 360"* (6 hours), per the workflow-syntax reference as quoted in [github/docs#7984](https://github.com/github/docs/issues/7984).
- **GitHub-hosted:** 360 minutes is a hard ceiling — *"Each job in a workflow can run for up to 6 hours of execution time.
  If a job reaches this limit, the job is terminated and fails."* Users can only lower it.
- **Self-hosted:** the 360-minute default still applies but is *not* a ceiling — a workflow may raise `timeout-minutes` beyond it.
  GitHub instead enforces *"Each job in a workflow can run for up to 5 days of execution time"* ([Actions limits](https://docs.github.com/en/actions/reference/limits); introduced by the [2024-04-04 changelog](https://github.blog/changelog/2024-04-04-actions-jobs-executing-on-self-hosted-runners-will-now-timeout-in-5-days/)).

Two numbers matter for choosing a default.
**6 hours** is the timeout every job gets unless its author explicitly opted out of it — so a job running longer than 6 hours is always a deliberate act.
**5 days** is the absolute ceiling above which no self-hosted job can run, so any cap we pick below it strictly tightens a bound GitHub already imposes.

### 3. Message retention / redelivery: undocumented, and this is the honest answer

Q435 flagged this as unmeasured.
It remains **unmeasured and undocumented**, and no amount of reading settles it.

What *is* documented is only the within-session acknowledgement contract: a client must call `DeleteMessage` to acknowledge, and unacknowledged messages are redelivered on the next poll.
That describes a client crashing mid-batch with the session still alive.
It says nothing about how long GitHub holds a terminal `JobCompleted` when **no session exists at all** for hours — which is the Q435 replay path.
There is no published retention period for that case.

Stating this plainly rather than inferring a number: **we do not know whether the replay path survives a multi-hour outage.** GAG's `scalesettest` fake keeps a session-independent queue log, which is the *permissive* assumption; real retention could be far shorter.
If it is, the replay path is not a recovery path after a long outage and this item's deadline is the *only* one — which is an argument for the default being **on**, not opt-in.

Filed as **Q468** for a live measurement; it needs cluster-only / live credentials and does not belong in this PR.
**Measured 2026-07-29** ([q468-jobcompleted-retention.md](q468-jobcompleted-retention.md)): retention is *not* far shorter — a `JobCompleted` was still redelivered to a new session after 13 h 3 m with no session in between.
That is past this item's 12 h default, so the two mechanisms overlap: the cap fires while redelivery is still live, rather than either leaving a window uncovered.

### 4. Prior art

**ARC does not solve this.** Actions Runner Controller exposes no maximum-runner- lifetime or deadline knob, and orphaned/stuck runner pods are a long-standing open class of issues against it — [#1833](https://github.com/actions/actions-runner-controller/issues/1833) (runner not cleaned up after completion), [#3821](https://github.com/actions/actions-runner-controller/issues/3821) (AutoscalingRunnerSet stuck believing runners exist), [#4136](https://github.com/actions/actions-runner-controller/issues/4136), [#4203](https://github.com/actions/actions-runner-controller/issues/4203).
The documented workaround is periodic manual deletion — the same hand-delete step GAG's runbook currently names.
So ARC is a confirmation that the gap is real and not a design to copy.

**The idiomatic Kubernetes mechanism is `activeDeadlineSeconds`.** Its contract, from the vendored API source ([`vendor/k8s.io/api/core/v1/types.go`](../../../vendor/k8s.io/api/core/v1/types.go)):

> Optional duration in seconds the pod may be active on the node relative to StartTime before the system will actively try to mark it failed and kill associated containers.

Three properties decide it:

1. **The kubelet enforces it, not the AGC.** This is the decisive one.
   In the dogfood incident the AGC was `Pending` for the entire 16 hours — so a *reaper-side* deadline would not have bounded that incident either, because there was no reaper running.
   `activeDeadlineSeconds` needs only the node.
2. **Measured from `StartTime`**, i.e. active-on-node time, so it does not double-count scheduling latency.
   Stuck-`Pending` is already covered separately by `pendingPodDeadline`.
3. **It composes with the existing reaper.** The pod lands in `Failed` with `Status.Reason == "DeadlineExceeded"`, which the reaper's existing terminal arm already reclaims via `completedPodTTL`.

Failure modes, stated honestly: it is not enforced if the node or kubelet is gone (such a pod becomes `Unknown` and the terminal arm reaps it anyway); and it cannot distinguish a wedged worker from a slow legitimate job any better than we can — it is a blunt cap, which is exactly why the value and the override matter.

## Decision

**Option 2 — a generous fixed default, on by default, enforced as `activeDeadlineSeconds`.** Option 3 is unavailable (§1); option 1 (opt-in, no default) fixes nothing by default and is close to what already exists, since a determined operator can already set `podTemplate.spec.activeDeadlineSeconds` by hand — the missing piece was never the field, it was the default.

**Default: 12 hours.**

The reasoning, and the tension the Queue row names:

- It is **2× GitHub's own 360-minute default job timeout**.
  A job this kills has explicitly declared a `timeout-minutes` more than twice the default it would otherwise have received.
  That is a principled anchor rather than an invented round number, which is the best available substitute for deriving from the real timeout.
- It **bounds the pathological case**: the 16-hour incident becomes a 12-hour one.
  A 24-hour default would not have bounded that incident at all, which is what rules out the top of the range.
- It **strictly tightens an existing bound**: GitHub already terminates a self-hosted job at 5 days, so this is not a new class of failure, only a nearer one.
- Going lower was considered — 8 hours (6 h + headroom) bounds the incident better.
  It was rejected because the population of jobs between 8 and 12 hours is essentially the same population (both are far past the 6-hour default), so the extra aggression buys little while making a legitimate long job likelier to die on a default nobody chose.
  When the safe default and the useful default point in opposite directions, the tie goes to not breaking a tenant's job.

**A legitimately long job under this default:** a job that runs past 12 hours is killed.
Its pod goes `Failed`/`DeadlineExceeded`, the job fails at GitHub, and the operator raises `maxWorkerLifetime` on that RunnerGroup / RunnerSet — or sets it to `0s` to opt out entirely.
The failure is legible enough to point straight at the cause (§ Legibility), which is the difference between a one-minute diagnosis and the mystery termination this item exists to avoid.

### Precedence

1. An explicit `podTemplate.spec.activeDeadlineSeconds` **wins** — if an operator set it by hand, GAG does not overwrite it.
   Same idiom as `buildPod`'s worker-image gap-fill.
2. Otherwise `maxWorkerLifetime` on the RunnerGroup (v1) / RunnerSet (v2).
3. Otherwise the 12-hour default.
4. `maxWorkerLifetime: 0s` disables the cap — the field is left unset and no deadline is stamped.

### Legibility

An operator debugging a killed long job must see the cause immediately, not a mystery termination.
Three signals, in increasing order of how hard you have to look:

- The pod itself carries `Failed` / `DeadlineExceeded` for its whole `completedPodTTL` window — visible in plain `kubectl get`/`describe`.
- The reaper classifies it as its own reap reason, **`lifetime_exceeded`**, rather than folding it into the generic `completed_ttl`, so `actions_gateway_worker_pods_reaped_total{reason="lifetime_exceeded"}` is a distinct, alertable series.
- A Warning Event **`WorkerPodLifetimeExceeded`** is recorded on the owning RunnerGroup / RunnerSet naming the pod and the lifetime that killed it.

`Status.Reason == "DeadlineExceeded"` is also disjoint from the eviction path, which matches strictly on `Reason == "Evicted"` ([`eviction_scaleset.go`](../../../cmd/agc/internal/provisioner/eviction_scaleset.go)), so a deadline kill cannot be mistaken for a node-pressure eviction and cannot trigger a spurious `rerun-failed-jobs`.

## What shipped

| Area | Change |
|---|---|
| API (v1) | `RunnerGroup.spec.maxWorkerLifetime` (`*metav1.Duration`, optional) |
| API (v2) | `RunnerSet.spec.maxWorkerLifetime` (`*metav1.Duration`, optional) |
| Provisioner | `ResolvedSpec.MaxWorkerLifetime`; `MaxWorkerLifetimeOrDefault`; `DefaultMaxWorkerLifetime = 12h`; `buildPod` stamps `activeDeadlineSeconds` unless the pod template set it |
| Reaper | `reapReasonLifetimeExceeded` for a terminal pod whose `Status.Reason` is `DeadlineExceeded`; `WorkerPodLifetimeExceeded` Warning Event |
| Docs | operator reference for both APIs, troubleshooting entry superseding the hand-delete step, design update to the Q435 residual |

## Tests

- Unit (`cmd/agc/internal/provisioner`): defaulting and precedence — default applied, per-owner override applied, `0s` disables, explicit `podTemplate.spec.activeDeadlineSeconds` preserved.
- Unit (`cmd/agc/internal/controller`): a `Failed`/`DeadlineExceeded` pod reaps under `lifetime_exceeded`, an ordinary `Failed` pod still reaps under `completed_ttl`, and an `Evicted` pod is untouched by the new arm.
- Integration (envtest, `cmd/agc/internal/controller/integration/`): a worker pod driven to `Failed`/`DeadlineExceeded` against a real apiserver is reaped with the distinct reason and Event — the Q435 residual class, now bounded.

## Open

Nothing.
The one follow-up is closed:

- **Q468** — measure real `JobCompleted` retention across a multi-hour gap with no session (§3).
  Live-only.
  Answered 2026-07-29 ([q468-jobcompleted-retention.md](q468-jobcompleted-retention.md)): RETAINED at 13 h 3 m, so the replay path can be relied on across any gap in which a worker is still alive to reclaim.
  As predicted, it did not change this decision — the deadline was required either way.
