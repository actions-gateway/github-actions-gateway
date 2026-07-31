# Q501 — relaying a run cancellation to the worker pod

**Status:** ⚠️ Partial. The *actuator* shipped (this plan's Phase 1); the *trigger*
is blocked on one live-GitHub measurement (Phase 2) and on whether the defect
exists on the non-deprecated tier at all (Phase 0).

Q501 was found by [Q459's cancellation measurement](q459-drained-worker-recovery.md#the-measurement-also-found-that-a-cancel-never-reaches-the-worker-q501):
a `sleep 600` job ran its full 600s after its run was cancelled from GitHub, which
concluded the job at its own ~5-minute grace. Cancelling a runaway job does not
reclaim its capacity.

## The gap is two gaps

Reading the code for the fix turned up a second, independent break in the same path.
They compose: closing either alone changes nothing observable.

| | What is missing | Where |
|---|---|---|
| **Trigger** | Nothing tells the AGC that the run was cancelled. | classic listener |
| **Actuator** | Even when the AGC *does* decide to give up on a job, it never deletes the worker pod. | `provision`, step 6 |

The actuator gap is the larger surprise, because it is not specific to
cancellation. `handleJob` derives a per-job context and the renew loop cancels it
when the job's lock is definitively lost (Q254) — the stated intent being that "the
worker tears down". It does not: `waitForCompletion` returns `ctx.Err()`,
[`provision`](../../cmd/agc/internal/provisioner/provisioner.go) deletes the job
Secret and returns, and the pod is left running. Nothing else reclaims it either —
the reaper's `orphaned_running` arm reads an annotation only the scale-set tier
stamps — so it burns a worker slot and a node until the kubelet kills it at
`spec.maxWorkerLifetime` (default **12 hours**).

So every Q254 lost-lock teardown has been leaving an orphan, not just the cancel
path. That half is unambiguous and does not need a measurement, which is why it
shipped first.

## Why the trigger is not obvious

The classic tier's worker pod runs bare `Runner.Worker` fed an acquirejob payload.
It has no broker session of its own, and — per Q114 — the AGC's session is *spent at
`AcquireJob`*: GitHub deletes the single-use JIT runner record, and further polling
degrades into empty-200/401 loops. So there is no live GitHub→AGC message channel
for a running classic job to carry a `JobCancellation` on, the way the real runner's
resident `Runner.Listener` receives one.

That leaves three candidate triggers:

| Candidate | Cost | What is unknown |
|---|---|---|
| **A. `renewjob` reports the loss** | free — the loop already runs every 60s | Does the run service stop honouring `renewjob` once a cancelled job is concluded? If it 404s, Q254's existing teardown *already* fires, and Phase 1's actuator alone bounds the waste to GitHub's ~5-minute grace plus one renew interval. |
| **B. REST run-status poll** | an installation-token API call per active run per interval | Nothing — `TokenFunc`, `GitHubAPIURL` and the run identity are all already in the provision goroutine (they are what `rerun-failed-jobs` uses). But at the design's thousands-of-concurrent-jobs scale a 30s poll blows through the installation rate limit, so it needs a long interval and an age threshold. |
| **C. Concurrent session poll** | a second long-poll per running job | Whether the spent session yields anything but empty 200s. Q114 says it does not. |

Candidate A is both the cheapest and the one whose answer decides whether B is
needed at all. The 600s measurement does **not** rule it out: at the time it was
taken the actuator was missing, so a renew-loop teardown at ~5 minutes would have
looked exactly like no teardown at all.

## Phase 0 — does this exist on the tier that ships?

The measurement ran against the v1 `ActionsGateway` fixture, which is the **classic**
tier. The default since Q264 P5 is `acquisitionProtocol: ScaleSet`, where the worker
runs the *full* runner (`run.sh --jitconfig`) and therefore holds its own broker
session — the channel a cancellation travels on. If a ScaleSet worker aborts
natively, Q501 is a defect confined to a tier already scheduled for removal at
`v2.0.0`, and candidate B's cost is not worth paying.

Unmeasured. Extending `E2E_GitHub_CancelledRunLeavesNoDeletionMark` to a v2 ScaleSet
`RunnerSet` answers it in one run.

## Phase 1 — the actuator (shipped)

When the listener abandons a job, the worker pod is reclaimed.

The subtlety is that a process-wide shutdown cancels every job context at once, and
must **not** delete live workers — an AGC rollout would kill every running job.
`context.Cause` separates the two: `handleJob` derives the job context with
`context.WithCancelCause`, the renew loop cancels it with `ErrJobAbandoned` wrapping
the teardown reason, and a plain parent cancellation leaves the cause as
`context.Canceled`. `provision` reclaims only on the former.

The delete is stamped `actions-gateway.com/deletion-reason: job_abandoned` before it
is issued, which is what keeps it from re-entering Q502's graceful-deletion recovery
as a disruption — the exclusion `deletion.go` requires of "any future AGC deletion
path — e.g. a Q501 cancel-relay that deletes the worker". A reclaimed worker is
counted on `actions_gateway_worker_pods_reaped_total{reason="job_abandoned"}`.

The pod delete is graceful, so the Q385 SIGTERM relay reaches `Runner.Worker` and the
runner reports its own outcome rather than being SIGKILLed — the same path a drain
takes, measured at 16s to a GitHub conclusion.

## Phase 2 — the trigger (open)

Measure candidate A on the next live-GitHub run of the cancel spec: capture whether
`renewjob` starts failing after GitHub concludes a cancelled job, and if so with what
shape. Two outcomes:

- **It fails definitively.** Nothing more to build. Q254's teardown plus Phase 1's
  actuator bounds a cancelled job's waste to ~5–6 minutes, and Q501 closes with a
  documented latency rather than a new mechanism.
- **It keeps returning 200.** Candidate B is the remaining option, and its
  rate-limit design (age threshold before a run is polled at all, one poll per run
  rather than per job, a slow interval) becomes the work.

Phase 0 gates whether Phase 2 is worth running: if ScaleSet workers cancel natively,
record that and close Q501 as classic-only.
