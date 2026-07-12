# Q170 — Kubernetes Events for job lifecycle

## Goal
Emit Kubernetes Events on the four key AGC job-lifecycle transitions that today surface
only via metrics/conditions, so operators get event-based incident visibility
(`kubectl describe`, event watchers). Mirror the reaper's existing EventRecorder usage.

## Transitions covered (confirmed against the code)
| Transition | Site | Object | Type | Reason | Action |
|---|---|---|---|---|---|
| Acquisition failure | `listener/goroutine.go` AcquireJob err | owner | Warning | `JobAcquisitionFailed` | `AcquireJob` |
| Session failure (version) | `listener/goroutine.go` createSession `VersionTooOld` | owner | Warning | `RunnerVersionTooOld` | `CreateSession` |
| Session failure (auth) | `listener/goroutine.go` createSession `Unauthorized` | owner | Warning | `SessionUnauthorized` | `CreateSession` |
| Eviction-retry exhaustion | `provisioner/provisioner.go` handleEviction budget exhausted | owner | Warning | `EvictionRetriesExhausted` | `RetryEvictedJob` |
| Quota rejection (exhausted) | `provisioner/provisioner.go` createPodWithQuotaRetry budget exhausted | owner | Warning | `QuotaRetriesExhausted` | `ProvisionWorker` |

Reasons mirror the existing Prometheus metric names so operators can correlate events with metrics.
All emit on the **owner** (RunnerGroup/RunnerSet) — same object the reaper uses; the message names the pod/run.

## Mechanism (mirror the existing `conditionCh` back-channel)
The transition sites run in listener/provisioner goroutines, which do not hold the live
owner object that the EventRecorder needs. Reuse the established condition-channel pattern:

- `listener.EventRecorder` interface (next to `ConditionUpdater`): `Event(namespace, name, eventtype, reason, action, note string)`. Non-blocking.
- `listener.Config.Events` carries it for the listener path; `recordEvent(cfg, ...)` helper mirrors `setCondition`.
- Provisioner path routes per-job via the `Target` seam: new `Target.RecordEvent(...)` method.
  - `runnerGroupTarget` (v1) routes via new `Provisioner.Events` field.
  - `runnerSetTarget` (v2) routes via a new `events` field captured from the reconciler.
- Controllers (`controller` package, shared): `eventRecord` struct + `channelEventRecorder` (in `runner_shared.go`); each reconciler gains an `eventCh` (256-buf, like `conditionCh`) and a `drainEvents` that records matching events on the live object via the existing `recordEvent(obj, ...)` and re-enqueues others. Drained each Reconcile right after `drainConditions`.

The provisioner is shared across v1/v2, so per-job Target routing (not a single shared
sink) is what sends each event to the correct owner's channel.

## No-spam
Events are pushed once per transition (failed acquire, non-retriable session failure, budget
exhaustion) and consumed once by the drain — never per reconcile. The k8s event recorder
additionally aggregates identical (reason+message+object) events with a count. Events are
recorded on the owner's next reconcile (pod-watch / baseline recheck drives it), best-effort.

## Tests
- listener: fake EventRecorder asserts acquisition-failure + session-failure events.
- provisioner: fake recorder via `Provisioner.Events` asserts quota-exhausted + eviction-exhausted events.
- `make check` green.

## Docs
- `docs/operations/troubleshooting.md`: new Events, their Reasons, meaning, how to react.
- `docs/development/kubernetes-conventions.md`: note the new operator-visible Event reasons.
- `docs/STATUS.md`: remove Q170 row (isolated commit).
