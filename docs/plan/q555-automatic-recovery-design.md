# Q555 automatic recovery: design

**Status:** ⓘ proposed design, written 2026-10-10.
Nothing here is built.
It turns [the requirements draft](q555-flaky-job-retry.md) into components against the code as it stands, one per backlog row, and answers the draft's [open questions](q555-flaky-job-retry.md#open-questions-for-the-design-session) with recommendations that need the maintainer's sign-off (marked **Decide**).
The draft says *what* and *why*; this says *how*.
Code facts were read from source on 2026-10-10 unless marked otherwise; four assumptions the design rests on are unmeasured and listed [first](#measure-before-building).

**Goal:** a job that failed for a platform reason GAG can name is re-run without a human, a job that a re-run cannot fix is not, and the rate of re-runs a human still has to make is measured per set.

**Approach:** detection is the substrate.
A per-set failure ledger joins each failed job's platform context to its later attempts, which gives the flake count, the manual re-run rate and each retry cause's success rate.
Retry classes then fire off that context through the re-run path the disruption arms already use, after one run read that both checks futility and enforces a budget that survives a restart.

## Measure before building

Each of these decides a component's shape, and none has been observed:

| # | Unknown | Decides | How to settle |
|---|---|---|---|
| M1 | The `result` vocabulary for a red step, a cancelled run and a timed-out job | Which completions the ledger records | The dogfood turn-up [Q555](../queue/Q555.md) already names |
| M2 | Whether a re-run job's `JobAssigned` carries a new job UUID and the same `jobDisplayName` | Pairing's key, and whether the listener's per-UUID completion dedup would drop the re-run's completion | Same turn-up: re-run the failed job and read both assignments at debug level |
| M3 | Whether a container killed at its memory limit takes the runner with it (the whole container is killed, so the pod fails and GitHub concludes the job only when the job lock lapses, about 10 minutes later) or only a step's process (the runner survives and reports a failed step) | Whether the OOM class fires from the pod watch, like the disruption arms, or from `JobCompleted` | A dogfood job that allocates past its limit under `Binpack` |
| M4 | Whether a finished worker's pod object still exists when its `JobCompleted` arrives | Where platform context is read | The same turn-up: compare the pod's deletion time to the completion's arrival in the AGC log |

M3 rests on recall that kubelets on cgroup v2 kill a container's whole process group on an out-of-memory event; it is not checked against a cluster.

## Component 1: the failure ledger (Q555, R1 to R8)

### Where the hook goes

The listener's `completeJob` already receives every `JobCompleted`, deduplicated per job, and calls `Config.Cleanup(ctx, jobID, runnerName)`, which carries neither the result nor the run identity.
Add a `Config.Completed func(ctx, Job, result string)` beside the existing `Config.Started`, called from `completeJob` after the cleanup, and wire it in `runnerset_scaleset.go` where `Started` and `Cleanup` are wired.
There is one listener per `RunnerSet` and one AGC replica per gateway, so per-set state has one writer in steady state; a rolling update can briefly run two AGC pods, and the code already allows for that (the recovery claims' compare-and-swap), so the ledger does too.

### What a failed job records (R7)

The pod is the evidence, and nothing maps a job to its pod or node once the pod is deleted.
So the runner-set controller, whose pod watch already sees every worker reach a terminal phase (it drives `RecoverDisruptedScaleSetWorker`), snapshots each worker at that transition into a bounded in-memory map keyed by runner name.
`Completed` takes the snapshot for its runner, falling back to reading the pod if it is still there (M4).

A snapshot holds only bounded fields:

- pod phase, reason and message (message truncated to 256 bytes), pod IP, `spec.nodeName`;
- per container, sidecars included: name, terminated reason, exit code, restart count;
- the node's `Ready` condition and any pressure condition that is true, read once at snapshot time, or `NodeGone`;
- the set's EgressProxy `Ready` condition and `readyReplicas` (the AGC already watches EgressProxies);
- the run identity and job display name from the pod's annotations.

A restart loses snapshots for workers that finished before it, and their records say `context: unknown`.
That is one of R5's stated blind spots.

### Where the ledger lives (open question 1)

**Decide:** a per-set ConfigMap, `scaleset-failures-<runnerset>`, key `failures.json`, owner-referenced to the `RunnerSet`, written with the same read, modify, compare-and-swap loop as `scaleset-recovery-claims` (`recovery_claims.go`).

- Memory alone loses every pairing on a restart, and an AGC rollout restarts it, so the manual re-run rate would undercount on every upgrade.
- Asking GitHub is stateless but needs a run read per completed job to learn whether it is a re-run, which R6 forbids, because `JobAssigned` carries no attempt number.
- `guards.json` is not the home: it is re-serialised whole by the listener and retired when the assignment message is deleted, while a failure must outlive that by days.

Writes happen on a failed completion, a GAG re-run and a pairing, never on a succeeded job with no record, so the rate tracks failures and not throughput.

An entry, keyed `<runID>/<jobDisplayName>`:

```json
{
  "failedAt": "2026-10-10T12:00:00Z",
  "jobID": "6f1c…",
  "result": "failed",
  "context": { "podPhase": "Failed", "containers": [{ "name": "runner", "reason": "OOMKilled", "exitCode": 137 }] },
  "rerun": { "by": "gag", "cause": "oom", "at": "2026-10-10T12:11:00Z" }
}
```

Bounds: entries expire after 7 days (a human re-runs Monday what failed Friday), and the oldest go first past a cap sized so the worst-case object stays under 512 KiB, which a unit test asserts against a maximal entry.

### Pairing and attribution (R1, R2, R8)

On a completion for a key that has an entry, the completion is a later attempt of a job that failed, which is the only way a job name repeats inside one run:

| Entry's `rerun.by` | Entry's context | New result | Counts as |
|---|---|---|---|
| `gag` with a disruption cause | any | `succeeded` | recovered by that cause (R14), not flaky (R2) |
| `gag` with a classified cause | any | `succeeded` | that class's success (R14) |
| `gag` (the run's other failed jobs, re-run alongside) | no platform fault | `succeeded` | flaky, triggered by GAG as collateral |
| empty | any | `succeeded` | flaky, re-run by somebody else (R8) |
| any | any | `failed` | the entry is updated, keeping the first failure's context |

Attribution reads the entry when the later attempt completes, not when the failure arrives, so the order of the disruption arm and the failed `JobCompleted` does not matter: an evicted job's failure arrives about 10 minutes after the eviction (the job lock), a deleted one's 15 to 26 seconds after.
A succeeded pairing deletes the entry.

The disruption arms mark the entry when they fire: they know the job display name from the pod's `job-name` annotation, so the mark is per job, not per run.
That matters for the third row: `rerun-failed-jobs` re-runs every failed job in the run, and a sibling's real test failure must not read as recovered.

### What the operator sees (R3, R4, R8)

Detection is on for every scale-set `RunnerSet` with no field (R4); it changes no behaviour.

| Metric | Labels | Meaning |
|---|---|---|
| `actions_gateway_scaleset_job_reruns_total` | `namespace`, `runner_set`, `trigger` (`gag`, `gag_collateral`, `other`) | A later attempt of a failed job completed. `trigger="other"` over `scaleset_jobs_completed_total` is the manual re-run rate, the success metric |
| `actions_gateway_scaleset_flaky_jobs_total` | `namespace`, `runner_set`, `trigger` | A failed job later succeeded with no disruption or class attributed |
| `actions_gateway_eviction_rerun_outcomes_total` | `namespace`, `runner_group`, `tier`, `cause`, `result` | The result of each attempt GAG re-ran, per cause (R14); `result="failed"` over the total is a cause's failure rate |

The third keeps the `eviction_` prefix and `runner_group` label of the family it extends, so one query joins it to `eviction_retries_total`.
No job or repository name is a label (R3); they go in an info-level log line per failed job, carrying the snapshot, which is the record that outlives both the pod and the ledger's 7 days in whatever log store the operator keeps.
A Warning Event, `PlatformFaultOnFailedJob`, fires only when the snapshot shows a fault (a non-zero or `OOMKilled` container, a node not ready or gone, an unready proxy), not on every red test.

## Component 2: one run read before every re-run (Q1182 budget, Q1181 futility)

### What the read decodes

`runConclusion` (`eviction.go`) decodes `status` and `conclusion` from `GET /repos/{o}/{r}/actions/runs/{id}`, and only the deletion arm calls it.
Widen the decode to `run_attempt`, `created_at`, `event`, `head_branch`, `head_sha` and `workflow_id`, and call it on every arm before every attempt in `rerunUntilAccepted`.
It keeps the existing failure handling: a transport error or a 5xx retries inside the window and ends as `conclusion_unknown`, a 4xx ends at once.
That is a new failure mode for the four arms that do not read the run today, and it fails in the safe direction: a run GAG cannot read is not re-run, and the re-run call would have needed the same API anyway.
`actions: read` covers the read, so R17 holds.

### The budget becomes `run_attempt` (R15)

Today the budget is an in-memory count per run id, reset by a restart and not shared between replicas.
**Decide:** replace the count with `run_attempt`: a run may be re-run by GAG while `run_attempt <= maxEvictionRetries`, which is the number of re-runs it has had, plus one.
It survives a restart, needs no write, and is the same number across replicas and both tiers.

The trade-off is that it counts a human's re-runs too: a run somebody re-ran twice gets no automatic re-run under the default budget of 2.
That fails safe (fewer automatic re-runs, never more), and a run already re-run twice by hand is one a person is watching.
`maxEvictionRetries` keeps its name, range and default; its godoc changes from "re-runs GAG has made" to "attempts the run has had", which [api-review.md](../development/api-review.md) treats as a meaning change, so the field's doc, the operator docs and a release note say so.

The in-memory map stays, re-keyed to `(run id, run_attempt)`, as a dedup: two jobs of one attempt disrupted together make one re-run, not two.
Reading the code suggests that today two disruptions in one run each reserve a slot and each loop until accepted, so the second can land after the first re-run's attempt concludes and re-run it again; that is unverified, and the dedup prevents it either way.

### The futility checks (R18), in order

Each check runs on the same read, before the budget is touched, and a hit is counted on the existing `actions_gateway_eviction_rerun_withheld_total{tier, cause, reason}`, whose `reason` today is only `run_cancelled` and whose `cause` is always `deletion`.
The `EvictionRerunWithheld` Event names the reason and the fix.

| `reason` | Condition | Fix the Event names |
|---|---|---|
| `already_rerun` | `run_attempt` has moved past the attempt first read for this disruption | None: somebody re-ran it |
| `run_cancelled` | `conclusion: cancelled`, on every arm except `abandoned`, whose own force-cancel produced it | None, as Q811 |
| `run_succeeded` | `conclusion: success`; GitHub refuses with a 403 | None |
| `attempt_limit` | `run_attempt >= 50`, GitHub's cap | None |
| `run_too_old` | `created_at` older than 30 days | None |
| `superseded` | A newer run of the same `workflow_id` on the same `head_branch` with a different `head_sha` exists (`GET …/workflows/{id}/runs?branch=…&per_page=1`), for `push` and `pull_request` events only | None: a newer commit is building |
| `budget_exhausted` | The budget above | As today's `EvictionRetriesExhausted` |
| `worker_unstartable` | The set's capacity gate holds a startup or placeability verdict (Q714) that every next pod fails | The verdict's own remediation |
| `own_limit` | Class-specific: the job hit its own limit (see [Component 3](#the-first-class-out-of-memory-under-right-sizing)) | Raise the limit, or enable a sizing profile |

`budget_exhausted` keeps its own counter, `eviction_retries_exhausted_total`, rather than moving under `withheld`, so existing alerts keep working.
The `superseded` check is the only extra call, and only on the two events where a branch can move; a merge-group run that loses its place is cancelled by GitHub and caught by `run_cancelled`.
Storage-limit eviction ([Q1177](../queue/Q1177.md)) becomes an `own_limit` hit on the eviction arm once its measurement settles the discriminator.

## Component 3: retry classes (Q1183, R9 to R17)

### Where a class fires

A class is a function over a failed worker's snapshot that returns a cause or nothing, plus a declaration of how its cause clears (R11):

```go
type recoveryClass struct {
	cause    string                 // the counters' cause label
	recognise func(snapshot) bool   // R9: facts GAG owns only
	clears   clearance              // freshPod, observable, or unobservable
	ready    func(ctx, owner) (bool, error) // for observable: has the cause cleared?
}
```

It runs where M3 says the evidence is first complete: on the worker's terminal transition, beside the disruption arms, if the class's failure kills the runner; on `Completed` if the runner survives to report.
Either way it reads the same snapshot detection records, which is why Q1183 waits on Q555.

### How it waits

Every class goes through one deferred path, generalised from the abandoned arm's (Q691, `abandoned_rerun.go`):

- **The pending entry** gains a release predicate in place of the hard-coded `capacityReturnedSince`.
  The abandoned arm becomes its first user, unchanged in behaviour.
- **Release needs the run concluded.** A failed job's siblings can run for an hour, and `rerunUntilAccepted`'s 15-minute window would expire into `run_never_concluded`.
  The sweeper reads the run on each tick, every 30 seconds as now, backing off to 2 minutes after the first 10.
  Each pending entry then expires at the run's own longest possible life, which is the job timeout, not at a fixed 30 minutes.
- **Release then needs the cause cleared:**
  - `freshPod` releases at once;
  - `observable` releases when `ready` is true;
  - `unobservable` releases after a backoff of 1, 4, then 16 minutes by attempt, each with ±25% jitter.
- **Then it re-runs.** Release calls `handleEviction` with no delay, which runs [Component 2](#component-2-one-run-read-before-every-re-run-q1182-budget-q1181-futility)'s read, checks and budget, and `rerunUntilAccepted`.

### The breaker (R11)

One breaker per `(set, cause)`, in memory.
It opens on either of:

- **A stampede.** The cause fired for more than 10 distinct runs in 10 minutes, which is a correlated outage, not a set of independent failures.
- **A misclassification.** Of the cause's last 10 re-runs, fewer than 5 succeeded (R14's pairing).

While open, a fire is withheld with `reason="breaker_open"`, and the set carries a new condition, `AutoRetryPaused`, `True`, whose reason names the cause.
It half-opens after 30 minutes and lets one re-run through.
The thresholds are controller constants, not API: the defaults will move once R14 has data, and api-review puts defaults that may change in the controller.
A restart closes every breaker; the per-run budget still bounds what that can cost.

### The first class: out of memory under right-sizing

Recognised when a worker container terminated `OOMKilled` and the set's `spec.sizing.profile` is `Binpack` or `Throughput` with `sizingProfileState: Active`.
Under `Static`, or `AwaitingSamples`, the same snapshot is withheld as `own_limit`: the limit is the template's and the next pod gets the same one.

**It clears by observation, and GAG computes the observation.** The aggregate cannot be counted on to move, because the sampler records an out-of-memory job's peak at or below the limit that killed it.
So the class sets the next attempt's limit directly from the killed pod's:

```
next = roundUp64Mi(killedLimit × headroom)
headroom = 1.4 (Binpack's memLimitHeadroom) | limitHeadroomPercent / 100 (Throughput, default 1.5)
next = min(next, spec.sizing.maxRequests.memory)   // when set
```

If `next <= killedLimit` (a ceiling reached, or `limitHeadroomPercent: 100`), the fire is withheld as `own_limit`.
Under `Binpack`, requests equal limits, so the request rises with it; under `Throughput`, only the limit.
The budget bounds the growth: the default 2 re-runs cap it at 1.4² ≈ 1.96× under `Binpack`.
`Throughput`'s limit is not clamped by `maxRequests` today, and applying the clamp here only to the bump is deliberate: a retry must not buy memory a profile would refuse.

**The override needs a per-job hook, which does not exist.** `Target.Resolve(ctx)` takes no job, and `buildPod` uses the job only for annotations.
Add a floor lookup after `Resolve` in `ProvisionScaleSetWorker`, keyed by the `ScaleSetJob`'s run id and job name, which `JobAssigned` already carries.
The floor is stored on the job's ledger entry, so it survives a restart and lands on the re-run however long the run takes to conclude.
It is consumed by the pairing that deletes the entry.

**It masks one thing, so it says so.** A pull request that doubles a job's memory gets the memory on retry.
Each bump emits a Normal Event, `MemoryLimitRaisedForRetry`, naming the job, both limits and the run, and is counted under `cause="oom"`, so a rising rate is a visible regression and `SizingDrift` still reports a template below observed peaks.

### Classes after the first

Each needs R12's dogfood reproduction first.
In the order the CI history and existing machinery make cheapest:

1. **Node lost under a running worker** (`freshPod`), if [Q1179](../queue/Q1179.md)'s measurement shows no arm fires: recognised by a `DisruptionTarget` condition of `TerminationByKubelet` or a node `NodeGone` in the snapshot.
2. **Egress proxy pool unavailable** (`observable`): the snapshot's proxy `Ready` was `False` or `readyReplicas` was 0 while the job ran, and `ready` is the EgressProxy's `Ready` condition.
   This needs nothing from [Q1178](../queue/Q1178.md): the AGC already watches the EgressProxy, and its RBAC allows it.
3. **`dockerd` sidecar crash** (`freshPod`): the Docker-in-Docker sidecar's restart count rose, with a discriminator against tenant disk use still to find.

**Decide (open question 3):** measure in that order, and ship only the out-of-memory class under Q1183; each later class is a row of its own once reproduced.

## Component 4: the proxy as a sensor (Q1178, Q1184)

### Outcome metrics (Q1178)

`checkDestination` returns `(dialAddr, allowed)` and has three allow paths, so which allowlist entry matched is computed and thrown away.
Change it to return the matched entry, as written in the allowlist (a host suffix or a CIDR), and an outcome.
Two defects it fixes on the way:

- **A failed lookup on the CIDR path reads as a policy denial.** A DNS outage then looks like "destination not allowed", which is exactly the misreading that would make a futility check withhold a retry it should make.
  It becomes `outcome="dns_error"`.
- **Every dial failure is one unlabelled counter**, though `errors.As` can tell the kinds apart.

| Metric | Labels |
|---|---|
| `actions_gateway_proxy_connects_total` | `destination` (the matched entry; `unlisted` for a denial; `any` with no allowlist), `outcome` (`tunneled`, `denied`, `dns_error`, `refused`, `timeout`, `unreachable`, `error`) |

Cardinality is the allowlist's length × 7.
Whether admission caps the allowlist's length is unchecked; if it does not, the label needs a cap, with entries past it collapsing to `other`.
The two unlabelled counters stay, as sums of the new one, until a release note retires them.

### Why the AGC does not read the proxy yet

Reading a replica's `/metrics` from the AGC needs three changes, none of them small:

- **A network path.** The generated NetworkPolicies admit port 8443 only from namespaces labelled `metrics=enabled`, and the AGC's own policy allows no egress to it.
- **A credential.** The port demands a client certificate, and the `<ep>-metrics-client` Secret is published for the monitoring stack and mounted nowhere.
- **Discovery.** The AGC can list same-namespace pods by label, but it has no EndpointSlice RBAC, and a shared pool lives in another namespace that its namespace-scoped cache cannot see.

The out-of-memory class and the proxy-unavailable class need none of it.
So Q1178 ships metrics for operators only, and the per-destination breaker the draft sketched waits for Q1184, where the new path and credential get their security review together.

### Per-job attribution (Q1184), the shape to review

- **The proxy** keeps a bounded ring of recent failures (denied, DNS, dial): time, client IP, the matched entry or, for a denial, the host, and the outcome.
  It is in memory only, with no record of successful connections, so it adds no retention of where jobs went.
- **The AGC**, on a failed job whose snapshot carries a pod IP, asks each ready replica for failures from that IP inside the job's start-to-end window, and adds them to the ledger entry.
  The window guards against pod IP reuse, so the pod IP and start time join the snapshot.
- **The futile and retry shapes it unlocks** are the draft's proxy rows: a denial is futile, and an upstream that failed and then recovered is a class that clears by observation.

The [security review](../design/05-security.md) owns whether a denied host may be held at all, and for how long.

## The CRD (open question 5)

**Decide:** no new field.

- Detection changes no behaviour, so it needs no switch (R4).
- Classes spend the existing budget and inherit `maxEvictionRetries: 0` as the off switch.
  That follows what ships: the disruption arms are on by default, so a platform cause is retried by default.
- The out-of-memory class fires only on a set whose owner chose a right-sizing profile, so the opt-in is already on the object.
- A per-class switch waits for its consumer: the first class some operator wants off.
  Then it is a `spec.retry.disabledCauses` list, whose emptiness is the default, rather than one boolean per class, and api-review's rules settle its shape then.

The one status addition is the `AutoRetryPaused` condition type, in `api/apiconditions`.

## Event scope (open question 4)

**Decide:** classes inherit the arms' scope, every event.
A deploy job killed by the platform is one a human would re-run, and a class only fires on a cause outside the job.
An event filter is a field whose consumer has not arrived.

## Run- or job-granular re-run (open question 2)

**Decide:** keep `rerun-failed-jobs` for everything.
Job-granular re-run needs the job's numeric id, which costs a jobs-list call per retry, and whether a `JobCompleted` display name matches the API's job name exactly for reusable workflows is unmeasured.
The cost of run-granular is collateral: a sibling's real failure is re-run too, and masked if it then passes.
Detection counts exactly that as `trigger="gag_collateral"`, so the decision comes back with a number if the collateral share turns out to matter.

## Failure modes

| Failure | Effect | Direction |
|---|---|---|
| The ledger's ConfigMap write fails | The pairing is lost; the job counts in nothing | Undercount, never a wrong re-run |
| The AGC restarts | Snapshots and breakers are lost; the ledger and budget are not | Context reads `unknown`; breakers close, the budget still bounds |
| GitHub's API is unreachable | No run read, so no re-run | Fail closed, as the deletion arm does today |
| A class is misdeclared | Its re-runs keep failing | The breaker opens on the success rate and says so |
| A floor's pod cannot be placed | The worker stays Pending past its deadline | The abandoned arm's existing path, which waits for capacity |
| Two AGC pods during a rollout | Both may see one disruption | The `(run, attempt)` dedup is per process, so the second read sees `already_rerun` |

## Test plan

- **Unit:** each `reason` in the futility table against an `httptest` GitHub returning that run.
  The budget at `run_attempt` 1, 2 and 3 with the default of 2.
  The out-of-memory floor at, below and above a ceiling, for both profiles.
  The pairing table, row by row.
  The ledger's worst-case size.
- **envtest:** the ledger's compare-and-swap under two writers.
  A restart in the middle of a run, asserting the re-run is still withheld at the budget, which today's in-memory count would allow.
- **Deleting the mechanism:** each futility check's test goes red with the check removed; the dedup's test goes red with the key back to run id alone.
- **Dogfood:** M1 to M4, and each class's R12 reproduction, before its class merges.

## Order of work

1. The dogfood turn-up settles M1, M2 and M4; M3 waits for the out-of-memory reproduction.
2. Q1182, then Q1181: both widen one read, and the budget change is the smaller diff to review alone.
3. Q555's ledger and metrics.
4. Q1183: the generalised deferred path, the breaker and the out-of-memory class.
5. Q1178 any time; Q1184 after its review.
