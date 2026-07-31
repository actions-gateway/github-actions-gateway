# Eviction and Oversubscription Validation

Turn the eviction-recovery and oversubscription capabilities from arguments
into measurements. Five experiments, sequenced by what blocks what.

Filed 2026-07-26 from
[issue #819](https://github.com/actions-gateway/github-actions-gateway/issues/819),
which proposed the five and asked for triage. This plan is the reviewed
version: two of the five changed shape under review, and the corrections are
recorded below with the evidence that produced them.

## Why this matters

Both capabilities are currently argued rather than measured. The recovery story
is design prose in [01-executive-summary.md](../design/01-executive-summary.md)
and the flows doc; the oversubscription story rests on `priorityTiers` doing
what the design says it does. The planned blog post
([go-to-market.md](go-to-market.md), "Recovering stuck Actions jobs after pod
eviction") can state the design but cannot state a result.

The one number available is confounded. Per
[scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#the-baseline-number-is-confounded),
the U5 probe's ~9.5 minutes coincided with the job's 10-minute
`timeout-minutes` boundary, leaving lock TTL, GitHub's liveness detection, and
the workflow timeout indistinguishable. Any published latency figure needs a
clean run with no `timeout-minutes` set.

## What is already tracked

| Row | Relationship to these experiments |
|---|---|
| [Q396](../STATUS.md#Q396) | **Is** experiment 1. Already covers both tiers as of #815; only the retry-budget assertion is additive. |
| Q417 | **Shipped 2026-07-26.** Was the hard prerequisite for the scale-set half of 1, and for 3 and 5: `ProvisionScaleSetWorker` is fire-and-forget, so scale-set evictions were never detected. Detection now runs from the owning reconciler off the worker pod ([scaleset-eviction-recovery.md § Phase 2 as built](scaleset-eviction-recovery.md#phase-2-as-built)). All three are unblocked. |
| Q419 | **Shipped 2026-07-26** with Q417 — the docs half of the same gap. The tier-agnostic claims in the exec summary, README, and why-gag are now true of both tiers rather than needing a qualification. Independent of these experiments. |
| Q420 | **Shipped 2026-07-26**, ahead of Q417 and independently of it — the reap deadline came from a pod annotation, not a pod watch. Orphaned Running workers would otherwise have contaminated 3 and 5 by holding quota, which is exactly the idle-capacity signature those experiments measure. |
| [Q418](../STATUS.md#Q418) | Deferred, event-gated on experiment 1 attributing the delay. |
| [Q459](q459-drained-worker-recovery.md) | **Filed by experiment 2**, 2026-07-27. Its residual: neither tier recovers a drained worker, and whether that matters turns on what GitHub does with the runner's own relayed report — a live-GitHub question. Both halves measured 2026-07-29; decided **close, gated on `deletionTimestamp`**, and the Queue row is retired in favour of [Q502](../STATUS.md#Q502). |

## Experiment 1: mid-job eviction latency, both tiers ([Q396](../STATUS.md#Q396))

Evict a worker mid-build with no `timeout-minutes` set; timestamp the kill and
GitHub's conclusion; assert the rerun fires and the per-run retry budget
decrements exactly once.

**Correction under review.** Issue #819 framed this as "extend Q396 to
scale-set". That already happened: #815 widened Q396's scope to both tiers on
2026-07-25, and
[scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#phase-1-measure-the-real-baseline-gate)
Phase 1 records the same. The genuinely additive assertion is the retry budget
(the Q106 sharded-reservation invariant), which the row did not name.

- **Venue:** live-GitHub on kind, per the row.
- **Proves:** the real eviction-to-conclusion latency, attributed to a mechanism.
- **Unlocks:** a defensible number in place of the confounded one, and the
  [Q418](../STATUS.md#Q418) trigger.
- **Unblocked** — Q417 shipped 2026-07-26, so the scale-set tier now detects evictions and fires the rerun this experiment measures.

### Result, measured 2026-07-29

Live-GitHub tier on kind, against `actions-gateway/gateway-test`
([run 30467282642](https://github.com/actions-gateway/gateway-test/actions/runs/30467282642)),
by `E2E_GitHub_EvictedWorkerLatencyAndRerun` in
[`github_e2e_test.go`](../../cmd/gmc/test/e2e/github_e2e_test.go). A real runner was
executing a real job — GitHub reported it `in_progress` before anything was touched —
the fixture carries no `timeout-minutes`, and the worker was evicted by the kubelet.

| Observation | Value |
|---|---|
| Worker pod phase/reason | `Running/` → `Failed/Evicted` |
| Kubelet message | `Pod ephemeral local storage usage exceeds the total limit of containers 256Mi.` |
| Runner container exit code | **137** — SIGKILL |
| Job conclusion on GitHub | **`failure`** |
| **Eviction → conclusion** | **9m36s** (`finishedAt=15:44:17Z` → `completed_at=15:53:53Z`, both server-side) |
| AGC decision | `pod evicted; scheduling auto-retry` … `"attempt":1,"tier":"classic"` |
| Re-run outcome | **`403 This workflow is already running`** |

**The confound is gone and the number survives it.** 9m36s is close to the U5
probe's ~9.5 minutes, so that figure was accidentally right about magnitude — but
it is only now attributable. With no `timeout-minutes` in play, the only mechanism
left that can end the job is GitHub's own detection of a lock that stopped being
renewed, and the design's "at worst ~10 minutes from the last renewal" is what the
measurement lands on. **Quote it as "about 9–10 minutes, bounded by the job lock's
TTL", not as the workflow timeout it used to be confused with.**

**The headline finding is not the latency. It is that classic-tier eviction
recovery never actually recovers the job.** The AGC waits `evictionRetryDelay`
(default 5s) after seeing the eviction and then calls `rerun-failed-jobs` — which,
per the line above, lands ~9.5 minutes *before* GitHub concludes the run. GitHub
refuses it:

```
rerun API returned 403: {"message":"This workflow is already running", ...}
```

So on the then-shipped default the sequence was: budget slot reserved,
`actions_gateway_eviction_retries_total` incremented, re-run refused, job left
failed. The metric an operator would watch said recovery happened; nothing was
recovered. This is exactly the question
[04-operational-flows.md §4.2](../design/04-operational-flows.md) flagged as
unmeasured — "whether the rerun call succeeds while the run is still winding down
inside that window" — and the answer is no. Q503 carried the fix; see the update
below.

**Update, 2026-07-30 — Q503 fixed, Q510 flipped the spec.** The AGC now treats the
`403 This workflow is already running` refusal as "not yet": the re-run is retried
every 30 seconds inside a 15-minute window (sized past the ~10-minute lock-TTL bound
this experiment measured), on a context detached from the job goroutine so neither
the classic TaskResult nor a reconcile is held for the wait, and the whole
refusal-spanning recovery still costs one slot of the per-run budget. A re-run that
never lands is no longer silent: `actions_gateway_eviction_rerun_failures_total`
(reasons `run_never_concluded`, `api_error`) and an `EvictionRerunFailed` Warning
Event name the run needing a manual re-run. The spec flipped with the fix (Q510): a
refused re-run now FAILS `E2E_GitHub_EvictedWorkerLatencyAndRerun` — the outcome
`switch` that recorded a refusal as a report entry and passed is gone, the
identity-unknown branch fails too (Q495 is fixed, so it is a regression now), and
the conclusion wait pins **attempt 1**, which the accepted re-run's second attempt
would otherwise displace from the `filter=latest` jobs listing mid-measurement.
Verified at the unit and envtest tiers against the measured refusal body; the
live-GitHub re-validation rides the next run of this spec (live-GitHub is a
singleton tier and out of scope for the fixing session).

**Why the runner could not report, unlike Q459's drained worker.** The wrapper's
relay *did* fire — `forwarding termination signal to child`, `grace: 25s`, and the
runner logged `Runner will be shutdown for UserCancelled` — but the kubelet's
eviction gave it about two seconds before SIGKILL, and the runner was still inside
`Waiting for process exit or 7.5 seconds after SIGINT` when it died. The report
never left. That is the whole difference between this experiment and Q459's, and it
is what the two numbers measure:

| Disruption | Grace | Runner reports? | Eviction → conclusion |
|---|---|---|---|
| Graceful delete / drain ([Q459](q459-drained-worker-recovery.md)) | 30s | yes | **15–26s** |
| Kubelet eviction (this experiment) | ~2s, then SIGKILL | no | **9m36s** |

**What remains.** The scale-set half. This run measured the classic tier; Q417
plumbed the same recovery onto scale-set from the owning reconciler, and whether
GitHub's conclusion latency and the 403 both reproduce there is unmeasured. The
403 is tier-independent by construction — it is a property of the API and of the
delay, not of how the AGC detected the eviction — but that is reasoning, not a
measurement.

### What the harness cost to build, and why it is shaped this way

**The eviction lever works, and it is the only aimed one.** Eviction recovery keys
on `PodFailed` with reason `Evicted` and nothing else, so the disruption has to
produce exactly that shape. Q421 already ruled out the graceful removals. Node-wide
memory or disk pressure produces the shape but lets the kubelet choose the victim,
on a node shared with the rest of the suite. A **pod-level `ephemeral-storage`
limit** is enforced per pod, and overshooting it was measured on the e2e kind
cluster to do exactly what is needed:

| Observation | Value |
|---|---|
| Pod phase/reason after the overshoot | `Failed/Evicted` |
| Kubelet message | `Pod ephemeral local storage usage exceeds the total limit of containers 16Mi.` |
| Container exit code | `137` — SIGKILL, no grace period, so no SIGTERM relay and nothing reported to GitHub |
| Write → kill | ~55s, the kubelet's local-storage housekeeping cadence |

The zero grace period is the point rather than a side effect: it is what makes this
the *ungraceful* case, where GitHub must notice by itself. The graceful counterpart,
where the runner does get its own report out, is [Q459](q459-drained-worker-recovery.md)'s.

**Sizing the cap needed a measurement of its own.** The kubelet charges a pod only
its writable layer, emptyDirs and logs — image layers are read-only and are not
charged — and a pod built from the real runner image was measured at **28KiB**
against the node's `stats/summary` endpoint. The e2e fixture jobs add nothing to
that: neither checks out a repository. 256Mi is therefore four orders of magnitude
of headroom, which is what makes the deliberate overshoot the only thing that can
cross it.

**Q495 was confirmed here, by direct observation rather than inference — and has
since been fixed** ([#967](https://github.com/actions-gateway/github-actions-gateway/pull/967)).
A worker pod provisioned for a real GitHub job on the classic tier carried
`actions-gateway.com/job-name` and **neither** `run-id` **nor** `repository`:

```
annotations: {"actions-gateway.com/job-name":"hold",
              "cluster-autoscaler.kubernetes.io/safe-to-evict":"false",
              "descheduler.alpha.kubernetes.io/prefer-no-eviction":"true",
              "karpenter.sh/do-not-disrupt":"true"}
```

`jobMetaFrom()` and `repoInfo()` read the same two payload fields, so an absent
annotation means `runID` is `"0"` and
[`handleEviction`](../../cmd/agc/internal/provisioner/eviction.go) returns at its
first line. Note also that `system.github.job` *did* arrive: the acquisition
payload's `variables` map is populated, and it is specifically the run-identity
keys that were missing — which narrowed Q495 from "the payload is empty" to "these
keys are not where we read them". That narrowing is what the fix acted on: run
identity travels in the serialised `github` context (`contextData.github.run_id`),
not in the job variables, and the worker pods this experiment provisions now carry
their `run-id` annotation.

**What that cost this experiment, and no longer does.** The latency half was never
affected — it measures GitHub's own detection of a runner that stopped answering,
which does not involve the AGC at all. The "assert the re-run fires" half, and the
Q106 budget assertion behind it, could not fire at all on the classic tier until
Q495 landed. The spec is written to hold either way: it asserts that the AGC *saw*
the eviction and reached a decision — the assertion that separates "recovery
declined to act" from "detection never happened" — and asserts the budget
invariant only on the branch where recovery ran. On a post-Q495 build that is the
branch it takes.

### live-GitHub does not parallelize, and the reason is not the cluster

Two sessions ran live-GitHub on 2026-07-29 from separate worktrees and separate kind
clusters, and still collided. Cluster isolation does not help, because the shared
resources are on GitHub's side:

- **One fixture repo and one workflow.** Both sessions dispatch `drain-probe.yml`
  to `actions-gateway/gateway-test`. `dispatchAndResolveRun` identifies its run as
  "the one that was not there before" — which is the *other* session's run when two
  dispatches land seconds apart.
- **One `runs-on` label, one org.** Every live-GitHub tenant registers runners labelled
  `e2e` in the `actions-gateway` org, so GitHub may route either session's job to
  either cluster's gateway. The two are entangled even when the clusters are not.
- **No run-id annotation to disambiguate by** — Q495, since fixed, but absent for
  these runs — so `runningWorkerForRun` fell back to "the sole Running worker" and
  gave up when there were two.

A throwaway cluster per run, which
[testing.md](../development/testing.md) now prescribes, is still the right
move: it removes the *other* half of the collision, where a parallel session's
`helm upgrade` and `kubectl set env` fight over one GMC. It just does not make two
live-GitHub runs independent. Both halves have to hold, so treat live-GitHub as a
**singleton**: one session at a time, across all worktrees, each on its own
cluster. Q500 tracks the GitHub-side half.

This is the same contention that kept `E2E_GitHub_CancelledRunLeavesNoDeletionMark`
pending in [q459-drained-worker-recovery.md](q459-drained-worker-recovery.md), seen
from the other side — there between specs, here between sessions.

**A related hazard, learned expensively.** A live-GitHub run killed mid-spec leaves its
tenant namespace `Terminating` on an `agentpool-cleanup` finalizer that only its own
AGC can clear — and the AGC's Deployment goes away with the namespace, so it never
clears. Force-removing the finalizer unblocks the namespace but **skips the
deregistration of that tenant's runners from the org**. Those stale registrations
keep taking job assignments, so the next run's job goes `in_progress` against a
runner that no longer exists and no worker pod is ever provisioned. Prefer stopping
a run with SIGTERM and letting Ginkgo's `AfterAll` run: it deletes the
`ActionsGateway` CR while the AGC is still up, which is what lets the finalizer do
its job.

## Experiment 2: the node-drain path (Q421)

**Done 2026-07-27** — jump to the [Result](#result-measured-2026-07-27). The heading
keeps its original slug because several docs link to it.

Cordon and drain a node mid-job. Assert what the wrapper, the runner, the
provisioner, and GitHub each do.

**Correction under review, and the reason this experiment got more valuable.**
Issue #819 predicted "a measured zero" on the grounds that this is the good
path and [Q385](https://github.com/actions-gateway/github-actions-gateway/pull/747)'s
SIGTERM relay already covers it. Reading the code suggests the drain path does
not reach eviction recovery at all:

- `kubectl drain` uses the Eviction API, which **deletes** the pod. A deleted
  pod never lands in `PodFailed` with `reason: Evicted`; that phase comes from
  kubelet-initiated node-pressure eviction.
- [`waitForPodCompletion`](../../cmd/agc/internal/provisioner/completion.go)
  treats a pod that has vanished as `PodSucceeded`.
- [`provision`](../../cmd/agc/internal/provisioner/provisioner.go) calls
  `handleEviction` only on `phase == PodFailed && reason == "Evicted"`.

So on classic, a drained worker most likely reports its own terminal result via
the relay, the provisioner records success, and nothing reruns. Q417's scale-set
detection (shipped 2026-07-26) reaches the same conclusion by construction: it fires
only on `PodFailed`/`Evicted`, and deliberately excludes deletion on the grounds that
the SIGTERM relay already owns that case. This experiment is what tests that
reasoning.

This is a code reading, not a measurement, and
[testing.md](../development/testing.md#diagnosing-failures-measure-before-asserting-a-root-cause)
is explicit that a
symptom match is a hypothesis until the failing system is measured. The
experiment is what settles it. If it holds, the outcome is a recovery gap on the *graceful*
path, which is worth more than confirming a zero. Q417 shipped without covering
deletion, on the reasoning above; a finding here that a drained worker does **not**
report its own result is the evidence that would reopen that decision on both tiers.

Assertions:

1. The wrapper relays SIGTERM and the runner reports its own terminal result
   ([terminationRelay](../../cmd/worker/main.go)). The relay is
   tier-independent; the scale-set `run.sh` branch has the same PID-1 handling,
   so this experiment runs on both tiers.
2. The report completes inside the grace period. The provisioner sets no
   `terminationGracePeriodSeconds`, so worker pods get the Kubernetes default
   of 30s unless a tenant overrides it in `podTemplate`. A runner that needs
   longer is truncated by SIGKILL and the case degrades to experiment 1's.
3. Whether the job requeues, and by what mechanism. Do **not** assume it does.
4. Classic only: whether the job lock is released without waiting out the
   lapse. Scale-set has no AGC-held per-job lock; the runner owns its session
   (see scaleset-eviction-recovery.md Phase 3, which fails to find a job-scoped
   credential on that tier), so this assertion does not port.

### Result, measured 2026-07-27

**The code reading held, on both tiers: a drain reaches no eviction recovery
whatsoever.** The prediction is now a measurement, taken at two venues.

**envtest, both tiers** —
[`drain_eviction_test.go`](../../cmd/agc/internal/controller/integration/drain_eviction_test.go).
Each test drives the real `pods/<name>/eviction` subresource against a worker pod
that came out of the real provisioning path, carrying a complete run identity, and
asserts the rerun API is never called. The classic case wires the production
`InformerPodWaiter` rather than the poll fallback, because the two agree on a pod
that reaches a terminal phase and disagree on one that is deleted without ever
reaching one. Both pass. Each is the exact twin of the eviction test that *does*
fire a rerun on the same wiring, so the eviction/deletion distinction is isolated
to one substitution rather than argued.

**fake-GitHub on kind** —
[`worker_drain_test.go`](../../cmd/gmc/test/e2e/worker_drain_test.go)
(`E2E_AGC_WorkerNodeDrain`). A real `kubectl drain` against the node holding a live,
AGC-watched worker pod. Recorded:

| Observation | Value |
|---|---|
| `kubectl drain` output | `node/… cordoned` then `evicting pod tenant-drain/runner-…` |
| Worker pod phase/reason sequence, sampled at 200 ms across the drain | `Pending/` — and then gone |
| `cluster-autoscaler.kubernetes.io/safe-to-evict` on the drained pod | `false` — the drain evicted it regardless |
| `rerun-failed-jobs` calls for the run, over 45 s after removal | `0` |

The pod never published *any* terminal phase, let alone `PodFailed`/`Evicted`: it
went from `Pending` straight to absent. Nothing either tier's detection reads ever
existed. Observing the rerun at this tier needed fakegithub to answer and record
the call at all, which it now does (`/control/reruns`); the spec first asserts the
AGC's `GITHUB_API_BASE_URL` points at fakegithub, so the absence it measures cannot
be an absence of instrumentation.

**The second finding, which the experiment was not looking for.** The AGC stamps
every worker pod `safe-to-evict: false`, Karpenter `do-not-disrupt`, and the
descheduler's prefer-no-eviction marker
([`defaults.go`](../../cmd/agc/internal/provisioner/defaults.go)) precisely so a
mid-job worker is not disrupted. `kubectl drain` honours none of them — they are
advisory to autoscalers and deschedulers, not to the Eviction API — and worker pods
carry no PodDisruptionBudget. So an operator draining a node is the one disruption
source that is *neither* deflected by the disruption-safety markers *nor* recovered
by eviction recovery. Every other disruption path is covered by one or the other.

**Answers to the assertions.**

- **3 (does the job requeue, and by what mechanism):** answered, and the answer is
  *not by the AGC, on either tier*. This is the load-bearing result.
- **1, 2 (does the relay get the runner's own report out inside the grace period):**
  **not answered here, and not answerable at this tier.** A fake-GitHub worker running the
  real runner image exits by itself within seconds — fakegithub's synthetic payload
  is not a job the runner can execute — so the spec deliberately drains a
  scheduled-but-`Pending` worker instead, which makes the drain the unambiguous cause
  of the pod's removal but leaves no live container to signal. The relay itself is
  covered by the `cmd/worker` unit tests (Q385/Q445); what remains unmeasured is
  whether a *real* job, reported by a *real* runner during the grace period, ends up
  in a state GitHub will retry.
- **4 (classic lock release):** the AGC's `provision()` returns as soon as the pod
  vanishes — the waiter reports a deleted pod as `PodSucceeded` — so the AGC does not
  hold the job open. What GitHub does with that is the same open question as 1–2.

**Consequence: the gap is real but its severity is not yet established, so it is
filed rather than fixed.** Q417 scoped scale-set eviction detection to
`PodFailed`/`Evicted` on the stated reasoning that the SIGTERM relay already owns
the deletion case. That reasoning is now known to be load-bearing on both tiers, and
its premise — that the relay's report leaves the job recoverable — is exactly the
part still unmeasured. Extending both tiers to treat a graceful deletion as
recoverable would be the fix, but doing it before knowing what GitHub does with a
relayed cancellation risks auto-rerunning jobs that a human deliberately cancelled
(a `kubectl delete pod`, or a run cancelled in the GitHub UI, arrives on the same
path as a drain). [Q459](q459-drained-worker-recovery.md) carries the live-GitHub measurement and the
decision that follows from it.

**Update, 2026-07-28.** Q459 took the first half of that measurement, and the premise
holds: a real runner interrupted mid-job gets its report out inside the grace period,
GitHub concludes the job `failure` well under a minute later (15–26s across five
runs), and `rerun-failed-jobs` returns `201`
with a second attempt that runs. It also corrected one thing this section infers. The
claim above that a deliberate cancel "arrives on the same path as a drain" is true of
`kubectl delete pod` but **not** established for a GitHub-UI cancel, which reaches the
runner over its own broker connection rather than through the pod — and a *running*
worker, unlike the `Pending` one drained here, publishes `PodFailed` with an empty
reason before its object is removed rather than vanishing without a terminal phase.
Details in [q459-drained-worker-recovery.md](q459-drained-worker-recovery.md).

Worth noting for that decision: the drain path is currently *worse* for the user
than the ungraceful one. A kubelet node-pressure eviction auto-reruns the job; a
graceful operator drain does not.

## Experiment 3: oversubscription demo (Q423)

**Done 2026-07-29** — jump to the [Result](#result-measured-2026-07-29-preemption-is-not-eviction).
The answer is the opposite of what this section predicted, so the prediction is kept
below rather than edited away.

Configure `priorityTiers` so low-priority CI runs inside capacity reserved for
higher-priority work. Force preemption. Assert the preempted job recovers with
no human action.

- **Proves:** the central claim, that tiering is only safe because recovery is
  automatic.
- **Unlocks:** turns the payoff section of the write-up from an argument into a
  result.
- **Unblocked** — both contaminants cleared: Q420 and Q417 shipped 2026-07-26.
- ~~Preemption is kubelet-initiated, so unlike experiment 2 it does produce
  `PodFailed`/`Evicted` and does exercise `handleEviction` on classic.~~
  **False — measured 2026-07-29.** This conflated two mechanisms that are both called
  eviction. See the Result.

### Result, measured 2026-07-29: preemption is not eviction

**A `PriorityClass` preemption reaches no eviction recovery on either tier.** It is
the *graceful-removal* path experiment 2 and [Q459](q459-drained-worker-recovery.md) already measured,
not the kubelet path recovery acts on. The demo this experiment set out to produce
does not exist to be produced: there is no automatic recovery on the preemption path to
demonstrate.

**Why the prediction was wrong.** Two different mechanisms share the word:

- **Node-pressure eviction** is the *kubelet's*. It leaves the pod in `PodFailed` with
  `Status.Reason` `"Evicted"` — the one shape both tiers key on
  ([`provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go) step 7 on
  classic, `evictedAwaitingRecovery` in
  [`eviction_scaleset.go`](../../cmd/agc/internal/provisioner/eviction_scaleset.go) on
  scale-set).
- **Preemption** is *kube-scheduler's*, and it is what a `PriorityClass` actually
  drives. The scheduler removes the victim by **deleting** it. The kubelet then runs an
  ordinary graceful termination.

`priorityTiers` drives the second. Nothing in that path produces `Evicted`.

**How the preemption was forced, at both venues.** Node CPU and memory are the obvious
contended resources and the wrong ones: how much of a kind node is free depends on
everything else the suite is running, so a preemption forced that way races the rest of
the cluster. Both runs instead advertise a custom integer **extended resource** — one
unit, on one node — and have the victim and the displacing pod each request it.
Extended resources are integers the kubelet does not manage, so the arithmetic is exact:
one slot, two claimants, higher priority wins, and preemption is the scheduler's only
way to place the second pod.

**fake-GitHub, the full gateway** —
[`worker_preemption_test.go`](../../cmd/gmc/test/e2e/worker_preemption_test.go)
(`E2E_AGC_WorkerPreemption`), passing 2026-07-29 on the e2e kind cluster (Kubernetes
v1.36.1). A real tenant declares `priorityTiers`, the AGC provisions a worker for a job
carrying a complete run identity, and a higher-priority pod displaces it.

| Observation | Value |
|---|---|
| `spec.priorityClassName` on the worker pod | `gag-e2e-opportunistic` — the tier reached the pod, so this is oversubscription and not an ordinary eviction |
| `safe-to-evict` on the worker | `false` — **and the preemption proceeded anyway** |
| Victim `phase/reason/deletionTimestamp/DisruptionTarget-reason`, sampled at 200 ms | `Pending//2026-07-29T13:30:50Z/PreemptionByScheduler` |
| `Failed`/`Evicted` ever observed | **no** |
| Scheduler event on the victim | `Preempted: Preempted by pod 0d6e0a7d-… on node actions-gateway-e2e-worker` |
| `rerun-failed-jobs` calls for the run, over 45 s after removal | **0** |

As with the drain spec, the rerun assertion is guarded so its absence cannot be an
absence of instrumentation: the spec first asserts the AGC's `GITHUB_API_BASE_URL`
addresses fakegithub, and pins an `AcquireJob` payload carrying owner/repo/run_id —
without which `handleEviction` returns early and no rerun could fire for reasons having
nothing to do with preemption.

> The last row is the measurement as taken, before Q497. The spec kept its whole
> apparatus and flipped that assertion: it is now `E2E_AGC_PreemptedWorkerIsRecovered`
> and requires **exactly one** rerun. The rows above it are unchanged and still asserted
> — recovery must be reached by the scheduler's marker, never by the victim turning up
> `Failed`/`Evicted`.

**A second spec, for the phase a *running* victim publishes** —
`E2E_AGC_PreemptedRunningPodPhaseFollowsItsExitCode`, in the same file. The first
spec's victim is deliberately held `Pending` (its image cannot be pulled), the same
trade `E2E_AGC_WorkerNodeDrain` makes and for the same reason, so it has no live
container and cannot show what the kubelet publishes on the way out. This one preempts a
worker-shaped pod — same disruption-safety annotations, no PodDisruptionBudget — running
a process that traps SIGTERM and **exits 0**.

| Observation | Value |
|---|---|
| Victim class / preemptor class | value `100`, `preemptionPolicy: Never` / value `1000000`, `PreemptLowerPriority` |
| Victim `phase/reason/deletionTimestamp/DisruptionTarget-reason`, sampled at 200 ms | `Running//2026-07-29T13:35:29Z/PreemptionByScheduler` → `Succeeded//…/PreemptionByScheduler` |
| `Failed`/`Evicted` ever observed | **no** |
| Scheduler event on the victim | `Preempted: Preempted by pod eefad962-… on node actions-gateway-e2e-worker` |
| kubelet event on the victim | `Killing: Stopping container sleeper` |

It is deliberately *not* a gateway worker: a worker's command is the injected wrapper, so
its exit code is the runner's and cannot be made 0 on demand. What is under test is the
kubelet's behaviour on a preemption, which is worker-independent, so the pod is built to
isolate it.

The two specs agree on everything that decides recovery, and differ only in the terminal
phase — which is finding 1 below.

**Two findings beyond the verdict.**

1. **The terminal phase on a graceful removal is the container's own exit status.**
   Q459 recorded a disrupted *running* worker landing in `PodFailed` with an empty
   reason, and reasoned from there that recovery cannot key on the phase because a
   genuinely failing job produces the same shape. The second spec lands in `Succeeded`
   — its container exits 0 on SIGTERM — from the identical removal path, and the first
   spec's victim never leaves `Pending` at all. So the phase is not merely
   *ambiguous* on this path, it is not even *stable*: `Pending`, `Succeeded` and
   `Failed` all occur, decided by what the interrupted process was doing and what it
   exited with. No phase/reason combination can carry the discrimination.
2. **The scheduler leaves an unambiguous marker, and the AGC ignores it.** The victim
   carries a `DisruptionTarget` condition with reason **`PreemptionByScheduler`**. Unlike
   `deletionTimestamp` — which Q459 is weighing, and which an operator's `kubectl delete
   pod` and a drain also set — this reason is written *only* by kube-scheduler
   preemption. It cannot be produced by a human cancelling a run, nor by a job failing on
   its own. That makes the preemption slice of the graceful-removal gap closable on its
   own, without the human-cancel ambiguity that is holding Q459's decision open.

   **Closed 2026-07-29 by Q497** ([plan](archive/q497-preemption-recovery.md)). Both tiers now
   recover a preemption off this condition, sharing the existing per-`run_id` retry
   budget, and the `cause` label on the eviction counters keeps a preemption recovery
   distinguishable from a node-pressure one. The spec below flipped with it: it is now
   `E2E_AGC_PreemptedWorkerIsRecovered`, and asserts exactly one re-run rather than none.
   Two things about the fix are worth recording here, because both are consequences of
   *this* measurement rather than choices:

   - Detection keys on the condition and never on the phase, because finding 1 above
     ruled the phase out entirely.
   - Matching the `DisruptionTarget` **type** alone would have been wrong: the eviction
     API stamps the same condition with reason `EvictionByEvictionAPI`, so a
     type-only match would have silently recovered the drain path and pre-empted Q459's
     open decision. The `reason` is the whole discriminator.

**What this cost the published claim, and how it was repaid.** The oversubscription
argument in [01-executive-summary.md](../design/01-executive-summary.md) §"safe
oversubscription" and in the README's problem statement rests on displaced work coming
back by itself. The *packing* half was real and unaffected — guaranteed tiers do preempt
their way in, which is what removes the need for reserved idle headroom. The *safety*
half, as published, was not: a preempted job was left needing a manual re-run, exactly
like a drained one. Both documents were corrected to say so on the day of the
measurement.

Q497 then made the original claim true rather than leaving the correction standing, and
both documents are corrected back — this time with a measurement behind them. The
residual cost is no longer a manual re-run but the displaced job's own elapsed time: the
re-run starts from the beginning rather than resuming, which is why the guidance to put
cheap-to-repeat work in displaceable tiers survives. The drain path keeps the original
correction until [Q502](../STATUS.md#Q502) implements Q459's decision;
[troubleshooting.md](../operations/troubleshooting.md#draining-a-worker-does-not-auto-re-run-the-jobs-it-interrupts)
now covers the drain alone, with
[a separate runbook](../operations/troubleshooting.md#a-preempted-workers-job-is-not-re-run)
for a preemption recovery that fails to fire.

**A third finding, from building the spec rather than running it (Q499, since
documented in [security-operations.md § Narrowing the allowlist](../operations/security-operations.md#narrowing-the-allowlist-drain-stored-references-first)).**
Narrowing the platform PriorityClass allowlist **wedges deletion of any tenant still
referencing the removed class**. The `priorityclass-allowlist-guard` policy re-validates
stored objects on update — deliberately, and documented as a feature — but tearing a
tenant down *is* a sequence of updates: the GMC clearing `gmc-cleanup` from the
`ActionsGateway`, the AGC clearing `agentpool-cleanup` from the `RunnerGroup`. With the
class off the allowlist every one of them is denied, so the finalizers can never be
removed and the namespace hangs in `Terminating` with no controller able to free it.
Recovering needs a human to re-widen the allowlist and strip the finalizer by hand.
Reproduced exactly that way here; the spec's teardown now drains the tenant *before*
restoring the fail-closed default, and the ordering is commented so it is not
"simplified" back.

**What is not measured here.** The wrapper's SIGTERM relay, and therefore whether a
*real* preempted job reports itself to GitHub in a state a re-run would accept. Neither
spec has a real runner in the victim — the first holds it `Pending`, the second runs a
stand-in process chosen for its exit code. That question is the drain path's too, and
Q459 answered it for a graceful delete at live-GitHub: the report gets out and
`rerun-failed-jobs` returns `201`. A preemption is the same removal, so the same answer
is expected — but it is inherited, not re-measured.

## Experiment 4: quota gate under real pressure ([Q422](../STATUS.md#Q422))

Fill the namespace `ResourceQuota`, submit more jobs than fit, and assert they
stay queued server-side rather than claimed-and-stalled.

**Correction under review: this is cheaper than #819 assumed, and it splits.**
The "visible in metrics or Events" half is already instrumented.
`actions_gateway_jobs_admission_rejected_total{reason="quota"}` ships with
[#793](https://github.com/actions-gateway/github-actions-gateway/pull/793) and
is documented in
[§4.2](../design/04-operational-flows.md#42-job-execution-flow-agc) step 2a, so
asserting it needs no new plumbing.

- **Half A (envtest) — done 2026-07-26.** Covered by
  [`q422_quota_admission_test.go`](../../cmd/agc/internal/controller/integration/q422_quota_admission_test.go),
  one test per tier the rung serves. Findings below.
- **Half B (live-GitHub):** two AGC sessions on the same RunnerGroup, one without
  headroom, and the job is picked up by the sibling. This is the half that
  needs live GitHub redelivery and cannot be faked.
- **Not blocked.**

### Half A findings

Both tests drive a live listener against the broker/scale-set fakes with a real
`ResourceQuota` read through the manager's informer cache, and both were
mutation-checked: disabling the rung fails each of them.

- **Classic tier.** With the quota full, three deliveries in a row are skipped:
  `acquirejob` is never called (asserted on the broker stub's server-side call
  counter, not on the absence of a pod — "no pod appeared" would also pass for
  the claim-and-stall this rung exists to prevent),
  `..._admission_rejected_total{reason="quota"}` increments once per delivery,
  and no worker pod or per-job Secret is staged.
- **The ceiling budget is untouched.** `maxWorkers` is 1, so a single leaked
  reservation would close the gate permanently. Once headroom returns a job is
  claimed and a pod is built, and the `reason="ceiling"` series never moves.
  Mutating `Admit` to reserve a slot before refusing for quota fails exactly
  this assertion.
- **Scale-set tier** (the rung reached it in Q443, and Q450 corrected the
  footprint). The existing Q443 test covers the all-or-nothing case; the gap was
  the *partial* one, where `AdvertiseCapacity` converts a headroom delta into a
  total. Under a half-consumed quota the invariant `advertised + withheld ==
  declared ceiling` holds, which is the scale-set expression of "the quota rung
  reserves nothing".
- **A caveat on what envtest can show.** There is no resourcequota controller,
  so the tests own `status.hard`/`status.used` outright — which is what lets
  them fill a quota the way a busy namespace does (`hard − used`, the arithmetic
  the gate actually runs) rather than declaring a hard limit too small to ever
  fit a worker, as every prior envtest does. The flip side is that `used` does
  not rise as worker pods are created, so these tests cannot assert an
  assignment count across more than one poll.

## Experiment 5: utilization delta ([Q424](../STATUS.md#Q424), deferred)

Same workload on dogfood, tiers off versus tiers on, occupancy measured over a
fixed window.

- **Proves:** the packing-density thesis directly, and it is the one number the
  whole argument is missing.
- **Deferred rather than queued.** Q417 shipped 2026-07-26, but this still needs a
  dogfood workload stable enough for a fixed-window comparison to mean anything. With
  no owner-actionable next step today, a Queue position would be fiction.

## Sequencing

Q417 shipped 2026-07-26, so nothing here is blocked on it any more.

1. ~~Q421 (experiment 2)~~ — **done 2026-07-27**; see
   [Result](#result-measured-2026-07-27). Its residual is
   [Q459](q459-drained-worker-recovery.md), which needs live-GitHub and so sequences with the other
   live-GitHub work below rather than ahead of it.
2. [Q422](../STATUS.md#Q422) (experiment 4).
3. [Q396](../STATUS.md#Q396) (experiment 1), which then gates
   [Q418](../STATUS.md#Q418). Fold [Q459](q459-drained-worker-recovery.md) in around here: both
   want a real GitHub run interrupted mid-job, and Q396 is already standing that
   up.
4. ~~Q423 (experiment 3)~~ — **done 2026-07-29**; see
   [Result](#result-measured-2026-07-29-preemption-is-not-eviction). ~~Its residual is
   Q497~~ — **also done 2026-07-29** ([plan](archive/q497-preemption-recovery.md)): the
   `PreemptionByScheduler` marker resolved the discriminator question for the preemption
   slice without waiting on Q459's human-cancel one, exactly as predicted here. Then
   revive [Q424](../STATUS.md#Q424) (experiment 5).

## Acceptance criteria

- A published eviction-recovery latency figure with the mechanism attributed,
  replacing the confounded U5 number everywhere it is cited.
- ~~A recorded answer for the drain path: either it recovers via the SIGTERM relay, as
  Q417 assumed when it scoped detection to `PodFailed`/`Evicted`, or the gap is filed
  and both tiers are extended to cover deletion.~~ **Met 2026-07-27** by the second
  branch: neither tier recovers a drained worker, and the gap is filed as
  [Q459](q459-drained-worker-recovery.md). Extending the tiers is deliberately left to that row —
  the same code path carries deliberate cancellations, so it needs the live-GitHub answer
  before it can tell a drain from a `kubectl delete pod` worth honouring.
- The quota gate demonstrated under contention, with the rejection counter as
  the observable.
- ~~Preemption recovery demonstrated end to end before the oversubscription claim
  is published.~~ **Met 2026-07-29, in two steps.** The experiment first found there was
  nothing to demonstrate — a `PriorityClass` preemption is a graceful deletion, not a
  kubelet eviction, so no automatic recovery fired — and the published claim was
  corrected rather than illustrated. Q497 then built the recovery
  ([plan](archive/q497-preemption-recovery.md)) and the claim was restored, this time with a
  measurement behind it. The fake-GitHub spec that made the original measurement was flipped
  from "no rerun" to "exactly one rerun" and is what demonstrates it end to end. See
  [the result](#result-measured-2026-07-29-preemption-is-not-eviction).
