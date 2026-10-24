# Q555 automatic recovery: design

**Status:** ⓘ proposed design, written 2026-10-10, revised 2026-10-11 after an independent review, and revised again 2026-10-12 with the maintainer's decisions.
Nothing here is built.
It turns [the requirements draft](q555-flaky-job-retry.md) into components against the code as it stands, one per backlog row, and answers the draft's [open questions](q555-flaky-job-retry.md#open-questions-for-the-design-session).
Each answer is marked **Decided** (the maintainer chose it, 2026-10-12), **Decide** (a recommendation still awaiting sign-off) or **Research** (needs a measurement before anyone can choose).
The draft says *what* and *why*; this says *how*.
Code facts were read from source on 2026-10-10 and 2026-10-11 unless marked otherwise; the assumptions the design rests on that nobody has measured are listed [first](#measure-before-building).

**Goal:** a job that failed for a platform reason GAG can name is re-run without a human, a job that a re-run cannot fix is not, and the rate of re-runs a human still has to make is measured per set.

**Approach:** detection is the substrate.
A per-set failure ledger joins each failed job's platform context to its later attempts, which gives the flake count, the manual re-run rate and each retry cause's success rate.
Retry classes then fire off that context through the re-run path the disruption arms already use, after one run read that both checks futility and enforces a budget that survives a restart.

## Measure before building

Each of these decides a component's shape, and none has been observed:

| # | Unknown | Decides | How to settle |
|---|---|---|---|
| M1 | The `result` vocabulary for a red step, a cancelled run and a timed-out job | Which completions the ledger records | The dogfood turn-up [Q555](../queue/Q555.md) already names |
| M2 | Whether a re-run job's `JobAssigned` carries a new job UUID and the same `jobDisplayName`, and whether two jobs in one run can share a display name (GitHub allows a duplicate `name:`, from recall) | Pairing's key | Same turn-up: re-run the failed job and read both assignments at debug level; a workflow with two jobs named alike |
| M3 | Whether a container killed at its memory limit takes the runner with it (the pod fails, and GitHub concludes the job only when the job lock lapses, about 10 minutes later) or only a step's process (the runner survives and reports a failed step) | Whether the out-of-memory class fires from the pod watch, like the disruption arms, or from `JobCompleted` | A dogfood job that allocates past its limit under `Binpack` |
| M4 | Whether a finished worker's pod object still exists when its `JobCompleted` arrives | Where platform context is read | The same turn-up: compare the pod's deletion time to the completion's arrival in the AGC log |
| M5 | The `superseded` check's answer for a pull request from a fork whose branch is `main` | Whether the check needs more than the filters below | A fork PR on a test repository |
| M6 | Whether `jobDisplayName` matches the jobs API's `name` exactly, for matrix jobs and reusable workflows, and what a single-job re-run does to the jobs it depends on and that depend on it | Whether a classified retry can re-run only its own job ([granularity](#run--or-job-granular-re-run-open-question-2)) | A test workflow with a matrix, a reusable workflow and a `needs:` chain, re-run one job at a time |

M3 rests on recall that kubelets on cgroup v2 kill a container's whole process group on an out-of-memory event.
The breaker thresholds, the 7-day ledger expiry and the deferred path's 24-hour expiry are proposals, not measurements; R14's metrics are what will move them.

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
`Completed` takes the snapshot for its runner, falling back to reading the pod if it is still there (M4).

A snapshot holds only bounded fields, all read from objects the AGC can already see:

- pod phase, reason and message (message truncated to 256 bytes), its conditions (`DisruptionTarget` included), `spec.nodeName`, pod IP and start time;
- per container, sidecars included: name, terminated reason, exit code, restart count;
- for a colocated egress proxy, the EgressProxy's `Ready` condition and `readyReplicas`.

**Two things R7 lists are left out, deliberately:**

- **The node's conditions.** The AGC has no `nodes` RBAC and a namespace-scoped cache, and granting a cluster-scoped read to every tenant's AGC is the widening [05-security.md](../design/05-security.md) rules out.
  The pod's own `DisruptionTarget` condition and phase carry what a node-lost class needs.
- **Registry mirror readiness.** It joins the snapshot with the mirror candidate's reproduction, once that settles which object reports it.

A **shared** proxy pool lives in another namespace, which the AGC's cache cannot see, so its readiness is absent too: the snapshot says `proxy: shared`.

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
Writes happen on a failed completion, a GAG re-run (its budget reservation) and a pairing, never on a succeeded job with no record, so the write rate tracks failures and not throughput.

It holds a version and three maps, every entry carrying its own `expiresAt` so any AGC version can age any map, including one it does not understand:

```json
{
  "version": 1,
  "failures": {
    "<runID>/<jobDisplayName>": {
      "expiresAt": "2026-10-17T12:00:00Z",
      "failedAt": "2026-10-10T12:00:00Z",
      "jobID": "6f1c…",
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
Whether `JobCompleted` carries them too is inferred from upstream's struct and unmeasured, so the listener carries them from the assignment, as it already carries the run identity, rather than relying on the completion; they cost no API call either way.

**Bounds.**

- **Expiry:** failure and pending entries after 7 days and 24 hours respectively; `reruns` entries after 30 days, GitHub's re-run window from recall, so a budget cannot refill while the run can still be re-run.
- **The cap evicts failures, never budgets.** Past a cap sized so the object stays under 512 KiB, the oldest failure entries go first, except one holding a `memoryFloor` whose re-run has not completed.
  A `reruns` or `pending` entry is never evicted to make room: if those alone reach the cap, new reservations fail and their re-runs are withheld, because evicting one would refill its run's budget.
- **The size, estimated rather than measured:** a `reruns` entry is about 300 bytes with two attempts recorded, so the 512 KiB cap holds about 1,700 runs that GAG re-ran inside 30 days on one set before failures are crowded out entirely.
  A unit test asserts the worst case against maximal entries in every map.

### Pairing and attribution (R1, R2, R8)

On a completion for a key with a failure entry, the completion is a later attempt of that job (M2):

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
  A log store that keeps only warnings, as dogfood's does ([Q1180](../queue/Q1180.md)), still keeps every platform fault.

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
| `attempt_limit` | `run_attempt >= 50`, GitHub's cap | None |
| `run_too_old` | `created_at` older than 30 days | None |
| `superseded` | A newer run of the same `workflow_id`, `event` and `head_branch` from the same `head_repository`, with a different `head_sha`, exists (`GET …/workflows/{id}/runs?branch=…&event=…&per_page=1`), for `push` and `pull_request` only | None: a newer commit is building |
| `same_class_again` | Class only: this job's failure entry already records a re-run by the same cause that failed again | The class does not fit this job; investigate |
| `budget_exhausted` | The budget above | As today's `EvictionRetriesExhausted` |
| `worker_unstartable` | The set's capacity gate holds a startup or placeability verdict (Q714) that every next pod fails | The verdict's own remediation |
| `own_limit` | Class only: the job hit its own limit (see [Component 3](#the-first-class-out-of-memory-under-right-sizing)) | Raise the limit, or enable a sizing profile |

Notes on the table:

- **`selfCancelledAttempt` replaces an arm exemption.** The abandoned arm records the attempt it force-cancels before cancelling, so the check holds on whichever arm a merge lets through, and a user's cancel of a later attempt is never mistaken for GAG's.
  Until Q1182 creates the ledger, Q1181 keeps the arm exemption, which is sound while there is no merge.
  The abandoned arm also reads the conclusion **before** force-cancelling: a run a user or a concurrency group already cancelled is withheld, not cancelled again and re-run.
- **`already_rerun` asks about the job, not the run.** A single-job re-run moves `run_attempt` (from recall) without touching the job's siblings, so "the attempt moved" would withhold a job that nobody re-ran.
  The jobs-list call it needs happens only when the attempt has moved, which is rare.
- **`superseded` is the other extra call**, and only on the two events where a branch can move.
  Filtering on `event` and comparing `head_repository` keeps a fork's `main` from matching upstream's (M5).
  A merge-group run that loses its place is assumed cancelled by GitHub and so caught by `run_cancelled`; that is unmeasured.
- **`worker_unstartable` is new wiring**: no re-run path consults `worker_startup_verdict.go` today.
  It matters most on the abandoned arm, which by design releases on any pod of the set being bound to a node (Q512's test for capacity), including one stuck pulling an image that every next pod will fail to pull too.
- **`budget_exhausted` keeps its own counter**, `eviction_retries_exhausted_total`, so existing alerts keep working.
- Storage-limit eviction ([Q1177](../queue/Q1177.md)) becomes an `own_limit` hit on the eviction arm once its measurement settles the discriminator.

**Two of the draft's futile shapes get no check, because GAG cannot see them:**

- **The platform's own image exits 127 in a setup step.** That is a step's exit code, which only the job log carries, and R9 rules out classifying from logs.
  The startup verdict covers the image failures that show on the pod.
- **The account cannot run jobs.** GitHub refuses before any job is assigned, so no GAG re-run path ever starts.

## Component 3: retry classes (Q1183, R9 to R17)

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
Either way it reads the same snapshot detection records, which is why Q1183 waits on Q555.

### How it waits

Classes go through one deferred path, built beside the abandoned arm's (Q691, `abandoned_rerun.go`) but persistent:

- **Pending re-runs live in the ledger**, in its `pending` map, keyed by run and job.
  The abandoned arm's waiting set is in process memory, so a restart drops it, which costs that arm at most 30 minutes of waiting; a class can wait hours for a run to conclude, so its pending entries must survive.
- **A pending entry records the attempt the job failed in**, read from the run when the class fires, as `failedAttempt`.
  Release judges `already_rerun` and the merge against that attempt, not against a fresh read: if a disruption arm has already re-run the run in the meantime, a fresh read would take the new attempt as the baseline and re-run it again.
  The abandoned arm keeps its own path unchanged.
- **Release needs the run concluded.** A failed job's siblings can run for an hour, and `rerunUntilAccepted`'s 15-minute window would expire into `run_never_concluded`.
  The sweeper reads the run every 30 seconds for the first 5 minutes, every 2 minutes to the first hour, then every 10 minutes.
  An entry expires after 24 hours, with `reason="run_never_concluded"`: GitHub lets a run go on far longer, but by then a person has seen the failure, and the cap bounds the polling at about 175 reads per entry.
- **Release then needs the cause cleared:**
  - `freshPod` releases at once;
  - `observable` releases when `ready` is true;
  - `unobservable` releases after a backoff of 1, 4, then 16 minutes by attempt, each with ±25% jitter.
- **Then it re-runs.** Release calls `handleEviction` with no delay, which runs [Component 2](#component-2-one-run-read-before-every-re-run-q1182-budget-q1181-futility)'s read, checks and budget, and `rerunUntilAccepted`.
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
`Throughput`'s limit is not clamped by `maxRequests` today; clamping the bump is deliberate, because a retry must not buy memory a profile would refuse.

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
   The AGC already watches EgressProxies in its own namespace, so this needs nothing from [Q1178](../queue/Q1178.md).
   A shared pool in another namespace is invisible to it, so the class does not cover one.
3. **`dockerd` sidecar crash** (`freshPod`): the Docker-in-Docker sidecar's restart count rose, with a discriminator against tenant disk use still to find.

**Decide (open question 3):** measure in that order, and ship only the out-of-memory class under Q1183; each later class is a row of its own once reproduced.

## Component 4: the proxy as a sensor (Q1178, Q1184)

### Outcome metrics (Q1178)

`checkDestination` returns `(dialAddr, allowed)`, and its helpers `matchesHostSuffix` and `ipInAny` return only a boolean, so which of its three allow paths and which entry admitted a connection is never computed.
Make the helpers return the matching entry, as written in the allowlist (a host suffix or a CIDR), and `checkDestination` return it with an outcome.
Two defects it fixes on the way:

- **A failed lookup on the CIDR path reads as a policy denial.** A DNS outage then looks like "destination not allowed", which is exactly the misreading that would make a futility check withhold a retry it should make.
  It becomes `outcome="dns_error"`.
- **Every dial failure is one unlabelled counter**, though `errors.As` can tell the kinds apart.

| Metric | Labels |
|---|---|
| `actions_gateway_proxy_connects_total` | `destination` (the matched entry; `unlisted` for a denial; `any` with no allowlist), `outcome` (`tunneled`, `denied`, `dns_error`, `refused`, `timeout`, `unreachable`, `error`) |

Admission caps the allowlist at 64 host suffixes and 64 CIDRs (`egressproxy_types.go`), so `destination` takes at most 130 values and the metric at most 910 series per pool.
The two unlabelled counters stay, as sums of the new one, until a release note retires them.

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

**Decide:** the ledger's compatibility rules below.
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
| The AGC restarts | Snapshots, breakers and the abandoned arm's waiting set are lost; the ledger, its pending class re-runs and the scale-set budget are not | Context reads `unknown`; breakers close and the budget still bounds; an abandoned run waiting for capacity needs a manual re-run, as today |
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

1. The dogfood turn-up settles M1, M2 and M4; M3 waits for the out-of-memory reproduction, and M6 needs its own test workflow.
2. Q1181, which needs no ledger: the futility checks on the widened run read, keeping the abandoned arm's exemption for `run_cancelled`.
3. Q1182 creates the ledger store, with its version rules, `reruns` map and reservation, moves the budget onto it, and swaps Q1181's exemption for `selfCancelledAttempt`.
4. Q555 adds the `failures` map, the snapshot, the pairing and the metrics on that store.
5. Q1183: the `pending` map, the breaker and the out-of-memory class, with job-granular re-run if M6 allows it.
6. Q1178 any time; Q1184 after its review.
