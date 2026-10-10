# Q555 flaky-job retry: problem statement, requirements, scope

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

**A retry is safe when the failure is classified as platform-caused, temporary and self-resolving.** That is the property GAG's existing arms already have.
Eviction, preemption, drain, deletion, a worker that never started, and a worker that vanished are each recognised from Kubernetes facts, come from a situation that is temporary, and resolve once capacity or the node comes back.
Retrying one of them masks nothing, because the tenant's code never got a fair run.
So the low-risk way to retry more is to recognise more classes that pass that test, and only after that to retry failures nobody has classified.
Classifying is where GAG is strong: no GitHub-layer product can see the pod, the node, the egress proxy or the mirror a job depended on.

**Retry moves the flake away from the person who can fix it.** Today the pull request author sees the red run.
After a GAG retry the run is green, and the flake survives only in operator metrics and Events, because writing back to GitHub is out of scope (R16).
Removing that friction also removes some of the pressure to fix the test, which is why Kubernetes bans automatic retries outright.
On 2026-10-10 the maintainer kept retry as an opt-in phase 2 with this cost understood, on the condition that detection ships first and makes the flake visible somewhere.

**Whoever pays decides.** The operator pays for compute; the repository owner owns the correctness of their checks.
Retry spends both, so it needs an explicit opt-in from whoever owns the `RunnerSet`, and a job's own workflow must never be able to switch it on (G.17 already holds this line for the tenant-signal channel).

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

1. **The merge queue is where retry pays most and where the only product on the same API refuses to go.** So R12 leaves `merge_group` out of the default scope and the design session decides.
2. **Classification is the idea G.17 lacked, and GAG is already good at it.** Zuul retries only setup and infrastructure failures; Jenkins' `kubernetesAgent()` retries a lost pod but not `OOMKilled`.
   GAG's disruption arms are the same idea, and phase 2 should widen them before it retries anything unclassified ([R12a](#retry-phase-2)).
3. **A global rate cap belongs beside the per-run cap** (Kubernetes' one per 20 minutes, Gradle's `maxFailures`); R11 now says so.
4. **The 50-re-run cap is shared with humans**, and a re-run by the AGC runs with the original actor's privileges, which the existing disruption path already accepts.

## Requirements

MUST and SHOULD are used in their usual sense.
Each requirement is written to be testable.

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

### Retry (phase 2)

- **R7.** Retry MUST be off by default and enabled per `RunnerSet`, by its owner, never by anything a workflow emits.
- **R8.** Retry MUST fire only on `result == failed`.
  It MUST NOT fire on succeeded, cancelled or skipped jobs, nor on a run a user cancelled.
- **R9.** Retry MUST be bounded per run by a cap that survives an AGC restart.
  The default cap SHOULD be one automatic retry.
  Capping on GitHub's `run_attempt` meets this without new state, and keeps the AGC well inside GitHub's 50-re-run limit, which humans share.
- **R10.** Flake retries MUST have their own budget and their own `cause` value, so the disruption budget is neither spent by flakes nor able to stand in for them.
  Whether a combined lifetime cap also applies is an [open question](#open-questions-for-the-design-session).
- **R11.** Retry MUST suspend itself when the set-wide failure rate over a window passes a threshold, report that as a condition on the `RunnerSet`, and resume without an operator.
  This is the stampede guard for correlated failure.
  A per-set rate cap on retries SHOULD sit beside it, so a burst of individually eligible runs cannot all retry at once.
- **R12.** Retry MUST be scoped by event type, defaulting to `pull_request`; whether `merge_group` joins the default is [open](#open-questions-for-the-design-session).
  Other events, `push` to a default branch above all, MUST be an explicit further opt-in, because those are where deploy and publish jobs live.
  A repository or workflow filter SHOULD be available.
- **R12a.** Phase 2 MUST first add retry for newly classified platform failures, each recognised from facts GAG owns and shown to be temporary and self-resolving, before it retries failures nobody has classified.
  A new class joins the existing re-run arms and their budget, so it is on by default like they are; a blanket retry of unclassified failures stays opt-in (R7).
  Candidates, none yet measured to be distinguishable: a job that failed during setup (image pull, runner registration, action download) before its first step; a job that failed while its egress proxy pool or registry mirror was reporting unavailable; a node that went `NotReady` under a running worker.
  `OOMKilled` is not a candidate: the job's own memory use is the tenant's to fix, which is the line Jenkins draws.
- **R13.** Every automatic retry MUST be counted and evented, and a retry that then passes MUST be counted as a detected flake under R1.
  A retry may never make a flake less visible to the operator than it was before.
- **R14.** Retry MUST reuse the existing paced re-run path (`rerunUntilAccepted`), so it inherits the wait for run conclusion, the refusal handling, and the API budget.
- **R15.** Retried compute MUST be attributable per set, so the [cost-attribution](../operations/cost-attribution.md) story gains a flake-spend line.
- **R16.** Retry MUST need no GitHub App permission beyond what disruption recovery already uses.

## Scope

**In scope:**

- The scale-set tier.
- Job-granular detection and run-granular retry through the existing re-run machinery.
- New classified platform-failure classes on the existing re-run arms (R12a).
- An opt-in on the `RunnerSet` for unclassified failures, with bounds, scope filters and a circuit breaker.
- Metrics, Events and a condition for the operator.

**Out of scope, and why:**

| Out | Why |
|---|---|
| The classic tier | It never receives `JobCompleted`, and it is terminal |
| Test-level retry or quarantine | Needs the test framework; Datadog, Trunk and Develocity own that layer |
| Failure-reason matching (exit codes, log patterns) | Ship with budgets and scopes first, as G.17 already says; a taxonomy needs data detection will produce |
| A signal from inside the job asking for retry | G.17's channel analysis stands; it gives up the "no workflow edits" property and needs its own design |
| Writing back to GitHub (check runs, PR comments) | Needs new App permissions (R16); GitHub's own "Attempt #2" label is the tenant-visible trace for now |
| Known-flakes-only retry | Needs the history phase 1 collects; revisit once it has some |
| Merge queue reordering or re-enqueue | The queue is GitHub's; GAG re-runs the job and lets the queue react |

## Open questions for the design session

1. **Where pairing state lives.** In memory loses pairs on restart (R5 accepts the undercount); a ConfigMap shares the write-contention problems the recovery claims already hit (Q1108); asking GitHub for the previous attempt's jobs is stateless but costs a call per re-run.
2. **Run-granular or job-granular re-run.** Both endpoints wait for the run to conclude and both re-run dependents, so timing does not decide it.
   `rerun-failed-jobs` re-runs every failed job, plus siblings fail-fast cancelled; `jobs/{id}/rerun` re-runs one, but needs the job's numeric id, which `JobCompleted` does not carry (it has a UUID), so it costs a jobs-list call per retry.
3. **Merge queue.** It is where a flake costs most, since an ejection rebuilds every group behind it, and where Mergify, the nearest product on the same API, declines to retry.
   In the default scope or not?
4. **Which platform classes can be told apart.** R12a's candidates rest on boundaries nobody has measured: whether the worker can report that it failed before the first step, and whether a proxy or mirror outage can be tied to the jobs it broke.
   Each candidate needs a dogfood measurement before it becomes a class.
5. **One lifetime cap or two.** Should a run that is both evicted and flaky be bounded by the sum of the two budgets, or by a single combined cap?
6. **The CRD shape**, via [api-review.md](../development/api-review.md), including whether detection's default-on needs a field at all.
