# Q501 — relaying a run cancellation to the worker pod

**Status:** ⚠️ Partial.
The *actuator* shipped (Phase 1). **Phase 0 is answered:** the ScaleSet tier is not exposed to the unbounded form of this defect, from a live measurement already committed to the repo — so Q501 is a **classic-tier** item and candidate B's cost is not worth paying.
The remaining question is Phase 2, one live-GitHub reading that the cancel spec is now instrumented to take.

Q501 was found by [Q459's cancellation measurement](archive/q459-drained-worker-recovery.md#the-measurement-also-found-that-a-cancel-never-reaches-the-worker-q501): a `sleep 600` job ran its full 600s after its run was cancelled from GitHub, which concluded the job at its own ~5-minute grace.
Cancelling a runaway job does not reclaim its capacity.

## The gap is two gaps

Reading the code for the fix turned up a second, independent break in the same path.
They compose: closing either alone changes nothing observable.

| | What is missing | Where |
|---|---|---|
| **Trigger** | Nothing tells the AGC that the run was cancelled. | classic listener |
| **Actuator** | Even when the AGC *does* decide to give up on a job, it never deletes the worker pod. | `provision`, step 6 |

The actuator gap is the larger surprise, because it is not specific to cancellation. `handleJob` derives a per-job context and the renew loop cancels it when the job's lock is definitively lost (Q254) — the stated intent being that "the worker tears down".
It does not: `waitForCompletion` returns `ctx.Err()`, [`provision`](../../cmd/agc/internal/provisioner/provisioner.go) deletes the job Secret and returns, and the pod is left running.
Nothing else reclaims it either — the reaper's `orphaned_running` arm reads an annotation only the scale-set tier stamps — so it burns a worker slot and a node until the kubelet kills it at `spec.maxWorkerLifetime` (default **12 hours**).

So every Q254 lost-lock teardown has been leaving an orphan, not just the cancel path.
That half is unambiguous and does not need a measurement, which is why it shipped first.

## Why the trigger is not obvious

The classic tier's worker pod runs bare `Runner.Worker` fed an acquirejob payload.
It has no broker session of its own, and — per Q114 — the AGC's session is *spent at `AcquireJob`*: GitHub deletes the single-use JIT runner record, and further polling degrades into empty-200/401 loops.
So there is no live GitHub→AGC message channel for a running classic job to carry a `JobCancellation` on, the way the real runner's resident `Runner.Listener` receives one.

That leaves three candidate triggers:

| Candidate | Cost | What is unknown |
|---|---|---|
| **A. `renewjob` reports the loss** | free — the loop already runs every 60s | Does the run service stop honouring `renewjob` once a cancelled job is concluded? If it 404s, Q254's existing teardown *already* fires, and Phase 1's actuator alone bounds the waste to GitHub's ~5-minute grace plus one renew interval. |
| **B. REST run-status poll** | an installation-token API call per active run per interval | Nothing — `TokenFunc`, `GitHubAPIURL` and the run identity are all already in the provision goroutine (they are what `rerun-failed-jobs` uses). But at the design's thousands-of-concurrent-jobs scale a 30s poll blows through the installation rate limit, so it needs a long interval and an age threshold. |
| **C. Concurrent session poll** | a second long-poll per running job | Whether the spent session yields anything but empty 200s. Q114 says it does not. |

Candidate A is both the cheapest and the one whose answer decides whether B is needed at all.
The 600s measurement does **not** rule it out: at the time it was taken the actuator was missing, so a renew-loop teardown at ~5 minutes would have looked exactly like no teardown at all.

## Phase 0 — does this exist on the tier that ships? No. (answered 2026-08-01)

The measurement ran against the v1 `ActionsGateway` fixture, which is the **classic** tier.
The default since Q264 P5 is `acquisitionProtocol: ScaleSet`, and that tier has a channel classic does not: its listener holds a live message queue for the whole job, not just the acquisition.

**A cancelled run puts a terminal message on that queue, measured live.** Q468's retention probe cancelled a real run and recorded the result: `POST …/runs/{id}/cancel` accepted, and the job's `JobCompleted` with `result: canceled` on the scale set's queue **~0.2 s later** ([q468-jobcompleted-retention.md](archive/q468-jobcompleted-retention.md), 2026-07-28).
The observation is load-bearing enough that the spelling — `canceled`, one L — is what `scalesetstub` was corrected to.
So the trigger this plan set out to find on classic already exists on ScaleSet and needed no new run to establish.

The actuator downstream of it is also already built, and it is not the Phase 1 one. `completeJob` → `CleanupScaleSetJob` → `markJobCompleted` stamps `actions-gateway.com/job-completed-at` on the job's worker pod, and the reaper deletes a pod still `Running` `completedJobRunningGrace` (5 min) later — the Q420 arm, proven end to end against a real apiserver by `TestV2_RunnerSet_OrphanedRunningPodReaped`.

So the ScaleSet worst case is two graces, not the job's remaining runtime: GitHub's own ~5-minute cancellation grace before it concludes a job whose runner is still going, then the 5-minute reap grace, plus reconcile lag.
Classic's worst case is the job running to completion, bounded only by `spec.maxWorkerLifetime` — 12 hours by default. **Q501 in its unbounded form is a classic-tier defect**, on a tier scheduled for removal at `v2.0.0`.

**What stays unmeasured, and why it does not gate anything.** Q468's cancel had *no runner attached* — it says so explicitly, since producing a `JobCompleted` without one is the whole trick the arming phase turns.
So the ~0.2 s is the latency for a job GitHub can conclude immediately, not for one with a live worker; the composed cancel→queue→stamp→reap figure has each component measured or proven but has never been observed as one number.
It also remains likely that a ScaleSet worker aborts natively long before any of that, since it runs the full runner (`run.sh --jitconfig`) over its own broker session — untested, and now not worth a run of its own.
Neither residual can move the bound above the two graces, which is the only thing Phase 0 was asked to decide.

## Phase 1 — the actuator (shipped)

When the listener abandons a job, the worker pod is reclaimed.

The subtlety is that a process-wide shutdown cancels every job context at once, and must **not** delete live workers — an AGC rollout would kill every running job. `context.Cause` separates the two: `handleJob` derives the job context with `context.WithCancelCause`, the renew loop cancels it with `ErrJobAbandoned` wrapping the teardown reason, and a plain parent cancellation leaves the cause as `context.Canceled`. `provision` reclaims only on the former.

The delete is stamped `actions-gateway.com/deletion-reason: job_abandoned` before it is issued, which is what keeps it from re-entering Q502's graceful-deletion recovery as a disruption — the exclusion `deletion.go` requires of "any future AGC deletion path — e.g. a Q501 cancel-relay that deletes the worker".
A reclaimed worker is counted on `actions_gateway_worker_pods_reaped_total{reason="job_abandoned"}`.

The pod delete is graceful, so the Q385 SIGTERM relay reaches `Runner.Worker` and the runner reports its own outcome rather than being SIGKILLed — the same path a drain takes, measured at 16s to a GitHub conclusion.

## Phase 2 — the trigger (instrumented, awaiting a run)

`E2E_GitHub_CancelledRunLeavesNoDeletionMark` now takes the candidate-A reading as part of the run it already makes.
It baselines the renew loop's two failure lines before the cancel and reports what the window added, and it reads the corroborating half off the pod: a renew-loop teardown reclaims the worker through Phase 1's actuator, which stamps it `deletion-reason: job_abandoned` before deleting it.
That stamp is a *positive* observation of candidate A rather than an absent log line, and the sampled sequence carries it.
The spec labels the outcome under `Q501 candidate A outcome`.

Two outcomes, unchanged:

- **`renewjob` fails definitively.** Nothing more to build.
  Q254's teardown plus Phase 1's actuator bounds a cancelled job's waste to ~5–6 minutes, and Q501 closes with a documented latency rather than a new mechanism.
- **It keeps returning 200.** Candidate B is the remaining option.
  Phase 0 has already narrowed what it would be worth: classic only, on a tier removed at `v2.0.0`, so the bar for building a per-run REST poll is "cheap enough to be obviously worth it" rather than "the last option standing".

**One thing the instrumentation had to fix to stay honest.** The spec's Q459 assertion demanded a terminal phase carrying *no* `deletionTimestamp`, which was the right shape when nothing could delete this worker.
Phase 1's actuator can, so a working gateway would now fail the spec.
The assertion is instead that no terminal phase carries a deletion mark **the gateway cannot account for** — the shape Q502 actually recovers, and the property Q459 was protecting all along.
