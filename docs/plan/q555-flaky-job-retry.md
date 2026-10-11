# Q555 flaky-job detection and classified retry: problem, requirements, scope

**Status:** ⓘ draft for a design session, written 2026-10-10.
Nothing is built and no CRD shape is committed.
[Q555](../queue/Q555.md) is in the Queue: on 2026-10-10 the maintainer ruled that this repository's own CI, which runs on GAG, is the operator demand its trigger waited for.
A design session settles the [open questions](#open-questions-for-the-design-session) next.
It extends [G.17](../design/appendix-g-future-enhancements.md#g17-opt-in-auto-retry-for-flaky-jobs-beyond-disruptions) rather than replacing it: G.17 holds the original prior-art table and the tenant-signal channel analysis, and this file adds the systems G.17 did not cover, a first-principles pass, and testable requirements.

## The prerequisite already exists

Q555, G.17 and the [release ladder](release-ladder.md) all say detection is blocked because nothing gives the AGC a real job outcome.
That is false on the scale-set tier, and was false before Q555 was filed on 2026-07-31.

- The scale-set queue delivers a terminal `JobCompleted` message per job, carrying `result`, the owner, repository, `workflowRunId`, and `jobDisplayName` ([`scaleset/types.go`](../../scaleset/types.go), `JobMessage`).
- The Q264 spike live-observed `result: "failed"` on 2026-07-05 ([phases record](archive/q264-scale-set-protocol-phases.md), finding 6).
- The listener already counts it, as `actions_gateway_scaleset_jobs_completed_total{result}`; nothing else reads it.

So the AGC holds, for every scale-set job, the outcome plus the `(owner, repo, run_id)` triple that `rerun-failed-jobs` takes.
The classic tier gets no `JobCompleted`, but classic is terminal ([sunset review](v1-classic-sunset-review.md)), so nothing here should be built for it.

The live evidence is thinner than "the outcome is delivered" suggests, and each gap matters to a requirement below:

1. **The one observed `failed` was almost certainly a disruption.** Finding 6 follows finding 5 in the same spike, which SIGKILLed the runner mid-job, and GitHub concluded that job `failure`; the record does not say outright that they were the same job.
   If they were, a disrupted job reads `failed`, the same as a flake would (R2 exists for this).
   A job that failed on its own merits, a red test step, has not been observed on this channel.
2. **The full `result` vocabulary.** Only `succeeded` and `failed` are observed on live GitHub.
   The unit tests use both `cancelled` and `canceled`, so the canceled spelling is a guess on at least one side.
3. **Whether a timed-out job is distinguishable** from a failed one.

All three are one dogfood turn-up to settle: fail a step, cancel a run, and time a job out, then read `actions_gateway_scaleset_jobs_completed_total` by `result`, which needs no code change.

## Problem statement

**The goal is automatic recovery that reduces the need for human intervention, and with it the operating cost of running CI on GAG** (maintainer, 2026-10-10).
Every requirement below is judged by that: a manual re-run, a triage of a failure the author could not have fixed, and a hunt for which job is flaky are each an intervention.
A mechanism that removes interventions now by hiding a regression that costs more to find later does not meet it, which is why a blanket retry is [not scheduled](#unclassified-retry-not-scheduled).

A flaky job fails for reasons unrelated to the change under test, and today a human has to notice, judge that it is a flake, and click re-run.
On a pull request that costs one round trip of human attention and however long the author takes to come back.
In a merge queue it costs more: the failed entry is ejected, and with batching, entries behind it are rebuilt.

GAG already re-runs a job the *cluster* broke (eviction, preemption, drain, deletion), and deliberately never re-runs a job that failed on its own merits, because re-running a genuine failure hides it.
That boundary is right for genuine failures and leaves flakes, the commonest CI annoyance, entirely manual.

Two problems hide inside "flaky-job retry", and they need different things:

- **Toil.** Someone has to click re-run.
  Retry fixes this.
- **Invisibility.** Nobody knows which jobs flake, how often, or what they cost.
  Each re-run is one person's private judgement, and the aggregate is lost.
  Detection fixes this.

Retry without detection makes the suite worse over time: flakes stop hurting, so nobody fixes them, and real intermittent regressions slip through with them.
Detection without retry leaves the toil but makes it fixable.
The platform layer is the only place both can be done without editing every workflow, and it is also the layer that knows least about *why* a job failed.

**A probe of this repo's own CI shows why detection must be per job, not per run.** Over 1,944 runs from 2026-10-03 to 2026-10-09, 59 had `run_attempt > 1`, all on `pull_request`, and they came from just 4 head SHAs, each re-running 10 to 17 workflows at once.
That is a whole PR being re-run, for reasons the run record cannot show; a flake and a re-run to force a gate look the same.
The sample is also uneven (2026-10-07 and 2026-10-08 returned one run each, which the API's search window may explain), so treat the counts as illustrative.
`run_attempt` counts human intent, not flakes.

## First principles

**What a flake is, operationally.** Same inputs, different outcome.
A re-run keeps its `run_id`, and GitHub re-runs a run against the same commit and workflow file it ran originally, so "job *J* in run *R* failed on attempt *n* and succeeded on a later attempt" is a flake by construction, with one caveat: the inputs a job pulls from outside the commit (a registry, a package index, a secret, the clock) can change between attempts.
At the platform layer that caveat does not matter, because both readings mean "not the change's fault".

**A retry is a bet, and the platform does not know the odds.** Retrying a failed job pays off when the failure was a flake and wastes one job's compute when it was real.
Without history the AGC has no prior for either, which is why detection comes first: it is what turns a blanket policy into an informed one.

**Masking has a number.** If a change introduces a failure that fires with probability *p* per attempt, one automatic retry lets it through with probability 1 − *p*² instead of 1 − *p*.
At *p* = 0.5 that is 75% instead of 50%; at *p* = 0.1, 99% instead of 90%.
Retry is therefore always a trade against catching intermittent regressions, which is why it must be opt-in and why every retry must leave a counted trace.

**The failure that retries worst is the correlated one.** When a shared dependency is down (a registry, the egress proxy, GitHub itself), every job fails, every job qualifies for retry, and retry doubles the load on the thing that is already failing.
A per-run budget cannot see this; only a set-wide rate can.

**Not every job is safe to run twice.** A failed deploy, release or publish job may have half-applied, and re-running it repeats the side effects.
Disruption recovery already accepts this, but disruptions are rare and a cluster event; flakes are common and a property of the job.
Pull request and merge queue jobs are the ones built to be thrown away, and they are where flakes cost the most human time.

**Budget state has to outlive the AGC.** The disruption budget lives in process memory (`reserveEvictionRetry`, [`eviction.go`](../../cmd/agc/internal/provisioner/eviction.go)), so an AGC restart refills it.
That is tolerable for disruptions; for a retry triggered by a deterministic failure it means a restart loop re-runs a broken job without limit.
GitHub already keeps a durable per-run counter, `run_attempt`, which no restart can reset and which also counts human re-runs, so capping on it is conservative in the right direction.

**A retry is safe when the failure is classified as platform-caused, temporary and self-resolving.** That is the property GAG's existing arms are meant to have.
Preemption, a drain or hand-delete (both the `deletion` cause), a worker that never started (`abandoned`), and one that vanished while the AGC was down are each recognised from Kubernetes facts, come from a temporary situation, and clear once capacity or the node comes back.
Retrying one of those masks nothing, because the tenant's code never got a fair run.
So the low-risk way to retry more is to recognise more classes that pass that test, and only after that to retry failures nobody has classified.
Classifying is where GAG is strong: no GitHub-layer product can see the pod, the node, the egress proxy or the mirror a job depended on.

**The `eviction` arm does not fully pass that test today.** It keys on `reason: Evicted` alone ([`eviction_scaleset.go`](../../cmd/agc/internal/provisioner/eviction_scaleset.go)), and the kubelet sets that reason when a pod exceeds its *own* `ephemeral-storage` limit, as well as when the node is short.
The live eviction-recovery e2e triggers eviction exactly that way, with a job that outgrows its own limit, and expects the re-run ([`github_scaleset_test.go`](../../cmd/gmc/test/e2e/github_scaleset_test.go)).
That is a tenant-caused, likely deterministic failure, the storage analogue of `OOMKilled`, retried up to `maxEvictionRetries` (default 2) and documented as "node pressure".
Whether the kubelet's `status.message` separates the two shapes is unmeasured; [Q1177](../queue/Q1177.md) tracks it, and R10's test applies to it as much as to any new class.

**Retry moves the flake away from the person who can fix it.** Today the pull request author sees the red run.
After a GAG retry the run is green, and the flake survives only in operator metrics and Events, because writing back to GitHub is out of scope (R17).
Removing that friction also removes some of the pressure to fix the test, which is why Kubernetes bans automatic retries outright.
On 2026-10-10 the maintainer first kept unclassified retry as an opt-in phase 2 with this cost understood, then preferred classified retry, which does not pay it; unclassified retry is now [not scheduled](#unclassified-retry-not-scheduled).

**Whoever pays decides.** The operator pays for compute; the repository owner owns the correctness of their checks.
An unclassified retry spends both, so it would need an explicit opt-in from whoever owns the `RunnerSet`, and a job's own workflow must never be able to switch it on (G.17 already holds this line for the tenant-signal channel).

## Prior art

G.17's table covers GitLab, Buildkite, CircleCI, Azure Pipelines, Datadog, Bazel, Argo and Tekton.
This section adds what that table lacks, researched 2026-10-10.
Each line was read at its source unless it says otherwise.

### What GitHub itself does and refuses

- **No native auto-retry or flaky-test feature as of 2026-10-10.** The Actions changelog from 2026-09-03 to 2026-10-02 has nothing on either, and the feature request ([#72195](https://github.com/orgs/community/discussions/72195)) has had no staff reply since it was opened.
- **A run can be re-run at most 50 times**, counting full and failed-only re-runs together, a cap [added on 2026-04-10](https://github.blog/changelog/2026-04-10-actions-workflows-are-limited-to-50-reruns/) because automations were retrying hundreds of times.
  Re-runs are allowed for 30 days, and reuse the original `GITHUB_SHA`, `GITHUB_REF` and the triggering actor's privileges ([docs](https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-workflow-runs/re-running-workflows-and-jobs)).
- **Both re-run endpoints re-run dependents**: `jobs/{id}/rerun` re-runs "a job and its dependent jobs", `rerun-failed-jobs` "failed jobs and their dependent jobs" ([REST docs](https://docs.github.com/en/rest/actions/workflow-runs)).
- **Neither works while the run is in progress.** The docs are silent; [#67654](https://github.com/orgs/community/discussions/67654) shows both refusals, and the AGC already handles the run-level one as `errRunNotConcluded`.
  So a platform retry waits for the whole run, whichever endpoint it uses.
- **The merge queue never retries.** A failed required check removes the PR and the remaining groups are rebuilt ([docs](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)).
- **Marketplace retries run inside the job.** [`nick-fields/retry`](https://github.com/nick-fields/retry) wraps a shell command; [`Wandalen/wretry.action`](https://github.com/Wandalen/wretry.action) wraps an action, and retries its pre, main and post stages together.
  Either way GitHub records one green step, and the flake survives only in the log.

### Systems that decide *what* to retry

| System | Mechanism | The idea worth taking |
|---|---|---|
| [Zuul](https://zuul-ci.org/docs/zuul/latest/config/job.html) | `attempts` (default 3) retries **only** a failure in the pre-run playbook or a node going unreachable; a failure in the job's own playbook never retries. Exhausting it concludes `RETRY_LIMIT`, not `FAILURE` | Classify by *phase*: setup and infrastructure retry, the tenant's payload does not. A distinct terminal result names why |
| [Jenkins `retry`](https://www.jenkins.io/doc/pipeline/steps/workflow-basic-steps/) | `conditions: [kubernetesAgent()]` retries a lost agent pod but ignores termination reasons "likely under the Pipeline author's control", such as `OOMKilled` | The closest precedent to GAG's disruption arm, including the line it draws |
| [Argo Workflows](https://argo-workflows.readthedocs.io/en/latest/retries/) | `retryPolicy`: `OnFailure` (main container failed), `OnError` (controller or sidecar failed), `OnTransientError`; an `expression` reads the last exit code and message | Error versus failure as a first-class split |
| [Mergify Auto-Retry](https://docs.mergify.com/ci-insights/auto-retry) | Rules on job name, log regex, branch, conclusion and known-flaky status; 1 to 10 retries; waits for the whole workflow, then re-runs failed jobs | The nearest product to this one, on the same GitHub API, and it **excludes merge-queue batch PRs** |

### Systems that decide *whether* retry is acceptable at all

- **Kubernetes forbids it.** Its [flaky-tests policy](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/flaky-tests.md) bans automatic retries inside test jobs and tracks every flake as a `kind/flake` issue.
  Its one automatic retest, a [robot](https://github.com/kubernetes/test-infra/blob/master/config/jobs/kubernetes/sig-k8s-infra/trusted/sig-contribex-triage-robot-retester.yaml), re-tests only approved PRs, at most one PR per 20 minutes project-wide, and posts a visible comment a human can stop with `/hold`.
- **Gradle's [test-retry plugin](https://github.com/gradle/test-retry-gradle-plugin)** has `maxFailures`, which stops retrying when one round fails too many tests (a full disk), and `failOnPassedAfterRetry`.
  Its README says retry alone "is not a viable flaky test mitigation strategy".
- **Trunk [quarantines](https://docs.trunk.io/flaky-tests/quarantining) instead of retrying**: a quarantined test still runs and is still reported, and only its effect on the exit code is removed.
- **Meta's [probabilistic flakiness score](https://engineering.fb.com/2020/12/10/developer-tools/probabilistic-flakiness/)** treats a retry as a sample rather than a verdict: every attempt feeds a per-test estimate of how often it fails in a good state.
- **Chromium** re-runs a failing test without the patch to tell a broken tip of tree from a broken change (2018 to 2020 docs, found by search; current behaviour not verified).

### What the evidence says about the cost

- **Retry usually works.** On GitHub Actions across 1,960 Java projects, 3.2% of builds are re-run, 67.73% of re-runs change the outcome, 91.6% are a single attempt, and 99% of flaky jobs pass within 5 ([Ge & Zhang, 2026](https://arxiv.org/html/2602.02307v1)).
- **Ignoring what looks flaky hides real regressions.** On Chromium, treating predicted-flaky failures as ignorable missed about 76.2% of regression faults ([Haben et al., 2023](https://arxiv.org/abs/2302.10594)).
  This is the strongest evidence for the masking cost the first-principles section computes.
- **No source recommends a retry count.** One automatic retry matches what people already do by hand (91.6% single attempts).

### What this changes

1. **The merge queue is where retry pays most and where the only product on the same API refuses to go.** It matters only to [unclassified retry](#unclassified-retry-not-scheduled), which is not scheduled.
2. **Classification is the idea G.17 lacked, and GAG is already good at it.** Zuul retries only setup and infrastructure failures; Jenkins' `kubernetesAgent()` retries a lost pod but not `OOMKilled`.
   GAG's disruption arms are the same idea, and phase 2 widens them ([classified retry](#classified-platform-retry-phase-2)).
3. **A global rate cap belongs beside the per-run cap** (Kubernetes' one per 20 minutes, Gradle's `maxFailures`); classified retry keeps one beside R11's per-class waits.
4. **The 50-re-run cap is shared with humans**, and a re-run by the AGC runs with the original actor's privileges, which the existing disruption path already accepts.

## Requirements

MUST and SHOULD are used in their usual sense.
Each requirement is written to be testable.

**Direction, set 2026-10-10.** The maintainer judged classified platform retry more practical and useful than a blanket flake retry: every class GAG can recognise and retry is a failure nobody has to re-run by hand to triage.
So phase 2 is classified retry.
A blanket retry of unclassified failures is [not scheduled](#unclassified-retry-not-scheduled), and its design is kept below in case detection shows it is still wanted.

### Detection (phase 1, no behaviour change)

- **R1.** The AGC MUST classify a job as flaky when, for the same `(owner, repo, run_id, job display name)`, a `failed` completion is followed by a `succeeded` completion on a later attempt.
- **R2.** A failure the AGC attributes to a disruption it recovered (any `cause` on the existing re-run counters) MUST NOT count toward R1.
  Otherwise every eviction reads as a flake.
- **R3.** Detection MUST be surfaced per `RunnerSet` as a counter with bounded label cardinality: job and repository names MUST NOT be metric labels.
  The identity of each flaky job MUST be reachable through Kubernetes Events or another per-occurrence surface.
- **R4.** Detection SHOULD be on by default.
  It changes nothing a job sees and makes a hidden cost visible, so it passes the [secure-by-default](../design/05-security.md) test that retry does not.
- **R5.** Detection MUST state its blind spots: a pairing lost to an AGC restart, a re-run that lands on another set, and a job that is never re-run all undercount.
  An undercount is acceptable; a false flake is not.
- **R6.** Detection MUST NOT need a GitHub API call per completed job.
  Calls are for the rare case (a failure, or a re-run), never the common one.
- **R7.** For each failed attempt, detection MUST record the platform context GAG could read at the time: the pod's phase, reason and message, each container's termination reason and restart count (sidecars included), the node's conditions, and the readiness of the set's egress proxy pool and registry mirror.
  The record of failures that later passed is the evidence that proposes new classes; without it, a candidate is a guess.
- **R8.** Detection MUST tell the re-runs GAG triggered from the ones it did not, and report per set the rate of re-runs nobody automated.
  That rate is this feature's success metric: each class shipped should lower it, and a class that does not is not paying for itself.

### Classified platform retry (phase 2)

A class is a failure shape GAG recognises and retries automatically, the way it already retries eviction, preemption, deletion, abandoned and vanished workers.

- **R9.** A class MUST be recognised from facts GAG owns (Kubernetes objects, its own components, protocol messages), never from job logs or anything tenant code writes.
- **R10.** A class MUST be caused outside the tenant's code and configuration.
  A job exceeding its own limits (memory, `ephemeral-storage`, `timeout-minutes`) does not qualify on that fact alone; [Q1177](../queue/Q1177.md) is the existing arm that misses this.
- **R11.** A class MUST NOT spend the budget while its cause still holds.
  How it waits depends on how the cause clears, and each class MUST declare which of three applies:
  - **A fresh pod clears it.** A re-run builds a new pod, and the scheduler will not place it on a `NotReady` node, so the retry is already the change.
    The existing delay and pacing are enough.
  - **GAG can observe it clearing.** The cause is state GAG owns or computes (proxy pool readiness, mirror readiness, schedulable capacity, the next pod's memory limit), so the retry waits on that state, as the `abandoned` arm already waits for a worker pod to bind before it re-runs (Q691, [`abandoned_rerun.go`](../../cmd/agc/internal/provisioner/abandoned_rerun.go)).
  - **Neither.** The cause is outside GAG and unobservable from it, such as GitHub or an upstream registry, so it gets the standard answer: backoff and a circuit breaker.

  A set-wide breaker applies to every class regardless, because a cause can be misdeclared.
  The timing is coarser than request-level retry: a re-run cannot start until the whole run concludes, up to about 10 minutes after a killed runner (Q396), so a backoff schedule matters less than not spending one of a run's two slots too early.
- **R12.** Each class MUST arrive with evidence: a dogfood reproduction that produces it, and its discriminator shown to separate it from the nearest tenant-caused shape.
- **R13.** Each class MUST get its own `cause` label on the existing counters and spend the existing per-run budget (`maxEvictionRetries`), so a run cannot collect one budget per class.
  It is on by default like the existing arms, and reuses the paced re-run path (`rerunUntilAccepted`).
- **R14.** The AGC MUST measure each cause's retry success rate (re-runs that then passed, over re-runs), using detection's pairing.
  A class whose retries keep failing is misclassified, and the rate is what says so: Q1177's shape would read near zero.
- **R15.** The per-run budget MUST survive an AGC restart before classes that fire more often are added.
  It lives in process memory today ([`reserveEvictionRetry`](../../cmd/agc/internal/provisioner/eviction.go)); capping on GitHub's `run_attempt` is one way, and keeps the AGC well inside GitHub's 50-re-run limit, which humans share.
- **R16.** Every classified retry MUST be counted and evented with its cause, and its compute attributable per set ([cost attribution](../operations/cost-attribution.md)).
- **R17.** Classified retry MUST need no GitHub App permission beyond what disruption recovery already uses.

### Known-futile retries

The inverse of a class: a failure GAG can recognise as one a retry will not fix.
Skipping it saves a budget slot, a node, queue time and throughput, and in a correlated failure it is the difference between one failed job and a stampede.
It also gives the person who has to act a reason, where today an exhausted budget only says a manual re-run is needed.

- **R18.** Before any retry spends a budget slot, on the existing arms and every new class alike, the AGC MUST check it against the known-futile shapes below.
  A futile retry is skipped, counted under its own reason label, and evented with the reason and the action that would fix it (raise the limit, fix the image, nothing because the run was superseded).

| Futile shape | What GAG would observe | Status |
|---|---|---|
| The worker's *shape* cannot run: an unpullable image, or a pod no node can host | The capacity gate's startup and placeability verdicts ([`worker_startup_verdict.go`](../../cmd/agc/internal/controller/worker_startup_verdict.go), Q714) | Built for intake: the gate stops new work when every next pod would fail the same way. Whether the re-run paths consult it is unverified |
| The job exceeded its own limit | `OOMKilled` at its limit under `Static` sizing, an `ephemeral-storage` limit eviction, `timeout-minutes` | The eviction arm retries the storage case today ([Q1177](../queue/Q1177.md)) |
| A newer commit superseded the run | The pull request's head no longer matches the run's `head_sha`, or the run was cancelled by a concurrency group | Not checked: nothing in the re-run path reads either. One API call per retry, which is affordable because retries are rare |
| GitHub will refuse the re-run | `run_attempt` at 50, a run older than 30 days, a `cancelled` conclusion | The deletion arm already stands down on `cancelled` (Q811); the other two are unchecked |
| The same class failed this job again | A second failure of the same class on a later attempt, after its cause was judged cleared | Per-run counterpart of R14: the class is wrong for this job, so stop and say so |
| A correlated outage | A set-wide failure rate past the breaker's threshold | R11's breaker |

### Candidate classes

None is measured yet; each needs R12's reproduction before it becomes a class.

| Candidate | What GAG would observe | Clears by (R11) | Note |
|---|---|---|---|
| Out of memory under a [right-sizing](runner-sizing-profiles.md) profile | Container `OOMKilled` at its limit, on a set whose profile derives limits from history | Observable: GAG computes the next limit | Qualifies under R11 only when the retry's pod gets a strictly higher memory limit; see [below](#oomkilled-under-right-sizing) |
| Setup failure before the first step | The job failed while the runner was registering or downloading actions | Neither, when the cause is GitHub: backoff and breaker | Needs the worker to report where setup ends; unmeasured |
| Egress proxy pool unavailable | The pool had no ready endpoint during the job's window | Observable: pool readiness | GAG owns the pool |
| Registry mirror unavailable | The mirror was unready while the job pulled an image | Observable: mirror readiness | Same shape as the proxy |
| Node lost under a running worker | Node `NotReady` or unreachable; the pod never publishes a terminal phase | Fresh pod | The `deletion` arm needs a published terminal phase, so whether this is already covered is unverified |
| Node-level out-of-memory kill | Container killed below its own limit, with a `SystemOOM` node event (the kubelet's name, from recall) | Fresh pod, not guaranteed: it can land on the same packed node | Overcommitted packing is the platform's doing; a kill at the container's own limit is not |
| `dockerd` sidecar crash | The Docker-in-Docker sidecar's restart count rose during the job | Fresh pod | Mixed: `dockerd` also dies of tenant disk use, so it needs a discriminator |

### OOMKilled under right-sizing

A job killed at its own memory limit is tenant-caused under R10, and retrying it under a fixed limit fails the same way.
Right-sizing changes that: under the `Throughput` and `Binpack` profiles the next pod's limit is derived from observed usage, so a retry can be the fix rather than a repeat.
It qualifies under R11 when the AGC can show the retry's pod will get a strictly higher memory limit than the pod that died, and it is easy to get wrong in three ways (all read from source, 2026-10-10):

1. **The set's history may not move.** The recommended limit is the set's highest sampled per-job peak × 1.4 ([`aggregate.go`](../../cmd/agc/internal/usage/aggregate.go)), sampled from metrics-server every 15 seconds.
   A job dies at its limit, so its sampled peak is at most that limit, and lower whenever the climb before the kill is shorter than one sample.
   One OOM may raise the limit by up to 1.4×, or not at all.
2. **History is not used until it is confident.** A set with fewer than 5 sampled jobs provisions `Static`, which is the template as written.
3. **A clamp can stop it.** A limit already at the profile's ceiling cannot grow, so the retry would repeat.

So the retry should not count on the aggregate.
The AGC SHOULD raise the limit for that job's next attempt from the killed pod's own limit (× the same headroom, clamped to the ceiling) and retry only if that is higher.
That needs the scale-set assignment to carry enough identity to recognise the re-run, which `JobAssigned` does (run id and job display name).

**It still masks one thing.** A pull request that doubles a job's memory use would get the memory silently.
The `SizingDrift` condition already reports a template below observed peaks, and this class's counter must make each bump visible.

### Unclassified retry (not scheduled)

A blanket retry of failures nobody classified: the flake retry G.17 sketched.
It is not scheduled, because [moving the flake away from the author](#first-principles) is a cost classified retry does not pay.
Revive it if detection shows flake toil the classes cannot reach and the maintainer accepts that cost.
The design if it is revived:

- Off by default, opted in per `RunnerSet` by its owner, never by anything a workflow emits.
- Only on `result == failed`; never on succeeded, cancelled or skipped jobs, nor a run a user cancelled.
- One retry per run by default, capped on `run_attempt`, with its own budget and `cause`.
- A set-wide failure-rate breaker and a retry rate cap.
- Scoped by event, defaulting to `pull_request`, with `merge_group` an open question (where a flake costs most, and where Mergify declines to retry).
- A retry that passes counts as a detected flake under R1.

## Scope

**In scope:**

- Flaky-job detection on the scale-set tier, with the platform context of each failure (R7).
- Classified platform retry on the existing re-run arms, one evidenced class at a time.
- Metrics, Events and a condition for the operator.

**Out of scope, and why:**

| Out | Why |
|---|---|
| The classic tier | It never receives `JobCompleted`, and it is terminal |
| Unclassified retry | Not scheduled; its design is kept [above](#unclassified-retry-not-scheduled) |
| Test-level retry or quarantine | Needs the test framework; Datadog, Trunk and Develocity own that layer |
| Classifying from job logs | R9: a class rests on facts GAG owns |
| A signal from inside the job asking for retry | G.17's channel analysis stands; it gives up the "no workflow edits" property and needs its own design |
| Writing back to GitHub (check runs, PR comments) | Needs new App permissions (R17) |

## Open questions for the design session

1. **Where pairing state lives.** In memory loses pairs on restart (R5 accepts the undercount); a ConfigMap shares the write-contention problems the recovery claims already hit (Q1108); asking GitHub for the previous attempt's jobs is stateless but costs a call per re-run.
2. **Run-granular or job-granular re-run.** Both endpoints wait for the run to conclude and both re-run dependents, so timing does not decide it.
   `rerun-failed-jobs` re-runs every failed job, plus siblings fail-fast cancelled; `jobs/{id}/rerun` re-runs one, but needs the job's numeric id, which `JobCompleted` does not carry (it has a UUID), so it costs a jobs-list call per retry.
3. **Which candidates can be told apart**, and in what order to measure them.
4. **Event scope.** The existing arms re-run every event, deploy and publish jobs included.
   Do new classes inherit that?
5. **The CRD shape**, via [api-review.md](../development/api-review.md): whether detection or any class needs a field, and whether a class needs a per-set off switch.
