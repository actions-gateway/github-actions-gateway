# Q575 — a worker pod whose `job-payload` secret is absent stalls until something else clears it

## Symptom (rc.3 dogfood gate, 2026-08-01)

8 worker pods sat `Pending` for ~10 minutes mounting a `job-ss-<jobID>` Secret that did not exist, then recovered unaided.

## Diagnosis

The Queue row offered two directions — "find what deletes it, or what creates the pod first".
Both were traced; the answer is the first, plus a third thing the row did not name: nothing acts on the result.

### The Secret is always created before the pod

`ProvisionScaleSetWorker` ([provisioner.go:698](../../../cmd/agc/internal/provisioner/provisioner.go)) stages the Secret as step 1 and creates the pod as step 5.
Every failure exit between them calls `unstage()`, which deletes the Secret only when *this* call created it and only on paths that leave no pod behind.
So a worker pod never starts life without its Secret, and "the provisioner creates the pod first" is refuted.

### The only deleter that can reach a live pod is `CleanupScaleSetJob`

`deleteSecret` has six call sites in the classic `provision()` path (all of which own the job end to end) and two on the scale-set path: `unstage()` (pre-pod, above) and `CleanupScaleSetJob`, which the listener calls from `completeJob` on every terminal `JobCompleted`.
That is the deleter.

`CleanupScaleSetJob` deletes the Secret without regard to the state of that job's worker pod.
Of the four states the pod can be in, three are safe and one is not:

| Pod state | Effect of deleting the Secret |
|---|---|
| Absent | Nothing to strand. |
| Terminal | The runner already consumed the JIT config; `completedPodTTL` owns the pod. |
| `Running` | Already mounted; the kubelet does not tear down a running pod when its Secret disappears. `markJobCompleted` stamps a reap deadline (Q420). |
| **`Pending`** | **Not yet mounted. The pod becomes permanently unstartable.** |

### Nothing acts on the `Pending` case — which is what makes it a ~10-minute stall

`markJobCompleted` *does* stamp `AnnotationJobCompletedAt` on a `Pending` pod: its early-return switch covers only `Succeeded`/`Failed`/`Unknown`.
But the reaper ([runner_shared.go:284](../../../cmd/agc/internal/controller/runner_shared.go)) consults that stamp **only in the `PodRunning` arm**.
The `PodPending` arm computes `due = pod.CreationTimestamp.Add(deadline)` and ignores the stamp entirely.

So the completion signal is written and then thrown away, and the pod waits out `spec.pendingPodDeadline` — `DefaultPendingPodDeadline = 10 * time.Minute`.
That constant is the "~10 minutes, then recovered unaided" in the capture: the pending-deadline reaper collected them.
The clue is fully explained.

It is also misreported.
The reap is labelled `pending_deadline` and emits a `WorkerPodStuckPending` Warning telling the operator to "check the pod template image and scheduling constraints" — blaming the scheduler for a pod the scheduler placed fine and the AGC then removed the Secret from.

### How the pod and the completion end up in that order

`handleMessage` ([listener.go:958](../../../cmd/agc/internal/scalesetlistener/listener.go)) processes a batch's `JobAssigned` messages **first**, then its `JobCompleted` messages.
And `provisionAssigned` guards on `provisioned` and `abandoned` — but **not** on `completed`.
So a batch carrying both messages for the same job provisions a worker and then deletes its Secret microseconds later.
That is not a race; it is deterministic.

Three orderings reach the stall, in rising order of likelihood for the capture:

1. **Same batch.** A run cancelled or fast-failing between two polls delivers `JobAssigned(X)` and `JobCompleted(X)` together.
   Guaranteed stall.
2. **Adjacent batches.** The completion lands while the pod is still Pending on ordinary scheduling latency (image pull, node scale-up).
3. **Restart replay.** A re-created session polls from cursor 0 with empty `provisioned`/`completed`/`abandoned` sets and replays the whole queue, provisioning workers for long-gone jobs and then completing them.
   **8 pods at once fits a replayed queue far better than 8 independent cancellations**, so this is the most likely trigger of the actual capture.
   The replay itself is Q583 and is explicitly out of scope here — but the stall it produces is this defect, and fixing this defect bounds its damage.

## Fix

Two changes, addressing different orderings.
Neither stops the replay (Q583).

1. **Listener (`handleMessage`): handle a batch's completions before its assignments, and skip provisioning a job already known completed.** Kills ordering 1 outright — no pod is created for a job whose completion is already in hand — and shrinks ordering 3 to its cross-batch remainder.
   This is the same reasoning as Q553's `abandoned` guard, applied to the completed case.

2. **Reaper (`reapWorkerPodsByLabel`): honour `AnnotationJobCompletedAt` in the `PodPending` arm**, as the `PodRunning` arm already does.
   A stamped Pending pod is reaped on a short completion-derived grace under a new `completed_pending` reason and its own Event, instead of masquerading as a scheduling stall for ten minutes.
   This is what ordering 2 needs, which fix 1 structurally cannot reach.

Fix 2 deliberately lands in the shared reaper rather than in `CleanupScaleSetJob`, so it inherits the machinery that already exists there: the Q502 stamp-then-delete (so eviction recovery does not read the reap as a disruption and re-run the job), the Q550 `deregisterRunner` hook, the UID precondition, and the reap metric.

## Status

- [x] Diagnosis traced end to end from source; the 10-minute constant matches the capture.
- [x] Fix 1 — listener ordering + `completed` guard.
- [x] Fix 2 — reaper honours the completion stamp on Pending pods.
- [x] Tests: listener unit (same-batch), reaper unit (grace arithmetic), envtest (`v2_completed_pending_test.go`) against a real apiserver.
- [x] Causation settled by deleting the mechanism (both fixes, independently).
- [x] Docs: reap reasons, Events, metric label, troubleshooting.

## Not verified live

The dogfood cluster is out of scope for this change.
Unit, race and envtest coverage is the bar here; live confirmation belongs to the rc.4 dogfood gate.

## Effect on Q577

Q577 (`stop.sh` leaves the pool up when its drain cannot converge) is blocked on this row alone.
This fix removes the ten-minute floor a stalled worker put under a drain, but Q577 was never re-measured with Q553's wedge closed, so whether it is still real is a question for rc.4 — see the PR body.
