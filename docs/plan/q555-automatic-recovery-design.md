# Q555 automatic recovery: design

**Status:** ⓘ proposed design, written 2026-10-10, revised 2026-10-11 after an independent review, and revised again 2026-10-12 with the maintainer's decisions.
Q1182's part is built (`cmd/agc/internal/provisioner/rerun_ledger.go`): the ledger store with its version rules, the `reruns` map, the reservation and `selfCancelledAttempt`; so is Q1181's `run_cancelled`, `attempt_limit`, `run_too_old` and `superseded` on the scale-set tier (`rerun_futility.go`); so is Q555's detection (`flaky_jobs.go`): the `failures` map, the worker snapshot, the pairing and its metrics, built before the dogfood turn-up whose [readings](#readings-from-the-dogfood-turn-up-2026-11-22) (Q1236) refute three of its assumptions, one of them since corrected (Q1376), with the colocated proxy's readiness added by Q1237; the rest is not.
It turns [the requirements draft](q555-flaky-job-retry.md) into components against the code as it stands, one per backlog row, and answers the draft's [open questions](q555-flaky-job-retry.md#open-questions-for-the-design-session).
Each answer is marked **Decided** (the maintainer chose it, 2026-10-12), **Decide** (a recommendation still awaiting sign-off) or **Research** (needs a measurement before anyone can choose).
The draft says *what* and *why*; this says *how*.
Code facts were read from source on 2026-10-10 and 2026-10-11 unless marked otherwise; the assumptions the design rests on that nobody has measured are listed [first](#measure-before-building).

**Goal:** a job that failed for a platform reason GAG can name is re-run without a human, a job that a re-run cannot fix is not, and the rate of re-runs a human still has to make is measured per set.

**Approach:** detection is the substrate.
A per-set failure ledger joins each failed job's platform context to its later attempts, which gives the flake count, the manual re-run rate and each retry cause's success rate.
Retry classes then fire off that context through the re-run path the disruption arms already use, after one run read that both checks futility and enforces a budget that survives a restart.

## Measure before building

Each of these decides a component's shape.
M1 to M4 were measured on dogfood on 2026-11-22 ([the readings](#readings-from-the-dogfood-turn-up-2026-11-22)), and M5 from GitHub's API the same day ([the readings](#readings-from-githubs-api-2026-11-22)); M6 has not been observed beyond the jobs-list half of [a single-job re-run](#readings-from-githubs-api-2026-11-23):

| # | Unknown | Decides | How to settle |
|---|---|---|---|
| M1 | The `result` vocabulary for a red step, a cancelled run and a timed-out job | Which completions the ledger records | Measured: [the readings](#readings-from-the-dogfood-turn-up-2026-11-22) |
| M2 | Whether a re-run job's `JobAssigned` carries a new job UUID and the same `jobDisplayName`, and whether two jobs in one run can share a display name | Pairing's key | Measured: [the readings](#readings-from-the-dogfood-turn-up-2026-11-22) |
| M3 | Whether a container killed at its memory limit takes the runner with it (the pod fails, and GitHub concludes the job only when the job lock lapses, about 10 minutes later) or only a step's process (the runner survives and reports a failed step) | Whether the out-of-memory class fires from the pod watch, like the disruption arms, or from `JobCompleted` | Measured under `Throughput`: [the readings](#readings-from-the-dogfood-turn-up-2026-11-22) |
| M4 | Whether a finished worker's pod object still exists when its `JobCompleted` arrives | Where platform context is read | Measured: [the readings](#readings-from-the-dogfood-turn-up-2026-11-22) |
| M5 | The `superseded` check's answer for a pull request from a fork whose branch is `main` | Whether the check needs more than the filters below | Measured from the API: [the readings](#readings-from-githubs-api-2026-11-22) |
| M6 | Whether `jobDisplayName` matches the jobs API's `name` exactly, for matrix jobs and reusable workflows, and what a single-job re-run does to the jobs it depends on and that depend on it | Whether a classified retry can re-run only its own job ([granularity](#run--or-job-granular-re-run-open-question-2)) | A test workflow with a matrix, a reusable workflow and a `needs:` chain, re-run one job at a time |

M3 rested on recall that kubelets on cgroup v2 kill a container's whole process group on an out-of-memory event; the reading below confirms it on dogfood's nodes.
The breaker thresholds, the 7-day ledger expiry and the deferred path's 24-hour expiry are proposals, not measurements; R14's metrics are what will move them.

### Readings from the dogfood turn-up, 2026-11-22

Taken on the `ci` set in `gag-dogfood`, whose AGC ran `v2.0.1-rc.2`; its detection and retry code is unchanged on `main` at `f414275fa`.
The jobs came from the [`q1236-detection-probe`](../../.github/workflows/q1236-detection-probe.yml) fixture, runs 45052015868 (attempts 1 and 2) and 45053166409, and for M3 the [`q1317-oom-probe`](../../.github/workflows/q1317-oom-probe.yml) fixture, run 45052020040 (attempts 1 and 2).
The readings are the AGC's `scaleset: job assigned`, `job completed`, `scale-set job failed` and `reaped worker pod` log lines, the failure line carrying the record's `context`, and the `scaleset-failures-ci` ledger, read last at 18:04:25 UTC.
`scaleset_jobs_completed_total` itself was not queried: the listener increments it with the same `result` it logs, in the same once-per-job block.

- **M1.** A red step completes `failed` (jobs `82a2530d…`, `abe6cad3…`, `ee99fae0…`).
  A job killed by `timeout-minutes` completes `canceled` (`b31558d4…`, `486169c1…`; the jobs API says `cancelled`), exactly as a user's cancel does (`3762586f…`), so detection records no timed-out job and never counts its later success as flaky ([Q1374](../queue/Q1374.md)).
  A job whose runner an out-of-memory kill ended completes `failed`, 9 min 48 s after the kill, when the job lock lapses (`ff028f4b…`, run 45052020040).
- **M2.** A re-run of the failed job arrives with a new job UUID (`abe6cad3…`, then `4349fbb7…`) under the same run ID and `jobDisplayName`, and pairs as `trigger=other`, flaky, as designed.
  GitHub accepts two jobs sharing one `name:`, and they share one failure key: the green twin's success paired with the red twin's failure and emitted a false `FlakyJobDetected` (18:05:32 UTC), and the red twin's re-run at 18:07:02 logged as a fresh failure rather than a pairing, so its record was gone.
  Q1375 fixed it since ([Pairing and attribution](#pairing-and-attribution-r1-r2-r8)).
- **M4.** A job that ends on a step's result completes before its pod's terminal transition: the completion is logged in the same second as `job started`, and every such record's `context` reads `podPhase: Running` (three in these runs, and `44325908424/scripts-test` from 2026-11-18); in two of the three no container had terminated yet, and in `ee99fae0…` the runner had just terminated `Completed`, exit 0.
  So `Completed` read the running pod every time, and the snapshot the terminal transition took moments later was never taken by any completion; it waited in the 1,024-entry map until evicted.
  Q1376 fixed both since ([What a failed job records](#what-a-failed-job-records-r7)).
  The runner exits 0 when its job fails: both red jobs' pods, and the timed-out job's, were reaped `Succeeded`, so a red test on its own never reads as a fault.
  A kill that ends the runner inverts the order: run 45052020040's out-of-memory worker failed at 17:58:21 UTC, was reaped at 18:03:23, and its completion at 18:08:09 took the terminal snapshot, so that record shows the fault and raised `PlatformFaultOnFailedJob`.
- **M3.** The worker's cgroup is v2 (`cgroup2fs`) and its `memory.oom.group` reads 1, both read from inside the job; the kubelet's `singleProcessOOMKill` is unset on the system pool's node (`configz`, kubelet `v1.35.8-gke.1796000`), and the worker pool's kubelet was not read.
  A step allocating 9216Mi past the runner's 8268Mi limit took the whole container: the pod reached `Failed` with the runner `OOMKilled`, exit 137, at 17:58:21 UTC, and GitHub concluded the job `failure` at 18:08:09 with "The self-hosted runner lost communication with the server" and no step log.
  So the class fires from the pod watch, as built: 2 s after the kill it recorded the floor 8268Mi to 12416Mi, emitted `MemoryLimitRaisedForRetry` and parked the re-run, which it released at 18:09:28, 79 s after the run concluded.
  The re-run's pod carried a runner limit of 12416Mi, its request unchanged at 5568Mi since `Throughput` raises only the limit, and `actions-gateway.com/rerun-cause: oom`; its step read `memory.max` as 13019119616 bytes (12416Mi), survived the same allocation, and paired as `trigger=gag`, `cause=oom`, not flaky.
  `Binpack`, which raises the request with the limit, was not measured: the live set runs `Throughput` ([Q1382](../queue/Q1382.md)).
- **`JobCompleted` carries `eventName` and `jobWorkflowRef`.** Every record holds `event: push` and the fixture's workflow ref, and the listener reads both from the completion message.

### Readings from GitHub's API, 2026-11-22

M5 and the merge-group question beside it were read from GitHub's REST API rather than on dogfood, since the queries the `superseded` check sends can be put to any public repository.
This repository has no `pull_request` run whose branch is `main` (`?branch=main&event=pull_request` returns 0), so the fork readings are from `github/docs`, which has many.
No fork pull request was disrupted end to end, so the AGC's own decode of a fork run is the piece these readings do not cover.

- **M5.** The `branch` filter matches a fork pull request's head branch, and the `event` filter is what keeps the two `main`s apart.
  On `github/docs` workflow 126781987, `?branch=main&event=push` returns only upstream's runs (3, all `github/docs`) and `?branch=main&event=pull_request` only forks' (5, from four forks); on workflow 6878645, the second query's 112 runs come from 58 forks, 6 with a deleted fork (`head_repository` null) and none from upstream.
  Without `event`, the newest `main` run is upstream's `schedule` run on the first workflow and a fork's pull request on the second, so either side would shadow the other.
  Comparing `head_repository` then separates one fork from another: fork `503badrr/docs` has runs 32531300946 and 32531324720 on two different head commits, 19 s apart, so the second supersedes the first.
  `per_page=1` returns whichever fork built last, though, so a fork's own newer commit is hidden whenever another fork's pull request from `main` has built since, and the check misses; a miss re-runs, so it costs a wasted re-run, never a withheld one.
- **A merge-group run that loses its place is not cancelled by GitHub.** On this repository's queue on 2026-10-30, #2190 failed at the head and was removed (`failed_checks`, 17:56:25 UTC), and the groups behind it were rebuilt at 17:56:30; of the old groups' 30 runs, six concluded `success` after the removal, the last at 18:05:13, and the eight `cancelled` were each ended by one of their own jobs exceeding `timeout-minutes` before 17:56:25 (the jobs' check-run annotations).
  So `run_cancelled` does not catch a displaced group's run, and nothing else does ([Q1381](../queue/Q1381.md)).

### Readings from GitHub's API, 2026-11-23

Taken from the REST API for `already_rerun` (Q1320), over public runs somebody had already re-run.

- **An attempt's jobs list, once it is complete, holds every job of the run, and a job the attempt did not run keeps its old times and runner.** Run 45325262167's attempt 2 re-ran `reusable / inner` alone (`POST …/jobs/{id}/rerun`).
  All its 23 jobs carry `run_attempt: 2`, a new `id` and a new `created_at`, so none of those tells a re-run job from a carried one.
  `dup` and `matrix-default (b, x64)`, which failed in attempt 1, are listed as `failure` with attempt 1's `runner_name` and times (`dup` completed 04:18:54 UTC, before attempt 2's `run_started_at` of 04:22:25), so a single-job re-run moves `run_attempt` and carries its siblings' failures.
- **`completed_at` before the attempt's `run_started_at` is the test for carried; before the job's own `created_at` is not.** GitHub lists a job that never got a runner as completed a second before it was created, in the attempt that skipped or cancelled it (`apache/airflow` run 45322354372, `Notify Slack`).
- **The list fills over the attempt's first seconds.** Over 62 re-run attempts in 15 repositories, the last carried job was created at most 15 s after `run_started_at`; a job that waits on `needs` is created when its turn comes, which can be an hour later.
  That a row cannot be read before its `created_at` is inferred; no list was read while filling.
- **`runner_name` is GAG's runner name**, `gag-ci-scaleset-<job UUID>`, and a job whose runner was lost keeps it in its own attempt (run 45052020040, the out-of-memory kill).
  A lost job carried into a later attempt was not observed; every carried failure read was an ordinary red step.

### Readings for pairing jobs that share a name, 2026-11-23

Taken from GitHub's REST API for Q1375.
In run 45052015868, listed with `filter=all`, both `shared-name` jobs appear in attempt 1, each with the runner the AGC registered; in attempt 2 the red twin appears on a new runner, and the green one, which nobody re-ran, appears again on attempt 1's runner.

- **An attempt's carried-over jobs keep their runner, and its re-run ones do not.** In dogfood runs 45052015868, 45052020040, 45325262167 and 45324839276, whose later attempts are all on the scale set, 46 carried-over listings name the runner of the attempt before and 8 re-run jobs a new one.
  Of this repository's 172 runs of 2026-11-01 to 2026-11-22 at attempt 2 or later, all GitHub-hosted, 165 list no job in attempt 1 (a pull request waiting for approval) and say nothing; the other 7 have 97 carried-over jobs on the old runner and 13 re-run ones on a new one.
  No exception in either.
- **The rule, replayed on those 8 re-run jobs' final lists, pairs each of them**, two of which share a name (`shared-name`, and `dup` in 45325262167).
  The lists as they stood at each completion were not kept.
- **A skipped job can run in the next attempt.** In `astral-sh/ruff` run 45313563236 `security-review / security review` failed in attempt 1 and `prepare` and `report` were skipped; attempt 2 ran all three (read in review).
  No run was found where such a job shares the failed one's name, which GitHub allows.
- **A job is not listed until its `needs` finish.** In a unit-test run of [#2391](https://github.com/actions-gateway/github-actions-gateway/pull/2391), every job that needs `changes` is created within a second of `changes` completing, and the gate job in the second the last of them did.
  So an attempt in progress does not show which jobs it will re-run, and a rule that reads only the latest attempt calls the re-run of one twin the re-run of the other while that one waits.
- **A list read in an attempt's first 15 s may be short** ([read for Q1320](#readings-from-githubs-api-2026-11-23)).
  A carried-over twin missing from it reads as replaced, so the re-run counts nothing rather than the wrong thing.
- **A started job already names its runner.** Polling the same pull request's checks, no job was seen in progress without one, the earliest sighting 2 s after its start.
- **The completion's runner name is the job list's**, in 35 of 35 jobs of runs 45325262167 and 45326925782 (dogfood, read for M6), where it differs per execution.
- **A matrix collides too**, where its legs' names pass 100 characters: GitHub truncates both to the same 97 and `...` (run 45326925782, two legs), on the wire as in the API.
  Ordinary legs do not, even under a static `name:`, which GitHub extends with the matrix values.

## Component 1: the failure ledger (Q555, R1 to R8)

### Where the hook goes

The listener's `completeJob` receives every `JobCompleted` and calls `Config.Cleanup(ctx, jobID, runnerName)`, which carries neither the result nor the run identity.
Only the completion metric is deduplicated per job: the cleanup runs on every delivery, deliberately, so a replay after a crash finishes it.
Add a `Config.Completed func(ctx, Job, result string)` beside the existing `Config.Started`, called from `completeJob` after the cleanup and wired in `runnerset_scaleset.go`.
It fires on replays too, so it is idempotent on the job UUID: a completion whose UUID the ledger already holds changes nothing.

There is one listener per `RunnerSet` and one AGC replica per gateway, with no leader election, so per-set state has one writer in steady state.
A rolling update can briefly run two AGC pods, so the ledger is written with a compare-and-swap, as the recovery claims are.

### What a failed job records (R7)

The pod is the evidence, and nothing maps a job to its pod once the pod is deleted.
So the runner-set controller, whose pod watch already sees every worker reach a terminal phase (it drives `RecoverDisruptedScaleSetWorker`), snapshots each worker at that transition into a bounded in-memory map keyed by runner name.
`Completed` takes the snapshot for its runner, falling back to reading the pod if it is still there.
Measured (M4), the fallback is the common path and reads a pod still `Running`: only a kill that ends the runner lets the snapshot arrive first.

So the record is written from whichever is known at the completion, and amended at the terminal transition where that comes later (Q1376, built).
The alternatives:

- **Wait for the transition before recording.** On the listener's goroutine, each failed job would delay every message behind it, new assignments included, by the worker's shutdown; off it, a restart during the wait loses the failure and its pairing, since the message has been deleted, and a worker that never exits needs a timeout that records the running pod anyway.
- **Record only at the transition.** A pod's transition carries no job result, so every worker would need a ledger write, against the rule that writes track failures and not throughput.
- **Only stop snapshotting a completed job's worker.** That fixes the map and leaves the record as it was.

The amendment costs one ledger write per job that fails while its worker runs, and none for a succeeded job.
A worker's later reads are merged, not swapped in: the kubelet drops a restarted sidecar's crash from the status it writes as the pod finishes (kubelet v1.35.5, `testdata/sidecar_restart_statuses.json`), so the pod watch passes a container's restart as well as a phase change, and each read carries forward the terminations the next one has lost.
A pod deleted while it runs is waited for: its finish amends the record, or its deletion event where it never finishes.
The amendment keeps the completion's proxy reading, and adds a fault only where its container or condition is dated before the completion, because a worker that outlives its job can be drained or killed without having failed it.

A snapshot holds only bounded fields, all read from objects the AGC can already see:

- pod phase, reason and message (message truncated to 256 bytes), its conditions (`DisruptionTarget` included), `spec.nodeName`, pod IP and start time;
- per container, sidecars included: name, terminated reason, exit code, restart count, and the termination before the last where a restarted container has since exited, carried from an earlier read where the kubelet has dropped it;
- for a colocated egress proxy, the EgressProxy's `Ready` condition and `readyReplicas`.

**Two things R7 lists are left out, deliberately:**

- **The node's conditions.** The AGC has no `nodes` RBAC and a namespace-scoped cache, and granting a cluster-scoped read to every tenant's AGC is the widening [05-security.md](../design/05-security.md) rules out.
  The pod's own `DisruptionTarget` condition and phase carry what a node-lost class needs.
- **Registry mirror readiness.** It joins the snapshot with the mirror candidate's reproduction, once that settles which object reports it.

A **shared** proxy pool lives in another namespace, which the AGC's cache cannot see, so its readiness is absent too: the snapshot's `proxy` says `mode: shared`.

A restart loses snapshots for workers that finished before it, and their records say `context: unknown`.
That is one of R5's stated blind spots.

### Where the ledger lives (open question 1)

**Decided:** a per-set ConfigMap, `scaleset-failures-<runnerset>`, key `failures.json`, owner-referenced to the `RunnerSet`, written with the same read, modify, compare-and-swap loop as `scaleset-recovery-claims` (`recovery_claims.go`).

- Memory alone loses every pairing on a restart, and an AGC rollout restarts it, so the manual re-run rate would undercount on every upgrade.
- Asking GitHub is stateless but needs a run read per completed job to learn whether it is a re-run, which R6 forbids, because `JobAssigned` carries no attempt number.
- `guards.json` is not the home: it is re-serialised whole by the listener and retired when the assignment message is deleted, while a failure must outlive that by days.
- Kubernetes Events are not either, though the design uses them for every notification: they expire after an hour by default, the client-go recorder drops and aggregates them under load (both from recall), and they have no compare-and-swap, so a budget kept in them could be lost or double-spent.
  The ledger is state the AGC reads back and acts on one record at a time, which needs exact, complete storage.
- A record CRD (one small object per run GAG re-ran) is the upgrade path if the ConfigMap's cap ever binds: no shared size limit and no write contention between runs, at the cost of new public API and an object per re-run.
  The ledger's size is the measure that says when.

The AGC's ConfigMap verbs are `get`, `create` and `update`, with no informer, so every read is an uncached API call.
The AGC loads the ledger once at start and keeps an in-memory copy, updated on each of its own writes and re-read when a compare-and-swap loses.
Lookups, including the per-provision floor lookup below, read that copy and cost nothing.
Q1182 ships without the copy: its only reads are a reservation's own compare-and-swap and the deletion arm's `selfCancelledAttempt` check on a cancelled run, both rare, so each reads uncached; the copy arrives with the first lookup on a hot path, Q1183's floor.
Writes happen on a failed completion and its worker's finish, a GAG re-run (its budget reservation) and a pairing, never on a succeeded job with no record, so the write rate tracks failures and not throughput.

It holds a version and three maps, every entry carrying its own `expiresAt` so any AGC version can age any map, including one it does not understand:

```json
{
  "version": 1,
  "failures": {
    "<runID>/<jobDisplayName>": {
      "expiresAt": "2026-10-17T12:00:00Z",
      "failedAt": "2026-10-10T12:00:00Z",
      "jobID": "6f1c…",
      "runner": "<the failed attempt's runner>",
      "event": "pull_request",
      "workflowRef": "org/repo/.github/workflows/ci.yml@refs/pull/42/merge",
      "context": { "podPhase": "Failed", "containers": [{ "name": "runner", "reason": "OOMKilled", "exitCode": 137 }] },
      "memoryFloor": "6Gi"
    }
  },
  "reruns": {
    "<runID>": {
      "expiresAt": "2026-11-09T12:11:00Z",
      "count": 1,
      "selfCancelledAttempt": 0,
      "attempts": {
        "1": { "scope": "FailedJobs", "cause": "oom", "jobs": ["<jobDisplayName>"], "disruptions": ["<pod or job key>"] }
      }
    }
  },
  "pending": {
    "<runID>/<jobDisplayName>": { "expiresAt": "2026-10-11T12:00:00Z", "cause": "oom", "failedAttempt": 1 }
  }
}
```

`reruns` is keyed by run, not by failure, because a disruption arm fires about 10 minutes before the failed completion that would create the failure entry (the job lock); a run-keyed record needs nothing to exist first.
It is also [the budget](#the-budget-is-gags-own-count-in-the-ledger-r15): `count` is how many times GAG has re-run the run, and `attempts` records, per run attempt GAG re-ran from, what it re-ran and for which disruptions.
`pending` holds the class re-runs waiting for their run to conclude ([How it waits](#how-it-waits)).

`event` and `workflowRef` are on the wire in `JobAssigned` (a live capture in [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md)), but `scaleset/types.go` decodes neither, so decoding them is part of Q555.
`JobCompleted` carries them too (measured on dogfood, [2026-11-22](#readings-from-the-dogfood-turn-up-2026-11-22)), and the listener reads them from the completion; they cost no API call either way.

**Bounds.**

- **Expiry:** failure and pending entries after 7 days and 24 hours respectively; `reruns` entries after 30 days, GitHub's re-run window from recall, so a budget cannot refill while the run can still be re-run.
- **The cap evicts failures, never budgets.** Past a cap sized so the object stays under 512 KiB, the oldest failure entries go first, except one holding a `memoryFloor` whose re-run has not completed.
  A `reruns` or `pending` entry is never evicted to make room: if those alone reach the cap, new reservations fail and their re-runs are withheld, because evicting one would refill its run's budget.
- **The size, measured** by `TestRerunLedger_CapHoldsEnoughBudgets` with 63-character pod names: a `reruns` entry is 476 bytes at the default budget of 2 with 40-character job names, so the cap holds about 1,100 runs GAG re-ran inside 30 days on one set, and 2,533 bytes at the field's ceiling of 10 with 100-character names, about 206 runs.
  Q555 extended it to the `failures` map (`TestRerunLedger_CapHoldsEnoughFailures`), and evicts failure records past *half* the cap rather than at it, because `v1.10.0` shares the object and cannot evict them.

### Pairing and attribution (R1, R2, R8)

On a completion for a key with a failure entry, the completion is either a later attempt of that job or another job of its name (M2), and carries no attempt number.
The run's job list over every attempt settles it, joined by the runner's name, which both sides carry (Q1375, built): the jobs of the completion's name must differ between its attempt and the one before by exactly two, the recorded failure's runner gone and the completion's added.
A job with no runner matches nothing, so a skipped one always differs: a twin skipped beside the failure can run in the re-run and complete first, and one that is skipped both times costs its name's re-runs, an undercount.
Anything else pairs with nothing, including a run that cannot be read: R5 accepts an undercount and not a false flake, and R6 allows the read because only a re-run or a shared name reaches it.

The alternatives:

- **Pair only a job assigned after the failure was recorded.** Wrong without a restart too: on a set at capacity the second twin is assigned after the first has failed, and it ran 3 min 40 s after the red one ended in the measured run.
  After a restart every assignment the AGC never saw would also have to be guessed.
- **The run's `run_started_at` against the failure's time.** One small read and no list, but it calls a re-run of all jobs, or of the twin alone, a re-run of the failed job: two false flakes left.
- **The message's own `queueTime`.** Free, and unmeasured on a re-run's completion; a value that did not move would end pairing without a sign.
- **The latest attempt's job list alone.** Half the rows, and a false flake where both twins re-run and the failed one waits on its `needs`, since it is then not listed.
- **Count a skipped twin as carried while it stays skipped.** It would keep the re-runs of a job beside a same-named placeholder, but two jobs can trade places, one skipped in each attempt, and the list does not say which is which.
- **Match every job by name in the list**, without the runner.
  Two jobs of one name cannot be told apart that way.

The list is read off the listener, once per run and set for the matches of 30 seconds, and its 10-second budget is the batch's: `TestPairing_OneListPerRunAndDelay` re-runs 100 of a run's 200 legs, 400 rows, for 4 requests and one ledger write, and `TestPairing_AHungReadHoldsNoListener` has GitHub never answer; in both, the listener waits for neither.
A run whose matches do not finish together costs a list per 30 seconds, so at most 1,200 requests an hour at the 1,000-row cap, and never more lists than matches.
The alternatives to that:

- **A read per match, on the listener.** Those 100 matches would be 400 requests, with every other message of the set behind them, for up to 10 seconds each where GitHub is slow.
- **One list per run and attempt, kept.** A completion names no attempt, so which list it belongs to is what the read is for, and a kept list lacks whatever started after it.
- **A cap on reads, counting nothing past it.** It drops the re-run of a large matrix, the case with most to count.

One false flake is left, and nothing in the list tells it from a re-run: a job first listed under the failure's name in a later attempt, completing while the failed job's re-run waits on its `needs` and is not listed.
A job in the workflow file is listed in every finished attempt, skipped or not, so it takes a name that expands differently between attempts, such as a dynamic matrix gaining a leg whose name truncates to the failed one's.

A pairing then counts as:

| GAG's re-run of the attempt the job failed in | This job in its `jobs` | New result | Counts as |
|---|---|---|---|
| a disruption cause | yes | `succeeded` | recovered by that cause (R14), not flaky (R2) |
| a classified cause | yes | `succeeded` | that class's success (R14) |
| any cause, `scope: FailedJobs` | no | `succeeded` | flaky, re-run by GAG as collateral |
| none | | `succeeded` | flaky, re-run by somebody else (R8) |
| any | any | `failed` | the entry is updated, keeping the first failure's context |

Attribution happens when the later attempt completes, so the order of a disruption arm and a failed `JobCompleted` does not matter: an evicted job's failure arrives about 10 minutes after the eviction, a deleted one's 15 to 26 seconds after.
A succeeded pairing deletes the failure entry.
A disruption whose pod belongs to another `RunnerSet` marks that set's ledger, not this one's, so a run spread across sets reads its collateral re-runs here as `other`: an R5 blind spot, overcounting the manual rate for multi-set runs.

### What the operator sees (R3, R4, R8)

Detection is on for every scale-set `RunnerSet` with no field (R4); it changes no behaviour.

| Metric | Labels | Meaning |
|---|---|---|
| `actions_gateway_scaleset_job_reruns_total` | `namespace`, `runner_set`, `trigger` (`gag`, `gag_collateral`, `other`) | A later attempt of a failed job completed. `trigger="other"` over `scaleset_jobs_completed_total` is the manual re-run rate, the success metric |
| `actions_gateway_scaleset_flaky_jobs_total` | `namespace`, `runner_set`, `trigger` | A failed job later succeeded with no disruption or class attributed |
| `actions_gateway_eviction_rerun_outcomes_total` | `namespace`, `runner_group`, `tier`, `cause`, `event`, `result` | The result of each attempt GAG re-ran, per cause (R14); `result="failed"` over the total is a cause's failure rate |

The third keeps the `eviction_` prefix and `runner_group` label of the family it extends, so one query joins it to `eviction_retries_total`.
No job or repository name is a label (R3).
Per occurrence, the names go to:

- **a Normal Event, `FlakyJobDetected`,** on the set when a pairing reaches the flaky verdict, naming the run, the job and the trigger;
- **a Warning Event, `PlatformFaultOnFailedJob`,** when a failed job's snapshot shows a fault (a non-zero or `OOMKilled` container, a `DisruptionTarget` condition, an unready colocated proxy), not on every red test;
- **a log line per failed job carrying the snapshot**, at Warn when the snapshot shows a fault and Info otherwise.
  A log store that keeps only warnings, as dogfood's does, still keeps every platform fault.

## Component 2: one run read before every re-run (Q1182 budget, Q1181 futility)

### What the read decodes

`runConclusion` (`eviction.go`) decodes `status` and `conclusion` from `GET /repos/{o}/{r}/actions/runs/{id}`, and only the deletion arm calls it.
Widen the decode to `run_attempt`, `created_at`, `event`, `head_branch`, `head_sha`, `head_repository` and `workflow_id`, and call it on every arm before every attempt in `rerunUntilAccepted`.
It keeps the existing failure handling: a transport error or a 5xx retries inside the window and ends as `conclusion_unknown`, a 4xx ends at once.
That is a new failure mode for the four arms that do not read the run today, and it fails in the safe direction: a run GAG cannot read is not re-run, and the re-run call needs the same API anyway.
The read, and the workflow-runs list below, need only read access to Actions, so R17 holds.

### The budget is GAG's own count, in the ledger (R15)

Today the budget is an in-memory count per run id, reset by a restart and not shared between replicas.
**Decided:** keep counting only GAG's own re-runs, and move the count into the ledger's `reruns` map.
`maxEvictionRetries` keeps its meaning, so nothing about the field changes and no release note is needed for it.

**Reserving a slot is a ledger write, taken once per disruption.** A disruption has a stable key: the worker pod's name for the disruption arms, the run and job for a class.
After the futility checks pass, a compare-and-swap reads the run's `reruns` entry and does one of four things, in order:

- **Resumes** when the key is already in some attempt's `disruptions`: this is the same disruption after a restart or a later turn of its own retry loop, which re-uses its reservation and never takes a second.
- **Merges** when the attempt the job failed in already has a re-run reserved that covers this job: `scope: FailedJobs` covers every failed job, `scope: Job` covers only the jobs it lists.
  The key is added to `disruptions`, the job to `jobs`, and no slot is spent.
- **Refuses** when `count` has reached `maxEvictionRetries`: `budget_exhausted`.
- **Reserves** otherwise: it increments `count` and records the attempt, scope, cause, job and key.

The reservation comes after the checks and before the call, so a re-run a check withholds spends nothing.
Once taken, a slot stays spent whether or not the call lands, as `rerunUntilAccepted` does today: refunding a failed call would let one disruption that keeps failing spend the whole budget in turn.
A write that fails withholds the re-run, the safe direction.

The merge also fixes a defect in today's code.
Two disruptions in one run each reserve a slot and each start a retry loop (`handleEviction`, read 2026-10-11): the second keeps meeting `403 This workflow is already running` while the first re-run is in progress, and lands a second re-run once that attempt concludes inside its 15-minute window.

A human's re-runs do not count, so a run somebody re-ran by hand can still be recovered.
GitHub's own cap of 50 attempts, which humans and GAG share, is the `attempt_limit` check below.
The ledger is per scale-set `RunnerSet`, so the classic tier keeps its in-memory count; classic is terminal, so it is not worth the second store.
A run whose jobs span two sets has a budget in each, so it can be re-run up to twice `maxEvictionRetries`, which the operator docs must say.
The two sets cannot merge across ledgers; what stops both re-running the same job is `already_rerun` below, which asks whether the job itself was re-run rather than whether the run's attempt moved.

### The futility checks (R18), in order

Each check runs on the same read, before the budget is touched, and a hit is counted on the existing `actions_gateway_eviction_rerun_withheld_total{tier, cause, reason}`, whose `reason` today is only `run_cancelled` and whose `cause` is always `deletion`.
The `EvictionRerunWithheld` Event names the reason and the fix.

| `reason` | Condition | Fix the Event names |
|---|---|---|
| `already_rerun` | The job itself has a later attempt than the one it failed in: unchanged `run_attempt` rules that out with no extra call, and a moved one is settled by the latest attempt's jobs list (`GET …/runs/{id}/attempts/{n}/jobs`) | None: somebody re-ran it |
| `run_cancelled` | `conclusion: cancelled`, unless the run's `selfCancelledAttempt` equals the current `run_attempt`, meaning GAG's own force-cancel produced this cancel | None, as Q811 |
| `run_succeeded` | `conclusion: success`; GitHub refuses with a 403 | None |
| `attempt_limit` | `run_attempt > 50`: GitHub caps a run at 50 re-runs, so attempt 51 is the last (changelog, 2026-04-10) | None |
| `run_too_old` | `created_at` older than 30 days | None |
| `superseded` | A newer run of the same `workflow_id`, `event` and `head_branch` from the same `head_repository`, with a different `head_sha`, exists (`GET …/workflows/{id}/runs?branch=…&event=…&per_page=1`), for `push` and `pull_request` only | None: a newer commit is building |
| `same_class_again` | Class only: this job's failure entry already records a re-run by the same cause that failed again | The class does not fit this job; investigate |
| `budget_exhausted` | The budget above | As today's `EvictionRetriesExhausted` |
| `worker_unstartable` | A worker of the set is placed and in startup backoff (Q714), and none has started since the oldest such worker was placed | The verdict's own remediation |
| `own_limit` | A class hit its own limit (see [Component 3](#the-first-class-out-of-memory-under-right-sizing)), or the eviction arm saw a storage-limit eviction (shipped, Q1177) | Raise the limit, or enable a sizing profile |

Notes on the table:

- **`selfCancelledAttempt` replaces an arm exemption.** The abandoned arm records the attempt it force-cancels before cancelling, so the check holds on whichever arm a merge lets through, and a user's cancel of a later attempt is never mistaken for GAG's.
  Q1182 shipped first, so Q1181 reads `selfCancelledAttempt` on every arm from the start and needs no arm exemption; until Q1181, only the deletion arm checks the conclusion, and it already honours the record.
  The abandoned arm also reads the conclusion **before** force-cancelling: a run a user or a concurrency group already cancelled is withheld, not cancelled again and re-run.
- **`already_rerun` asks about the job, not the run.** A single-job re-run moves `run_attempt` ([measured](#readings-from-githubs-api-2026-11-23)) without touching the job's siblings, so "the attempt moved" would withhold a job that nobody re-ran.
  The jobs-list call it needs happens only when the attempt has moved, which is rare.
  An arm that does not know its attempt takes it as moved when `run_started_at` is later than the worker's provisioning ([as built](../design/04-operational-flows.md#futile-re-runs-are-withheld-before-a-slot-is-spent-scale-set)).
- **`superseded` is the other extra call**, and only on the two events where a branch can move.
  Filtering on `event` and comparing `head_repository` keeps a fork's `main` from matching upstream's (M5, [measured](#readings-from-githubs-api-2026-11-22)); with `per_page=1` another fork's newer run can hide the fork's own, which re-runs.
  A merge-group run that loses its place is not cancelled by GitHub, so `run_cancelled` does not catch it ([measured](#readings-from-githubs-api-2026-11-22), [Q1381](../queue/Q1381.md)).
- **`worker_unstartable` reads the startup half of the verdict, on every set** (Q1320), gated or not, since Q906 publishes it ungated.
  The placeability half is left out: a full cluster reads as unschedulable, so a drain on a fixed-size cluster would withhold every disrupted job's re-run, finally, where the re-run's job would only have queued; and the arm it was for has no need of it, because the abandoned arm is released by a pod being bound.
  It runs before the reservation, where it spends nothing, so a run past its budget can report it in place of `budget_exhausted`.
  It matters most on the abandoned arm, which by design releases on any pod of the set being bound to a node (Q512's test for capacity), including one stuck pulling an image that every next pod will fail to pull too.
- **`budget_exhausted` keeps its own counter**, `eviction_retries_exhausted_total`, so existing alerts keep working.
- Storage-limit eviction is already an `own_limit` hit on the eviction arm (Q1177): the kubelet's limit message with no `TerminationByKubelet` condition, withheld before the budget is touched ([the measurement](../design/04-operational-flows.md#an-eviction-for-the-pods-own-limit-is-not-a-disruption)).

**Two of the draft's futile shapes get no check, because GAG cannot see them:**

- **The platform's own image exits 127 in a setup step.** That is a step's exit code, which only the job log carries, and R9 rules out classifying from logs.
  The startup verdict covers the image failures that show on the pod.
- **The account cannot run jobs.** GitHub refuses before any job is assigned, so no GAG re-run path ever starts.

## Component 3: retry classes (Q1183, R9 to R17)

**Built** (Q1183) for the out-of-memory class only, with five departures from what follows; [§4.2](../design/04-operational-flows.md#out-of-memory-under-right-sizing-the-first-retry-class-scale-set-q1183) is the account as built:

- **No `pending` map at first.** The class re-ran on the disruption arms' path, inside its 15-minute window; Q1244 built the persistent wait since, with the differences listed under [How it waits](#how-it-waits).
- **The floor is its own map**, `memoryFloors`, keyed by run and job, not a field on the failure entry: the failure record is written on `JobCompleted`, which can arrive after the re-run's assignment needs the floor, and is deleted by the pairing a later attempt's completion makes.
  A floor lives its 7 days instead, so the second kill it finds is `same_class_again`.
- **The breaker cools down rather than half-opens**: 30 minutes after it opens it closes with an empty window, since nothing observes a single trial re-run's outcome inside that time.
- **No `auto_retry_paused` gauge and no misclassification alert at first**: the pause was a `breaker_open` withhold and one `AutoRetryPaused` Event.
  Q1245 added both and the `rerun-cause` pod annotation since; the alert counts at least 5 re-runs before it judges a share, and the annotation is set from a ledger that records re-runs by run, not attempt.
- **The profile is read off the killed pod**: its `actions-gateway.com/sizing-profile` annotation says the profile derived its limits when it was built, and the set's current profile supplies the headroom and ceiling.

### Where a class fires

A class is a function over a failed worker's snapshot that returns a cause or nothing, plus a declaration of how its cause clears (R11):

```go
type recoveryClass struct {
	cause     string                         // the counters' cause label
	recognise func(snapshot) bool            // R9: facts GAG owns only
	clears    clearance                      // freshPod, observable, or unobservable
	ready     func(ctx, owner) (bool, error) // for observable: has the cause cleared?
}
```

It runs where M3 says the evidence is first complete: on the worker's terminal transition, beside the disruption arms, if the class's failure kills the runner; on `Completed` if the runner survives to report.
An out-of-memory kill takes the runner with it (M3, [measured](#readings-from-the-dogfood-turn-up-2026-11-22)), so the first class fires from the terminal transition, as built.
Either way it reads the same snapshot detection records, which is why Q1183 waits on Q555.

For a class whose job ends on a step's result ([Q1315](../queue/Q1315.md), [Q1316](../queue/Q1316.md)), the completion is where it fires, and the amendment (`amendFailedJob`, Q1376) adds what only a later read of the pod shows:

- **The proxy class** fires from `Completed`, which holds the result and the proxy readiness together.
  The amendment never replaces that reading, so it changes nothing for the class, and a restart after the record is written costs it nothing.
- **The sidecar class** fires from `Completed` where the running pod already shows the crash, as a restarted container's last termination.
  A crash the pod reports later, which the near-simultaneous end of a sidecar and the step that used it makes likely, is first known to the amendment, so the class also judges the record there, and the crash's time tells one before the job's end from one after.
  It cannot read the crash from the finished pod alone: the kubelet drops a restarted container's earlier termination from the status it writes as the pod finishes (measured on kubelet v1.35.5, 2026-11-23), so the AGC carries it from the events before.
- **A restart between the two** loses only what the completion had not seen: a sidecar crash reported after it does not fire the class.
  Fewer re-runs, never a wrong one.

### How it waits

Classes go through one deferred path, built beside the abandoned arm's (Q691, `abandoned_rerun.go`) but persistent:

- **Pending re-runs live in the ledger**, in its `pending` map, keyed by run and job.
  The abandoned arm's waiting set is in process memory, so a restart drops it, which costs that arm at most 30 minutes of waiting; a class can wait hours for a run to conclude, so its pending entries must survive.
- **A pending entry records the attempt the job failed in**, read from the run when the class fires, as `failedAttempt`.
  Release judges `already_rerun` and the merge against that attempt, not against a fresh read: if a disruption arm has already re-run the run in the meantime, a fresh read would take the new attempt as the baseline and re-run it again.
  The abandoned arm keeps its own path unchanged.
- **Release needs the run concluded.** A failed job's siblings can run for an hour, and `rerunUntilAccepted`'s 15-minute window would expire into `run_never_concluded`.
  The sweeper reads the run every 30 seconds for the first 5 minutes, every 2 minutes to the first hour, then every 10 minutes.
  An entry expires after 24 hours, with `reason="run_never_concluded"`, once one last read finds the run still in progress, or reads keep failing for half an hour past it (a concluded run is released, Q1254): GitHub lets a run go on far longer, but by then a person has seen the failure, and the cap bounds the polling at about 175 reads per entry.
- **Release then needs the cause cleared:**
  - `freshPod` releases at once;
  - `observable` releases when `ready` is true;
  - `unobservable` releases after a backoff of 1, 4, then 16 minutes by attempt, each with ±25% jitter.
- **Then it re-runs.** Release calls `handleEviction` with no delay, which runs [Component 2](#component-2-one-run-read-before-every-re-run-q1182-budget-q1181-futility)'s read, checks and budget, and `rerunUntilAccepted`.
- **Built in Q1244 for the out-of-memory class, with three differences.** The slot is taken when the class fires, with the pending entry in the same ledger write, so the breaker and the `MemoryLimitRaisedForRetry` Event still count only a run that holds a slot; release takes the read and checks again and its reservation resumes that slot.
  Only the `freshPod` release is built, the one the first class needs.
  A restart finds the sets to sweep through the `RunnerSet` reconciler, which hands each set to the sweep once per process to read its ledger, because the AGC runs no ConfigMap informer and a ledger does not hold the `Target` a release needs.
  The operator-facing account is [04-operational-flows.md](../design/04-operational-flows.md#out-of-memory-under-right-sizing-the-first-retry-class-scale-set-q1183).
- **The re-run's pods say so (R16).** The provisioner, which already looks up the ledger for a floor, stamps each pod of a run GAG re-ran with `actions-gateway.com/rerun-cause`, so [cost attribution](../operations/cost-attribution.md) can split re-run compute by cause.

### The breaker (R11)

One breaker per `(set, class cause)`, in memory, covering classes only.
The disruption arms are outside it: a Spot mass-preemption is exactly when their recovery is wanted, and a stampede rule would stop it.

- **It opens on a stampede:** the cause fired for more than 10 distinct runs in 10 minutes, which is a correlated outage, not a set of independent failures.
- **While open**, a fire is withheld with `reason="breaker_open"`, and a gauge, `actions_gateway_auto_retry_paused{namespace, runner_group, cause}`, reads 1.
  It half-opens after 30 minutes and lets one re-run through.
- **A restart closes every breaker**; the per-run budget still bounds what that can cost.

**Misclassification is an alert, not a breaker.** A class whose re-runs keep failing shows in `eviction_rerun_outcomes_total` over days, a window an in-memory breaker would never fill between upgrades.
A `PrometheusRule` alert fires when a cause's failed share over 7 days passes 50%, and the per-job `same_class_again` check above stops the repeat on any one job.
The thresholds are controller constants and rule parameters, not API: they will move once R14 has data.

### The first class: out of memory under right-sizing

Recognised when a worker container terminated `OOMKilled` and the set's `spec.sizing.profile` is `Binpack` or `Throughput` with `sizingProfileState: Active`, which needs every container to have 20 sampled jobs (`MinSamplesForDrift`).
Under `Static`, or `AwaitingSamples`, the same snapshot is withheld as `own_limit`: the limit is the template's and the next pod gets the same one.

**It clears by observation, and GAG computes the observation.** The aggregate cannot be counted on to move, because the sampler records an out-of-memory job's peak at or below the limit that killed it.
So the class sets the next attempt's limit directly from the killed pod's:

```
headroom = 1.4 (Binpack's memLimitHeadroom) | limitHeadroomPercent / 100 (Throughput, default 1.5)
ceiling  = spec.sizing.maxRequests.memory if set, else 2 × killedLimit
next     = min(roundUp64Mi(killedLimit × headroom), ceiling)
```

If `next <= killedLimit` (a ceiling reached, or `limitHeadroomPercent: 100`), the fire is withheld as `own_limit`.
**A job gets one bump.** A second out-of-memory kill on the same job is `same_class_again`, so growth is bounded by the ceiling, at most 2× the limit that first failed when `maxRequests` is unset, whatever `maxEvictionRetries` and `limitHeadroomPercent` allow (up to 10 and 1000% respectively).
Under `Binpack`, requests equal limits, so the request rises with it; under `Throughput`, only the limit.
Clamping the bump is deliberate, because a retry must not buy memory a profile would refuse; `Throughput`'s own limit is clamped by `maxRequests` too (Q1327).

**The override needs a per-job hook, which does not exist.** `Target.Resolve(ctx)` takes no job, and `buildPod` uses the job only for annotations.
Add a floor lookup after `Resolve` in `ProvisionScaleSetWorker`, keyed by the `ScaleSetJob`'s run id and job name, which `JobAssigned` already carries, against the ledger's in-memory copy.
The floor is the failure entry's `memoryFloor`, pinned against the cap until the re-run completes, and consumed by the pairing that deletes the entry.

**It masks one thing, so it says so.** A pull request that doubles a job's memory gets the memory on retry.
Each bump emits a Normal Event, `MemoryLimitRaisedForRetry`, naming the job, both limits and the run, and is counted under `cause="oom"`, so a rising rate is a visible regression and `SizingDrift` still reports a template below observed peaks.

### Classes after the first

Each needs R12's dogfood reproduction first.
In the order the CI history and existing machinery make cheapest:

1. **Node lost under a running worker** (`freshPod`): no longer a class to build here, because Q1179 added a `node_shutdown` recovery arm keyed on the pod's `DisruptionTarget` condition (`TerminationByKubelet` or `DeletionByPodGC`) after kubelet and PodGC source showed no existing arm fired.
2. **Egress proxy pool unavailable** (`observable`), for a colocated pool: the snapshot's proxy `Ready` was `False` or `readyReplicas` was 0 while the job ran, and `ready` is the EgressProxy's `Ready` condition.
   The AGC already watches EgressProxies in its own namespace, so this needs nothing from Q1178.
   A shared pool in another namespace is invisible to it, so the class does not cover one.
3. **`dockerd` sidecar crash** (`freshPod`): the Docker-in-Docker sidecar's restart count rose, with a discriminator against tenant disk use still to find.

**Decide (open question 3):** measure in that order, and ship only the out-of-memory class under Q1183; each later class is a row of its own once reproduced.

## Component 4: the proxy as a sensor (Q1178, Q1184)

### Outcome metrics (Q1178)

Built.
`checkDestination` returns the allowlist entry that admitted a connection, as written in the allowlist (a host suffix or a CIDR), beside the dial address, where it used to return only whether one did.
Two defects it fixed on the way:

- **A failed lookup on the CIDR path read as a policy denial.** A DNS outage then looked like "destination not allowed", which is exactly the misreading that would make a futility check withhold a retry it should make.
  It is `outcome="dns_error"`, for a lookup that failed without an answer; a name that does not exist (`IsNotFound`) stays `denied`, since a workload probing nonexistent names is still probing.
- **Every dial failure was one unlabelled counter**, though `errors.As` can tell the kinds apart.

| Metric | Labels |
|---|---|
| `actions_gateway_proxy_connects_total` | `destination` (the matched entry; `unlisted` for a denial; `any` with no allowlist), `outcome` (`tunneled`, `denied`, `dns_error`, `refused`, `timeout`, `unreachable`, `error`) |

`destination` takes one value per allowlist entry plus `unlisted` and `any`.
Admission caps the operator's entries at 64 host suffixes and 64 CIDRs (`egressproxy_types.go`); the GMC adds six implicit GitHub suffixes and one per GitHub Enterprise Server host a referrer binds (`proxyAllowlistEnv`), so the bound is that sum plus two, times seven outcomes, per pool.
The two unlabelled counters stay, as sums of the new one, until a release note retires them: `…_connect_denied_total` is the `unlisted` series, and `…_dial_errors_total` the failures on every other destination.

### Why the AGC does not read the proxy yet

Reading a replica's `/metrics` from the AGC needs three changes, none of them small:

- **A network path.** The generated NetworkPolicies admit port 8443 only from namespaces labelled `metrics=enabled`, and the AGC's own policy allows no egress to it.
- **A credential.** The port demands a client certificate, and the `<ep>-metrics-client` Secret is published for the monitoring stack and mounted nowhere.
- **Discovery.** The AGC can list same-namespace pods by label, but it has no EndpointSlice RBAC, and a shared pool lives in another namespace that its namespace-scoped cache cannot see.

The out-of-memory class and the proxy-unavailable class need none of it.
So Q1178 ships metrics for operators only, and the per-destination breaker the draft sketched waits for Q1184, where the new path and credential get their security review together.

### Per-job attribution (Q1184), the shape to review

- **The proxy** keeps a bounded in-memory ring of recent failures (denied, DNS, dial): time, client IP, the matched entry or, for a denial, the host, and the outcome.
  It records no successful connection.
- **The AGC**, on a failed job whose snapshot carries a pod IP, asks each ready replica for failures from that IP inside the job's start-to-end window.
  The window guards against pod IP reuse.
- **What the AGC keeps is a retention decision**, the one [05-security.md](../design/05-security.md) makes opt-in for per-job egress records.
  The shape to review keeps only counts by outcome and matched entry on the ledger entry, never a denied host, and puts the host in the `PlatformFaultOnFailedJob` Event alone, which expires with the cluster's Event TTL.
  Whether even that is acceptable by default is the review's call.
- **The futile and retry shapes it unlocks** are the draft's proxy rows: a denial is futile, and an upstream that failed and then recovered is a class that clears by observation.

## The CRD (open question 5)

**Decided:** no new field and no new condition; classes are on by default.

- Detection changes no behaviour, so it needs no switch (R4).
- Classes spend the existing budget and inherit `maxEvictionRetries: 0` as the off switch.
  That follows what ships: the disruption arms are on by default, so a platform cause is retried by default.
- The out-of-memory class fires only on a set whose owner chose a right-sizing profile, so the opt-in is already on the object.
- A per-class switch waits for its consumer: the first class some operator wants off.
  Then it is a rule in `spec.retry.rules`, under [Built to extend](#built-to-extend), not a boolean per class.
- The breaker reports through a gauge and an Event rather than a condition, because its state is in memory: a condition would flip on every restart, and two AGC pods during a rollout would write it in conflict.

Nothing in the API changes: the budget keeps `maxEvictionRetries`' meaning.

## Built to extend

The maintainer's direction (2026-10-12): not every option above needs to be configurable, but the API and the storage must be shaped so any of them can be added later without a breaking change or a migration.
Nothing in this section ships with the first rows; it fixes the shape the first configurable option will take.

### The API shape

**Decided** (2026-10-12): one home and one shape for every future retry option.

- **One home.** New retry configuration goes under a `spec.retry` block, typed as a standalone `RetryPolicy` so a gateway-wide default can reuse it later.
  `maxEvictionRetries` and `evictionRetryDelay` stay where they are: moving shipped fields is churn with no reader asking for it.
  The block is named for its subject, as `sizing` and `capacityGate` are, rather than `retryPolicy` after the Kubernetes Job API's `podFailurePolicy`; the list inside it is `rules`, because each entry is one rule of the policy (maintainer's choice, 2026-10-12).
- **One policy per set.** A set has one scale set and one budget, so one policy decides each job.
  Sharing a policy across sets, if that is ever wanted, is a gateway or template default of the same `RetryPolicy` type, or a reference to a named policy object beside `rules`, never several policies in line.
- **One shape: an ordered rule list.** `spec.retry.rules[]`, first match wins, and a job no rule matches gets the built-in behaviour.

  ```yaml
  spec:
    retry:
      rules:
        - match:
            workflows: [".github/workflows/release.yml"]
          action: Never
        - match:
            causes: [oom]
            events: [pull_request, merge_group]
          action: Retry
          retry:
            scope: Job
  ```

  Every `match` field is optional, its values OR together, and the fields AND together; an absent field matches anything.
- **Why a list and not a field per axis.** A new axis (an environment, a branch) is a new optional `match` field, which every stored object lacks and so reads as "any": no existing rule changes meaning.
  Top-level per-axis fields could not say "never for `release.yml`, retry with `scope: Job` for everything else", which is what an ordered list of matches buys.
- **A discriminated union for the verb.** `action` is the discriminator, `Retry` or `Never`, and `retry` is the member that only `Retry` carries, holding `scope` (`FailedJobs` or `Job`), which is where [granularity](#run--or-job-granular-re-run-open-question-2) becomes configurable per rule if M6 allows it.
  One CEL rule on the list item ties `retry` to `action: Retry`, the same shape as `WorkerSizing`'s `nodeShare` tied to its `profile`; a future verb gets its own member and its own clause.
- **Enum values are additive, and a skewed AGC fails safe on verbs.** CRD enum validation runs only on writes, so an AGC older than its stored objects can still read a value it does not know.
  It treats an unknown `action` or `scope` as `Never`.
- **An unknown `match` field fails open, and that is bounded.** An older AGC drops a match field it does not know, and a rolled-back CRD schema prunes it (from recall), so the rule matches more than it says.
  A widened `Never` rule only retries less.
  A widened `Retry` rule can take a job a later `Never` rule would have caught, but a rule cannot retry anything the built-in behaviour would not (see below), so the worst a rollback across a new axis does is restore the built-in behaviour for those jobs.
  The release that adds an axis says so in its notes.
- **List semantics,** per api-review: atomic, `MaxItems` bounded, and the first-match ordering stated in the godoc as a contract admission does not check, as `priorityTiers`' ordering is.
- **What a rule cannot do:** turn a futility check off, or raise a budget.
  Rules narrow what is retried and how; `maxEvictionRetries` stays the only budget, and futile shapes stay futile.

### The code shape

One decision function, called by every arm and class after the futility checks and before the budget:

```go
// decide returns what the set's retry policy says for one failed job.
func (p RetryPolicy) decide(f retryFacts) retryDecision // {action, scope, rule index}

type retryFacts struct {
	cause, event, workflowRef, headBranch string // headBranch from the run read
	// an axis added later is a field here and a match field on the rule
}
```

The disruption arms call it too, so a rule can turn one off for one workflow, not only the classes.
With no `spec.retry`, it returns today's behaviour.

### The storage shape

**Built** (Q1182): the ledger's compatibility rules below.
The ledger is internal, not API, but two AGC versions share it during every rollout and every rollback, so it is built to be read and written by a version that does not understand all of it:

- **Additive changes need no version gate.** Unknown fields survive a rewrite and every entry ages itself by `expiresAt`, so a new field or a new map needs nothing from an older AGC, and a rollback across one changes nothing.
  This is the expected case, and it is why a rollback is safe.
- **`version` and `minWriterVersion` are for the exception.** `version` is the schema a writer produced; `minWriterVersion` is the oldest AGC that may still write it.
  Only a change an older AGC cannot write safely raises `minWriterVersion`, and it is made the way an API version change is: one release that reads both shapes and still writes the old one, then one that writes the new.
  An AGC below `minWriterVersion` reads but does not write: detection pauses and reservations fail, so re-runs are withheld.
  Nothing lowers `minWriterVersion`, so a rollback below the first of those two releases stays read-only until the ConfigMap is deleted, which resets every budget and drops every pairing; that release's notes say so.
- **Unknown fields survive a rewrite.** Each entry, and the top level, keeps fields it does not recognise as raw JSON and writes them back.
  Go's decoder drops unknown fields by default, so an older AGC doing a compare-and-swap would otherwise erase whatever a newer one added.
- **New data is a new key.** Maps are keyed by stable identifiers (run id; run and job), so a new kind of record is a new top-level map, as the pending re-runs already are, never a reinterpretation of an existing one.
- **Record the facts before the rule exists.** Every failure entry carries every `retryFacts` field GAG can read (`cause`, `event`, `workflowRef`, and `headBranch` once a run read has happened), so when a rule ships, the ledger and the log lines already show what it would have matched.
  `actions_gateway_eviction_rerun_outcomes_total` gains an `event` label for the same reason: GitHub's event list bounds it, and it is the one axis cheap enough to keep as a label.

## Event scope (open question 4)

**Decide:** classes inherit the arms' scope, every event, and detection records each failure's `event` and `workflowRef` so the data shows which scopes retries help or hurt before anything is made configurable.

The event is a proxy for two properties that matter more, whether a re-run has side effects and whether it is still useful later:

| Group | Events | Why it might behave differently |
|---|---|---|
| Validation | `pull_request`, `pull_request_target`, `merge_group`, `push` to a non-default branch | Safe to retry; `superseded` matters most here |
| Side effects | `release`, `deployment`, `workflow_dispatch` (often a deploy), `push` to the default branch (often a publish) | A retry can publish or deploy twice if the failed job got partway |
| Time-sensitive | `schedule` | A retry hours later may not be worth running |

When an operator asks for a knob, the candidates, in the order they say most about a job, are:

- **The workflow file** (`jobWorkflowRef`), which names `release.yml` outright rather than guessing from its event.
- **The GitHub Environment** the job deploys to, the most direct marker of a deploy job; whether the job message or any API GAG already calls exposes it is unverified.
- **The branch**, the default against the rest.
- **The event**, the coarsest.

A tenant (a workflow author) wanting to opt a job out is a different case from an operator: an opt-out only ever means fewer retries, so it is safe to take from tenant input, unlike anything that triggers a retry (R9).
The natural channel is the job's `runs-on` labels: a scale set routes a job by its name plus any extra label the set registered, and labels register only when the scale set is created ([migration-from-arc.md](../operations/migration-from-arc.md)).
So an opt-out label works only if the operator put it on the set first, and GAG would read it from `requestLabels`, which the job message carries and the client does not yet decode.
None of these ships until someone asks for it; each then lands as one optional match field on a `spec.retry.rules` entry ([Built to extend](#built-to-extend)), never as a field per axis.

## Run- or job-granular re-run (open question 2)

**Research**, with the maintainer's preference recorded: re-run only the job GAG classified, unless something rules it out (2026-10-12).
GitHub re-runs no finer than a job, through three endpoints:

- `POST …/runs/{id}/rerun` re-runs every job: the UI's "Re-run all jobs".
- `POST …/runs/{id}/rerun-failed-jobs` re-runs every failed job and its dependents: the UI's "Re-run failed jobs", and what GAG calls today.
- `POST …/jobs/{job_id}/rerun` re-runs one job and its dependents: the re-run button on a single job.

Only the third leaves a failed sibling alone.

Run-granular re-runs a sibling's real failure too, and hides it if it passes, which is the masking R1 exists to measure.
Job-granular avoids that, and costs:

- **A jobs-list call per retry** (`GET …/runs/{id}/attempts/{n}/jobs`), to turn the job's display name into the numeric id the endpoint takes; the job message carries only a UUID.
  Retries are rare, so the cost is small.
- **A name match that may not hold** (M6): if `jobDisplayName` and the API's `name` differ for matrix or reusable-workflow jobs, or two jobs share a name, the match is ambiguous.
  An ambiguous match falls back to `rerun-failed-jobs`, counted as `trigger="gag_collateral"`.
- **The external-state case**: a job whose passed upstream jobs set state it depends on (a staging deploy, a lock, a published artifact) can fail again on a single-job re-run where a whole-run re-run would pass.
  GitHub keeps the upstream jobs' outputs for the re-run, from recall; whether it keeps their artifacts is part of M6.
  The fallback for this case is the same `rerun-failed-jobs`, and choosing it needs a signal GAG does not have, so the first version does not try; `same_class_again` stops the repeat.

M6 settles it; until then, detection's `gag_collateral` count measures what run-granular costs on the existing arms.

## Failure modes

| Failure | Effect | Direction |
|---|---|---|
| The ledger's ConfigMap write fails | The pairing is lost; the job counts in nothing | Undercount, never a wrong re-run |
| The AGC restarts | Snapshots, failures awaiting their worker's finish, breakers and the abandoned arm's waiting set are lost; the ledger, its pending class re-runs and the scale-set budget are not | Context reads `unknown`, or stays the running pod's; breakers close and the budget still bounds; an abandoned run waiting for capacity needs a manual re-run, as today |
| The budget reservation write fails | No slot is reserved | The re-run is withheld; never a re-run without a slot |
| GitHub's API is unreachable | No run read, so no re-run | Fail closed, as the deletion arm does today |
| A class is misdeclared | Its re-runs keep failing | `same_class_again` stops each job's repeat, and the alert names the cause |
| A floor's pod cannot be placed | The worker stays Pending past its deadline | The abandoned arm's existing path, which waits for capacity |
| Two AGC pods during a rollout | Both may see one disruption | The reservation is a compare-and-swap on one object keyed by the disruption, so the second resumes the first's reservation rather than taking another |

## Test plan

- **Unit:**
  - each `reason` in the futility table against an `httptest` GitHub returning that run;
  - the reservation's resume, merge, refuse and reserve paths, with a human re-run between two of GAG's;
  - a second job disrupted in an attempt whose re-run was `scope: Job` for another job, asserting it reserves rather than merges;
  - one disruption's retry loop turning several times, asserting one slot;
  - `already_rerun` after a human re-ran an unrelated job, asserting GAG's job is still re-run;
  - a user's cancel of an attempt after one GAG force-cancelled, asserting `run_cancelled`;
  - the out-of-memory floor at, below and above each ceiling, for both profiles, and a second kill on one job;
  - the pairing table, row by row, and a replayed completion;
  - the ledger's worst-case size, a floor surviving the cap, and a full cap withholding a reservation rather than evicting a `reruns` entry;
  - an older decoder round-tripping a ledger with a field and a map it does not know, and ageing that map by `expiresAt`.
- **envtest:**
  - the ledger's compare-and-swap under two writers;
  - a restart in the middle of a run, asserting the re-run is still withheld at the budget, which today's in-memory count would allow;
  - a restart while a class re-run is pending, asserting it still lands, and a disruption arm re-running the run while it waits, asserting no second re-run.
- **Deleting the mechanism:**
  - each futility check's test goes red with the check removed;
  - the merge's test goes red with the merge removed, which also reproduces today's double re-run.
- **Dogfood:** M1 to M6, and each class's R12 reproduction, before its class merges.

## Order of work

1. The dogfood turn-up of 2026-11-22 settled M1 to M4 ([the readings](#readings-from-the-dogfood-turn-up-2026-11-22)), and the API settled M5 ([the readings](#readings-from-githubs-api-2026-11-22)); M6 needs its own test workflow.
2. Q1182 (built) created the ledger store, with its version rules, `reruns` map and reservation, moved the scale-set budget onto it, and records the abandoned arm's force-cancel as `selfCancelledAttempt`.
3. Q1181 (built): `run_cancelled`, `attempt_limit`, `run_too_old` and `superseded` on the widened run read, honouring `selfCancelledAttempt`, between the run read and the reservation in `handleScaleSetEviction`; Q1320 (built) added `already_rerun`, `run_succeeded` and `worker_unstartable` there, and the classic tier takes none.
4. Q555 (built) added the `failures` map, the snapshot, the pairing and the metrics on that store.
   Two departures from Component 1: event and workflow ref are read from the completion rather than carried from the assignment; and the in-memory copy is an index of failure keys, loaded on first use.
5. Q1183 (built): the breaker and the out-of-memory class, on the disruption arms' re-run window; Q1244 (built) moved its re-run onto the `pending` map, Q1245 (built) added the alert, the gauge and the annotation, and job-granular re-run still waits on M6.
6. Q1178 and Q1184 (built).
