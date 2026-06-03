# Q64 — Replace provisioner per-session poll with a shared Pod informer waiter

Tracked as [Q64](../STATUS.md); split from the k8s-best-practices audit
[§A4](k8s-best-practices.md#a-controller-correctness-).

## Goal

Make worker-pod completion detection in the AGC provisioner event-driven instead
of poll-driven, so detection latency drops from ~5 s to near-zero and the
provisioner stops spawning one polling ticker per in-flight session.

## Background — what the poll actually costs

`provisioner.go`'s `waitForPodCompletion` opens a `time.NewTicker(5s)` per
`provision()` call and, on every tick, issues `Client.Get` for the pod until it
reaches a terminal phase. At the project's target of thousands of concurrent
virtual sessions, that is thousands of goroutines each holding a ticker.

The audit row framed this as "~200 gets/s at 1,000 sessions" of **API-server**
load. That framing is inaccurate: the provisioner's `Client` is
`mgr.GetClient()`, the controller-runtime **cache-backed** client. Pods are not
in the cache `DisableFor` list (only Secrets are — see `main.go` §5), so those
`Get` calls are already served from the in-process shared Pod informer, not the
API server. The real costs the poll imposes are therefore:

1. **Detection latency** — up to one `PollInterval` (5 s) between a pod going
   terminal and the provisioner noticing. On a 10 s job that is a 50 % tail.
2. **Per-session goroutine + ticker churn** — N tickers waking N goroutines to
   re-read the cache, instead of the informer pushing one event when the pod
   actually changes.

A single shared Pod informer already exists (the cache lazily starts one on the
first Pod read). The fix is to **register one event handler on it** and have each
session block on a channel that the handler signals, rather than each session
polling the cache on a timer.

## Approach

Introduce a `PodWaiter` seam in the provisioner:

```go
type PodWaiter interface {
    WaitForCompletion(ctx context.Context, namespace, name string) (corev1.PodPhase, string, error)
}
```

- **`InformerPodWaiter`** (production): registers a single
  `ResourceEventHandler` on the shared Pod informer
  (`cache.Cache.GetInformer(ctx, &corev1.Pod{})`) and keeps a registry of
  per-pod waiter channels. Add/Update events that reach a terminal phase, and
  Delete events, signal the matching waiters. It implements `manager.Runnable`
  so the handler is registered after the cache syncs; `main.go` wires it with
  `mgr.Add(...)` and sets `prov.Waiter`.

- **Legacy poll fallback**: when `prov.Waiter` is nil the provisioner keeps the
  existing `waitForPodCompletion` ticker loop. This path is used only by the
  fake-client unit tests (which have no informer) and as a defensive fallback;
  production always wires the informer waiter, so the hot path no longer polls.

### Race handling

The handler is registered once at startup and persists, so every created pod
produces an Add event to it (client-go replays current cache state to a freshly
added handler and watches thereafter). Per `WaitForCompletion` call:

1. Register the waiter channel under `ns/name` **first**.
2. Read current state from the cache:
   - terminal phase → resolve immediately (covers the event-fired-before-register
     race);
   - found non-terminal → block for an event;
   - `NotFound` → block for an event (do **not** conclude success — the cache may
     simply not have observed our just-issued `Create` yet; the Add event will
     arrive).
3. Block on the channel or `ctx.Done()`.

Terminal-phase detection matches the existing poll exactly
(`Succeeded`/`Failed`/`Unknown`, carrying `Status.Reason` for eviction
detection). A Delete event resolves as `Succeeded` with empty reason — matching
the poll's existing "pod deleted externally → treat as completion" behaviour.

## Files

- `cmd/agc/internal/provisioner/podwaiter.go` (new) — `PodWaiter` interface,
  `InformerPodWaiter`, terminal-phase helper.
- `cmd/agc/internal/provisioner/provisioner.go` — add `Waiter PodWaiter` field;
  `provision` step 5 calls the waiter when set, else the legacy poll.
- `cmd/agc/main.go` — construct `InformerPodWaiter`, `mgr.Add` it, set
  `prov.Waiter`.

## Tests

- `cmd/agc/internal/provisioner/podwaiter_internal_test.go` (white-box,
  `package provisioner`): drive `onUpdate`/`onDelete` directly against a fake
  `client.Reader` for the initial-state read. Cases: terminal-before-wait,
  event-driven Succeeded/Failed-Evicted, delete→Succeeded, ctx cancel,
  NotFound-then-terminal (no premature success), multiple concurrent waiters.
- `cmd/agc/internal/controller/integration/` (envtest, `integration` tag): a
  focused test that runs a manager with an `InformerPodWaiter`, creates a pod,
  transitions it to a terminal phase, and asserts `WaitForCompletion` returns
  promptly with the right phase — proving the real informer wiring.
- Existing fake-client provisioner tests stay green unchanged (nil Waiter →
  poll path).

## Out of scope

- A pod-completion-latency metric (the 5 s → event-driven win is the deliverable;
  a histogram can follow if operators want to track it).
- Q63 (RunnerGroup pod watch for `ActiveSessions` self-heal) — paired but
  separate; this change does not touch the RunnerGroup controller.
