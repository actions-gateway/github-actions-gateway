# Eviction and Oversubscription Validation

Turn the eviction-recovery and oversubscription capabilities from arguments into measurements.
Five experiments, sequenced by what blocks what.

Filed 2026-07-26 from [issue #819](https://github.com/actions-gateway/github-actions-gateway/issues/819), which proposed the five and asked for triage.
This plan is the reviewed version: two of the five changed shape under review, and the corrections are recorded below with the evidence that produced them.

## Why this matters

Both capabilities are currently argued rather than measured.
The recovery story is design prose in [01-executive-summary.md](../design/01-executive-summary.md) and the flows doc; the oversubscription story rests on `priorityTiers` doing what the design says it does.
The planned blog post ([go-to-market.md](go-to-market.md), "Recovering stuck Actions jobs after pod eviction") can state the design but cannot state a result.

The one number available is confounded.
Per [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#the-baseline-number-is-confounded), the U5 probe's ~9.5 minutes coincided with the job's 10-minute `timeout-minutes` boundary, leaving lock TTL, GitHub's liveness detection, and the workflow timeout indistinguishable.
Any published latency figure needs a clean run with no `timeout-minutes` set.

## What is already tracked

| Row | Relationship to these experiments |
|---|---|
| Q396 | **Is** experiment 1. Already covers both tiers as of #815; only the retry-budget assertion is additive. |
| Q417 | **Shipped 2026-07-26.** Was the hard prerequisite for the scale-set half of 1, and for 3 and 5: `ProvisionScaleSetWorker` is fire-and-forget, so scale-set evictions were never detected. Detection now runs from the owning reconciler off the worker pod ([scaleset-eviction-recovery.md § Phase 2 as built](scaleset-eviction-recovery.md#phase-2-as-built)). All three are unblocked. |
| Q419 | **Shipped 2026-07-26** with Q417 — the docs half of the same gap. The tier-agnostic claims in the exec summary, README, and why-gag are now true of both tiers rather than needing a qualification. Independent of these experiments. |
| Q420 | **Shipped 2026-07-26**, ahead of Q417 and independently of it — the reap deadline came from a pod annotation, not a pod watch. Orphaned Running workers would otherwise have contaminated 3 and 5 by holding quota, which is exactly the idle-capacity signature those experiments measure. |
| [Q418](../queue/Q418.md) | Deferred, event-gated on experiment 1 attributing the delay. |
| [Q459](archive/q459-drained-worker-recovery.md) | **Filed by experiment 2**, 2026-07-27. Its residual: neither tier recovers a drained worker, and whether that matters turns on what GitHub does with the runner's own relayed report — a live-GitHub question. Both halves measured 2026-07-29; decided **close, gated on `deletionTimestamp`**, and Q502 shipped that implementation on both tiers. |

## Experiment 1: mid-job eviction latency, both tiers (Q396)

Evict a worker mid-build with no `timeout-minutes` set; timestamp the kill and GitHub's conclusion; assert the rerun fires and the per-run retry budget decrements exactly once.

**Correction under review.** Issue #819 framed this as "extend Q396 to scale-set".
That already happened: #815 widened Q396's scope to both tiers on 2026-07-25, and [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#phase-1-measure-the-real-baseline-gate) Phase 1 records the same.
The genuinely additive assertion is the retry budget (the Q106 sharded-reservation invariant), which the row did not name.

- **Venue:** live-GitHub on kind, per the row.
- **Proves:** the real eviction-to-conclusion latency, attributed to a mechanism.
- **Unlocks:** a defensible number in place of the confounded one, and the [Q418](../queue/Q418.md) trigger.
- **Unblocked** — Q417 shipped 2026-07-26, so the scale-set tier now detects evictions and fires the rerun this experiment measures.

### Result, measured 2026-07-29

Live-GitHub tier on kind, against `actions-gateway/gateway-test` ([run 30467282642](https://github.com/actions-gateway/gateway-test/actions/runs/30467282642)), by `E2E_GitHub_EvictedWorkerLatencyAndRerun` in [`github_e2e_test.go`](../../cmd/gmc/test/e2e/github_e2e_test.go).
A real runner was executing a real job — GitHub reported it `in_progress` before anything was touched — the fixture carries no `timeout-minutes`, and the worker was evicted by the kubelet.

| Observation | Value |
|---|---|
| Worker pod phase/reason | `Running/` → `Failed/Evicted` |
| Kubelet message | `Pod ephemeral local storage usage exceeds the total limit of containers 256Mi.` |
| Runner container exit code | **137** — SIGKILL |
| Job conclusion on GitHub | **`failure`** |
| **Eviction → conclusion** | **9m36s** (`finishedAt=15:44:17Z` → `completed_at=15:53:53Z`, both server-side) |
| AGC decision | `pod evicted; scheduling auto-retry` … `"attempt":1,"tier":"classic"` |
| Re-run outcome | **`403 This workflow is already running`** |

**The confound is gone and the number survives it.** 9m36s is close to the U5 probe's ~9.5 minutes, so that figure was accidentally right about magnitude — but it is only now attributable.
With no `timeout-minutes` in play, the only mechanism left that can end the job is GitHub's own detection of a lock that stopped being renewed, and the design's "at worst ~10 minutes from the last renewal" is what the measurement lands on.
**Quote it as "about 9–10 minutes, bounded by the job lock's TTL", not as the workflow timeout it used to be confused with.**

**The headline finding is not the latency.
It is that classic-tier eviction recovery never actually recovers the job.** The AGC waits `evictionRetryDelay` (default 5s) after seeing the eviction and then calls `rerun-failed-jobs` — which, per the line above, lands ~9.5 minutes *before* GitHub concludes the run.
GitHub refuses it:

```
rerun API returned 403: {"message":"This workflow is already running", ...}
```

So on the then-shipped default the sequence was: budget slot reserved, `actions_gateway_eviction_retries_total` incremented, re-run refused, job left failed.
The metric an operator would watch said recovery happened; nothing was recovered.
This is exactly the question [04-operational-flows.md §4.2](../design/04-operational-flows.md) flagged as unmeasured — "whether the rerun call succeeds while the run is still winding down inside that window" — and the answer is no. Q503 carried the fix; see the update below.

**Update, 2026-07-30 — Q503 fixed, Q510 flipped the spec.** The AGC now treats the `403 This workflow is already running` refusal as "not yet": the re-run is retried every 30 seconds inside a 15-minute window (sized past the ~10-minute lock-TTL bound this experiment measured), on a context detached from the job goroutine so neither the classic TaskResult nor a reconcile is held for the wait, and the whole refusal-spanning recovery still costs one slot of the per-run budget.
A re-run that never lands is no longer silent: `actions_gateway_eviction_rerun_failures_total` (reasons `run_never_concluded`, `api_error`) and an `EvictionRerunFailed` Warning Event name the run needing a manual re-run.
The spec flipped with the fix (Q510): a refused re-run now FAILS `E2E_GitHub_EvictedWorkerLatencyAndRerun` — the outcome `switch` that recorded a refusal as a report entry and passed is gone, the identity-unknown branch fails too (Q495 is fixed, so it is a regression now), and the conclusion wait pins **attempt 1**, which the accepted re-run's second attempt would otherwise displace from the `filter=latest` jobs listing mid-measurement.
Verified at the unit and envtest tiers against the measured refusal body; the live-GitHub re-validation rides the next run of this spec (live-GitHub is a singleton tier and out of scope for the fixing session).
The kind tier now reproduces the refusal too (Q517): fakegithub's `/control/runstate` gate answers the measured 403 until a spec concludes its run, and `E2E_AGC_PreemptedWorkerIsRecovered` drives the refused-then-accepted sequence — closing the fidelity gap that let the fire-once recovery pass every fake-backed tier while failing here.

**Why the runner could not report, unlike Q459's drained worker.** The wrapper's relay *did* fire — `forwarding termination signal to child`, `grace: 25s`, and the runner logged `Runner will be shutdown for UserCancelled` — but the kubelet's eviction gave it about two seconds before SIGKILL, and the runner was still inside `Waiting for process exit or 7.5 seconds after SIGINT` when it died.
The report never left.
That is the whole difference between this experiment and Q459's, and it is what the two numbers measure:

| Disruption | Grace | Runner reports? | Eviction → conclusion |
|---|---|---|---|
| Graceful delete / drain ([Q459](archive/q459-drained-worker-recovery.md)) | 30s | yes | **15–26s** |
| Kubelet eviction (this experiment) | ~2s, then SIGKILL | no | **9m36s** |

**The scale-set half followed on 2026-08-03; see the result below.** The 403 was argued here to be tier-independent by construction — a property of the API and of the delay, not of how the AGC detected the eviction — and the measurement agrees, but it is worth noting that the argument was not what settled it.

### Scale-set result, measured 2026-08-03

Live-GitHub tier on a throwaway kind cluster, against `actions-gateway/gateway-test` ([run 30857541535](https://github.com/actions-gateway/gateway-test/actions/runs/30857541535)), by `E2E_GitHub_ScaleSetEvictedWorkerLatencyAndRerun`.
Every image came from the published `v1.3.0` release, so this result is citeable against a version an operator can pin; the harness and pins are described in the section below.

A ScaleSet-protocol RunnerSet registered a scale set against the fixture repo, GitHub routed the job to its label, and a real runner executed it before anything was touched.

| Observation | Scale-set | Classic (same day) | Classic (2026-07-29) |
|---|---|---|---|
| Worker pod phase/reason | `Running/` → `Failed/Evicted` | same | same |
| Runner container exit code | **137** | 137 | 137 |
| Fill → kubelet eviction | 15s | 13s | ~55s |
| Job conclusion | **`failure`** | `failure` | `failure` |
| **Eviction → conclusion** | **9m38s** | 9m37s | 9m36s |
| **Re-run calls before acceptance** | **20** | 20 | n/a — fired once, refused |

**The scale-set tier reproduces the classic behaviour — when the runner's report does not escape.** On this run it did not: 9m38s to conclusion, within two seconds of both classic measurements, and 20 paced calls before GitHub accepted the re-run.
The 403 refusal window is real on this tier, so the retry loop is load-bearing here and not merely inherited.

**But a second run of the same spec, on the same build, disagreed — and that is the more important result.** Re-run 2026-08-03 inside the full container, it saw:

| Observation | This run | The run above |
|---|---|---|
| Runner container exit code | 137 | 137 |
| Pod phase/reason | `Running/` → `Failed/Evicted` | same |
| Job runtime before conclusion | **17s** | ~9.5 min |
| Eviction → conclusion | **−1s** (GitHub concluded *before* the kubelet's recorded exit) | 9m38s |
| Re-run calls before acceptance | **1** | 20 |

17 seconds is the *graceful* path's signature — [Q459](archive/q459-drained-worker-recovery.md) measured 15–26s for a drained worker whose runner does report — so the likeliest reading is that this eviction gave the runner enough time to get its report out, and GitHub concluded on the report rather than on the lapsed lock.
Exit 137 does not contradict that: a runner that reports and then overruns its grace is still SIGKILLed.

**That reading is a hypothesis, not a measurement.** The captured worker log ends mid-stream in both runs and shows no SIGTERM relay line either way, and the failing run's AGC log was not captured at all — the container's failure hook dumps the *classic* tenant's AGC, which is a gap in the harness rather than in the product.
What would settle it: capture the scale-set tenant's AGC log and the worker's full log on failure, then run the spec enough times to see how often each outcome occurs.

**Q657 did that, and the hypothesis did not survive it.
See [the re-measurement below](#the-17s-outlier-is-unattributable-q657-2026-08-03).**

**What this does and does not license.** The ~10-minute figure stands for the case the design cares about — nothing reported, GitHub notices by itself — and is now measured on both tiers.
It must not be quoted as "what an eviction costs".
The spec's `rerunCalls >= 2` assertion encodes the stronger claim and is what failed here; it is correct as a description of the ungraceful path, and the assertion is now conditioned on the measured latency rather than applied to every eviction.

**The detection path is genuinely the scale-set one.** The refusals log `cause=eviction` against `owner=set-ss` and a `runner-set-ss-…` pod, so it is the owning reconciler's worker-pod watch (Q417) that saw the disruption, not a classic job goroutine.
The budget was reserved once, at `tier="scaleset"`.

**What this run established for the first time, incidentally.** The AGC's scale-set listener had never bootstrapped against real GitHub from inside a cluster — `cmd/probe` Investigation E drives the same wire protocol from a standalone binary, which is a different question.
It registered the scale set, opened its session, took the assignment, minted a JIT config and provisioned a worker that GitHub then reported running the job.
The pre-registered runner name was `real-ag-ss-de2978a1-838b-59ae-a1fc-5dcd47d793db`, matching `listener.runnerName`'s `<scaleSetName>-<jobID>` exactly.

### The 17s outlier is unattributable (Q657, 2026-08-03)

Q657 set out to quantify how often each outcome occurs and then qualify the design claim.
It did neither, because the first thing the new instrument found was that the spec could not prove *which worker* it had evicted — and the 17s observation is the run that most needs that proof.

**The instrument.** Both eviction specs now record, per run: the worker log from two captures that fail in different ways, GitHub's per-step records for the interrupted attempt, and one classified outcome line.
The scale-set spec adds the `job-completed-at` annotation its listener stamps and an unfiltered AGC log tail.
Same `v1.3.0` image pins as the runs above, on a throwaway kind cluster.

**Attempt 1 evicted a worker that had no job.** The AGC log shows the listener provisioned two workers ten seconds apart: one for jobID `22463488…` at 23:59:05 — whose job Secret it reclaimed and whose job-completion it stamped in the same second — and one for jobID `cec0e443…` at 23:59:15, after the dispatch.
GitHub ran the job on the first (`runner_name: real-ag-ss-22463488-…`).
`scaleSetWorkerForRun` matched the second on its `run-id` annotation and returned it, because that lookup takes whichever candidate the API lists first.

Nothing downstream noticed.
The AGC detected the eviction and attributed it correctly — `runID 30864091648`, `tier=scaleset`, `attempt 1`, `cause=eviction` — and the only tell was the runner exiting **143** rather than 137: it was still `Listening for Jobs`, so it shut down inside its grace period.
What that run timed was the cost of killing an idle runner.

**So the harness gained an identity check**, and it is GitHub's `runner_name` that supplies it — the only source that knows which runner took the job.
The scale-set runner is named `<scaleSetName>-<jobID>` and the worker pod embeds the same jobID, so the two reconcile before the spec touches the storage limit.

**Attempt 2, with the check in place, measured the ungraceful path cleanly** ([run 30864859954](https://github.com/actions-gateway/gateway-test/actions/runs/30864859954)):

| Observation | Value |
|---|---|
| Worker pod phase/reason | `Running/` → `Failed/Evicted` |
| Kubelet message | `Pod ephemeral local storage usage exceeds the total limit of containers 256Mi.` |
| Runner container exit code | **137** |
| Fill → kubelet eviction | 8s |
| Job conclusion | **`failure`** |
| **Eviction → conclusion** | **9m45s** (`finishedAt=00:13:21Z` → `completed_at=00:23:06Z`) |
| Re-run calls before acceptance | **21** |
| Step records, attempt 1 | `1. Set up job=completed/success`, `2. Hold the job open=`**`in_progress/null`** |
| Verdict | `lock-lapsed` |

**The step records are the discriminator, and they are the durable result of this work.** On the lock-lapsed path GitHub holds the job at `completed/failure` while the step it was interrupted in stays frozen at `in_progress/null` — the runner never posted that step's end.
It is a per-run, log-independent read of "did the runner report?", and it survived every capture failure below.
The shape a *reported* loss leaves is still uncaptured.

**Where that leaves the claim.** Four scale-set attempts now exist: 9m38s, 17s, one that evicted the wrong worker, and 9m45s.
Three of the four are consistent with the lock TTL being the only mechanism.
The 17s run is the sole evidence for "a kubelet ephemeral-storage eviction is not reliably ungraceful", and it predates the identity check, so it cannot be said to have evicted the runner that was executing the job.
**Treat that claim as unsupported rather than established** — it is not refuted either, because attempt 1 exited 143 where the 17s run exited 137, so "it evicted an idle worker" does not fully account for it.
Quoting ~10 minutes for the case where nothing is reported remains correct and is now backed by three clean observations across both tiers.

**Two capture defects the runs found in the instrument itself, both fixed.** Recording them because each one produced a plausible-looking false reading:

- **A capture reported late is a capture lost.** Attempt 1 read the evicted worker's log and then discarded it: the `exitCode == 137` assertion fires before the verdict is written.
  Diagnostics now report at the point of capture.
- **A failed read is not a silent runner.** On attempt 2 the post-hoc `kubectl logs` came back `unable to retrieve container logs for containerd://…` — the kubelet had already reclaimed the container — and kubectl still exited 0.
  The first version of the outcome line rendered that as a measured `relayRan=false`.
  It is now three-valued, with `logsRead` naming which captures answered.
  The follower is the stronger of the two sources here, streaming to within a second of the container's `finishedAt`; the post-hoc read is corroboration.

**What would settle the split**, and what Q657 did not buy: repeated runs *with* the identity check, enough of them to see whether a fast conclusion ever occurs on a worker confirmed to be executing the job.

### The second worker was not a replay (Q661, 2026-08-04)

Attempt 1's two workers were filed as a listener defect on the reading that a replayed queue message had built the second one.
The two identifiers the capture recorded refute that on their own, before any code is read.

**Both identifiers are jobIDs, and they are different jobs.** `22463488…` and `cec0e443…` look like different shapes only because the first happens to be all decimal digits; both are the first group of a UUID.
The runner name is the proof: the listener composes it as `<scaleSetName>-<jobID>`, and this document records one in full from the same tier: `real-ag-ss-de2978a1-838b-59ae-a1fc-5dcd47d793db`.
So `real-ag-ss-22463488-…` is a whole jobID with its tail elided, not a numeric id of some other kind.

**A replay cannot produce two jobIDs.** Redelivery is keyed on the jobID at both seams: the listener's `provisioned`/`completed`/`abandoned` sets in [listener.go](../../cmd/agc/internal/scalesetlistener/listener.go), and the provisioner's Secret and pod names, which are derived from the jobID and treat `AlreadyExists` as success.
The same jobID arriving twice is a no-op even across a process restart, where the listener's in-memory sets are empty and the deterministic names are the only guard left.
`oversubscribe_q661_test.go` pins both halves against one queue log, and deleting the listener's jobID guard turns three deliveries of one assignment into three workers, which is the shape the row described.

**So GitHub assigned two distinct jobs carrying `workflowRunId` 30864091648, and provisioning both is the contract.** A workflow run has many jobs; the listener is handed one assignment per job and must build one worker per assignment.
The run id was never a worker identity, which is exactly the defect Q657 found and fixed in the harness.
The listener has no defect to fix here, and a run-scoped dedup would be a bug.

**What is still open is on GitHub's side**, not the AGC's: why the second job's runner never received a job.
That is the Q420 class (an assignment that lapses leaves a worker at `Listening for Jobs`), whose only bound when no terminal `JobCompleted` follows is `maxWorkerLifetime`.
The capture cannot say more, because the raw AGC log was not kept and the surviving prose cannot separate "the first job's Secret was reclaimed in the second it was provisioned" from "reclaimed later, in the same second as its completion stamp".
The first would be a real anomaly against GitHub reporting that same runner ran the job to completion; the second is the ordinary lifecycle.
**Settling it needs a fresh capture, not more reading**: the scale-set tenant's unfiltered AGC log retained as a run artifact, read against the per-job records.
The provisioning log line now carries `runID` alongside `jobID` so that capture can be read without reconstructing the pairing from pod annotations.

### The scale-set half: how it was measured

The harness behind the result above, and the two verifications that rode the same work.

**The run is pinned to published images, so the result can be cited.** The 2026-07-29 measurement is quotable for latency but not for recovery, because its images came from `719e67f1` and predate the Q495 fix — the defect and the build are inseparable in it.
`v1.3.0` (tagged 2026-08-03) carries both behaviours under measurement, verified by content rather than by SHA: `rerunUntilAccepted` in [eviction.go](../../cmd/agc/internal/provisioner/eviction.go) (Q503) and the `contextData.github.run_id` extraction in [payload.go](../../cmd/agc/internal/provisioner/payload.go) (Q495).
All five images are published at that tag, so the run sets `GMC_IMG`/`AGC_IMG`/`WORKER_IMG`/`WRAPPER_IMG`/ `PROXY_IMG` to `ghcr.io/actions-gateway/<name>:v1.3.0` and lets the specs compile from the branch.
Test code does not ship in the image, so the new assertions run against released binaries.

Two deltas belong in the result rather than in the setup: `fakegithub` is test-only and stays locally built (the live-GitHub arm does not use it), and the chart is `v1.3.0` plus the v1alpha1 deprecation annotation on two CRDs (#1199), which is an apiserver warning and not behaviour under measurement.
A defect found on the scale-set arm would move that arm's pin to whichever release ships the fix, leaving the classic arm's pin where it is.

**Two verifications rode the same work, and both are now closed.** Neither needed a tier of its own, so both became assertions on the classic spec:

- **Q503 — verified, 20 calls.** The retry loop shipped 2026-07-30 (#1010) and `E2E_GitHub_EvictedWorkerLatencyAndRerun` already failed a refused re-run (Q510), but it could not separate "the loop outlasted GitHub's refusal window" from "GitHub accepted the first call" — a fire-once recovery would have passed on any run GitHub happened to have concluded.
  `rerunUntilAccepted` logs `rerunCalls` on the acceptance line ([eviction.go](../../cmd/agc/internal/provisioner/eviction.go)), so the spec now reads that count and requires at least one absorbed refusal.
  Measured 2026-08-03: **20** on the classic tier and **20** on scale-set, against a ~9.5-minute conclusion at a 30-second retry interval.
- **Q544 — verified.** The `run-id` and `repository` annotations were both present on a real worker at live-GitHub, on a `v1.3.0` build.
  The spec no longer accepts their absence: worker lookup matches on the annotation and nothing else, so resolving at all proves `run-id`, and `repository` is asserted alongside it because the two arrive from the same payload context and `rerun-failed-jobs` needs both.

  Making that an assertion immediately caught a second defect, in the harness rather than the product.
  The lookup used to fall back to "the sole Running worker that was not there before this spec dispatched", which was correct only while Q495 left the annotation absent.
  Once these specs began triggering re-runs — all of them now do — an earlier spec's second attempt could provision a worker mid-spec and make someone else's pod look fresh.
  On 2026-08-03 the cancel-path spec dispatched run `30856065695` and was handed a worker annotated `30856024324`; before the assertion it would have cancel-tested a run it never dispatched, and passed.
  Freshness is gone as an identity signal.

**The scale-set arm needs a tenant that has never existed.** The live-GitHub suite runs one classic v1 tenant.
`E2E_AGC_ScaleSetAcquisition` and `E2E_AGC_ScaleSetRecovery` both run against fakegithub, and `cmd/probe` Investigation E drives the scale-set wire from a standalone binary rather than from a deployed AGC.
So this measurement stands up a v2 object set — `ActionsGateway`, `RunnerTemplate`, ScaleSet-protocol `RunnerSet` — with `githubURL` naming the fixture repo directly, in its own namespace, inside the existing `github-real` `Ordered` container (a second top-level container would run concurrently with it, which is the Q511 collision inside one process).

Three constraints shape it:

1. **The scale-set name is the RunnerSet's single `runnerLabels` entry** ([runnerset_scaleset.go](../../cmd/agc/internal/controller/runnerset_scaleset.go)), and CEL enforces exactly one.
   So the fixture workflow's `runs-on` has to be that label, and it cannot be `e2e` without colliding with the classic tenant's runner group.
2. **The fixture workflow takes `runs-on` as a `workflow_dispatch` input.** `drain-probe.yml` in `actions-gateway/gateway-test` pinned `runs-on: e2e`; it now takes an input defaulting to `e2e`, so the classic measurement is unchanged and one fixture serves both tiers.
   This is also the change [Q530](../queue/Q530.md) names as its prerequisite for live-run isolation, so that row is partly unblocked.
3. **The listener's bootstrap is asserted before anything is dispatched.** A tenant that never registers its scale set would otherwise surface as "the job stayed queued for ten minutes" — indistinguishable from GitHub being slow, from a label mismatch, and from the runner group's public-repository rule.
   `Listener.Start` publishes `Degraded=False/SessionAuthorized` only after ensuring the scale set *and* opening the session, so that condition is checked first and its failure dumps every condition on the RunnerSet.

The eviction arm then mirrors the classic spec: overshoot the runner container's ephemeral-storage limit, wait for `Failed/Evicted`, measure the kubelet's `finishedAt` against GitHub's `completed_at`, and assert one budget slot reserved at `tier="scaleset"`, a re-run accepted after more than one call, and a second attempt created.

**The risk that did not materialise.** The AGC's scale-set listener had never bootstrapped against real GitHub from inside a cluster — Investigation E proves the protocol, not the deployed path — and the likeliest failure was registration scope, since these probes register against the *repo* rather than the org precisely because the org's `Default` runner group sets `allows_public_repositories: false`.
Pointing `githubURL` at the fixture repo was sufficient: it registered, sessioned, and acquired on the first live attempt.

**What cost two runs instead was the container's shape**, and both causes are now fixed in the harness rather than worked around:

- The container's `BeforeAll` strips the GMC's `AGC_EXTRA_*` fakegithub overrides cluster-wide, so the rest of the suite cannot run beside it — five fakegithub-backed specs timed out unable to register a session.
  `SUITE=live-github` selects the label.
- `--timeout` is a whole-suite budget and 30m does not fit an `Ordered` container whose specs wait out two ~10-minute conclusions; a run was interrupted in the sixth of seven specs.
  `SUITE=live-github` raises it to 90m.
- Ginkgo skips the remainder of an `Ordered` container after a failure, so this spec being declared last made every spec ahead of it a gate — it lost two full runs that way at ~55 minutes each.
  It now carries a `scaleset-live` label (`SUITE=live-github-scaleset`) so the measurement can be retaken alone.

### What the harness cost to build, and why it is shaped this way

**The eviction lever works, and it is the only aimed one.** Eviction recovery keys on `PodFailed` with reason `Evicted` and nothing else, so the disruption has to produce exactly that shape.
Q421 already ruled out the graceful removals.
Node-wide memory or disk pressure produces the shape but lets the kubelet choose the victim, on a node shared with the rest of the suite.
A **pod-level `ephemeral-storage` limit** is enforced per pod, and overshooting it was measured on the e2e kind cluster to do exactly what is needed:

| Observation | Value |
|---|---|
| Pod phase/reason after the overshoot | `Failed/Evicted` |
| Kubelet message | `Pod ephemeral local storage usage exceeds the total limit of containers 16Mi.` |
| Container exit code | `137` — SIGKILL, so the kubelet took the container rather than the job ending on its own |
| Write → kill | ~55s, the kubelet's local-storage housekeeping cadence |

The near-zero grace is the point rather than a side effect: it is what usually makes this the *ungraceful* case, where GitHub must notice by itself.
The graceful counterpart, where the runner reliably gets its own report out, is [Q459](archive/q459-drained-worker-recovery.md)'s.

**"Usually" is doing real work in that sentence, and this section originally read "always".** It inferred from the 137 exit that no SIGTERM was relayed and nothing reached GitHub.
The exit code does not carry that: a runner that reports and *then* overruns its grace is SIGKILLed too, and the 2026-08-03 scale-set pair exited 137 on both sides of a 9m38s/17s split.
What the runner managed to say is now read from its own log and from GitHub's per-step records rather than inferred from the exit code — see [Q657](../queue/Q657.md) and the scale-set result above.

**Sizing the cap needed a measurement of its own.** The kubelet charges a pod only its writable layer, emptyDirs and logs — image layers are read-only and are not charged — and a pod built from the real runner image was measured at **28KiB** against the node's `stats/summary` endpoint.
The e2e fixture jobs add nothing to that: neither checks out a repository.
256Mi is therefore four orders of magnitude of headroom, which is what makes the deliberate overshoot the only thing that can cross it.

**Q495 was confirmed here, by direct observation rather than inference — and has since been fixed** ([#967](https://github.com/actions-gateway/github-actions-gateway/pull/967)).
A worker pod provisioned for a real GitHub job on the classic tier carried `actions-gateway.com/job-name` and **neither** `run-id` **nor** `repository`:

```
annotations: {"actions-gateway.com/job-name":"hold",
              "cluster-autoscaler.kubernetes.io/safe-to-evict":"false",
              "descheduler.alpha.kubernetes.io/prefer-no-eviction":"true",
              "karpenter.sh/do-not-disrupt":"true"}
```

`jobMetaFrom()` and `repoInfo()` read the same two payload fields, so an absent annotation means `runID` is `"0"` and [`handleEviction`](../../cmd/agc/internal/provisioner/eviction.go) returns at its first line.
Note also that `system.github.job` *did* arrive: the acquisition payload's `variables` map is populated, and it is specifically the run-identity keys that were missing — which narrowed Q495 from "the payload is empty" to "these keys are not where we read them".
That narrowing is what the fix acted on: run identity travels in the serialised `github` context (`contextData.github.run_id`), not in the job variables, and the worker pods this experiment provisions now carry their `run-id` annotation.

**What that cost this experiment, and no longer does.** The latency half was never affected — it measures GitHub's own detection of a runner that stopped answering, which does not involve the AGC at all.
The "assert the re-run fires" half, and the Q106 budget assertion behind it, could not fire at all on the classic tier until Q495 landed.
The spec is written to hold either way: it asserts that the AGC *saw* the eviction and reached a decision — the assertion that separates "recovery declined to act" from "detection never happened" — and asserts the budget invariant only on the branch where recovery ran.
On a post-Q495 build that is the branch it takes.

### live-GitHub does not parallelize, and the reason is not the cluster

Two sessions ran live-GitHub on 2026-07-29 from separate worktrees and separate kind clusters, and still collided.
Cluster isolation does not help, because the shared resources are on GitHub's side:

- **One fixture repo and one workflow.** Both sessions dispatch `drain-probe.yml` to `actions-gateway/gateway-test`.
  `dispatchAndResolveRun` identifies its run as "the one that was not there before" — which is the *other* session's run when two dispatches land seconds apart.
- **One `runs-on` label, one org.** Every live-GitHub tenant registers runners labelled `e2e` in the `actions-gateway` org, so GitHub may route either session's job to either cluster's gateway.
  The two are entangled even when the clusters are not.
- **No run-id annotation to disambiguate by** — Q495, since fixed, but absent for these runs — so `runningWorkerForRun` fell back to "the sole Running worker" and gave up when there were two.

A throwaway cluster per run, which [testing.md](../development/testing.md) now prescribes, is still the right move: it removes the *other* half of the collision, where a parallel session's `helm upgrade` and `kubectl set env` fight over one GMC.
It just does not make two live-GitHub runs independent.
Both halves have to hold, so treat live-GitHub as a **singleton**: one session at a time, across all worktrees, each on its own cluster.
The GitHub-side half is settled in [q511-live-github-run-isolation.md](q511-live-github-run-isolation.md): the suite's `BeforeAll` now refuses to start while the fixture repo is not idle.

This is the same contention that kept `E2E_GitHub_CancelledRunLeavesNoDeletionMark` pending in [q459-drained-worker-recovery.md](archive/q459-drained-worker-recovery.md), seen from the other side — there between specs, here between sessions.

**A related hazard, learned expensively.** A live-GitHub run killed mid-spec leaves its tenant namespace `Terminating` on an `agentpool-cleanup` finalizer that only its own AGC can clear — and the AGC's Deployment goes away with the namespace, so it never clears.
Force-removing the finalizer unblocks the namespace but **skips the deregistration of that tenant's runners from the org**.
Those stale registrations keep taking job assignments, so the next run's job goes `in_progress` against a runner that no longer exists and no worker pod is ever provisioned.
Prefer stopping a run with SIGTERM and letting Ginkgo's `AfterAll` run: it deletes the `ActionsGateway` CR while the AGC is still up, which is what lets the finalizer do its job.

## Experiment 2: the node-drain path (Q421)

**Done 2026-07-27** — jump to the [Result](#result-measured-2026-07-27).
The heading keeps its original slug because several docs link to it.

Cordon and drain a node mid-job.
Assert what the wrapper, the runner, the provisioner, and GitHub each do.

**Correction under review, and the reason this experiment got more valuable.** Issue #819 predicted "a measured zero" on the grounds that this is the good path and [Q385](https://github.com/actions-gateway/github-actions-gateway/pull/747)'s SIGTERM relay already covers it.
Reading the code suggests the drain path does not reach eviction recovery at all:

- `kubectl drain` uses the Eviction API, which **deletes** the pod.
  A deleted pod never lands in `PodFailed` with `reason: Evicted`; that phase comes from kubelet-initiated node-pressure eviction.
- [`waitForPodCompletion`](../../cmd/agc/internal/provisioner/completion.go) treats a pod that has vanished as `PodSucceeded`.
- [`provision`](../../cmd/agc/internal/provisioner/provisioner.go) calls `handleEviction` only on `phase == PodFailed && reason == "Evicted"`.

So on classic, a drained worker most likely reports its own terminal result via the relay, the provisioner records success, and nothing reruns.
Q417's scale-set detection (shipped 2026-07-26) reaches the same conclusion by construction: it fires only on `PodFailed`/`Evicted`, and deliberately excludes deletion on the grounds that the SIGTERM relay already owns that case.
This experiment is what tests that reasoning.

This is a code reading, not a measurement, and [testing.md](../development/testing.md#diagnosing-failures-measure-before-asserting-a-root-cause) is explicit that a symptom match is a hypothesis until the failing system is measured.
The experiment is what settles it.
If it holds, the outcome is a recovery gap on the *graceful* path, which is worth more than confirming a zero.
Q417 shipped without covering deletion, on the reasoning above; a finding here that a drained worker does **not** report its own result is the evidence that would reopen that decision on both tiers.

Assertions:

1. The wrapper relays SIGTERM and the runner reports its own terminal result ([terminationRelay](../../cmd/worker/main.go)).
   The relay is tier-independent; the scale-set `run.sh` branch has the same PID-1 handling, so this experiment runs on both tiers.
2. The report completes inside the grace period.
   The provisioner sets no `terminationGracePeriodSeconds`, so worker pods get the Kubernetes default of 30s unless a tenant overrides it in `podTemplate`.
   A runner that needs longer is truncated by SIGKILL and the case degrades to experiment 1's.
3. Whether the job requeues, and by what mechanism.
   Do **not** assume it does.
4. Classic only: whether the job lock is released without waiting out the lapse.
   Scale-set has no AGC-held per-job lock; the runner owns its session (see scaleset-eviction-recovery.md Phase 3, which fails to find a job-scoped credential on that tier), so this assertion does not port.

### Result, measured 2026-07-27

**The code reading held, on both tiers: a drain reaches no eviction recovery whatsoever.** The prediction is now a measurement, taken at two venues.

**envtest, both tiers** — [`drain_eviction_test.go`](../../cmd/agc/internal/controller/integration/drain_eviction_test.go).
Each test drives the real `pods/<name>/eviction` subresource against a worker pod that came out of the real provisioning path, carrying a complete run identity, and asserts the rerun API is never called.
The classic case wires the production `InformerPodWaiter` rather than the poll fallback, because the two agree on a pod that reaches a terminal phase and disagree on one that is deleted without ever reaching one.
Both pass.
Each is the exact twin of the eviction test that *does* fire a rerun on the same wiring, so the eviction/deletion distinction is isolated to one substitution rather than argued.

**fake-GitHub on kind** — [`worker_drain_test.go`](../../cmd/gmc/test/e2e/worker_drain_test.go) (`E2E_AGC_WorkerNodeDrain`).
A real `kubectl drain` against the node holding a live, AGC-watched worker pod.
Recorded:

| Observation | Value |
|---|---|
| `kubectl drain` output | `node/… cordoned` then `evicting pod tenant-drain/runner-…` |
| Worker pod phase/reason sequence, sampled at 200 ms across the drain | `Pending/` — and then gone |
| `cluster-autoscaler.kubernetes.io/safe-to-evict` on the drained pod | `false` — the drain evicted it regardless |
| `rerun-failed-jobs` calls for the run, over 45 s after removal | `0` |

The pod never published *any* terminal phase, let alone `PodFailed`/`Evicted`: it went from `Pending` straight to absent.
Nothing either tier's detection reads ever existed.
Observing the rerun at this tier needed fakegithub to answer and record the call at all, which it now does (`/control/reruns`); the spec first asserts the AGC's `GITHUB_API_BASE_URL` points at fakegithub, so the absence it measures cannot be an absence of instrumentation.

**The second finding, which the experiment was not looking for.** The AGC stamps every worker pod `safe-to-evict: false`, Karpenter `do-not-disrupt`, and the descheduler's prefer-no-eviction marker ([`defaults.go`](../../cmd/agc/internal/provisioner/defaults.go)) precisely so a mid-job worker is not disrupted.
`kubectl drain` honours none of them — they are advisory to autoscalers and deschedulers, not to the Eviction API — and worker pods carry no PodDisruptionBudget.
So an operator draining a node is the one disruption source that is *neither* deflected by the disruption-safety markers *nor* recovered by eviction recovery.
Every other disruption path is covered by one or the other.

**Answers to the assertions.**

- **3 (does the job requeue, and by what mechanism):** answered, and the answer is *not by the AGC, on either tier*.
  This is the load-bearing result.
- **1, 2 (does the relay get the runner's own report out inside the grace period):** **not answered here, and not answerable at this tier.** A fake-GitHub worker running the real runner image exits by itself within seconds — fakegithub's synthetic payload is not a job the runner can execute — so the spec deliberately drains a scheduled-but-`Pending` worker instead, which makes the drain the unambiguous cause of the pod's removal but leaves no live container to signal.
  The relay itself is covered by the `cmd/worker` unit tests (Q385/Q445); what remains unmeasured is whether a *real* job, reported by a *real* runner during the grace period, ends up in a state GitHub will retry.
- **4 (classic lock release):** the AGC's `provision()` returns as soon as the pod vanishes — the waiter reports a deleted pod as `PodSucceeded` — so the AGC does not hold the job open.
  What GitHub does with that is the same open question as 1–2.

**Consequence: the gap is real but its severity is not yet established, so it is filed rather than fixed.** Q417 scoped scale-set eviction detection to `PodFailed`/`Evicted` on the stated reasoning that the SIGTERM relay already owns the deletion case.
That reasoning is now known to be load-bearing on both tiers, and its premise — that the relay's report leaves the job recoverable — is exactly the part still unmeasured.
Extending both tiers to treat a graceful deletion as recoverable would be the fix, but doing it before knowing what GitHub does with a relayed cancellation risks auto-rerunning jobs that a human deliberately cancelled (a `kubectl delete pod`, or a run cancelled in the GitHub UI, arrives on the same path as a drain).
[Q459](archive/q459-drained-worker-recovery.md) carries the live-GitHub measurement and the decision that follows from it.

**Update, 2026-07-28.** Q459 took the first half of that measurement, and the premise holds: a real runner interrupted mid-job gets its report out inside the grace period, GitHub concludes the job `failure` well under a minute later (15–26s across five runs), and `rerun-failed-jobs` returns `201` with a second attempt that runs.
It also corrected one thing this section infers.
The claim above that a deliberate cancel "arrives on the same path as a drain" is true of `kubectl delete pod` but **not** established for a GitHub-UI cancel, which reaches the runner over its own broker connection rather than through the pod — and a *running* worker, unlike the `Pending` one drained here, publishes `PodFailed` with an empty reason before its object is removed rather than vanishing without a terminal phase.
Details in [q459-drained-worker-recovery.md](archive/q459-drained-worker-recovery.md).

Worth noting for that decision: the drain path is currently *worse* for the user than the ungraceful one.
A kubelet node-pressure eviction auto-reruns the job; a graceful operator drain does not.

## Experiment 3: oversubscription demo (Q423)

**Done 2026-07-29** — jump to the [Result](#result-measured-2026-07-29-preemption-is-not-eviction).
The answer is the opposite of what this section predicted, so the prediction is kept below rather than edited away.

Configure `priorityTiers` so low-priority CI runs inside capacity reserved for higher-priority work.
Force preemption.
Assert the preempted job recovers with no human action.

- **Proves:** the central claim, that tiering is only safe because recovery is automatic.
- **Unlocks:** turns the payoff section of the write-up from an argument into a result.
- **Unblocked** — both contaminants cleared: Q420 and Q417 shipped 2026-07-26.
- ~~Preemption is kubelet-initiated, so unlike experiment 2 it does produce `PodFailed`/`Evicted` and does exercise `handleEviction` on classic.~~ **False — measured 2026-07-29.** This conflated two mechanisms that are both called eviction.
  See the Result.

### Result, measured 2026-07-29: preemption is not eviction

**A `PriorityClass` preemption reaches no eviction recovery on either tier.** It is the *graceful-removal* path experiment 2 and [Q459](archive/q459-drained-worker-recovery.md) already measured, not the kubelet path recovery acts on.
The demo this experiment set out to produce does not exist to be produced: there is no automatic recovery on the preemption path to demonstrate.

**Why the prediction was wrong.** Two different mechanisms share the word:

- **Node-pressure eviction** is the *kubelet's*.
  It leaves the pod in `PodFailed` with `Status.Reason` `"Evicted"` — the one shape both tiers key on ([`provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go) step 7 on classic, `evictedAwaitingRecovery` in [`eviction_scaleset.go`](../../cmd/agc/internal/provisioner/eviction_scaleset.go) on scale-set).
- **Preemption** is *kube-scheduler's*, and it is what a `PriorityClass` actually drives.
  The scheduler removes the victim by **deleting** it.
  The kubelet then runs an ordinary graceful termination.

`priorityTiers` drives the second.
Nothing in that path produces `Evicted`.

**How the preemption was forced, at both venues.** Node CPU and memory are the obvious contended resources and the wrong ones: how much of a kind node is free depends on everything else the suite is running, so a preemption forced that way races the rest of the cluster.
Both runs instead advertise a custom integer **extended resource** — one unit, on one node — and have the victim and the displacing pod each request it.
Extended resources are integers the kubelet does not manage, so the arithmetic is exact: one slot, two claimants, higher priority wins, and preemption is the scheduler's only way to place the second pod.

**fake-GitHub, the full gateway** — [`worker_preemption_test.go`](../../cmd/gmc/test/e2e/worker_preemption_test.go) (`E2E_AGC_WorkerPreemption`), passing 2026-07-29 on the e2e kind cluster (Kubernetes v1.36.1).
A real tenant declares `priorityTiers`, the AGC provisions a worker for a job carrying a complete run identity, and a higher-priority pod displaces it.

| Observation | Value |
|---|---|
| `spec.priorityClassName` on the worker pod | `gag-e2e-opportunistic` — the tier reached the pod, so this is oversubscription and not an ordinary eviction |
| `safe-to-evict` on the worker | `false` — **and the preemption proceeded anyway** |
| Victim `phase/reason/deletionTimestamp/DisruptionTarget-reason`, sampled at 200 ms | `Pending//2026-07-29T13:30:50Z/PreemptionByScheduler` |
| `Failed`/`Evicted` ever observed | **no** |
| Scheduler event on the victim | `Preempted: Preempted by pod 0d6e0a7d-… on node actions-gateway-e2e-worker` |
| `rerun-failed-jobs` calls for the run, over 45 s after removal | **0** |

As with the drain spec, the rerun assertion is guarded so its absence cannot be an absence of instrumentation: the spec first asserts the AGC's `GITHUB_API_BASE_URL` addresses fakegithub, and pins an `AcquireJob` payload carrying owner/repo/run_id — without which `handleEviction` returns early and no rerun could fire for reasons having nothing to do with preemption.

> The last row is the measurement as taken, before Q497.
> The spec kept its whole apparatus and flipped that assertion: it is now `E2E_AGC_PreemptedWorkerIsRecovered` and requires **exactly one** rerun.
> The rows above it are unchanged and still asserted — recovery must be reached by the scheduler's marker, never by the victim turning up `Failed`/`Evicted`.

**A second spec, for the phase a *running* victim publishes** — `E2E_AGC_PreemptedRunningPodPhaseFollowsItsExitCode`, in the same file.
The first spec's victim is deliberately held `Pending` (its image cannot be pulled), the same trade `E2E_AGC_WorkerNodeDrain` makes and for the same reason, so it has no live container and cannot show what the kubelet publishes on the way out.
This one preempts a worker-shaped pod — same disruption-safety annotations, no PodDisruptionBudget — running a process that traps SIGTERM and **exits 0**.

| Observation | Value |
|---|---|
| Victim class / preemptor class | value `100`, `preemptionPolicy: Never` / value `1000000`, `PreemptLowerPriority` |
| Victim `phase/reason/deletionTimestamp/DisruptionTarget-reason`, sampled at 200 ms | `Running//2026-07-29T13:35:29Z/PreemptionByScheduler` → `Succeeded//…/PreemptionByScheduler` |
| `Failed`/`Evicted` ever observed | **no** |
| Scheduler event on the victim | `Preempted: Preempted by pod eefad962-… on node actions-gateway-e2e-worker` |
| kubelet event on the victim | `Killing: Stopping container sleeper` |

It is deliberately *not* a gateway worker: a worker's command is the injected wrapper, so its exit code is the runner's and cannot be made 0 on demand.
What is under test is the kubelet's behaviour on a preemption, which is worker-independent, so the pod is built to isolate it.

The two specs agree on everything that decides recovery, and differ only in the terminal phase — which is finding 1 below.

**Two findings beyond the verdict.**

1. **The terminal phase on a graceful removal is the container's own exit status.** Q459 recorded a disrupted *running* worker landing in `PodFailed` with an empty reason, and reasoned from there that recovery cannot key on the phase because a genuinely failing job produces the same shape.
   The second spec lands in `Succeeded` — its container exits 0 on SIGTERM — from the identical removal path, and the first spec's victim never leaves `Pending` at all.
   So the phase is not merely *ambiguous* on this path, it is not even *stable*: `Pending`, `Succeeded` and `Failed` all occur, decided by what the interrupted process was doing and what it exited with.
   No phase/reason combination can carry the discrimination.
2. **The scheduler leaves an unambiguous marker, and the AGC ignores it.** The victim carries a `DisruptionTarget` condition with reason **`PreemptionByScheduler`**.
   Unlike `deletionTimestamp` — which Q459 is weighing, and which an operator's `kubectl delete pod` and a drain also set — this reason is written *only* by kube-scheduler preemption.
   It cannot be produced by a human cancelling a run, nor by a job failing on its own.
   That makes the preemption slice of the graceful-removal gap closable on its own, without the human-cancel ambiguity that is holding Q459's decision open.

   **Closed 2026-07-29 by Q497** ([plan](archive/q497-preemption-recovery.md)).
   Both tiers now recover a preemption off this condition, sharing the existing per-`run_id` retry budget, and the `cause` label on the eviction counters keeps a preemption recovery distinguishable from a node-pressure one.
   The spec below flipped with it: it is now `E2E_AGC_PreemptedWorkerIsRecovered`, and asserts exactly one re-run rather than none.
   Two things about the fix are worth recording here, because both are consequences of *this* measurement rather than choices:

   - Detection keys on the condition and never on the phase, because finding 1 above ruled the phase out entirely.
   - Matching the `DisruptionTarget` **type** alone would have been wrong: the eviction API stamps the same condition with reason `EvictionByEvictionAPI`, so a type-only match would have silently recovered the drain path and pre-empted Q459's open decision.
     The `reason` is the whole discriminator.

**What this cost the published claim, and how it was repaid.** The oversubscription argument in [01-executive-summary.md](../design/01-executive-summary.md) §"safe oversubscription" and in the README's problem statement rests on displaced work coming back by itself.
The *packing* half was real and unaffected — guaranteed tiers do preempt their way in, which is what removes the need for reserved idle headroom.
The *safety* half, as published, was not: a preempted job was left needing a manual re-run, exactly like a drained one.
Both documents were corrected to say so on the day of the measurement.

Q497 then made the original claim true rather than leaving the correction standing, and both documents are corrected back — this time with a measurement behind them.
The residual cost is no longer a manual re-run but the displaced job's own elapsed time: the re-run starts from the beginning rather than resuming, which is why the guidance to put cheap-to-repeat work in displaceable tiers survives.
The drain path has since followed: Q502 implemented Q459's decision, so a drained worker whose terminal phase publishes with the deletion mark is re-run too; [troubleshooting.md](../operations/troubleshooting.md#draining-a-worker-auto-re-runs-the-jobs-it-interrupts) now covers the drain alone, with [a separate runbook](../operations/troubleshooting.md#a-preempted-workers-job-is-not-re-run) for a preemption recovery that fails to fire.

**A third finding, from building the spec rather than running it (Q499, since documented in [security-operations.md § Narrowing the allowlist](../operations/security-operations.md#narrowing-the-allowlist-drain-stored-references-first)).** Narrowing the platform PriorityClass allowlist **wedges deletion of any tenant still referencing the removed class**.
The `priorityclass-allowlist-guard` policy re-validates stored objects on update — deliberately, and documented as a feature — but tearing a tenant down *is* a sequence of updates: the GMC clearing `gmc-cleanup` from the `ActionsGateway`, the AGC clearing `agentpool-cleanup` from the `RunnerGroup`.
With the class off the allowlist every one of them is denied, so the finalizers can never be removed and the namespace hangs in `Terminating` with no controller able to free it.
Recovering needs a human to re-widen the allowlist and strip the finalizer by hand.
Reproduced exactly that way here; the spec's teardown now drains the tenant *before* restoring the fail-closed default, and the ordering is commented so it is not "simplified" back.

**What is not measured here.** The wrapper's SIGTERM relay, and therefore whether a *real* preempted job reports itself to GitHub in a state a re-run would accept.
Neither spec has a real runner in the victim — the first holds it `Pending`, the second runs a stand-in process chosen for its exit code.
That question is the drain path's too, and Q459 answered it for a graceful delete at live-GitHub: the report gets out and `rerun-failed-jobs` returns `201`.
A preemption is the same removal, so the same answer is expected — but it is inherited, not re-measured.

## Experiment 4: quota gate under real pressure (Q422)

Fill the namespace `ResourceQuota`, submit more jobs than fit, and assert they stay queued server-side rather than claimed-and-stalled.

**Correction under review: this is cheaper than #819 assumed, and it splits.** The "visible in metrics or Events" half is already instrumented.
`actions_gateway_jobs_admission_rejected_total{reason="quota"}` ships with [#793](https://github.com/actions-gateway/github-actions-gateway/pull/793) and is documented in [§4.2](../design/04-operational-flows.md#42-job-execution-flow-agc) step 2a, so asserting it needs no new plumbing.

- **Half A (envtest) — done 2026-07-26.** Covered by [`q422_quota_admission_test.go`](../../cmd/agc/internal/controller/integration/q422_quota_admission_test.go), one test per tier the rung serves.
  Findings below.
- **Half B (live-GitHub) — done 2026-07-31.** Two AGC sessions on the same runner group, one without headroom, and the job is picked up by the sibling.
  This is the half that needed live GitHub redelivery and could not be faked.
  Covered by `E2E_GitHub_QuotaBlockedJobRunsOnSibling` in [`github_e2e_test.go`](../../cmd/gmc/test/e2e/github_e2e_test.go); the arrangement is below, the [result](#half-b-result-measured-2026-07-31) after it.
- **Not blocked.**

### Half A findings

Both tests drive a live listener against the broker/scale-set fakes with a real `ResourceQuota` read through the manager's informer cache, and both were mutation-checked: disabling the rung fails each of them.

- **Classic tier.** With the quota full, three deliveries in a row are skipped: `acquirejob` is never called (asserted on the broker stub's server-side call counter, not on the absence of a pod — "no pod appeared" would also pass for the claim-and-stall this rung exists to prevent), `..._admission_rejected_total{reason="quota"}` increments once per delivery, and no worker pod or per-job Secret is staged.
- **The ceiling budget is untouched.** `maxWorkers` is 1, so a single leaked reservation would close the gate permanently.
  Once headroom returns a job is claimed and a pod is built, and the `reason="ceiling"` series never moves.
  Mutating `Admit` to reserve a slot before refusing for quota fails exactly this assertion.
- **Scale-set tier** (the rung reached it in Q443, and Q450 corrected the footprint).
  The existing Q443 test covers the all-or-nothing case; the gap was the *partial* one, where `AdvertiseCapacity` converts a headroom delta into a total.
  Under a half-consumed quota the invariant `advertised + withheld == declared ceiling` holds, which is the scale-set expression of "the quota rung reserves nothing".
- **A caveat on what envtest can show.** There is no resourcequota controller, so the tests own `status.hard`/`status.used` outright — which is what lets them fill a quota the way a busy namespace does (`hard − used`, the arithmetic the gate actually runs) rather than declaring a hard limit too small to ever fit a worker, as every prior envtest does.
  The flip side is that `used` does not rise as worker pods are created, so these tests cannot assert an assignment count across more than one poll.

### Half B: how the spec is arranged, and why

Half A proved the *refusal*.
Half B is about the **premise the refusal rests on** — that a job left unclaimed comes back, and that a sibling with headroom then runs it.
That premise cannot be tested against a fake broker: `fakegithub` redelivers because it was written to, so asserting redelivery there restates the fake rather than GitHub.

Four choices carry the spec, each answering a way it could have passed without demonstrating anything.

- **The sibling gateway is stood up *after* the decline is observed.** Both gateways register into the org's Default runner group under the same `e2e` label, so GitHub may offer the job to either.
  With both up at dispatch, the sibling could take it on the first offer and the blocked gateway would never see it — a green run with no decline in it.
  Bringing the sibling in late makes the ordering a property of the spec instead of GitHub's routing.
- **The claim is disproven by the backstop's silence, not by an absent pod.** A job that *was* claimed and whose worker pod the quota then rejected also leaves no pod; that claim-and-stall is the exact failure the rung exists to prevent, and half A names the same trap.
  `createPodWithQuotaRetry` logs `pod creation blocked by namespace quota` at Info on every quota-rejected create, so **zero** such lines, beside the gate's own `reason=quota` decline, is what says the job was left at GitHub rather than claimed and abandoned.
- **The quota constrains `pods`, not `requests.cpu`.** A CPU quota rejects any pod that declares no CPU request, which is why a quota'd tenant needs a `LimitRange` (Q262) — the tenant's own control plane would become collateral.
  `pods` filled to the namespace's live occupancy models a busy namespace the way half A's `hard − used` arithmetic does, without that side effect.
- **The observable is the AGC log, not the rejection counter.** The counter (`..._admission_rejected_total{reason="quota"}`) is half A's observable and is asserted there directly.
  At this tier the AGC's metrics endpoint is TLS- and authn-gated inside the tenant namespace, and reaching it means a scrape pod in a namespace this spec has deliberately filled to its pod ceiling.
  The decline log line carries the same `reason` label and states the consequence — the job is left queued — so it is the cheaper read of the same fact.

Two known residuals, neither of which changed the result:

- **Sizing the quota from live occupancy assumes occupancy holds.** If the tenant's proxy HPA scales back to its floor mid-spec, a pod is vacated and the gate gains the one slot the spec needs it not to have.
  The spec holds `WorkerQuotaExceeded=True` for 30s before dispatching so that churn fails up front as itself; churn *after* dispatch would surface as the final "the blocked gateway provisioned a worker" assertion instead.
- **The sibling gateway is new shared GitHub-side state.** It is named `real-ag-sib` — `agName` plus a suffix — for two reasons at once: differing from `real-ag` is what keeps the two off one runner name and out of the 409 path where each deregisters the other, and *extending* it keeps every runner this suite registers under the one `real-ag-` prefix that a stranded-runner sweep identifies the suite by ([Q511](q511-live-github-run-isolation.md)).
  A sibling named independently would have registered runners nothing knew to clean up.

### Half B result, measured 2026-07-31

Live-GitHub tier on a throwaway kind cluster, against `actions-gateway/gateway-test` ([run 30649990040](https://github.com/actions-gateway/gateway-test/actions/runs/30649990040)), by `E2E_GitHub_QuotaBlockedJobRunsOnSibling`.
The blocked tenant's namespace held two pods (its AGC and one proxy) and the quota was filled to exactly that: `pods=2`, all occupied.

| Observation | Value |
|---|---|
| Quota the gate read | `needs 1 more pods but quota "q422-full" has 0 free` |
| Job queued at GitHub | `created_at=17:08:29Z` |
| First decline | `17:08:31Z` — 2s after dispatch |
| Declines observed, all `reason=quota` | **≥63 in the first 9s** (~7/s) |
| Post-claim backstop (`pod creation blocked by namespace quota`) | **0 lines** |
| Job state while blocked | `queued` — GitHub held it |
| Runner that ran it | **`real-ag-sib-e2e-6d8749c-0`** — the sibling's |
| Job started / concluded | `started_at=17:09:32Z`, `success`, attempt **1** |
| **Queued → running on the sibling** | **63s** |
| Worker pods in the blocked namespace | unchanged across the whole window |

**The premise holds.** GitHub redelivered the job the out-of-quota gateway declined, and a sibling on the same runner group ran it green on the first attempt — no re-run, no second attempt, no operator action.
The `runner_name` on GitHub's own job record is the discriminator: `real-ag-sib-…`, not `real-ag-…`.

**The rung refused before the claim, not after.** Zero `pod creation blocked by namespace quota` lines is the load-bearing negative: that backstop fires on every quota-rejected pod create, so its silence means `acquirejob` was never called.
Had the gate not fired, the job would have been claimed and stalled against a pod the namespace could not admit — which is the failure this rung exists to prevent, and which "no worker pod appeared" would not have distinguished.

**What was not expected: redelivery is a tight loop, not a retry with backoff.** The job came back **~7 times a second** for as long as it was watched.
That is the first measurement of the redelivery cadence, and it retroactively justifies a decision made on suspicion: the per-delivery decline is logged at Debug precisely because it is "high-volume under sustained capacity pressure" (`listener/job.go`), with the metric as the operator-facing signal.
At 7/s a tenant sitting at its quota ceiling would bury its own log at Info.
It also means the rung is on a genuinely hot path — the quota read is cache-backed, which is what makes ~7 evaluations/s per blocked session affordable.

Two caveats on the numbers, both in the conservative direction:

- **The decline count is a floor, not a total.** The spec snapshots the AGC log once, ~9s after dispatch, and the job did not reach the sibling for another 52s.
  Declines almost certainly continued through that window; 63 is what had accumulated by the snapshot.
  The rate is the meaningful figure, not the count.
- **The 63s queued→running is this run's arrangement, not GitHub's latency.** Most of it is the spec deliberately standing the sibling up after observing the decline — namespace, Secret, CR, AGC rollout, and the RunnerGroup reaching `observedGeneration`.
  It is an upper bound on redelivery latency, not a measurement of it; the first decline landing 2s after dispatch is the tighter signal.

Both gateways deregistered cleanly on teardown — the fixture repo listed zero runners afterwards — which also confirms the `real-ag-` prefix reasoning above against real registrations rather than against the naming code.

## Experiment 5: utilization delta ([Q424](../queue/Q424.md), deferred)

Same workload on dogfood, tiers off versus tiers on, occupancy measured over a fixed window.

- **Proves:** the packing-density thesis directly, and it is the one number the whole argument is missing.
- **Deferred rather than queued.** Q417 shipped 2026-07-26, but this still needs a dogfood workload stable enough for a fixed-window comparison to mean anything.
  With no owner-actionable next step today, a Queue position would be fiction.

## Sequencing

Q417 shipped 2026-07-26, so nothing here is blocked on it any more.

1. ~~Q421 (experiment 2)~~ — **done 2026-07-27**; see [Result](#result-measured-2026-07-27).
   Its residual is [Q459](archive/q459-drained-worker-recovery.md), which needs live-GitHub and so sequences with the other live-GitHub work below rather than ahead of it.
2. ~~Q422 (experiment 4)~~ — **done 2026-07-31**; see the [result](#half-b-result-measured-2026-07-31).
   Both halves are now covered, and it left no residual.
3. Q396 (experiment 1), which then gates [Q418](../queue/Q418.md).
   Fold [Q459](archive/q459-drained-worker-recovery.md) in around here: both want a real GitHub run interrupted mid-job, and Q396 is already standing that up.
4. ~~Q423 (experiment 3)~~ — **done 2026-07-29**; see [Result](#result-measured-2026-07-29-preemption-is-not-eviction). ~~Its residual is Q497~~ — **also done 2026-07-29** ([plan](archive/q497-preemption-recovery.md)): the `PreemptionByScheduler` marker resolved the discriminator question for the preemption slice without waiting on Q459's human-cancel one, exactly as predicted here.
   Then revive [Q424](../queue/Q424.md) (experiment 5).

## Acceptance criteria

- A published eviction-recovery latency figure with the mechanism attributed, replacing the confounded U5 number everywhere it is cited.
- ~~A recorded answer for the drain path: either it recovers via the SIGTERM relay, as Q417 assumed when it scoped detection to `PodFailed`/`Evicted`, or the gap is filed and both tiers are extended to cover deletion.~~ **Met 2026-07-27** by the second branch: neither tier recovers a drained worker, and the gap is filed as [Q459](archive/q459-drained-worker-recovery.md).
  Extending the tiers is deliberately left to that row — the same code path carries deliberate cancellations, so it needs the live-GitHub answer before it can tell a drain from a `kubectl delete pod` worth honouring.
- ~~The quota gate demonstrated under contention, with the rejection counter as the observable.~~ **Met 2026-07-31** — see [the result](#half-b-result-measured-2026-07-31).
  Half A asserts the rejection counter directly; half B substituted the decline log line, which carries the same `reason` label, because scraping the AGC's TLS/authn-gated metrics endpoint would have meant scheduling a pod into the namespace the experiment had just filled.
- ~~Preemption recovery demonstrated end to end before the oversubscription claim is published.~~ **Met 2026-07-29, in two steps.** The experiment first found there was nothing to demonstrate — a `PriorityClass` preemption is a graceful deletion, not a kubelet eviction, so no automatic recovery fired — and the published claim was corrected rather than illustrated.
  Q497 then built the recovery ([plan](archive/q497-preemption-recovery.md)) and the claim was restored, this time with a measurement behind it.
  The fake-GitHub spec that made the original measurement was flipped from "no rerun" to "exactly one rerun" and is what demonstrates it end to end.
  See [the result](#result-measured-2026-07-29-preemption-is-not-eviction).
